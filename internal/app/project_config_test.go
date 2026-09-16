package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ripley/internal/ios/storyboard"
)

func TestResolveIOSProjectUsesXcodeProjectAsSourceOfTruth(t *testing.T) {
	root := t.TempDir()
	iosRoot := filepath.Join(root, "ios")
	projectPath := filepath.Join(iosRoot, "CustomApp.xcodeproj")
	flutterRoot := filepath.Join(root, "fake-flutter")

	mustWriteTestFile(t, filepath.Join(root, "pubspec.yaml"), "name: source_truth\nversion: 2.3.4+56\n")
	mustWriteTestFile(t, filepath.Join(root, "lib", "prod.dart"), "void main() {}\n")
	mustWriteTestFile(t, filepath.Join(flutterRoot, "bin", "cache", "flutter.version.json"), `{"engineRevision":"engine","dartSdkVersion":"3.0.0"}`)
	mustWriteTestFile(t, filepath.Join(root, ".dart_tool", "package_config.json"), fmt.Sprintf(`{"packages":[{"name":"flutter","rootUri":%q}]}`, "file://"+filepath.ToSlash(filepath.Join(flutterRoot, "packages", "flutter"))))
	mustWriteTestFile(t, filepath.Join(iosRoot, "Flutter", "Generated.xcconfig"), "FLUTTER_TARGET=lib/prod.dart\nAPP_DISPLAY_NAME=Source Truth\n")
	mustWriteTestFile(t, filepath.Join(iosRoot, "Flutter", "Release.xcconfig"), "#include \"Generated.xcconfig\"\nCUSTOM_SUFFIX=prod\n")
	mustWriteTestFile(t, filepath.Join(iosRoot, "Native", "AppInfo.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>$(PRODUCT_BUNDLE_IDENTIFIER)</string>
<key>CFBundleExecutable</key><string>$(EXECUTABLE_NAME)</string>
<key>CFBundleName</key><string>$(PRODUCT_NAME)</string>
<key>CFBundleDisplayName</key><string>$(APP_DISPLAY_NAME)</string>
<key>CFBundleShortVersionString</key><string>$(FLUTTER_BUILD_NAME)</string>
<key>CFBundleVersion</key><string>$(FLUTTER_BUILD_NUMBER)</string>
</dict></plist>`)
	mustWriteTestFile(t, filepath.Join(iosRoot, "Native", "App.entitlements"), `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict/></plist>`)
	mustWriteTestFile(t, filepath.Join(iosRoot, "Native", "App-Bridging-Header.h"), "")
	mustWriteTestFile(t, filepath.Join(iosRoot, "Native", "AppDelegate.swift"), "import UIKit\n@main class AppDelegate: UIResponder {}\n")
	mustWriteTestFile(t, filepath.Join(iosRoot, "Native", "Sub", "Helper.swift"), "struct Helper {}\n")

	const (
		projectID       = "000000000000000000000001"
		targetID        = "000000000000000000000002"
		projectListID   = "000000000000000000000003"
		targetListID    = "000000000000000000000004"
		projectConfigID = "000000000000000000000005"
		targetConfigID  = "000000000000000000000006"
		xcconfigID      = "000000000000000000000007"
	)
	pbx := fmt.Sprintf(`// !$*UTF8*$!
{
objects = {
	%s /* Project object */ = {
		isa = PBXProject;
		buildConfigurationList = %s /* Build configuration list for PBXProject "CustomApp" */;
		developmentRegion = en;
	};
	%s /* SourceTruthTarget */ = {
		isa = PBXNativeTarget;
		buildConfigurationList = %s /* Build configuration list for PBXNativeTarget "SourceTruthTarget" */;
		buildPhases = (
			EE0000000000000000000001 /* Sources */,
		);
		name = SourceTruthTarget;
		productName = IgnoredByResolvedProductName;
		productType = "com.apple.product-type.application";
	};
	EE0000000000000000000001 /* Sources */ = {
		isa = PBXSourcesBuildPhase;
		buildActionMask = 2147483647;
		files = (
			EE0000000000000000000002 /* AppDelegate.swift in Sources */,
			EE0000000000000000000003 /* Helper.swift in Sources */,
		);
		runOnlyForDeploymentPostprocessing = 0;
	};
	EE0000000000000000000002 /* AppDelegate.swift in Sources */ = {isa = PBXBuildFile; fileRef = EE0000000000000000000004 /* AppDelegate.swift */; };
	EE0000000000000000000003 /* Helper.swift in Sources */ = {isa = PBXBuildFile; fileRef = EE0000000000000000000005 /* Helper.swift */; };
	EE0000000000000000000004 /* AppDelegate.swift */ = {
		isa = PBXFileReference;
		lastKnownFileType = sourcecode.swift;
		path = AppDelegate.swift;
		sourceTree = "<group>";
	};
	EE0000000000000000000005 /* Helper.swift */ = {
		isa = PBXFileReference;
		lastKnownFileType = sourcecode.swift;
		path = Sub/Helper.swift;
		sourceTree = "<group>";
	};
	EE0000000000000000000006 /* Native */ = {
		isa = PBXGroup;
		children = (
			EE0000000000000000000004 /* AppDelegate.swift */,
			EE0000000000000000000005 /* Helper.swift */,
		);
		path = Native;
		sourceTree = "<group>";
	};
	%s /* Release.xcconfig */ = {
		isa = PBXFileReference;
		path = Flutter/Release.xcconfig;
		sourceTree = "<group>";
	};
	%s /* Release */ = {
		isa = XCBuildConfiguration;
		buildSettings = {
			IPHONEOS_DEPLOYMENT_TARGET = 14.4;
		};
		name = Release;
	};
	%s /* Release */ = {
		isa = XCBuildConfiguration;
		baseConfigurationReference = %s /* Release.xcconfig */;
		buildSettings = {
			CODE_SIGN_ENTITLEMENTS = Native/App.entitlements;
			INFOPLIST_FILE = Native/AppInfo.plist;
			INFOPLIST_KEY_CFBundleDisplayName = "$(APP_DISPLAY_NAME)";
			IPHONEOS_DEPLOYMENT_TARGET = 16.2;
			PRODUCT_BUNDLE_IDENTIFIER = dev.example.$(CUSTOM_SUFFIX);
			PRODUCT_MODULE_NAME = SourceTruthModule;
			PRODUCT_NAME = SourceTruth;
			SWIFT_OBJC_BRIDGING_HEADER = Native/App-Bridging-Header.h;
			TARGETED_DEVICE_FAMILY = "1,2";
		};
		name = Release;
	};
	%s /* Build configuration list for PBXProject "CustomApp" */ = {
		isa = XCConfigurationList;
		buildConfigurations = (
			%s /* Release */,
		);
		defaultConfigurationName = Release;
	};
	%s /* Build configuration list for PBXNativeTarget "SourceTruthTarget" */ = {
		isa = XCConfigurationList;
		buildConfigurations = (
			%s /* Release */,
		);
		defaultConfigurationName = Release;
	};
};
rootObject = %s /* Project object */;
}
`, projectID, projectListID, targetID, targetListID, xcconfigID, projectConfigID, targetConfigID, xcconfigID, projectListID, projectConfigID, targetListID, targetConfigID, projectID)
	mustWriteTestFile(t, filepath.Join(projectPath, "project.pbxproj"), pbx)

	resolved, err := resolveIOSProject(root, "release", "", "")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]string{
		"target":         {resolved.TargetName, "SourceTruthTarget"},
		"product":        {resolved.ProductName, "SourceTruth"},
		"executable":     {resolved.ExecutableName, "SourceTruth"},
		"module":         {resolved.ModuleName, "SourceTruthModule"},
		"bundle":         {resolved.BundleID, "dev.example.prod"},
		"minimum iOS":    {resolved.MinOS, "16.2"},
		"configuration":  {resolved.Configuration, "Release"},
		"build name":     {resolved.BuildName, "2.3.4"},
		"build number":   {resolved.BuildNumber, "56"},
		"Flutter root":   {resolved.FlutterRoot, flutterRoot},
		"Flutter target": {resolved.FlutterTarget, filepath.Join(root, "lib", "prod.dart")},
		"source dir":     {resolved.SourceDir, filepath.Join(iosRoot, "Native")},
	}
	for name, check := range checks {
		if check[0] != check[1] {
			t.Errorf("%s: got %q, want %q", name, check[0], check[1])
		}
	}

	// An explicit Flutter-compatible -t/--target wins over the value inherited
	// from Generated.xcconfig, and shell phases receive the same relative value.
	mustWriteTestFile(t, filepath.Join(root, "lib", "cli.dart"), "void main() {}\n")
	overridden, err := resolveIOSProject(root, "release", "", "lib/cli.dart")
	if err != nil {
		t.Fatalf("resolve with CLI Flutter target: %v", err)
	}
	if got, want := overridden.FlutterTarget, filepath.Join(root, "lib", "cli.dart"); got != want {
		t.Fatalf("CLI Flutter target = %q, want %q", got, want)
	}
	if got := overridden.BuildSettings["FLUTTER_TARGET"]; got != "lib/cli.dart" {
		t.Fatalf("FLUTTER_TARGET setting = %q, want project-relative CLI override", got)
	}
	wantSources := []string{
		filepath.Join(iosRoot, "Native", "AppDelegate.swift"),
		filepath.Join(iosRoot, "Native", "Sub", "Helper.swift"),
	}
	if !equalStringSlices(resolved.SourceFiles, wantSources) {
		t.Errorf("SourceFiles = %#v, want %#v", resolved.SourceFiles, wantSources)
	}

	info, _, err := loadProjectInfoPlist(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if got := info["CFBundleDisplayName"]; got != "Source Truth" {
		t.Errorf("CFBundleDisplayName = %#v, want Source Truth", got)
	}
	if got := info["CFBundleIdentifier"]; got != "dev.example.prod" {
		t.Errorf("CFBundleIdentifier = %#v, want dev.example.prod", got)
	}
	if got := info["MinimumOSVersion"]; got != "16.2" {
		t.Errorf("MinimumOSVersion = %#v, want 16.2", got)
	}
	launchScreen, ok := info["UILaunchScreen"].(map[string]any)
	if !ok || len(launchScreen) != 0 {
		t.Errorf("UILaunchScreen = %#v, want empty dictionary fallback", info["UILaunchScreen"])
	}
}

func TestStageLaunchStoryboardKeepsStoryboardPlistMode(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "Runner")
	launchPath := filepath.Join(sourceDir, "Base.lproj", "LaunchScreen.storyboard")
	data, err := os.ReadFile(filepath.Join("..", "ios", "storyboard", "testdata", "ThreeImageBranding.storyboard"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, launchPath, string(data))
	launch, err := storyboard.ParseLaunchScreen(launchPath)
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(t.TempDir(), "App.app")
	if err := os.MkdirAll(filepath.Join(app, "Base.lproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Mirror copyIOSResources so stageLaunchStoryboard also proves it removes the
	// source XML Xcode would not ship next to the compiled resource.
	mustWriteTestFile(t, filepath.Join(app, "Base.lproj", "LaunchScreen.storyboard"), string(data))
	if err := stageLaunchStoryboard(iosProject{SourceDir: sourceDir}, app, launch); err != nil {
		t.Fatal(err)
	}
	compiled := filepath.Join(app, "Base.lproj", "LaunchScreen.storyboardc")
	for _, file := range []string{
		"Info.plist",
		"UIViewController-01J-lp-oVM.nib",
		"01J-lp-oVM-view-Ze5-6b-2t3.nib",
	} {
		if st, err := os.Stat(filepath.Join(compiled, file)); err != nil || st.IsDir() {
			t.Fatalf("compiled launch resource %s missing: %v", file, err)
		}
	}
	if _, err := os.Stat(filepath.Join(app, "Base.lproj", "LaunchScreen.storyboard")); !os.IsNotExist(err) {
		t.Fatalf("source launch storyboard still staged: %v", err)
	}
	info := map[string]any{"UILaunchStoryboardName": "LaunchScreen"}
	ensurePlistLaunchScreen(info)
	if _, exists := info["UILaunchScreen"]; exists {
		t.Fatalf("compiled storyboard was shadowed by UILaunchScreen fallback: %#v", info)
	}
}

func TestStageMainStoryboardPreservesUIKitStartupSemantics(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "Runner")
	mainPath := filepath.Join(sourceDir, "Base.lproj", "Main.storyboard")
	data, err := os.ReadFile(filepath.Join("..", "ios", "storyboard", "testdata", "FlutterMainInterface.storyboard"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, mainPath, string(data))
	app := filepath.Join(t.TempDir(), "App.app")
	if err := os.MkdirAll(filepath.Join(app, "Base.lproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(app, "Base.lproj", "Main.storyboard"), string(data))
	info := map[string]any{"UIMainStoryboardFile": "Main"}
	if err := stageMainStoryboard(iosProject{SourceDir: sourceDir}, app, info); err != nil {
		t.Fatal(err)
	}
	if got := info["UIMainStoryboardFile"]; got != "Main" {
		t.Fatalf("UIMainStoryboardFile = %#v, want Main", got)
	}
	compiled := filepath.Join(app, "Base.lproj", "Main.storyboardc")
	for _, file := range []string{
		"Info.plist",
		"UIViewController-BYZ-38-t0r.nib",
		"BYZ-38-t0r-view-8bC-Xf-vdC.nib",
	} {
		if st, err := os.Stat(filepath.Join(compiled, file)); err != nil || st.IsDir() {
			t.Fatalf("compiled main resource %s missing: %v", file, err)
		}
	}
	if _, err := os.Stat(filepath.Join(app, "Base.lproj", "Main.storyboard")); !os.IsNotExist(err) {
		t.Fatalf("source main storyboard still staged: %v", err)
	}
}

func TestStageMainStoryboardFallsBackForRichInterface(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "Runner")
	mainPath := filepath.Join(sourceDir, "Base.lproj", "Main.storyboard")
	data, err := os.ReadFile(filepath.Join("..", "ios", "storyboard", "testdata", "CustomizedLaunchScreen.storyboard"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, mainPath, string(data))
	app := filepath.Join(t.TempDir(), "App.app")
	info := map[string]any{"UIMainStoryboardFile": "Main"}
	if err := stageMainStoryboard(iosProject{SourceDir: sourceDir}, app, info); err != nil {
		t.Fatal(err)
	}
	if _, exists := info["UIMainStoryboardFile"]; exists {
		t.Fatalf("unsupported main storyboard remained enabled: %#v", info)
	}
	if _, err := os.Stat(filepath.Join(app, "Base.lproj", "Main.storyboardc")); !os.IsNotExist(err) {
		t.Fatalf("unsupported main storyboard produced output: %v", err)
	}
}

func TestEnsurePlistLaunchScreenPreservesProjectConfiguration(t *testing.T) {
	custom := map[string]any{"UIImageName": "LaunchLogo"}
	info := map[string]any{"UILaunchScreen": custom}
	ensurePlistLaunchScreen(info)
	got, ok := info["UILaunchScreen"].(map[string]any)
	if !ok || got["UIImageName"] != "LaunchLogo" {
		t.Fatalf("UILaunchScreen = %#v, want project configuration preserved", info["UILaunchScreen"])
	}

	multi := map[string]any{"UIDefaultLaunchScreen": "default"}
	info = map[string]any{"UILaunchScreens": multi}
	ensurePlistLaunchScreen(info)
	if _, exists := info["UILaunchScreen"]; exists {
		t.Fatalf("UILaunchScreen fallback added despite project UILaunchScreens: %#v", info)
	}

	info = map[string]any{"UILaunchStoryboardName": "LaunchScreen"}
	ensurePlistLaunchScreen(info)
	if _, exists := info["UILaunchScreen"]; exists {
		t.Fatalf("UILaunchScreen fallback added despite compiled launch storyboard: %#v", info)
	}
}

func TestSelectBuildConfigurationUsesFlavor(t *testing.T) {
	configs := []xcodeBuildConfiguration{{Name: "Release"}, {Name: "Release-staging"}, {Name: "Profile-staging"}}
	got, err := selectBuildConfiguration(configs, "release", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Release-staging" {
		t.Fatalf("got %q, want Release-staging", got.Name)
	}
}

// targetSourceFilesProject is a legacy per-file project shaped like immich's:
// the Runner group holds AppDelegate.swift plus subdirectory groups (Images,
// Background) whose members are only reachable through the Sources phase, an
// Info.plist the phase does not list, a generated file anchored at SOURCE_ROOT,
// a nested group chain (Images/Nested), a PBXVariantGroup and a framework
// anchored in BUILT_PRODUCTS_DIR. A second target owns a Sources phase listing a
// file the first target must not see, and a third target has no Sources phase.
const targetSourceFilesProject = `// !$*UTF8*$!
{
objects = {
	AA0000000000000000000001 /* Runner */ = {
		isa = PBXNativeTarget;
		buildPhases = (
			AA0000000000000000000010 /* Resources */,
			AA0000000000000000000011 /* Sources */,
		);
		name = Runner;
		productType = "com.apple.product-type.application";
	};
	AA0000000000000000000002 /* ShareExtension */ = {
		isa = PBXNativeTarget;
		buildPhases = (
			AA0000000000000000000012 /* Sources */,
		);
		name = ShareExtension;
		productType = "com.apple.product-type.app-extension";
	};
	AA0000000000000000000003 /* WidgetExtension */ = {
		isa = PBXNativeTarget;
		buildPhases = (
			AA0000000000000000000010 /* Resources */,
		);
		name = WidgetExtension;
		productType = "com.apple.product-type.app-extension";
	};
	AA0000000000000000000010 /* Resources */ = {
		isa = PBXResourcesBuildPhase;
		files = (
			AA0000000000000000000020 /* Info.plist in Resources */,
		);
	};
	AA0000000000000000000011 /* Sources */ = {
		isa = PBXSourcesBuildPhase;
		files = (
			AA0000000000000000000021 /* AppDelegate.swift in Sources */,
			AA0000000000000000000022 /* Thumbhash.swift in Sources */,
			AA0000000000000000000023 /* BackgroundWorker.swift in Sources */,
			AA0000000000000000000024 /* Deep.swift in Sources */,
			AA0000000000000000000025 /* GeneratedPluginRegistrant.m in Sources */,
			AA0000000000000000000026 /* Localized.strings in Sources */,
			AA0000000000000000000027 /* Pods_Runner.framework in Sources */,
			AA0000000000000000000028 /* FlutterGeneratedPluginSwiftPackage in Sources */,
			AA0000000000000000000021 /* AppDelegate.swift in Sources */,
		);
	};
	AA0000000000000000000012 /* Sources */ = {
		isa = PBXSourcesBuildPhase;
		files = (
			AA0000000000000000000029 /* ShareViewController.swift in Sources */,
		);
	};
	AA0000000000000000000020 /* Info.plist in Resources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000040 /* Info.plist */; };
	AA0000000000000000000021 /* AppDelegate.swift in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000041 /* AppDelegate.swift */; };
	AA0000000000000000000022 /* Thumbhash.swift in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000042 /* Thumbhash.swift */; };
	AA0000000000000000000023 /* BackgroundWorker.swift in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000043 /* BackgroundWorker.swift */; };
	AA0000000000000000000024 /* Deep.swift in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000044 /* Deep.swift */; };
	AA0000000000000000000025 /* GeneratedPluginRegistrant.m in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000045 /* GeneratedPluginRegistrant.m */; };
	AA0000000000000000000026 /* Localized.strings in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000046 /* Localized.strings */; };
	AA0000000000000000000027 /* Pods_Runner.framework in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000047 /* Pods_Runner.framework */; };
	AA0000000000000000000028 /* FlutterGeneratedPluginSwiftPackage in Sources */ = {isa = PBXBuildFile; productRef = AA0000000000000000000060 /* FlutterGeneratedPluginSwiftPackage */; };
	AA0000000000000000000029 /* ShareViewController.swift in Sources */ = {isa = PBXBuildFile; fileRef = AA0000000000000000000048 /* ShareViewController.swift */; };
	AA0000000000000000000040 /* Info.plist */ = {isa = PBXFileReference; path = Info.plist; sourceTree = "<group>"; };
	AA0000000000000000000041 /* AppDelegate.swift */ = {isa = PBXFileReference; path = AppDelegate.swift; sourceTree = "<group>"; };
	AA0000000000000000000042 /* Thumbhash.swift */ = {isa = PBXFileReference; path = Thumbhash.swift; sourceTree = "<group>"; };
	AA0000000000000000000043 /* BackgroundWorker.swift */ = {isa = PBXFileReference; path = BackgroundWorker.swift; sourceTree = "<group>"; };
	AA0000000000000000000044 /* Deep.swift */ = {isa = PBXFileReference; path = Deep.swift; sourceTree = "<group>"; };
	AA0000000000000000000045 /* GeneratedPluginRegistrant.m */ = {isa = PBXFileReference; name = GeneratedPluginRegistrant.m; path = "Runner/Generated/GeneratedPluginRegistrant.m"; sourceTree = SOURCE_ROOT; };
	AA0000000000000000000047 /* Pods_Runner.framework */ = {isa = PBXFileReference; path = Pods_Runner.framework; sourceTree = BUILT_PRODUCTS_DIR; };
	AA0000000000000000000048 /* ShareViewController.swift */ = {isa = PBXFileReference; path = ShareViewController.swift; sourceTree = "<group>"; };
	AA0000000000000000000046 /* Localized.strings */ = {
		isa = PBXVariantGroup;
		children = (
			AA0000000000000000000049 /* en */,
			AA000000000000000000004A /* de */,
		);
		name = Localized.strings;
		sourceTree = "<group>";
	};
	AA0000000000000000000049 /* en */ = {isa = PBXFileReference; name = en; path = en.lproj/Localized.strings; sourceTree = "<group>"; };
	AA000000000000000000004A /* de */ = {isa = PBXFileReference; name = de; path = de.lproj/Localized.strings; sourceTree = "<group>"; };
	AA0000000000000000000050 /* Runner */ = {
		isa = PBXGroup;
		children = (
			AA0000000000000000000040 /* Info.plist */,
			AA0000000000000000000041 /* AppDelegate.swift */,
			AA0000000000000000000046 /* Localized.strings */,
			AA0000000000000000000051 /* Images */,
			AA0000000000000000000053 /* Background */,
		);
		path = Runner;
		sourceTree = "<group>";
	};
	AA0000000000000000000051 /* Images */ = {
		isa = PBXGroup;
		children = (
			AA0000000000000000000042 /* Thumbhash.swift */,
			AA0000000000000000000052 /* Nested */,
		);
		path = Images;
		sourceTree = "<group>";
	};
	AA0000000000000000000052 /* Nested */ = {
		isa = PBXGroup;
		children = (
			AA0000000000000000000044 /* Deep.swift */,
		);
		path = Nested;
		sourceTree = "<group>";
	};
	AA0000000000000000000053 /* Background */ = {
		isa = PBXGroup;
		children = (
			AA0000000000000000000043 /* BackgroundWorker.swift */,
		);
		path = Background;
		sourceTree = "<group>";
	};
	AA0000000000000000000054 /* ShareExtension */ = {
		isa = PBXGroup;
		children = (
			AA0000000000000000000048 /* ShareViewController.swift */,
		);
		path = ShareExtension;
		sourceTree = "<group>";
	};
	AA0000000000000000000055 = {
		isa = PBXGroup;
		children = (
			AA0000000000000000000050 /* Runner */,
			AA0000000000000000000054 /* ShareExtension */,
			AA0000000000000000000047 /* Pods_Runner.framework */,
		);
		sourceTree = "<group>";
	};
};
}
`

func TestTargetSourceFilesResolvesSourcesPhase(t *testing.T) {
	objects, err := parsePBXObjects(targetSourceFilesProject)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot := filepath.Join(t.TempDir(), "ios")
	target := xcodeTarget{
		ID:          "AA0000000000000000000001",
		Name:        "Runner",
		BuildPhases: []string{"AA0000000000000000000010", "AA0000000000000000000011"},
	}

	got, err := targetSourceFiles(objects, target, srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(srcRoot, "Runner", "AppDelegate.swift"),
		filepath.Join(srcRoot, "Runner", "Images", "Thumbhash.swift"),
		filepath.Join(srcRoot, "Runner", "Background", "BackgroundWorker.swift"),
		filepath.Join(srcRoot, "Runner", "Images", "Nested", "Deep.swift"),
		filepath.Join(srcRoot, "Runner", "Generated", "GeneratedPluginRegistrant.m"),
		filepath.Join(srcRoot, "Runner", "en.lproj", "Localized.strings"),
		filepath.Join(srcRoot, "Runner", "de.lproj", "Localized.strings"),
	}
	if !equalStringSlices(got, want) {
		t.Fatalf("targetSourceFiles =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A top-level glob of the target directory would return AppDelegate.swift and
// nothing else while also sweeping in files no target compiles. Assert both
// halves: subdirectory sources are present, and files outside the phase are not.
func TestTargetSourceFilesRejectsDirectoryGlob(t *testing.T) {
	objects, err := parsePBXObjects(targetSourceFilesProject)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot := filepath.Join(t.TempDir(), "ios")
	target := xcodeTarget{
		ID:          "AA0000000000000000000001",
		Name:        "Runner",
		BuildPhases: []string{"AA0000000000000000000010", "AA0000000000000000000011"},
	}
	got, err := targetSourceFiles(objects, target, srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	index := make(map[string]bool, len(got))
	for _, path := range got {
		index[path] = true
	}

	for _, nested := range []string{
		filepath.Join(srcRoot, "Runner", "Images", "Thumbhash.swift"),
		filepath.Join(srcRoot, "Runner", "Background", "BackgroundWorker.swift"),
		filepath.Join(srcRoot, "Runner", "Images", "Nested", "Deep.swift"),
	} {
		if !index[nested] {
			t.Errorf("subdirectory source %s missing; a top-level glob of the target directory cannot see it", nested)
		}
	}
	for _, foreign := range []string{
		filepath.Join(srcRoot, "ShareExtension", "ShareViewController.swift"),
		filepath.Join(srcRoot, "Runner", "Info.plist"),
		filepath.Join(srcRoot, "Pods_Runner.framework"),
	} {
		if index[foreign] {
			t.Errorf("%s is not in the target's Sources phase but was returned", foreign)
		}
	}
}

func TestTargetSourceFilesOtherTargetsAndMissingPhase(t *testing.T) {
	objects, err := parsePBXObjects(targetSourceFilesProject)
	if err != nil {
		t.Fatal(err)
	}
	srcRoot := filepath.Join(t.TempDir(), "ios")

	extension := xcodeTarget{
		ID:          "AA0000000000000000000002",
		Name:        "ShareExtension",
		BuildPhases: []string{"AA0000000000000000000012"},
	}
	got, err := targetSourceFiles(objects, extension, srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(srcRoot, "ShareExtension", "ShareViewController.swift")}
	if !equalStringSlices(got, want) {
		t.Errorf("ShareExtension sources = %#v, want %#v", got, want)
	}

	widget := xcodeTarget{
		ID:          "AA0000000000000000000003",
		Name:        "WidgetExtension",
		BuildPhases: []string{"AA0000000000000000000010"},
	}
	got, err = targetSourceFiles(objects, widget, srcRoot)
	if err != nil {
		t.Fatalf("target with no Sources phase: %v", err)
	}
	if got != nil {
		t.Errorf("target with no Sources phase = %#v, want nil", got)
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func mustWriteTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPBXFieldParsesCompactObject(t *testing.T) {
	body := `isa = PBXFileReference; lastKnownFileType = text.xcconfig; name = Release.xcconfig; path = Flutter/Release.xcconfig; sourceTree = "<group>"; `
	if got := pbxField(body, "path"); got != "Flutter/Release.xcconfig" {
		t.Fatalf("path = %q", got)
	}
	if got := pbxField(body, "name"); got != "Release.xcconfig" {
		t.Fatalf("name = %q", got)
	}
	if got := pbxQuotedField(body, "sourceTree"); got != "<group>" {
		t.Fatalf("sourceTree = %q", got)
	}
}

// `Pods/Target Support Files/Pods-<Target>/*.xcconfig` is generated by
// `pod install` and is gitignored, so a fresh clone never has it. ripley resolves
// pods itself and supplies those build settings, so the reference must not fail
// the build. localsend's ShareExtension is exactly this shape.
func TestResolvePBXFileReferenceToleratesMissingGeneratedPodXCConfig(t *testing.T) {
	iosRoot := t.TempDir()
	const id = "AAA"
	objects := map[string]pbxObject{
		id: {ID: id, Body: `isa = PBXFileReference; lastKnownFileType = text.xcconfig; name = "Pods-ShareExtension.release.xcconfig"; path = "Target Support Files/Pods-ShareExtension/Pods-ShareExtension.release.xcconfig"; sourceTree = "<group>"; `},
	}
	path, err := resolvePBXFileReference(iosRoot, objects, id)
	if err != nil {
		t.Fatalf("missing CocoaPods-generated xcconfig must not fail the build: %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty so no xcconfig is loaded", path)
	}
}

// A project-authored xcconfig that the project references and does not ship is a
// real error: its settings are not reconstructible, so silently continuing would
// build with wrong identity/flags.
func TestResolvePBXFileReferenceStillFailsForMissingProjectXCConfig(t *testing.T) {
	iosRoot := t.TempDir()
	const id = "BBB"
	objects := map[string]pbxObject{
		id: {ID: id, Body: `isa = PBXFileReference; lastKnownFileType = text.xcconfig; name = Signing.xcconfig; path = Signing.xcconfig; sourceTree = "<group>"; `},
	}
	if _, err := resolvePBXFileReference(iosRoot, objects, id); err == nil {
		t.Fatal("a missing project-authored xcconfig must be reported")
	}
}

func TestApplyBundleIDOverridePreservesOriginalIdentity(t *testing.T) {
	config := iosProject{
		BundleID:         "org.example.App",
		OriginalBundleID: "org.example.App",
		BuildSettings:    map[string]string{"PRODUCT_BUNDLE_IDENTIFIER": "org.example.App"},
	}
	applyBundleIDOverride(&config, "dev.device.corpus")
	if config.BundleID != "dev.device.corpus" {
		t.Fatalf("BundleID = %q", config.BundleID)
	}
	if config.OriginalBundleID != "org.example.App" {
		t.Fatalf("OriginalBundleID = %q", config.OriginalBundleID)
	}
	if got := config.BuildSettings["PRODUCT_BUNDLE_IDENTIFIER"]; got != "dev.device.corpus" {
		t.Fatalf("PRODUCT_BUNDLE_IDENTIFIER = %q", got)
	}
	if got := overriddenChildBundleID(config, "org.example.App.NotificationService"); got != "dev.device.corpus.NotificationService" {
		t.Fatalf("child bundle id = %q", got)
	}
	if got := overriddenChildBundleID(config, "com.vendor.UnrelatedExtension"); got != "com.vendor.UnrelatedExtension" {
		t.Fatalf("unrelated child bundle id changed to %q", got)
	}
}
