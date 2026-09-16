package storyboard

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// nibObject and the encoder below implement the compact NIBArchive format used
// by UIKit. The format is intentionally kept private to this package: the public
// surface is CompileLaunchStoryboard, not a general-purpose nib API.
type nibObject struct {
	class string
	props []nibProperty
}

type nibProperty struct {
	key   string
	value any
}

type nibInt8 int8
type nibInt16 int16
type nibInt32 int32
type nibInt64 int64
type nibFloat float32
type nibDouble float64
type nibData []byte
type nibNil struct{}

func nibObj(class string) *nibObject { return &nibObject{class: class} }

func (o *nibObject) add(key string, value any) *nibObject {
	if value != nil {
		o.props = append(o.props, nibProperty{key: key, value: value})
	}
	return o
}

func nibString(s string) *nibObject {
	return nibObj("NSString").add("NS.bytes", nibData([]byte(s)))
}

func nibArray(items []*nibObject, mutable bool) *nibObject {
	class := "NSArray"
	if mutable {
		class = "NSMutableArray"
	}
	o := nibObj(class).add("NSInlinedValue", false)
	for _, item := range items {
		if item != nil {
			o.add("UINibEncoderEmptyKey", item)
		}
	}
	return o
}

func nibEmptyDictionary() *nibObject {
	return nibObj("NSDictionary").add("NSInlinedValue", false)
}

func nibStruct(values ...float64) nibData {
	// UIKit stores CGPoint/CGSize/CGRect values in the data value kind. The
	// first byte is the element encoding (0x07 = double), followed by little
	// endian doubles.
	data := make([]byte, 1+8*len(values))
	data[0] = 0x07
	for i, v := range values {
		binary.LittleEndian.PutUint64(data[1+i*8:], math.Float64bits(v))
	}
	return nibData(data)
}

const (
	nibTypeInt8      = 0
	nibTypeInt16     = 1
	nibTypeInt32     = 2
	nibTypeInt64     = 3
	nibTypeTrue      = 4
	nibTypeFalse     = 5
	nibTypeFloat     = 6
	nibTypeDouble    = 7
	nibTypeData      = 8
	nibTypeNil       = 9
	nibTypeObjectRef = 10
)

type nibArchiveObject struct {
	classIndex int
	valueIndex int
	valueCount int
}

type nibArchiveValue struct {
	keyIndex int
	kind     byte
	value    any
}

// encodeNibArchive serializes a graph rooted at root using the UIKit
// NIBArchive container. Coder version 10 is what current Xcode emits; UIKit
// remains backwards compatible with the object/value vocabulary used here.
func encodeNibArchive(root *nibObject) ([]byte, error) {
	if root == nil {
		return nil, fmt.Errorf("nil nib root")
	}

	var graph []*nibObject
	indices := map[*nibObject]int{}
	var visit func(*nibObject)
	visit = func(o *nibObject) {
		if o == nil {
			return
		}
		if _, ok := indices[o]; ok {
			return
		}
		indices[o] = len(graph)
		graph = append(graph, o)
		for _, p := range o.props {
			if child, ok := p.value.(*nibObject); ok {
				visit(child)
			}
		}
	}
	visit(root)

	var keys []string
	keyIndex := map[string]int{}
	internKey := func(key string) int {
		if i, ok := keyIndex[key]; ok {
			return i
		}
		i := len(keys)
		keys = append(keys, key)
		keyIndex[key] = i
		return i
	}
	var classes []string
	classIndex := map[string]int{}
	internClass := func(class string) int {
		if i, ok := classIndex[class]; ok {
			return i
		}
		i := len(classes)
		classes = append(classes, class)
		classIndex[class] = i
		return i
	}

	objects := make([]nibArchiveObject, 0, len(graph))
	var values []nibArchiveValue
	for _, o := range graph {
		start := len(values)
		for _, p := range o.props {
			v := nibArchiveValue{keyIndex: internKey(p.key)}
			switch x := p.value.(type) {
			case *nibObject:
				v.kind, v.value = nibTypeObjectRef, indices[x]
			case nibInt8:
				v.kind, v.value = nibTypeInt8, int8(x)
			case nibInt16:
				v.kind, v.value = nibTypeInt16, int16(x)
			case nibInt32:
				v.kind, v.value = nibTypeInt32, int32(x)
			case nibInt64:
				v.kind, v.value = nibTypeInt64, int64(x)
			case bool:
				if x {
					v.kind = nibTypeTrue
				} else {
					v.kind = nibTypeFalse
				}
			case nibFloat:
				v.kind, v.value = nibTypeFloat, float32(x)
			case nibDouble:
				v.kind, v.value = nibTypeDouble, float64(x)
			case nibData:
				v.kind, v.value = nibTypeData, []byte(x)
			case nibNil:
				v.kind = nibTypeNil
			default:
				return nil, fmt.Errorf("unsupported nib value %T for %s.%s", p.value, o.class, p.key)
			}
			values = append(values, v)
		}
		objects = append(objects, nibArchiveObject{
			classIndex: internClass(o.class),
			valueIndex: start,
			valueCount: len(values) - start,
		})
	}

	var objectBytes, keyBytes, valueBytes, classBytes bytes.Buffer
	for _, o := range objects {
		writeNibFlex(&objectBytes, o.classIndex)
		writeNibFlex(&objectBytes, o.valueIndex)
		writeNibFlex(&objectBytes, o.valueCount)
	}
	for _, key := range keys {
		writeNibFlex(&keyBytes, len(key))
		keyBytes.WriteString(key)
	}
	for _, v := range values {
		writeNibFlex(&valueBytes, v.keyIndex)
		valueBytes.WriteByte(v.kind)
		switch v.kind {
		case nibTypeInt8:
			valueBytes.WriteByte(byte(v.value.(int8)))
		case nibTypeInt16:
			_ = binary.Write(&valueBytes, binary.LittleEndian, v.value.(int16))
		case nibTypeInt32:
			_ = binary.Write(&valueBytes, binary.LittleEndian, v.value.(int32))
		case nibTypeInt64:
			_ = binary.Write(&valueBytes, binary.LittleEndian, v.value.(int64))
		case nibTypeFloat:
			_ = binary.Write(&valueBytes, binary.LittleEndian, v.value.(float32))
		case nibTypeDouble:
			_ = binary.Write(&valueBytes, binary.LittleEndian, v.value.(float64))
		case nibTypeData:
			data := v.value.([]byte)
			writeNibFlex(&valueBytes, len(data))
			valueBytes.Write(data)
		case nibTypeObjectRef:
			_ = binary.Write(&valueBytes, binary.LittleEndian, uint32(v.value.(int)))
		case nibTypeFalse, nibTypeTrue, nibTypeNil:
			// no payload
		}
	}
	for _, class := range classes {
		writeNibFlex(&classBytes, len(class)+1)
		writeNibFlex(&classBytes, 0) // no fallback classes
		classBytes.WriteString(class)
		classBytes.WriteByte(0)
	}

	const headerSize = 50 // 10-byte magic + ten uint32 fields
	objectOffset := headerSize
	keyOffset := objectOffset + objectBytes.Len()
	valueOffset := keyOffset + keyBytes.Len()
	classOffset := valueOffset + valueBytes.Len()

	var out bytes.Buffer
	out.Grow(classOffset + classBytes.Len())
	out.WriteString("NIBArchive")
	for _, n := range []uint32{
		1, 10,
		uint32(len(objects)), uint32(objectOffset),
		uint32(len(keys)), uint32(keyOffset),
		uint32(len(values)), uint32(valueOffset),
		uint32(len(classes)), uint32(classOffset),
	} {
		_ = binary.Write(&out, binary.LittleEndian, n)
	}
	out.Write(objectBytes.Bytes())
	out.Write(keyBytes.Bytes())
	out.Write(valueBytes.Bytes())
	out.Write(classBytes.Bytes())
	return out.Bytes(), nil
}

// NIBArchive's variable integer is a 7-bit little-endian integer whose *last*
// byte, rather than continuation bytes, carries bit 0x80.
func writeNibFlex(buf *bytes.Buffer, value int) {
	if value < 0 {
		panic("negative NIBArchive flex integer")
	}
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value == 0 {
			buf.WriteByte(b | 0x80)
			return
		}
		buf.WriteByte(b)
	}
}
