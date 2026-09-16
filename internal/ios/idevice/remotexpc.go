package idevice

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"
)

// RemoteXPC is the protocol behind the CoreDevice tunnel. Apple layers it on
// HTTP/2: the client opens a connection, and each logical channel is a pair of
// HTTP/2 streams carrying XPC messages as DATA frames.
//
// Only the framing subset RSD uses is implemented here, because RSD does not
// negotiate anything beyond it: no header compression state is ever needed (the
// peers exchange empty HEADERS), no flow control window updates are required for
// messages this small, and no stream multiplexing beyond the two fixed channels.
const (
	// http2Preface is the mandatory connection preamble.
	http2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	frameData         uint8 = 0x0
	frameHeaders      uint8 = 0x1
	frameSettings     uint8 = 0x4
	framePing         uint8 = 0x6
	frameGoAway       uint8 = 0x7
	frameWindowUpdate uint8 = 0x8

	flagAck        uint8 = 0x1
	flagEndStream  uint8 = 0x1
	flagEndHeaders uint8 = 0x4

	// rootChannel carries service discovery, replyChannel the responses. RSD
	// fixes both stream identifiers.
	rootChannel  uint32 = 1
	replyChannel uint32 = 3

	http2MaxFrame = 1 << 24
)

// http2Conn is the minimal HTTP/2 client RemoteXPC needs.
type http2Conn struct {
	conn net.Conn
	// buf accumulates DATA payload per stream, because one XPC message may span
	// several frames.
	buf map[uint32][]byte
}

func newHTTP2(conn net.Conn) (*http2Conn, error) {
	h := &http2Conn{conn: conn, buf: map[uint32][]byte{}}
	if _, err := conn.Write([]byte(http2Preface)); err != nil {
		return nil, fmt.Errorf("http2 preface: %w", err)
	}
	// An empty SETTINGS frame accepts the peer's defaults.
	if err := h.writeFrame(frameSettings, 0, 0, nil); err != nil {
		return nil, err
	}
	// Give the device room to stream without waiting for window updates.
	var inc [4]byte
	binary.BigEndian.PutUint32(inc[:], 1<<20)
	if err := h.writeFrame(frameWindowUpdate, 0, 0, inc[:]); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *http2Conn) writeFrame(kind uint8, flags uint8, stream uint32, payload []byte) error {
	hdr := make([]byte, 9+len(payload))
	hdr[0] = byte(len(payload) >> 16)
	hdr[1] = byte(len(payload) >> 8)
	hdr[2] = byte(len(payload))
	hdr[3] = kind
	hdr[4] = flags
	binary.BigEndian.PutUint32(hdr[5:], stream)
	copy(hdr[9:], payload)
	if _, err := h.conn.Write(hdr); err != nil {
		return fmt.Errorf("http2 write frame: %w", err)
	}
	return nil
}

// openStream announces a new stream with an empty HEADERS frame, which is what
// RSD expects: it never inspects request headers.
func (h *http2Conn) openStream(stream uint32) error {
	return h.writeFrame(frameHeaders, flagEndHeaders, stream, nil)
}

type http2Frame struct {
	Kind    uint8
	Flags   uint8
	Stream  uint32
	Payload []byte
}

func (h *http2Conn) readFrame(timeout time.Duration) (http2Frame, error) {
	if err := h.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return http2Frame{}, err
	}
	var hdr [9]byte
	if _, err := io.ReadFull(h.conn, hdr[:]); err != nil {
		return http2Frame{}, fmt.Errorf("http2 read frame header: %w", err)
	}
	length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
	if length > http2MaxFrame {
		return http2Frame{}, fmt.Errorf("http2: oversized frame %d", length)
	}
	f := http2Frame{
		Kind:   hdr[3],
		Flags:  hdr[4],
		Stream: binary.BigEndian.Uint32(hdr[5:]) & 0x7FFFFFFF,
	}
	if length > 0 {
		f.Payload = make([]byte, length)
		if _, err := io.ReadFull(h.conn, f.Payload); err != nil {
			return http2Frame{}, fmt.Errorf("http2 read frame payload: %w", err)
		}
	}
	return f, nil
}

// readXPCAny pumps frames until a complete XPC message arrives on any stream and
// returns it together with the stream that carried it.
//
// The housekeeping frames (SETTINGS, PING) are answered here because leaving
// them unanswered stalls the connection.
func (h *http2Conn) readXPCAny(timeout time.Duration) (uint32, xpcMessage, error) {
	deadline := time.Now().Add(timeout)
	for {
		// Drain first: one DATA frame can carry several messages, and the
		// leftovers must be decoded before blocking on another frame that the
		// device has no reason to send.
		for stream, buf := range h.buf {
			if len(buf) == 0 {
				continue
			}
			msg, n, err := decodeXPC(buf)
			if err == errXPCShort {
				continue
			}
			if err != nil {
				return 0, xpcMessage{}, err
			}
			h.buf[stream] = buf[n:]
			return stream, msg, nil
		}

		if remaining := time.Until(deadline); remaining <= 0 {
			return 0, xpcMessage{}, fmt.Errorf("xpc: timed out waiting for a message")
		}
		f, err := h.readFrame(time.Until(deadline))
		if err != nil {
			return 0, xpcMessage{}, err
		}
		switch f.Kind {
		case frameSettings:
			if f.Flags&flagAck == 0 {
				if err := h.writeFrame(frameSettings, flagAck, 0, nil); err != nil {
					return 0, xpcMessage{}, err
				}
			}
		case framePing:
			if f.Flags&flagAck == 0 {
				if err := h.writeFrame(framePing, flagAck, 0, f.Payload); err != nil {
					return 0, xpcMessage{}, err
				}
			}
		case frameGoAway:
			return 0, xpcMessage{}, fmt.Errorf("xpc: device closed the RemoteXPC connection (GOAWAY)")
		case frameData:
			h.buf[f.Stream] = append(h.buf[f.Stream], f.Payload...)
		}
	}
}

// readXPC waits for a complete message on one specific stream, discarding
// messages that arrive on the others.
func (h *http2Conn) readXPC(stream uint32, timeout time.Duration) (xpcMessage, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return xpcMessage{}, fmt.Errorf("xpc: timed out waiting for a reply on stream %d", stream)
		}
		got, msg, err := h.readXPCAny(remaining)
		if err != nil {
			return xpcMessage{}, err
		}
		if got == stream {
			return msg, nil
		}
	}
}

func (h *http2Conn) writeXPC(stream uint32, msg xpcMessage) error {
	payload, err := encodeXPC(msg)
	if err != nil {
		return err
	}
	return h.writeFrame(frameData, 0, stream, payload)
}

func (h *http2Conn) Close() error { return h.conn.Close() }

// RSDService is one service advertised by Remote Service Discovery.
type RSDService struct {
	Name string
	Port int
}

// rsdClient is a Remote Service Discovery session over a tunnel.
type rsdClient struct {
	h        *http2Conn
	services map[string]RSDService
	// Properties is the device information RSD reports alongside its services.
	Properties map[string]any
}

// dialRSD connects to the RSD port over the tunnel and reads the service list.
//
// RSD announces itself unprompted: after the HTTP/2 handshake it sends a
// handshake message on the root channel whose payload contains every service and
// its port. Nothing has to be requested to obtain it.
func dialRSD(stack *tunnelStack, port int) (*rsdClient, error) {
	conn, err := stack.dialTunnelTCP(port, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to RSD port %d: %w", port, err)
	}
	h, err := newHTTP2(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := h.openStream(rootChannel); err != nil {
		conn.Close()
		return nil, err
	}
	// An empty message with the "always set" flag opens the root channel.
	if err := h.writeXPC(rootChannel, xpcMessage{Flags: xpcFlagAlwaysSet}); err != nil {
		conn.Close()
		return nil, err
	}
	if err := h.openStream(replyChannel); err != nil {
		conn.Close()
		return nil, err
	}
	if err := h.writeXPC(replyChannel, xpcMessage{Flags: xpcFlagInitHandshake | xpcFlagAlwaysSet}); err != nil {
		conn.Close()
		return nil, err
	}

	c := &rsdClient{h: h, services: map[string]RSDService{}}
	// The handshake payload arrives on the root channel, possibly after a few
	// bookkeeping messages.
	deadline := time.Now().Add(30 * time.Second)
	for len(c.services) == 0 {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("RSD did not advertise any service")
		}
		msg, err := h.readXPC(rootChannel, time.Until(deadline))
		if err != nil {
			conn.Close()
			return nil, err
		}
		c.absorb(msg.Body)
	}
	return c, nil
}

// absorb records the services and properties from an RSD handshake payload.
func (c *rsdClient) absorb(body any) {
	dict, ok := body.(map[string]any)
	if !ok {
		return
	}
	if props, ok := dict["Properties"].(map[string]any); ok {
		c.Properties = props
	}
	services, ok := dict["Services"].(map[string]any)
	if !ok {
		return
	}
	for name, raw := range services {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		portStr, _ := entry["Port"].(string)
		port := 0
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port == 0 {
			continue
		}
		c.services[name] = RSDService{Name: name, Port: port}
	}
}

func (c *rsdClient) Close() error { return c.h.Close() }

// Service looks up an advertised service by its exact name.
func (c *rsdClient) Service(name string) (RSDService, bool) {
	svc, ok := c.services[name]
	return svc, ok
}

// Names returns every advertised service name, sorted.
func (c *rsdClient) Names() []string {
	names := make([]string, 0, len(c.services))
	for name := range c.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// find returns the first service whose name contains substr, which tolerates the
// version suffixes Apple appends (".shim.remote", ".v2", ...).
func (c *rsdClient) find(substr string) (RSDService, bool) {
	for _, name := range c.Names() {
		if strings.Contains(name, substr) {
			return c.services[name], true
		}
	}
	return RSDService{}, false
}

// xpcConn is a RemoteXPC channel to one service behind RSD.
//
// The channel is the same shape RSD itself uses: the root stream carries
// requests and replies, the reply stream exists only so the peer can acknowledge
// the handshake. Message identifiers increment per request and the device echoes
// them, which is what lets request() match a reply to its call.
type xpcConn struct {
	h  *http2Conn
	id uint64
}

// handshake performs the channel opening exchange every RemoteXPC service
// expects before it will accept a request.
func (c *xpcConn) handshake() error {
	if err := c.h.openStream(rootChannel); err != nil {
		return err
	}
	if err := c.h.writeXPC(rootChannel, xpcMessage{Flags: xpcFlagAlwaysSet}); err != nil {
		return err
	}
	if err := c.h.openStream(replyChannel); err != nil {
		return err
	}
	return c.h.writeXPC(replyChannel, xpcMessage{Flags: xpcFlagInitHandshake | xpcFlagAlwaysSet})
}

// request sends one message and returns the body of its reply.
//
// The reply does not necessarily come back on the stream the request went out
// on: CoreDevice answers on the reply channel while RSD answers on the root
// channel, so both are accepted and the message id is what ties a reply to its
// call.
func (c *xpcConn) request(body map[string]any, timeout time.Duration) (any, error) {
	c.id++
	id := c.id
	if err := c.h.writeXPC(rootChannel, xpcMessage{
		Flags: xpcFlagAlwaysSet | xpcFlagWantingReply,
		ID:    id,
		Body:  body,
	}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("xpc: timed out waiting for a reply to message %d", id)
		}
		_, msg, err := c.h.readXPCAny(remaining)
		if err != nil {
			return nil, err
		}
		// Skip the protocol's own bookkeeping. Handshake and keepalive messages
		// carry either no body or an empty dictionary, and returning one of
		// those as the reply loses the real answer that arrives afterwards.
		if len(msg.Body) == 0 {
			continue
		}
		// The device echoes the request id on its reply and tags it REPLY. A
		// message with neither belongs to some other exchange on this channel.
		if msg.ID != id && msg.Flags&xpcFlagReply == 0 {
			continue
		}
		return msg.Body, nil
	}
}

func (c *xpcConn) Close() error { return c.h.Close() }
