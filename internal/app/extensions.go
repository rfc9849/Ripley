package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ripley/internal/ios"
	"ripley/internal/ios/assetcatalog"
	"ripley/internal/toolchain"

	"howett.net/plist"
)

// appExtensionProductType is the PBXNativeTarget productType of an iOS
// application extension. selectApplicationTarget deliberately matches only
// `com.apple.product-type.application`, so these targets are invisible to the
// rest of the project resolution and are collected here instead.
const appExtensionProductType = "com.apple.product-type.app-extension"

// appExtensionPackageType is CFBundlePackageType for an `.appex`. UIKit refuses
// to load a PlugIns member that claims to be an application (`APPL`).
const appExtensionPackageType = "XPC!"

// extensionSourceExtensions are the files an extension target compiles.
var extensionSourceExtensions = map[string]bool{
	".swift": true, ".m": true, ".mm": true, ".c": true, ".cc": true, ".cpp": true, ".cxx": true,
}

// extensionHeaderExtensions only need to be reachable on the include path.
var extensionHeaderExtensions = map[string]bool{
	".h": true, ".hpp": true, ".hh": true, ".hxx": true,
}

// extensionImplicitModules are modules that resolve without naming a framework
// on the link line: the Swift runtime's own modules, the Darwin/POSIX overlays
// the SDK exposes through `usr/include/module.modulemap` (zulip's
// `import os`), and the two frameworks BuildAppExtension always links.
var extensionImplicitModules = map[string]bool{
	"Swift": true, "Foundation": true, "UIKit": true, "Darwin": true, "Dispatch": true,
	"ObjectiveC": true, "os": true, "_Concurrency": true, "CoreFoundation": true,
}

var swiftImportRE = regexp.MustCompile(`(?m)^\s*(?:@[A-Za-z_]+\s+)*import\s+(?:struct\s+|class\s+|enum\s+|func\s+|typealias\s+|protocol\s+|var\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
var objcImportRE = regexp.MustCompile(`(?m)^\s*@import\s+([A-Za-z_][A-Za-z0-9_]*)|^\s*#import\s+<([A-Za-z_][A-Za-z0-9_]*)/`)

// appExtension is one resolved `com.apple.product-type.app-extension` target:
// everything needed to compile it, name its bundle and write its Info.plist.
//
// Sources merges the target's legacy PBXSourcesBuildPhase file list with the
// members of its Xcode 16 folder-synchronized groups, because a real project
// uses either or both: immich's WidgetExtension has an empty Sources phase and
// draws its whole compile set from a synced group, while its ShareExtension
// lists one file the classic way.
type appExtension struct {
	TargetName     string
	ProductName    string
	ExecutableName string
	ModuleName     string
	BundleID       string
	MinOS          string
	InfoPlist      string
	Entitlements   string
	SourceDir      string
	BridgingHeader string
	Sources        []string
	HeaderDirs     []string
	AssetCatalogs  []string
	// Uncompilable lists bundle members whose Xcode compiler ripley has no
	// replacement for on Linux (a storyboard, a string catalog). They are
	// reported rather than silently dropped.
	Uncompilable  []string
	BuildSettings map[string]string
}

// BundleName is the `.appex` directory name inside `<App>.app/PlugIns`.
func (e appExtension) BundleName() string { return e.ProductName + ".appex" }

// discoverAppExtensions re-reads the project file and returns every application
// extension target with its build settings, sources and bundle identity already
// resolved. A project with no extension target yields nil and no error, so
// nothing changes for a simple app.
//
// The pbxproj is re-read rather than threaded through iosProject for the same
// reason projectBuildRules re-reads it: the resolved iosProject describes one
// target, and these are the other ones.
func discoverAppExtensions(config iosProject) ([]appExtension, error) {
	pbxPath := filepath.Join(config.XcodeProject, "project.pbxproj")
	data, err := os.ReadFile(pbxPath)
	if err != nil {
		return nil, err
	}
	objects, err := parsePBXObjects(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", pbxPath, err)
	}

	targets := appExtensionTargets(objects)
	if len(targets) == 0 {
		return nil, nil
	}
	extensions := make([]appExtension, 0, len(targets))
	for _, target := range targets {
		extension, err := resolveAppExtension(config, objects, target)
		if err != nil {
			return nil, err
		}
		extensions = append(extensions, extension)
	}
	return extensions, nil
}

// appExtensionTargets collects the extension targets in a deterministic order.
// Object iteration is map order, so the list is sorted by target name to keep
// discovery, build order and the reported result stable.
func appExtensionTargets(objects map[string]pbxObject) []xcodeTarget {
	var targets []xcodeTarget
	for _, object := range objects {
		if pbxField(object.Body, "isa") != "PBXNativeTarget" {
			continue
		}
		if pbxField(object.Body, "productType") != appExtensionProductType {
			continue
		}
		targets = append(targets, xcodeTarget{
			ID:                     object.ID,
			Name:                   pbxField(object.Body, "name"),
			BuildConfigurationList: pbxObjectIDField(object.Body, "buildConfigurationList"),
			BuildPhases:            pbxObjectIDArrayField(object.Body, "buildPhases"),
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets
}

// resolveAppExtension resolves one extension target the way resolveIOSProject
// resolves the application target: seed the Xcode-provided settings, layer the
// project-wide configuration, then the target's own, then expand.
//
// The configuration is selected by the name the application target already
// resolved to, because Xcode builds every target of a scheme in one
// configuration. That is what makes immich's WidgetExtension pick up
// `$(IMMICH_BUNDLE_ID_DEV).debug.Widget` for Debug and
// `$(IMMICH_BUNDLE_ID_PROD).Widget` for Release.
func resolveAppExtension(config iosProject, objects map[string]pbxObject, target xcodeTarget) (appExtension, error) {
	configs, err := configurationsForList(objects, target.BuildConfigurationList)
	if err != nil {
		return appExtension{}, fmt.Errorf("extension %s: %w", target.Name, err)
	}
	configuration, err := selectExtensionConfiguration(configs, config.Configuration)
	if err != nil {
		return appExtension{}, fmt.Errorf("extension %s: %w", target.Name, err)
	}

	iosRoot := config.IOSRoot
	settings := map[string]string{
		"SRCROOT":                 iosRoot,
		"SOURCE_ROOT":             iosRoot,
		"PROJECT_DIR":             iosRoot,
		"PROJECT_NAME":            strings.TrimSuffix(filepath.Base(config.XcodeProject), ".xcodeproj"),
		"TARGET_NAME":             target.Name,
		"CONFIGURATION":           configuration.Name,
		"SDKROOT":                 "iphoneos",
		"PLATFORM_NAME":           "iphoneos",
		"EFFECTIVE_PLATFORM_NAME": "-iphoneos",
		"FLUTTER_BUILD_NAME":      config.BuildName,
		"FLUTTER_BUILD_NUMBER":    config.BuildNumber,
	}
	if language := projectDevelopmentRegion(objects); language != "" {
		settings["DEVELOPMENT_LANGUAGE"] = language
	}
	if projectConfig, ok, err := projectBuildConfiguration(objects, configuration.Name, configuration.Name); err != nil {
		return appExtension{}, err
	} else if ok {
		if err := applyBuildConfiguration(settings, iosRoot, objects, projectConfig); err != nil {
			return appExtension{}, err
		}
	}
	if err := applyBuildConfiguration(settings, iosRoot, objects, configuration); err != nil {
		return appExtension{}, err
	}
	expandAllBuildSettings(settings)

	productName := firstNonEmpty(settings["PRODUCT_NAME"], target.Name)
	settings["PRODUCT_NAME"] = productName
	executableName := firstNonEmpty(settings["EXECUTABLE_NAME"], productName)
	settings["EXECUTABLE_NAME"] = executableName
	moduleName := firstNonEmpty(settings["PRODUCT_MODULE_NAME"], xcodeModuleName(productName))
	settings["PRODUCT_MODULE_NAME"] = moduleName
	expandAllBuildSettings(settings)

	bundleID := strings.TrimSpace(settings["PRODUCT_BUNDLE_IDENTIFIER"])
	if bundleID == "" {
		return appExtension{}, fmt.Errorf("extension %s configuration %s has no PRODUCT_BUNDLE_IDENTIFIER", target.Name, configuration.Name)
	}
	bundleID = overriddenChildBundleID(config, bundleID)
	settings["PRODUCT_BUNDLE_IDENTIFIER"] = bundleID
	// An extension without its own deployment target inherits the host app's;
	// Xcode resolves that through the project-level setting, but a project that
	// sets neither still has to produce a valid MinimumOSVersion.
	minOS := firstNonEmpty(settings["IPHONEOS_DEPLOYMENT_TARGET"], config.MinOS)
	settings["IPHONEOS_DEPLOYMENT_TARGET"] = minOS

	infoSetting := strings.TrimSpace(settings["INFOPLIST_FILE"])
	if infoSetting == "" {
		return appExtension{}, fmt.Errorf("extension %s configuration %s has no INFOPLIST_FILE; ripley needs the target's concrete Info.plist to read its NSExtension dictionary", target.Name, configuration.Name)
	}
	infoPlist := resolveProjectPath(iosRoot, expandBuildValue(infoSetting, settings))
	if _, err := os.Stat(infoPlist); err != nil {
		return appExtension{}, fmt.Errorf("extension %s Info.plist %s: %w", target.Name, infoPlist, err)
	}
	entitlements := ""
	if value := strings.TrimSpace(settings["CODE_SIGN_ENTITLEMENTS"]); value != "" {
		entitlements = resolveProjectPath(iosRoot, expandBuildValue(value, settings))
		if _, err := os.Stat(entitlements); err != nil {
			return appExtension{}, fmt.Errorf("extension %s entitlements %s: %w", target.Name, entitlements, err)
		}
	}
	bridgingHeader := ""
	if value := strings.TrimSpace(settings["SWIFT_OBJC_BRIDGING_HEADER"]); value != "" {
		bridgingHeader = resolveProjectPath(iosRoot, expandBuildValue(value, settings))
	}

	extension := appExtension{
		TargetName:     target.Name,
		ProductName:    productName,
		ExecutableName: executableName,
		ModuleName:     moduleName,
		BundleID:       bundleID,
		MinOS:          minOS,
		InfoPlist:      infoPlist,
		Entitlements:   entitlements,
		SourceDir:      filepath.Dir(infoPlist),
		BridgingHeader: bridgingHeader,
		BuildSettings:  settings,
	}

	listed, err := targetSourceFiles(objects, target, iosRoot)
	if err != nil {
		return appExtension{}, err
	}
	synced, err := targetSynchronizedSources(objects, target, iosRoot)
	if err != nil {
		return appExtension{}, err
	}
	classifyExtensionInputs(&extension, append(listed, synced...))
	return extension, nil
}

// selectExtensionConfiguration picks the extension configuration matching the
// name the application target resolved to. A flavor-suffixed application
// configuration (`Release-production`) has no counterpart on an extension that
// only declares the three stock names, so the base name is accepted as the
// fallback rather than failing a project Xcode builds fine.
func selectExtensionConfiguration(configs []xcodeBuildConfiguration, wanted string) (xcodeBuildConfiguration, error) {
	for _, config := range configs {
		if config.Name == wanted {
			return config, nil
		}
	}
	if base, _, found := strings.Cut(wanted, "-"); found {
		for _, config := range configs {
			if config.Name == base {
				return config, nil
			}
		}
	}
	names := make([]string, 0, len(configs))
	for _, config := range configs {
		names = append(names, config.Name)
	}
	sort.Strings(names)
	return xcodeBuildConfiguration{}, fmt.Errorf("configuration %q not found; available: %s", wanted, strings.Join(names, ", "))
}

// classifyExtensionInputs sorts a target's membership list into the buckets the
// build acts on. Folder-synchronized groups hand back every file in the folder
// unclassified, so this is where an `Info.plist`, an entitlements file or a
// `README` stops being mistaken for a translation unit.
func classifyExtensionInputs(extension *appExtension, members []string) {
	seenSource := make(map[string]bool)
	headerDirs := make(map[string]bool)
	seenCatalog := make(map[string]bool)
	seenOther := make(map[string]bool)
	for _, path := range members {
		switch ext := strings.ToLower(filepath.Ext(path)); {
		case extensionSourceExtensions[ext]:
			if !seenSource[path] {
				seenSource[path] = true
				extension.Sources = append(extension.Sources, path)
			}
		case extensionHeaderExtensions[ext]:
			headerDirs[filepath.Dir(path)] = true
		case ext == ".xcassets":
			if !seenCatalog[path] {
				seenCatalog[path] = true
				extension.AssetCatalogs = append(extension.AssetCatalogs, path)
			}
		case ext == ".storyboard" || ext == ".xib" || ext == ".xcstrings" || ext == ".intentdefinition":
			if !seenOther[path] {
				seenOther[path] = true
				extension.Uncompilable = append(extension.Uncompilable, path)
			}
		}
	}
	// A localized storyboard reaches us as its Base.lproj member, so the
	// enclosing .lproj is already covered by the member list above.
	for dir := range headerDirs {
		extension.HeaderDirs = append(extension.HeaderDirs, dir)
	}
	sort.Strings(extension.HeaderDirs)
	sort.Strings(extension.AssetCatalogs)
	sort.Strings(extension.Uncompilable)
}

// extensionImports returns the module names the extension's own sources
// import, sorted for a deterministic build. Imports are the only available
// evidence: an extension target lists no frameworks in the pbxproj beyond an
// empty PBXFrameworksBuildPhase, and Xcode resolves the rest implicitly.
func extensionImports(extension appExtension) []string {
	modules := make(map[string]bool)
	for _, source := range extension.Sources {
		data, err := os.ReadFile(source)
		if err != nil {
			continue
		}
		text := string(data)
		re := swiftImportRE
		if strings.ToLower(filepath.Ext(source)) != ".swift" {
			re = objcImportRE
		}
		for _, match := range re.FindAllStringSubmatch(text, -1) {
			for _, name := range match[1:] {
				if name != "" {
					modules[name] = true
				}
			}
		}
	}
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// podModulesByModule indexes built pod targets by the module name an import
// statement uses, which is the pod target name unless the podspec renamed the
// module.
func podModulesByModule(pods map[string]ios.PodModule) map[string]ios.PodModule {
	byModule := make(map[string]ios.PodModule, len(pods))
	for name, pod := range pods {
		byModule[name] = pod
		if pod.ModuleName != "" {
			byModule[pod.ModuleName] = pod
		}
	}
	return byModule
}

// linkExtensionPod records the link and header inputs an extension needs for
// one pod module and, recursively, the pod's own dependencies: a Swift pod's
// .swiftmodule names the modules it imports, so the whole closure must be
// reachable. The pod's static archive is linked into the extension itself —
// the same way Xcode links a static-library pod into each target that uses it.
func linkExtensionPod(pod ios.PodModule, pods map[string]ios.PodModule, inputs *ios.HostInputs, archives map[string]bool, linked map[string]bool, frameworks *[]string, frameworkPaths map[string]bool) {
	if linked[pod.Name] {
		return
	}
	linked[pod.Name] = true
	if pod.Framework != "" {
		if pod.StaticFramework {
			hasObjC := false
			for _, flag := range inputs.LinkerFlags {
				if flag == "-ObjC" {
					hasObjC = true
					break
				}
			}
			if !hasObjC {
				inputs.LinkerFlags = append(inputs.LinkerFlags, "-ObjC")
			}
		}
		if !frameworkPaths[pod.Framework] {
			frameworkPaths[pod.Framework] = true
			*frameworks = append(*frameworks, pod.Framework)
		}
		inputs.LinkerFlags = append(inputs.LinkerFlags, pod.LinkerFlags...)
	} else {
		if pod.Archive != "" && !archives[pod.Archive] {
			archives[pod.Archive] = true
			inputs.Objects = append(inputs.Objects, pod.Archive)
		}
		if pod.ModuleMap != "" {
			// The public modulemap's directory is the pod's public header root;
			// its parent makes `#import <pod/Header.h>` resolve too.
			inputs.HeaderSearchPaths = append(inputs.HeaderSearchPaths, filepath.Dir(pod.ModuleMap), filepath.Dir(filepath.Dir(pod.ModuleMap)))
		}
	}
	for _, dependency := range pod.Dependencies {
		if dep, ok := pods[dependency]; ok {
			linkExtensionPod(dep, pods, inputs, archives, linked, frameworks, frameworkPaths)
		}
	}
}

// extensionFrameworks returns the frameworks the extension's own sources import,
// as link inputs for BuildAppExtension. A system framework contributes its bare
// name; a framework built for this project (a plugin framework, a CocoaPods
// Swift pod) contributes its absolute path so the compile and link also get a
// `-F` for its parent directory. A module built as a CocoaPods static library
// contributes its archive through inputs.Objects instead, and its public
// headers reach the compile through inputs.HeaderSearchPaths.
//
// Anything that resolves without a link flag (the Swift runtime, the Darwin
// overlays, UIKit and Foundation, which BuildAppExtension always links) is left
// out, and an import naming neither a known framework nor a built one is
// skipped rather than passed to the linker as a `-framework` that would not
// resolve.
func extensionFrameworks(extension appExtension, sdk string, built map[string]string, pods map[string]ios.PodModule, inputs *ios.HostInputs) []string {
	byModule := podModulesByModule(pods)
	archives := make(map[string]bool)
	linked := make(map[string]bool)
	frameworkPaths := make(map[string]bool)
	var out []string
	for _, name := range extensionImports(extension) {
		if extensionImplicitModules[name] {
			continue
		}
		if pod, ok := byModule[name]; ok {
			if pod.Framework != "" {
				linkExtensionPod(pod, pods, inputs, archives, linked, &out, frameworkPaths)
				continue
			}
			// Compatibility fallback for a framework product indexed from the app
			// or build directory rather than recorded on PodModule.
			for _, module := range []string{pod.ModuleName, pod.Name} {
				if module == "" {
					continue
				}
				if path, exists := built[module]; exists {
					if !frameworkPaths[path] {
						frameworkPaths[path] = true
						out = append(out, path)
					}
					inputs.LinkerFlags = append(inputs.LinkerFlags, pod.LinkerFlags...)
					linked[pod.Name] = true
					break
				}
			}
			if linked[pod.Name] {
				continue
			}
			linkExtensionPod(pod, pods, inputs, archives, linked, &out, frameworkPaths)
			continue
		}
		if path, ok := built[name]; ok {
			if !frameworkPaths[path] {
				frameworkPaths[path] = true
				out = append(out, path)
			}
			continue
		}
		if sdk != "" && sdkHasFramework(sdk, name) {
			out = append(out, name)
			continue
		}
		fmt.Printf("app extension %s: imports %s, which is neither an SDK framework nor built for this project; not linked\n", extension.TargetName, name)
	}
	return out
}

// sdkHasFramework reports whether the iPhoneOS SDK ships a framework of that
// name, in either of the two directories a public framework lives in.
func sdkHasFramework(sdk, name string) bool {
	for _, dir := range []string{"Frameworks", "SubFrameworks"} {
		if dirExists(filepath.Join(sdk, "System", "Library", dir, name+".framework")) {
			return true
		}
	}
	return false
}

// buildAppExtensions compiles every application extension target into
// `<App>.app/PlugIns/<Name>.appex` and returns the bundle paths it created. A
func buildAppExtensions(tc toolchain.Toolchain, project project, config iosProject, app string, plugins ios.PluginBuild, macroPlugins []string) ([]string, error) {
	extensions, err := discoverAppExtensions(config)
	if err != nil {
		return nil, err
	}
	if len(extensions) == 0 {
		return nil, nil
	}
	plugIns := filepath.Join(app, "PlugIns")
	if err := os.MkdirAll(plugIns, 0o755); err != nil {
		return nil, err
	}
	// A missing SDK is not fatal here: it only costs the ability to recognise a
	// system framework by name, and BuildAppExtension reports the real failure.
	sdk, _ := tc.IOSSDK()
	built := builtFrameworksByModule(app, project.BuildDir, tc.FlutterXCFramework())
	bundles := make([]string, 0, len(extensions))
	for _, extension := range extensions {
		bundle, err := buildAppExtension(tc, project, plugIns, extension, sdk, built, plugins, macroPlugins)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}
	return bundles, nil
}

// builtFrameworksByModule indexes the frameworks this build can link an
// extension against, keyed by the module name it would import: the plugin and
// pod frameworks staged into the app bundle, anything still only in the build
// directory, and the toolchain's own Flutter.framework slice. Later directories
// win, so the copy inside the app bundle - the one the extension's rpath
// resolves to at runtime - takes precedence over a build-directory original.
//
// Flutter has to be in here explicitly: it never passes through the app's
// Frameworks directory before extensions are built, and zulip's
// NotificationService imports it to run a headless engine.
func builtFrameworksByModule(app, buildDir, flutterXCFramework string) map[string]string {
	dirs := []string{buildDir, filepath.Join(app, "Frameworks")}
	if flutterXCFramework != "" {
		dirs = append([]string{filepath.Join(flutterXCFramework, "ios-arm64")}, dirs...)
	}
	out := make(map[string]string)
	for _, dir := range dirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.framework"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			out[strings.TrimSuffix(filepath.Base(path), ".framework")] = path
		}
	}
	return out
}
func buildAppExtension(tc toolchain.Toolchain, project project, plugIns string, extension appExtension, sdk string, built map[string]string, plugins ios.PluginBuild, macroPlugins []string) (string, error) {
	if len(extension.Sources) == 0 {
		return "", fmt.Errorf("extension %s has no source files: neither its Sources build phase nor a folder-synchronized group contributes one", extension.TargetName)
	}
	for _, path := range extension.Uncompilable {
		fmt.Printf("app extension %s: %s needs an Xcode compiler ripley has no Linux replacement for; it is not in the bundle\n", extension.TargetName, filepath.Base(path))
	}

	bundle := filepath.Join(plugIns, extension.BundleName())
	if err := os.RemoveAll(bundle); err != nil {
		return "", err
	}
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		return "", err
	}
	work := filepath.Join(project.BuildDir, "extensions", extension.TargetName)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}

	inputs := ios.HostInputs{
		HeaderSearchPaths: extension.HeaderDirs,
		// An extension sees the same pod modules the application does
		// (`inherit! :search_paths` in the Podfile): the .swiftmodule search
		// paths and the public modulemaps. Only the archives of pods it
		// actually imports are linked, via extensionFrameworks.
		ModuleSearchPaths: plugins.ModuleSearchPaths,
		ModuleMaps:        plugins.ModuleMaps,
		MacroPlugins:      macroPlugins,
	}
	frameworks := extensionFrameworks(extension, sdk, built, plugins.PodModules, &inputs)
	binary := filepath.Join(bundle, extension.ExecutableName)
	if _, err := ios.BuildAppExtension(tc, binary, work, extension.MinOS, extension.ModuleName, extension.BridgingHeader, extension.Sources, inputs, frameworks); err != nil {
		return "", fmt.Errorf("build extension %s: %w", extension.TargetName, err)
	}

	info, err := extensionInfoPlist(extension)
	if err != nil {
		return "", err
	}
	if err := compileExtensionAssetCatalogs(extension, work, bundle, info); err != nil {
		return "", err
	}
	if err := writePlist(filepath.Join(bundle, "Info.plist"), info); err != nil {
		return "", err
	}
	return bundle, nil
}

// extensionInfoPlist builds the `.appex` Info.plist from the target's own plist
// with build settings expanded, which is the only place `NSExtension` lives, and
// then fills in the identity keys the loader needs.
func extensionInfoPlist(extension appExtension) (map[string]any, error) {
	data, err := os.ReadFile(extension.InfoPlist)
	if err != nil {
		return nil, err
	}
	text := string(data)
	for iteration := 0; iteration < 12; iteration++ {
		expanded := expandBuildValue(text, extension.BuildSettings)
		if expanded == text {
			break
		}
		text = expanded
	}
	info := make(map[string]any)
	if _, err := plist.Unmarshal([]byte(text), &info); err != nil {
		return nil, fmt.Errorf("parse %s: %w", extension.InfoPlist, err)
	}
	for key, value := range extension.BuildSettings {
		if !strings.HasPrefix(key, "INFOPLIST_KEY_") {
			continue
		}
		if plistKey := strings.TrimPrefix(key, "INFOPLIST_KEY_"); plistKey != "" {
			info[plistKey] = plistBuildSettingValue(value)
		}
	}
	if _, ok := info["NSExtension"]; !ok {
		return nil, fmt.Errorf("extension %s Info.plist %s has no NSExtension dictionary, so iOS would never load the built .appex", extension.TargetName, extension.InfoPlist)
	}
	info["CFBundleIdentifier"] = extension.BundleID
	info["CFBundleExecutable"] = extension.ExecutableName
	info["CFBundlePackageType"] = appExtensionPackageType
	info["MinimumOSVersion"] = extension.MinOS
	info["CFBundleSupportedPlatforms"] = []string{"iPhoneOS"}
	if _, ok := info["CFBundleName"]; !ok {
		info["CFBundleName"] = extension.ProductName
	}
	if _, ok := info["CFBundleShortVersionString"]; !ok {
		info["CFBundleShortVersionString"] = extension.BuildSettings["FLUTTER_BUILD_NAME"]
	}
	if _, ok := info["CFBundleVersion"]; !ok {
		info["CFBundleVersion"] = extension.BuildSettings["FLUTTER_BUILD_NUMBER"]
	}
	if families := deviceFamilies(extension.BuildSettings["TARGETED_DEVICE_FAMILY"]); len(families) > 0 {
		info["UIDeviceFamily"] = families
	}
	return info, nil
}

// compileExtensionAssetCatalogs lowers the extension's own `.xcassets` into the
// `.appex`. A widget's catalog carries the accent and background colors WidgetKit
// resolves by name, which only exist in a compiled catalog.
func compileExtensionAssetCatalogs(extension appExtension, work, bundle string, info map[string]any) error {
	if len(extension.AssetCatalogs) == 0 {
		return nil
	}
	result, err := assetcatalog.Compile(extension.AssetCatalogs, filepath.Join(work, "assetcatalog"), assetcatalog.Options{
		DeploymentTarget: extension.MinOS,
		AppIconName:      strings.Trim(extension.BuildSettings["ASSETCATALOG_COMPILER_APPICON_NAME"], `"`),
	})
	if err != nil {
		return fmt.Errorf("extension %s asset catalog: %w", extension.TargetName, err)
	}
	for _, item := range result.Skipped {
		fmt.Printf("app extension %s asset catalog: skipped %s (%s): %s\n", extension.TargetName, item.Item, item.Kind, item.Reason)
	}
	if result.CarPath != "" {
		if err := copyFile(result.CarPath, filepath.Join(bundle, "Assets.car")); err != nil {
			return err
		}
	}
	for _, file := range result.LooseFiles {
		if err := copyFile(file, filepath.Join(bundle, filepath.Base(file))); err != nil {
			return err
		}
	}
	for key, value := range result.InfoPlistKeys {
		info[key] = value
	}
	return nil
}

// appExtensionSigningReport states, per extension, whether the given
// provisioning profile can actually sign it. An explicit single-App-ID profile
// covers exactly one bundle id, so it can never cover a child bundle id like
// `<app>.ShareExtension`: the extension is still built, and the shortfall is
// reported as data instead of failing the build. An empty result means every
// extension is coverable.
func appExtensionSigningReport(profile string, extensions []appExtension) []string {
	if profile == "" || len(extensions) == 0 {
		return nil
	}
	var report []string
	for _, extension := range extensions {
		if err := ios.ValidateProvisioningProfile(profile, extension.BundleID); err != nil {
			report = append(report, fmt.Sprintf("%s (%s): %v", extension.TargetName, extension.BundleID, err))
		}
	}
	return report
}
