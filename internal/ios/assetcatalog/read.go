package assetcatalog

import (
	"encoding/binary"
	"fmt"
)

// Archive is a decoded `Assets.car`. It exists so that written archives can be
// verified against what CoreUI will see, and so that a real Apple-produced
// archive can be read back with the same code paths.
type Archive struct {
	// Platform and PlatformVersion come from the extended metadata block.
	Platform        string
	PlatformVersion string
	// KeyFormat is the rendition key token order the archive declares.
	KeyFormat []uint32
	// Appearances maps a CoreUI appearance name onto its stored index.
	Appearances map[string]uint16
	// Facets are the named assets, keyed by name.
	Facets map[string]ArchiveFacet
	// Renditions are every stored variant, in tree order.
	Renditions []ArchiveRendition
}

// ArchiveFacet is one named asset of an archive.
type ArchiveFacet struct {
	Name       string
	Identifier uint16
	// Part is the CoreUI asset class (partImage, partColor, ...).
	Part uint16
}

// ArchiveRendition is one decoded variant of a facet.
type ArchiveRendition struct {
	// Name is the rendition's stored file name.
	Name string
	// Identifier links the rendition to its facet.
	Identifier uint16
	Scale      Scale
	Idiom      Idiom
	Appearance Appearance
	Gamut      Gamut
	Subtype    int
	// SizeIndex is the icon size slot for app icon renditions, 0 otherwise.
	SizeIndex int
	// Part is the CoreUI asset class of this rendition.
	Part uint16
	// Layout is the CoreUI rendition layout.
	Layout uint16
	// Template is the declared template rendering intent.
	Template TemplateIntent
	Vector   bool

	// Width and Height are the pixel dimensions, 0 for non-bitmap renditions.
	Width, Height int
	// Encoding is the stored pixel format tag, e.g. "ARGB", "GA8 ", "DATA".
	Encoding string
	// ColorSpace is the stored colorspace id.
	ColorSpace uint32

	// Pixels is the decoded pixel buffer for bitmap renditions, in the byte
	// order Encoding describes.
	Pixels []byte
	// Data is the payload of a raw-data (`DWAR`) rendition: the data asset
	// bytes, or the original JPEG file.
	Data []byte
	// UTI is the declared UTI of a data rendition.
	UTI string
	// Color holds the R,G,B,A components of a color rendition.
	Color []float64
	// MultiSizes lists the sizes of a multi-size image rendition.
	MultiSizes []ArchiveMultiSize
}

// ArchiveMultiSize is one size entry of a multi-size image rendition.
type ArchiveMultiSize struct {
	Width, Height uint32
	Index         uint32
}

// ReadArchive decodes a compiled asset archive.
func ReadArchive(data []byte) (*Archive, error) {
	bom, err := openBOM(data)
	if err != nil {
		return nil, err
	}

	head, err := bom.variable(varCARHeader)
	if err != nil {
		return nil, err
	}
	if len(head) < carHeaderLen {
		return nil, fmt.Errorf("car: header is %d bytes, need %d", len(head), carHeaderLen)
	}
	if string(head[0:4]) != "RATC" {
		return nil, fmt.Errorf("car: header magic %q", head[0:4])
	}

	out := &Archive{
		Appearances: map[string]uint16{},
		Facets:      map[string]ArchiveFacet{},
	}

	kf, err := bom.variable(varKeyFormat)
	if err != nil {
		return nil, err
	}
	if len(kf) < 12 || string(kf[0:4]) != "tmfk" {
		return nil, fmt.Errorf("car: bad key format block")
	}
	n := int(binary.LittleEndian.Uint32(kf[8:12]))
	if 12+n*4 > len(kf) {
		return nil, fmt.Errorf("car: key format declares %d identifiers, block is %d bytes", n, len(kf))
	}
	for i := 0; i < n; i++ {
		out.KeyFormat = append(out.KeyFormat, binary.LittleEndian.Uint32(kf[12+i*4:]))
	}

	if meta, err := bom.variable(varExtendedMetadata); err == nil && len(meta) >= 1028 {
		out.Platform = cstring(meta[metaPlatformOffset:])
		out.PlatformVersion = cstring(meta[metaPlatformVersionOffset:])
	}

	appearances, err := bom.tree(varAppearanceKeys)
	if err != nil {
		return nil, err
	}
	for _, kv := range appearances {
		if len(kv.value) < 2 {
			return nil, fmt.Errorf("car: appearance %q has a %d-byte value", kv.key, len(kv.value))
		}
		out.Appearances[string(kv.key)] = binary.LittleEndian.Uint16(kv.value)
	}

	facets, err := bom.tree(varFacetKeys)
	if err != nil {
		return nil, err
	}
	for _, kv := range facets {
		f, err := decodeFacet(string(kv.key), kv.value)
		if err != nil {
			return nil, err
		}
		out.Facets[f.name] = ArchiveFacet{Name: f.name, Identifier: f.identifier, Part: f.part}
	}

	renditions, err := bom.tree(varRenditions)
	if err != nil {
		return nil, err
	}
	for _, kv := range renditions {
		key, err := decodeRenditionKey(kv.key, out.KeyFormat)
		if err != nil {
			return nil, err
		}
		r, err := decodeRendition(key, kv.value)
		if err != nil {
			return nil, err
		}
		ar, err := describeRendition(r)
		if err != nil {
			return nil, err
		}
		out.Renditions = append(out.Renditions, ar)
	}
	return out, nil
}

// describeRendition converts a raw rendition into its decoded payload form.
func describeRendition(r rendition) (ArchiveRendition, error) {
	ar := ArchiveRendition{
		Name:       r.name,
		Identifier: r.key.identifier,
		Scale:      Scale(r.key.scale),
		Idiom:      idiomFromCAR(r.key.idiom),
		Appearance: Appearance(r.key.appearance),
		Gamut:      Gamut(r.key.gamut),
		Subtype:    int(r.key.subtype),
		SizeIndex:  int(r.key.dimension2),
		Part:       r.key.part,
		Layout:     r.layout,
		Template:   flagsTemplateIntent(r.flags),
		Vector:     r.flags&0x4 != 0,
		Width:      r.width,
		Height:     r.height,
		Encoding:   pixelFormatName(r.pixelFormat),
		ColorSpace: r.colorSpace,
		UTI:        r.uti(),
	}

	switch {
	case r.layout == layoutColor:
		_, comps, err := decodeColorPayload(r.body)
		if err != nil {
			return ArchiveRendition{}, fmt.Errorf("rendition %q: %w", r.name, err)
		}
		ar.Color = comps
	case r.layout == layoutMultiSizeImage:
		entries, err := decodeMultiSizePayload(r.body)
		if err != nil {
			return ArchiveRendition{}, fmt.Errorf("rendition %q: %w", r.name, err)
		}
		for _, e := range entries {
			ar.MultiSizes = append(ar.MultiSizes, ArchiveMultiSize{Width: e.width, Height: e.height, Index: e.index})
		}
	case r.pixelFormat == pixelFormatData || r.pixelFormat == pixelFormatJPEG:
		payload, err := decodeRawPayload(r.body)
		if err != nil {
			return ArchiveRendition{}, fmt.Errorf("rendition %q: %w", r.name, err)
		}
		ar.Data = payload
	case len(r.body) > 0:
		pixels, err := decodeBitmapPayload(r.body)
		if err != nil {
			return ArchiveRendition{}, fmt.Errorf("rendition %q: %w", r.name, err)
		}
		ar.Pixels = pixels
	}
	return ar, nil
}

// pixelFormatName renders a stored little-endian format tag as the four-char
// code CoreUI documents ("ARGB", "GA8 ", "DATA", "JPEG").
func pixelFormatName(pf [4]byte) string {
	if pf == pixelFormatNone {
		return ""
	}
	return string([]byte{pf[3], pf[2], pf[1], pf[0]})
}

func idiomFromCAR(v uint16) Idiom {
	switch v {
	case 1:
		return IdiomIPhone
	case 2:
		return IdiomIPad
	case 6:
		return IdiomMarketing
	default:
		return IdiomUniversal
	}
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
