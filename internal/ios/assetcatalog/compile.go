package assetcatalog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Compile lowers the given `.xcassets` directories into outDir.
//
// Two representations are produced from the same parse:
//
//   - `Assets.car`, the compiled CoreUI archive, holding every image, named
//     color and data asset. This is the only representation that supports
//     `UIColor(named:)` and `NSDataAsset`.
//   - loose bundle resources (`<Name>@2x.png`, `AppIcon60x60@2x.png`, ...) plus
//     the `CFBundleIcons` family of `Info.plist` keys, which is what UIKit and
//     SpringBoard fall back to. Emitting both means app icons still work when
//     archive fidelity is imperfect.
//
// Every path in Result.LooseFiles is absolute and lives under outDir; the
// caller copies them into the bundle root. Result.InfoPlistKeys must be merged
// into the application `Info.plist`.
//
// A catalog containing only unsupported idioms or asset kinds is not an error:
// Compile returns an empty-ish Result whose Skipped field explains, item by
// item, what was left out.
func Compile(catalogDirs []string, outDir string, opt Options) (*Result, error) {
	cat, err := Parse(catalogDirs, opt)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("asset catalog: create output directory: %w", err)
	}

	res := &Result{
		InfoPlistKeys: map[string]any{},
		Skipped:       cat.Skipped,
	}

	if err := writeLooseFiles(cat, outDir, res); err != nil {
		return nil, err
	}
	writeIconPlistKeys(cat, res)

	archive, err := buildArchive(cat)
	if err != nil {
		return nil, err
	}
	if archive == nil {
		return res, nil
	}

	data, err := archive.write()
	if err != nil {
		return nil, err
	}
	carPath := filepath.Join(outDir, "Assets.car")
	if err := os.WriteFile(carPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("asset catalog: write %s: %w", carPath, err)
	}
	res.CarPath = carPath
	return res, nil
}

// buildArchive turns a parsed catalog into a CAR archive, or nil when there is
// nothing to compile.
func buildArchive(cat *Catalog) (*carArchive, error) {
	a := &carArchive{deploymentTarget: cat.DeploymentTarget}
	ids := newIdentifierAllocator()

	if err := addImageRenditions(a, ids, cat.Images, partImage); err != nil {
		return nil, err
	}
	if err := addAppIconRenditions(a, ids, cat); err != nil {
		return nil, err
	}
	if err := addColorRenditions(a, ids, cat.Colors); err != nil {
		return nil, err
	}
	if err := addDataRenditions(a, ids, cat.Data); err != nil {
		return nil, err
	}

	if len(a.renditions) == 0 {
		return nil, nil
	}
	a.useAppearance(AppearanceAny)
	return a, nil
}

// identifierAllocator hands out the 16-bit facet identifiers that tie
// renditions to their facet, keeping them stable per name and collision-free.
type identifierAllocator struct {
	byName map[string]uint16
	used   map[uint16]bool
}

func newIdentifierAllocator() *identifierAllocator {
	return &identifierAllocator{byName: map[string]uint16{}, used: map[uint16]bool{}}
}

func (al *identifierAllocator) get(name string) uint16 {
	if id, ok := al.byName[name]; ok {
		return id
	}
	id := facetIdentifier(name)
	for id == 0 || al.used[id] {
		id++
	}
	al.byName[name] = id
	al.used[id] = true
	return id
}

// addImageRenditions adds one facet per image name plus a rendition per
// variant. Vector sources have no rasterizer available offline, so they are
// reported as loose-only rather than emitted as a broken rendition.
func addImageRenditions(a *carArchive, ids *identifierAllocator, images []Image, part uint16) error {
	seen := map[string]bool{}
	for _, img := range images {
		if img.Vector {
			continue
		}
		id := ids.get(img.Name)
		if !seen[img.Name] {
			seen[img.Name] = true
			a.addFacet(facet{name: img.Name, identifier: id, part: part})
		}
		r, err := imageRendition(img, id, part, nil)
		if err != nil {
			return err
		}
		a.useAppearance(img.Appearance)
		a.addRendition(r)
	}
	return nil
}

// imageRendition decodes an image file and packs it into a CAR rendition.
// iconSizes supplies the archive's icon size ladder and is only consulted for
// icon-image renditions.
func imageRendition(img Image, id uint16, part uint16, iconSizes []float64) (rendition, error) {
	key := renditionKey{
		appearance: uint16(img.Appearance),
		scale:      uint16(img.Scale),
		idiom:      carIdiomValue(img.Idiom),
		subtype:    uint16(img.Subtype),
		gamut:      uint16(img.Gamut),
		identifier: id,
		element:    elementAsset,
		part:       part,
	}
	if part == partIconImage {
		// Icon renditions are indexed by their size slot inside the icon set.
		key.dimension2 = uint16(iconSizeIndex(img, iconSizes))
	}

	r := rendition{
		key:    key,
		name:   filepath.Base(img.Path),
		width:  img.PixelWidth,
		height: img.PixelHeight,
		scale:  int(img.Scale) * 100,
		layout: layoutOnePartScale,
		flags:  renditionFlags(false, img.Template),
	}
	if r.scale == 0 {
		r.scale = 100
	}

	switch img.Encoding {
	case EncodingJPEG:
		data, err := os.ReadFile(img.Path)
		if err != nil {
			return rendition{}, fmt.Errorf("asset catalog: read %s: %w", img.Path, err)
		}
		r.pixelFormat = pixelFormatJPEG
		r.colorSpace = colorSpaceSRGB
		r.info = imageInfoSections(img.PixelWidth, img.PixelHeight, 0)
		r.body = encodeRawPayload(data)
		return r, nil
	case EncodingGA8:
		pixels, err := decodePixels(img.Path, EncodingGA8)
		if err != nil {
			return rendition{}, err
		}
		body, err := encodeBitmapPayload(pixels)
		if err != nil {
			return rendition{}, err
		}
		r.pixelFormat = pixelFormatGA8
		r.colorSpace = colorSpaceGray
		r.info = imageInfoSections(img.PixelWidth, img.PixelHeight, bytesPerRow(img.PixelWidth, EncodingGA8))
		r.body = body
		return r, nil
	default:
		pixels, err := decodePixels(img.Path, EncodingARGB)
		if err != nil {
			return rendition{}, err
		}
		body, err := encodeBitmapPayload(pixels)
		if err != nil {
			return rendition{}, err
		}
		r.pixelFormat = pixelFormatARGB
		r.colorSpace = colorSpaceSRGB
		r.info = imageInfoSections(img.PixelWidth, img.PixelHeight, bytesPerRow(img.PixelWidth, EncodingARGB))
		r.body = body
		return r, nil
	}
}

// addAppIconRenditions adds the icon-image renditions plus the per-idiom
// multi-size (`MSIS`) renditions CoreUI uses to enumerate icon variants.
func addAppIconRenditions(a *carArchive, ids *identifierAllocator, cat *Catalog) error {
	if len(cat.AppIcons) == 0 {
		return nil
	}
	id := ids.get(cat.AppIconName)
	a.addFacet(facet{name: cat.AppIconName, identifier: id, part: partMultiSizeImage})

	// CoreUI's Icon Index is a position in the set's own ascending size list.
	ladder := iconSizeLadder(cat.AppIcons)

	for _, img := range cat.AppIcons {
		if img.Vector {
			continue
		}
		r, err := imageRendition(img, id, partIconImage, ladder)
		if err != nil {
			return err
		}
		a.useAppearance(img.Appearance)
		a.addRendition(r)
	}

	// One MSIS rendition per (idiom, subtype), listing that idiom's sizes.
	type group struct {
		idiom   Idiom
		subtype int
	}
	order := []group{}
	sizes := map[group][]multiSizeEntry{}
	for _, img := range cat.AppIcons {
		if img.SizePoints <= 0 {
			continue
		}
		g := group{idiom: img.Idiom, subtype: img.Subtype}
		e := multiSizeEntry{
			width: uint32(img.SizePoints),
			// CoreUI stores integral point sizes; 83.5 becomes 83.
			height: uint32(img.SizePoints),
			index:  uint32(iconSizeIndex(img, ladder)),
		}
		if _, ok := sizes[g]; !ok {
			order = append(order, g)
		}
		dup := false
		for _, existing := range sizes[g] {
			if existing.index == e.index {
				dup = true
				break
			}
		}
		if !dup {
			sizes[g] = append(sizes[g], e)
		}
	}

	for _, g := range order {
		entries := sizes[g]
		sort.Slice(entries, func(i, j int) bool { return entries[i].index < entries[j].index })
		a.addRendition(rendition{
			key: renditionKey{
				scale:      1,
				idiom:      carIdiomValue(g.idiom),
				subtype:    uint16(g.subtype),
				identifier: id,
				element:    elementAsset,
				part:       partMultiSizeImage,
			},
			name:   cat.AppIconName,
			layout: layoutMultiSizeImage,
			info:   []csiInfo{compositionInfo(0), {magic: infoBitmapInfo, payload: u32s(1)}},
			body:   encodeMultiSizePayload(entries),
		})
	}
	return nil
}

// iconSizeLadder is the ascending list of distinct icon point sizes present in
// an app icon set. CoreUI's "Icon Index" is a 1-based position in this list.
//
// The numbering is relative to the set, not to a fixed table of iOS sizes:
// `actool` compiled a {60, 1024} set as 60->1, 1024->2, a {40, 76} set as
// 40->1, 76->2, and a {20, 29, 40, 60, 76, 83.5, 1024} set as 20->1 .. 1024->7.
// It spans idioms, so an iPad-only size shares one ladder with the iPhone ones.
func iconSizeLadder(icons []Image) []float64 {
	var ladder []float64
	for _, img := range icons {
		if img.SizePoints <= 0 {
			continue
		}
		seen := false
		for _, s := range ladder {
			if s == img.SizePoints {
				seen = true
				break
			}
		}
		if !seen {
			ladder = append(ladder, img.SizePoints)
		}
	}
	sort.Float64s(ladder)
	return ladder
}

// iconSizeIndex is the "Icon Index" CoreUI stores in a rendition's dimension2
// token and in the multi-size payload. Slots are shared across scales, so the
// 2x and 3x renditions of a 20pt icon both carry the same index.
func iconSizeIndex(img Image, ladder []float64) int {
	for i, s := range ladder {
		if s == img.SizePoints {
			return i + 1
		}
	}
	return 0
}

// marketingIconPoints is the 1024pt App Store icon size. It is delivered to the
// App Store inside the compiled archive and is never a bundle resource: neither
// a loose file nor a CFBundleIconFiles entry. Catalogs in the wild declare it
// with idiom `ios-marketing` (localsend, zulip) or plain `universal` (saber),
// so it must be recognised by size rather than by idiom.
const marketingIconPoints = 1024

// addColorRenditions adds one facet and rendition per named color variant.
func addColorRenditions(a *carArchive, ids *identifierAllocator, colors []Color) error {
	seen := map[string]bool{}
	for _, c := range colors {
		id := ids.get(c.Name)
		if !seen[c.Name] {
			seen[c.Name] = true
			a.addFacet(facet{name: c.Name, identifier: id, part: partColor})
		}
		space := uint32(colorSpaceSRGB)
		if c.Gamut == GamutP3 {
			space = colorSpaceP3
		}
		a.useAppearance(c.Appearance)
		a.addRendition(rendition{
			key: renditionKey{
				appearance: uint16(c.Appearance),
				scale:      1,
				idiom:      carIdiomValue(c.Idiom),
				gamut:      uint16(c.Gamut),
				identifier: id,
				element:    elementAsset,
				part:       partColor,
			},
			name:   c.Name,
			layout: layoutColor,
			info:   []csiInfo{compositionInfo(0), {magic: infoBitmapInfo, payload: u32s(1)}},
			body:   encodeColorPayload(space, [4]float64{c.R, c.G, c.B, c.A}),
		})
	}
	return nil
}

// addDataRenditions adds one facet and raw-data rendition per data asset.
func addDataRenditions(a *carArchive, ids *identifierAllocator, datas []Data) error {
	seen := map[string]bool{}
	for _, d := range datas {
		payload, err := os.ReadFile(d.Path)
		if err != nil {
			return fmt.Errorf("asset catalog: read %s: %w", d.Path, err)
		}
		id := ids.get(d.Name)
		if !seen[d.Name] {
			seen[d.Name] = true
			a.addFacet(facet{name: d.Name, identifier: id, part: partImage})
		}
		info := []csiInfo{compositionInfo(1)}
		if d.UTI != "" {
			info = append(info, utiInfo(d.UTI))
		}
		info = append(info, csiInfo{magic: infoBitmapInfo, payload: u32s(1)})
		a.useAppearance(d.Appearance)
		a.addRendition(rendition{
			key: renditionKey{
				appearance: uint16(d.Appearance),
				scale:      1,
				idiom:      carIdiomValue(d.Idiom),
				identifier: id,
				element:    elementAsset,
				part:       partImage,
			},
			name:        filepath.Base(d.Path),
			scale:       100,
			layout:      layoutRawData,
			pixelFormat: pixelFormatData,
			info:        info,
			body:        encodeRawPayload(payload),
		})
	}
	return nil
}

// writeLooseFiles copies every non-icon image variant and every app icon into
// outDir under the names UIKit resolves by convention.
func writeLooseFiles(cat *Catalog, outDir string, res *Result) error {
	written := map[string]bool{}
	emit := func(src, name string) error {
		if written[name] {
			return nil
		}
		written[name] = true
		dst := filepath.Join(outDir, name)
		if err := copyFile(src, dst); err != nil {
			return err
		}
		res.LooseFiles = append(res.LooseFiles, dst)
		return nil
	}

	for _, img := range cat.Images {
		if err := emit(img.Path, looseImageName(img)); err != nil {
			return err
		}
	}
	for _, img := range cat.AppIcons {
		if img.SizePoints == marketingIconPoints {
			// The 1024 App Store icon ships only inside the archive.
			continue
		}
		if err := emit(img.Path, looseIconName(cat.AppIconName, img)); err != nil {
			return err
		}
	}
	return nil
}

// looseImageName is `<Name>[@Nx][~idiom].<ext>`, the UIKit fallback naming for
// an image set variant. Namespaced names keep only their leaf, since loose
// resources live flat in the bundle root.
func looseImageName(img Image) string {
	base := img.Name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base + scaleSuffix(img.Scale) + idiomSuffix(img.Idiom) + strings.ToLower(filepath.Ext(img.Path))
}

// looseIconName is `<AppIcon><WxH>[@Nx][~ipad].png`, the naming SpringBoard
// resolves from `CFBundleIconFiles`.
func looseIconName(setName string, img Image) string {
	return setName + img.Size + scaleSuffix(img.Scale) + idiomSuffix(img.Idiom) + ".png"
}

func scaleSuffix(s Scale) string {
	if s <= 1 {
		return ""
	}
	return fmt.Sprintf("@%dx", int(s))
}

// writeIconPlistKeys emits the `CFBundleIcons` family. `CFBundleIconFiles`
// holds base names without scale or extension, per-idiom, in the order the
// catalog declares them; SpringBoard picks the best match at runtime.
func writeIconPlistKeys(cat *Catalog, res *Result) {
	if len(cat.AppIcons) == 0 {
		return
	}

	idiomOrder := []Idiom{}
	files := map[Idiom][]string{}
	for _, img := range cat.AppIcons {
		if img.SizePoints == marketingIconPoints {
			// The 1024 marketing icon is an App Store asset, never a bundle
			// resource entry.
			continue
		}
		name := cat.AppIconName + img.Size
		if _, ok := files[img.Idiom]; !ok {
			idiomOrder = append(idiomOrder, img.Idiom)
		}
		dup := false
		for _, existing := range files[img.Idiom] {
			if existing == name {
				dup = true
				break
			}
		}
		if !dup {
			files[img.Idiom] = append(files[img.Idiom], name)
		}
	}

	// The ~ipad dictionary must list the phone icons too: iPad resolves
	// CFBundleIcons~ipad exclusively, and Apple's compiler merges both sets.
	universal := append([]string{}, files[IdiomUniversal]...)
	phone := append(append([]string{}, universal...), files[IdiomIPhone]...)
	pad := append(append([]string{}, phone...), files[IdiomIPad]...)

	primary := func(names []string) map[string]any {
		icon := map[string]any{
			"CFBundleIconFiles": names,
			"CFBundleIconName":  cat.AppIconName,
		}
		if cat.PreRenderedIcon {
			icon["UIPrerenderedIcon"] = true
		}
		return map[string]any{"CFBundlePrimaryIcon": icon}
	}

	if len(phone) > 0 {
		res.InfoPlistKeys["CFBundleIcons"] = primary(phone)
	}
	hasPad := false
	for _, i := range idiomOrder {
		if i == IdiomIPad {
			hasPad = true
		}
	}
	if hasPad && len(pad) > 0 {
		res.InfoPlistKeys["CFBundleIcons~ipad"] = primary(pad)
	}

	// Flat keys for pre-iOS-5 style lookups and tooling that reads them
	// directly; Apple's compiler emits CFBundleIconName as well.
	if len(pad) > 0 {
		res.InfoPlistKeys["CFBundleIconFiles"] = pad
	} else if len(phone) > 0 {
		res.InfoPlistKeys["CFBundleIconFiles"] = phone
	}
	res.InfoPlistKeys["CFBundleIconName"] = cat.AppIconName
	if cat.PreRenderedIcon {
		res.InfoPlistKeys["UIPrerenderedIcon"] = true
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("asset catalog: open %s: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("asset catalog: create %s: %w", filepath.Dir(dst), err)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("asset catalog: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("asset catalog: copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("asset catalog: close %s: %w", dst, err)
	}
	return nil
}
