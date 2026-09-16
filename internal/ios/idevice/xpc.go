package idevice

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// RemoteXPC is the message format the iOS 17+ developer services speak. It is
// Apple's XPC object serialisation carried over HTTP/2 streams.
//
// A message is a 24 byte little-endian header followed by an optional payload:
//
//	magic    uint32  0x29b00b92
//	flags    uint32
//	length   uint64  payload byte count
//	id       uint64  message id, echoed in the reply
//
// The payload is itself prefixed:
//
//	magic    uint32  0x42133742
//	version  uint32  5
//	root     object
//
// Every object is a 4 byte type tag followed by a type-specific body. All
// variable-length bodies are padded to a 4 byte boundary.
const (
	xpcMagic        uint32 = 0x29b00b92
	xpcPayloadMagic uint32 = 0x42133742
	xpcPayloadVer   uint32 = 5

	xpcHeaderLen  = 24
	xpcPayloadHdr = 8
)

// XPC message flags. DATA_PRESENT is the one that cannot be forgotten: a
// message whose header omits it while bytes follow is a protocol desync, and
// the device answers by resetting the connection. encodeXPC therefore derives
// it from the payload instead of trusting the caller.
const (
	xpcFlagAlwaysSet     uint32 = 0x00000001
	xpcFlagPing          uint32 = 0x00000002
	xpcFlagDataPresent   uint32 = 0x00000100
	xpcFlagWantingReply  uint32 = 0x00010000
	xpcFlagReply         uint32 = 0x00020000
	xpcFlagInitHandshake uint32 = 0x00400000
)

// XPC object type tags.
const (
	xpcTypeNull   uint32 = 0x00001000
	xpcTypeBool   uint32 = 0x00002000
	xpcTypeInt64  uint32 = 0x00003000
	xpcTypeUInt64 uint32 = 0x00004000
	xpcTypeDouble uint32 = 0x00005000
	xpcTypeData   uint32 = 0x00008000
	xpcTypeString uint32 = 0x00009000
	xpcTypeUUID   uint32 = 0x0000a000
	xpcTypeArray  uint32 = 0x0000e000
	xpcTypeDict   uint32 = 0x0000f000
)

// xpcUUID is a 16 byte XPC uuid. CoreDevice identifies its request/response
// pairs with these, and they must round trip as uuids rather than as data.
type xpcUUID [16]byte

// xpcMessage is one RemoteXPC message.
type xpcMessage struct {
	Flags uint32
	ID    uint64
	// Body is the decoded root object, nil when the message carries no payload
	// (which is how the protocol sends bare flag updates such as a ping).
	Body map[string]any
}

func align4(n int) int { return (n + 3) & ^3 }

// encodeXPC renders a message. A nil body produces a header-only message.
func encodeXPC(msg xpcMessage) ([]byte, error) {
	var payload []byte
	if msg.Body != nil {
		var buf bytes.Buffer
		buf.Grow(256)
		var hdr [xpcPayloadHdr]byte
		binary.LittleEndian.PutUint32(hdr[0:], xpcPayloadMagic)
		binary.LittleEndian.PutUint32(hdr[4:], xpcPayloadVer)
		buf.Write(hdr[:])
		if err := encodeXPCObject(&buf, msg.Body); err != nil {
			return nil, err
		}
		payload = buf.Bytes()
	}

	flags := msg.Flags
	if len(payload) > 0 {
		flags |= xpcFlagDataPresent
	}
	out := make([]byte, xpcHeaderLen+len(payload))
	binary.LittleEndian.PutUint32(out[0:], xpcMagic)
	binary.LittleEndian.PutUint32(out[4:], flags)
	binary.LittleEndian.PutUint64(out[8:], uint64(len(payload)))
	binary.LittleEndian.PutUint64(out[16:], msg.ID)
	copy(out[xpcHeaderLen:], payload)
	return out, nil
}

func encodeXPCObject(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case nil:
		return writeU32(buf, xpcTypeNull)
	case bool:
		if err := writeU32(buf, xpcTypeBool); err != nil {
			return err
		}
		var n uint32
		if val {
			n = 1
		}
		return writeU32(buf, n)
	case int:
		return encodeXPCObject(buf, int64(val))
	case int64:
		if err := writeU32(buf, xpcTypeInt64); err != nil {
			return err
		}
		return writeU64(buf, uint64(val))
	case uint64:
		if err := writeU32(buf, xpcTypeUInt64); err != nil {
			return err
		}
		return writeU64(buf, val)
	case float64:
		if err := writeU32(buf, xpcTypeDouble); err != nil {
			return err
		}
		return writeU64(buf, math.Float64bits(val))
	case string:
		if err := writeU32(buf, xpcTypeString); err != nil {
			return err
		}
		// The declared length includes the NUL terminator.
		raw := append([]byte(val), 0)
		if err := writeU32(buf, uint32(len(raw))); err != nil {
			return err
		}
		buf.Write(raw)
		buf.Write(make([]byte, align4(len(raw))-len(raw)))
		return nil
	case []byte:
		if err := writeU32(buf, xpcTypeData); err != nil {
			return err
		}
		if err := writeU32(buf, uint32(len(val))); err != nil {
			return err
		}
		buf.Write(val)
		buf.Write(make([]byte, align4(len(val))-len(val)))
		return nil
	case xpcUUID:
		if err := writeU32(buf, xpcTypeUUID); err != nil {
			return err
		}
		buf.Write(val[:])
		return nil
	case []any:
		body := &bytes.Buffer{}
		for _, item := range val {
			if err := encodeXPCObject(body, item); err != nil {
				return err
			}
		}
		if err := writeU32(buf, xpcTypeArray); err != nil {
			return err
		}
		// The length counts the entry-count field plus the entries.
		if err := writeU32(buf, uint32(body.Len()+4)); err != nil {
			return err
		}
		if err := writeU32(buf, uint32(len(val))); err != nil {
			return err
		}
		buf.Write(body.Bytes())
		return nil
	case map[string]any:
		body := &bytes.Buffer{}
		// Deterministic order keeps the encoding testable; XPC dictionaries are
		// unordered so the device does not care.
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			raw := append([]byte(k), 0)
			body.Write(raw)
			body.Write(make([]byte, align4(len(raw))-len(raw)))
			if err := encodeXPCObject(body, val[k]); err != nil {
				return err
			}
		}
		if err := writeU32(buf, xpcTypeDict); err != nil {
			return err
		}
		if err := writeU32(buf, uint32(body.Len()+4)); err != nil {
			return err
		}
		if err := writeU32(buf, uint32(len(val))); err != nil {
			return err
		}
		buf.Write(body.Bytes())
		return nil
	default:
		return fmt.Errorf("xpc: cannot encode %T", v)
	}
}

func writeU32(buf *bytes.Buffer, v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := buf.Write(b[:])
	return err
}

func writeU64(buf *bytes.Buffer, v uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, err := buf.Write(b[:])
	return err
}

// decodeXPC parses one complete message. It returns the number of bytes
// consumed so a stream reader can handle several messages in one buffer.
func decodeXPC(data []byte) (xpcMessage, int, error) {
	if len(data) < xpcHeaderLen {
		return xpcMessage{}, 0, errXPCShort
	}
	if magic := binary.LittleEndian.Uint32(data[0:]); magic != xpcMagic {
		return xpcMessage{}, 0, fmt.Errorf("xpc: bad magic 0x%08x", magic)
	}
	msg := xpcMessage{
		Flags: binary.LittleEndian.Uint32(data[4:]),
		ID:    binary.LittleEndian.Uint64(data[16:]),
	}
	length := binary.LittleEndian.Uint64(data[8:])
	if length > uint64(len(data)-xpcHeaderLen) {
		return xpcMessage{}, 0, errXPCShort
	}
	total := xpcHeaderLen + int(length)
	if length == 0 {
		return msg, total, nil
	}
	payload := data[xpcHeaderLen:total]
	if len(payload) < xpcPayloadHdr {
		return xpcMessage{}, 0, fmt.Errorf("xpc: truncated payload")
	}
	if magic := binary.LittleEndian.Uint32(payload[0:]); magic != xpcPayloadMagic {
		return xpcMessage{}, 0, fmt.Errorf("xpc: bad payload magic 0x%08x", magic)
	}
	root, _, err := decodeXPCObject(payload[xpcPayloadHdr:])
	if err != nil {
		return xpcMessage{}, 0, err
	}
	dict, ok := root.(map[string]any)
	if !ok && root != nil {
		return xpcMessage{}, 0, fmt.Errorf("xpc: root object is %T, want a dictionary", root)
	}
	msg.Body = dict
	return msg, total, nil
}

// errXPCShort marks an incomplete message, which is normal on a stream.
var errXPCShort = fmt.Errorf("xpc: incomplete message")

func decodeXPCObject(data []byte) (any, int, error) {
	if len(data) < 4 {
		return nil, 0, fmt.Errorf("xpc: truncated object")
	}
	typ := binary.LittleEndian.Uint32(data)
	rest := data[4:]
	switch typ {
	case xpcTypeNull:
		return nil, 4, nil
	case xpcTypeBool:
		if len(rest) < 4 {
			return nil, 0, fmt.Errorf("xpc: truncated bool")
		}
		return binary.LittleEndian.Uint32(rest) != 0, 8, nil
	case xpcTypeInt64:
		if len(rest) < 8 {
			return nil, 0, fmt.Errorf("xpc: truncated int64")
		}
		return int64(binary.LittleEndian.Uint64(rest)), 12, nil
	case xpcTypeUInt64:
		if len(rest) < 8 {
			return nil, 0, fmt.Errorf("xpc: truncated uint64")
		}
		return binary.LittleEndian.Uint64(rest), 12, nil
	case xpcTypeDouble:
		if len(rest) < 8 {
			return nil, 0, fmt.Errorf("xpc: truncated double")
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(rest)), 12, nil
	case xpcTypeUUID:
		if len(rest) < 16 {
			return nil, 0, fmt.Errorf("xpc: truncated uuid")
		}
		var u xpcUUID
		copy(u[:], rest)
		return u, 20, nil
	case xpcTypeString:
		if len(rest) < 4 {
			return nil, 0, fmt.Errorf("xpc: truncated string")
		}
		n := int(binary.LittleEndian.Uint32(rest))
		body := rest[4:]
		if n > len(body) {
			return nil, 0, fmt.Errorf("xpc: string length %d exceeds payload", n)
		}
		s := body[:n]
		// Drop the NUL terminator the length includes.
		s = bytes.TrimRight(s, "\x00")
		return string(s), 8 + align4(n), nil
	case xpcTypeData:
		if len(rest) < 4 {
			return nil, 0, fmt.Errorf("xpc: truncated data")
		}
		n := int(binary.LittleEndian.Uint32(rest))
		body := rest[4:]
		if n > len(body) {
			return nil, 0, fmt.Errorf("xpc: data length %d exceeds payload", n)
		}
		out := make([]byte, n)
		copy(out, body[:n])
		return out, 8 + align4(n), nil
	case xpcTypeArray:
		if len(rest) < 8 {
			return nil, 0, fmt.Errorf("xpc: truncated array")
		}
		size := int(binary.LittleEndian.Uint32(rest))
		count := int(binary.LittleEndian.Uint32(rest[4:]))
		if size < 4 || size-4 > len(rest)-8 {
			return nil, 0, fmt.Errorf("xpc: array size %d exceeds payload", size)
		}
		body := rest[8 : 8+size-4]
		items := make([]any, 0, count)
		for i := 0; i < count; i++ {
			item, n, err := decodeXPCObject(body)
			if err != nil {
				return nil, 0, err
			}
			items = append(items, item)
			body = body[n:]
		}
		return items, 8 + size, nil
	case xpcTypeDict:
		if len(rest) < 8 {
			return nil, 0, fmt.Errorf("xpc: truncated dictionary")
		}
		size := int(binary.LittleEndian.Uint32(rest))
		count := int(binary.LittleEndian.Uint32(rest[4:]))
		if size < 4 || size-4 > len(rest)-8 {
			return nil, 0, fmt.Errorf("xpc: dictionary size %d exceeds payload", size)
		}
		body := rest[8 : 8+size-4]
		out := make(map[string]any, count)
		for i := 0; i < count; i++ {
			end := bytes.IndexByte(body, 0)
			if end < 0 {
				return nil, 0, fmt.Errorf("xpc: unterminated dictionary key")
			}
			key := string(body[:end])
			body = body[align4(end+1):]
			val, n, err := decodeXPCObject(body)
			if err != nil {
				return nil, 0, err
			}
			out[key] = val
			body = body[n:]
		}
		return out, 8 + size, nil
	default:
		return nil, 0, fmt.Errorf("xpc: unsupported object type 0x%08x", typ)
	}
}
