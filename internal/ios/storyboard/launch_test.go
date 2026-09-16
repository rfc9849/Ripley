package storyboard

import (
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, name string) *LaunchScreen {
	t.Helper()
	ls, err := ParseLaunchScreen(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("ParseLaunchScreen(%s): %v", name, err)
	}
	return ls
}

// launchDict returns the UILaunchScreen dictionary, failing when absent: every
// lowering must produce one so UIKit leaves the legacy letterboxed launch mode.
func launchDict(t *testing.T, ls *LaunchScreen) map[string]any {
	t.Helper()
	d, ok := ls.InfoPlistKeys["UILaunchScreen"].(map[string]any)
	if !ok {
		t.Fatalf("UILaunchScreen missing or wrong type: %#v", ls.InfoPlistKeys["UILaunchScreen"])
	}
	return d
}

func requireUnsupported(t *testing.T, ls *LaunchScreen, substr string) {
	t.Helper()
	for _, r := range ls.Unsupported {
		if strings.Contains(r, substr) {
			return
		}
	}
	t.Fatalf("no Unsupported entry containing %q; got %#v", substr, ls.Unsupported)
}

func requireNoUnsupported(t *testing.T, ls *LaunchScreen, substr string) {
	t.Helper()
	for _, r := range ls.Unsupported {
		if strings.Contains(r, substr) {
			t.Fatalf("unexpected Unsupported entry %q", r)
		}
	}
}

func findColor(t *testing.T, ls *LaunchScreen, name string) RequiredColor {
	t.Helper()
	for _, c := range ls.RequiredColors {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("RequiredColors has no %q; got %#v", name, ls.RequiredColors)
	return RequiredColor{}
}

// The stock Flutter template is the case that must be perfect: an opaque white
// background plus a centered LaunchImage. contentMode="center" is exactly what
// UILaunchScreen draws, so nothing may be reported unsupported.
func TestFlutterDefaultTemplateLowersExactly(t *testing.T) {
	ls := parse(t, "FlutterDefaultLaunchScreen.storyboard")
	d := launchDict(t, ls)

	if got := d["UIImageName"]; got != "LaunchImage" {
		t.Errorf("UIImageName = %v, want LaunchImage", got)
	}
	colorName, ok := d["UIColorName"].(string)
	if !ok || colorName == "" {
		t.Fatalf("UIColorName = %#v, want a generated colorset name", d["UIColorName"])
	}
	// The white background is a literal sRGB color, so it must come back as a
	// colorset request the asset catalog can synthesize.
	c := findColor(t, ls, colorName)
	if c.R != 1 || c.G != 1 || c.B != 1 || c.A != 1 {
		t.Errorf("background = %+v, want opaque white", c)
	}
	if c.Dynamic {
		t.Errorf("literal white must not be dynamic: %+v", c)
	}
	// A literal storyboard color has no colorset of its own, so ripley must own it.
	if !c.Generated {
		t.Errorf("literal color %q not marked Generated; nothing would create the colorset UIColorName names", c.Name)
	}

	if got := ls.RequiredImages; len(got) != 1 || got[0] != "LaunchImage" {
		t.Errorf("RequiredImages = %v, want [LaunchImage]", got)
	}
	// A centered image at natural size is exactly UILaunchScreen's own
	// rendering, and UIImageRespectsSafeAreaInsets must stay absent (default
	// false = full screen).
	if _, exists := d["UIImageRespectsSafeAreaInsets"]; exists {
		t.Errorf("UIImageRespectsSafeAreaInsets set for a centered image: %#v", d)
	}
	if len(ls.Unsupported) != 0 {
		t.Errorf("stock Flutter template must lower losslessly, got %#v", ls.Unsupported)
	}
	// No idiom-specific content, so no UILaunchScreens.
	if _, exists := ls.InfoPlistKeys["UILaunchScreens"]; exists {
		t.Errorf("UILaunchScreens emitted for a single-idiom document: %#v", ls.InfoPlistKeys)
	}
	// Bars must only appear when the storyboard has them.
	for _, k := range []string{"UINavigationBar", "UITabBar", "UIToolbar"} {
		if _, exists := d[k]; exists {
			t.Errorf("%s emitted though the storyboard has no such bar", k)
		}
	}
}

// harmonoid: a named catalog color whose components the document also carries as
// a <namedColor>, plus scaleAspectFit with an explicit size. The color name must
// be reused verbatim (not regenerated) and the scaling must be reported.
func TestNamedCatalogColorReusesNameAndReportsScaling(t *testing.T) {
	ls := parse(t, "NamedColorCenteredImage.storyboard")
	d := launchDict(t, ls)

	if got := d["UIColorName"]; got != "LaunchBackground" {
		t.Errorf("UIColorName = %v, want the catalog color name LaunchBackground", got)
	}
	c := findColor(t, ls, "LaunchBackground")
	// The document mirrors the catalog's colorset in <namedColor>, so the
	// components are known and usable as a fallback.
	if !approx(c.R, 0.3843137254901961) || !approx(c.G, 0) || !approx(c.B, 0.9176470588235294) || c.A != 1 {
		t.Errorf("LaunchBackground = %+v, want the document's namedColor components", c)
	}
	// But the colorset belongs to the project, so ripley must NOT claim to generate
	// it. harmonoid's real LaunchBackground.colorset carries a dark appearance
	// (0.102 gray) that the storyboard mirror omits; overwriting it from the
	// storyboard would silently break dark mode.
	if c.Generated {
		t.Errorf("catalog-owned color %q marked Generated; overwriting it would drop the project's dark appearance", c.Name)
	}
	if got := d["UIImageName"]; got != "LaunchImage" {
		t.Errorf("UIImageName = %v", got)
	}
	requireUnsupported(t, ls, "contentMode \"scaleAspectFit\"")
}

// spotube: three stacked image views. UILaunchScreen renders one image, so the
// centered logo must win and the other two must be reported by name.
func TestMultipleImageViewsKeepCenteredAndReportRest(t *testing.T) {
	ls := parse(t, "ThreeImageBranding.storyboard")
	d := launchDict(t, ls)

	if got := d["UIImageName"]; got != "LaunchImage" {
		t.Errorf("UIImageName = %v, want the centered LaunchImage", got)
	}
	if got := ls.RequiredImages; len(got) != 1 || got[0] != "LaunchImage" {
		t.Errorf("RequiredImages = %v, want only the rendered image", got)
	}
	requireUnsupported(t, ls, `image "LaunchBackground"`)
	requireUnsupported(t, ls, `image "BrandingImage"`)
}

// A UILabel has no UILaunchScreen equivalent; the text must be named so the user
// knows what will be missing, and the supported keys must still be emitted.
func TestLabelReportedAndBackgroundStillLowered(t *testing.T) {
	ls := parse(t, "SystemColorWithLabel.storyboard")
	d := launchDict(t, ls)

	requireUnsupported(t, ls, "Loading your library")
	if got := d["UIColorName"]; got == nil || got == "" {
		t.Errorf("background dropped because of an unsupported label: %#v", d)
	}
	if got := d["UIImageName"]; got != "Wordmark" {
		t.Errorf("UIImageName = %v, want Wordmark", got)
	}
	// The storyboard has a navigation bar, so the key must be present; it has no
	// tab bar, so that key must not be.
	if _, ok := d["UINavigationBar"]; !ok {
		t.Errorf("UINavigationBar missing though the storyboard has one: %#v", d)
	}
	if _, ok := d["UITabBar"]; ok {
		t.Errorf("UITabBar emitted though the storyboard has none: %#v", d)
	}
}

// A system color must resolve to Apple's real components on Linux and carry its
// dark-appearance variant, so the generated colorset matches UIKit.
func TestSystemBackgroundColorResolvesBothAppearances(t *testing.T) {
	ls := parse(t, "SystemColorWithLabel.storyboard")
	d := launchDict(t, ls)

	name, _ := d["UIColorName"].(string)
	c := findColor(t, ls, name)
	if !c.Dynamic {
		t.Fatalf("systemBackgroundColor must be dynamic: %+v", c)
	}
	if c.R != 1 || c.G != 1 || c.B != 1 || c.A != 1 {
		t.Errorf("light appearance = %+v, want opaque white", c)
	}
	if c.DarkR != 0 || c.DarkG != 0 || c.DarkB != 0 || c.DarkA != 1 {
		t.Errorf("dark appearance = %+v, want opaque black", c)
	}
}

// An image pinned to the safe area layout guide is the only case where
// UIImageRespectsSafeAreaInsets must be set; pinning to the root view is not.
func TestSafeAreaPinnedImageSetsRespectsInsets(t *testing.T) {
	ls := parse(t, "SafeAreaFullscreenImage.storyboard")
	d := launchDict(t, ls)

	if got := d["UIImageRespectsSafeAreaInsets"]; got != true {
		t.Errorf("UIImageRespectsSafeAreaInsets = %#v, want true for a safe-area-pinned image", got)
	}
	if got := d["UIImageName"]; got != "Cover" {
		t.Errorf("UIImageName = %v, want Cover", got)
	}

	// Contrast: the Flutter template pins nothing to the safe area.
	base := parse(t, "FlutterDefaultLaunchScreen.storyboard")
	if _, exists := launchDict(t, base)["UIImageRespectsSafeAreaInsets"]; exists {
		t.Errorf("root-view-pinned image must not set UIImageRespectsSafeAreaInsets")
	}
}

// Idiom-specific art must become UILaunchScreens with both definitions, and the
// iPad image must be required too.
func TestIdiomVariantsProduceLaunchScreensArray(t *testing.T) {
	ls := parse(t, "IdiomVariantLaunchScreen.storyboard")

	screens, ok := ls.InfoPlistKeys["UILaunchScreens"].(map[string]any)
	if !ok {
		t.Fatalf("UILaunchScreens = %#v, want a dictionary", ls.InfoPlistKeys["UILaunchScreens"])
	}
	defs, ok := screens["UILaunchScreenDefinitions"].([]any)
	if !ok || len(defs) != 2 {
		t.Fatalf("UILaunchScreenDefinitions = %#v, want two definitions", screens["UILaunchScreenDefinitions"])
	}
	def := screens["UIDefaultLaunchScreen"]
	if def == nil || def == "" {
		t.Errorf("UIDefaultLaunchScreen missing: %#v", screens)
	}
	// Every definition needs the identifying key, else the plist is invalid.
	var ids []string
	for _, raw := range defs {
		d, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("definition %#v is not a dictionary", raw)
		}
		id, ok := d["UILaunchScreenIdentifier"].(string)
		if !ok || id == "" {
			t.Fatalf("definition missing UILaunchScreenIdentifier: %#v", d)
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Errorf("definition identifiers collide: %v", ids)
	}
	if !contains(ids, def.(string)) {
		t.Errorf("UIDefaultLaunchScreen %q names no definition (%v)", def, ids)
	}

	ipad, ok := ls.InfoPlistKeys["UILaunchScreen~ipad"].(map[string]any)
	if !ok {
		t.Fatalf("UILaunchScreen~ipad = %#v", ls.InfoPlistKeys["UILaunchScreen~ipad"])
	}
	if got := ipad["UIImageName"]; got != "LaunchImagePad" {
		t.Errorf("iPad UIImageName = %v, want LaunchImagePad", got)
	}
	if got := launchDict(t, ls)["UIImageName"]; got != "LaunchImage" {
		t.Errorf("phone UIImageName = %v, want LaunchImage", got)
	}
}

// A macOS AppKit document has no iOS launch semantics; mis-lowering one would
// silently produce a wrong launch screen, so it must be refused.
func TestAppKitDocumentRejected(t *testing.T) {
	if _, err := ParseLaunchScreen(filepath.Join("testdata", "MacMainMenu.xib")); err == nil {
		t.Fatal("AppKit .xib accepted as an iOS launch screen")
	}
}

// The task's "customised variant": a colored background plus a centered logo.
// Both are expressible, so this must lower with nothing reported unsupported.
func TestCustomisedColoredBackgroundAndCenteredImage(t *testing.T) {
	ls := parse(t, "CustomizedLaunchScreen.storyboard")
	d := launchDict(t, ls)

	if got := d["UIImageName"]; got != "SplashLogo" {
		t.Errorf("UIImageName = %v, want SplashLogo", got)
	}
	name, ok := d["UIColorName"].(string)
	if !ok || name == "" {
		t.Fatalf("UIColorName = %#v", d["UIColorName"])
	}
	c := findColor(t, ls, name)
	if !approx(c.R, 0.125490196) || !approx(c.G, 0.243137255) || !approx(c.B, 0.4) || c.A != 1 {
		t.Errorf("background = %+v, want the document's dark blue", c)
	}
	// A distinct literal color must get a distinct generated colorset name, so
	// two different launch screens cannot collide on one colorset.
	white := parse(t, "FlutterDefaultLaunchScreen.storyboard")
	whiteName, _ := launchDict(t, white)["UIColorName"].(string)
	if name == whiteName {
		t.Errorf("distinct colors collided on colorset name %q", name)
	}
	if len(ls.Unsupported) != 0 {
		t.Errorf("colored background + centered image must lower losslessly, got %#v", ls.Unsupported)
	}
}

// A storyboard with several scenes cannot be represented; the extra scenes must
// be named rather than silently ignored, and the initial one must still lower.
func TestExtraScenesReported(t *testing.T) {
	ls := parse(t, "SystemColorWithLabel.storyboard")
	d := launchDict(t, ls)
	if got := d["UIImageName"]; got != "Wordmark" {
		t.Errorf("UIImageName = %v, want Wordmark from the initial scene", got)
	}
	requireUnsupported(t, ls, "not the initial view controller")
}

// zulip-flutter: Main.storyboard with a FlutterViewController and nothing else.
// The programmatic SceneDelegate substitution reproduces it exactly, and that
// must be reported as data, not as an error.
func TestMainInterfaceReportsProgrammaticEquivalence(t *testing.T) {
	mi, err := AnalyzeMainInterface(filepath.Join("testdata", "FlutterMainInterface.storyboard"))
	if err != nil {
		t.Fatalf("AnalyzeMainInterface: %v", err)
	}
	if mi.Lowerable {
		t.Error("a main interface storyboard instantiates a class; it is never lowerable")
	}
	if len(mi.Reasons) == 0 {
		t.Error("Reasons must explain why the storyboard cannot be lowered")
	}
	if mi.RootControllerClass != "FlutterViewController" {
		t.Errorf("RootControllerClass = %q, want FlutterViewController", mi.RootControllerClass)
	}
	if !mi.EquivalentToProgrammaticFlutterRoot {
		t.Errorf("stock Flutter Main.storyboard must be equivalent to the programmatic root; reasons: %v", mi.Reasons)
	}
	if mi.Name != "FlutterMainInterface" {
		t.Errorf("Name = %q", mi.Name)
	}
	// calibratedWhite must resolve, so the substituted window can match the
	// storyboard's first frame.
	if mi.Background == nil {
		t.Fatal("Background not resolved from a calibratedWhite color")
	}
	if mi.Background.R != 1 || mi.Background.G != 1 || mi.Background.B != 1 || mi.Background.A != 1 {
		t.Errorf("Background = %+v, want opaque white", *mi.Background)
	}
}

// A main interface carrying real view content is NOT equivalent to the
// programmatic root, and the difference must be visible to the caller.
func TestMainInterfaceWithContentNotEquivalent(t *testing.T) {
	mi, err := AnalyzeMainInterface(filepath.Join("testdata", "CustomizedLaunchScreen.storyboard"))
	if err != nil {
		t.Fatalf("AnalyzeMainInterface: %v", err)
	}
	if mi.EquivalentToProgrammaticFlutterRoot {
		t.Error("a storyboard with subviews must not be reported as equivalent to a bare programmatic root")
	}
}

// LaunchStoryboardPath must find Base.lproj documents and must return "" for a
// project with no launch storyboard (the zulip-flutter case) rather than
// guessing.
func TestLaunchStoryboardPathLookup(t *testing.T) {
	got := LaunchStoryboardPath("testdata", "FlutterDefaultLaunchScreen")
	if got != filepath.Join("testdata", "FlutterDefaultLaunchScreen.storyboard") {
		t.Errorf("LaunchStoryboardPath = %q", got)
	}
	if got := LaunchStoryboardPath("testdata", "LaunchScreen"); got != "" {
		t.Errorf("LaunchStoryboardPath for a missing storyboard = %q, want \"\"", got)
	}
	if got := LaunchStoryboardPath("testdata", ""); got != "" {
		t.Errorf("empty name = %q, want \"\"", got)
	}
}

// Two different literal colors must not collide on one generated colorset name,
// and the same literal must reuse one.
func TestGeneratedColorsetNamesDistinguishComponents(t *testing.T) {
	white := Color{R: 1, G: 1, B: 1, A: 1}
	black := Color{R: 0, G: 0, B: 0, A: 1}
	if white.colorsetName() == black.colorsetName() {
		t.Fatalf("distinct colors share a colorset name: %q", white.colorsetName())
	}
	if white.colorsetName() != (Color{R: 1, G: 1, B: 1, A: 1}).colorsetName() {
		t.Error("identical colors must reuse one colorset name")
	}
	// A catalog color keeps its own name so ripley does not duplicate the asset.
	named := Color{CatalogName: "LaunchBackground", R: 0.5, A: 1}
	if named.colorsetName() != "LaunchBackground" {
		t.Errorf("catalog color renamed to %q", named.colorsetName())
	}
}

// An unrecognised color form must be reported, never guessed at.
func TestUnknownColorFormReported(t *testing.T) {
	doc, err := parseDocumentBytes([]byte(`<?xml version="1.0"?>
<document type="com.apple.InterfaceBuilder3.CocoaTouch.Storyboard.XIB" launchScreen="YES" initialViewController="vc">
  <scenes><scene sceneID="s1"><objects>
    <viewController id="vc" sceneMemberID="viewController">
      <view key="view" id="root">
        <color key="backgroundColor" catalog="System" colorSpace="catalog" cocoaTouchSystemColor="notARealColor"/>
      </view>
    </viewController>
  </objects></scene></scenes>
</document>`), "inline")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ls, err := lowerLaunchScreen(doc, "inline")
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	requireUnsupported(t, ls, "unknown system color")
	// The dictionary must still exist so UIKit leaves legacy launch mode.
	if _, ok := ls.InfoPlistKeys["UILaunchScreen"]; !ok {
		t.Error("UILaunchScreen dropped because a color was unreadable")
	}
}

// Every real launch screen in the corpus must yield a UILaunchScreen dictionary
// and never a parse error: the build must not fail because of launch art.
func TestAllTestdataLaunchScreensLower(t *testing.T) {
	files := []string{
		"FlutterDefaultLaunchScreen.storyboard",
		"NamedColorCenteredImage.storyboard",
		"ThreeImageBranding.storyboard",
		"CustomizedLaunchScreen.storyboard",
		"SystemColorWithLabel.storyboard",
		"SafeAreaFullscreenImage.storyboard",
		"IdiomVariantLaunchScreen.storyboard",
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			ls := parse(t, f)
			launchDict(t, ls)
			// Any required color must carry usable components.
			for _, c := range ls.RequiredColors {
				if c.Name == "" {
					t.Errorf("unnamed required color: %+v", c)
				}
				if c.A == 0 && c.R == 0 && c.G == 0 && c.B == 0 {
					t.Logf("fully transparent color %q (legitimate but worth noting)", c.Name)
				}
			}
		})
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
