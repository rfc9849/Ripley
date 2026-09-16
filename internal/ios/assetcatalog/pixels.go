package assetcatalog

import (
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // registered so DecodeConfig recognises JPEG sources
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

// CoreUI colorspace identifiers stored in the CSI header's colorspace field.
const (
	// colorSpaceSRGB is sRGB, used for every ARGB and JPEG rendition.
	colorSpaceSRGB = 1
	// colorSpaceGray is "gray gamma 22", used for GA8 renditions.
	colorSpaceGray = 2
	// colorSpaceP3 is Display P3, used for wide-gamut color renditions.
	colorSpaceP3 = 3
)

// imageDimensions reads the pixel dimensions of path from the file's own bytes
// and reports whether it is grayscale. Only the header is decoded, so this is
// cheap even for 1024x1024 marketing icons.
func imageDimensions(path string) (width, height int, gray bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false, fmt.Errorf("asset catalog: open %s: %w", path, err)
	}
	defer f.Close()

	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false, fmt.Errorf("asset catalog: decode %s: %w", path, err)
	}
	switch cfg.ColorModel {
	case color.GrayModel, color.Gray16Model:
		gray = true
	}
	// JPEG is stored as the original file, so its color model does not select a
	// CAR pixel format.
	if format == "jpeg" {
		gray = false
	}
	return cfg.Width, cfg.Height, gray, nil
}

// encodingForFile picks the CAR payload encoding for a source file.
func encodingForFile(path string) (enc PixelEncoding, vector bool, err error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf", ".svg", ".svgz":
		return EncodingARGB, true, nil
	case ".jpg", ".jpeg":
		return EncodingJPEG, false, nil
	case ".png":
		_, _, gray, err := imageDimensions(path)
		if err != nil {
			return EncodingARGB, false, err
		}
		if gray {
			return EncodingGA8, false, nil
		}
		return EncodingARGB, false, nil
	default:
		return EncodingARGB, false, fmt.Errorf("asset catalog: %s: unsupported image type %q",
			path, filepath.Ext(path))
	}
}

// bytesPerRow returns the row stride CoreUI expects for a bitmap rendition.
//
// CoreUI does not use the natural width*bpp stride: every `actool` archive
// rounds the row up to a 32-byte boundary (a 10px ARGB row is 64 bytes, not 40;
// a 20px ARGB row is 96, not 80; an 8px GA8 row is 32, not 16). The stride is
// stored in the rendition's bytes-per-row info entry and the pixel buffer must
// be padded to match, or CoreUI reads each row at the wrong offset and the
// image shears.
func bytesPerRow(width int, enc PixelEncoding) int {
	var bpp int
	switch enc {
	case EncodingGA8:
		bpp = 2
	default:
		bpp = 4
	}
	return (width*bpp + carRowAlignment - 1) / carRowAlignment * carRowAlignment
}

// carRowAlignment is the row-stride alignment CoreUI bitmaps use.
const carRowAlignment = 32

// decodePixels decodes path into the raw pixel buffer CoreUI expects for enc:
//
//   - EncodingARGB: 4 bytes per pixel, B,G,R,A order, alpha premultiplied.
//   - EncodingGA8:  2 bytes per pixel, gray then alpha, alpha premultiplied.
//
// Rows run top to bottom, matching the CSI slice/metrics geometry written
// alongside the payload, and each row is padded to bytesPerRow.
func decodePixels(path string, enc PixelEncoding) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("asset catalog: open %s: %w", path, err)
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("asset catalog: decode %s: %w", path, err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	stride := bytesPerRow(w, enc)

	switch enc {
	case EncodingGA8:
		out := make([]byte, stride*h)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			i := (y - b.Min.Y) * stride
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, a := premultiplied8(img.At(x, y))
				out[i] = grayFrom8(r, g, bl)
				out[i+1] = a
				i += 2
			}
		}
		return out, nil
	case EncodingARGB:
		out := make([]byte, stride*h)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			i := (y - b.Min.Y) * stride
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, a := premultiplied8(img.At(x, y))
				out[i] = bl
				out[i+1] = g
				out[i+2] = r
				out[i+3] = a
				i += 4
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("asset catalog: encoding %d has no pixel buffer", enc)
	}
}

// premultiplied8 returns c as 8-bit alpha-premultiplied components.
//
// Go's color.Color.RGBA() premultiplies in 16-bit space and truncating that
// back to 8 bits loses a step: a 5/255 blue at alpha 200/255 truncates to 3
// where Apple's compiler stores 4. Premultiplying the 8-bit components with
// round-half-up reproduces `actool`'s values.
func premultiplied8(c color.Color) (r, g, b, a uint8) {
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	return mul8(n.R, n.A), mul8(n.G, n.A), mul8(n.B, n.A), n.A
}

// mul8 multiplies two 8-bit channels, rounding half up.
func mul8(v, a uint8) uint8 {
	return uint8((uint32(v)*uint32(a) + 127) / 255)
}

// grayFrom8 converts 8-bit premultiplied RGB to an 8-bit luminance value using
// the Rec. 601 weights Go's color.GrayModel uses.
func grayFrom8(r, g, b uint8) uint8 {
	y := (19595*uint32(r) + 38470*uint32(g) + 7471*uint32(b) + 1<<15) >> 16
	if y > 0xff {
		y = 0xff
	}
	return uint8(y)
}
