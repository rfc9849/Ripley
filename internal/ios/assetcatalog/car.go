package assetcatalog

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"sort"
)

// CoreUI attribute identifiers. The values are the ones CoreUI itself reports
// through `assetutil --info`'s "Key Format" list, which names each token:
// 7 kCRThemeAppearanceName, 13 kCRThemeLocalizationName, 12 kCRThemeScaleName,
// 15 kCRThemeIdiomName, 16 kCRThemeSubtypeName, 9 kCRThemeDimension2Name,
// 8 kCRThemeDimension1Name, 24 kCRThemeDisplayGamutName,
// 17 kCRThemeIdentifierName, 1 kCRThemeElementName, 2 kCRThemePartName.
const (
	attrElement      = 1
	attrPart         = 2
	attrAppearance   = 7
	attrDimension1   = 8
	attrDimension2   = 9
	attrScale        = 12
	attrLocalization = 13
	attrIdiom        = 15
	attrSubtype      = 16
	attrIdentifier   = 17
	attrGamut        = 24
)

// keyFormat is the rendition key token order this writer emits.
//
// CoreUI matches renditions positionally against KEYFORMAT, so the order of
// the tokens here and in every encoded key must agree. The ranking was read
// back out of `actool` archives:
//
//	appearance, localization, scale, idiom, subtype,
//	dimension2, dimension1, gamut, identifier, element, part
//
// CoreUI accepts at most ten tokens: every `actool` archive inspected emitted
// exactly ten, choosing between dimension1 and gamut depending on whether the
// catalog carried multi-part images or a wide-gamut variant. Emitting an
// eleventh token is not a superset — `assetutil` then misparses every key,
// collapsing all renditions onto one name and idiom. So the list below is the
// ten-token format with dimension1 dropped, since this compiler never emits
// multi-part (sliced) images but does emit gamut-qualified assets.
var keyFormat = []uint32{
	attrAppearance,
	attrLocalization,
	attrScale,
	attrIdiom,
	attrSubtype,
	attrDimension2,
	attrGamut,
	attrIdentifier,
	attrElement,
	attrPart,
}

// CoreUI element/part values observed in Apple archives. `element` 85 marks a
// catalog asset; `part` selects the asset class.
const (
	elementAsset = 85

	partImage          = 181
	partColor          = 217
	partMultiSizeImage = 218
	partIconImage      = 220
)

// CoreUI rendition layouts.
const (
	layoutOnePartScale   = 12
	layoutInternalLink   = 1003
	layoutColor          = 1009
	layoutMultiSizeImage = 1010
	layoutRawData        = 1000
)

// CoreUI rendition info-section magics.
const (
	infoSlices      = 1001
	infoMetrics     = 1003
	infoComposition = 1004
	infoUTI         = 1005
	infoBitmapInfo  = 1006
	infoBytesPerRow = 1007
)

// CoreUI bitmap compression ids, as named by `assetutil --info`: 0
// uncompressed, 1 rle, 2 zip, 3 lzvn, 4 lzfse, 5 jpeg-lzfse, 6 blurred,
// 7 astc, 8 palette-img, 9 deepmap-lzfse, 10 deepmap2. Every id but 0 and 2
// is one of Apple's proprietary codecs; id 2 is a plain gzip stream, so it is
// the only compressed form this writer can emit.
const (
	compressionUncompressed = 0
	compressionZip          = 2
)

// csiBitmapHeaderLen is the size of the compressed-bitmap payload header:
// magic, version, compression id, payload length.
const csiBitmapHeaderLen = 16

// bitmapRowAlignment is the byte alignment CoreUI requires for a bitmap row.
// Apple's compiler rounds every rendition's bytes-per-row up to this, and the
// stored pixel buffer is padded to match: a 10px BGRA row is 40 bytes of pixels
// in a 64-byte row, and an 8px GA8 row is 16 bytes in a 32-byte row.
const bitmapRowAlignment = 32

// alignedBytesPerRow is the padded stride of a bitmap rendition.
func alignedBytesPerRow(width, bytesPerPixel int) int {
	row := width * bytesPerPixel
	if rem := row % bitmapRowAlignment; rem != 0 {
		row += bitmapRowAlignment - rem
	}
	return row
}

// pixel format tags, stored little-endian (so 'ARGB' reads as "BGRA").
var (
	pixelFormatARGB = [4]byte{'B', 'G', 'R', 'A'}
	pixelFormatGA8  = [4]byte{' ', '8', 'A', 'G'}
	pixelFormatJPEG = [4]byte{'G', 'E', 'P', 'J'}
	pixelFormatData = [4]byte{'A', 'T', 'A', 'D'}
	pixelFormatNone = [4]byte{0, 0, 0, 0}
)

// renditionKey is a rendition's coordinate in the CAR key space. Fields map
// one-to-one onto keyFormat tokens. `localization` and `dimension1` are always
// zero for the assets this compiler emits, but they occupy their slot in the
// encoded key because CoreUI reads keys positionally against KEYFORMAT.
type renditionKey struct {
	appearance   uint16
	localization uint16
	scale        uint16
	idiom        uint16
	subtype      uint16
	dimension2   uint16
	dimension1   uint16
	gamut        uint16
	identifier   uint16
	element      uint16
	part         uint16
}

func (k renditionKey) encode() []byte {
	out := make([]byte, 0, len(keyFormat)*2)
	for _, id := range keyFormat {
		out = binary.LittleEndian.AppendUint16(out, k.value(id))
	}
	return out
}

func (k renditionKey) value(id uint32) uint16 {
	switch id {
	case attrAppearance:
		return k.appearance
	case attrLocalization:
		return k.localization
	case attrScale:
		return k.scale
	case attrIdiom:
		return k.idiom
	case attrSubtype:
		return k.subtype
	case attrDimension2:
		return k.dimension2
	case attrDimension1:
		return k.dimension1
	case attrGamut:
		return k.gamut
	case attrIdentifier:
		return k.identifier
	case attrElement:
		return k.element
	case attrPart:
		return k.part
	default:
		return 0
	}
}

func decodeRenditionKey(b []byte, format []uint32) (renditionKey, error) {
	if len(b) < len(format)*2 {
		return renditionKey{}, fmt.Errorf("rendition key: %d bytes for %d tokens", len(b), len(format))
	}
	var k renditionKey
	for i, id := range format {
		v := binary.LittleEndian.Uint16(b[i*2:])
		switch id {
		case attrAppearance:
			k.appearance = v
		case attrLocalization:
			k.localization = v
		case attrScale:
			k.scale = v
		case attrIdiom:
			k.idiom = v
		case attrSubtype:
			k.subtype = v
		case attrDimension2:
			k.dimension2 = v
		case attrDimension1:
			k.dimension1 = v
		case attrGamut:
			k.gamut = v
		case attrIdentifier:
			k.identifier = v
		case attrElement:
			k.element = v
		case attrPart:
			k.part = v
		}
	}
	return k, nil
}

// renditionFlags is the packed flag word of a CSI header.
//
//	bit 0     header is FPO
//	bit 1     excluded from contrast filter
//	bit 2     is vector
//	bits 3-4  template rendering intent (0 original, 1 template, 2 automatic)
//	bit 5..   reserved
//
// The `is_opaque` bit documented by older reverse-engineering work is in fact
// part of this template field: Apple emits 0x08 for `template`, 0x10 for
// `automatic` and 0x00 for `original` regardless of image opacity, and CoreUI
// recomputes opacity from the pixels.
func renditionFlags(vector bool, intent TemplateIntent) uint32 {
	var f uint32
	if vector {
		f |= 1 << 2
	}
	switch intent {
	case TemplateTemplate:
		f |= 1 << 3
	case TemplateAutomatic:
		f |= 2 << 3
	}
	return f
}

func flagsTemplateIntent(f uint32) TemplateIntent {
	switch (f >> 3) & 0x3 {
	case 1:
		return TemplateTemplate
	case 2:
		return TemplateAutomatic
	default:
		return TemplateOriginal
	}
}

// csiInfo is one entry of a rendition's info section.
type csiInfo struct {
	magic   uint32
	payload []byte
}

func (i csiInfo) encode() []byte {
	out := make([]byte, 0, 8+len(i.payload))
	out = binary.LittleEndian.AppendUint32(out, i.magic)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(i.payload)))
	return append(out, i.payload...)
}

func u32s(vals ...uint32) []byte {
	out := make([]byte, 0, len(vals)*4)
	for _, v := range vals {
		out = binary.LittleEndian.AppendUint32(out, v)
	}
	return out
}

// csiHeaderLen is the fixed size of the CSI rendition header preceding the
// info section: magic, version, flags, width, height, scale, pixel format,
// colorspace, metadata (timestamp + layout + reserved + 128-byte name),
// info length and the bitmap descriptor.
const csiHeaderLen = 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + (4 + 2 + 2 + 128) + 4 + 12

// rendition is a fully described CAR rendition ready to be serialised.
type rendition struct {
	key    renditionKey
	name   string
	layout uint16
	flags  uint32

	width, height int
	scale         int // percent: 100, 200, 300
	pixelFormat   [4]byte
	colorSpace    uint32

	// info entries emitted verbatim after the header.
	info []csiInfo
	// body is the payload following the info section.
	body []byte
}

func (r rendition) encode() []byte {
	var info bytes.Buffer
	for _, i := range r.info {
		info.Write(i.encode())
	}

	out := make([]byte, 0, csiHeaderLen+info.Len()+len(r.body))
	out = append(out, 'I', 'S', 'T', 'C')
	out = binary.LittleEndian.AppendUint32(out, 1) // version
	out = binary.LittleEndian.AppendUint32(out, r.flags)
	out = binary.LittleEndian.AppendUint32(out, uint32(r.width))
	out = binary.LittleEndian.AppendUint32(out, uint32(r.height))
	out = binary.LittleEndian.AppendUint32(out, uint32(r.scale))
	out = append(out, r.pixelFormat[:]...)
	out = binary.LittleEndian.AppendUint32(out, r.colorSpace)

	// metadata: modification timestamp, layout, reserved, name[128].
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint16(out, r.layout)
	out = binary.LittleEndian.AppendUint16(out, 0)
	var name [128]byte
	copy(name[:], r.name)
	out = append(out, name[:]...)

	out = binary.LittleEndian.AppendUint32(out, uint32(info.Len()))

	// Bitmap descriptor: bitmap count, reserved, payload size. Every rendition
	// `actool` emits — including colors, data assets and multi-size icons that
	// carry no bitmap at all — stores a count of 1 here, and CoreUI rejects a
	// color rendition whose count is 0 with "INVALID COLOR #components 1".
	out = binary.LittleEndian.AppendUint32(out, 1)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(r.body)))

	out = append(out, info.Bytes()...)
	return append(out, r.body...)
}

func decodeRendition(key renditionKey, b []byte) (rendition, error) {
	if len(b) < csiHeaderLen {
		return rendition{}, fmt.Errorf("rendition: %d bytes, need %d", len(b), csiHeaderLen)
	}
	if string(b[:4]) != "ISTC" {
		return rendition{}, fmt.Errorf("rendition: magic %q", b[:4])
	}
	r := rendition{key: key}
	r.flags = binary.LittleEndian.Uint32(b[8:])
	r.width = int(binary.LittleEndian.Uint32(b[12:]))
	r.height = int(binary.LittleEndian.Uint32(b[16:]))
	r.scale = int(binary.LittleEndian.Uint32(b[20:]))
	copy(r.pixelFormat[:], b[24:28])
	r.colorSpace = binary.LittleEndian.Uint32(b[28:])
	r.layout = binary.LittleEndian.Uint16(b[36:])
	r.name = string(bytes.TrimRight(b[40:168], "\x00"))
	infoLen := int(binary.LittleEndian.Uint32(b[168:]))
	bodyLen := int(binary.LittleEndian.Uint32(b[180:]))

	if csiHeaderLen+infoLen > len(b) {
		return rendition{}, fmt.Errorf("rendition %q: info section overruns value", r.name)
	}
	infoBytes := b[csiHeaderLen : csiHeaderLen+infoLen]
	for len(infoBytes) >= 8 {
		magic := binary.LittleEndian.Uint32(infoBytes)
		n := int(binary.LittleEndian.Uint32(infoBytes[4:]))
		if 8+n > len(infoBytes) {
			return rendition{}, fmt.Errorf("rendition %q: info entry %d overruns section", r.name, magic)
		}
		r.info = append(r.info, csiInfo{magic: magic, payload: infoBytes[8 : 8+n]})
		infoBytes = infoBytes[8+n:]
	}

	body := b[csiHeaderLen+infoLen:]
	if bodyLen > len(body) {
		return rendition{}, fmt.Errorf("rendition %q: payload %d bytes, %d available", r.name, bodyLen, len(body))
	}
	r.body = body[:bodyLen]
	return r, nil
}

// uti returns the declared UTI of a raw-data rendition.
func (r rendition) uti() string {
	for _, i := range r.info {
		if i.magic != infoUTI || len(i.payload) < 8 {
			continue
		}
		n := int(binary.LittleEndian.Uint32(i.payload))
		if 8+n > len(i.payload) {
			continue
		}
		return string(bytes.TrimRight(i.payload[8:8+n], "\x00"))
	}
	return ""
}

// imageInfoSections builds the info section Apple emits for a bitmap
// rendition: slices, metrics, composition, bitmap info and bytes-per-row.
//
// The slice entry is (count, x, y, width, height); the metrics entry is
// (count, then two zero point-size pairs, then the pixel size). Both were read
// back out of `actool` archives.
func imageInfoSections(width, height, bytesPerRow int) []csiInfo {
	w, h := uint32(width), uint32(height)
	return []csiInfo{
		{magic: infoSlices, payload: u32s(1, 0, 0, w, h)},
		{magic: infoMetrics, payload: u32s(1, 0, 0, 0, 0, w, h)},
		compositionInfo(1),
		{magic: infoBitmapInfo, payload: u32s(1)},
		{magic: infoBytesPerRow, payload: u32s(uint32(bytesPerRow))},
	}
}

// compositionInfo is the blend-mode/opacity info entry.
func compositionInfo(opacity float32) csiInfo {
	p := make([]byte, 0, 8)
	p = binary.LittleEndian.AppendUint32(p, 0) // blend mode: normal
	p = binary.LittleEndian.AppendUint32(p, math.Float32bits(opacity))
	return csiInfo{magic: infoComposition, payload: p}
}

// utiInfo is the declared-UTI info entry of a raw-data rendition: the byte
// length of the NUL-terminated identifier, a reserved word, then the string.
func utiInfo(uti string) csiInfo {
	p := make([]byte, 0, 8+len(uti)+1)
	p = binary.LittleEndian.AppendUint32(p, uint32(len(uti)+1))
	p = binary.LittleEndian.AppendUint32(p, 0)
	p = append(p, uti...)
	p = append(p, 0)
	return csiInfo{magic: infoUTI, payload: p}
}

// encodeBitmapPayload wraps raw pixel bytes in the CoreUI compressed-bitmap
// container. The magic is stored little-endian, so the four bytes on disk read
// "MLEC"; the header is magic, version, compression id, payload length.
//
// Apple's own encoders (rle, lzvn, lzfse, deepmap2, ...) are proprietary and
// are not reimplemented. CoreUI's compression id 2 ("zip" as `assetutil`
// names it) is a gzip stream of the row-padded pixel buffer, which Go's
// standard library can produce; it was verified end to end by decoding the
// resulting archive through CUICatalog and comparing pixels.
func encodeBitmapPayload(pixels []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(pixels); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	compressed := buf.Bytes()
	// Clear the gzip OS byte so archives are reproducible across platforms.
	if len(compressed) > 9 {
		compressed[9] = 0
	}

	out := make([]byte, 0, csiBitmapHeaderLen+len(compressed))
	out = append(out, 'M', 'L', 'E', 'C')
	out = binary.LittleEndian.AppendUint32(out, 0) // version
	out = binary.LittleEndian.AppendUint32(out, compressionZip)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(compressed)))
	return append(out, compressed...), nil
}

// decodeBitmapPayload reverses encodeBitmapPayload. Only the encoding this
// package writes is understood; Apple's codecs are not reimplemented, so a
// rendition produced by `actool` reports its compression id instead.
func decodeBitmapPayload(body []byte) ([]byte, error) {
	if len(body) < csiBitmapHeaderLen || string(body[:4]) != "MLEC" {
		return nil, fmt.Errorf("bitmap payload: bad header")
	}
	if c := binary.LittleEndian.Uint32(body[8:]); c != compressionZip {
		return nil, fmt.Errorf("bitmap payload: compression id %d is not decoded by ripley", c)
	}
	n := int(binary.LittleEndian.Uint32(body[12:]))
	if csiBitmapHeaderLen+n > len(body) {
		return nil, fmt.Errorf("bitmap payload: %d bytes declared, %d available", n, len(body)-csiBitmapHeaderLen)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body[csiBitmapHeaderLen : csiBitmapHeaderLen+n]))
	if err != nil {
		return nil, fmt.Errorf("bitmap payload: %w", err)
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, fmt.Errorf("bitmap payload: %w", err)
	}
	return out.Bytes(), nil
}

// encodeRawPayload wraps opaque file bytes in the `DWAR` raw-data header used
// by data assets and JPEG renditions.
func encodeRawPayload(data []byte) []byte {
	out := make([]byte, 0, 12+len(data))
	out = append(out, 'D', 'W', 'A', 'R')
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
	return append(out, data...)
}

func decodeRawPayload(body []byte) ([]byte, error) {
	if len(body) < 12 || string(body[:4]) != "DWAR" {
		return nil, fmt.Errorf("raw payload: bad header")
	}
	n := int(binary.LittleEndian.Uint32(body[8:]))
	if 12+n > len(body) {
		return nil, fmt.Errorf("raw payload: %d bytes declared, %d available", n, len(body)-12)
	}
	return body[12 : 12+n], nil
}

// encodeColorPayload writes the color body: magic, version, colorspace,
// component count and float64 non-premultiplied components. Like every CoreUI
// payload magic this is stored little-endian, so the bytes on disk read "RLOC".
func encodeColorPayload(space uint32, comps [4]float64) []byte {
	out := make([]byte, 0, 16+len(comps)*8)
	out = append(out, 'R', 'L', 'O', 'C')
	out = binary.LittleEndian.AppendUint32(out, 1) // version
	out = binary.LittleEndian.AppendUint32(out, space)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(comps)))
	for _, c := range comps {
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(c))
	}
	return out
}

func decodeColorPayload(body []byte) (space uint32, comps []float64, err error) {
	if len(body) < 16 || string(body[:4]) != "RLOC" {
		return 0, nil, fmt.Errorf("color payload: bad header")
	}
	space = binary.LittleEndian.Uint32(body[8:])
	n := int(binary.LittleEndian.Uint32(body[12:]))
	if 16+n*8 > len(body) {
		return 0, nil, fmt.Errorf("color payload: %d components, %d bytes available", n, len(body)-16)
	}
	comps = make([]float64, n)
	for i := range comps {
		comps[i] = math.Float64frombits(binary.LittleEndian.Uint64(body[16+i*8:]))
	}
	return space, comps, nil
}

// multiSizeEntry is one size of a multi-size image rendition.
type multiSizeEntry struct {
	width, height uint32
	index         uint32
}

// encodeMultiSizePayload writes the multi-size image body listing every icon
// size an idiom provides; CoreUI uses it to enumerate app icon variants. The
// magic is stored little-endian, so the bytes on disk read "SISM".
func encodeMultiSizePayload(entries []multiSizeEntry) []byte {
	out := make([]byte, 0, 12+len(entries)*12)
	out = append(out, 'S', 'I', 'S', 'M')
	out = binary.LittleEndian.AppendUint32(out, 1) // version
	out = binary.LittleEndian.AppendUint32(out, uint32(len(entries)))
	for _, e := range entries {
		out = binary.LittleEndian.AppendUint32(out, e.width)
		out = binary.LittleEndian.AppendUint32(out, e.height)
		out = binary.LittleEndian.AppendUint32(out, e.index)
	}
	return out
}

func decodeMultiSizePayload(body []byte) ([]multiSizeEntry, error) {
	if len(body) < 12 || string(body[:4]) != "SISM" {
		return nil, fmt.Errorf("multisize payload: bad header")
	}
	n := int(binary.LittleEndian.Uint32(body[8:]))
	if 12+n*12 > len(body) {
		return nil, fmt.Errorf("multisize payload: %d entries, %d bytes available", n, len(body)-12)
	}
	out := make([]multiSizeEntry, n)
	for i := range out {
		off := 12 + i*12
		out[i] = multiSizeEntry{
			width:  binary.LittleEndian.Uint32(body[off:]),
			height: binary.LittleEndian.Uint32(body[off+4:]),
			index:  binary.LittleEndian.Uint32(body[off+8:]),
		}
	}
	return out, nil
}

// facet names an asset and links it to its renditions through an identifier.
type facet struct {
	name       string
	identifier uint16
	part       uint16
}

func (f facet) encode() []byte {
	attrs := [][2]uint16{
		{attrElement, elementAsset},
		{attrPart, f.part},
		{attrIdentifier, f.identifier},
	}
	out := make([]byte, 0, 6+len(attrs)*4)
	out = binary.LittleEndian.AppendUint16(out, 0) // hot spot x
	out = binary.LittleEndian.AppendUint16(out, 0) // hot spot y
	out = binary.LittleEndian.AppendUint16(out, uint16(len(attrs)))
	for _, a := range attrs {
		out = binary.LittleEndian.AppendUint16(out, a[0])
		out = binary.LittleEndian.AppendUint16(out, a[1])
	}
	return out
}

func decodeFacet(name string, b []byte) (facet, error) {
	if len(b) < 6 {
		return facet{}, fmt.Errorf("facet %q: %d bytes", name, len(b))
	}
	f := facet{name: name}
	n := int(binary.LittleEndian.Uint16(b[4:]))
	if 6+n*4 > len(b) {
		return facet{}, fmt.Errorf("facet %q: %d attributes, %d bytes available", name, n, len(b)-6)
	}
	for i := 0; i < n; i++ {
		id := binary.LittleEndian.Uint16(b[6+i*4:])
		v := binary.LittleEndian.Uint16(b[8+i*4:])
		switch id {
		case attrIdentifier:
			f.identifier = v
		case attrPart:
			f.part = v
		}
	}
	return f, nil
}

// facetIdentifier derives a facet's 16-bit identifier from its name. CoreUI
// only requires the value to be unique inside the archive and consistent
// between the facet and its renditions; a CRC of the name gives that
// deterministically, and collisions are resolved by the caller.
func facetIdentifier(name string) uint16 {
	return uint16(crc32.ChecksumIEEE([]byte(name)) & 0xffff)
}

// carArchive accumulates facets and renditions and serialises the BOM.
type carArchive struct {
	deploymentTarget string
	facets           []facet
	renditions       []rendition
	appearances      []Appearance
}

func (a *carArchive) addFacet(f facet) { a.facets = append(a.facets, f) }

func (a *carArchive) addRendition(r rendition) { a.renditions = append(a.renditions, r) }

// useAppearance records an appearance so it lands in APPEARANCEKEYS.
func (a *carArchive) useAppearance(ap Appearance) {
	for _, existing := range a.appearances {
		if existing == ap {
			return
		}
	}
	a.appearances = append(a.appearances, ap)
}

// carHeaderLen is the size of the CARHEADER structure as `actool` writes it:
// magic, ui version, storage version, storage timestamp, rendition count, a
// 128-byte main-version string, a 256-byte asset-storage-version string, a
// 16-byte uuid, then associated-checksum, schema version, colorspace id and
// key semantics. Measured at 436 bytes in an Xcode 26.6 archive.
const carHeaderLen = 4 + 4 + 4 + 4 + 4 + 128 + 256 + 16 + 4 + 4 + 4 + 4

// ripleyAssetCatalogVersion identifies this writer in the archive's version
// strings. It is informational only; nothing parses it.
const ripleyAssetCatalogVersion = "ripley-assetcatalog-1"

const (
	// carUIVersion and carStorageVersion match the archives produced by
	// CoreUI 975, which is what current iOS runtimes expect.
	carUIVersion      = 0x3cf
	carStorageVersion = 17
	carSchemaVersion  = 2
	carKeySemantics   = 2
	carColorSpaceID   = 1
)

func (a *carArchive) header() []byte {
	out := make([]byte, 0, carHeaderLen)
	out = append(out, 'R', 'A', 'T', 'C')
	out = binary.LittleEndian.AppendUint32(out, carUIVersion)
	out = binary.LittleEndian.AppendUint32(out, carStorageVersion)
	out = binary.LittleEndian.AppendUint32(out, 0) // storage timestamp
	out = binary.LittleEndian.AppendUint32(out, uint32(len(a.renditions)))

	var creator [128]byte
	copy(creator[:], "@(#)PROGRAM:CoreUI  PROJECT:CoreUI-975")
	out = append(out, creator[:]...)

	// The second string is the "AssetStorageVersion" assetutil reports. It is
	// free-form; CoreUI does not parse it.
	var other [256]byte
	copy(other[:], "ripley "+ripleyAssetCatalogVersion)
	out = append(out, other[:]...)

	out = append(out, make([]byte, 16)...) // uuid

	out = binary.LittleEndian.AppendUint32(out, 0) // associated checksum
	out = binary.LittleEndian.AppendUint32(out, carSchemaVersion)
	out = binary.LittleEndian.AppendUint32(out, carColorSpaceID)
	out = binary.LittleEndian.AppendUint32(out, carKeySemantics)
	return out
}

// The EXTENDED_METADATA block is `META` followed by a fixed 1024-byte contents
// field. CoreUI reads three NUL-terminated strings from fixed offsets. The
// offsets below are relative to the start of the block (i.e. they include the
// 4-byte magic), which is how they were measured in `actool` archives:
// platform version at 260, platform at 516, authoring tool at 772.
const (
	metaContentsLen           = 1024
	metaPlatformVersionOffset = 256
	metaPlatformOffset        = 512
	metaAuthoringToolOffset   = 768
)

// extendedMetadata is the fixed-size META block.
func (a *carArchive) extendedMetadata() []byte {
	out := make([]byte, 4+metaContentsLen)
	copy(out, "META")
	copy(out[metaPlatformVersionOffset:], a.deploymentTarget)
	copy(out[metaPlatformOffset:], "ios")
	copy(out[metaAuthoringToolOffset:], "@(#)PROGRAM:ripley  PROJECT:"+ripleyAssetCatalogVersion)
	return out
}

func (a *carArchive) keyFormatBlob() []byte {
	out := make([]byte, 0, 12+len(keyFormat)*4)
	out = append(out, 't', 'm', 'f', 'k')
	out = binary.LittleEndian.AppendUint32(out, 0) // reserved
	out = binary.LittleEndian.AppendUint32(out, uint32(len(keyFormat)))
	for _, id := range keyFormat {
		out = binary.LittleEndian.AppendUint32(out, id)
	}
	return out
}

// write serialises the archive into a BOM.
func (a *carArchive) write() ([]byte, error) {
	b := newBOMBuilder()

	b.addVariable(varCARHeader, b.addBlock(a.header()))
	b.addVariable(varKeyFormat, b.addBlock(a.keyFormatBlob()))

	facetEntries := make([]bomKV, 0, len(a.facets))
	for _, f := range a.facets {
		facetEntries = append(facetEntries, bomKV{key: []byte(f.name), value: f.encode()})
	}
	b.addTree(varFacetKeys, bomVariableKeys, facetEntries)

	renditionEntries := make([]bomKV, 0, len(a.renditions))
	for _, r := range a.renditions {
		renditionEntries = append(renditionEntries, bomKV{key: r.key.encode(), value: r.encode()})
	}
	b.addTree(varRenditions, len(keyFormat)*2, renditionEntries)

	appearances := make([]Appearance, len(a.appearances))
	copy(appearances, a.appearances)
	sort.Slice(appearances, func(i, j int) bool { return appearances[i] < appearances[j] })
	appearanceEntries := make([]bomKV, 0, len(appearances))
	for _, ap := range appearances {
		v := make([]byte, 2)
		binary.LittleEndian.PutUint16(v, uint16(ap))
		appearanceEntries = append(appearanceEntries, bomKV{key: []byte(ap.appearanceName()), value: v})
	}
	b.addTree(varAppearanceKeys, bomVariableKeys, appearanceEntries)

	b.addVariable(varExtendedMetadata, b.addBlock(a.extendedMetadata()))

	// BITMAPKEYS maps a facet identifier onto per-facet packed-bitmap metadata.
	// This writer emits no packed atlases, so the tree is present but empty;
	// CoreUI treats a facet with no entry as having no packed representation.
	b.addTree(varBitmapKeys, 0, nil)

	return b.build(), nil
}

// CAR BOM variable names.
const (
	varCARHeader        = "CARHEADER"
	varKeyFormat        = "KEYFORMAT"
	varFacetKeys        = "FACETKEYS"
	varRenditions       = "RENDITIONS"
	varAppearanceKeys   = "APPEARANCEKEYS"
	varExtendedMetadata = "EXTENDED_METADATA"
	varBitmapKeys       = "BITMAPKEYS"
)
