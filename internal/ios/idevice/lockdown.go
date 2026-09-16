package idevice

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"howett.net/plist"
)

// plistConn is a length-prefixed plist channel. lockdownd and every
// lockdown-started service (AFC excepted) use the same framing: a 32 bit
// big-endian byte count followed by one plist.
type plistConn struct {
	conn net.Conn
}

const plistServiceMaxFrame = 1 << 26 // 64 MiB; real messages are kilobytes

func (c *plistConn) Close() error { return c.conn.Close() }

func (c *plistConn) send(request any) error {
	body, err := plist.Marshal(request, plist.XMLFormat)
	if err != nil {
		return fmt.Errorf("encode plist request: %w", err)
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	if err := c.conn.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	if _, err := c.conn.Write(buf); err != nil {
		return fmt.Errorf("write plist request: %w", err)
	}
	return nil
}

func (c *plistConn) receive(out any) error {
	return c.receiveTimeout(out, ioTimeout)
}

func (c *plistConn) receiveTimeout(out any, timeout time.Duration) error {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	var length [4]byte
	if _, err := io.ReadFull(c.conn, length[:]); err != nil {
		return fmt.Errorf("read plist frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(length[:])
	if n == 0 || n > plistServiceMaxFrame {
		return fmt.Errorf("plist service: bogus frame length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return fmt.Errorf("read plist frame body: %w", err)
	}
	if _, err := plist.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode plist frame: %w", err)
	}
	return nil
}

// upgradeTLS replaces the transport with a TLS session over the same socket.
func (c *plistConn) upgradeTLS(cfg *tls.Config) error {
	tlsConn := tls.Client(c.conn, cfg)
	if err := tlsConn.SetDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("lockdown TLS handshake: %w", err)
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	c.conn = tlsConn
	return nil
}

// lockdownConn is a lockdownd client for one device.
type lockdownConn struct {
	plistConn
	device    muxDevice
	pair      *PairRecord
	sessionID string
	tls       *tls.Config
}

const lockdownLabel = "ripley"

type lockdownReply struct {
	Request          string `plist:"Request"`
	Result           string `plist:"Result"`
	Error            string `plist:"Error"`
	Type             string `plist:"Type"`
	SessionID        string `plist:"SessionID"`
	EnableSessionSSL bool   `plist:"EnableSessionSSL"`
	Port             uint64 `plist:"Port"`
	EnableServiceSSL bool   `plist:"EnableServiceSSL"`
	Service          string `plist:"Service"`
	Key              string `plist:"Key"`
	Value            any    `plist:"Value"`
}

func (r lockdownReply) err(what string) error {
	if r.Error == "" {
		return nil
	}
	return fmt.Errorf("lockdownd %s: %s%s", what, r.Error, lockdownHint(r.Error))
}

func lockdownHint(code string) string {
	switch code {
	case "InvalidHostID":
		return " (the stored pairing record is stale: unpair and trust this computer on the device again)"
	case "PasswordProtected":
		return " (unlock the device and tap Trust)"
	case "InvalidService":
		return " (the device does not offer this service; on iOS 17 and newer the developer services moved behind the CoreDevice tunnel)"
	case "SessionInactive":
		return " (lockdownd dropped the session; reconnect)"
	}
	return ""
}

// dialLockdown opens lockdownd on the device and validates that it really is
// lockdownd on the other end.
func dialLockdown(device muxDevice) (*lockdownConn, error) {
	conn, err := connectDevice(device.DeviceID, lockdownPort)
	if err != nil {
		return nil, err
	}
	ld := &lockdownConn{plistConn: plistConn{conn: conn}, device: device}
	kind, err := ld.QueryType()
	if err != nil {
		ld.Close()
		return nil, err
	}
	if kind != "com.apple.mobile.lockdown" {
		ld.Close()
		return nil, fmt.Errorf("lockdownd on %s reported unexpected type %q", device.UDID, kind)
	}
	return ld, nil
}

// QueryType asks lockdownd what it is. It is the cheapest liveness probe and
// works without a pairing record.
func (l *lockdownConn) QueryType() (string, error) {
	if err := l.send(map[string]any{"Request": "QueryType", "Label": lockdownLabel}); err != nil {
		return "", err
	}
	var reply lockdownReply
	if err := l.receive(&reply); err != nil {
		return "", err
	}
	if err := reply.err("QueryType"); err != nil {
		return "", err
	}
	return reply.Type, nil
}

// GetValue reads one lockdownd value. Domain may be empty for the root domain.
func (l *lockdownConn) GetValue(domain, key string) (any, error) {
	req := map[string]any{"Request": "GetValue", "Label": lockdownLabel}
	if domain != "" {
		req["Domain"] = domain
	}
	if key != "" {
		req["Key"] = key
	}
	if err := l.send(req); err != nil {
		return nil, err
	}
	var reply lockdownReply
	if err := l.receive(&reply); err != nil {
		return nil, err
	}
	if err := reply.err("GetValue " + key); err != nil {
		return nil, err
	}
	return reply.Value, nil
}

func (l *lockdownConn) getString(key string) string {
	v, err := l.GetValue("", key)
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// StartSession authenticates with the pairing record and, when lockdownd asks
// for it, switches the connection to TLS. Every privileged request (including
// StartService for developer services) requires an active session.
func (l *lockdownConn) StartSession(pair *PairRecord) error {
	if pair == nil {
		return fmt.Errorf("%w for %s", ErrNoPairRecord, l.device.UDID)
	}
	cfg, err := pair.tlsConfig()
	if err != nil {
		return err
	}
	buid := pair.SystemBUID
	if buid == "" {
		if buid, err = readBUID(); err != nil {
			return err
		}
	}
	if err := l.send(map[string]any{
		"Request":    "StartSession",
		"Label":      lockdownLabel,
		"HostID":     pair.HostID,
		"SystemBUID": buid,
	}); err != nil {
		return err
	}
	var reply lockdownReply
	if err := l.receive(&reply); err != nil {
		return err
	}
	if err := reply.err("StartSession"); err != nil {
		return err
	}
	l.sessionID = reply.SessionID
	l.pair = pair
	l.tls = cfg
	if reply.EnableSessionSSL {
		if err := l.upgradeTLS(cfg); err != nil {
			return err
		}
	}
	return nil
}

// StopSession ends the lockdownd session. It is best effort: the device drops
// the session anyway when the socket closes.
func (l *lockdownConn) StopSession() {
	if l.sessionID == "" {
		return
	}
	_ = l.send(map[string]any{
		"Request":   "StopSession",
		"Label":     lockdownLabel,
		"SessionID": l.sessionID,
	})
	var reply lockdownReply
	_ = l.receive(&reply)
	l.sessionID = ""
}

// service is a live connection to a lockdown-started device service.
type service struct {
	plistConn
	name string
}

// StartService asks lockdownd for a service port, opens it through usbmuxd and
// applies TLS when the device demands it.
//
// EnableServiceSSL is honoured exactly: modern iOS returns it for
// installation_proxy and AFC, and the service will simply time out if the
// client speaks cleartext to a TLS port (or vice versa).
func (l *lockdownConn) StartService(name string) (*service, error) {
	if l.sessionID == "" {
		return nil, fmt.Errorf("StartService %s: no active lockdownd session", name)
	}
	if err := l.send(map[string]any{
		"Request": "StartService",
		"Label":   lockdownLabel,
		"Service": name,
	}); err != nil {
		return nil, err
	}
	var reply lockdownReply
	if err := l.receive(&reply); err != nil {
		return nil, err
	}
	if err := reply.err("StartService " + name); err != nil {
		return nil, err
	}
	if reply.Port == 0 || reply.Port > 65535 {
		return nil, fmt.Errorf("lockdownd StartService %s returned port %d", name, reply.Port)
	}
	// Decide about TLS before spending a device connection on the port: a
	// service that demands SSL is unusable without the pairing certificates,
	// and speaking cleartext to it would simply hang.
	if reply.EnableServiceSSL && l.tls == nil {
		return nil, fmt.Errorf("service %s requires SSL but no pairing certificates are available", name)
	}
	conn, err := connectDevice(l.device.DeviceID, uint16(reply.Port))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	svc := &service{plistConn: plistConn{conn: conn}, name: name}
	if reply.EnableServiceSSL {
		if err := svc.upgradeTLS(l.tls); err != nil {
			conn.Close()
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
	}
	return svc, nil
}

// errServiceUnavailable marks a StartService rejection, which is how iOS 17+
// reports that a developer service has moved behind the CoreDevice tunnel.
var errServiceUnavailable = errors.New("service unavailable")
