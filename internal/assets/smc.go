package assets

import (
	"encoding/binary"
	"math"
)

const (
	smcDouble = 6
	smcString = 7
	smcList   = 12
	smcMap    = 13
)

type manifestVariant struct {
	asset  string
	DPR    float64
	HasDPR bool
}

func encodeAssetManifest(manifest map[string][]manifestVariant, keys []string) []byte {
	buf := make([]byte, 0, 1024)
	buf = append(buf, smcMap)
	buf = appendSMCSize(buf, len(keys))
	for _, key := range keys {
		buf = appendSMCString(buf, key)
		variants := manifest[key]
		buf = append(buf, smcList)
		buf = appendSMCSize(buf, len(variants))
		for _, variant := range variants {
			count := 1
			if variant.HasDPR {
				count = 2
			}
			buf = append(buf, smcMap)
			buf = appendSMCSize(buf, count)
			buf = appendSMCString(buf, "asset")
			buf = appendSMCString(buf, variant.asset)
			if variant.HasDPR {
				buf = appendSMCString(buf, "dpr")
				buf = append(buf, smcDouble)
				buf = alignSMC(buf, 8)
				var raw [8]byte
				binary.LittleEndian.PutUint64(raw[:], math.Float64bits(variant.DPR))
				buf = append(buf, raw[:]...)
			}
		}
	}
	return buf
}

func encodeEmptySMCMap() []byte {
	return []byte{smcMap, 0}
}

func appendSMCString(buf []byte, value string) []byte {
	buf = append(buf, smcString)
	buf = appendSMCSize(buf, len(value))
	return append(buf, value...)
}

func appendSMCSize(buf []byte, n int) []byte {
	switch {
	case n < 0xfe:
		return append(buf, byte(n))
	case n <= 0xffff:
		buf = append(buf, 0xfe)
		var raw [2]byte
		binary.LittleEndian.PutUint16(raw[:], uint16(n))
		return append(buf, raw[:]...)
	default:
		buf = append(buf, 0xff)
		var raw [4]byte
		binary.LittleEndian.PutUint32(raw[:], uint32(n))
		return append(buf, raw[:]...)
	}
}

func alignSMC(buf []byte, alignment int) []byte {
	if rem := len(buf) % alignment; rem != 0 {
		buf = append(buf, make([]byte, alignment-rem)...)
	}
	return buf
}
