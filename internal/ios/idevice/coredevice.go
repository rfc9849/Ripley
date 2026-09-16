package idevice

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// The CoreDevice tunnel is how iOS 17 and newer expose the developer services
// that used to live behind lockdownd.
//
// Contrary to the common assumption that this needs root and a kernel utun
// interface, the tunnel is reachable through plain usbmuxd: lockdownd still
// starts `com.apple.internal.devicecompute.CoreDeviceProxy`, and after a small
// "CDTunnel" handshake that service turns into a point-to-point IP link that
// carries raw IPv6 datagrams over the same socket. Everything stays in user
// space, so this works unprivileged on Linux.
//
// The handshake and the framing below are what the attached iPhone14,2 on iOS
// 26.6.1 actually accepts and emits; see TestTunnelHandshakeFraming for the
// encoding contract.
const (
	coreDeviceProxyService = "com.apple.internal.devicecompute.CoreDeviceProxy"

	// tunnelMagic prefixes every handshake frame.
	tunnelMagic = "CDTunnel"

	// tunnelMTU is the link MTU requested from the device. The device echoes it
	// back in clientParameters and sizes its own datagrams accordingly.
	tunnelMTU = 16000

	// tunnelMaxFrame bounds a handshake body.
	tunnelMaxFrame = 1 << 16
)

// TunnelInfo describes an established CoreDevice tunnel.
//
// ServerAddress/RSDPort are the device end of the link: the address a Remote
// Service Discovery client must connect to in order to enumerate the modern
// developer services. LocalAddress is the address the device assigned to this
// host.
type TunnelInfo struct {
	ServerAddress string
	RSDPort       int
	LocalAddress  string
	Netmask       string
	MTU           int
}

// tunnel is an established CoreDevice tunnel. Reads and writes are whole IPv6
// datagrams: the link has no framing of its own past the handshake.
type tunnel struct {
	conn net.Conn
	info TunnelInfo
}

type tunnelHandshakeRequest struct {
	Type string `json:"type"`
	MTU  int    `json:"mtu"`
}

type tunnelParameters struct {
	MTU     int    `json:"mtu"`
	Address string `json:"address"`
	Netmask string `json:"netmask"`
}

type tunnelHandshakeResponse struct {
	Type             string           `json:"type"`
	ServerAddress    string           `json:"serverAddress"`
	ServerRSDPort    int              `json:"serverRSDPort"`
	Flags            int              `json:"flags"`
	ClientParameters tunnelParameters `json:"clientParameters"`
}

// encodeTunnelFrame renders a handshake frame: magic, big-endian length, JSON.
func encodeTunnelFrame(payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode tunnel handshake: %w", err)
	}
	if len(body) > tunnelMaxFrame {
		return nil, fmt.Errorf("tunnel handshake payload too large (%d bytes)", len(body))
	}
	frame := make([]byte, 0, len(tunnelMagic)+2+len(body))
	frame = append(frame, tunnelMagic...)
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(body)))
	return append(frame, body...), nil
}

// decodeTunnelFrame reads one handshake frame body.
func decodeTunnelFrame(r io.Reader) ([]byte, error) {
	var hdr [10]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("read tunnel handshake header: %w", err)
	}
	if got := string(hdr[:8]); got != tunnelMagic {
		return nil, fmt.Errorf("tunnel handshake: bad magic %q, want %q", got, tunnelMagic)
	}
	n := binary.BigEndian.Uint16(hdr[8:])
	if n == 0 {
		return nil, fmt.Errorf("tunnel handshake: empty body")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read tunnel handshake body: %w", err)
	}
	return body, nil
}

// startTunnel opens CoreDeviceProxy and performs the CDTunnel handshake.
//
// The MTU must be sent at the top level of the request. Nesting it under
// clientParameters, or supplying our own addresses, makes the device close the
// connection without answering.
func (s *session) startTunnel() (*tunnel, error) {
	svc, err := s.ld.StartService(coreDeviceProxyService)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", coreDeviceProxyService, err)
	}

	frame, err := encodeTunnelFrame(tunnelHandshakeRequest{
		Type: "clientHandshakeRequest",
		MTU:  tunnelMTU,
	})
	if err != nil {
		svc.Close()
		return nil, err
	}
	if err := svc.conn.SetDeadline(time.Now().Add(ioTimeout)); err != nil {
		svc.Close()
		return nil, err
	}
	if _, err := svc.conn.Write(frame); err != nil {
		svc.Close()
		return nil, fmt.Errorf("write tunnel handshake: %w", err)
	}
	body, err := decodeTunnelFrame(svc.conn)
	if err != nil {
		svc.Close()
		return nil, err
	}
	var resp tunnelHandshakeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		svc.Close()
		return nil, fmt.Errorf("decode tunnel handshake response: %w", err)
	}
	if resp.Type != "serverHandshakeResponse" {
		svc.Close()
		return nil, fmt.Errorf("tunnel handshake: unexpected response type %q", resp.Type)
	}
	if resp.ServerAddress == "" || resp.ServerRSDPort == 0 {
		svc.Close()
		return nil, fmt.Errorf("tunnel handshake: device returned no RSD endpoint (%s)", body)
	}
	if err := svc.conn.SetDeadline(time.Time{}); err != nil {
		svc.Close()
		return nil, err
	}
	mtu := resp.ClientParameters.MTU
	if mtu == 0 {
		mtu = tunnelMTU
	}
	return &tunnel{
		conn: svc.conn,
		info: TunnelInfo{
			ServerAddress: resp.ServerAddress,
			RSDPort:       resp.ServerRSDPort,
			LocalAddress:  resp.ClientParameters.Address,
			Netmask:       resp.ClientParameters.Netmask,
			MTU:           mtu,
		},
	}, nil
}

func (t *tunnel) Close() error { return t.conn.Close() }

// OpenTunnel establishes a CoreDevice tunnel to the device and reports the
// Remote Service Discovery endpoint behind it.
//
// This is the transport iOS 17+ requires for developer services such as
// launching a process. It needs no root and no kernel tunnel interface: the
// datagrams travel over the usbmuxd socket. The tunnel is closed before
// returning, so the returned endpoint describes what was reachable rather than a
// live link.
func OpenTunnel(udid string) (*TunnelInfo, error) {
	s, err := openSession(udid)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	tun, err := s.startTunnel()
	if err != nil {
		return nil, err
	}
	defer tun.Close()
	info := tun.info
	return &info, nil
}
