package storyboard

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Color is an sRGB color extracted from an Interface Builder document.
//
// R, G, B and A are non-premultiplied sRGB components in the 0..1 range, which
// is the same convention as assetcatalog.Color, so a RequiredColor can be
// turned into a `.colorset` without conversion.
type Color struct {
	R, G, B, A float64
	// CatalogName is set when the storyboard referenced a named color from an
	// asset catalog (<color name="LaunchBackground"/>) rather than literal
	// components. The components are then only known if the document carried a
	// <namedColor> definition in its <resources> section.
	CatalogName string
	// Dynamic is true for a system color that resolves differently in light and
	// dark appearance. Components hold the light-appearance value; DarkR..DarkA
	// hold the dark one.
	Dynamic                    bool
	DarkR, DarkG, DarkB, DarkA float64
	// SystemName is the UIColor selector name for a system color
	// ("systemBackgroundColor"), empty otherwise.
	SystemName string
}

// systemColor is one entry of Apple's Interface Builder system color table.
type systemColor struct {
	light [4]float64
	dark  [4]float64
	// dynamic is false for the handful of fixed colors (whiteColor,
	// blackColor, ...) that do not vary by appearance.
	dynamic bool
}

// systemColors holds the resolved sRGB values of the UIColor system colors that
// Interface Builder can attach to a view.
//
// The values are Apple's own: they were read out of Xcode's Interface Builder
// colour table (IDEInterfaceBuilderCocoaTouchIntegration.framework's
// iPhoneSDK.colorplist, an NSKeyedArchiver archive of the per-appearance
// concrete colours) rather than eyeballed from screenshots. Colours whose
// archived form is a white/alpha pair are expanded to equal RGB components,
// which is what a calibrated- or device-white colour means in sRGB.
//
// This table is data, not a tool dependency: it lets `ripley` resolve a system
// colour to concrete components on Linux, where no UIKit exists to ask.
var systemColors = map[string]systemColor{
	// Fixed (appearance-independent) colors.
	"whiteColor":       {light: [4]float64{1, 1, 1, 1}, dark: [4]float64{1, 1, 1, 1}},
	"systemWhiteColor": {light: [4]float64{1, 1, 1, 1}, dark: [4]float64{1, 1, 1, 1}},
	"blackColor":       {light: [4]float64{0, 0, 0, 1}, dark: [4]float64{0, 0, 0, 1}},
	"clearColor":       {light: [4]float64{0, 0, 0, 0}, dark: [4]float64{0, 0, 0, 0}},
	"darkTextColor":    {light: [4]float64{0, 0, 0, 1}, dark: [4]float64{0, 0, 0, 1}},
	"lightTextColor":   {light: [4]float64{1, 1, 1, 0.6}, dark: [4]float64{1, 1, 1, 0.6}},
	"darkGrayColor":    {light: [4]float64{1.0 / 3, 1.0 / 3, 1.0 / 3, 1}, dark: [4]float64{1.0 / 3, 1.0 / 3, 1.0 / 3, 1}},
	"lightGrayColor":   {light: [4]float64{2.0 / 3, 2.0 / 3, 2.0 / 3, 1}, dark: [4]float64{2.0 / 3, 2.0 / 3, 2.0 / 3, 1}},
	"grayColor":        {light: [4]float64{0.5, 0.5, 0.5, 1}, dark: [4]float64{0.5, 0.5, 0.5, 1}},

	// Dynamic backgrounds.
	"systemBackgroundColor":                 {light: [4]float64{1, 1, 1, 1}, dark: [4]float64{0, 0, 0, 1}, dynamic: true},
	"secondarySystemBackgroundColor":        {light: [4]float64{0.9490196078, 0.9490196078, 0.968627451, 1}, dark: [4]float64{0.1098039216, 0.1098039216, 0.1176470588, 1}, dynamic: true},
	"tertiarySystemBackgroundColor":         {light: [4]float64{1, 1, 1, 1}, dark: [4]float64{0.1725490196, 0.1725490196, 0.1803921569, 1}, dynamic: true},
	"systemGroupedBackgroundColor":          {light: [4]float64{0.9490196078, 0.9490196078, 0.968627451, 1}, dark: [4]float64{0, 0, 0, 1}, dynamic: true},
	"secondarySystemGroupedBackgroundColor": {light: [4]float64{1, 1, 1, 1}, dark: [4]float64{0.1098039216, 0.1098039216, 0.1176470588, 1}, dynamic: true},
	"tertiarySystemGroupedBackgroundColor":  {light: [4]float64{0.9490196078, 0.9490196078, 0.968627451, 1}, dark: [4]float64{0.1725490196, 0.1725490196, 0.1803921569, 1}, dynamic: true},
	"groupTableViewBackgroundColor":         {light: [4]float64{0.9490196078, 0.9490196078, 0.968627451, 1}, dark: [4]float64{0, 0, 0, 1}, dynamic: true},
	"tableBackgroundColor":                  {light: [4]float64{1, 1, 1, 1}, dark: [4]float64{0, 0, 0, 1}, dynamic: true},

	// Dynamic content colors.
	"labelColor":           {light: [4]float64{0, 0, 0, 1}, dark: [4]float64{1, 1, 1, 1}, dynamic: true},
	"secondaryLabelColor":  {light: [4]float64{0.2352941176, 0.2352941176, 0.262745098, 0.6}, dark: [4]float64{0.9215686275, 0.9215686275, 0.9607843137, 0.6}, dynamic: true},
	"tertiaryLabelColor":   {light: [4]float64{0.2352941176, 0.2352941176, 0.262745098, 0.2980392157}, dark: [4]float64{0.9215686275, 0.9215686275, 0.9607843137, 0.2980392157}, dynamic: true},
	"quaternaryLabelColor": {light: [4]float64{0.2352941176, 0.2352941176, 0.262745098, 0.1764705882}, dark: [4]float64{0.9215686275, 0.9215686275, 0.9607843137, 0.1568627451}, dynamic: true},
	"placeholderTextColor": {light: [4]float64{0.2352941176, 0.2352941176, 0.262745098, 0.2980392157}, dark: [4]float64{0.9215686275, 0.9215686275, 0.9607843137, 0.2980392157}, dynamic: true},
	"separatorColor":       {light: [4]float64{0.2352941176, 0.2352941176, 0.262745098, 0.12}, dark: [4]float64{0.3294117647, 0.3294117647, 0.3450980392, 0.5}, dynamic: true},
	"opaqueSeparatorColor": {light: [4]float64{0.7764705882, 0.7764705882, 0.7843137255, 1}, dark: [4]float64{0.2196078431, 0.2196078431, 0.2274509804, 1}, dynamic: true},
	"linkColor":            {light: [4]float64{0, 0.4784313725, 1, 1}, dark: [4]float64{0.03529411765, 0.5176470588, 1, 1}, dynamic: true},

	// Dynamic fills.
	"systemFillColor":           {light: [4]float64{0.4705882353, 0.4705882353, 0.5019607843, 0.2}, dark: [4]float64{0.4705882353, 0.4705882353, 0.5019607843, 0.36}, dynamic: true},
	"secondarySystemFillColor":  {light: [4]float64{0.4705882353, 0.4705882353, 0.5019607843, 0.16}, dark: [4]float64{0.4705882353, 0.4705882353, 0.5019607843, 0.32}, dynamic: true},
	"tertiarySystemFillColor":   {light: [4]float64{0.462745098, 0.462745098, 0.5019607843, 0.12}, dark: [4]float64{0.462745098, 0.462745098, 0.5019607843, 0.24}, dynamic: true},
	"quaternarySystemFillColor": {light: [4]float64{0.4549019608, 0.4549019608, 0.5019607843, 0.08}, dark: [4]float64{0.462745098, 0.462745098, 0.5019607843, 0.18}, dynamic: true},

	// Dynamic tints.
	"tintColor":         {light: [4]float64{0, 0.5333333333, 1, 1}, dark: [4]float64{0, 0.568627451, 1, 1}, dynamic: true},
	"defaultTintColor":  {light: [4]float64{0, 0.5333333333, 1, 1}, dark: [4]float64{0, 0.568627451, 1, 1}, dynamic: true},
	"systemBlueColor":   {light: [4]float64{0, 0.5333333333, 1, 1}, dark: [4]float64{0, 0.568627451, 1, 1}, dynamic: true},
	"systemBrownColor":  {light: [4]float64{0.6745098039, 0.4980392157, 0.368627451, 1}, dark: [4]float64{0.7176470588, 0.5411764706, 0.4, 1}, dynamic: true},
	"systemCyanColor":   {light: [4]float64{0, 0.7529411765, 0.9098039216, 1}, dark: [4]float64{0.2352941176, 0.8274509804, 0.9960784314, 1}, dynamic: true},
	"systemGreenColor":  {light: [4]float64{0.2039215686, 0.7803921569, 0.3490196078, 1}, dark: [4]float64{0.1882352941, 0.8196078431, 0.3450980392, 1}, dynamic: true},
	"systemIndigoColor": {light: [4]float64{0.3803921569, 0.3333333333, 0.9607843137, 1}, dark: [4]float64{0.4274509804, 0.4862745098, 1, 1}, dynamic: true},
	"systemMintColor":   {light: [4]float64{0, 0.7843137255, 0.7019607843, 1}, dark: [4]float64{0, 0.8549019608, 0.7647058824, 1}, dynamic: true},
	"systemOrangeColor": {light: [4]float64{1, 0.5529411765, 0.1568627451, 1}, dark: [4]float64{1, 0.5725490196, 0.1882352941, 1}, dynamic: true},
	"systemPinkColor":   {light: [4]float64{1, 0.1764705882, 0.3333333333, 1}, dark: [4]float64{1, 0.2156862745, 0.3725490196, 1}, dynamic: true},
	"systemPurpleColor": {light: [4]float64{0.7960784314, 0.1882352941, 0.8784313725, 1}, dark: [4]float64{0.8588235294, 0.2039215686, 0.9490196078, 1}, dynamic: true},
	"systemRedColor":    {light: [4]float64{1, 0.2196078431, 0.2352941176, 1}, dark: [4]float64{1, 0.2588235294, 0.2705882353, 1}, dynamic: true},
	"systemTealColor":   {light: [4]float64{0, 0.7647058824, 0.8156862745, 1}, dark: [4]float64{0, 0.8235294118, 0.8784313725, 1}, dynamic: true},
	"systemYellowColor": {light: [4]float64{1, 0.8, 0, 1}, dark: [4]float64{1, 0.8392156863, 0, 1}, dynamic: true},

	// Dynamic grays.
	"systemGrayColor":  {light: [4]float64{0.5568627451, 0.5568627451, 0.5764705882, 1}, dark: [4]float64{0.5568627451, 0.5568627451, 0.5764705882, 1}, dynamic: true},
	"systemGray2Color": {light: [4]float64{0.6823529412, 0.6823529412, 0.6980392157, 1}, dark: [4]float64{0.3882352941, 0.3882352941, 0.4, 1}, dynamic: true},
	"systemGray3Color": {light: [4]float64{0.7803921569, 0.7803921569, 0.8, 1}, dark: [4]float64{0.2823529412, 0.2823529412, 0.2901960784, 1}, dynamic: true},
	"systemGray4Color": {light: [4]float64{0.8196078431, 0.8196078431, 0.8392156863, 1}, dark: [4]float64{0.2274509804, 0.2274509804, 0.2352941176, 1}, dynamic: true},
	"systemGray5Color": {light: [4]float64{0.8980392157, 0.8980392157, 0.9176470588, 1}, dark: [4]float64{0.1725490196, 0.1725490196, 0.1803921569, 1}, dynamic: true},
	"systemGray6Color": {light: [4]float64{0.9490196078, 0.9490196078, 0.968627451, 1}, dark: [4]float64{0.1098039216, 0.1098039216, 0.1176470588, 1}, dynamic: true},
}

// srgbFromLinearGray converts a calibrated/generic white component to an sRGB
// component.
//
// Interface Builder writes `calibratedWhite` and `deviceWhite` colours as a
// single gamma-encoded white value with the same transfer function as sRGB, so
// the component maps straight across; only the number of channels changes.
// Keeping this as a named function documents that no gamma conversion is
// intentionally being skipped.
func srgbFromLinearGray(white float64) float64 { return white }

// parseColor interprets an Interface Builder <color> element.
//
// Every form Interface Builder emits is handled:
//
//	<color red=".." green=".." blue=".." alpha=".." colorSpace="custom" customColorSpace="sRGB"/>
//	<color red=".." green=".." blue=".." alpha=".." colorSpace="calibratedRGB"/>
//	<color white="1" alpha="1" colorSpace="custom" customColorSpace="calibratedWhite"/>
//	<color systemColor="systemBackgroundColor"/>
//	<color cocoaTouchSystemColor="whiteColor"/>
//	<color name="LaunchBackground"/>
//
// An unrecognised form returns an error naming the form, so the caller reports
// it as unsupported instead of emitting a wrong colour.
func parseColor(n *node, named map[string]Color) (Color, error) {
	if n == nil {
		return Color{}, fmt.Errorf("missing color element")
	}
	// Catalog color reference. A <namedColor> definition in <resources> gives
	// the concrete components; without one, only the name is known and the
	// asset catalog must already contain the colorset.
	if name := n.attr("name"); name != "" {
		c := Color{CatalogName: name, A: 1}
		if def, ok := named[name]; ok {
			def.CatalogName = name
			return def, nil
		}
		return c, nil
	}
	// System color. Interface Builder spells these two ways depending on the
	// document's tools version.
	sysName := n.attr("systemColor")
	if sysName == "" {
		sysName = n.attr("cocoaTouchSystemColor")
	}
	if sysName != "" {
		sc, ok := systemColors[sysName]
		if !ok {
			return Color{}, fmt.Errorf("unknown system color %q", sysName)
		}
		return Color{
			R: sc.light[0], G: sc.light[1], B: sc.light[2], A: sc.light[3],
			DarkR: sc.dark[0], DarkG: sc.dark[1], DarkB: sc.dark[2], DarkA: sc.dark[3],
			Dynamic:    sc.dynamic,
			SystemName: sysName,
		}, nil
	}
	// Literal white + alpha.
	if w := n.attr("white"); w != "" {
		white, err := parseComponent(w)
		if err != nil {
			return Color{}, fmt.Errorf("white component: %w", err)
		}
		alpha, err := parseAlpha(n)
		if err != nil {
			return Color{}, err
		}
		v := srgbFromLinearGray(white)
		return Color{R: v, G: v, B: v, A: alpha}, nil
	}
	// Literal RGB + alpha.
	if r := n.attr("red"); r != "" {
		red, err := parseComponent(r)
		if err != nil {
			return Color{}, fmt.Errorf("red component: %w", err)
		}
		green, err := parseComponent(n.attr("green"))
		if err != nil {
			return Color{}, fmt.Errorf("green component: %w", err)
		}
		blue, err := parseComponent(n.attr("blue"))
		if err != nil {
			return Color{}, fmt.Errorf("blue component: %w", err)
		}
		alpha, err := parseAlpha(n)
		if err != nil {
			return Color{}, err
		}
		return Color{R: red, G: green, B: blue, A: alpha}, nil
	}
	return Color{}, fmt.Errorf("unrecognised color form (attributes %v)", sortedAttrNames(n))
}

func parseAlpha(n *node) (float64, error) {
	a := n.attr("alpha")
	if a == "" {
		return 1, nil
	}
	v, err := parseComponent(a)
	if err != nil {
		return 0, fmt.Errorf("alpha component: %w", err)
	}
	return v, nil
}

func parseComponent(s string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < 0 || v > 1 || math.IsNaN(v) {
		return 0, fmt.Errorf("%q out of range 0..1", s)
	}
	return v, nil
}

// namedColorDefinitions reads the <resources><namedColor> definitions of a
// document. Interface Builder mirrors the asset catalog's colorset values there,
// which lets `ripley` synthesize the colorset even when the catalog lacks it.
func namedColorDefinitions(doc *node) map[string]Color {
	res := doc.child("resources")
	if res == nil {
		return nil
	}
	out := map[string]Color{}
	for _, nc := range res.children("namedColor") {
		name := nc.attr("name")
		if name == "" {
			continue
		}
		inner := nc.child("color")
		if inner == nil {
			continue
		}
		c, err := parseColor(inner, nil)
		if err != nil {
			continue
		}
		c.CatalogName = name
		out[name] = c
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// colorsetName returns the asset catalog color name `ripley` uses for a literal
// storyboard colour. UILaunchScreen's UIColorName can only name a catalog
// colour, so a literal <color> has to be materialised as a generated colorset.
// The name is derived from the components so two different literals never
// collide and an identical literal reuses one colorset.
func (c Color) colorsetName() string {
	if c.CatalogName != "" {
		return c.CatalogName
	}
	if c.SystemName != "" {
		return "FLLaunchColor" + capitalize(c.SystemName)
	}
	return fmt.Sprintf("FLLaunchColor%02X%02X%02X%02X", channel(c.R), channel(c.G), channel(c.B), channel(c.A))
}

func channel(v float64) int {
	n := int(math.Round(v * 255))
	if n < 0 {
		n = 0
	}
	if n > 255 {
		n = 255
	}
	return n
}

// capitalize upper-cases the first character so a UIColor selector name becomes
// a CamelCase colorset name suffix.
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
