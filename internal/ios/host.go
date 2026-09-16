package ios

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"ripley/internal/toolchain"
)

const libSystemTBD = `--- !tapi-tbd
tbd-version: 4
targets: [ arm64-ios ]
install-name: /usr/lib/libSystem.B.dylib
exports:
  - targets: [ arm64-ios ]
    symbols: [ _exit, dyld_stub_binder, _dyld_stub_binder ]
...
`

const libObjCTBD = `--- !tapi-tbd
tbd-version: 4
targets: [ arm64-ios ]
install-name: /usr/lib/libobjc.A.dylib
exports:
  - targets: [ arm64-ios ]
    symbols: [ _objc_getClass, _objc_msgSend, _objc_msgSendSuper,
               _objc_allocateClassPair, _objc_registerClassPair,
               _class_addMethod, _sel_registerName ]
...
`

const uiKitTBD = `--- !tapi-tbd
tbd-version: 4
targets: [ arm64-ios ]
install-name: /System/Library/Frameworks/UIKit.framework/UIKit
exports:
  - targets: [ arm64-ios ]
    symbols: [ _UIApplicationMain ]
    objc-classes: [ UIApplication, UIResponder, UIViewController ]
...
`

const foundationTBD = `--- !tapi-tbd
tbd-version: 4
targets: [ arm64-ios ]
install-name: /System/Library/Frameworks/Foundation.framework/Foundation
exports:
  - targets: [ arm64-ios ]
    symbols: [ _NSStringFromClass ]
...
`

const hostShimC = `extern int UIApplicationMain(int, char**, void*, void*);
extern void* NSStringFromClass(void*);
extern void* objc_getClass(const char*);
extern void* objc_allocateClassPair(void*, const char*, unsigned long);
extern void objc_registerClassPair(void*);
extern void class_addMethod(void*, void*, void*, const char*);
extern void* sel_registerName(const char*);
extern void* objc_msgSend(void*, void*, ...);
extern void* objc_msgSendSuper(void*, void*, ...);

struct objc_super { void* receiver; void* super_class; };

static int didFinish(void* self, void* _cmd, void* app, void* opts) {
  void* reg = objc_getClass("GeneratedPluginRegistrant");
  if (reg) {
    void* sel = sel_registerName("registerWithRegistry:");
    ((void*(*)(void*,void*,void*))objc_msgSend)(reg, sel, self);
  }
  struct objc_super sup = { self, objc_getClass("FlutterAppDelegate") };
  void* ssel = sel_registerName("application:didFinishLaunchingWithOptions:");
  return ((int(*)(struct objc_super*,void*,void*,void*))objc_msgSendSuper)(
           &sup, ssel, app, opts);
}

int main(int argc, char** argv) {
  void* delegate = objc_getClass("FlutterAppDelegate");
  if (objc_getClass("GeneratedPluginRegistrant")) {
    void* sub = objc_allocateClassPair(delegate, "FLAppDelegate", 0);
    void* m = sel_registerName("application:didFinishLaunchingWithOptions:");
    class_addMethod(sub, m, (void*)didFinish, "i@:@@");
    objc_registerClassPair(sub);
    delegate = sub;
  }
  return UIApplicationMain(argc, argv, 0, NSStringFromClass(delegate));
}
`

const fallbackSceneDelegate = `import Flutter
import UIKit

class SceneDelegate: FlutterSceneDelegate {
  override func scene(
    _ scene: UIScene,
    willConnectTo session: UISceneSession,
    options connectionOptions: UIScene.ConnectionOptions
  ) {
    super.scene(scene, willConnectTo: session, options: connectionOptions)
    guard let windowScene = scene as? UIWindowScene else { return }
    if self.window == nil {
      let window = UIWindow(windowScene: windowScene)
      window.frame = windowScene.coordinateSpace.bounds
      window.rootViewController = FlutterViewController(
        project: nil, nibName: nil, bundle: nil)
      self.window = window
      window.makeKeyAndVisible()
    }
  }
}
`

func materializeMinSDK(tc toolchain.Toolchain) (string, error) {
	root := filepath.Join(tc.Root, "minsdk")
	lib := filepath.Join(root, "usr", "lib")
	uiKit := filepath.Join(root, "System", "Library", "Frameworks", "UIKit.framework")
	foundation := filepath.Join(root, "System", "Library", "Frameworks", "Foundation.framework")
	for _, dir := range []string{lib, uiKit, foundation} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	files := map[string]string{
		filepath.Join(lib, "libSystem.B.dylib.tbd"): libSystemTBD,
		filepath.Join(lib, "libobjc.A.dylib.tbd"):   libObjCTBD,
		filepath.Join(uiKit, "UIKit.tbd"):           uiKitTBD,
		filepath.Join(foundation, "Foundation.tbd"): foundationTBD,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return root, nil
}

// HostInputs carries the extra compile and link inputs that custom build
// phases, PBXBuildRules and SwiftPM packages contribute to the application host
// binary: sources they generated, directories holding generated headers or
// compiled Swift modules, objects or archives to link, and libraries and system
type HostInputs struct {
	Sources           []string
	HeaderSearchPaths []string
	ModuleSearchPaths []string
	// ModuleMaps are explicit `<Module>.modulemap` files. CocoaPods names its
	// per-target map after the module rather than `module.modulemap`, so clang
	// cannot discover it from a header search path and it must be passed
	// explicitly.
	ModuleMaps []string
	// MacroPlugins are `<executable>#<ModuleName>` entries for swiftc's
	// -load-plugin-executable. SwiftPM macro targets publish them, and the app
	// host's own Swift sources expand those macros, so they must reach the
	// host compile, not just the package targets.
	MacroPlugins     []string
	Objects          []string
	LinkerLibraries  []string
	LinkerFrameworks []string
	LinkerFlags      []string
	// TargetSources are the files the Xcode target's PBXSourcesBuildPhase
	// actually lists, already resolved to absolute paths. Xcode compiles exactly
	// this set, and real projects group sources into subdirectories, so globbing
	// the target's top-level directory silently drops them. Empty for a project
	// whose target has no Sources phase, in which case the glob still applies.
	TargetSources []string
}

func BuildProjectHost(tc toolchain.Toolchain, sourceDir, out, minOS, moduleName, bridgingHeader string, extra HostInputs, pluginFrameworks []string) (string, error) {
	swiftSources, err := projectSwiftSources(sourceDir, "", extra.TargetSources)
	if err != nil {
		return "", err
	}
	generatedSwift, generatedC := splitHostSources(extra.Sources)
	if hasMainAttribute(swiftSources) {
		return buildHostSwift(tc, sourceDir, out, minOS, moduleName, bridgingHeader, extra, generatedSwift, generatedC, pluginFrameworks)
	}
	if len(generatedSwift) > 0 {
		// The C launcher shim has no Swift module to compile against, so
		// silently dropping generated Swift would produce a binary missing the
		// rule's output.
		return "", fmt.Errorf("build rule generated Swift sources (%s) but %s has no Swift @main entry point to compile them with", strings.Join(generatedSwift, ", "), sourceDir)
	}
	return buildHost(tc, out, minOS, extra, generatedC, pluginFrameworks)
}

func buildHost(tc toolchain.Toolchain, out, minOS string, extra HostInputs, generatedC []string, pluginFrameworks []string) (string, error) {
	work := filepath.Join(filepath.Dir(filepath.Dir(out)), ".host_build")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	source := filepath.Join(work, "host.c")
	object := filepath.Join(work, "host.o")
	if err := os.WriteFile(source, []byte(hostShimC), 0o644); err != nil {
		return "", err
	}
	clang, err := hostClang(tc)
	if err != nil {
		return "", err
	}
	lld := filepath.Join(tc.ToolsetBin(), "ld64.lld")
	if !fileExists(lld) {
		lld = filepath.Join(filepath.Dir(clang), "ld64.lld")
	}
	generatedObjects, err := compileHostSources(tc, generatedC, hostIncludeDirs(extra), frameworkParentDirs(pluginFrameworks), extra.ModuleMaps, work, minOS)
	if err != nil {
		return "", err
	}

	if len(pluginFrameworks) > 0 {
		sdk, err := tc.IOSSDK()
		if err != nil {
			return "", err
		}
		if err := run("", nil, clang, "-target", "arm64-apple-ios"+minOS, "-isysroot", sdk, "-c", source, "-o", object); err != nil {
			return "", err
		}
		sdkVersion, err := tc.IOSSDKVersion()
		if err != nil {
			return "", err
		}
		args := []string{"-arch", "arm64", "-fixup_chains", "-platform_version", "ios", minOS, sdkVersion, "-syslibroot", sdk, "-o", out, object}
		args = append(args, extra.Objects...)
		args = append(args, generatedObjects...)
		for _, framework := range pluginFrameworks {
			name := strings.TrimSuffix(filepath.Base(framework), ".framework")
			args = append(args, "-framework", name, "-F", filepath.Dir(framework))
		}
		for _, framework := range extra.LinkerFrameworks {
			args = append(args, "-framework", framework)
		}
		for _, lib := range extra.LinkerLibraries {
			args = append(args, "-l"+lib)
		}
		args = append(args, extra.LinkerFlags...)
		for _, dir := range extra.ModuleSearchPaths {
			args = append(args, "-F", dir)
		}
		libs, err := tc.SwiftIOSLibs()
		if err != nil {
			return "", err
		}
		args = append(args,
			"-framework", "Flutter", "-framework", "UIKit", "-framework", "Foundation",
			"-F", filepath.Join(tc.FlutterXCFramework(), "ios-arm64"),
			"-F", filepath.Join(sdk, "System", "Library", "Frameworks"),
			"-F", filepath.Join(sdk, "System", "Library", "PrivateFrameworks"),
			"-L", filepath.Join(sdk, "usr", "lib"),
			"-L", filepath.Join(sdk, "usr", "lib", "swift"),
			"-L", libs,
		)
		args = append(args, swiftExecutableRPathArgs("@executable_path/Frameworks")...)
		runtimeLib := filepath.Join(libs, "libclang_rt.ios.a")
		if fileExists(runtimeLib) {
			args = append(args, runtimeLib)
		}
		args = append(args, "-lSystem")
		if err := runLD64Path(tc, lld, args...); err != nil {
			return "", err
		}
	} else {
		sdk, err := materializeMinSDK(tc)
		if err != nil {
			return "", err
		}
		if err := run("", nil, clang, "-target", "arm64-apple-ios"+minOS, "-c", source, "-o", object); err != nil {
			return "", err
		}
		args := []string{
			"-arch", "arm64", "-fixup_chains",
			"-platform_version", "ios", minOS, minOS,
			"-o", out, object,
		}
		args = append(args, generatedObjects...)
		args = append(args,
			filepath.Join(sdk, "usr", "lib", "libSystem.B.dylib.tbd"),
			filepath.Join(sdk, "usr", "lib", "libobjc.A.dylib.tbd"),
			filepath.Join(sdk, "System", "Library", "Frameworks", "UIKit.framework", "UIKit.tbd"),
			filepath.Join(sdk, "System", "Library", "Frameworks", "Foundation.framework", "Foundation.tbd"),
		)
		if err := run("", nil, lld, args...); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(out, 0o755); err != nil {
		return "", err
	}
	return out, nil
}

// projectSwiftSources returns the Swift files that make up the application
// target, applying the programmatic SceneDelegate substitution.
//
// targetSources is the target's own PBXSourcesBuildPhase file list and is
// authoritative when present: Xcode compiles exactly those files, and real
// projects group sources into subdirectories (immich keeps 14 of its 15 Swift
// files under Images/, Background/, Connectivity/ and Permission/), which a
// top-level glob silently drops. The glob remains the fallback for a target
// with no Sources phase.
func projectSwiftSources(sourceDir, work string, targetSources []string) ([]string, error) {
	var matches []string
	for _, path := range targetSources {
		if strings.EqualFold(filepath.Ext(path), ".swift") {
			matches = append(matches, path)
		}
	}
	if len(matches) == 0 {
		globbed, err := filepath.Glob(filepath.Join(sourceDir, "*.swift"))
		if err != nil {
			return nil, err
		}
		sort.Strings(globbed)
		matches = globbed
	}
	out := make([]string, 0, len(matches)+1)
	hasSceneDelegate := false
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text := string(data)
		// Flutter's UIScene migration has two valid shapes: newer projects own a
		// SceneDelegate.swift subclass, while migrated/custom AppDelegates may
		// point Info.plist directly at FlutterSceneDelegate and therefore have no
		// source file at all. ripley cannot ship Main.storyboard uncompiled, so both
		// shapes need the same programmatic FlutterViewController bootstrap.
		if strings.Contains(text, "class SceneDelegate") {
			hasSceneDelegate = true
		}
		if filepath.Base(path) == "SceneDelegate.swift" && strings.Contains(text, "FlutterSceneDelegate") && !strings.Contains(text, "UIWindow(") && work != "" {
			if err := os.MkdirAll(work, 0o755); err != nil {
				return nil, err
			}
			fallback := filepath.Join(work, "SceneDelegate.swift")
			if err := os.WriteFile(fallback, []byte(fallbackSceneDelegate), 0o644); err != nil {
				return nil, err
			}
			out = append(out, fallback)
			continue
		}
		out = append(out, path)
	}
	if work != "" && !hasSceneDelegate {
		if err := os.MkdirAll(work, 0o755); err != nil {
			return nil, err
		}
		fallback := filepath.Join(work, "SceneDelegate.swift")
		if err := os.WriteFile(fallback, []byte(fallbackSceneDelegate), 0o644); err != nil {
			return nil, err
		}
		out = append(out, fallback)
	}
	return out, nil
}

func hasMainAttribute(swiftFiles []string) bool {
	for _, path := range swiftFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(data)
		if strings.Contains(text, "@main") || strings.Contains(text, "@NSApplicationMain") || strings.Contains(text, "@UIApplicationMain") {
			return true
		}
	}
	return false
}

func buildHostSwift(tc toolchain.Toolchain, sourceDir, out, minOS, moduleName, bridgingHeader string, extra HostInputs, generatedSwift, generatedC []string, pluginFrameworks []string) (string, error) {
	work := filepath.Join(filepath.Dir(filepath.Dir(out)), ".host_build")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	sources, err := projectSwiftSources(sourceDir, work, extra.TargetSources)
	if err != nil {
		return "", err
	}
	swiftc, err := swiftCompiler(tc)
	if err != nil {
		return "", err
	}
	flags, err := commonSwiftFlags(tc, minOS)
	if err != nil {
		return "", err
	}
	search, err := frameworkSearch(tc)
	if err != nil {
		return "", err
	}
	sources = append(sources, generatedSwift...)
	object := filepath.Join(work, "app_swift.o")
	args := append(flags, search...)
	for _, dir := range extra.HeaderSearchPaths {
		args = append(args, "-Xcc", "-I"+dir)
	}
	for _, dir := range extra.ModuleSearchPaths {
		// SwiftPM packages expose both a .swiftmodule and a generated ObjC
		// header from the same directory, so it is a search path for the Swift
		// importer and for clang.
		args = append(args, "-I", dir, "-Xcc", "-I"+dir)
	}
	for _, path := range extra.ModuleMaps {
		// CocoaPods writes `<Module>.modulemap`, which clang cannot discover from
		// a header search path; only the public tree's copy resolves an umbrella
		// header's imports correctly.
		args = append(args, "-Xcc", "-fmodule-map-file="+path)
	}
	for _, plugin := range extra.MacroPlugins {
		// `<path>#<ModuleName>`; swiftc matches the module name against the
		// macro declaration's `module:`.
		args = append(args, "-load-plugin-executable", plugin)
	}
	// A Swift AppDelegate may import a Flutter plugin module directly (for
	// example workmanager_apple). CocoaPods use_frameworks products are real
	// framework module containers, so their parent directories must be visible
	// while compiling the host itself, not only while compiling the registrant
	// and linking the final executable.
	args = append(args, hostSwiftFrameworkSearchArgs(pluginFrameworks)...)
	// #Preview/#Previewable are Xcode-canvas macros declared by the SDK but
	// implemented by a plugin the Linux toolchain lacks; a stub that expands
	// them to nothing is correct for a device build.
	if preview, err := previewsMacrosPlugin(tc, sources); err != nil {
		return "", err
	} else if preview != "" {
		args = append(args, "-load-plugin-executable", preview)
	}
	args = append(args, sources...)
	args = append(args, "-module-name", moduleName, "-o", object)
	if bridgingHeader != "" && fileExists(bridgingHeader) {
		args = append(args, "-import-objc-header", bridgingHeader)
	}
	if err := run("", nil, swiftc, args...); err != nil {
		return "", err
	}
	generatedObjects, err := compileHostSources(tc, generatedC, hostIncludeDirs(extra), frameworkParentDirs(pluginFrameworks), extra.ModuleMaps, work, minOS)
	if err != nil {
		return "", err
	}

	sdk, err := tc.IOSSDK()
	if err != nil {
		return "", err
	}
	sdkVersion, err := tc.IOSSDKVersion()
	if err != nil {
		return "", err
	}
	args = []string{"-arch", "arm64", "-fixup_chains", "-platform_version", "ios", minOS, sdkVersion, "-syslibroot", sdk, "-o", out, object}
	for _, object := range extra.Objects {
		if object != "" {
			args = append(args, object)
		}
	}
	args = append(args, generatedObjects...)
	for _, framework := range pluginFrameworks {
		name := strings.TrimSuffix(filepath.Base(framework), ".framework")
		args = append(args, "-framework", name, "-F", filepath.Dir(framework))
	}
	for _, framework := range extra.LinkerFrameworks {
		args = append(args, "-framework", framework)
	}
	for _, library := range extra.LinkerLibraries {
		args = append(args, "-l"+library)
	}
	args = append(args, extra.LinkerFlags...)
	for _, dir := range extra.ModuleSearchPaths {
		args = append(args, "-F", dir)
	}
	args = append(args,
		"-framework", "Flutter", "-framework", "UIKit", "-framework", "Foundation",
		"-lswiftCore", "-lswiftFoundation", "-lswiftDarwin", "-lswiftObjectiveC", "-lswiftDispatch", "-lswiftCoreFoundation",
		"-lswiftCompatibility56", "-lswiftCompatibilityPacks", "-lswiftCompatibilityConcurrency",
	)
	args = append(args, search...)
	libs, err := libSearch(tc)
	if err != nil {
		return "", err
	}
	args = append(args, libs...)
	swiftIOSLibs, err := tc.SwiftIOSLibs()
	if err != nil {
		return "", err
	}
	runtimeLib := filepath.Join(swiftIOSLibs, "libclang_rt.ios.a")
	if fileExists(runtimeLib) {
		args = append(args, runtimeLib)
	}
	args = append(args, swiftExecutableRPathArgs("@executable_path/Frameworks")...)
	args = append(args, "-lSystem")
	if err := runLD64(tc, args...); err != nil {
		return "", err
	}
	if err := os.Chmod(out, 0o755); err != nil {
		return "", err
	}
	return out, nil
}

// splitHostSources separates generated Swift sources from C-family sources.
// Generated headers and resources are not compile inputs and are ignored here;
// headers reach the compile through HostInputs.HeaderSearchPaths.
func splitHostSources(sources []string) (swift, cfamily []string) {
	for _, src := range sources {
		switch strings.ToLower(filepath.Ext(src)) {
		case ".swift":
			swift = append(swift, src)
		case ".c", ".m", ".mm", ".cc", ".cpp", ".cxx":
			cfamily = append(cfamily, src)
		}
	}
	return swift, cfamily
}

// compileHostSources compiles C-family sources for the host binary or an app
// extension: build-rule generated files plus any the target's Sources phase
// lists. The iPhoneOS SDK is used when one is provisioned; the SDK-free launcher
// path compiles freestanding exactly as the C shim does.
//
// frameworkPaths are directories holding `.framework` bundles. They are required
// whenever a source resolves a framework-style header such as
// `#import <Flutter/Flutter.h>`; a header search path alone cannot satisfy that
// include.
func compileHostSources(tc toolchain.Toolchain, sources, headerPaths, frameworkPaths, moduleMaps []string, work, minOS string) ([]string, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	// Use the Clang shipped with the Swift toolchain when available. Modern
	// iPhoneOS SDK module maps contain Clang feature requirements that the host
	// distro compiler may not understand; pluginClang is already the proven
	// compiler used for CocoaPods/plugin Objective-C consumers.
	clang, err := pluginClang(tc)
	if err != nil {
		return nil, err
	}
	base := []string{"-target", "arm64-apple-ios" + minOS}
	if sdk, sdkErr := tc.IOSSDK(); sdkErr == nil {
		base = append(base, "-isysroot", sdk)
	}
	for _, dir := range headerPaths {
		base = append(base, "-I", dir)
	}
	for _, dir := range frameworkPaths {
		base = append(base, "-F", dir)
	}
	objects := make([]string, 0, len(sources))
	for i, src := range sources {
		name := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		object := filepath.Join(work, fmt.Sprintf("generated_%d_%s.o", i, name))
		args := append([]string{}, base...)
		args = append(args, hostCSourceModuleFlags(src, clangModuleMaps(headerPaths, moduleMaps))...)
		args = append(args, "-c", src, "-o", object)
		if err := run("", nil, clang, args...); err != nil {
			return nil, fmt.Errorf("compile generated host source %s: %w", src, err)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

// clangModuleMaps builds the clang-module view of a target from the same
// header search paths the compiler receives, plus any maps that cannot be
// discovered from those directories. CocoaPods exposes ObjC-only and Swift
// pods alike through Headers/Public/<Module>/*.modulemap; treating only Swift
// module maps as compile inputs makes downstream @import behavior depend on
// which language a dependency happened to be implemented in.
func clangModuleMaps(headerPaths, explicit []string) []string {
	var maps []string
	seenPaths := make(map[string]bool)
	claimedModules := make(map[string]bool)

	add := func(path string) {
		if path == "" || seenPaths[path] {
			return
		}
		// Flutter is imported from Flutter.framework. Passing CocoaPods' stub
		// Flutter.modulemap as well declares the same module twice.
		if strings.Contains(filepath.ToSlash(path), "/Headers/Public/Flutter/Flutter.modulemap") {
			return
		}
		names := moduleMapTopLevelNames(path)
		for _, name := range names {
			if claimedModules[name] {
				// A target can see both CocoaPods' canonical public map and a mirror
				// beside an emitted .swiftmodule. They describe the same clang
				// module; passing both is a hard "redefinition of module" error.
				// Since header-path maps are added first below, the canonical public
				// map wins over any explicit build-directory mirror.
				seenPaths[path] = true
				return
			}
		}
		seenPaths[path] = true
		maps = append(maps, path)
		for _, name := range names {
			claimedModules[name] = true
		}
	}

	// Header search paths are the target's canonical clang view. Prefer module
	// maps discoverable there (notably Pods/Headers/Public/<Module>) over mirrors
	// supplied explicitly for Swift importer compatibility.
	for _, dir := range headerPaths {
		if dir == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "*.modulemap"))
		sort.Strings(matches)
		for _, path := range matches {
			add(path)
		}
	}
	for _, path := range explicit {
		add(path)
	}
	sort.Strings(maps)
	return maps
}

// hostCSourceModuleFlags mirrors the Xcode Clang settings needed by ObjC and
// ObjC++ target sources. GeneratedPluginRegistrant.m commonly uses @import, so
// modules and the CocoaPods module maps are load-bearing for extensions that
// intentionally compile the registrant (for example a headless FlutterEngine
// notification service). Pure C/C++ sources do not receive Objective-C flags.
func hostCSourceModuleFlags(source string, moduleMaps []string) []string {
	ext := strings.ToLower(filepath.Ext(source))
	if ext != ".m" && ext != ".mm" {
		return nil
	}
	flags := []string{"-fobjc-arc", "-fmodules"}
	seen := make(map[string]bool, len(moduleMaps))
	for _, moduleMap := range moduleMaps {
		if moduleMap == "" || seen[moduleMap] {
			continue
		}
		seen[moduleMap] = true
		flags = append(flags, "-fmodule-map-file="+moduleMap)
	}
	return flags
}

// frameworkParentDirs returns the `-F` search directories implied by a list of
// framework paths, so a C/ObjC translation unit can resolve
// `#import <Flutter/Flutter.h>` for the same frameworks the Swift stage sees.
// Bare system framework names contribute nothing: those live in the SDK, which
// is already on the search path.
// hostSwiftFrameworkSearchArgs returns the -F search roots needed when
// Swift host/extension sources import plugin framework modules directly.
// System-framework names in pluginFrameworks are intentionally ignored here;
// frameworkSearch(tc) already provides the SDK framework roots.
func hostSwiftFrameworkSearchArgs(frameworks []string) []string {
	var args []string
	for _, dir := range frameworkParentDirs(frameworks) {
		args = append(args, "-F", dir)
	}
	return args
}

func frameworkParentDirs(frameworks []string) []string {
	var dirs []string
	seen := make(map[string]bool, len(frameworks))
	for _, framework := range frameworks {
		if !strings.HasSuffix(framework, ".framework") {
			continue
		}
		dir := filepath.Dir(framework)
		if dir == "" || dir == "." || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs
}

// hostIncludeDirs returns the `-I` directories a C/ObjC translation unit needs:
// generated-header directories plus the compiled-module directories, which also
// hold each module's generated ObjC header.
func hostIncludeDirs(extra HostInputs) []string {
	dirs := make([]string, 0, len(extra.HeaderSearchPaths)+len(extra.ModuleSearchPaths))
	dirs = append(dirs, extra.HeaderSearchPaths...)
	dirs = append(dirs, extra.ModuleSearchPaths...)
	return dirs
}

func hostClang(tc toolchain.Toolchain) (string, error) {
	if clang, err := exec.LookPath("clang"); err == nil {
		return clang, nil
	}
	var candidates []string
	err := filepath.WalkDir(tc.Root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && d.Name() == "clang" && filepath.Base(filepath.Dir(path)) == "bin" {
			candidates = append(candidates, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", errors.New("clang not found; install clang")
	}
	sort.Strings(candidates)
	return candidates[0], nil
}
