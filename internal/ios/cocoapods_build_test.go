package ios

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStaticAggregatePodLinkerFlagsUsesConsumerXCConfig(t *testing.T) {
	manifest := podManifest{Targets: []podTarget{
		{Name: "Foo", ProductType: "com.apple.product-type.framework", ProductName: "Foo"},
		{
			Name:                     "Pods-FLResolver",
			ProductType:              "com.apple.product-type.framework",
			OtherLDFlags:             nil, // synthetic target itself may blank this
			BaseOtherLDFlags:         []string{"-ObjC", "-framework", "Foo", "-framework", "AVFoundation", "-lxml2"},
			BaseLibrarySearchPaths:   []string{"/sdk/usr/lib/swift"},
			BaseFrameworkSearchPaths: []string{"/pods/Foo"},
		},
	}}
	got := staticAggregatePodLinkerFlags(manifest)
	joined := strings.Join(got, " ")
	for _, want := range []string{"-ObjC", "-framework AVFoundation", "-lxml2"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("static aggregate flags %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "-framework Foo") {
		t.Fatalf("static aggregate flags %q must not relink pod framework Foo from aggregate xcconfig", joined)
	}
}

func TestEmbedPodResourceBundlesCopiesBundleIntoFramework(t *testing.T) {
	root := t.TempDir()
	framework := filepath.Join(root, "RipleyPods.framework")
	bundle := filepath.Join(root, "Release-iphoneos", "LatinOCRResources.bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(framework, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "model.tflite"), []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := embedPodResourceBundles(framework, []string{bundle, bundle}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(framework, "LatinOCRResources.bundle", "model.tflite"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "model" {
		t.Fatalf("embedded resource = %q, want %q", got, "model")
	}
	if _, err := os.Stat(bundle); err != nil {
		t.Fatalf("source bundle should remain available for app-root copy: %v", err)
	}
}

// A Swift pod is only importable from the application host compile if the build
// reports where its emitted .swiftmodule lives AND which module map describes
// the target's ObjC half. CocoaPods names that map `<Module>.modulemap`, not
// `module.modulemap`, so clang cannot discover it from a header search path: on
// real immich, `-I <dir>` alone still fails with "cannot load underlying module
// for 'native_video_player'". The map must therefore be surfaced for an explicit
// -fmodule-map-file.
func TestPodSwiftModuleInputsReportsModuleDirAndPublicMap(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")

	target := podTarget{Name: "native_video_player", ModuleName: "native_video_player"}
	moduleDir := filepath.Join(configDir, target.Name)
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "native_video_player.swiftmodule"), []byte("swiftmodule"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicDir := filepath.Join(podsRoot, "Headers", "Public", "native_video_player")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "native_video_player.modulemap")
	if err := os.WriteFile(publicMap, []byte("module native_video_player {\n  export *\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := podManifest{PodsRoot: podsRoot}
	dir, gotMap := podSwiftModuleInputs(manifest, target, configDir)
	if dir != moduleDir {
		t.Errorf("module dir = %q, want %q", dir, moduleDir)
	}
	// The public map, not the mirror written beside the .swiftmodule: an
	// umbrella header resolves its #imports relative to the map's own
	// directory, and only the public header tree holds the pod's other headers.
	if gotMap != publicMap {
		t.Errorf("module map = %q, want the public map %q", gotMap, publicMap)
	}
}

func TestPodClangModuleMapIncludesObjCOnlyPod(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	target := podTarget{Name: "FirebaseMessaging", ModuleName: "FirebaseMessaging"}
	publicDir := filepath.Join(podsRoot, "Headers", "Public", "FirebaseMessaging")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "FirebaseMessaging.modulemap")
	if err := os.WriteFile(publicMap, []byte("module FirebaseMessaging { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := podClangModuleMap(podManifest{PodsRoot: podsRoot}, target); got != publicMap {
		t.Fatalf("clang module map = %q, want %q", got, publicMap)
	}
}

// An ObjC-only pod emits no Swift module, so it must contribute no module search
// path. Returning its target dir anyway would put a directory with no
// .swiftmodule on every downstream -I line.
func TestPodSwiftModuleInputsSkipsTargetWithoutSwiftModule(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "objc_only"}
	if err := os.MkdirAll(filepath.Join(configDir, target.Name), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, moduleMap := podSwiftModuleInputs(podManifest{PodsRoot: filepath.Join(root, "Pods")}, target, configDir)
	if dir != "" || moduleMap != "" {
		t.Errorf("ObjC-only target contributed module inputs (%q, %q), want none", dir, moduleMap)
	}
}

// A pod whose module name differs from its target name emits
// <ModuleName>.swiftmodule; keying the probe off the target name would miss it.
func TestPodSwiftModuleInputsUsesModuleNameForSwiftModule(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "share_handler_ios", ModuleName: "ShareHandlerIOS"}

	moduleDir := filepath.Join(configDir, target.Name)
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "ShareHandlerIOS.swiftmodule"), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicDir := filepath.Join(podsRoot, "Headers", "Public", "ShareHandlerIOS")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "ShareHandlerIOS.modulemap")
	if err := os.WriteFile(publicMap, []byte("module ShareHandlerIOS {\n  export *\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, gotMap := podSwiftModuleInputs(podManifest{PodsRoot: podsRoot}, target, configDir)
	if dir != moduleDir || gotMap != publicMap {
		t.Errorf("got (%q, %q), want (%q, %q)", dir, gotMap, moduleDir, publicMap)
	}
}

// A pod resource bundle holding an .xcassets must be compiled, not rejected:
// several real pods (and Flutter plugins) ship their images that way, and
// UIImage(named:) in a bundle resolves against Assets.car.
func TestBuildPodResourceBundleCompilesAssetCatalog(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "Release-iphoneos")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(root, "Assets.xcassets")
	imageset := filepath.Join(catalog, "marker.imageset")
	if err := os.MkdirAll(imageset, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(imageset, "marker@2x.png"), 4, 4)
	contents := `{"images":[{"idiom":"universal","scale":"2x","filename":"marker@2x.png"}],"info":{"version":1,"author":"ripley"}}`
	if err := os.WriteFile(filepath.Join(imageset, "Contents.json"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	target := podTarget{
		Name:             "somepod-somepodResources",
		ProductName:      "somepodResources",
		DeploymentTarget: "13.0",
		Resources:        []string{catalog},
	}
	bundle, err := buildPodResourceBundle(target, configDir, "12.0")
	if err != nil {
		t.Fatalf("asset catalog in a pod resource bundle must compile: %v", err)
	}
	car := filepath.Join(bundle, "Assets.car")
	info, err := os.Stat(car)
	if err != nil {
		t.Fatalf("expected a compiled %s: %v", car, err)
	}
	if info.Size() == 0 {
		t.Error("Assets.car is empty")
	}
	// The catalog is lowered, never copied in verbatim: a bundled .xcassets is
	// not a runtime-loadable resource.
	if _, err := os.Stat(filepath.Join(bundle, "Assets.xcassets")); err == nil {
		t.Error("raw .xcassets must not be copied into the bundle")
	}
	if _, err := os.Stat(filepath.Join(bundle, "Info.plist")); err != nil {
		t.Errorf("resource bundle still needs its Info.plist: %v", err)
	}
}

func writeTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: uint8(x * 32), G: uint8(y * 32), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cupertino_http intentionally ships hand-written Objective-C facade headers
// for classes implemented in Swift. Swift's generated compatibility header
// therefore declares the same @objc classes a second time. Xcode keeps the
// generated header out of the clang module; putting both into one module makes
// `import cupertino_http` fail with "duplicate interface definition".
func TestExposeSwiftPodModuleOmitsConflictingGeneratedHeader(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	publicDir := filepath.Join(root, "Pods", "Headers", "Public", "cupertino_http")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	moduleMap := filepath.Join(publicDir, "cupertino_http.modulemap")
	// Include the generated header already to exercise incremental builds made
	// by the old behavior: the fix must remove it, not merely stop adding it.
	moduleText := "module cupertino_http {\n  umbrella header \"cupertino_http-umbrella.h\"\n  export *\n  header \"cupertino_http-Swift.h\"\n}\n"
	if err := os.WriteFile(moduleMap, []byte(moduleText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publicDir, "cupertino_http-umbrella.h"), []byte("#import \"CUPHTTPStreamingTask.h\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publicDir, "CUPHTTPStreamingTask.h"), []byte("@interface CUPHTTPStreamingTask : NSObject\n@end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swiftHeader := filepath.Join(publicDir, "cupertino_http-Swift.h")
	if err := os.WriteFile(swiftHeader, []byte("@interface CUPHTTPStreamingTask : NSObject\n@end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := podTarget{Name: "cupertino_http", ModuleName: "cupertino_http"}
	if err := exposeSwiftPodModule(configDir, target, moduleMap, swiftHeader); err != nil {
		t.Fatal(err)
	}
	derivedMap := podDerivedModuleMapPath(configDir, target)
	data, err := os.ReadFile(derivedMap)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`header "cupertino_http-Swift.h"`)) {
		t.Fatalf("conflicting generated Swift header remained in derived map %s:\n%s", derivedMap, data)
	}
	base, err := os.ReadFile(moduleMap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(base, []byte(`header "cupertino_http-Swift.h"`)) {
		t.Fatalf("source CocoaPods module map was unexpectedly mutated:\n%s", base)
	}
	if _, err := os.Stat(filepath.Join(podDerivedModuleMapDir(configDir, target), "cupertino_http-Swift.h")); err != nil {
		t.Fatalf("generated header should still be mirrored for direct Objective-C includes: %v", err)
	}
}

func TestExposeSwiftPodModuleIncludesNonConflictingGeneratedHeader(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	publicDir := filepath.Join(root, "Pods", "Headers", "Public", "SwiftPlugin")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	moduleMap := filepath.Join(publicDir, "SwiftPlugin.modulemap")
	if err := os.WriteFile(moduleMap, []byte("module SwiftPlugin {\n  export *\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publicDir, "ExistingObjC.h"), []byte("@interface ExistingObjC : NSObject\n@end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swiftHeader := filepath.Join(publicDir, "SwiftPlugin-Swift.h")
	if err := os.WriteFile(swiftHeader, []byte("@interface SwiftPluginClass : NSObject\n@end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := podTarget{Name: "SwiftPlugin", ModuleName: "SwiftPlugin"}
	if err := exposeSwiftPodModule(configDir, target, moduleMap, swiftHeader); err != nil {
		t.Fatal(err)
	}
	derivedMap := podDerivedModuleMapPath(configDir, target)
	data, err := os.ReadFile(derivedMap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`header "SwiftPlugin-Swift.h"`)) {
		t.Fatalf("non-conflicting generated Swift header was not exposed in derived map:\n%s", data)
	}
	base, err := os.ReadFile(moduleMap)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(base, []byte(`header "SwiftPlugin-Swift.h"`)) {
		t.Fatalf("source CocoaPods module map was unexpectedly mutated:\n%s", base)
	}
}

func TestPodManifestAppliesMatchingDeviceConditionalSettings(t *testing.T) {
	for _, want := range []string{
		"def condition_matches?(key, context)",
		"'sdk' => 'iphoneos'",
		"effective_list(raw, 'LIBRARY_SEARCH_PATHS', vars)",
		"effective_list(raw, 'OTHER_LDFLAGS', vars)",
	} {
		if !strings.Contains(podManifestRuby, want) {
			t.Fatalf("pod manifest script no longer applies device conditional setting %q", want)
		}
	}
	if strings.Contains(podManifestRuby, "values.uniq\nend") {
		t.Fatal("effective conditional settings must preserve repeated linker operators such as -framework")
	}
}

func TestPodLinkerFlagsUnwrapClangDriverForwarding(t *testing.T) {
	got := podLinkerFlags([]string{
		"$(inherited)",
		"-Wl,-force_load,/tmp/librive_native.a",
		"-lrive",
		"-Xlinker", "-dead_strip",
	})
	want := []string{"-force_load", "/tmp/librive_native.a", "-lrive", "-dead_strip"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("direct linker flags = %#v, want %#v", got, want)
	}
}

func TestPodTargetNeedsCXXRuntimeForObjCXX(t *testing.T) {
	if !podTargetNeedsCXXRuntime(podTarget{Sources: []podSource{{Path: "Sources/rive_native_plugin.mm"}}}) {
		t.Fatal("Objective-C++ pod target must link libc++")
	}
	if podTargetNeedsCXXRuntime(podTarget{Sources: []podSource{{Path: "Sources/plugin.m"}}}) {
		t.Fatal("pure Objective-C pod target must not gain an unnecessary libc++ dependency")
	}
}

func TestPodManifestFlattensAggregateDependencies(t *testing.T) {
	for _, want := range []string{
		"def native_dependency_names(target, seen = {})",
		"native_dependency_names(child, nested_seen)",
		"'dependencies' => native_dependency_names(target)",
	} {
		if !strings.Contains(podManifestRuby, want) {
			t.Fatalf("pod manifest script no longer contains aggregate dependency normalization %q", want)
		}
	}
}

func TestPodRegistrantIncludeDirsPreferDerivedSwiftModuleMap(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	publicDir := filepath.Join(podsRoot, "Headers", "Public", "storage_info")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "storage_info.modulemap")
	if err := os.WriteFile(publicMap, []byte("module storage_info { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "storage_info", ModuleName: "storage_info"}
	derivedDir := podDerivedModuleMapDir(configDir, target)
	if err := os.MkdirAll(derivedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	derivedMap := podDerivedModuleMapPath(configDir, target)
	if err := os.WriteFile(derivedMap, []byte("module storage_info { header \"storage_info-Swift.h\" export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	includeDirs := podRegistrantIncludeDirs(podManifest{PodsRoot: podsRoot}, []podTarget{target}, configDir)
	if len(includeDirs) < 3 {
		t.Fatalf("registrant include dirs too short: %#v", includeDirs)
	}
	if includeDirs[0] != derivedDir {
		t.Fatalf("first registrant include dir = %q, want derived module dir %q", includeDirs[0], derivedDir)
	}
	maps := clangModuleMaps(includeDirs, nil)
	if !containsString(maps, derivedMap) {
		t.Fatalf("registrant module maps = %#v, missing derived Swift map %q", maps, derivedMap)
	}
	if containsString(maps, publicMap) {
		t.Fatalf("canonical map %q shadowed derived Swift map in registrant inputs: %#v", publicMap, maps)
	}
}

func TestPodCaseInsensitiveHeaderAliasesMatchMacOSHeaderLookup(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "SYPictureMetadata")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	header := filepath.Join(sourceDir, "SYMetadataExif.h")
	if err := os.WriteFile(header, []byte("// header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "SYMetadataExif.m")
	if err := os.WriteFile(source, []byte("#import \"SYMetadataEXIF.h\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := podTarget{
		Name:    "flutter_image_compress_common",
		Sources: []podSource{{Path: source}},
		Headers: []podHeader{{Path: header}},
	}

	aliases, err := podCaseInsensitiveHeaderAliases(target, filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(aliases, "SYMetadataEXIF.h")
	resolved, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Fatalf("case-insensitive alias missing: %v", err)
	}
	want, err := filepath.EvalSymlinks(header)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(resolved, want) {
		t.Fatalf("alias resolves to %q, want %q", resolved, want)
	}
}

func TestPodCaseInsensitiveHeaderAliasesIgnoreExactSpelling(t *testing.T) {
	root := t.TempDir()
	header := filepath.Join(root, "Header.h")
	source := filepath.Join(root, "Source.m")
	if err := os.WriteFile(header, []byte("// header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("#include <Header.h>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	aliases, err := podCaseInsensitiveHeaderAliases(podTarget{
		Sources: []podSource{{Path: source}},
		Headers: []podHeader{{Path: header}},
	}, filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if aliases != "" {
		t.Fatalf("exact-case include unexpectedly created alias root %q", aliases)
	}
}

func TestPodOwnTargetHeaderMapUsesSourceSymlinks(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "Public", "Thing.h")
	project := filepath.Join(root, "Project", "Internal.h")
	for _, path := range []string{public, project} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("// header\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	target := podTarget{Name: "MixedKit", ModuleName: "MixedKit", Headers: []podHeader{
		{Path: public, Attributes: []string{"Public"}},
		{Path: project, Attributes: []string{"Project"}},
	}}
	rootMap, moduleDir, err := podOwnTargetHeaderMap(target, filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(moduleDir) != "MixedKit" || filepath.Dir(moduleDir) != rootMap {
		t.Fatalf("header map layout root=%q module=%q", rootMap, moduleDir)
	}
	for name, want := range map[string]string{"Thing.h": public, "Internal.h": project} {
		path := filepath.Join(moduleDir, name)
		got, err := os.Readlink(path)
		if err != nil {
			t.Fatalf("%s is not a source symlink: %v", path, err)
		}
		if got != want {
			t.Fatalf("%s -> %q, want %q", path, got, want)
		}
	}
}

func TestPodOwnTargetModuleMapConvertsFrameworkMapWithoutMutatingPods(t *testing.T) {
	root := t.TempDir()
	support := filepath.Join(root, "Pods", "Target Support Files", "MixedKit")
	if err := os.MkdirAll(support, 0o755); err != nil {
		t.Fatal(err)
	}
	moduleMap := filepath.Join(support, "MixedKit.modulemap")
	original := "framework module MixedKit {\n  umbrella header \"MixedKit-umbrella.h\"\n  export *\n}\n"
	if err := os.WriteFile(moduleMap, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(support, "MixedKit-umbrella.h"), []byte("#import \"MixedKit.h\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	derived, err := podOwnTargetModuleMap(podTarget{Name: "MixedKit", ModuleName: "MixedKit"}, moduleMap, filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(derived)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "framework module MixedKit") || !strings.Contains(string(got), "module MixedKit {") {
		t.Fatalf("derived module map = %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(derived), "MixedKit-umbrella.h")); err != nil {
		t.Fatalf("derived umbrella header missing: %v", err)
	}
	source, err := os.ReadFile(moduleMap)
	if err != nil {
		t.Fatal(err)
	}
	if string(source) != original {
		t.Fatalf("source CocoaPods module map was mutated: %q", source)
	}
}

func TestStagePodFrameworkMetadataExportsOnlyHeaderPhaseProducts(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	public := filepath.Join(root, "Public.h")
	project := filepath.Join(root, "ProjectOnly.h")
	private := filepath.Join(root, "Private.h")
	for path, body := range map[string]string{
		public:  "// public\n",
		project: "// project\n",
		private: "// private\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	target := podTarget{
		Name:        "HeaderKit",
		ProductName: "HeaderKit",
		ModuleName:  "HeaderKit",
		ProductType: "com.apple.product-type.framework",
		Headers: []podHeader{
			{Path: public, Attributes: []string{"Public"}},
			{Path: project, Attributes: []string{"Project"}},
			{Path: private, Attributes: []string{"Private"}},
		},
	}
	framework, err := stagePodFrameworkMetadata(podManifest{PodsRoot: podsRoot, FrameworkLinkage: "dynamic"}, target, configDir, "13.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(framework, "Headers", "Public.h")); err != nil {
		t.Fatalf("public header missing from framework Headers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(framework, "PrivateHeaders", "Private.h")); err != nil {
		t.Fatalf("private header missing from framework PrivateHeaders: %v", err)
	}
	for _, path := range []string{
		filepath.Join(framework, "Headers", "ProjectOnly.h"),
		filepath.Join(framework, "PrivateHeaders", "ProjectOnly.h"),
		filepath.Join(framework, "Headers", "Private.h"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("compile-only/private header leaked into public framework product at %s", path)
		}
	}
}

func TestStageStaticPodFrameworkUsesArchiveAsFrameworkBinary(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{
		Name:             "StaticKit",
		ProductName:      "StaticKit",
		ModuleName:       "StaticKit",
		ProductType:      "com.apple.product-type.framework",
		DeploymentTarget: "13.0",
	}
	archive := filepath.Join(configDir, target.Name, "libStaticKit-ripley-target.a")
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("!<arch>\nstatic-framework-fixture")
	if err := os.WriteFile(archive, want, 0o644); err != nil {
		t.Fatal(err)
	}

	framework, err := stageStaticPodFramework(
		podManifest{PodsRoot: podsRoot, FrameworkLinkage: "static"},
		builtPodTarget{Target: target, Archive: archive},
		configDir,
		"13.0",
	)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(framework, "StaticKit")
	got, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("static framework binary = %q, want exact archive payload %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(framework, "Modules", "module.modulemap")); err != nil {
		t.Fatalf("static framework missing module map: %v", err)
	}
	if _, err := os.Stat(filepath.Join(framework, "Info.plist")); err != nil {
		t.Fatalf("static framework missing Info.plist: %v", err)
	}
}

func TestStaticPodTargetLinkerFlagsPreserveSearchPathsAndForceLoad(t *testing.T) {
	root := t.TempDir()
	frameworks := filepath.Join(root, "Frameworks")
	libraries := filepath.Join(root, "Libraries")
	if err := os.MkdirAll(frameworks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(libraries, 0o755); err != nil {
		t.Fatal(err)
	}
	cargo := filepath.Join(root, "librust.a")
	if err := os.WriteFile(cargo, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := podTarget{
		FrameworkSearchPaths: []string{frameworks},
		LibrarySearchPaths:   []string{libraries},
		OtherLDFlags:         []string{"$(inherited)", "-force_load", cargo, "-framework", "Security"},
	}
	got := staticPodTargetLinkerFlags(target)
	want := []string{"-F", frameworks, "-L", libraries, "-force_load", cargo, "-framework", "Security"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("static linker flags = %#v, want %#v", got, want)
	}
}

func TestStaticAggregatePodLinkerFlagsKeepsObjCAndSystemFrameworks(t *testing.T) {
	root := t.TempDir()
	libDir := filepath.Join(root, "Libraries")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := podManifest{Targets: []podTarget{
		{Name: "StaticPod", ProductName: "StaticPod", ModuleName: "StaticPod", ProductType: "com.apple.product-type.framework"},
		{Name: "Pods-FLResolver", BaseLibrarySearchPaths: []string{libDir}, BaseOtherLDFlags: []string{
			"$(inherited)", "-ObjC",
			"-framework", "StaticPod",
			"-framework", "Flutter",
			"-framework", "StoreKit",
			"-weak_framework", "UserNotifications",
			"-lz",
		}},
	}}
	got := staticAggregatePodLinkerFlags(manifest)
	want := []string{"-L", libDir, "-ObjC", "-framework", "StoreKit", "-weak_framework", "UserNotifications", "-lz"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("aggregate static linker flags = %#v, want %#v", got, want)
	}
}

func TestExposeSwiftPodModuleKeepsSwiftModuleSearchDirModuleMapFree(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "PromisesSwift", ModuleName: "Promises"}
	moduleDir := filepath.Join(configDir, target.Name)
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale artifact produced by the old layout. exposeSwiftPodModule
	// must never recreate this path; compilePodTarget removes it before compile.
	legacyMap := filepath.Join(moduleDir, "Promises.modulemap")
	if err := os.WriteFile(legacyMap, []byte("module Promises { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(legacyMap); err != nil {
		t.Fatal(err)
	}

	publicDir := filepath.Join(root, "Pods", "Headers", "Public", "Promises")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "PromisesSwift.modulemap")
	if err := os.WriteFile(publicMap, []byte("module Promises {\n  umbrella header \"PromisesSwift-umbrella.h\"\n  export *\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publicDir, "PromisesSwift-umbrella.h"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swiftHeader := filepath.Join(publicDir, "Promises-Swift.h")
	if err := os.WriteFile(swiftHeader, []byte("@interface FBLPromise : NSObject\n@end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := exposeSwiftPodModule(configDir, target, publicMap, swiftHeader); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(moduleDir, "Promises.modulemap")) {
		t.Fatalf("derived clang map leaked beside .swiftmodule in %s", moduleDir)
	}
	if !fileExists(podDerivedModuleMapPath(configDir, target)) {
		t.Fatalf("derived clang map missing from isolated directory %s", podDerivedModuleMapDir(configDir, target))
	}
}

func TestExposeSwiftPodModuleCanRestageReadOnlyPublicHeaders(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "FirebaseSessions", ModuleName: "FirebaseSessions"}
	publicDir := filepath.Join(root, "Pods", "Headers", "Public", target.Name)
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "FirebaseSessions.modulemap")
	if err := os.WriteFile(publicMap, []byte("module FirebaseSessions { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readOnlyHeader := filepath.Join(publicDir, "FIRSESNanoPBHelpers.h")
	if err := os.WriteFile(readOnlyHeader, []byte("// readonly public header\n"), 0o464); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnlyHeader, 0o464); err != nil {
		t.Fatal(err)
	}
	swiftHeader := filepath.Join(root, "FirebaseSessions-Swift.h")
	if err := os.WriteFile(swiftHeader, []byte("// generated swift header\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := exposeSwiftPodModule(configDir, target, publicMap, swiftHeader); err != nil {
		t.Fatalf("first module staging failed: %v", err)
	}
	staged := filepath.Join(podDerivedModuleMapDir(configDir, target), filepath.Base(readOnlyHeader))
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o200 != 0 {
		t.Fatalf("staged header unexpectedly owner-writable: mode %#o", got)
	}
	linkInfo, err := os.Lstat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("staged public header mode = %v, want symlink", linkInfo.Mode())
	}
	sourceInfo, err := os.Stat(readOnlyHeader)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, info) {
		t.Fatal("staged public header does not preserve CocoaPods header identity")
	}
	// A second build must replace the symlink cleanly. The CocoaPods header
	// intentionally remains owner-read-only (0464), the exact FirebaseSessions
	// mode observed in a large modular-headers corpus app.
	if err := exposeSwiftPodModule(configDir, target, publicMap, swiftHeader); err != nil {
		t.Fatalf("restaging owner-read-only public header failed: %v", err)
	}
}

func TestPodSwiftModuleInputsPrefersPublicMapOverDerivedMap(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "PromisesSwift", ModuleName: "Promises"}
	moduleDir := filepath.Join(configDir, target.Name)
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "Promises.swiftmodule"), []byte("swiftmodule"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicDir := filepath.Join(podsRoot, "Headers", "Public", "Promises")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "PromisesSwift.modulemap")
	if err := os.WriteFile(publicMap, []byte("module Promises { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(podDerivedModuleMapDir(configDir, target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(podDerivedModuleMapPath(configDir, target), []byte("module Promises { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, moduleMap := podSwiftModuleInputs(podManifest{PodsRoot: podsRoot}, target, configDir)
	if dir != moduleDir {
		t.Fatalf("module dir = %q, want %q", dir, moduleDir)
	}
	if moduleMap != publicMap {
		t.Fatalf("module map = %q, want canonical public map %q", moduleMap, publicMap)
	}
}

func TestCanonicalModuleMapsDeduplicatesLogicalModuleName(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "PromisesSwift.modulemap")
	mirror := filepath.Join(root, "Promises.modulemap")
	other := filepath.Join(root, "Other.modulemap")
	for path, body := range map[string]string{
		public: "module Promises { export * }\n",
		mirror: "module Promises { header \"Promises-Swift.h\" export * }\n",
		other:  "module Other { export * }\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := canonicalModuleMaps([]string{public, mirror, other, public})
	want := []string{public, other}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical module maps = %#v, want %#v", got, want)
	}
}

func TestCleanPodCompilerFlagsRewritesGeneratedModuleMapToCanonicalPublicMap(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "PromisesSwift", ModuleName: "Promises"}
	publicDir := filepath.Join(podsRoot, "Headers", "Public", "Promises")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "PromisesSwift.modulemap")
	if err := os.WriteFile(publicMap, []byte("module Promises { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(configDir, target.Name, "Promises.modulemap")
	manifest := podManifest{PodsRoot: podsRoot, Targets: []podTarget{target}}
	got := cleanPodCompilerFlags(manifest, configDir, []string{"-DTEST", "-fmodule-map-file=" + legacy}, false)
	want := []string{"-DTEST", "-fmodule-map-file=" + publicMap}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compiler flags = %#v, want %#v", got, want)
	}
}

func TestCleanPodCompilerFlagsDeduplicatesModuleMapByLogicalName(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "PromisesSwift", ModuleName: "Promises"}
	publicDir := filepath.Join(podsRoot, "Headers", "Public", "Promises")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "PromisesSwift.modulemap")
	if err := os.WriteFile(publicMap, []byte("module Promises { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(configDir, target.Name, "Promises.modulemap")
	manifest := podManifest{PodsRoot: podsRoot, Targets: []podTarget{target}}
	got := cleanPodCompilerFlags(manifest, configDir, []string{
		"-fmodule-map-file=" + publicMap,
		"-fmodule-map-file=" + legacy,
	}, false)
	want := []string{"-fmodule-map-file=" + publicMap}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compiler flags = %#v, want %#v", got, want)
	}
}

func TestCleanPodCompilerFlagsUsesDerivedMapForObjCConsumer(t *testing.T) {
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	target := podTarget{Name: "FirebaseCoreInternal", ModuleName: "FirebaseCoreInternal"}
	publicDir := filepath.Join(podsRoot, "Headers", "Public", target.Name)
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	publicMap := filepath.Join(publicDir, "FirebaseCoreInternal.modulemap")
	if err := os.WriteFile(publicMap, []byte("module FirebaseCoreInternal { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(podDerivedModuleMapDir(configDir, target), 0o755); err != nil {
		t.Fatal(err)
	}
	derived := podDerivedModuleMapPath(configDir, target)
	if err := os.WriteFile(derived, []byte("module FirebaseCoreInternal { header \"FirebaseCoreInternal-Swift.h\" export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(configDir, target.Name, "FirebaseCoreInternal.modulemap")
	manifest := podManifest{PodsRoot: podsRoot, Targets: []podTarget{target}}
	got := cleanPodCompilerFlags(manifest, configDir, []string{"-fmodule-map-file=" + legacy}, true)
	want := []string{"-fmodule-map-file=" + derived}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compiler flags = %#v, want %#v", got, want)
	}
}

func TestCleanupLegacyPodModuleArtifactsRunsAcrossGraph(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "Release-iphoneos")
	manifest := podManifest{Targets: []podTarget{
		{Name: "PromisesSwift", ModuleName: "Promises"},
		{Name: "nanopb", ModuleName: "nanopb"},
	}}
	for _, target := range manifest.Targets {
		moduleName := target.ModuleName
		targetOut := filepath.Join(configDir, target.Name)
		if err := os.MkdirAll(filepath.Join(targetOut, "clang-module"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{moduleName + ".modulemap", moduleName + "-Swift.h"} {
			if err := os.WriteFile(filepath.Join(targetOut, name), []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// The isolated current layout must survive migration cleanup.
		if err := os.WriteFile(filepath.Join(targetOut, "clang-module", moduleName+".modulemap"), []byte("current"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cleanupLegacyPodModuleArtifacts(manifest, configDir)
	for _, target := range manifest.Targets {
		targetOut := filepath.Join(configDir, target.Name)
		for _, name := range []string{target.ModuleName + ".modulemap", target.ModuleName + "-Swift.h"} {
			if fileExists(filepath.Join(targetOut, name)) {
				t.Fatalf("legacy artifact survived graph cleanup: %s", filepath.Join(targetOut, name))
			}
		}
		if !fileExists(filepath.Join(targetOut, "clang-module", target.ModuleName+".modulemap")) {
			t.Fatalf("isolated derived map was removed for %s", target.Name)
		}
	}
}

func TestPodSwiftIncludeSearchPathsRejectsPureObjCProductDirs(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "Release-iphoneos")
	swiftDir := filepath.Join(configDir, "PromisesSwift")
	objcDir := filepath.Join(configDir, "nanopb")
	privateDir := filepath.Join(root, "Pods", "Sentry", "Sources")
	for _, dir := range []string{swiftDir, objcDir, privateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(swiftDir, "Promises.swiftmodule"), []byte("module"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stale root module map must not be enough to make a pure C/ObjC product
	// directory a Swift include root.
	if err := os.WriteFile(filepath.Join(objcDir, "nanopb.modulemap"), []byte("module nanopb { export * }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := podSwiftIncludeSearchPaths(configDir, []string{objcDir, privateDir, swiftDir, swiftDir})
	want := []string{privateDir, swiftDir}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Swift include search paths = %#v, want %#v", got, want)
	}
}

func TestModuleMapPathsFromCompilerFlagsHandlesSwiftXccPairs(t *testing.T) {
	got := moduleMapPathsFromCompilerFlags([]string{"-DTEST", "-Xcc", "-fmodule-map-file=/a/Foo.modulemap", "-fmodule-map-file=/b/Bar.modulemap"})
	want := []string{"/a/Foo.modulemap", "/b/Bar.modulemap"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("module maps = %#v, want %#v", got, want)
	}
}
