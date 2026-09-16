// Package assetcatalog lowers Xcode `.xcassets` asset catalogs on Linux, where
// Apple's `actool` does not exist.
//
// Two representations are produced from the same parsed catalog:
//
//   - loose bundle resources plus `Info.plist` keys, which is what UIKit falls
//     back to for `UIImage(named:)` and `CFBundleIcons`;
//   - a real compiled `Assets.car`, which is the only representation UIKit
//     accepts for named colors (`UIColor(named:)`) and data assets
//     (`NSDataAsset`).
//
// The `Assets.car` writer implements the BOM container plus the CoreUI asset
// storage variables (`CARHEADER`, `KEYFORMAT`, `FACETKEYS`, `RENDITIONS`,
// `APPEARANCEKEYS`, `EXTENDED_METADATA`, `BITMAPKEYS`). Byte layouts were
// derived from Apple-produced archives (CoreUI 975 / storage version 17) and
// cross-checked against the public xcbuild `libcar`/`libbom` descriptions.
// Image payloads are emitted as gzip-deflated pixel data (CoreUI compression
// id 2) rather than Apple's LZFSE/deepmap2 encoders; colors are `COLR`
// renditions and data assets are `DWAR` raw renditions.
//
// This package MUST NOT import ripley/internal/ios (that package imports this
// one).
package assetcatalog

// Scale is an asset rendition scale factor: 1, 2 or 3.
type Scale int

// Idiom is an Xcode asset idiom ("universal", "iphone", "ipad", ...).
type Idiom string

const (
	IdiomUniversal Idiom = "universal"
	IdiomIPhone    Idiom = "iphone"
	IdiomIPad      Idiom = "ipad"
	IdiomMarketing Idiom = "ios-marketing"
)

// TemplateIntent is the `template-rendering-intent` of an image rendition.
type TemplateIntent int

const (
	// TemplateAutomatic lets UIKit decide; it is Xcode's default and the
	// value Apple's compiler writes when the key is absent.
	TemplateAutomatic TemplateIntent = iota
	// TemplateOriginal forces the image to be drawn as-is.
	TemplateOriginal
	// TemplateTemplate forces template (tint-mask) rendering.
	TemplateTemplate
)

// Appearance is a CoreUI appearance selector. The zero value applies to every
// appearance. The values are the indices CoreUI stores in APPEARANCEKEYS, read
// back out of `actool` archives.
type Appearance int

const (
	AppearanceAny              Appearance = 0
	AppearanceDark             Appearance = 1
	AppearanceHighContrast     Appearance = 2
	AppearanceHighContrastDark Appearance = 3
	// AppearanceTintable is the iOS 18 tinted icon variant
	// (`luminosity`/`tinted`). CoreUI numbers it 10, well away from the
	// luminosity/contrast block.
	AppearanceTintable Appearance = 10
)

// appearanceName is the CoreUI key stored in the APPEARANCEKEYS tree.
func (a Appearance) appearanceName() string {
	switch a {
	case AppearanceDark:
		return "UIAppearanceDark"
	case AppearanceHighContrast:
		return "UIAppearanceHighContrastAny"
	case AppearanceHighContrastDark:
		return "UIAppearanceHighContrastDark"
	case AppearanceTintable:
		return "ISAppearanceTintable"
	default:
		return "UIAppearanceAny"
	}
}

// Gamut is the `display-gamut` selector of a rendition.
type Gamut int

const (
	GamutSRGB Gamut = iota
	GamutP3
)

// PixelEncoding is the storage encoding of an image rendition payload.
type PixelEncoding int

const (
	// EncodingARGB is 8-bit BGRA byte order, alpha premultiplied.
	EncodingARGB PixelEncoding = iota
	// EncodingGA8 is 8-bit gray + alpha, alpha premultiplied.
	EncodingGA8
	// EncodingJPEG stores the original JPEG file bytes.
	EncodingJPEG
)

// Image is one rendition of an image set or app icon set.
type Image struct {
	// Name is the asset name (the `.imageset`/`.appiconset` directory name
	// without its extension, prefixed by any namespacing parent folders).
	Name string
	// Scale is the rendition scale; 0 means the file is scale-independent
	// (vector or single-size).
	Scale Scale
	Idiom Idiom
	// Size is the app-icon point size ("60x60") when this rendition belongs to
	// an app icon set, empty otherwise.
	Size string
	// SizePoints is Size parsed as points; 0 when Size is empty.
	SizePoints float64
	// Subtype is the app-icon device subtype (1792 for the 87mm iPhone icon),
	// 0 when unspecified.
	Subtype int
	// Path is the absolute path of the source file inside the catalog.
	Path string
	// PixelWidth and PixelHeight are the decoded dimensions of Path.
	PixelWidth  int
	PixelHeight int
	// Encoding is the CAR payload encoding chosen for Path.
	Encoding PixelEncoding
	// Vector is true for PDF/SVG sources, which are copied as loose files but
	// are not compiled into the archive (no rasterizer is available offline).
	Vector bool
	// Template is the declared template rendering intent.
	Template TemplateIntent
	Appearance
	Gamut
}

// Color is a named color from a `.colorset`.
type Color struct {
	Name  string
	Idiom Idiom
	// R, G, B and A are in the 0..1 range, non-premultiplied.
	R, G, B, A float64
	Appearance
	Gamut
}

// Data is a named data asset from a `.dataset`.
type Data struct {
	Name  string
	Idiom Idiom
	// Path is the absolute path of the payload file.
	Path string
	// UTI is the declared universal type identifier, empty when unspecified.
	UTI string
	Appearance
}

// Catalog is the parsed, platform-filtered content of one or more `.xcassets`
// directories.
type Catalog struct {
	// Images excludes app icon renditions; those live in AppIcons.
	Images []Image
	// AppIcons holds the renditions of the selected app icon set.
	AppIcons []Image
	// AppIconName is the name of the selected app icon set, empty when none.
	AppIconName string
	// PreRenderedIcon reports the app icon set's `pre-rendered` property.
	PreRenderedIcon bool
	Colors          []Color
	Data            []Data
	// DeploymentTarget is the iOS version the catalog is compiled for.
	DeploymentTarget string
	// Skipped lists items that were recognised but not compiled.
	Skipped []SkippedItem
}

// SkippedItem records one catalog entry that was deliberately not compiled.
type SkippedItem struct {
	// Catalog is the `.xcassets` directory the item came from.
	Catalog string `json:"catalog"`
	// Item is the path of the skipped asset relative to Catalog.
	Item string `json:"item"`
	// Kind is the asset kind ("imageset", "appiconset", "mipmapset", ...).
	Kind string `json:"kind"`
	// Reason explains why the item was skipped.
	Reason string `json:"reason"`
}

// Options configures Compile.
type Options struct {
	// DeploymentTarget is the minimum iOS version, e.g. "13.0". It is recorded
	// in the archive's extended metadata.
	DeploymentTarget string
	// AppIconName selects the `.appiconset` to treat as the application icon
	// (`ASSETCATALOG_COMPILER_APPICON_NAME`). Empty means "AppIcon" if such a
	// set exists, otherwise the only app icon set present.
	AppIconName string
	// Idioms restricts the accepted idioms. Empty means the iOS set
	// (universal, iphone, ipad, ios-marketing).
	Idioms []string
}

// Result is the output of Compile.
type Result struct {
	// CarPath is the absolute path of the written `Assets.car`, empty when the
	// catalogs held nothing compilable.
	CarPath string
	// LooseFiles are absolute paths of the individual resources written under
	// outDir; they must be copied into the bundle root.
	LooseFiles []string
	// InfoPlistKeys must be merged into the application `Info.plist`.
	InfoPlistKeys map[string]any
	// Skipped lists catalog entries that were recognised but not compiled.
	Skipped []SkippedItem
}

// iOSIdioms is the default accepted idiom set.
var iOSIdioms = map[Idiom]bool{
	IdiomUniversal: true,
	IdiomIPhone:    true,
	IdiomIPad:      true,
	IdiomMarketing: true,
}

// carIdiomValue maps an idiom onto its CoreUI attribute value.
func carIdiomValue(i Idiom) uint16 {
	switch i {
	case IdiomIPhone:
		return 1
	case IdiomIPad:
		return 2
	case IdiomMarketing:
		return 6
	default:
		return 0
	}
}

// idiomSuffix is the loose-file name suffix for an idiom.
func idiomSuffix(i Idiom) string {
	if i == IdiomIPad {
		return "~ipad"
	}
	return ""
}
