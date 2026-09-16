package ios

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"ripley/internal/toolchain"
)

// fakeManifests installs a swiftPMCommandRunner that answers dump-package from a
// map of package-root -> manifest JSON, so manifest handling is exercised
// without a Swift toolchain.
func fakeManifests(t *testing.T, manifests map[string]string) {
	t.Helper()
	// macOS resolves t.TempDir() through /var -> /private/var, and the resolver
	// canonicalizes package roots (Flutter reaches plugin packages via symlinks),
	// so both sides of the lookup are normalized here.
	normalized := make(map[string]string, len(manifests))
	for root, body := range manifests {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		normalized[root] = body
	}
	previous := swiftPMCommandRunner
	swiftPMCommandRunner = func(_ toolchain.Toolchain, dir string, args ...string) ([]byte, error) {
		root := dir
		for i, arg := range args {
			if arg == "--package-path" && i+1 < len(args) {
				root = args[i+1]
			}
		}
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		body, ok := normalized[root]
		if !ok {
			t.Fatalf("dump-package for unexpected package root %s", root)
		}
		return []byte("warning: ignored preamble\n" + body), nil
	}
	t.Cleanup(func() { swiftPMCommandRunner = previous })
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

func TestFlutterPluginSwiftPackageRootFindsNestedManifest(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "app_settings")
	packageRoot := filepath.Join(pluginRoot, "ios", "app_settings")
	writeFile(t, filepath.Join(packageRoot, "Package.swift"), "// swift-tools-version: 6.0\n")
	got, err := flutterPluginSwiftPackageRoot(Plugin{Name: "app_settings", Path: pluginRoot, IOSDir: filepath.Join(pluginRoot, "ios")})
	if err != nil {
		t.Fatal(err)
	}
	if got != packageRoot {
		t.Fatalf("package root = %q, want %q", got, packageRoot)
	}
}

func TestSwiftPMGraphTreatsFlutterFrameworkAsProvidedByToolchain(t *testing.T) {
	plugin := &swiftPMPackage{
		Root:     t.TempDir(),
		Identity: "app_settings",
		Manifest: swiftPMManifest{
			Name:     "app_settings",
			Products: []swiftPMProduct{{Name: "app-settings", Targets: []string{"app_settings"}}},
			Targets: []swiftPMTarget{{
				Name: "app_settings",
				Type: "regular",
				ParsedDependencies: []swiftPMTargetDependency{{
					ProductName: "FlutterFramework",
					PackageName: "FlutterFramework",
				}},
			}},
		},
	}
	resolver := &swiftPMResolver{loaded: map[string]*swiftPMPackage{plugin.Root: plugin}}
	graph, err := newSwiftPMGraph([]swiftPMRootPackage{{Package: plugin, Products: []string{"app-settings"}}}, resolver)
	if err != nil {
		t.Fatalf("generated FlutterFramework marker must not require a materialized package: %v", err)
	}
	if len(graph.Ordered) != 1 || graph.Ordered[0].Target.Name != "app_settings" {
		t.Fatalf("unexpected reachable graph: %#v", graph.Ordered)
	}
}

func TestSwiftPMNoUsageIsCheapAndSilent(t *testing.T) {
	root := t.TempDir()
	pbxproj := filepath.Join(root, "ios", "Runner.xcodeproj", "project.pbxproj")
	writeFile(t, pbxproj, "// !$*UTF8*$!\n{\n\tobjects = {\n\t};\n}\n")

	swiftPMCommandRunner = func(toolchain.Toolchain, string, ...string) ([]byte, error) {
		t.Fatal("no Swift toolchain call expected when the project uses no packages")
		return nil, nil
	}
	t.Cleanup(func() {
		swiftPMCommandRunner = swiftPMDefaultCommandRunner
	})

	build, err := BuildSwiftPMPackages(toolchain.Toolchain{}, root, filepath.Join(root, "ios", "Runner"), "13.0")
	if err != nil {
		t.Fatalf("no SwiftPM usage must not be an error: %v", err)
	}
	if len(build.Objects) != 0 || len(build.Packages) != 0 || len(build.Frameworks) != 0 {
		t.Fatalf("expected empty build, got %+v", build)
	}
}

func TestSwiftPMDiscoversLocalAndRemoteReferences(t *testing.T) {
	iosRoot := t.TempDir()
	generated := filepath.Join(iosRoot, "Flutter", "ephemeral", "Packages", flutterGeneratedPluginSwiftPackageName)
	writeFile(t, filepath.Join(generated, "Package.swift"), "// swift-tools-version: 5.9\n")
	local := filepath.Join(iosRoot, "LocalPkg")
	writeFile(t, filepath.Join(local, "Package.swift"), "// swift-tools-version: 5.9\n")

	// Shape copied from a real Xcode-written pbxproj.
	writeFile(t, filepath.Join(iosRoot, "Runner.xcodeproj", "project.pbxproj"), `// !$*UTF8*$!
{
	objects = {
/* Begin XCLocalSwiftPackageReference section */
		781AD8BC2B33823900A9FFBB /* XCLocalSwiftPackageReference "LocalPkg" */ = {
			isa = XCLocalSwiftPackageReference;
			relativePath = LocalPkg;
		};
/* End XCLocalSwiftPackageReference section */
/* Begin XCRemoteSwiftPackageReference section */
		AA0000000000000000000001 /* XCRemoteSwiftPackageReference "sqlite-data" */ = {
			isa = XCRemoteSwiftPackageReference;
			repositoryURL = "https://github.com/pointfreeco/sqlite-data";
			requirement = {
				kind = upToNextMajorVersion;
				minimumVersion = 1.0.0;
			};
		};
/* End XCRemoteSwiftPackageReference section */
	};
}
`)

	refs, err := discoverSwiftPMReferences(iosRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("expected generated + local + remote references, got %d: %+v", len(refs), refs)
	}
	if refs[0].Path != generated {
		t.Errorf("first reference = %q, want Flutter generated package %q", refs[0].Path, generated)
	}
	if refs[1].Path != local {
		t.Errorf("local reference = %q, want %q", refs[1].Path, local)
	}
	remote := refs[2]
	if remote.URL != "https://github.com/pointfreeco/sqlite-data" {
		t.Errorf("remote URL = %q", remote.URL)
	}
	if remote.identity() != "sqlite-data" {
		t.Errorf("remote identity = %q, want sqlite-data", remote.identity())
	}
	if !strings.Contains(remote.Requirement, "upToNextMajorVersion") {
		t.Errorf("requirement should be reported for error messages, got %q", remote.Requirement)
	}
}

// Real Xcode pbxproj writes `package = <ID> /* XCRemoteSwiftPackageReference
// "name" */;`. If the trailing comment defeats the field parse, the product list
// of the reference stays empty, which is indistinguishable from "the whole
// package is consumed" — and that silently drags every product of the package,
// including test-support ones needing XCTest, into an app build.
func TestSwiftPMProductDependenciesLimitWhatIsBuilt(t *testing.T) {
	iosRoot := t.TempDir()
	writeFile(t, filepath.Join(iosRoot, "Runner.xcodeproj", "project.pbxproj"), `// !$*UTF8*$!
{
	objects = {
/* Begin XCRemoteSwiftPackageReference section */
		FEE084F62EC172460045228E /* XCRemoteSwiftPackageReference "sqlite-data" */ = {
			isa = XCRemoteSwiftPackageReference;
			repositoryURL = "https://github.com/pointfreeco/sqlite-data";
			requirement = {
				kind = upToNextMajorVersion;
				minimumVersion = 1.6.0;
			};
		};
		FEE084F92EC1725A0045228E /* XCRemoteSwiftPackageReference "swift-http-structured-headers" */ = {
			isa = XCRemoteSwiftPackageReference;
			repositoryURL = "https://github.com/apple/swift-http-structured-headers.git";
			requirement = {
				kind = upToNextMajorVersion;
				minimumVersion = 1.0.0;
			};
		};
/* End XCRemoteSwiftPackageReference section */
/* Begin XCSwiftPackageProductDependency section */
		FEE084F72EC172460045228E /* SQLiteData */ = {
			isa = XCSwiftPackageProductDependency;
			package = FEE084F62EC172460045228E /* XCRemoteSwiftPackageReference "sqlite-data" */;
			productName = SQLiteData;
		};
		FEE084FA2EC1725A0045228E /* RawStructuredFieldValues */ = {
			isa = XCSwiftPackageProductDependency;
			package = FEE084F92EC1725A0045228E /* XCRemoteSwiftPackageReference "swift-http-structured-headers" */;
			productName = RawStructuredFieldValues;
		};
		FEE084FC2EC1725A0045228E /* StructuredFieldValues */ = {
			isa = XCSwiftPackageProductDependency;
			package = FEE084F92EC1725A0045228E /* XCRemoteSwiftPackageReference "swift-http-structured-headers" */;
			productName = StructuredFieldValues;
		};
/* End XCSwiftPackageProductDependency section */
	};
}
`)

	refs, err := discoverSwiftPMReferences(iosRoot)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string][]string)
	for _, ref := range refs {
		got[ref.identity()] = ref.Products
	}
	if want := []string{"SQLiteData"}; !reflect.DeepEqual(got["sqlite-data"], want) {
		t.Errorf("sqlite-data products = %v, want %v", got["sqlite-data"], want)
	}
	want := []string{"RawStructuredFieldValues", "StructuredFieldValues"}
	structured := append([]string(nil), got["swift-http-structured-headers"]...)
	sort.Strings(structured)
	if !reflect.DeepEqual(structured, want) {
		t.Errorf("swift-http-structured-headers products = %v, want %v", structured, want)
	}
}

func TestSwiftPMRemotePackageWithoutPinIsRefused(t *testing.T) {
	resolver := &swiftPMResolver{
		pins:   map[string]packageResolvedPin{},
		cache:  t.TempDir(),
		loaded: map[string]*swiftPMPackage{},
	}
	_, err := resolver.checkout(swiftPMReference{
		Name:        "sqlite-data",
		URL:         "https://github.com/pointfreeco/sqlite-data",
		Requirement: "upToNextMajorVersion 1.0.0",
	})
	if err == nil {
		t.Fatal("a remote package with no Package.resolved pin must fail, never float to a network resolve")
	}
	for _, want := range []string{"Package.resolved", "sqlite-data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestSwiftPMPackageResolvedPinsAreIndexedByIdentity(t *testing.T) {
	iosRoot := t.TempDir()
	writeFile(t, filepath.Join(iosRoot, "Runner.xcodeproj", "project.xcworkspace", "xcshareddata", "swiftpm", "Package.resolved"), `{
  "pins" : [
    {
      "identity" : "swift-http-structured-headers",
      "kind" : "remoteSourceControl",
      "location" : "https://github.com/apple/swift-http-structured-headers",
      "state" : { "revision" : "3fbdf4dd1b9b1b1b0e0d1a5e9c9a9d9f9e9c9b9a", "version" : "1.2.0" }
    }
  ],
  "version" : 3
}
`)
	pins, err := loadPackageResolved(iosRoot)
	if err != nil {
		t.Fatal(err)
	}
	pin, ok := pins["swift-http-structured-headers"]
	if !ok {
		t.Fatalf("pin not indexed by identity: %+v", pins)
	}
	if pin.State.Revision != "3fbdf4dd1b9b1b1b0e0d1a5e9c9a9d9f9e9c9b9a" {
		t.Errorf("revision = %q", pin.State.Revision)
	}
}

// localPackage builds a synthetic package tree and returns its root plus its
// dump-package JSON.
func localPackage(t *testing.T, dir, name string, targetsJSON string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, "Package.swift"), "// swift-tools-version: 5.9\n")
	return `{
  "name": "` + name + `",
  "platforms": [{"platformName": "ios", "version": "13.0"}],
  "products": [{"name": "` + name + `", "targets": ["Core"], "type": {"library": ["static"]}}],
  "dependencies": [],
  "targets": ` + targetsJSON + `,
  "toolsVersion": {"_version": "5.9.0"}
}`
}

func TestSwiftPMDependencyOrderPutsDependenciesFirst(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Sources", "Core", "a.swift"), "public let a = 1\n")
	writeFile(t, filepath.Join(root, "Sources", "CLib", "b.c"), "int b(void) { return 2; }\n")
	writeFile(t, filepath.Join(root, "Sources", "CLib", "include", "b.h"), "int b(void);\n")

	manifest := localPackage(t, root, "demo", `[
    {"name": "Core", "type": "regular", "dependencies": [{"byName": ["CLib", null]}], "exclude": [], "resources": [], "settings": []},
    {"name": "CLib", "type": "regular", "dependencies": [], "exclude": [], "resources": [], "settings": [], "publicHeadersPath": "include"}
  ]`)
	fakeManifests(t, map[string]string{root: manifest})

	resolver := &swiftPMResolver{cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := newSwiftPMGraph([]swiftPMRootPackage{{Package: pkg}}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, node := range graph.Ordered {
		order = append(order, node.Target.Name)
	}
	if len(order) != 2 || order[0] != "CLib" || order[1] != "Core" {
		t.Fatalf("link order = %v, want [CLib Core] so the dependency is linked before its dependent", order)
	}
}

func TestSwiftPMTargetDependencyCycleIsReported(t *testing.T) {
	root := t.TempDir()
	manifest := localPackage(t, root, "cyclic", `[
    {"name": "Core", "type": "regular", "dependencies": [{"byName": ["Other", null]}], "exclude": [], "resources": [], "settings": []},
    {"name": "Other", "type": "regular", "dependencies": [{"byName": ["Core", null]}], "exclude": [], "resources": [], "settings": []}
  ]`)
	fakeManifests(t, map[string]string{root: manifest})
	resolver := &swiftPMResolver{cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "cyclic", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSwiftPMGraph([]swiftPMRootPackage{{Package: pkg}}, resolver); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
}

func TestSwiftPMCrossPackageProductDependencyResolves(t *testing.T) {
	base := t.TempDir()
	app := filepath.Join(base, "app")
	lib := filepath.Join(base, "lib")
	writeFile(t, filepath.Join(app, "Sources", "Core", "a.swift"), "public let a = 1\n")
	writeFile(t, filepath.Join(lib, "Sources", "Leaf", "leaf.swift"), "public let leaf = 1\n")

	appManifest := `{
  "name": "app",
  "platforms": [{"platformName": "ios", "version": "13.0"}],
  "products": [{"name": "app", "targets": ["Core"], "type": {"library": ["static"]}}],
  "dependencies": [{"fileSystem": [{"identity": "lib", "nameForTargetDependencyResolutionOnly": "lib", "path": "` + lib + `"}]}],
  "targets": [{"name": "Core", "type": "regular", "dependencies": [{"product": ["LeafProduct", "lib", null, null]}], "exclude": [], "resources": [], "settings": []}],
  "toolsVersion": {"_version": "5.9.0"}
}`
	libManifest := `{
  "name": "lib",
  "platforms": [{"platformName": "ios", "version": "13.0"}],
  "products": [{"name": "LeafProduct", "targets": ["Leaf"], "type": {"library": ["static"]}}],
  "dependencies": [],
  "targets": [{"name": "Leaf", "type": "regular", "dependencies": [], "exclude": [], "resources": [], "settings": []}],
  "toolsVersion": {"_version": "5.9.0"}
}`
	writeFile(t, filepath.Join(app, "Package.swift"), "// swift-tools-version: 5.9\n")
	writeFile(t, filepath.Join(lib, "Package.swift"), "// swift-tools-version: 5.9\n")
	fakeManifests(t, map[string]string{app: appManifest, lib: libManifest})

	resolver := &swiftPMResolver{cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "app", Path: app})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := newSwiftPMGraph([]swiftPMRootPackage{{Package: pkg}}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, node := range graph.Ordered {
		order = append(order, node.Package.Manifest.Name+"/"+node.Target.Name)
	}
	if len(order) != 2 || order[0] != "lib/Leaf" || order[1] != "app/Core" {
		t.Fatalf("order = %v, want the dependency package's target first", order)
	}
}

func TestSwiftPMUnknownProductIsReported(t *testing.T) {
	root := t.TempDir()
	manifest := localPackage(t, root, "demo", `[
    {"name": "Core", "type": "regular", "dependencies": [{"product": ["Nope", "ghost", null, null]}], "exclude": [], "resources": [], "settings": []}
  ]`)
	fakeManifests(t, map[string]string{root: manifest})
	resolver := &swiftPMResolver{cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = newSwiftPMGraph([]swiftPMRootPackage{{Package: pkg}}, resolver)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected an error naming the unknown package, got %v", err)
	}
}

// testToolchain points the SDK/Swift lookups at fabricated directories so flag
// composition can be asserted without any Apple or Swift tooling present.
func testToolchain(t *testing.T) toolchain.Toolchain {
	t.Helper()
	home := t.TempDir()
	t.Setenv("RIPLEY_HOME", home)
	sdk := filepath.Join(home, "sdks", "iPhoneOS26.5.sdk")
	if err := os.MkdirAll(filepath.Join(sdk, "System", "Library", "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPLEY_IOS_SDK", sdk)

	swiftRoot := filepath.Join(home, "swift-test")
	swiftLib := filepath.Join(swiftRoot, "usr", "lib", "swift")
	if err := os.MkdirAll(filepath.Join(swiftLib, "shims"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(swiftRoot, "usr", "lib", "clang", "17"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(swiftRoot, "usr", "bin", "swiftc"), "#!/bin/sh\n")
	iphoneos := filepath.Join(home, "sdks", "iphoneos")
	if err := os.MkdirAll(iphoneos, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPLEY_SWIFT", swiftRoot)
	t.Setenv("RIPLEY_SWIFT_IOS_LIBS", iphoneos)

	return toolchain.NewToolchain("testengine", "3.13.3")
}

func TestSwiftPMCompileSpecComposesFlags(t *testing.T) {
	tc := testToolchain(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Sources", "Core", "a.swift"), "public let a = 1\n")
	writeFile(t, filepath.Join(root, "Sources", "Core", "helper.m"), "void helper(void) {}\n")
	writeFile(t, filepath.Join(root, "Sources", "Core", "include", "Core.h"), "void helper(void);\n")
	writeFile(t, filepath.Join(root, "Sources", "Core", "Resources", "data.json"), "{}\n")

	manifest := `{
  "name": "demo",
  "platforms": [{"platformName": "ios", "version": "15.0"}],
  "products": [{"name": "demo", "targets": ["Core"], "type": {"library": ["static"]}}],
  "dependencies": [],
  "targets": [{
    "name": "Core", "type": "regular", "dependencies": [], "exclude": [],
    "publicHeadersPath": "include",
    "resources": [{"path": "Resources", "rule": {"process": {}}}],
    "settings": [
      {"tool": "c", "kind": {"define": {"_0": "PICKER_MEDIA=1"}}},
      {"tool": "c", "kind": {"headerSearchPath": {"_0": "include"}}},
      {"tool": "c", "kind": {"unsafeFlags": {"_0": ["-fno-objc-arc"]}}},
      {"tool": "swift", "kind": {"define": {"_0": "FEATURE_X"}}},
      {"tool": "swift", "kind": {"unsafeFlags": {"_0": ["-enable-testing"]}}},
      {"tool": "linker", "kind": {"linkedFramework": {"_0": "UIKit"}}},
      {"tool": "linker", "kind": {"linkedLibrary": {"_0": "z"}}}
    ]
  }],
  "toolsVersion": {"_version": "5.9.0"}
}`
	writeFile(t, filepath.Join(root, "Package.swift"), "// swift-tools-version: 5.9\n")
	fakeManifests(t, map[string]string{root: manifest})

	resolver := &swiftPMResolver{tc: tc, cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	node := swiftPMGraphTarget{Package: pkg, Target: pkg.Manifest.Targets[0]}
	work := t.TempDir()
	spec, err := swiftPMTargetCompileSpec(tc, node, nil, work, "13.0", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	swiftArgs := strings.Join(spec.SwiftArgs, " ")
	// The package requires iOS 15.0 while the app allows 13.0; the higher wins.
	if !strings.Contains(swiftArgs, "-target arm64-apple-ios15.0") {
		t.Errorf("swift args must target the package's higher minimum iOS: %s", swiftArgs)
	}
	if !strings.Contains(swiftArgs, "-module-name Core") {
		t.Errorf("missing module name: %s", swiftArgs)
	}
	if !strings.Contains(swiftArgs, "-DFEATURE_X") {
		t.Errorf("swiftSettings define must reach swiftc: %s", swiftArgs)
	}
	if !strings.Contains(swiftArgs, "-enable-testing") {
		t.Errorf("swiftSettings unsafeFlags must reach swiftc: %s", swiftArgs)
	}
	if !strings.Contains(swiftArgs, "-emit-module-path "+spec.SwiftModule) {
		t.Errorf("swift compile must emit a .swiftmodule: %s", swiftArgs)
	}
	if !strings.HasSuffix(spec.SwiftModule, "Core.swiftmodule") {
		t.Errorf("module path = %s", spec.SwiftModule)
	}

	if len(spec.ClangArgs) != 2 {
		t.Fatalf("expected the target's .m plus the generated resource accessor, got %d clang commands", len(spec.ClangArgs))
	}
	clangArgs := strings.Join(spec.ClangArgs[0], " ")
	if !strings.Contains(clangArgs, "-target arm64-apple-ios15.0") {
		t.Errorf("clang must use the same iOS target: %s", clangArgs)
	}
	if !strings.Contains(clangArgs, "-DPICKER_MEDIA=1") {
		t.Errorf("cSettings define must reach clang: %s", clangArgs)
	}
	if !strings.Contains(clangArgs, "-fno-objc-arc") {
		t.Errorf("cSettings unsafeFlags must reach clang: %s", clangArgs)
	}
	publicHeaders := filepath.Join(pkg.Root, "Sources", "Core", "include")
	if !strings.Contains(clangArgs, "-I "+publicHeaders) {
		t.Errorf("publicHeadersPath must be on the include path: %s", clangArgs)
	}
	if !strings.Contains(swiftArgs, "-Xcc -I -Xcc "+publicHeaders) {
		t.Errorf("publicHeadersPath must be visible to swift's clang importer: %s", swiftArgs)
	}

	if len(spec.ResourceCopy) != 1 {
		t.Fatalf("expected one resource copy, got %+v", spec.ResourceCopy)
	}
	if !spec.ResourceCopy[0].Process {
		t.Error(".process resources must be marked processed")
	}
	if base := filepath.Base(filepath.Dir(spec.ResourceCopy[0].Dest)); base != "demo_Core.bundle" {
		// Bundle.module resolves exactly <package>_<target>.bundle at runtime.
		t.Errorf("resource bundle name = %q, want demo_Core.bundle", base)
	}
}

func TestSwiftPMCommandPluginIsRefusedAndBuildToolPluginRuns(t *testing.T) {
	tc := testToolchain(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Sources", "Core", "a.swift"), "public let a = 1\n")
	writeFile(t, filepath.Join(root, "Package.swift"), "// swift-tools-version: 5.9\n")

	manifest := `{
  "name": "demo",
  "platforms": [{"platformName": "ios", "version": "13.0"}],
  "products": [{"name": "demo", "targets": ["Core"], "type": {"library": ["static"]}}],
  "dependencies": [],
  "targets": [
    {"name": "Core", "type": "regular", "dependencies": [], "exclude": [], "resources": [], "settings": [],
     "pluginUsages": [{"plugin": ["Cmd", null]}]},
    {"name": "Cmd", "type": "plugin", "dependencies": [], "exclude": [], "resources": [], "settings": [],
     "pluginCapability": {"command": [{"custom": {"verb": "generate-code", "description": "d"}}, []]}}
  ],
  "toolsVersion": {"_version": "5.9.0"}
}`
	fakeManifests(t, map[string]string{root: manifest})
	resolver := &swiftPMResolver{tc: tc, cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	node := swiftPMGraphTarget{Package: pkg, Target: pkg.Manifest.Targets[0]}
	_, err = runSwiftPMPlugins(tc, node, t.TempDir())
	if err == nil {
		t.Fatal("a command plugin must be refused rather than silently skipped")
	}
	for _, want := range []string{"Cmd", "generate-code"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the plugin and its verb (%s)", err, want)
		}
	}
}

func TestSwiftPMBuildToolPluginOutputsBecomeSources(t *testing.T) {
	tc := testToolchain(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Sources", "Core", "a.swift"), "public let a = 1\n")
	writeFile(t, filepath.Join(root, "Package.swift"), "// swift-tools-version: 5.9\n")

	manifest := `{
  "name": "demo",
  "platforms": [{"platformName": "ios", "version": "13.0"}],
  "products": [{"name": "demo", "targets": ["Core"], "type": {"library": ["static"]}}],
  "dependencies": [],
  "targets": [
    {"name": "Core", "type": "regular", "dependencies": [], "exclude": [], "resources": [], "settings": [],
     "pluginUsages": [{"plugin": ["Gen", null]}]},
    {"name": "Gen", "type": "plugin", "dependencies": [], "exclude": [], "resources": [], "settings": [],
     "pluginCapability": {"buildTool": null}}
  ],
  "toolsVersion": {"_version": "5.9.0"}
}`
	fakeManifests(t, map[string]string{root: manifest})
	resolver := &swiftPMResolver{tc: tc, cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	var gotScratch, gotTarget string
	previous := swiftPMBuildDriver
	swiftPMBuildDriver = func(_ toolchain.Toolchain, packageRoot, scratch, targetName string) error {
		gotScratch, gotTarget = scratch, targetName
		// Mimic a prebuild command writing into the plugin outputs tree.
		writeFile(t, filepath.Join(scratch, "plugins", "outputs", "demo", "Core", "destination", "Gen", "gen.swift"), "public let generated = 7\n")
		return nil
	}
	t.Cleanup(func() { swiftPMBuildDriver = previous })

	node := swiftPMGraphTarget{Package: pkg, Target: pkg.Manifest.Targets[0]}
	generated, err := runSwiftPMPlugins(tc, node, work)
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget != "Core" {
		t.Errorf("plugin driver target = %q, want Core", gotTarget)
	}
	if !strings.HasPrefix(gotScratch, work) {
		t.Errorf("plugin scratch %q must live under the build work dir %q, never in the package source", gotScratch, work)
	}
	if len(generated) != 1 || filepath.Base(generated[0]) != "gen.swift" {
		t.Fatalf("plugin outputs = %v, want the generated gen.swift", generated)
	}

	spec, err := swiftPMTargetCompileSpec(tc, node, generated, work, "13.0", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, source := range spec.SwiftSources {
		if source == generated[0] {
			found = true
		}
	}
	if !found {
		t.Fatalf("plugin-generated source must be compiled, sources = %v", spec.SwiftSources)
	}
}

func TestSwiftPMBuildToolPluginProducingNothingFails(t *testing.T) {
	tc := testToolchain(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Package.swift"), "// swift-tools-version: 5.9\n")
	manifest := `{
  "name": "demo",
  "platforms": [],
  "products": [],
  "dependencies": [],
  "targets": [
    {"name": "Core", "type": "regular", "dependencies": [], "exclude": [], "resources": [], "settings": [],
     "pluginUsages": [{"plugin": ["Gen", null]}]},
    {"name": "Gen", "type": "plugin", "dependencies": [], "exclude": [], "resources": [], "settings": [],
     "pluginCapability": {"buildTool": null}}
  ],
  "toolsVersion": {"_version": "5.9.0"}
}`
	fakeManifests(t, map[string]string{root: manifest})
	resolver := &swiftPMResolver{tc: tc, cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	previous := swiftPMBuildDriver
	swiftPMBuildDriver = func(toolchain.Toolchain, string, string, string) error { return nil }
	t.Cleanup(func() { swiftPMBuildDriver = previous })

	node := swiftPMGraphTarget{Package: pkg, Target: pkg.Manifest.Targets[0]}
	if _, err := runSwiftPMPlugins(tc, node, t.TempDir()); err == nil {
		t.Fatal("a build tool plugin that emits nothing must fail loudly, not silently produce an empty build")
	}
}

func TestSwiftPMSourceFilesHonoursExcludeAndExplicitSources(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.swift"), "")
	writeFile(t, filepath.Join(root, "skip.swift"), "")
	writeFile(t, filepath.Join(root, "nested", "deep.m"), "")
	writeFile(t, filepath.Join(root, "include", "umbrella.h"), "")

	target := swiftPMTarget{Name: "Core", Exclude: []string{"skip.swift"}}
	swift, clang, err := swiftPMSourceFiles(root, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(swift) != 1 || filepath.Base(swift[0]) != "keep.swift" {
		t.Fatalf("swift sources = %v, exclude must drop skip.swift", swift)
	}
	if len(clang) != 1 || filepath.Base(clang[0]) != "deep.m" {
		t.Fatalf("clang sources = %v", clang)
	}

	explicit := swiftPMTarget{Name: "Core", Sources: []string{"keep.swift"}}
	swift, clang, err = swiftPMSourceFiles(root, explicit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(swift) != 1 || len(clang) != 0 {
		t.Fatalf("explicit sources must be the only inputs, got swift=%v clang=%v", swift, clang)
	}
}

func TestSwiftPMResourcePathsAreNotCompiled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.swift"), "")
	writeFile(t, filepath.Join(root, "Resources", "generated.swift"), "")
	resources := map[string]bool{filepath.Join(root, "Resources"): true}
	swift, _, err := swiftPMSourceFiles(root, swiftPMTarget{Name: "Core"}, resources)
	if err != nil {
		t.Fatal(err)
	}
	if len(swift) != 1 || filepath.Base(swift[0]) != "a.swift" {
		t.Fatalf("a .swift file inside a resource directory is data, not a source: %v", swift)
	}
}

func TestSwiftPMUnsupportedSettingIsReported(t *testing.T) {
	target := swiftPMTarget{
		Name: "Core",
		Settings: []swiftPMSetting{{
			Tool: "swift",
			Kind: map[string]json.RawMessage{"someFutureSetting": json.RawMessage(`{"_0":"x"}`)},
		}},
	}
	if _, err := decodeSwiftPMSettings(t.TempDir(), target); err == nil {
		t.Fatal("an unrecognised build setting must fail rather than be dropped silently")
	}
}

func TestSwiftPMEmbedInCodeResourceIsReported(t *testing.T) {
	tc := testToolchain(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Sources", "Core", "a.swift"), "")
	writeFile(t, filepath.Join(root, "Sources", "Core", "x.json"), "{}")
	pkg := &swiftPMPackage{
		Root: root,
		Manifest: swiftPMManifest{Name: "demo", Targets: []swiftPMTarget{{
			Name:      "Core",
			Type:      "regular",
			Resources: []swiftPMResource{{Path: "x.json", Rule: map[string]json.RawMessage{"embedInCode": json.RawMessage(`{}`)}}},
		}}},
	}
	node := swiftPMGraphTarget{Package: pkg, Target: pkg.Manifest.Targets[0]}
	_, err := swiftPMTargetCompileSpec(tc, node, nil, t.TempDir(), "13.0", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "embedInCode") {
		t.Fatalf("expected an explicit embedInCode error, got %v", err)
	}
}

func TestSwiftPMBinaryTargetSelectsIOSArm64Slice(t *testing.T) {
	root := t.TempDir()
	xcframework := filepath.Join(root, "Vendor", "Thing.xcframework")
	slice := filepath.Join(xcframework, "ios-arm64", "Thing.framework")
	if err := os.MkdirAll(filepath.Join(slice, "Headers"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(slice, "Thing"), "")
	writeFile(t, filepath.Join(xcframework, "Info.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>AvailableLibraries</key>
	<array>
		<dict>
			<key>LibraryIdentifier</key><string>ios-arm64-simulator</string>
			<key>LibraryPath</key><string>Thing.framework</string>
			<key>SupportedArchitectures</key><array><string>arm64</string></array>
			<key>SupportedPlatform</key><string>ios</string>
			<key>SupportedPlatformVariant</key><string>simulator</string>
		</dict>
		<dict>
			<key>LibraryIdentifier</key><string>ios-arm64</string>
			<key>LibraryPath</key><string>Thing.framework</string>
			<key>SupportedArchitectures</key><array><string>arm64</string></array>
			<key>SupportedPlatform</key><string>ios</string>
		</dict>
	</array>
	<key>CFBundlePackageType</key><string>XFWK</string>
</dict>
</plist>
`)
	path := "Vendor/Thing.xcframework"
	pkg := &swiftPMPackage{Root: root, Manifest: swiftPMManifest{Name: "demo"}}
	target := swiftPMTarget{Name: "Thing", Type: "binary", Path: &path}

	framework, objects, headers, err := resolveSwiftPMBinaryTarget(pkg, target, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if framework != slice {
		t.Fatalf("selected %q, want the device slice %q (never the simulator slice)", framework, slice)
	}
	if len(objects) != 0 {
		t.Errorf("a .framework slice contributes no standalone objects, got %v", objects)
	}
	found := false
	for _, dir := range headers {
		if dir == filepath.Join(slice, "Headers") {
			found = true
		}
	}
	if !found {
		t.Errorf("framework Headers dir must be published to dependents, got %v", headers)
	}
}

func TestSwiftPMRemoteBinaryTargetWithoutCachedArtifactIsReported(t *testing.T) {
	t.Setenv("RIPLEY_HOME", t.TempDir())
	url := "https://example.com/Thing.xcframework.zip"
	checksum := "abc123"
	pkg := &swiftPMPackage{Root: t.TempDir(), Identity: "demo", Manifest: swiftPMManifest{Name: "demo"}}
	target := swiftPMTarget{Name: "Thing", Type: "binary", URL: &url, Checksum: &checksum}
	_, _, _, err := resolveSwiftPMBinaryTarget(pkg, target, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), url) {
		t.Fatalf("a missing remote artifact must name its URL, got %v", err)
	}
}

func TestSwiftPMModuleNameSanitizesTargetName(t *testing.T) {
	if got := swiftPMModuleName("image-picker-ios"); got != "image_picker_ios" {
		t.Errorf("swiftPMModuleName = %q, want image_picker_ios", got)
	}
}

func TestSwiftPMExistingModuleMapIsAuthoritative(t *testing.T) {
	headers := t.TempDir()
	writeFile(t, filepath.Join(headers, "module.modulemap"), "module Vendor { umbrella header \"V.h\" }\n")
	dir, err := swiftPMModuleMapDir("Vendor", headers, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if dir != headers {
		t.Fatalf("a package's own module.modulemap must be used unchanged, got %q", dir)
	}
}

// macroPackage writes a package whose Core target depends on a `.macro` target,
// mirroring how swift-structured-queries declares StructuredQueriesMacros: the
// macro target depends on SwiftCompilerPlugin/SwiftSyntaxMacros products of a
// swift-syntax package dependency, which are host-only.
func macroPackage(t *testing.T, root, syntax string) string {
	t.Helper()
	writeFile(t, filepath.Join(root, "Sources", "Core", "a.swift"), "public let a = 1\n")
	writeFile(t, filepath.Join(root, "Sources", "CoreMacros", "Plugin.swift"), "// macro plugin entry point\n")
	writeFile(t, filepath.Join(root, "Package.swift"), "// swift-tools-version: 5.9\n")
	return `{
  "name": "demo",
  "platforms": [{"platformName": "ios", "version": "13.0"}],
  "products": [{"name": "demo", "targets": ["Core"], "type": {"library": ["static"]}}],
  "dependencies": [{"sourceControl": [{"identity": "swift-syntax", "location": {"local": [{"path": "` + syntax + `"}]}, "requirement": {"exact": ["1.0.0"]}}]}],
  "targets": [
    {"name": "Core", "type": "regular", "dependencies": [{"byName": ["CoreMacros", null]}], "exclude": [], "resources": [], "settings": []},
    {"name": "CoreMacros", "type": "macro", "exclude": [], "resources": [], "settings": [],
     "dependencies": [{"product": ["SwiftCompilerPlugin", "swift-syntax", null, null]},
                      {"product": ["SwiftSyntaxMacros", "swift-syntax", null, null]}]}
  ],
  "toolsVersion": {"_version": "5.9.0"}
}`
}

func syntaxPackage(t *testing.T, root string) string {
	t.Helper()
	writeFile(t, filepath.Join(root, "Package.swift"), "// swift-tools-version: 5.9\n")
	writeFile(t, filepath.Join(root, "Sources", "SwiftCompilerPlugin", "Plugin.swift"), "public struct P {}\n")
	writeFile(t, filepath.Join(root, "Sources", "SwiftSyntaxMacros", "Macro.swift"), "public struct M {}\n")
	return `{
  "name": "swift-syntax",
  "platforms": [],
  "products": [
    {"name": "SwiftCompilerPlugin", "targets": ["SwiftCompilerPlugin"], "type": {"library": ["automatic"]}},
    {"name": "SwiftSyntaxMacros", "targets": ["SwiftSyntaxMacros"], "type": {"library": ["automatic"]}}
  ],
  "dependencies": [],
  "targets": [
    {"name": "SwiftCompilerPlugin", "type": "regular", "dependencies": [], "exclude": [], "resources": [], "settings": []},
    {"name": "SwiftSyntaxMacros", "type": "regular", "dependencies": [], "exclude": [], "resources": [], "settings": []}
  ],
  "toolsVersion": {"_version": "5.9.0"}
}`
}

// fakeHostPluginModules creates the toolchain's prebuilt host SwiftSyntax
// directory that a macro plugin links against, and returns its path.
func fakeHostPluginModules(t *testing.T, tc toolchain.Toolchain) string {
	t.Helper()
	lib, err := tc.SwiftLib()
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(lib, "host")
	for _, module := range []string{"SwiftSyntaxMacros", "SwiftCompilerPluginMessageHandling"} {
		if err := os.MkdirAll(filepath.Join(host, module+".swiftmodule"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return host
}

// fakeSwiftc replaces the toolchain's swiftc with a script that records its
// arguments and touches whatever -o names, so compile orchestration can be
// exercised without a Swift toolchain. observe is called with each invocation's
// argument list, in order.
func fakeSwiftc(t *testing.T, tc toolchain.Toolchain, observe func(args []string)) func() {
	t.Helper()
	swiftc, err := swiftCompiler(tc)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "invocations")
	// The recorder writes one NUL-separated argument list per line, then creates
	// the output file so the caller's fileExists/freshness checks pass.
	script := `#!/bin/sh
printf '%s\0' "$@" >> ` + log + `
printf '\n' >> ` + log + `
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then out="$arg"; fi
  prev="$arg"
done
if [ -n "$out" ]; then mkdir -p "$(dirname "$out")" && : > "$out" && chmod +x "$out"; fi
exit 0
`
	// testToolchain already created a non-executable swiftc placeholder;
	// os.WriteFile does not change the mode of an existing file, so set it.
	if err := os.WriteFile(swiftc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(swiftc, 0o755); err != nil {
		t.Fatal(err)
	}
	return func() {
		data, err := os.ReadFile(log)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" {
				continue
			}
			args := strings.Split(strings.TrimSuffix(line, "\x00"), "\x00")
			observe(args)
		}
	}
}

// containsPair reports whether args contains flag immediately followed by value.
func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// A macro target must never be cross-compiled for the device, and its host-only
// swift-syntax dependencies must not enter the iOS link order.
func TestSwiftPMMacroTargetIsHostOnlyAndNotLinkedForIOS(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "demo")
	syntax := filepath.Join(base, "swift-syntax")
	manifest := macroPackage(t, root, syntax)
	fakeManifests(t, map[string]string{root: manifest, syntax: syntaxPackage(t, syntax)})

	resolver := &swiftPMResolver{cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := newSwiftPMGraph([]swiftPMRootPackage{{Package: pkg}}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, node := range graph.Ordered {
		order = append(order, node.Package.Manifest.Name+"/"+node.Target.Name)
	}
	// The macro target is ordered before its dependent (its plugin must exist
	// before Core compiles) but swift-syntax's targets are not in the iOS graph.
	if len(order) != 2 || order[0] != "demo/CoreMacros" || order[1] != "demo/Core" {
		t.Fatalf("order = %v, want [demo/CoreMacros demo/Core] with no swift-syntax targets cross-compiled", order)
	}

	// The macro node carries no iOS dependency edges, yet its package's
	// host-only dependencies must have been resolved so the plugin build can
	// find the swift-syntax checkout.
	for _, node := range graph.Ordered {
		if node.Target.Name != "CoreMacros" {
			continue
		}
		if len(node.Deps) != 0 {
			t.Errorf("macro target must contribute no iOS dependency edges, got %v", node.Deps)
		}
		if found := swiftPMFindPackage(node, "swift-syntax"); found == nil {
			t.Error("swift-syntax must be reachable from the macro node so SwiftCompilerPlugin can be built")
		}
	}
}

// The whole point of building a macro plugin is that swiftc loads it while
// compiling code that expands the macro. immich's own app target
// (ios/Runner/Schemas/Tables.swift) applies @Table from
// swift-structured-queries, so the plugin has to escape the package graph and
// reach the app host compile via SwiftPMBuild.MacroPlugins. Without that field
// the host swiftc line carries no -load-plugin-executable and the compile fails
// with "external macro implementation type ... could not be found".
func TestSwiftPMMacroPluginsAreExportedForTheHostCompile(t *testing.T) {
	tc := testToolchain(t)
	base := t.TempDir()
	root := filepath.Join(base, "demo")
	syntax := filepath.Join(base, "swift-syntax")
	fakeManifests(t, map[string]string{root: macroPackage(t, root, syntax), syntax: syntaxPackage(t, syntax)})

	// Stand in for the toolchain's prebuilt host SwiftSyntax.
	hostDir := fakeHostPluginModules(t, tc)

	resolver := &swiftPMResolver{tc: tc, cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := newSwiftPMGraph([]swiftPMRootPackage{{Package: pkg}}, resolver)
	if err != nil {
		t.Fatal(err)
	}

	// swiftc is faked: it only has to create the file at the -o path so the
	// macro plugin build looks like it produced an executable.
	work := t.TempDir()
	var macroInvocations [][]string
	drain := fakeSwiftc(t, tc, func(args []string) { macroInvocations = append(macroInvocations, args) })

	build, err := compileSwiftPMGraph(tc, graph, work, "13.0")
	if err != nil {
		t.Fatal(err)
	}
	drain()

	if len(build.MacroPlugins) != 1 {
		t.Fatalf("SwiftPMBuild.MacroPlugins = %v, want exactly the one macro target's plugin", build.MacroPlugins)
	}
	entry := build.MacroPlugins[0]
	// -load-plugin-executable takes <path>#<ModuleName>; the module name is what
	// swiftc matches against the macro's `module:` in its @attached declaration.
	path, module, ok := strings.Cut(entry, "#")
	if !ok {
		t.Fatalf("macro plugin entry %q must be <path>#<ModuleName> for -load-plugin-executable", entry)
	}
	if module != "CoreMacros" {
		t.Errorf("macro plugin module = %q, want CoreMacros", module)
	}
	if !fileExists(path) {
		t.Errorf("macro plugin executable %s was not produced", path)
	}
	if filepath.Base(path) != "CoreMacros" {
		t.Errorf("macro plugin path = %q, want it named after the macro target", path)
	}

	// A macro target contributes no device code: it must not be linked.
	for _, object := range build.Objects {
		if strings.Contains(object, "CoreMacros") {
			t.Errorf("macro target must not contribute link inputs, got %s", object)
		}
	}

	// The plugin is built for the host, not the device. An -target
	// arm64-apple-ios flag here would mean we tried to cross-compile a program
	// that has to execute on the build machine.
	if len(macroInvocations) == 0 {
		t.Fatal("no swiftc invocation recorded for the macro target")
	}
	var macroArgs []string
	for _, args := range macroInvocations {
		if len(args) > 0 && args[len(args)-1] == path {
			macroArgs = args
		}
	}
	if macroArgs == nil {
		t.Fatalf("no swiftc invocation produced %s; invocations: %v", path, macroInvocations)
	}
	joined := strings.Join(macroArgs, " ")
	if strings.Contains(joined, "arm64-apple-ios") {
		t.Errorf("macro plugin must be built for the host, not the device: %s", joined)
	}
	// It links the toolchain's prebuilt host SwiftSyntax rather than building
	// swift-syntax from source, which OOMs on a small builder.
	if !strings.Contains(joined, "-I "+hostDir) || !strings.Contains(joined, "-L "+hostDir) {
		t.Errorf("macro plugin must compile and link against the prebuilt host modules at %s: %s", hostDir, joined)
	}
	if !strings.Contains(joined, "-parse-as-library") {
		t.Errorf("macro plugin needs -parse-as-library so its @main plugin entry point is honoured: %s", joined)
	}

	// Downstream iOS compiles must actually receive the flag.
	var coreArgs []string
	for _, args := range macroInvocations {
		if strings.Contains(strings.Join(args, " "), "-module-name Core ") {
			coreArgs = args
		}
	}
	if coreArgs == nil {
		t.Fatalf("no swiftc invocation compiled the Core target; invocations: %v", macroInvocations)
	}
	if !containsPair(coreArgs, "-load-plugin-executable", entry) {
		t.Errorf("the dependent target's compile must pass -load-plugin-executable %s, got %v", entry, coreArgs)
	}
}

// Building swift-syntax from source needs far more memory than a small Linux
// builder has, so the prebuilt host modules are required. When the toolchain
// does not ship them the failure must name the directory that was looked for
// rather than falling back to a source build that gets OOM-killed.
func TestSwiftPMMacroPluginWithoutPrebuiltHostSyntaxIsReported(t *testing.T) {
	tc := testToolchain(t)
	base := t.TempDir()
	root := filepath.Join(base, "demo")
	syntax := filepath.Join(base, "swift-syntax")
	fakeManifests(t, map[string]string{root: macroPackage(t, root, syntax), syntax: syntaxPackage(t, syntax)})

	// Deliberately do NOT create <swiftlib>/host.
	resolver := &swiftPMResolver{tc: tc, cache: t.TempDir(), loaded: map[string]*swiftPMPackage{}, pins: map[string]packageResolvedPin{}}
	pkg, err := resolver.load(swiftPMReference{Name: "demo", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := newSwiftPMGraph([]swiftPMRootPackage{{Package: pkg}}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	_, err = compileSwiftPMGraph(tc, graph, t.TempDir(), "13.0")
	if err == nil {
		t.Fatal("a macro target without prebuilt host SwiftSyntax must fail explicitly")
	}
	for _, want := range []string{"SwiftSyntaxMacros.swiftmodule", "macro"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %s so the missing directory is obvious", err, want)
		}
	}
}
