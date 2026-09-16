package idevice

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// DTX is the message protocol every `com.apple.instruments.*` service speaks.
//
// A DTX message is a 32 byte header, a 16 byte payload header, an "auxiliary"
// argument list in Apple's DTXPrimitiveDictionary encoding, and an
// NSKeyedArchiver-encoded selector. Messages are multiplexed over numbered
// channels; channel 0 is the control channel used to open the others.
const (
	dtxMagic         uint32 = 0x1F3D5B79
	dtxHeaderLen     uint32 = 32
	dtxPayloadHdrLen        = 16

	// dtxAuxMagic prefixes a serialised DTXPrimitiveDictionary.
	dtxAuxMagic uint64 = 0x1F0

	// dtxMaxMessage bounds one reassembled message.
	dtxMaxMessage = 1 << 26
)

// DTX message types.
const (
	dtxTypeAck             uint32 = 0
	dtxTypeMethodInvoke    uint32 = 2
	dtxTypeReturnValue     uint32 = 3
	dtxTypeError           uint32 = 4
	dtxTypeCompressedOrNil uint32 = 5
)

// DTXPrimitiveDictionary value types.
const (
	dtxAuxKeyMarker uint32 = 10
	dtxAuxObject    uint32 = 2
	dtxAuxInt32     uint32 = 3
	dtxAuxInt64     uint32 = 4
	dtxAuxUInt32    uint32 = 5
	dtxAuxUInt64    uint32 = 6
)

// dtxAux is an ordered argument list for a DTX selector.
type dtxAux struct {
	buf bytes.Buffer
}

// AddObject appends an NSKeyedArchiver-encoded argument.
func (a *dtxAux) AddObject(v any) error {
	blob, err := archive(v)
	if err != nil {
		return err
	}
	_ = binary.Write(&a.buf, binary.LittleEndian, dtxAuxKeyMarker)
	_ = binary.Write(&a.buf, binary.LittleEndian, dtxAuxObject)
	_ = binary.Write(&a.buf, binary.LittleEndian, uint32(len(blob)))
	a.buf.Write(blob)
	return nil
}

// AddInt32 appends a primitive 32 bit integer argument.
func (a *dtxAux) AddInt32(v int32) {
	_ = binary.Write(&a.buf, binary.LittleEndian, dtxAuxKeyMarker)
	_ = binary.Write(&a.buf, binary.LittleEndian, dtxAuxInt32)
	_ = binary.Write(&a.buf, binary.LittleEndian, v)
}

// Bytes serialises the dictionary with its magic and length prefix.
func (a *dtxAux) Bytes() []byte {
	if a.buf.Len() == 0 {
		return nil
	}
	out := make([]byte, 16, 16+a.buf.Len())
	binary.LittleEndian.PutUint64(out[0:], dtxAuxMagic)
	binary.LittleEndian.PutUint64(out[8:], uint64(a.buf.Len()))
	return append(out, a.buf.Bytes()...)
}

// dtxMessage is one reassembled DTX message.
type dtxMessage struct {
	Identifier        uint32
	ConversationIndex uint32
	ChannelCode       int32
	ExpectsReply      bool
	MessageType       uint32
	Auxiliary         []byte
	// Selector is the decoded NSKeyedArchiver payload: a selector name for a
	// method invocation, a return value for a response, an *NSError for a
	// failure.
	Selector any
}

// dtxConn multiplexes DTX channels over one device service connection.
type dtxConn struct {
	conn net.Conn

	mu         sync.Mutex
	identifier uint32
	channel    int32
}

func newDTX(conn net.Conn) *dtxConn {
	// Identifiers start at 1; 0 is reserved for the implicit control channel
	// handshake and the device rejects a reused identifier.
	return &dtxConn{conn: conn, identifier: 0, channel: 0}
}

func (d *dtxConn) Close() error { return d.conn.Close() }

// send writes one message. A nil aux means the selector takes no arguments.
func (d *dtxConn) send(channel int32, expectsReply bool, selector string, aux *dtxAux) (uint32, error) {
	var payload []byte
	if selector != "" {
		blob, err := archive(selector)
		if err != nil {
			return 0, err
		}
		payload = blob
	}
	var auxBytes []byte
	if aux != nil {
		auxBytes = aux.Bytes()
	}

	d.mu.Lock()
	d.identifier++
	id := d.identifier
	d.mu.Unlock()

	total := len(auxBytes) + len(payload)
	frame := make([]byte, 0, int(dtxHeaderLen)+dtxPayloadHdrLen+total)
	header := make([]byte, dtxHeaderLen)
	binary.LittleEndian.PutUint32(header[0:], dtxMagic)
	binary.LittleEndian.PutUint32(header[4:], dtxHeaderLen)
	binary.LittleEndian.PutUint16(header[8:], 0)  // fragment index
	binary.LittleEndian.PutUint16(header[10:], 1) // fragment count
	binary.LittleEndian.PutUint32(header[12:], uint32(dtxPayloadHdrLen+total))
	binary.LittleEndian.PutUint32(header[16:], id)
	binary.LittleEndian.PutUint32(header[20:], 0) // conversation index
	binary.LittleEndian.PutUint32(header[24:], uint32(channel))
	var expects uint32
	if expectsReply {
		expects = 1
	}
	binary.LittleEndian.PutUint32(header[28:], expects)
	frame = append(frame, header...)

	payloadHeader := make([]byte, dtxPayloadHdrLen)
	binary.LittleEndian.PutUint32(payloadHeader[0:], dtxTypeMethodInvoke)
	binary.LittleEndian.PutUint32(payloadHeader[4:], uint32(len(auxBytes)))
	binary.LittleEndian.PutUint32(payloadHeader[8:], uint32(total))
	binary.LittleEndian.PutUint32(payloadHeader[12:], 0) // flags
	frame = append(frame, payloadHeader...)
	frame = append(frame, auxBytes...)
	frame = append(frame, payload...)

	if err := d.conn.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		return 0, err
	}
	if _, err := d.conn.Write(frame); err != nil {
		return 0, fmt.Errorf("dtx write: %w", err)
	}
	return id, nil
}

// receive reads one message, reassembling fragments.
func (d *dtxConn) receive(timeout time.Duration) (*dtxMessage, error) {
	if err := d.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	var body []byte
	var msg dtxMessage
	for {
		var header [32]byte
		if _, err := io.ReadFull(d.conn, header[:]); err != nil {
			return nil, fmt.Errorf("dtx read header: %w", err)
		}
		if magic := binary.LittleEndian.Uint32(header[0:]); magic != dtxMagic {
			return nil, fmt.Errorf("dtx: bad magic 0x%08X", magic)
		}
		headerLen := binary.LittleEndian.Uint32(header[4:])
		if headerLen != dtxHeaderLen {
			return nil, fmt.Errorf("dtx: unexpected header length %d", headerLen)
		}
		fragmentIndex := binary.LittleEndian.Uint16(header[8:])
		fragmentCount := binary.LittleEndian.Uint16(header[10:])
		length := binary.LittleEndian.Uint32(header[12:])
		if length > dtxMaxMessage {
			return nil, fmt.Errorf("dtx: oversized message %d", length)
		}

		msg.Identifier = binary.LittleEndian.Uint32(header[16:])
		msg.ConversationIndex = binary.LittleEndian.Uint32(header[20:])
		msg.ChannelCode = int32(binary.LittleEndian.Uint32(header[24:]))
		msg.ExpectsReply = binary.LittleEndian.Uint32(header[28:]) != 0

		// A fragmented message announces itself with an empty first fragment.
		if fragmentCount > 1 && fragmentIndex == 0 {
			continue
		}
		chunk := make([]byte, length)
		if _, err := io.ReadFull(d.conn, chunk); err != nil {
			return nil, fmt.Errorf("dtx read body: %w", err)
		}
		body = append(body, chunk...)
		if fragmentCount <= 1 || fragmentIndex == fragmentCount-1 {
			break
		}
		if len(body) > dtxMaxMessage {
			return nil, fmt.Errorf("dtx: oversized reassembled message %d", len(body))
		}
	}

	if len(body) == 0 {
		msg.MessageType = dtxTypeAck
		return &msg, nil
	}
	if len(body) < dtxPayloadHdrLen {
		return nil, fmt.Errorf("dtx: truncated payload header (%d bytes)", len(body))
	}
	msg.MessageType = binary.LittleEndian.Uint32(body[0:])
	auxLen := binary.LittleEndian.Uint32(body[4:])
	totalLen := binary.LittleEndian.Uint32(body[8:])
	payload := body[dtxPayloadHdrLen:]
	if uint32(len(payload)) < totalLen {
		return nil, fmt.Errorf("dtx: payload short by %d bytes", totalLen-uint32(len(payload)))
	}
	payload = payload[:totalLen]
	if auxLen > uint32(len(payload)) {
		return nil, fmt.Errorf("dtx: auxiliary length %d exceeds payload %d", auxLen, len(payload))
	}
	msg.Auxiliary = payload[:auxLen]
	if rest := payload[auxLen:]; len(rest) > 0 {
		decoded, err := unarchive(rest)
		if err != nil {
			return nil, err
		}
		msg.Selector = decoded
	}
	return &msg, nil
}

// call invokes selector on a channel and returns the response value.
func (d *dtxConn) call(channel int32, selector string, aux *dtxAux, timeout time.Duration) (any, error) {
	id, err := d.send(channel, true, selector, aux)
	if err != nil {
		return nil, err
	}
	for {
		msg, err := d.receive(timeout)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", selector, err)
		}
		// Services interleave unsolicited channel traffic with replies.
		if msg.Identifier != id {
			continue
		}
		if msg.MessageType == dtxTypeError {
			if e, ok := msg.Selector.(*NSError); ok {
				return nil, fmt.Errorf("%s: %w", selector, e)
			}
			return nil, fmt.Errorf("%s: device reported an error: %v", selector, msg.Selector)
		}
		if e, ok := msg.Selector.(*NSError); ok {
			return nil, fmt.Errorf("%s: %w", selector, e)
		}
		return msg.Selector, nil
	}
}

// makeChannel opens a named service channel and returns its code.
func (d *dtxConn) makeChannel(identifier string) (int32, error) {
	d.mu.Lock()
	d.channel++
	code := d.channel
	d.mu.Unlock()

	aux := &dtxAux{}
	aux.AddInt32(code)
	if err := aux.AddObject(identifier); err != nil {
		return 0, err
	}
	if _, err := d.call(0, "_requestChannelWithCode:identifier:", aux, ioTimeout); err != nil {
		return 0, fmt.Errorf("open DTX channel %s: %w", identifier, err)
	}
	return code, nil
}
