package assetcatalog

import (
	"encoding/json"
	"fmt"

	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// contentsFile is the subset of `Contents.json` this compiler consumes. Xcode
// writes one at the catalog root and one per asset directory.
type contentsFile struct {
	Info struct {
		Author  string `json:"author"`
		Version int    `json:"version"`
	} `json:"info"`
	Properties struct {
		PreRendered        *bool   `json:"pre-rendered"`
		ProvidesNamespace  *bool   `json:"provides-namespace"`
		TemplateRendering  *string `json:"template-rendering-intent"`
		OnDemandResourceID *string `json:"on-demand-resource-tags"`
	} `json:"properties"`
	Images []contentsImage `json:"images"`
	Colors []contentsColor `json:"colors"`
	Data   []contentsData  `json:"data"`
}

type contentsAppearance struct {
	Appearance string `json:"appearance"`
	Value      string `json:"value"`
}

type contentsImage struct {
	Filename          string               `json:"filename"`
	Idiom             string               `json:"idiom"`
	Scale             string               `json:"scale"`
	Size              string               `json:"size"`
	Subtype           string               `json:"subtype"`
	Unassigned        *bool                `json:"unassigned"`
	Appearances       []contentsAppearance `json:"appearances"`
	DisplayGamut      string               `json:"display-gamut"`
	TemplateRendering string               `json:"template-rendering-intent"`
	Role              string               `json:"role"`
	Platform          string               `json:"platform"`
	// Extent/orientation belong to `.launchimage` entries.
	Extent      string `json:"extent"`
	Orientation string `json:"orientation"`
	MinimumOS   string `json:"minimum-system-version"`
}

type contentsColorValue struct {
	ColorSpace string          `json:"color-space"`
	Reference  string          `json:"reference"`
	Components json.RawMessage `json:"components"`
}

type contentsColor struct {
	Idiom        string               `json:"idiom"`
	Color        *contentsColorValue  `json:"color"`
	Appearances  []contentsAppearance `json:"appearances"`
	DisplayGamut string               `json:"display-gamut"`
	Subtype      string               `json:"subtype"`
}

type contentsData struct {
	Filename    string               `json:"filename"`
	Idiom       string               `json:"idiom"`
	UTI         string               `json:"universal-type-identifier"`
	Appearances []contentsAppearance `json:"appearances"`
}

// colorComponents holds the raw component strings/numbers of a colorset entry.
// Xcode writes either floats ("0.5"), 8-bit integers ("128"), hex ("0x80") or
// JSON numbers, and may use `white` instead of `red`/`green`/`blue`.
type colorComponents struct {
	Red   json.RawMessage `json:"red"`
	Green json.RawMessage `json:"green"`
	Blue  json.RawMessage `json:"blue"`
	White json.RawMessage `json:"white"`
	Alpha json.RawMessage `json:"alpha"`
}

// Parse reads the given `.xcassets` directories and returns the platform
// filtered catalog. Unsupported idioms and asset kinds are recorded in
// Catalog.Skipped instead of failing the build.
func Parse(catalogDirs []string, opt Options) (*Catalog, error) {
	accepted := map[Idiom]bool{}
	if len(opt.Idioms) == 0 {
		for k, v := range iOSIdioms {
			accepted[k] = v
		}
	} else {
		for _, i := range opt.Idioms {
			accepted[Idiom(i)] = true
		}
	}

	cat := &Catalog{DeploymentTarget: opt.DeploymentTarget}
	iconSets := map[string]*appIconSet{}

	for _, dir := range catalogDirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		if err := walkCatalog(abs, abs, "", accepted, cat, iconSets); err != nil {
			return nil, err
		}
	}

	if err := selectAppIcon(cat, iconSets, opt.AppIconName); err != nil {
		return nil, err
	}
	return cat, nil
}

// appIconSet is an app icon set discovered during the walk. The chosen one
// becomes Catalog.AppIcons; the others are compiled as ordinary image sets so
// that alternate icons still resolve at runtime.
type appIconSet struct {
	name        string
	catalog     string
	preRendered bool
	images      []Image
}

// walkCatalog recurses through a catalog directory. namespace accumulates the
// prefix contributed by folders declaring `provides-namespace`.
func walkCatalog(root, dir, namespace string, accepted map[Idiom]bool, cat *Catalog, icons map[string]*appIconSet) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read asset catalog %s: %w", dir, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(dir, name)
		ext := strings.TrimPrefix(filepath.Ext(name), ".")
		base := strings.TrimSuffix(name, filepath.Ext(name))

		switch ext {
		case "imageset":
			if err := parseImageSet(root, path, qualify(namespace, base), accepted, cat); err != nil {
				return err
			}
		case "appiconset":
			set, err := parseAppIconSet(root, path, qualify(namespace, base), accepted, cat)
			if err != nil {
				return err
			}
			icons[set.name] = set
		case "colorset":
			if err := parseColorSet(root, path, qualify(namespace, base), accepted, cat); err != nil {
				return err
			}
		case "dataset":
			if err := parseDataSet(root, path, qualify(namespace, base), accepted, cat); err != nil {
				return err
			}
		case "launchimage":
			// Launch images are legacy; UIKit loads them as loose files by
			// their generated names, so they compile like an image set but
			// their extent/orientation qualifiers are recorded as skipped.
			if err := parseLaunchImage(root, path, qualify(namespace, base), accepted, cat); err != nil {
				return err
			}
		case "":
			// A plain folder: it may declare a namespace and holds nested assets.
			ns := namespace
			if c, err := readContents(filepath.Join(path, "Contents.json")); err == nil && c != nil {
				if c.Properties.ProvidesNamespace != nil && *c.Properties.ProvidesNamespace {
					ns = qualify(namespace, name)
				}
			}
			if err := walkCatalog(root, path, ns, accepted, cat, icons); err != nil {
				return err
			}
		default:
			cat.Skipped = append(cat.Skipped, SkippedItem{
				Catalog: root,
				Item:    relTo(root, path),
				Kind:    ext,
				Reason:  "asset kind is not supported on iOS by ripley",
			})
		}
	}
	return nil
}

func qualify(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// readContents parses a Contents.json. A missing file yields (nil, nil) so that
// callers can treat asset directories without metadata as empty.
func readContents(path string) (*contentsFile, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c contentsFile
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

func parseImageSet(root, dir, name string, accepted map[Idiom]bool, cat *Catalog) error {
	c, err := readContents(filepath.Join(dir, "Contents.json"))
	if err != nil {
		return err
	}
	if c == nil {
		cat.Skipped = append(cat.Skipped, SkippedItem{
			Catalog: root, Item: relTo(root, dir), Kind: "imageset",
			Reason: "no Contents.json",
		})
		return nil
	}

	setIntent := TemplateAutomatic
	if c.Properties.TemplateRendering != nil {
		setIntent = parseTemplateIntent(*c.Properties.TemplateRendering)
	}

	for _, ci := range c.Images {
		img, skip := parseImageEntry(root, dir, name, ci, accepted, setIntent)
		if skip != nil {
			cat.Skipped = append(cat.Skipped, *skip)
			continue
		}
		if img == nil {
			continue
		}
		cat.Images = append(cat.Images, *img)
	}
	return nil
}

func parseLaunchImage(root, dir, name string, accepted map[Idiom]bool, cat *Catalog) error {
	c, err := readContents(filepath.Join(dir, "Contents.json"))
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	for _, ci := range c.Images {
		img, skip := parseImageEntry(root, dir, name, ci, accepted, TemplateAutomatic)
		if skip != nil {
			cat.Skipped = append(cat.Skipped, *skip)
			continue
		}
		if img == nil {
			continue
		}
		// Launch images with an extent/orientation qualifier cannot be
		// distinguished by the loose-file naming scheme UIKit uses for
		// `UILaunchImageFile`; record them so the caller can report it.
		if ci.Extent != "" || ci.Orientation != "" {
			cat.Skipped = append(cat.Skipped, SkippedItem{
				Catalog: root, Item: relTo(root, filepath.Join(dir, ci.Filename)),
				Kind:   "launchimage",
				Reason: "extent/orientation qualifiers are not modelled; rendition compiled without them",
			})
		}
		cat.Images = append(cat.Images, *img)
	}
	return nil
}

func parseAppIconSet(root, dir, name string, accepted map[Idiom]bool, cat *Catalog) (*appIconSet, error) {
	set := &appIconSet{name: name, catalog: root}
	c, err := readContents(filepath.Join(dir, "Contents.json"))
	if err != nil {
		return nil, err
	}
	if c == nil {
		cat.Skipped = append(cat.Skipped, SkippedItem{
			Catalog: root, Item: relTo(root, dir), Kind: "appiconset",
			Reason: "no Contents.json",
		})
		return set, nil
	}
	if c.Properties.PreRendered != nil {
		set.preRendered = *c.Properties.PreRendered
	}

	for _, ci := range c.Images {
		img, skip := parseImageEntry(root, dir, name, ci, accepted, TemplateAutomatic)
		if skip != nil {
			cat.Skipped = append(cat.Skipped, *skip)
			continue
		}
		if img == nil {
			continue
		}
		if img.Size == "" {
			// A single-size 1024 icon set: Xcode omits `size` and `idiom`
			// defaults to universal. Derive the size from the pixels.
			if img.PixelWidth > 0 && img.PixelWidth == img.PixelHeight {
				scale := int(img.Scale)
				if scale == 0 {
					scale = 1
				}
				pts := img.PixelWidth / scale
				img.Size = fmt.Sprintf("%dx%d", pts, pts)
				img.SizePoints = float64(pts)
			} else {
				cat.Skipped = append(cat.Skipped, SkippedItem{
					Catalog: root, Item: relTo(root, filepath.Join(dir, ci.Filename)),
					Kind:   "appiconset",
					Reason: "icon has no size and non-square pixels",
				})
				continue
			}
		}
		set.images = append(set.images, *img)
	}
	return set, nil
}

// parseImageEntry converts one Contents.json image entry. It returns (nil, nil)
// for entries that are placeholders (no filename / explicitly unassigned).
func parseImageEntry(root, dir, name string, ci contentsImage, accepted map[Idiom]bool, setIntent TemplateIntent) (*Image, *SkippedItem) {
	if ci.Filename == "" || (ci.Unassigned != nil && *ci.Unassigned) {
		return nil, nil
	}
	idiom := Idiom(ci.Idiom)
	if idiom == "" {
		idiom = IdiomUniversal
	}
	path := filepath.Join(dir, ci.Filename)
	if !accepted[idiom] {
		return nil, &SkippedItem{
			Catalog: root, Item: relTo(root, path), Kind: filepath.Ext(dir),
			Reason: fmt.Sprintf("idiom %q is not an iOS idiom", idiom),
		}
	}
	if ci.Platform != "" && ci.Platform != "ios" {
		return nil, &SkippedItem{
			Catalog: root, Item: relTo(root, path), Kind: filepath.Ext(dir),
			Reason: fmt.Sprintf("platform %q is not ios", ci.Platform),
		}
	}

	img := Image{
		Name:       name,
		Idiom:      idiom,
		Size:       ci.Size,
		Path:       path,
		Template:   setIntent,
		Appearance: parseAppearances(ci.Appearances),
		Gamut:      parseGamut(ci.DisplayGamut),
	}
	if ci.TemplateRendering != "" {
		img.Template = parseTemplateIntent(ci.TemplateRendering)
	}
	if ci.Scale != "" {
		s, err := parseScale(ci.Scale)
		if err != nil {
			return nil, &SkippedItem{
				Catalog: root, Item: relTo(root, path), Kind: filepath.Ext(dir),
				Reason: err.Error(),
			}
		}
		img.Scale = s
	}
	if ci.Size != "" {
		pts, err := parseIconSize(ci.Size)
		if err != nil {
			return nil, &SkippedItem{
				Catalog: root, Item: relTo(root, path), Kind: filepath.Ext(dir),
				Reason: err.Error(),
			}
		}
		img.SizePoints = pts
	}
	if ci.Subtype != "" {
		st, err := strconv.Atoi(ci.Subtype)
		if err != nil {
			return nil, &SkippedItem{
				Catalog: root, Item: relTo(root, path), Kind: filepath.Ext(dir),
				Reason: fmt.Sprintf("invalid subtype %q", ci.Subtype),
			}
		}
		img.Subtype = st
	}

	enc, vector, err := encodingForFile(path)
	if err != nil {
		return nil, &SkippedItem{
			Catalog: root, Item: relTo(root, path), Kind: filepath.Ext(dir),
			Reason: err.Error(),
		}
	}
	img.Encoding = enc
	img.Vector = vector
	if !vector {
		// A vector source has no pixel dimensions until it is rasterized, and
		// no rasterizer is available offline; it stays a loose file only.
		w, h, _, err := imageDimensions(path)
		if err != nil {
			return nil, &SkippedItem{
				Catalog: root, Item: relTo(root, path), Kind: filepath.Ext(dir),
				Reason: err.Error(),
			}
		}
		img.PixelWidth, img.PixelHeight = w, h
	}

	return &img, nil
}

func parseColorSet(root, dir, name string, accepted map[Idiom]bool, cat *Catalog) error {
	c, err := readContents(filepath.Join(dir, "Contents.json"))
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	for _, cc := range c.Colors {
		idiom := Idiom(cc.Idiom)
		if idiom == "" {
			idiom = IdiomUniversal
		}
		if !accepted[idiom] {
			cat.Skipped = append(cat.Skipped, SkippedItem{
				Catalog: root, Item: relTo(root, dir), Kind: "colorset",
				Reason: fmt.Sprintf("idiom %q is not an iOS idiom", idiom),
			})
			continue
		}
		if cc.Color == nil {
			// An entry without a color is a placeholder ("Any" unset).
			continue
		}
		if cc.Color.Reference != "" {
			cat.Skipped = append(cat.Skipped, SkippedItem{
				Catalog: root, Item: relTo(root, dir), Kind: "colorset",
				Reason: fmt.Sprintf("system color reference %q cannot be resolved offline", cc.Color.Reference),
			})
			continue
		}
		r, g, b, a, err := decodeColorComponents(cc.Color.Components)
		if err != nil {
			return fmt.Errorf("%s: %w", relTo(root, dir), err)
		}
		cat.Colors = append(cat.Colors, Color{
			Name:       name,
			Idiom:      idiom,
			R:          r,
			G:          g,
			B:          b,
			A:          a,
			Appearance: parseAppearances(cc.Appearances),
			Gamut:      parseGamut(cc.DisplayGamut),
		})
	}
	return nil
}

func parseDataSet(root, dir, name string, accepted map[Idiom]bool, cat *Catalog) error {
	c, err := readContents(filepath.Join(dir, "Contents.json"))
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	for _, cd := range c.Data {
		if cd.Filename == "" {
			continue
		}
		idiom := Idiom(cd.Idiom)
		if idiom == "" {
			idiom = IdiomUniversal
		}
		path := filepath.Join(dir, cd.Filename)
		if !accepted[idiom] {
			cat.Skipped = append(cat.Skipped, SkippedItem{
				Catalog: root, Item: relTo(root, path), Kind: "dataset",
				Reason: fmt.Sprintf("idiom %q is not an iOS idiom", idiom),
			})
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("data asset %s: %w", relTo(root, path), err)
		}
		cat.Data = append(cat.Data, Data{
			Name:       name,
			Idiom:      idiom,
			Path:       path,
			UTI:        cd.UTI,
			Appearance: parseAppearances(cd.Appearances),
		})
	}
	return nil
}

// selectAppIcon chooses the application icon set. Non-selected app icon sets
// are still compiled, as ordinary named images, so `UIApplication`'s alternate
// icon API can find them.
func selectAppIcon(cat *Catalog, sets map[string]*appIconSet, requested string) error {
	if len(sets) == 0 {
		return nil
	}
	chosen := ""
	switch {
	case requested != "":
		if _, ok := sets[requested]; !ok {
			return fmt.Errorf("app icon set %q not found in asset catalogs", requested)
		}
		chosen = requested
	case len(sets) == 1:
		for k := range sets {
			chosen = k
		}
	default:
		if _, ok := sets["AppIcon"]; ok {
			chosen = "AppIcon"
		}
	}

	names := make([]string, 0, len(sets))
	for k := range sets {
		names = append(names, k)
	}
	sortStrings(names)

	for _, n := range names {
		set := sets[n]
		if n == chosen {
			cat.AppIconName = set.name
			cat.PreRenderedIcon = set.preRendered
			cat.AppIcons = append(cat.AppIcons, set.images...)
			continue
		}
		// Alternate icon sets are compiled as plain images keyed by their set
		// name; the icon size becomes part of the rendition's own name so the
		// distinct sizes do not collide on one key.
		for _, img := range set.images {
			alt := img
			alt.Size = ""
			alt.SizePoints = 0
			alt.Subtype = 0
			cat.Images = append(cat.Images, alt)
		}
	}
	return nil
}

func parseScale(s string) (Scale, error) {
	v := strings.TrimSuffix(s, "x")
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 3 {
		return 0, fmt.Errorf("invalid scale %q", s)
	}
	return Scale(n), nil
}

// parseIconSize converts "60x60" or "83.5x83.5" into points.
func parseIconSize(s string) (float64, error) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid icon size %q", s)
	}
	w, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid icon size %q", s)
	}
	return w, nil
}

func parseTemplateIntent(s string) TemplateIntent {
	switch s {
	case "template":
		return TemplateTemplate
	case "original":
		return TemplateOriginal
	default:
		return TemplateAutomatic
	}
}

// parseAppearances reduces an appearance qualifier list to the single CoreUI
// appearance selector CoreUI stores in a rendition key. Xcode writes
// `luminosity`/`dark`, `luminosity`/`light`, `luminosity`/`tinted` (the iOS 18
// tinted home-screen icon) and `contrast`/`high`, and it may write a
// luminosity and a contrast qualifier together.
//
// The mapping was read back out of `actool` archives: dark and high-contrast
// compose into their own selector, while `tinted` is a separate axis that
// `actool` stores as ISAppearanceTintable.
func parseAppearances(list []contentsAppearance) Appearance {
	dark, contrast, tinted := false, false, false
	for _, a := range list {
		switch {
		case a.Appearance == "luminosity" && a.Value == "dark":
			dark = true
		case a.Appearance == "luminosity" && a.Value == "tinted":
			tinted = true
		case a.Appearance == "contrast" && a.Value == "high":
			contrast = true
		}
	}
	switch {
	case tinted:
		return AppearanceTintable
	case dark && contrast:
		return AppearanceHighContrastDark
	case dark:
		return AppearanceDark
	case contrast:
		return AppearanceHighContrast
	}
	return AppearanceAny
}

func parseGamut(s string) Gamut {
	if s == "display-P3" || s == "p3" || s == "P3" {
		return GamutP3
	}
	return GamutSRGB
}

// decodeColorComponents converts Xcode's component encodings into 0..1 floats.
func decodeColorComponents(raw []byte) (r, g, b, a float64, err error) {
	if len(raw) == 0 {
		return 0, 0, 0, 0, fmt.Errorf("color has no components")
	}
	var comps colorComponents
	if err := json.Unmarshal(raw, &comps); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("parse color components: %w", err)
	}

	a = 1
	if len(comps.Alpha) > 0 {
		a, err = decodeComponent(comps.Alpha)
		if err != nil {
			return
		}
	}

	if len(comps.White) > 0 {
		w, e := decodeComponent(comps.White)
		if e != nil {
			return 0, 0, 0, 0, e
		}
		return w, w, w, a, nil
	}

	if r, err = decodeComponent(comps.Red); err != nil {
		return
	}
	if g, err = decodeComponent(comps.Green); err != nil {
		return
	}
	if b, err = decodeComponent(comps.Blue); err != nil {
		return
	}
	return r, g, b, a, nil
}

// decodeComponent handles the four encodings Xcode emits: a JSON number, a
// decimal float string, an integer 0..255 string, and a "0x.." hex string.
// Xcode's own rule is that a value written as a float (containing '.') is a
// 0..1 fraction, while an integer or hex value is an 8-bit channel.
func decodeComponent(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("missing color component")
	}
	s := string(raw)
	if s == "null" {
		return 0, fmt.Errorf("null color component")
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, err
		}
		str = strings.TrimSpace(str)
		if strings.HasPrefix(str, "0x") || strings.HasPrefix(str, "0X") {
			n, err := strconv.ParseUint(str[2:], 16, 16)
			if err != nil {
				return 0, fmt.Errorf("invalid hex color component %q", str)
			}
			return float64(n) / 255, nil
		}
		v, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid color component %q", str)
		}
		if strings.ContainsAny(str, ".eE") {
			return v, nil
		}
		return v / 255, nil
	}

	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("invalid color component %s", s)
	}
	// A bare JSON number is a fraction unless it exceeds 1, in which case
	// Xcode meant an 8-bit channel.
	if v > 1 {
		return v / 255, nil
	}
	return v, nil
}

// sortStrings is a tiny insertion sort; catalogs hold a handful of icon sets so
// pulling in sort for this would not pay for itself.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
