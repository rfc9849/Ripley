package ios

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ripley/internal/toolchain"
)

func TestDiscoverPluginsMergesGeneratedDependenciesWithPubspecMetadata(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "cache", "shared_preferences_foundation")
	if err := os.MkdirAll(filepath.Join(pluginRoot, "darwin"), 0o755); err != nil {
		t.Fatal(err)
	}
	pubspec := `name: shared_preferences_foundation
flutter:
  plugin:
    platforms:
      ios:
        pluginClass: SharedPreferencesPlugin
        sharedDarwinSource: true
`
	if err := os.WriteFile(filepath.Join(pluginRoot, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := map[string]any{
		"plugins": map[string]any{
			"ios": []any{map[string]any{
				"name":         "shared_preferences_foundation",
				"path":         pluginRoot,
				"native_build": true,
			}},
		},
	}
	data, err := json.Marshal(deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".flutter-plugins-dependencies"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".dart_tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dart_tool", "package_config.json"), []byte(`{"packages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	plugins, err := DiscoverPlugins(root, filepath.Join(root, ".dart_tool", "package_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 {
		t.Fatalf("got %d plugins, want 1", len(plugins))
	}
	plugin := plugins[0]
	if plugin.PluginClass != "SharedPreferencesPlugin" {
		t.Fatalf("plugin class = %q, want SharedPreferencesPlugin", plugin.PluginClass)
	}
	if plugin.IOSDir != filepath.Join(pluginRoot, "darwin") {
		t.Fatalf("iOS dir = %q", plugin.IOSDir)
	}
	if !plugin.NativeBuild {
		t.Fatal("native_build should remain true")
	}
}

// A pub workspace member carries no member-local package config: pub writes one
// shared config at the workspace root. Plugin discovery must scan the config it
// is given, otherwise an FFI/native plugin that only the shared config lists is
// silently not built.
func TestDiscoverPluginsUsesWorkspacePackageConfig(t *testing.T) {
	workspace := t.TempDir()
	member := filepath.Join(workspace, "app")
	pluginRoot := filepath.Join(workspace, "packages", "native_thing")
	if err := os.MkdirAll(filepath.Join(pluginRoot, "ios"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".dart_tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatal(err)
	}
	pubspec := `name: native_thing
flutter:
  plugin:
    platforms:
      ios:
        pluginClass: NativeThingPlugin
`
	if err := os.WriteFile(filepath.Join(pluginRoot, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(workspace, ".dart_tool", "package_config.json")
	contents := `{"packages":[{"name":"native_thing","rootUri":"../packages/native_thing","packageUri":"lib/"}]}`
	if err := os.WriteFile(config, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	plugins, err := DiscoverPlugins(member, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || plugins[0].Name != "native_thing" {
		t.Fatalf("workspace plugin not discovered: %+v", plugins)
	}
	if plugins[0].PluginClass != "NativeThingPlugin" {
		t.Fatalf("plugin class = %q", plugins[0].PluginClass)
	}
}

func TestParsePodspecKeepsVendoredArtifactsSeparate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "BinaryPlugin.podspec")
	podspec := `Pod::Spec.new do |s|
  s.name = 'BinaryPlugin'
  s.source_files = 'Classes/**/*.{h,m}'
  s.vendored_frameworks = ['Frameworks/Foo.xcframework', 'Frameworks/Bar.framework']
  s.vendored_libraries = 'Libraries/libBaz.a'
end
`
	if err := os.WriteFile(path, []byte(podspec), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := parsePodspec(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.VendoredFrameworks) != 2 || info.VendoredFrameworks[0] != "Frameworks/Foo.xcframework" || info.VendoredFrameworks[1] != "Frameworks/Bar.framework" {
		t.Fatalf("vendored frameworks = %#v", info.VendoredFrameworks)
	}
	if len(info.VendoredLibraries) != 1 || info.VendoredLibraries[0] != "Libraries/libBaz.a" {
		t.Fatalf("vendored libraries = %#v", info.VendoredLibraries)
	}
}

func TestSelectXCFrameworkSliceChoosesIOSArm64Device(t *testing.T) {
	root := t.TempDir()
	xc := filepath.Join(root, "Foo.xcframework")
	device := filepath.Join(xc, "ios-arm64")
	simulator := filepath.Join(xc, "ios-arm64_x86_64-simulator")
	for _, dir := range []string{filepath.Join(device, "Foo.framework"), filepath.Join(simulator, "Foo.framework")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Foo"), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>AvailableLibraries</key><array>
<dict><key>LibraryIdentifier</key><string>ios-arm64_x86_64-simulator</string><key>LibraryPath</key><string>Foo.framework</string><key>SupportedArchitectures</key><array><string>arm64</string><string>x86_64</string></array><key>SupportedPlatform</key><string>ios</string><key>SupportedPlatformVariant</key><string>simulator</string></dict>
<dict><key>LibraryIdentifier</key><string>ios-arm64</string><key>LibraryPath</key><string>Foo.framework</string><key>SupportedArchitectures</key><array><string>arm64</string></array><key>SupportedPlatform</key><string>ios</string></dict>
</array></dict></plist>`
	if err := os.WriteFile(filepath.Join(xc, "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	got, headers, err := selectXCFrameworkSlice(xc)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(device, "Foo.framework")
	if got != want || headers != "" {
		t.Fatalf("slice = %q headers=%q, want %q", got, headers, want)
	}
}

func TestFrameworkIsDynamicDistinguishesArchiveAndDylib(t *testing.T) {
	root := t.TempDir()
	staticFramework := filepath.Join(root, "Static.framework")
	if err := os.MkdirAll(staticFramework, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticFramework, "Static"), []byte("!<arch>\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if dynamic, err := frameworkIsDynamic(staticFramework); err != nil || dynamic {
		t.Fatalf("static framework: dynamic=%v err=%v", dynamic, err)
	}

	dynamicFramework := filepath.Join(root, "Dynamic.framework")
	if err := os.MkdirAll(dynamicFramework, 0o755); err != nil {
		t.Fatal(err)
	}
	// Minimal 64-bit Mach-O header is enough for debug/macho to identify MH_DYLIB.
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header[0:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(header[4:8], 0x0100000c)  // arm64
	binary.LittleEndian.PutUint32(header[12:16], uint32(6)) // MH_DYLIB
	if err := os.WriteFile(filepath.Join(dynamicFramework, "Dynamic"), header, 0o755); err != nil {
		t.Fatal(err)
	}
	if dynamic, err := frameworkIsDynamic(dynamicFramework); err != nil || !dynamic {
		t.Fatalf("dynamic framework: dynamic=%v err=%v", dynamic, err)
	}

	fatBinary := func(payload []byte) []byte {
		const headerSize = 8 + 20
		data := make([]byte, headerSize+len(payload))
		binary.BigEndian.PutUint32(data[0:4], 0xcafebabe)
		binary.BigEndian.PutUint32(data[4:8], 1)
		binary.BigEndian.PutUint32(data[8:12], 0x0100000c) // arm64
		binary.BigEndian.PutUint32(data[12:16], 0)
		binary.BigEndian.PutUint32(data[16:20], headerSize)
		binary.BigEndian.PutUint32(data[20:24], uint32(len(payload)))
		copy(data[headerSize:], payload)
		return data
	}
	fatStaticFramework := filepath.Join(root, "FatStatic.framework")
	if err := os.MkdirAll(fatStaticFramework, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fatStaticFramework, "FatStatic"), fatBinary([]byte("!<arch>\n")), 0o755); err != nil {
		t.Fatal(err)
	}
	if dynamic, err := frameworkIsDynamic(fatStaticFramework); err != nil || dynamic {
		t.Fatalf("fat static framework: dynamic=%v err=%v", dynamic, err)
	}

	fatDynamicFramework := filepath.Join(root, "FatDynamic.framework")
	if err := os.MkdirAll(fatDynamicFramework, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fatDynamicFramework, "FatDynamic"), fatBinary(header), 0o755); err != nil {
		t.Fatal(err)
	}
	if dynamic, err := frameworkIsDynamic(fatDynamicFramework); err != nil || !dynamic {
		t.Fatalf("fat dynamic framework: dynamic=%v err=%v", dynamic, err)
	}
}

func TestStaticFrameworkLinkInputExtractsArm64UniversalPayload(t *testing.T) {
	root := t.TempDir()
	framework := filepath.Join(root, "FatStatic.framework")
	if err := os.MkdirAll(framework, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("!<arch>\narm64 archive payload")
	const headerSize = 8 + 20
	fat := make([]byte, headerSize+len(payload))
	binary.BigEndian.PutUint32(fat[0:4], 0xcafebabe)
	binary.BigEndian.PutUint32(fat[4:8], 1)
	binary.BigEndian.PutUint32(fat[8:12], 0x0100000c) // arm64
	binary.BigEndian.PutUint32(fat[16:20], headerSize)
	binary.BigEndian.PutUint32(fat[20:24], uint32(len(payload)))
	copy(fat[headerSize:], payload)
	if err := os.WriteFile(filepath.Join(framework, "FatStatic"), fat, 0o755); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	input, forceLoad, err := staticFrameworkLinkInput(vendoredFramework{Path: framework, Name: "FatStatic"}, configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !forceLoad {
		t.Fatal("fat static archive was not marked for force_load")
	}
	got, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("extracted payload = %q, want %q", got, payload)
	}
	if input == filepath.Join(framework, "FatStatic") {
		t.Fatal("universal framework binary was passed through without arm64 extraction")
	}
}

func TestStaticFrameworkLinkInputUsesThinObjectDirectly(t *testing.T) {
	root := t.TempDir()
	framework := filepath.Join(root, "Object.framework")
	if err := os.MkdirAll(framework, 0o755); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header[0:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(header[4:8], 0x0100000c)  // arm64
	binary.LittleEndian.PutUint32(header[12:16], uint32(1)) // MH_OBJECT
	binary.LittleEndian.PutUint32(header[16:20], 0)         // ncmds
	binary.LittleEndian.PutUint32(header[20:24], 0)         // sizeofcmds
	binary.LittleEndian.PutUint32(header[24:28], 0)
	if err := os.WriteFile(filepath.Join(framework, "Object"), header, 0o755); err != nil {
		t.Fatal(err)
	}
	input, forceLoad, err := staticFrameworkLinkInput(vendoredFramework{Path: framework, Name: "Object"}, filepath.Join(root, "build", "Release-iphoneos"))
	if err != nil {
		t.Fatal(err)
	}
	if forceLoad {
		t.Fatal("Mach-O object framework should be linked as an object, not force_loaded")
	}
	if input != filepath.Join(framework, "Object") {
		t.Fatalf("thin object input = %q", input)
	}
}

func TestSwiftLanguageVersion(t *testing.T) {
	if got := swiftLanguageVersion("5.0"); got != "5" {
		t.Fatalf("swiftLanguageVersion(5.0) = %q, want 5", got)
	}
	if got := swiftLanguageVersion("5.9"); got != "5" {
		t.Fatalf("swiftLanguageVersion(5.9) = %q, want 5", got)
	}
	if got := swiftLanguageVersion("4.2"); got != "4.2" {
		t.Fatalf("swiftLanguageVersion(4.2) = %q, want 4.2", got)
	}
}

func TestWriteFrameworkSkeletonCanRestageReadOnlyHeaders(t *testing.T) {
	root := t.TempDir()
	header := filepath.Join(root, "ReadOnly.h")
	if err := os.WriteFile(header, []byte("// first\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	framework := filepath.Join(root, "ReadOnly.framework")
	sources := pluginSources{Headers: []string{header}, DeploymentTarget: "13.0", ModuleName: "ReadOnly"}
	if err := writeFrameworkSkeleton(framework, "ReadOnly", sources); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(header, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(header, []byte("// second\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := writeFrameworkSkeleton(framework, "ReadOnly", sources); err != nil {
		t.Fatalf("restaging a read-only framework header failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(framework, "Headers", "ReadOnly.h"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "// second\n" {
		t.Fatalf("restaged header = %q, want updated contents", got)
	}
}

func TestSwiftPrebuiltModuleFlagsMatchSDKVersion(t *testing.T) {
	root := t.TempDir()
	sdk := filepath.Join(root, "iPhoneOS27.0.sdk")
	if err := os.MkdirAll(sdk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "SDKSettings.json"), []byte(`{"Version":"27.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	libs := filepath.Join(root, "swift-ios")
	prebuilt := filepath.Join(libs, "prebuilt-modules", "27.0")
	if err := os.MkdirAll(prebuilt, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPLEY_IOS_SDK", sdk)
	t.Setenv("RIPLEY_SWIFT_IOS_LIBS", libs)

	got, err := swiftPrebuiltModuleFlags(toolchain.Toolchain{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-I", prebuilt}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prebuilt module flags = %#v, want %#v", got, want)
	}

	if err := os.RemoveAll(prebuilt); err != nil {
		t.Fatal(err)
	}
	got, err = swiftPrebuiltModuleFlags(toolchain.Toolchain{})
	if err == nil || !strings.Contains(err.Error(), "prebuilt modules") {
		t.Fatalf("missing Xcode 27 prebuilt modules error = %v, flags = %#v", err, got)
	}
}

func TestSwiftShimImportFlags(t *testing.T) {
	resource := t.TempDir()
	sdk := t.TempDir()
	if got := swiftShimImportFlags(resource, sdk); len(got) != 0 {
		t.Fatalf("flags without shims module map = %#v", got)
	}
	shims := filepath.Join(resource, "shims")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	moduleMap := filepath.Join(shims, "module.modulemap")
	if err := os.WriteFile(moduleMap, []byte("module SwiftShims {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	libcxx := filepath.Join(sdk, "usr", "include", "c++", "v1")
	if err := os.MkdirAll(libcxx, 0o755); err != nil {
		t.Fatal(err)
	}
	got := swiftShimImportFlags(resource, sdk)
	want := []string{
		"-Xcc", "-I", "-Xcc", shims,
		"-Xcc", "-I", "-Xcc", libcxx,
		"-Xcc", "-fmodule-map-file=" + moduleMap,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("swift shim flags = %#v, want %#v", got, want)
	}
}
