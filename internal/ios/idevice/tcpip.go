package idevice

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

// A CoreDevice tunnel is a point-to-point IPv6 link with no operating system
// involvement: datagrams are written to and read from the usbmuxd socket. To
// reach a TCP service on the device (Remote Service Discovery and everything it
// advertises) the host therefore needs its own TCP, because the kernel never
// sees the link.
//
// The implementation below is deliberately the smallest TCP that is still
// correct for this link:
//
//   - The link is a USB bulk pipe carrying whole datagrams, so there is no
//     fragmentation, no reordering and effectively no loss.
//   - Sending is stop-and-wait: one segment in flight, retransmitted on timeout.
//     RSD and the developer services exchange small messages, so a sliding
//     window would add risk without adding throughput.
//   - Receiving accepts in-order segments and drops anything else, re-ACKing so
//     the device retransmits.
const (
	ipv6HeaderLen = 40
	tcpHeaderLen  = 20

	protoTCP  uint8 = 6
	protoICMP uint8 = 58

	flagFIN uint8 = 0x01
	flagSYN uint8 = 0x02
	flagRST uint8 = 0x04
	flagPSH uint8 = 0x08
	flagACK uint8 = 0x10

	// tcpRetransmit is how long a segment waits for its ACK before being sent
	// again. The link is USB-local, so a lost segment is pathological.
	tcpRetransmit = 500 * time.Millisecond

	// tcpWindow is advertised to the device. It is the largest value the 16 bit
	// header field can carry; this stack sends one segment at a time but the
	// device may stream replies.
	tcpWindow uint16 = 0xFFFF
)

// tunnelStack demultiplexes the TCP connections sharing one tunnel.
type tunnelStack struct {
	tun    *tunnel
	local  netip.Addr
	remote netip.Addr
	mtu    int

	mu       sync.Mutex
	conns    map[uint16]*tcpConn
	nextPort uint16
	closed   bool
	err      error

	done chan struct{}
}

func newTunnelStack(tun *tunnel) (*tunnelStack, error) {
	local, err := netip.ParseAddr(tun.info.LocalAddress)
	if err != nil {
		return nil, fmt.Errorf("tunnel local address %q: %w", tun.info.LocalAddress, err)
	}
	remote, err := netip.ParseAddr(tun.info.ServerAddress)
	if err != nil {
		return nil, fmt.Errorf("tunnel device address %q: %w", tun.info.ServerAddress, err)
	}
	mtu := tun.info.MTU
	if mtu <= ipv6HeaderLen+tcpHeaderLen {
		mtu = tunnelMTU
	}
	s := &tunnelStack{
		tun:      tun,
		local:    local,
		remote:   remote,
		mtu:      mtu,
		conns:    map[uint16]*tcpConn{},
		nextPort: 51000,
		done:     make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// mss is the largest TCP payload that fits the link MTU.
func (s *tunnelStack) mss() int { return s.mtu - ipv6HeaderLen - tcpHeaderLen }

func (s *tunnelStack) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]*tcpConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		c.fail(net.ErrClosed)
	}
	return s.tun.Close()
}

// readLoop reassembles datagrams from the tunnel and routes TCP segments.
func (s *tunnelStack) readLoop() {
	defer close(s.done)
	buf := make([]byte, s.mtu+ipv6HeaderLen)
	for {
		n, err := s.tun.conn.Read(buf)
		if err != nil {
			s.failAll(err)
			return
		}
		s.dispatch(buf[:n])
	}
}

func (s *tunnelStack) failAll(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	conns := make([]*tcpConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.fail(err)
	}
}

func (s *tunnelStack) dispatch(pkt []byte) {
	if len(pkt) < ipv6HeaderLen || pkt[0]>>4 != 6 {
		return
	}
	if pkt[6] != protoTCP {
		// ICMPv6 (the device announces itself with MLD reports) needs no reply
		// on a point-to-point link with statically assigned addresses.
		return
	}
	payloadLen := int(binary.BigEndian.Uint16(pkt[4:]))
	if payloadLen < tcpHeaderLen || ipv6HeaderLen+payloadLen > len(pkt) {
		return
	}
	seg := pkt[ipv6HeaderLen : ipv6HeaderLen+payloadLen]
	dstPort := binary.BigEndian.Uint16(seg[2:])

	s.mu.Lock()
	c := s.conns[dstPort]
	s.mu.Unlock()
	if c != nil {
		c.deliver(seg)
	}
}

// send writes one TCP segment for c.
func (s *tunnelStack) send(c *tcpConn, flags uint8, seq, ack uint32, payload []byte) error {
	total := ipv6HeaderLen + tcpHeaderLen + len(payload)
	pkt := make([]byte, total)

	pkt[0] = 6 << 4
	binary.BigEndian.PutUint16(pkt[4:], uint16(tcpHeaderLen+len(payload)))
	pkt[6] = protoTCP
	pkt[7] = 64
	copy(pkt[8:], s.local.AsSlice())
	copy(pkt[24:], s.remote.AsSlice())

	tcp := pkt[ipv6HeaderLen:]
	binary.BigEndian.PutUint16(tcp[0:], c.localPort)
	binary.BigEndian.PutUint16(tcp[2:], c.remotePort)
	binary.BigEndian.PutUint32(tcp[4:], seq)
	binary.BigEndian.PutUint32(tcp[8:], ack)
	tcp[12] = (tcpHeaderLen / 4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:], tcpWindow)
	copy(tcp[tcpHeaderLen:], payload)
	binary.BigEndian.PutUint16(tcp[16:], tcpChecksum(s.local, s.remote, tcp))

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if _, err := s.tun.conn.Write(pkt); err != nil {
		return fmt.Errorf("tunnel write: %w", err)
	}
	return nil
}

// tcpChecksum computes the TCP checksum over the IPv6 pseudo-header.
func tcpChecksum(src, dst netip.Addr, tcp []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	s16 := src.AsSlice()
	d16 := dst.AsSlice()
	add(s16)
	add(d16)

	var meta [8]byte
	binary.BigEndian.PutUint32(meta[0:], uint32(len(tcp)))
	meta[7] = protoTCP
	add(meta[:])

	// The checksum field itself must read as zero.
	saved := binary.BigEndian.Uint16(tcp[16:])
	binary.BigEndian.PutUint16(tcp[16:], 0)
	add(tcp)
	binary.BigEndian.PutUint16(tcp[16:], saved)

	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// tcpConn is one TCP connection over the tunnel. It satisfies net.Conn.
type tcpConn struct {
	stack      *tunnelStack
	localPort  uint16
	remotePort uint16

	mu   sync.Mutex
	cond *sync.Cond

	rcvNxt      uint32
	sndNxt      uint32
	sndUna      uint32
	rcv         []byte
	finSeen     bool
	established bool
	err         error

	readDeadline  time.Time
	writeDeadline time.Time
	timer         *time.Timer
}

// dialTunnelTCP opens a TCP connection to a port on the device.
func (s *tunnelStack) dialTunnelTCP(port int, timeout time.Duration) (*tcpConn, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("tunnel dial: invalid port %d", port)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, net.ErrClosed
	}
	localPort := s.nextPort
	s.nextPort++
	c := &tcpConn{
		stack:      s,
		localPort:  localPort,
		remotePort: uint16(port),
		// A fixed initial sequence number is safe here: the link is
		// point-to-point and every connection uses a fresh local port.
		sndNxt: 1,
		sndUna: 1,
	}
	c.cond = sync.NewCond(&c.mu)
	s.conns[localPort] = c
	s.mu.Unlock()

	if err := s.send(c, flagSYN, c.sndNxt, 0, nil); err != nil {
		s.release(localPort)
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for !c.established && c.err == nil {
		if time.Now().After(deadline) {
			c.mu.Unlock()
			s.release(localPort)
			c.mu.Lock()
			return nil, fmt.Errorf("tunnel dial [%s]:%d: timed out waiting for SYN-ACK", s.remote, port)
		}
		c.waitLocked(200 * time.Millisecond)
	}
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		s.release(localPort)
		c.mu.Lock()
		return nil, err
	}
	return c, nil
}

func (s *tunnelStack) release(port uint16) {
	s.mu.Lock()
	delete(s.conns, port)
	s.mu.Unlock()
}

// waitLocked blocks until the connection changes or d elapses.
func (c *tcpConn) waitLocked(d time.Duration) {
	t := time.AfterFunc(d, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	c.cond.Wait()
	t.Stop()
}

func (c *tcpConn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// deliver processes one inbound segment.
func (c *tcpConn) deliver(seg []byte) {
	seq := binary.BigEndian.Uint32(seg[4:])
	ack := binary.BigEndian.Uint32(seg[8:])
	flags := seg[13]
	dataOff := int(seg[12]>>4) * 4
	if dataOff < tcpHeaderLen || dataOff > len(seg) {
		return
	}
	payload := seg[dataOff:]

	c.mu.Lock()
	switch {
	case flags&flagRST != 0:
		if c.err == nil {
			c.err = fmt.Errorf("connection reset by device on port %d", c.remotePort)
		}
		c.cond.Broadcast()
		c.mu.Unlock()
		return

	case flags&flagSYN != 0 && flags&flagACK != 0:
		if !c.established {
			c.rcvNxt = seq + 1
			c.sndUna = ack
			c.sndNxt = ack
			c.established = true
			c.cond.Broadcast()
			c.mu.Unlock()
			// Complete the handshake.
			_ = c.stack.send(c, flagACK, c.sndNxt, c.rcvNxt, nil)
			return
		}
	}

	if flags&flagACK != 0 && seqGE(ack, c.sndUna) {
		c.sndUna = ack
	}

	var needAck bool
	if len(payload) > 0 {
		if seq == c.rcvNxt {
			c.rcv = append(c.rcv, payload...)
			c.rcvNxt += uint32(len(payload))
		}
		// Out-of-order data is dropped; the ACK below tells the device what is
		// still expected so it retransmits.
		needAck = true
	}
	if flags&flagFIN != 0 && seq+uint32(len(payload)) == c.rcvNxt {
		c.finSeen = true
		c.rcvNxt++
		needAck = true
	}
	sndNxt, rcvNxt := c.sndNxt, c.rcvNxt
	c.cond.Broadcast()
	c.mu.Unlock()

	if needAck {
		_ = c.stack.send(c, flagACK, sndNxt, rcvNxt, nil)
	}
}

// seqGE reports a >= b in TCP sequence arithmetic.
func seqGE(a, b uint32) bool { return int32(a-b) >= 0 }

func (c *tcpConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if len(c.rcv) > 0 {
			n := copy(p, c.rcv)
			c.rcv = c.rcv[n:]
			return n, nil
		}
		if c.err != nil {
			return 0, c.err
		}
		if c.finSeen {
			return 0, io.EOF
		}
		if !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline) {
			return 0, os.ErrDeadlineExceeded
		}
		wait := 200 * time.Millisecond
		if !c.readDeadline.IsZero() {
			if until := time.Until(c.readDeadline); until < wait {
				wait = until
			}
		}
		if wait <= 0 {
			wait = time.Millisecond
		}
		c.waitLocked(wait)
	}
}

func (c *tcpConn) Write(p []byte) (int, error) {
	mss := c.stack.mss()
	written := 0
	for written < len(p) {
		end := written + mss
		if end > len(p) {
			end = len(p)
		}
		if err := c.writeSegment(p[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

// writeSegment sends one segment and waits for its acknowledgement,
// retransmitting until the device takes it.
func (c *tcpConn) writeSegment(payload []byte) error {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	seq := c.sndNxt
	want := seq + uint32(len(payload))
	ack := c.rcvNxt
	deadline := c.writeDeadline
	c.sndNxt = want
	c.mu.Unlock()

	if err := c.stack.send(c, flagPSH|flagACK, seq, ack, payload); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for !seqGE(c.sndUna, want) {
		if c.err != nil {
			return c.err
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return os.ErrDeadlineExceeded
		}
		c.waitLocked(tcpRetransmit)
		if !seqGE(c.sndUna, want) && c.err == nil {
			rcv := c.rcvNxt
			c.mu.Unlock()
			err := c.stack.send(c, flagPSH|flagACK, seq, rcv, payload)
			c.mu.Lock()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *tcpConn) Close() error {
	c.mu.Lock()
	seq, ack := c.sndNxt, c.rcvNxt
	closed := c.err != nil
	if c.err == nil {
		c.err = net.ErrClosed
	}
	c.cond.Broadcast()
	c.mu.Unlock()

	if !closed {
		_ = c.stack.send(c, flagFIN|flagACK, seq, ack, nil)
	}
	c.stack.release(c.localPort)
	return nil
}

func (c *tcpConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: c.stack.local.AsSlice(), Port: int(c.localPort)}
}

func (c *tcpConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: c.stack.remote.AsSlice(), Port: int(c.remotePort)}
}

func (c *tcpConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline, c.writeDeadline = t, t
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *tcpConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *tcpConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

var _ net.Conn = (*tcpConn)(nil)
