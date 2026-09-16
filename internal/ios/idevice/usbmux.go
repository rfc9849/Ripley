// Package idevice talks to physical iOS devices from any operating system.
//
// Everything here is spoken directly on the wire: the usbmuxd multiplexer
// protocol, lockdownd, AFC and installation_proxy. There is no dependency on
// libimobiledevice, on cgo, or on any Apple binary, so the same code path works
// on Linux (where `usbmuxd` from the libimobiledevice project provides the
// socket) and on macOS (where Apple's own usbmuxd provides it).
//
// The only host-side prerequisite is a usbmuxd socket plus a pairing record for
// the device. Pair records are read back *through* usbmuxd
// (`ReadPairRecord`), never from `/var/db/lockdown` or
// `/var/lib/lockdown`, because those directories are root-only.
package idevice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"howett.net/plist"
)

const (
	// DefaultSocket is the usbmuxd endpoint used when USBMUXD_SOCKET_ADDRESS is
	// unset. Apple's usbmuxd on macOS and usbmuxd(1) on Linux both listen here.
	DefaultSocket = "/var/run/usbmuxd"

	muxHeaderSize   = 16
	muxVersionPlist = 1
	muxMessagePlist = 8

	// lockdownPort is the TCP port lockdownd listens on inside the device.
	lockdownPort = 62078

	clientVersionString = "ripley usbmux/1.0"
	progName            = "ripley"
	libUSBMuxVersion    = 3

	// ioTimeout bounds every individual read/write against usbmuxd. Install
	// progress messages can be slow, so this is generous.
	ioTimeout = 90 * time.Second
)

// ErrNoUSBMuxd is returned when the usbmuxd socket cannot be reached. It always
// wraps into a message naming the socket that was tried plus how to get one.
var ErrNoUSBMuxd = errors.New("usbmuxd is not reachable")

// ErrNoPairRecord is returned when usbmuxd has no pairing record for a device,
// which means the device has never been trusted by this host.
var ErrNoPairRecord = errors.New("no pairing record")

// SocketAddress reports the usbmuxd endpoint this process will use.
//
// USBMUXD_SOCKET_ADDRESS is honoured with the same syntax libusbmuxd uses:
// "UNIX:/path/to/socket" for an explicit unix socket, "host:port" for TCP, and
// a bare path for a unix socket.
func SocketAddress() (network, address string) {
	spec := os.Getenv("USBMUXD_SOCKET_ADDRESS")
	switch {
	case spec == "":
		return "unix", DefaultSocket
	case strings.HasPrefix(spec, "UNIX:"):
		return "unix", strings.TrimPrefix(spec, "UNIX:")
	case strings.Contains(spec, ":"):
		return "tcp", spec
	default:
		return "unix", spec
	}
}

// muxConn is a framed connection to usbmuxd. After a successful Connect the
// framing stops and the underlying conn becomes a raw pipe to a device port.
type muxConn struct {
	conn net.Conn
	tag  uint32
}

func dialMux() (*muxConn, error) {
	network, address := SocketAddress()
	conn, err := net.DialTimeout(network, address, 10*time.Second)
	if err != nil {
		// net's own message already repeats the network and address; keep only
		// the syscall reason so the hint is the most visible part.
		reason := err
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Err != nil {
			reason = opErr.Err
		}
		return nil, fmt.Errorf("%w: cannot connect to the usbmuxd socket at %s %s: %v\n%s",
			ErrNoUSBMuxd, network, address, reason, installHint())
	}
	return &muxConn{conn: conn}, nil
}

func installHint() string {
	return "install and start usbmuxd (Debian/Ubuntu: `sudo apt install usbmuxd`, " +
		"Fedora: `sudo dnf install usbmuxd`), make sure the service is running " +
		"(`systemctl status usbmuxd`) and that the phone is plugged in; set " +
		"USBMUXD_SOCKET_ADDRESS to reach a usbmuxd on another host"
}

func (m *muxConn) Close() error { return m.conn.Close() }

// Conn exposes the raw connection, valid as a device pipe after Connect.
func (m *muxConn) Conn() net.Conn { return m.conn }

func (m *muxConn) send(payload map[string]any) error {
	payload["ClientVersionString"] = clientVersionString
	payload["ProgName"] = progName
	payload["kLibUSBMuxVersion"] = libUSBMuxVersion

	body, err := plist.Marshal(payload, plist.XMLFormat)
	if err != nil {
		return fmt.Errorf("encode usbmux request: %w", err)
	}
	m.tag++
	buf := make([]byte, muxHeaderSize+len(body))
	binary.LittleEndian.PutUint32(buf[0:], uint32(muxHeaderSize+len(body)))
	binary.LittleEndian.PutUint32(buf[4:], muxVersionPlist)
	binary.LittleEndian.PutUint32(buf[8:], muxMessagePlist)
	binary.LittleEndian.PutUint32(buf[12:], m.tag)
	copy(buf[muxHeaderSize:], body)

	if err := m.conn.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	if _, err := m.conn.Write(buf); err != nil {
		return fmt.Errorf("write usbmux request: %w", err)
	}
	return nil
}

func (m *muxConn) receive(out any) error {
	if err := m.conn.SetReadDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	var header [muxHeaderSize]byte
	if _, err := io.ReadFull(m.conn, header[:]); err != nil {
		return fmt.Errorf("read usbmux header: %w", err)
	}
	length := binary.LittleEndian.Uint32(header[0:])
	version := binary.LittleEndian.Uint32(header[4:])
	message := binary.LittleEndian.Uint32(header[8:])
	if length < muxHeaderSize {
		return fmt.Errorf("usbmux: bogus frame length %d", length)
	}
	if version != muxVersionPlist || message != muxMessagePlist {
		return fmt.Errorf("usbmux: unexpected frame version=%d message=%d (want plist protocol)", version, message)
	}
	body := make([]byte, length-muxHeaderSize)
	if _, err := io.ReadFull(m.conn, body); err != nil {
		return fmt.Errorf("read usbmux body: %w", err)
	}
	if _, err := plist.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode usbmux response: %w", err)
	}
	return nil
}

// muxResult is the generic {MessageType: Result, Number: n} reply.
type muxResult struct {
	MessageType string `plist:"MessageType"`
	Number      uint64 `plist:"Number"`
}

func resultError(number uint64) error {
	switch number {
	case 0:
		return nil
	case 2:
		return errors.New("usbmuxd: device not connected")
	case 3:
		return errors.New("usbmuxd: connection refused by device (port closed)")
	case 5:
		return errors.New("usbmuxd: bad protocol version")
	case 6:
		return errors.New("usbmuxd: bad request")
	default:
		return fmt.Errorf("usbmuxd: request failed with result %d", number)
	}
}

// muxDevice is a device as usbmuxd sees it, before lockdownd is consulted.
type muxDevice struct {
	DeviceID       uint32
	UDID           string
	ConnectionType string
}

type muxProperties struct {
	ConnectionType string `plist:"ConnectionType"`
	DeviceID       uint32 `plist:"DeviceID"`
	SerialNumber   string `plist:"SerialNumber"`
	UDID           string `plist:"UDID"`
	LocationID     uint64 `plist:"LocationID"`
	ProductID      uint64 `plist:"ProductID"`
	InterfaceIndex uint64 `plist:"InterfaceIndex"`
}

type muxDeviceEntry struct {
	MessageType string        `plist:"MessageType"`
	DeviceID    uint32        `plist:"DeviceID"`
	Properties  muxProperties `plist:"Properties"`
}

func (e muxDeviceEntry) device() muxDevice {
	udid := e.Properties.SerialNumber
	if udid == "" {
		udid = e.Properties.UDID
	}
	id := e.Properties.DeviceID
	if id == 0 {
		id = e.DeviceID
	}
	conn := e.Properties.ConnectionType
	if conn == "" {
		conn = "USB"
	}
	return muxDevice{DeviceID: id, UDID: normalizeUDID(udid), ConnectionType: conn}
}

// normalizeUDID restores the dash in 25-character UDIDs. usbmuxd hands out the
// serial number exactly as the device reports it, and some transports drop the
// separator from the modern "8+16" form.
func normalizeUDID(udid string) string {
	if len(udid) == 24 && !strings.Contains(udid, "-") {
		return udid[:8] + "-" + udid[8:]
	}
	return udid
}

// listMuxDevices performs a single ListDevices round trip.
func listMuxDevices() ([]muxDevice, error) {
	m, err := dialMux()
	if err != nil {
		return nil, err
	}
	defer m.Close()

	if err := m.send(map[string]any{"MessageType": "ListDevices"}); err != nil {
		return nil, err
	}
	var reply struct {
		DeviceList []muxDeviceEntry `plist:"DeviceList"`
	}
	if err := m.receive(&reply); err != nil {
		return nil, err
	}
	devices := make([]muxDevice, 0, len(reply.DeviceList))
	for _, entry := range reply.DeviceList {
		d := entry.device()
		if d.UDID == "" || d.DeviceID == 0 {
			continue
		}
		devices = append(devices, d)
	}
	return devices, nil
}

// readPairRecord asks usbmuxd for the stored pairing record of a device.
//
// This is the only supported way to obtain the host identity on a machine where
// the lockdown directory is not world readable, which is every macOS host and a
// default Linux install.
func readPairRecord(udid string) (*PairRecord, error) {
	m, err := dialMux()
	if err != nil {
		return nil, err
	}
	defer m.Close()

	if err := m.send(map[string]any{
		"MessageType":  "ReadPairRecord",
		"PairRecordID": udid,
	}); err != nil {
		return nil, err
	}
	var reply struct {
		PairRecordData []byte `plist:"PairRecordData"`
		Number         uint64 `plist:"Number"`
		MessageType    string `plist:"MessageType"`
	}
	if err := m.receive(&reply); err != nil {
		return nil, err
	}
	if len(reply.PairRecordData) == 0 {
		return nil, fmt.Errorf("%w for %s: usbmuxd result %d; trust this computer on the device "+
			"(unlock it, plug it in, tap Trust) or copy the pair record from a host that already did",
			ErrNoPairRecord, udid, reply.Number)
	}
	return parsePairRecord(reply.PairRecordData)
}

// readBUID returns the host's SystemBUID as usbmuxd knows it.
func readBUID() (string, error) {
	m, err := dialMux()
	if err != nil {
		return "", err
	}
	defer m.Close()

	if err := m.send(map[string]any{"MessageType": "ReadBUID"}); err != nil {
		return "", err
	}
	var reply struct {
		BUID string `plist:"BUID"`
	}
	if err := m.receive(&reply); err != nil {
		return "", err
	}
	return reply.BUID, nil
}

// connectDevice opens a raw pipe to a TCP port inside the device. The returned
// connection is no longer framed: it is the device socket.
func connectDevice(deviceID uint32, port uint16) (net.Conn, error) {
	m, err := dialMux()
	if err != nil {
		return nil, err
	}
	// usbmuxd wants the port in network byte order inside a host-order field.
	swapped := port<<8 | port>>8
	if err := m.send(map[string]any{
		"MessageType": "Connect",
		"DeviceID":    deviceID,
		"PortNumber":  swapped,
	}); err != nil {
		m.Close()
		return nil, err
	}
	var res muxResult
	if err := m.receive(&res); err != nil {
		m.Close()
		return nil, err
	}
	if err := resultError(res.Number); err != nil {
		m.Close()
		return nil, fmt.Errorf("connect to device port %d: %w", port, err)
	}
	if err := m.conn.SetDeadline(time.Time{}); err != nil {
		m.Close()
		return nil, err
	}
	return m.conn, nil
}

// EventType distinguishes usbmuxd device notifications.
type EventType string

const (
	EventAttached EventType = "attached"
	EventDetached EventType = "detached"
)

// Event is one usbmuxd device notification.
type Event struct {
	Type EventType
	// UDID is set for attach events and for detach events of devices that were
	// seen attached on this listener.
	UDID string
	// ConnectionType is "USB" or "Network"; empty for detach events.
	ConnectionType string
}

// Listen streams device attach and detach notifications until ctx is done.
//
// The initial burst of Attached messages describes the devices that are already
// plugged in, so a caller that only wants a snapshot can stop after the first
// event batch. Listen closes events before returning.
func Listen(ctx context.Context, events chan<- Event) error {
	m, err := dialMux()
	if err != nil {
		close(events)
		return err
	}
	defer close(events)
	defer m.Close()

	if err := m.send(map[string]any{"MessageType": "Listen"}); err != nil {
		return err
	}
	var res muxResult
	if err := m.receive(&res); err != nil {
		return err
	}
	if err := resultError(res.Number); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		m.Close()
	}()

	known := map[uint32]muxDevice{}
	for {
		var msg muxDeviceEntry
		if err := m.receive(&msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		switch msg.MessageType {
		case "Attached":
			d := msg.device()
			known[d.DeviceID] = d
			select {
			case events <- Event{Type: EventAttached, UDID: d.UDID, ConnectionType: d.ConnectionType}:
			case <-ctx.Done():
				return ctx.Err()
			}
		case "Detached":
			d := known[msg.DeviceID]
			delete(known, msg.DeviceID)
			select {
			case events <- Event{Type: EventDetached, UDID: d.UDID}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
