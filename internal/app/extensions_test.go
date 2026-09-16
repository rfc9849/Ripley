package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ripley/internal/ios"
)

// extensionFixture writes a project whose shape is immich's: an application
// target plus two app-extension targets, one drawing its whole compile set from
// an Xcode 16 folder-synchronized group that excludes Info.plist, the other
// listing one file the classic PBXSourcesBuildPhase way. Bundle identifiers are
// spelled as variables resolved from the project-level configuration, which is
// how immich's Signing.xcconfig feeds `$(IMMICH_BUNDLE_ID_DEV)` to every target.
//
// It returns an iosProject resolved to the named configuration.
func extensionFixture(t *testing.T, configuration string) iosProject {
	t.Helper()
	root := t.TempDir()
	iosRoot := filepath.Join(root, "ios")
	project := filepath.Join(iosRoot, "Runner.xcodeproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(rel, content string) string {
		path := filepath.Join(iosRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// The synchronized folder on disk. Every file below it is a member of the
	// owning target except the ones the exception set names.
	write("WidgetExtension/WidgetBundle.swift", "import WidgetKit\nimport SwiftUI\n@main\nstruct B: WidgetBundle { var body: some Widget { W() } }\n")
	write("WidgetExtension/widgets/MemoryWidget.swift", "import WidgetKit\nstruct W: Widget { var body: some WidgetConfiguration { fatalError() } }\n")
	write("WidgetExtension/ImmichAPI.swift", "import Foundation\n")
	write("WidgetExtension/UIImage+Resize.swift", "import UIKit\n")
	write("WidgetExtension/Info.plist", `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>AppGroupId</key><string>$(CUSTOM_GROUP_ID)</string>
	<key>NSExtension</key><dict>
		<key>NSExtensionPointIdentifier</key><string>com.apple.widgetkit-extension</string>
	</dict>
</dict></plist>`)
	write("WidgetExtension/WidgetExtension.entitlements", `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict/></plist>`)
	write("WidgetExtension/Assets.xcassets/Contents.json", `{"info":{"author":"xcode","version":1}}`)
	write("WidgetExtension/README.md", "not a source\n")
	// A folder-synced member ripley has no Linux compiler for. It must be reported,
	// never compiled and never silently dropped.
	write("WidgetExtension/Legacy.storyboard", "<document/>")

	// The classic target: one source listed the per-PBXBuildFile way.
	write("ShareExtension/ShareViewController.swift", "import UIKit\nimport Social\n")
	write("ShareExtension/Base.lproj/MainInterface.storyboard", "<document/>")
	write("ShareExtension/Info.plist", `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>NSExtension</key><dict>
		<key>NSExtensionMainStoryboard</key><string>MainInterface</string>
		<key>NSExtensionPointIdentifier</key><string>com.apple.share-services</string>
	</dict>
</dict></plist>`)

	// The application target's own plist, which resolveIOSProject requires.
	write("Runner/Info.plist", `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>CFBundleName</key><string>Runner</string>
</dict></plist>`)

	pbx := `// !$*UTF8*$!
{
	archiveVersion = 1;
	objectVersion = 54;
	objects = {
		AAAA000000000000000000A1 /* Project object */ = {
			isa = PBXProject;
			buildConfigurationList = AAAA000000000000000000B0 /* Build configuration list for PBXProject */;
			developmentRegion = en;
			targets = (
				AAAA000000000000000000C1 /* Runner */,
				AAAA000000000000000000C2 /* WidgetExtension */,
				AAAA000000000000000000C3 /* ShareExtension */,
			);
		};
		AAAA000000000000000000B0 /* Build configuration list for PBXProject */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				AAAA000000000000000000B1 /* Debug */,
				AAAA000000000000000000B2 /* Release */,
			);
		};
		AAAA000000000000000000B1 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				APP_ID_DEV = app.futo.immich;
				APP_ID_PROD = app.alextran.immich;
				CUSTOM_GROUP_ID = "group.app.immich.share.debug";
				IPHONEOS_DEPLOYMENT_TARGET = 15.0;
				TARGETED_DEVICE_FAMILY = "1,2";
			};
			name = Debug;
		};
		AAAA000000000000000000B2 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				APP_ID_DEV = app.futo.immich;
				APP_ID_PROD = app.alextran.immich;
				CUSTOM_GROUP_ID = "group.app.immich.share";
				IPHONEOS_DEPLOYMENT_TARGET = 15.0;
				TARGETED_DEVICE_FAMILY = "1,2";
			};
			name = Release;
		};

		AAAA000000000000000000C1 /* Runner */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = AAAA000000000000000000D0 /* Build configuration list for Runner */;
			buildPhases = (
			);
			name = Runner;
			productName = Runner;
			productType = "com.apple.product-type.application";
		};
		AAAA000000000000000000D0 /* Build configuration list for Runner */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				AAAA000000000000000000D1 /* Debug */,
				AAAA000000000000000000D2 /* Release */,
			);
		};
		AAAA000000000000000000D1 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				INFOPLIST_FILE = Runner/Info.plist;
				IPHONEOS_DEPLOYMENT_TARGET = 15.0;
				PRODUCT_BUNDLE_IDENTIFIER = "$(APP_ID_DEV).debug";
				PRODUCT_NAME = Runner;
			};
			name = Debug;
		};
		AAAA000000000000000000D2 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				INFOPLIST_FILE = Runner/Info.plist;
				IPHONEOS_DEPLOYMENT_TARGET = 15.0;
				PRODUCT_BUNDLE_IDENTIFIER = "$(APP_ID_PROD)";
				PRODUCT_NAME = Runner;
			};
			name = Release;
		};

		AAAA000000000000000000C2 /* WidgetExtension */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = AAAA000000000000000000E0 /* Build configuration list for WidgetExtension */;
			buildPhases = (
				AAAA000000000000000000E9 /* Sources */,
			);
			fileSystemSynchronizedGroups = (
				AAAA000000000000000000F0 /* WidgetExtension */,
			);
			name = WidgetExtension;
			productName = WidgetExtension;
			productType = "com.apple.product-type.app-extension";
		};
		AAAA000000000000000000E9 /* Sources */ = {
			isa = PBXSourcesBuildPhase;
			files = (
			);
		};
		AAAA000000000000000000E0 /* Build configuration list for WidgetExtension */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				AAAA000000000000000000E1 /* Debug */,
				AAAA000000000000000000E2 /* Release */,
			);
		};
		AAAA000000000000000000E1 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				CODE_SIGN_ENTITLEMENTS = WidgetExtension/WidgetExtension.entitlements;
				INFOPLIST_FILE = WidgetExtension/Info.plist;
				IPHONEOS_DEPLOYMENT_TARGET = 17.0;
				PRODUCT_BUNDLE_IDENTIFIER = "$(APP_ID_DEV).debug.Widget";
				PRODUCT_NAME = "$(TARGET_NAME)";
			};
			name = Debug;
		};
		AAAA000000000000000000E2 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				CODE_SIGN_ENTITLEMENTS = WidgetExtension/WidgetExtension.entitlements;
				INFOPLIST_FILE = WidgetExtension/Info.plist;
				IPHONEOS_DEPLOYMENT_TARGET = 17.0;
				PRODUCT_BUNDLE_IDENTIFIER = "$(APP_ID_PROD).Widget";
				PRODUCT_NAME = "$(TARGET_NAME)";
			};
			name = Release;
		};
		AAAA000000000000000000F0 /* WidgetExtension */ = {
			isa = PBXFileSystemSynchronizedRootGroup;
			exceptions = (
				AAAA000000000000000000F1 /* Exceptions */,
			);
			path = WidgetExtension;
			sourceTree = "<group>";
		};
		AAAA000000000000000000F1 /* Exceptions */ = {
			isa = PBXFileSystemSynchronizedBuildFileExceptionSet;
			membershipExceptions = (
				Info.plist,
			);
			target = AAAA000000000000000000C2 /* WidgetExtension */;
		};

		AAAA000000000000000000C3 /* ShareExtension */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = AAAA00000000000000000AE0 /* Build configuration list for ShareExtension */;
			buildPhases = (
				AAAA00000000000000000AE9 /* Sources */,
			);
			name = ShareExtension;
			productName = ShareExtension;
			productType = "com.apple.product-type.app-extension";
		};
		AAAA00000000000000000AE9 /* Sources */ = {
			isa = PBXSourcesBuildPhase;
			files = (
				AAAA00000000000000000AF5 /* ShareViewController.swift in Sources */,
			);
		};
		AAAA00000000000000000AF5 /* ShareViewController.swift in Sources */ = {
			isa = PBXBuildFile;
			fileRef = AAAA00000000000000000AF6 /* ShareViewController.swift */;
		};
		AAAA00000000000000000AF6 /* ShareViewController.swift */ = {
			isa = PBXFileReference;
			lastKnownFileType = sourcecode.swift;
			path = ShareExtension/ShareViewController.swift;
			sourceTree = SOURCE_ROOT;
		};
		AAAA00000000000000000AE0 /* Build configuration list for ShareExtension */ = {
			isa = XCConfigurationList;
			buildConfigurations = (
				AAAA00000000000000000AE1 /* Debug */,
				AAAA00000000000000000AE2 /* Release */,
			);
		};
		AAAA00000000000000000AE1 /* Debug */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				INFOPLIST_FILE = ShareExtension/Info.plist;
				PRODUCT_BUNDLE_IDENTIFIER = "$(APP_ID_DEV).debug.ShareExtension";
				PRODUCT_NAME = "$(TARGET_NAME)";
			};
			name = Debug;
		};
		AAAA00000000000000000AE2 /* Release */ = {
			isa = XCBuildConfiguration;
			buildSettings = {
				INFOPLIST_FILE = ShareExtension/Info.plist;
				PRODUCT_BUNDLE_IDENTIFIER = "$(APP_ID_PROD).ShareExtension";
				PRODUCT_NAME = "$(TARGET_NAME)";
			};
			name = Release;
		};
	};
	rootObject = AAAA000000000000000000A1 /* Project object */;
}
`
	if err := os.WriteFile(filepath.Join(project, "project.pbxproj"), []byte(pbx), 0o644); err != nil {
		t.Fatal(err)
	}

	return iosProject{
		Root:          root,
		IOSRoot:       iosRoot,
		XcodeProject:  project,
		TargetName:    "Runner",
		Configuration: configuration,
		MinOS:         "15.0",
		BuildName:     "1.2.3",
		BuildNumber:   "240",
		BuildSettings: map[string]string{},
	}
}

func extensionByName(t *testing.T, extensions []appExtension, name string) appExtension {
	t.Helper()
	for _, extension := range extensions {
		if extension.TargetName == name {
			return extension
		}
	}
	t.Fatalf("no extension named %s in %d discovered", name, len(extensions))
	return appExtension{}
}

// A widget extension whose Sources build phase is empty must still compile: its
// whole membership arrives through the folder-synchronized group. This is the
// bug the extension pipeline exists to fix, and immich's WidgetExtension is
// exactly this shape.
func TestDiscoverAppExtensionsResolvesSyncedGroupOnlySources(t *testing.T) {
	extensions, err := discoverAppExtensions(extensionFixture(t, "Release"))
	if err != nil {
		t.Fatal(err)
	}
	widget := extensionByName(t, extensions, "WidgetExtension")

	var names []string
	for _, source := range widget.Sources {
		names = append(names, filepath.Base(source))
	}
	// Order is the sorted absolute path, so the `widgets/` subdirectory sorts
	// after the files directly in the folder.
	want := []string{"ImmichAPI.swift", "UIImage+Resize.swift", "WidgetBundle.swift", "MemoryWidget.swift"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("sources = %v, want %v", names, want)
	}
	if len(widget.AssetCatalogs) != 1 || filepath.Base(widget.AssetCatalogs[0]) != "Assets.xcassets" {
		t.Errorf("asset catalogs = %v, want the target's own Assets.xcassets", widget.AssetCatalogs)
	}
	// A synced member ripley has no Linux compiler for is reported, not compiled.
	if len(widget.Uncompilable) != 1 || filepath.Base(widget.Uncompilable[0]) != "Legacy.storyboard" {
		t.Errorf("uncompilable = %v, want Legacy.storyboard reported", widget.Uncompilable)
	}
}

// membershipExceptions on a folder the target owns means "exclude". Compiling
// Info.plist would fail the build, and an entitlements file or a README is not a
// translation unit either.
func TestDiscoverAppExtensionsExcludesNonSources(t *testing.T) {
	extensions, err := discoverAppExtensions(extensionFixture(t, "Release"))
	if err != nil {
		t.Fatal(err)
	}
	widget := extensionByName(t, extensions, "WidgetExtension")
	for _, source := range widget.Sources {
		switch base := filepath.Base(source); base {
		case "Info.plist", "WidgetExtension.entitlements", "README.md", "Contents.json":
			t.Errorf("%s must not be compiled as a source", base)
		}
	}
}

// A target that lists its sources the classic per-PBXBuildFile way must resolve
// exactly those, and a file sitting in the target's directory that no build
// phase lists must stay out of the build entirely: this target owns no
// folder-synced group, so its MainInterface.storyboard is not a member.
func TestDiscoverAppExtensionsResolvesListedSourcesOnly(t *testing.T) {
	extensions, err := discoverAppExtensions(extensionFixture(t, "Release"))
	if err != nil {
		t.Fatal(err)
	}
	share := extensionByName(t, extensions, "ShareExtension")
	if len(share.Sources) != 1 || filepath.Base(share.Sources[0]) != "ShareViewController.swift" {
		t.Fatalf("sources = %v, want just ShareViewController.swift", share.Sources)
	}
	if len(share.Uncompilable) != 0 {
		t.Errorf("uncompilable = %v, want nothing: no build phase lists those files", share.Uncompilable)
	}
}

// An extension's bundle id is normally spelled with variables that only resolve
// through the project-level configuration, and it differs per configuration.
// Getting this wrong ships an extension iOS refuses to load, because an
// extension bundle id must be a child of the host app's.
func TestDiscoverAppExtensionsResolvesBundleIDPerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		configuration string
		widget        string
		share         string
	}{
		{"Release", "app.alextran.immich.Widget", "app.alextran.immich.ShareExtension"},
		{"Debug", "app.futo.immich.debug.Widget", "app.futo.immich.debug.ShareExtension"},
	} {
		extensions, err := discoverAppExtensions(extensionFixture(t, tc.configuration))
		if err != nil {
			t.Fatalf("%s: %v", tc.configuration, err)
		}
		if got := extensionByName(t, extensions, "WidgetExtension").BundleID; got != tc.widget {
			t.Errorf("%s WidgetExtension bundle id = %q, want %q", tc.configuration, got, tc.widget)
		}
		if got := extensionByName(t, extensions, "ShareExtension").BundleID; got != tc.share {
			t.Errorf("%s ShareExtension bundle id = %q, want %q", tc.configuration, got, tc.share)
		}
	}
}

// An extension declaring its own deployment target keeps it; one declaring none
// inherits a usable value rather than emitting an empty MinimumOSVersion.
func TestDiscoverAppExtensionsDeploymentTarget(t *testing.T) {
	extensions, err := discoverAppExtensions(extensionFixture(t, "Release"))
	if err != nil {
		t.Fatal(err)
	}
	if got := extensionByName(t, extensions, "WidgetExtension").MinOS; got != "17.0" {
		t.Errorf("WidgetExtension MinOS = %q, want its own 17.0", got)
	}
	if got := extensionByName(t, extensions, "ShareExtension").MinOS; got != "15.0" {
		t.Errorf("ShareExtension MinOS = %q, want the inherited 15.0", got)
	}
}

// A project with no extension target must change nothing for a simple app.
func TestDiscoverAppExtensionsIsEmptyWithoutExtensionTargets(t *testing.T) {
	config := extensionFixture(t, "Release")
	pbxPath := filepath.Join(config.XcodeProject, "project.pbxproj")
	data, err := os.ReadFile(pbxPath)
	if err != nil {
		t.Fatal(err)
	}
	plain := strings.ReplaceAll(string(data), `"com.apple.product-type.app-extension"`, `"com.apple.product-type.bundle.unit-test"`)
	if err := os.WriteFile(pbxPath, []byte(plain), 0o644); err != nil {
		t.Fatal(err)
	}
	extensions, err := discoverAppExtensions(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(extensions) != 0 {
		t.Fatalf("discovered %d extensions in a project that has none", len(extensions))
	}
}

// The .appex Info.plist must carry the target's own NSExtension dictionary
// (iOS uses it to decide what the extension is) with build settings expanded,
// and must declare itself an extension rather than an application.
func TestExtensionInfoPlistCarriesNSExtensionAndIdentity(t *testing.T) {
	extensions, err := discoverAppExtensions(extensionFixture(t, "Release"))
	if err != nil {
		t.Fatal(err)
	}
	widget := extensionByName(t, extensions, "WidgetExtension")
	info, err := extensionInfoPlist(widget)
	if err != nil {
		t.Fatal(err)
	}

	nsExtension, ok := info["NSExtension"].(map[string]any)
	if !ok {
		t.Fatalf("NSExtension = %#v, want a dictionary", info["NSExtension"])
	}
	if got := nsExtension["NSExtensionPointIdentifier"]; got != "com.apple.widgetkit-extension" {
		t.Errorf("NSExtensionPointIdentifier = %v", got)
	}
	if got := info["CFBundlePackageType"]; got != "XPC!" {
		t.Errorf("CFBundlePackageType = %v, want XPC! (APPL would not load from PlugIns)", got)
	}
	if got := info["CFBundleIdentifier"]; got != "app.alextran.immich.Widget" {
		t.Errorf("CFBundleIdentifier = %v", got)
	}
	if got := info["CFBundleExecutable"]; got != "WidgetExtension" {
		t.Errorf("CFBundleExecutable = %v", got)
	}
	if got := info["MinimumOSVersion"]; got != "17.0" {
		t.Errorf("MinimumOSVersion = %v, want the target's own 17.0", got)
	}
	// $(CUSTOM_GROUP_ID) only resolves through the project configuration; an
	// unexpanded value would put a literal "$(...)" in the shipped plist.
	if got := info["AppGroupId"]; got != "group.app.immich.share" {
		t.Errorf("AppGroupId = %v, want the expanded group id", got)
	}
}

// An extension whose plist has no NSExtension dictionary would be installed and
// never loaded, so it is refused rather than shipped as a silent no-op.
func TestExtensionInfoPlistRequiresNSExtension(t *testing.T) {
	config := extensionFixture(t, "Release")
	extensions, err := discoverAppExtensions(config)
	if err != nil {
		t.Fatal(err)
	}
	widget := extensionByName(t, extensions, "WidgetExtension")
	if err := os.WriteFile(widget.InfoPlist, []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>CFBundleName</key><string>Widget</string></dict></plist>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extensionInfoPlist(widget); err == nil {
		t.Fatal("expected a plist without NSExtension to be refused")
	}
}

// The bundle directory name is what the host app's PlugIns entry is looked up
// by, and it follows PRODUCT_NAME rather than the target name.
func TestAppExtensionBundleNameFollowsProductName(t *testing.T) {
	if got := (appExtension{ProductName: "Widget"}).BundleName(); got != "Widget.appex" {
		t.Fatalf("BundleName() = %q", got)
	}
}

// Imports are the only evidence an extension target gives about the frameworks
// it needs. A framework the SDK ships is linked by name, one built for this
// project by path so its -F is added, a CocoaPods static-library pod links its
// archive, and the modules that resolve without a link flag must not reach the
// linker.
func TestExtensionFrameworksResolvesImports(t *testing.T) {
	sdk := t.TempDir()
	for _, name := range []string{"WidgetKit", "SwiftUI", "Social"} {
		if err := os.MkdirAll(filepath.Join(sdk, "System", "Library", "Frameworks", name+".framework"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "W.swift")
	if err := os.WriteFile(source, []byte("import WidgetKit\nimport SwiftUI\nimport Foundation\nimport os\nimport share_handler_ios\nimport share_handler_ios_models\nimport NotAThing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	built := map[string]string{"share_handler_ios": "/build/share_handler_ios.framework"}
	pods := map[string]ios.PodModule{
		"share_handler_ios_models": {
			Name:       "share_handler_ios_models",
			ModuleName: "share_handler_ios_models",
			Archive:    "/build/libshare_handler_ios_models.a",
		},
	}

	inputs := ios.HostInputs{}
	got := extensionFrameworks(appExtension{Sources: []string{source}}, sdk, built, pods, &inputs)
	want := []string{"SwiftUI", "WidgetKit", "/build/share_handler_ios.framework"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frameworks = %v, want %v", got, want)
	}
	if len(inputs.Objects) != 1 || inputs.Objects[0] != "/build/libshare_handler_ios_models.a" {
		t.Fatalf("linked archives = %v, want the pod's static library", inputs.Objects)
	}
}

func TestExtensionFrameworksPrefersBuiltPodFrameworkOverStaticArchive(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "Share.swift")
	if err := os.WriteFile(source, []byte("import share_handler_ios_models\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	framework := "/app/Frameworks/share_handler_ios_models.framework"
	built := map[string]string{"share_handler_ios_models": framework}
	pods := map[string]ios.PodModule{
		"share_handler_ios_models": {
			Name:         "share_handler_ios_models",
			ModuleName:   "share_handler_ios_models",
			Archive:      "/build/libshare_handler_ios_models.a",
			ModuleMap:    "/build/share_handler_ios_models.modulemap",
			Dependencies: nil,
		},
	}
	inputs := ios.HostInputs{}
	got := extensionFrameworks(appExtension{Sources: []string{source}}, "", built, pods, &inputs)
	if len(got) != 1 || got[0] != framework {
		t.Fatalf("frameworks = %v, want [%s]", got, framework)
	}
	if len(inputs.Objects) != 0 {
		t.Fatalf("static pod archives = %v, want none when a framework product exists", inputs.Objects)
	}
	if len(inputs.HeaderSearchPaths) != 0 {
		t.Fatalf("loose pod header/module paths = %v, want none for framework product", inputs.HeaderSearchPaths)
	}
}

func TestExtensionFrameworksUsesStaticPodFrameworkClosureAndLinkerFlags(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "Share.swift")
	if err := os.WriteFile(source, []byte("import TopStatic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	top := filepath.Join(dir, "TopStatic.framework")
	dep := filepath.Join(dir, "DepStatic.framework")
	pods := map[string]ios.PodModule{
		"TopStatic": {
			Name:            "TopStatic",
			ModuleName:      "TopStatic",
			Framework:       top,
			StaticFramework: true,
			LinkerFlags:     []string{"-force_load", "/build/top-extra.a"},
			Dependencies:    []string{"DepStatic"},
		},
		"DepStatic": {
			Name:       "DepStatic",
			ModuleName: "DepStatic",
			Framework:  dep,
		},
	}
	inputs := ios.HostInputs{}
	got := extensionFrameworks(appExtension{Sources: []string{source}}, "", nil, pods, &inputs)
	want := []string{top, dep}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("framework closure = %#v, want %#v", got, want)
	}
	if strings.Join(inputs.LinkerFlags, "\x00") != "-ObjC\x00-force_load\x00/build/top-extra.a" {
		t.Fatalf("extension linker flags = %#v, want -ObjC plus pod target force-load flags", inputs.LinkerFlags)
	}
	if len(inputs.Objects) != 0 {
		t.Fatalf("static framework pod should link by framework identity, got loose objects %#v", inputs.Objects)
	}
	if len(inputs.HeaderSearchPaths) != 0 {
		t.Fatalf("static framework pod should use framework modules, got loose headers %#v", inputs.HeaderSearchPaths)
	}
}
