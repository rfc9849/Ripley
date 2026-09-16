package storyboard

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

func compileLaunch(t *testing.T, name string) (string, *CompiledLaunchStoryboard) {
	t.Helper()
	out := filepath.Join(t.TempDir(), strings.TrimSuffix(name, filepath.Ext(name))+".storyboardc")
	result, err := CompileLaunchStoryboard(filepath.Join("testdata", name), out)
	if err != nil {
		t.Fatalf("CompileLaunchStoryboard(%s): %v", name, err)
	}
	return out, result
}

func readNib(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 50 || string(data[:10]) != "NIBArchive" {
		t.Fatalf("%s is not a NIBArchive", path)
	}
	if format := binary.LittleEndian.Uint32(data[10:14]); format != 1 {
		t.Fatalf("%s format version = %d, want 1", path, format)
	}
	if coder := binary.LittleEndian.Uint32(data[14:18]); coder != 10 {
		t.Fatalf("%s coder version = %d, want 10", path, coder)
	}
	return data
}

func requireNibString(t *testing.T, data []byte, value string) {
	t.Helper()
	if !strings.Contains(string(data), value) {
		t.Fatalf("compiled nib does not contain %q", value)
	}
}

func TestCompileLaunchStoryboardProducesStoryboardc(t *testing.T) {
	out, result := compileLaunch(t, "ThreeImageBranding.storyboard")
	if result.ControllerNib != "UIViewController-01J-lp-oVM" {
		t.Fatalf("ControllerNib = %q", result.ControllerNib)
	}
	if result.ViewNib != "01J-lp-oVM-view-Ze5-6b-2t3" {
		t.Fatalf("ViewNib = %q", result.ViewNib)
	}

	data, err := os.ReadFile(filepath.Join(out, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if _, err := plist.Unmarshal(data, &info); err != nil {
		t.Fatalf("parse compiled Info.plist: %v", err)
	}
	if got := info["UIStoryboardDesignatedEntryPointIdentifier"]; got != result.ControllerNib {
		t.Fatalf("entry point = %#v, want %q", got, result.ControllerNib)
	}
	mapping, ok := info["UIViewControllerIdentifiersToNibNames"].(map[string]any)
	if !ok || mapping[result.ControllerNib] != result.ControllerNib {
		t.Fatalf("controller mapping = %#v", info["UIViewControllerIdentifiersToNibNames"])
	}

	scene := readNib(t, filepath.Join(out, result.ControllerNib+".nib"))
	requireNibString(t, scene, "UIClassSwapper")
	requireNibString(t, scene, result.ViewNib)
	requireNibString(t, scene, "sceneViewController")

	view := readNib(t, filepath.Join(out, result.ViewNib+".nib"))
	for _, name := range []string{"LaunchBackground", "LaunchImage", "BrandingImage"} {
		requireNibString(t, view, name)
	}
	for _, class := range []string{"UIView", "UIImageView", "UIImageNibPlaceholder", "NSLayoutConstraint"} {
		requireNibString(t, view, class)
	}
}

func TestCompileLaunchStoryboardPreservesLabel(t *testing.T) {
	out, result := compileLaunch(t, "SystemColorWithLabel.storyboard")
	view := readNib(t, filepath.Join(out, result.ViewNib+".nib"))
	for _, value := range []string{"UILabel", "Loading your library", "Wordmark", "UINavigationBar", "labelColor"} {
		requireNibString(t, view, value)
	}
}

func TestCompileLaunchStoryboardPreservesSafeArea(t *testing.T) {
	out, result := compileLaunch(t, "SafeAreaFullscreenImage.storyboard")
	view := readNib(t, filepath.Join(out, result.ViewNib+".nib"))
	for _, value := range []string{"UILayoutGuide", "UIViewSafeAreaLayoutGuide", "Cover"} {
		requireNibString(t, view, value)
	}
}

func TestCompileLaunchStoryboardPreservesTraitVariations(t *testing.T) {
	out, result := compileLaunch(t, "IdiomVariantLaunchScreen.storyboard")
	view := readNib(t, filepath.Join(out, result.ViewNib+".nib"))
	for _, value := range []string{
		"_UITraitStorageList",
		"_UIAttributeTraitStorage",
		"_UIAttributeTraitStorageRecord",
		"UITraitCollection",
		"UITraitCollectionBuiltinTrait-_UITraitNameHorizontalSizeClass",
		"UITraitCollectionBuiltinTrait-_UITraitNameVerticalSizeClass",
		"LaunchImagePad",
		"NSSet",
	} {
		requireNibString(t, view, value)
	}
}

func TestCompileLaunchStoryboardPreservesBackgroundColorVariation(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "IdiomVariantLaunchScreen.storyboard"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(source),
		`<color key="backgroundColor" red="1" green="1" blue="1" alpha="1" colorSpace="custom" customColorSpace="sRGB"/>`,
		`<color key="backgroundColor" red="1" green="1" blue="1" alpha="1" colorSpace="custom" customColorSpace="sRGB"/>
                        <variation key="heightClass=regular-widthClass=regular"><color key="backgroundColor" red="0" green="0" blue="0" alpha="1" colorSpace="custom" customColorSpace="sRGB"/></variation>`, 1)
	path := filepath.Join(t.TempDir(), "ColorVariation.storyboard")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "ColorVariation.storyboardc")
	result, err := CompileLaunchStoryboard(path, out)
	if err != nil {
		t.Fatal(err)
	}
	view := readNib(t, filepath.Join(out, result.ViewNib+".nib"))
	for _, value := range []string{"backgroundColor", "_UIAttributeTraitStorage", "UITraitCollection"} {
		requireNibString(t, view, value)
	}
}

func TestCompileLaunchStoryboardIsDeterministic(t *testing.T) {
	var outputs [][]byte
	for i := 0; i < 3; i++ {
		out, result := compileLaunch(t, "IdiomVariantLaunchScreen.storyboard")
		outputs = append(outputs, readNib(t, filepath.Join(out, result.ViewNib+".nib")))
	}
	for i := 1; i < len(outputs); i++ {
		if string(outputs[i]) != string(outputs[0]) {
			t.Fatalf("trait-variant NIB differs between compile 0 and %d", i)
		}
	}
}

func TestCompileLaunchStoryboardFallsBackForUnsupportedTraitProperty(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "IdiomVariantLaunchScreen.storyboard"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(source), `image="LaunchImagePad"`, `hidden="YES"`, 1)
	path := filepath.Join(t.TempDir(), "UnsupportedVariation.storyboard")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "LaunchScreen.storyboardc")
	_, err = CompileLaunchStoryboard(path, out)
	if err == nil {
		t.Fatal("unsupported trait property compiled without reporting unsupported content")
	}
	if !IsUnsupportedCompileError(err) {
		t.Fatalf("error = %T %v, want UnsupportedCompileError", err, err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("unsupported compilation left output at %s: %v", out, statErr)
	}
}

func TestNibArchiveBooleanTagsMatchUIKit(t *testing.T) {
	root := nibObj("NSObject").add("false", false).add("true", true)
	data, err := encodeNibArchive(root)
	if err != nil {
		t.Fatal(err)
	}
	// Parse just enough of the archive to reach the value records. The bool
	// tags are a subtle format detail: current UIKit/Xcode use 4=true, 5=false.
	keyCount := int(binary.LittleEndian.Uint32(data[26:30]))
	valueCount := int(binary.LittleEndian.Uint32(data[34:38]))
	valueOffset := int(binary.LittleEndian.Uint32(data[38:42]))
	if keyCount != 2 || valueCount != 2 {
		t.Fatalf("counts keys=%d values=%d", keyCount, valueCount)
	}
	pos := valueOffset
	_, n := readTestFlex(data[pos:])
	pos += n
	if got := data[pos]; got != nibTypeFalse {
		t.Fatalf("false type tag = %d, want %d", got, nibTypeFalse)
	}
	pos++
	_, n = readTestFlex(data[pos:])
	pos += n
	if got := data[pos]; got != nibTypeTrue {
		t.Fatalf("true type tag = %d, want %d", got, nibTypeTrue)
	}
}

func readTestFlex(data []byte) (int, int) {
	value, shift := 0, 0
	for i, b := range data {
		value |= int(b&0x7f) << shift
		if b&0x80 != 0 {
			return value, i + 1
		}
		shift += 7
	}
	return 0, 0
}

func TestCompileMainStoryboardProducesFlutterViewControllerStoryboardc(t *testing.T) {
	path := filepath.Join("testdata", "FlutterMainInterface.storyboard")
	out := filepath.Join(t.TempDir(), "Main.storyboardc")
	result, err := CompileMainStoryboard(path, out)
	if err != nil {
		t.Fatalf("CompileMainStoryboard: %v", err)
	}
	if result.ControllerNib != "UIViewController-BYZ-38-t0r" {
		t.Fatalf("ControllerNib = %q", result.ControllerNib)
	}
	scene := readNib(t, filepath.Join(out, result.ControllerNib+".nib"))
	for _, value := range []string{"UIClassSwapper", "FlutterViewController", "UIViewController", result.ViewNib} {
		requireNibString(t, scene, value)
	}
	view := readNib(t, filepath.Join(out, result.ViewNib+".nib"))
	requireNibString(t, view, "UIView")

	data, err := os.ReadFile(filepath.Join(out, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if _, err := plist.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if info["UIStoryboardDesignatedEntryPointIdentifier"] != result.ControllerNib {
		t.Fatalf("entry point = %#v", info["UIStoryboardDesignatedEntryPointIdentifier"])
	}
}

func TestCompileMainStoryboardRejectsRichInterface(t *testing.T) {
	path := filepath.Join("testdata", "CustomizedLaunchScreen.storyboard")
	out := filepath.Join(t.TempDir(), "Main.storyboardc")
	if _, err := CompileMainStoryboard(path, out); err == nil {
		t.Fatal("rich main interface compiled as stock Flutter Main.storyboard")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("rejected main compilation left output: %v", err)
	}
}
