package assetcatalog

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writePNG writes a w*h PNG whose pixels are a deterministic function of the
// coordinates, so a round-tripped pixel buffer can be checked exactly.
func writePNG(t *testing.T, path string, w, h int, gray bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var img image.Image
	if gray {
		g := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				g.SetGray(x, y, color.Gray{Y: uint8((x*7 + y*13) & 0xff)})
			}
		}
		img = g
	} else {
		n := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				n.SetNRGBA(x, y, color.NRGBA{
					R: uint8(x * 3),
					G: uint8(y * 5),
					B: uint8(x + y),
					A: 255,
				})
			}
		}
		img = n
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const dataPayload = "hello-ripley-data-asset\n"

// buildCatalog lays out a synthetic .xcassets covering every asset kind this
// compiler supports and returns its path.
func buildCatalog(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Assets.xcassets")
	writeFile(t, filepath.Join(root, "Contents.json"), `{"info":{"author":"xcode","version":1}}`)

	// Logo.imageset: 1x/2x/3x colour renditions.
	logo := filepath.Join(root, "Logo.imageset")
	writePNG(t, filepath.Join(logo, "logo.png"), 10, 10, false)
	writePNG(t, filepath.Join(logo, "logo2.png"), 20, 20, false)
	writePNG(t, filepath.Join(logo, "logo3.png"), 30, 30, false)
	writeFile(t, filepath.Join(logo, "Contents.json"), `{"images":[
	{"filename":"logo.png","idiom":"universal","scale":"1x"},
	{"filename":"logo2.png","idiom":"universal","scale":"2x"},
	{"filename":"logo3.png","idiom":"universal","scale":"3x"}],
	"info":{"author":"xcode","version":1}}`)

	// Gray.imageset: a grayscale source, which selects the GA8 encoding.
	gray := filepath.Join(root, "Gray.imageset")
	writePNG(t, filepath.Join(gray, "g.png"), 8, 8, true)
	writeFile(t, filepath.Join(gray, "Contents.json"), `{"images":[
	{"filename":"g.png","idiom":"universal","scale":"1x"}],
	"info":{"author":"xcode","version":1}}`)

	// Brand.colorset: an Any and a Dark variant.
	brand := filepath.Join(root, "Brand.colorset")
	writeFile(t, filepath.Join(brand, "Contents.json"), `{"colors":[
	{"idiom":"universal","color":{"color-space":"srgb","components":{"red":"0.1","green":"0.2","blue":"0.4","alpha":"1.0"}}},
	{"idiom":"universal","appearances":[{"appearance":"luminosity","value":"dark"}],
	 "color":{"color-space":"srgb","components":{"red":"0.7","green":"0.8","blue":"0.9","alpha":"1.0"}}}],
	"info":{"author":"xcode","version":1}}`)

	// Payload.dataset.
	payload := filepath.Join(root, "Payload.dataset")
	writeFile(t, filepath.Join(payload, "payload.bin"), dataPayload)
	writeFile(t, filepath.Join(payload, "Contents.json"), `{"data":[
	{"filename":"payload.bin","idiom":"universal","universal-type-identifier":"public.data"}],
	"info":{"author":"xcode","version":1}}`)

	// AppIcon.appiconset: iphone, ipad and marketing renditions.
	icon := filepath.Join(root, "AppIcon.appiconset")
	for _, e := range []struct {
		file string
		px   int
	}{
		{"i20-2.png", 40}, {"i20-3.png", 60},
		{"i60-2.png", 120}, {"i60-3.png", 180},
		{"p20-1.png", 20}, {"p76-2.png", 152}, {"p83-2.png", 167},
		{"m1024.png", 1024},
	} {
		writePNG(t, filepath.Join(icon, e.file), e.px, e.px, false)
	}
	writeFile(t, filepath.Join(icon, "Contents.json"), `{"images":[
	{"filename":"i20-2.png","idiom":"iphone","scale":"2x","size":"20x20"},
	{"filename":"i20-3.png","idiom":"iphone","scale":"3x","size":"20x20"},
	{"filename":"i60-2.png","idiom":"iphone","scale":"2x","size":"60x60"},
	{"filename":"i60-3.png","idiom":"iphone","scale":"3x","size":"60x60"},
	{"filename":"p20-1.png","idiom":"ipad","scale":"1x","size":"20x20"},
	{"filename":"p76-2.png","idiom":"ipad","scale":"2x","size":"76x76"},
	{"filename":"p83-2.png","idiom":"ipad","scale":"2x","size":"83.5x83.5"},
	{"filename":"m1024.png","idiom":"ios-marketing","scale":"1x","size":"1024x1024"}],
	"info":{"author":"xcode","version":1}}`)

	return root
}

func TestParseCatalog(t *testing.T) {
	root := buildCatalog(t)
	cat, err := Parse([]string{root}, Options{DeploymentTarget: "13.0"})
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string][]Image{}
	for _, img := range cat.Images {
		byName[img.Name] = append(byName[img.Name], img)
	}

	logos := byName["Logo"]
	if len(logos) != 3 {
		t.Fatalf("Logo: got %d renditions, want 3", len(logos))
	}
	wantDims := map[Scale][2]int{1: {10, 10}, 2: {20, 20}, 3: {30, 30}}
	for _, img := range logos {
		want, ok := wantDims[img.Scale]
		if !ok {
			t.Fatalf("Logo: unexpected scale %d", img.Scale)
		}
		if img.PixelWidth != want[0] || img.PixelHeight != want[1] {
			t.Errorf("Logo@%dx: got %dx%d, want %dx%d",
				img.Scale, img.PixelWidth, img.PixelHeight, want[0], want[1])
		}
		if img.Idiom != IdiomUniversal {
			t.Errorf("Logo@%dx: idiom %q, want universal", img.Scale, img.Idiom)
		}
		if img.Encoding != EncodingARGB {
			t.Errorf("Logo@%dx: encoding %v, want ARGB", img.Scale, img.Encoding)
		}
	}

	grays := byName["Gray"]
	if len(grays) != 1 {
		t.Fatalf("Gray: got %d renditions, want 1", len(grays))
	}
	if grays[0].Encoding != EncodingGA8 {
		t.Errorf("Gray: encoding %v, want GA8", grays[0].Encoding)
	}

	if cat.AppIconName != "AppIcon" {
		t.Errorf("AppIconName = %q, want AppIcon", cat.AppIconName)
	}
	if len(cat.AppIcons) != 8 {
		t.Fatalf("AppIcons: got %d, want 8", len(cat.AppIcons))
	}
	// The 83.5pt iPad icon must keep its fractional point size.
	found := false
	for _, img := range cat.AppIcons {
		if img.Size == "83.5x83.5" {
			found = true
			if img.SizePoints != 83.5 {
				t.Errorf("83.5 icon: SizePoints = %v, want 83.5", img.SizePoints)
			}
			if img.PixelWidth != 167 {
				t.Errorf("83.5 icon: PixelWidth = %d, want 167", img.PixelWidth)
			}
		}
	}
	if !found {
		t.Error("83.5x83.5 iPad icon missing from AppIcons")
	}

	if len(cat.Colors) != 2 {
		t.Fatalf("Colors: got %d, want 2", len(cat.Colors))
	}
	var anyC, darkC *Color
	for i := range cat.Colors {
		switch cat.Colors[i].Appearance {
		case AppearanceAny:
			anyC = &cat.Colors[i]
		case AppearanceDark:
			darkC = &cat.Colors[i]
		}
	}
	if anyC == nil || darkC == nil {
		t.Fatal("Brand: want one Any and one Dark variant")
	}
	if !nearly(anyC.R, 0.1) || !nearly(anyC.G, 0.2) || !nearly(anyC.B, 0.4) || !nearly(anyC.A, 1) {
		t.Errorf("Brand any: got %v,%v,%v,%v", anyC.R, anyC.G, anyC.B, anyC.A)
	}
	if !nearly(darkC.R, 0.7) || !nearly(darkC.G, 0.8) || !nearly(darkC.B, 0.9) {
		t.Errorf("Brand dark: got %v,%v,%v", darkC.R, darkC.G, darkC.B)
	}

	if len(cat.Data) != 1 {
		t.Fatalf("Data: got %d, want 1", len(cat.Data))
	}
	if cat.Data[0].UTI != "public.data" {
		t.Errorf("Data UTI = %q, want public.data", cat.Data[0].UTI)
	}
}

func nearly(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// TestCompileRoundTrip is the primary oracle: it compiles a catalog and reads
// the archive back through the reader, checking every payload class.
func TestCompileRoundTrip(t *testing.T) {
	root := buildCatalog(t)
	out := filepath.Join(t.TempDir(), "out")
	res, err := Compile([]string{root}, out, Options{DeploymentTarget: "13.0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CarPath == "" {
		t.Fatal("CarPath is empty")
	}

	raw, err := os.ReadFile(res.CarPath)
	if err != nil {
		t.Fatal(err)
	}
	arc, err := ReadArchive(raw)
	if err != nil {
		t.Fatalf("ReadArchive: %v", err)
	}

	if arc.Platform != "ios" {
		t.Errorf("Platform = %q, want ios", arc.Platform)
	}
	if arc.PlatformVersion != "13.0" {
		t.Errorf("PlatformVersion = %q, want 13.0", arc.PlatformVersion)
	}
	if len(arc.KeyFormat) != len(keyFormat) {
		t.Errorf("KeyFormat has %d tokens, want %d", len(arc.KeyFormat), len(keyFormat))
	}
	if _, ok := arc.Appearances["UIAppearanceDark"]; !ok {
		t.Error("APPEARANCEKEYS is missing UIAppearanceDark")
	}

	for _, name := range []string{"Logo", "Gray", "Brand", "Payload", "AppIcon"} {
		if _, ok := arc.Facets[name]; !ok {
			t.Errorf("facet %q missing (have %v)", name, facetNames(arc))
		}
	}
	if got := arc.Facets["Brand"].Part; got != partColor {
		t.Errorf("Brand facet part = %d, want %d", got, partColor)
	}
	if got := arc.Facets["AppIcon"].Part; got != partMultiSizeImage {
		t.Errorf("AppIcon facet part = %d, want %d", got, partMultiSizeImage)
	}

	// Colours: both variants present with exact components.
	colors := map[Appearance][]float64{}
	for _, r := range arc.Renditions {
		if r.Identifier == arc.Facets["Brand"].Identifier && r.Color != nil {
			colors[r.Appearance] = r.Color
		}
	}
	if got := colors[AppearanceAny]; len(got) != 4 ||
		!nearly(got[0], 0.1) || !nearly(got[1], 0.2) || !nearly(got[2], 0.4) || !nearly(got[3], 1) {
		t.Errorf("Brand any components = %v", got)
	}
	if got := colors[AppearanceDark]; len(got) != 4 ||
		!nearly(got[0], 0.7) || !nearly(got[1], 0.8) || !nearly(got[2], 0.9) {
		t.Errorf("Brand dark components = %v", got)
	}

	// Data asset: exact payload bytes and UTI.
	var dataRend *ArchiveRendition
	for i := range arc.Renditions {
		if arc.Renditions[i].Identifier == arc.Facets["Payload"].Identifier {
			dataRend = &arc.Renditions[i]
		}
	}
	if dataRend == nil {
		t.Fatal("Payload rendition missing")
	}
	if string(dataRend.Data) != dataPayload {
		t.Errorf("Payload data = %q, want %q", dataRend.Data, dataPayload)
	}
	if dataRend.UTI != "public.data" {
		t.Errorf("Payload UTI = %q, want public.data", dataRend.UTI)
	}

	// Image renditions: dimensions, scale and decoded pixel buffers.
	logoID := arc.Facets["Logo"].Identifier
	byScale := map[Scale]*ArchiveRendition{}
	for i := range arc.Renditions {
		if arc.Renditions[i].Identifier == logoID {
			byScale[arc.Renditions[i].Scale] = &arc.Renditions[i]
		}
	}
	for scale, wantPx := range map[Scale]int{1: 10, 2: 20, 3: 30} {
		r := byScale[scale]
		if r == nil {
			t.Errorf("Logo@%dx missing", scale)
			continue
		}
		if r.Width != wantPx || r.Height != wantPx {
			t.Errorf("Logo@%dx: %dx%d, want %dx%d", scale, r.Width, r.Height, wantPx, wantPx)
		}
		if r.Encoding != "ARGB" {
			t.Errorf("Logo@%dx: encoding %q, want ARGB", scale, r.Encoding)
		}
		// The payload is a row-padded BGRA buffer; check its size and the
		// first pixel against the generator.
		bpr := bytesPerRow(wantPx, EncodingARGB)
		if len(r.Pixels) != bpr*wantPx {
			t.Errorf("Logo@%dx: %d payload bytes, want %d", scale, len(r.Pixels), bpr*wantPx)
			continue
		}
		// Pixel (1,0) of the generator: R=3, G=0, B=1, A=255 -> BGRA.
		got := r.Pixels[4:8]
		want := []byte{1, 0, 3, 255}
		if !bytes.Equal(got, want) {
			t.Errorf("Logo@%dx: pixel(1,0) = %v, want %v", scale, got, want)
		}
	}

	grayRend := findByIdent(arc, arc.Facets["Gray"].Identifier)
	if grayRend == nil {
		t.Fatal("Gray rendition missing")
	}
	if grayRend.Encoding != "GA8 " {
		t.Errorf("Gray encoding = %q, want %q", grayRend.Encoding, "GA8 ")
	}
	if want := bytesPerRow(8, EncodingGA8) * 8; len(grayRend.Pixels) != want {
		t.Errorf("Gray payload = %d bytes, want %d", len(grayRend.Pixels), want)
	}

	// App icon: one icon-image rendition per source plus the per-idiom
	// multi-size renditions. A rendition is addressed by
	// (idiom, subtype, scale, Icon Index); slot numbers are shared across
	// scales, exactly as `actool` numbers them, so scale is part of the key.
	iconID := arc.Facets["AppIcon"].Identifier
	iconImages := 0
	multiSize := 0
	type slot struct {
		idiom   Idiom
		subtype int
		scale   Scale
		index   int
	}
	slots := map[slot]bool{}
	for _, r := range arc.Renditions {
		if r.Identifier != iconID {
			continue
		}
		switch r.Part {
		case partIconImage:
			iconImages++
			s := slot{r.Idiom, r.Subtype, r.Scale, r.SizeIndex}
			if r.SizeIndex == 0 {
				t.Errorf("icon %q has no size index", r.Name)
			}
			if slots[s] {
				t.Errorf("duplicate icon slot %+v (rendition %q)", s, r.Name)
			}
			slots[s] = true
		case partMultiSizeImage:
			multiSize++
			if len(r.MultiSizes) == 0 {
				t.Errorf("multi-size rendition for %v has no entries", r.Idiom)
			}
		}
	}
	if iconImages != 8 {
		t.Errorf("icon image renditions = %d, want 8", iconImages)
	}
	if multiSize != 3 {
		t.Errorf("multi-size renditions = %d, want 3 (iphone, ipad, marketing)", multiSize)
	}

	// Loose files and Info.plist keys.
	names := map[string]bool{}
	for _, p := range res.LooseFiles {
		names[filepath.Base(p)] = true
		if _, err := os.Stat(p); err != nil {
			t.Errorf("loose file %s: %v", p, err)
		}
	}
	for _, want := range []string{
		"Logo.png", "Logo@2x.png", "Logo@3x.png",
		"AppIcon60x60@2x.png", "AppIcon76x76@2x~ipad.png",
	} {
		if !names[want] {
			t.Errorf("loose file %q missing", want)
		}
	}
}

func findByIdent(a *Archive, id uint16) *ArchiveRendition {
	for i := range a.Renditions {
		if a.Renditions[i].Identifier == id {
			return &a.Renditions[i]
		}
	}
	return nil
}

func facetNames(a *Archive) []string {
	out := make([]string, 0, len(a.Facets))
	for n := range a.Facets {
		out = append(out, n)
	}
	return out
}

func TestIconInfoPlistKeys(t *testing.T) {
	root := buildCatalog(t)
	out := filepath.Join(t.TempDir(), "out")
	res, err := Compile([]string{root}, out, Options{DeploymentTarget: "13.0"})
	if err != nil {
		t.Fatal(err)
	}

	if got := res.InfoPlistKeys["CFBundleIconName"]; got != "AppIcon" {
		t.Errorf("CFBundleIconName = %v, want AppIcon", got)
	}

	phone := primaryIconFiles(t, res.InfoPlistKeys, "CFBundleIcons")
	pad := primaryIconFiles(t, res.InfoPlistKeys, "CFBundleIcons~ipad")

	// The marketing (1024) icon is an App Store asset and must never appear.
	for _, list := range [][]string{phone, pad} {
		for _, n := range list {
			if n == "AppIcon1024x1024" {
				t.Errorf("marketing icon leaked into CFBundleIconFiles: %v", list)
			}
		}
	}
	if !contains(phone, "AppIcon60x60") {
		t.Errorf("CFBundleIcons files = %v, want AppIcon60x60", phone)
	}
	if !contains(pad, "AppIcon76x76") {
		t.Errorf("CFBundleIcons~ipad files = %v, want AppIcon76x76", pad)
	}
	// iPad resolves CFBundleIcons~ipad exclusively, so it must also carry the
	// phone entries.
	if !contains(pad, "AppIcon60x60") {
		t.Errorf("CFBundleIcons~ipad = %v, want it to include the iPhone icons", pad)
	}
	if _, ok := res.InfoPlistKeys["CFBundleIconFiles"].([]string); !ok {
		t.Errorf("CFBundleIconFiles = %#v, want []string", res.InfoPlistKeys["CFBundleIconFiles"])
	}
}

func primaryIconFiles(t *testing.T, keys map[string]any, key string) []string {
	t.Helper()
	top, ok := keys[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want a dictionary", key, keys[key])
	}
	primary, ok := top["CFBundlePrimaryIcon"].(map[string]any)
	if !ok {
		t.Fatalf("%s.CFBundlePrimaryIcon = %#v, want a dictionary", key, top["CFBundlePrimaryIcon"])
	}
	if got := primary["CFBundleIconName"]; got != "AppIcon" {
		t.Errorf("%s.CFBundleIconName = %v, want AppIcon", key, got)
	}
	files, ok := primary["CFBundleIconFiles"].([]string)
	if !ok {
		t.Fatalf("%s.CFBundleIconFiles = %#v, want []string", key, primary["CFBundleIconFiles"])
	}
	return files
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestUnsupportedOnly checks that a catalog holding nothing compilable is not
// an error: Compile reports it through Skipped and writes no archive.
func TestUnsupportedOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Assets.xcassets")
	writeFile(t, filepath.Join(root, "Contents.json"), `{"info":{"author":"xcode","version":1}}`)

	// A watchOS-only image set and an unsupported asset kind.
	watch := filepath.Join(root, "Watch.imageset")
	writePNG(t, filepath.Join(watch, "w.png"), 4, 4, false)
	writeFile(t, filepath.Join(watch, "Contents.json"), `{"images":[
	{"filename":"w.png","idiom":"watch","scale":"2x"}],
	"info":{"author":"xcode","version":1}}`)

	mip := filepath.Join(root, "Thing.mipmapset")
	writeFile(t, filepath.Join(mip, "Contents.json"), `{"info":{"author":"xcode","version":1}}`)

	out := filepath.Join(t.TempDir(), "out")
	res, err := Compile([]string{root}, out, Options{DeploymentTarget: "13.0"})
	if err != nil {
		t.Fatalf("Compile returned an error for an unsupported-only catalog: %v", err)
	}
	if res.CarPath != "" {
		t.Errorf("CarPath = %q, want empty", res.CarPath)
	}
	if len(res.LooseFiles) != 0 {
		t.Errorf("LooseFiles = %v, want none", res.LooseFiles)
	}
	if len(res.Skipped) < 2 {
		t.Fatalf("Skipped = %+v, want the watch idiom and the mipmapset", res.Skipped)
	}
	var sawIdiom, sawKind bool
	for _, s := range res.Skipped {
		if s.Kind == "mipmapset" {
			sawKind = true
		}
		if s.Reason != "" && s.Item != "" && s.Kind == ".imageset" {
			sawIdiom = true
		}
	}
	if !sawIdiom {
		t.Errorf("no skip recorded for the watch idiom: %+v", res.Skipped)
	}
	if !sawKind {
		t.Errorf("no skip recorded for the mipmapset: %+v", res.Skipped)
	}
}

// TestBOMTreeRoundTrip pins the container invariant that broke CoreUI before:
// a tree leaf must be padded to the node block size the header declares, and
// every block must lie inside the file.
func TestBOMTreeRoundTrip(t *testing.T) {
	b := newBOMBuilder()
	entries := make([]bomKV, 0, 200)
	for i := 0; i < 200; i++ {
		key := binary.LittleEndian.AppendUint32(nil, uint32(i))
		entries = append(entries, bomKV{key: key, value: []byte{byte(i), 0xaa}})
	}
	b.addTree("T", 4, entries)
	data := b.build()

	r, err := openBOM(data)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.variable("T")
	if err != nil {
		t.Fatal(err)
	}
	blockSize := int(binary.BigEndian.Uint32(head[12:16]))
	leaf, err := r.block(binary.BigEndian.Uint32(head[8:12]))
	if err != nil {
		t.Fatal(err)
	}
	// CoreUI maps a node by asking the BOM stream for blockSize bytes and then
	// reads the inline key array that follows the pair array, so the leaf must
	// be at least a whole node block plus that array. `actool` leaves are sized
	// exactly blockSize + keySize*count.
	if want := blockSize + 4*len(entries); len(leaf) != want {
		t.Errorf("leaf is %d bytes, want %d (%d-byte node block + %d 4-byte keys)",
			len(leaf), want, blockSize, len(entries))
	}
	if len(leaf) < bomTreeEntryHeader+bomTreeEntrySize*len(entries) {
		t.Errorf("leaf of %d bytes cannot hold %d entries", len(leaf), len(entries))
	}

	// The inline key array is what CoreUI actually reads; an all-zero array is
	// the silent-corruption mode that made every rendition decode identically.
	arrayOffset := (bomTreeEntryHeader + bomTreeEntrySize*len(entries) + 7) & ^7
	for i := range entries {
		inline := leaf[arrayOffset+4*i : arrayOffset+4*i+4]
		if got := binary.LittleEndian.Uint32(inline); got != uint32(i) {
			t.Fatalf("inline key %d = %d, want %d", i, got, i)
		}
	}

	got, err := r.tree("T")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(entries) {
		t.Fatalf("read %d entries, wrote %d", len(got), len(entries))
	}
	for i, kv := range got {
		if binary.LittleEndian.Uint32(kv.key) != uint32(i) {
			t.Fatalf("entry %d has key %v; tree is not sorted by key", i, kv.key)
		}
		if kv.value[0] != byte(i) || kv.value[1] != 0xaa {
			t.Fatalf("entry %d value = %v", i, kv.value)
		}
	}
}

// TestBlocksInsideFile is the direct regression test for the
// "BOMStreamGetDataPointer buffer overflow" failure: every index entry must
// describe a range that lies wholly inside the archive.
func TestBlocksInsideFile(t *testing.T) {
	root := buildCatalog(t)
	out := filepath.Join(t.TempDir(), "out")
	res, err := Compile([]string{root}, out, Options{DeploymentTarget: "13.0"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(res.CarPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openBOM(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range r.addresses {
		start, length := int(r.addresses[i]), int(r.lengths[i])
		if start+length > len(raw) {
			t.Errorf("block %d spans [%d,%d) but the file is %d bytes",
				i, start, start+length, len(raw))
		}
	}
	// The header's block count must match the index it points at.
	if got := int(binary.BigEndian.Uint32(raw[12:16])); got != len(r.addresses) {
		t.Errorf("header declares %d blocks, index holds %d", got, len(r.addresses))
	}
}
