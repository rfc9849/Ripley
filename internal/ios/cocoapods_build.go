package ios

import (
	"bytes"
	"crypto/sha1"
	"debug/macho"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ripley/internal/ios/assetcatalog"
	"ripley/internal/toolchain"
)

type builtPodTarget struct {
	Target  podTarget
	Archive string
}

type podBinaryArtifacts struct {
	FrameworkSearchPaths []string
	StaticFrameworks     []vendoredFramework
	DynamicFrameworks    []vendoredFramework
	StaticLibraries      []string
}

func buildCocoaPods(tc toolchain.Toolchain, projectRoot, sourceDir string, plugins []Plugin, minOS, swiftVersion string) (PluginBuild, error) {
	manifest, podBuildDir, err := resolveCocoaPodsManifest(tc, projectRoot, plugins, minOS, swiftVersion)
	if err != nil {
		return PluginBuild{}, err
	}
	configDir := filepath.Join(podBuildDir, "Release-iphoneos")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return PluginBuild{}, err
	}

	ordered, err := topologicalPodTargets(manifest.Targets)
	if err != nil {
		return PluginBuild{}, err
	}
	binaryArtifacts, err := discoverPodBinaryArtifacts(manifest)
	if err != nil {
		return PluginBuild{}, err
	}

	var built []builtPodTarget
	var podFrameworks []string
	var embeddedFrameworks []string
	var staticLinkerFlags []string
	var resources []string
	var moduleDirs []string
	var moduleMaps []string
	var pluginObjects []string
	var pluginLinkerLibraries []string
	var pluginLinkerFrameworks []string
	podModules := make(map[string]PodModule)
	// Migrate build trees produced by older ripley versions before compiling any
	// target. Legacy derived module maps lived beside emitted .swiftmodule files;
	// a downstream Swift compile can see such a map through SWIFT_INCLUDE_PATHS
	// even when the producer target itself has not been reached yet.
	cleanupLegacyPodModuleArtifacts(manifest, configDir)
	for _, target := range ordered {
		switch target.ProductType {
		case "com.apple.product-type.bundle":
			bundle, err := buildPodResourceBundle(target, configDir, minOS)
			if err != nil {
				return PluginBuild{}, fmt.Errorf("CocoaPod resource target %s: %w", target.Name, err)
			}
			if bundle != "" {
				resources = append(resources, bundle)
			}
		case "com.apple.product-type.library.static", "com.apple.product-type.framework":
			if target.Name == "Flutter" || strings.HasPrefix(target.Name, "Pods-") {
				continue
			}
			archive, err := compilePodStaticTarget(tc, manifest, target, configDir, minOS, binaryArtifacts.FrameworkSearchPaths)
			if err != nil {
				return PluginBuild{}, fmt.Errorf("CocoaPod target %s failed to compile: %w", target.Name, err)
			}

			var framework string
			var targetLinkerFlags []string
			if archive != "" {
				item := builtPodTarget{Target: target, Archive: archive}
				built = append(built, item)
				if target.ProductType == "com.apple.product-type.framework" {
					switch manifest.FrameworkLinkage {
					case "dynamic":
						framework, err = linkDynamicPodFramework(tc, manifest, item, binaryArtifacts, configDir, minOS)
					case "static":
						framework, err = stageStaticPodFramework(manifest, item, configDir, minOS)
						targetLinkerFlags = staticPodTargetLinkerFlags(target)
						staticLinkerFlags = append(staticLinkerFlags, targetLinkerFlags...)
					}
					if err != nil {
						return PluginBuild{}, err
					}
					if framework != "" {
						podFrameworks = append(podFrameworks, framework)
						if manifest.FrameworkLinkage == "dynamic" {
							embeddedFrameworks = append(embeddedFrameworks, framework)
						}
					}
				}
				fmt.Printf("  pod %s\n", target.Name)
			}

			// Classic CocoaPods static-library products expose Swift/clang modules
			// as loose build artifacts. With use_frameworks! (dynamic or static)
			// the framework bundle itself is the module container instead.
			dir, swiftModuleMap := podSwiftModuleInputs(manifest, target, configDir)
			moduleMap := podClangModuleMap(manifest, target)
			moduleName := target.ModuleName
			if moduleName == "" {
				moduleName = target.Name
			}
			podModules[target.Name] = PodModule{
				Name:            target.Name,
				ModuleName:      moduleName,
				ModuleDir:       dir,
				ModuleMap:       moduleMap,
				Archive:         archive,
				Framework:       framework,
				StaticFramework: manifest.FrameworkLinkage == "static" && framework != "",
				LinkerFlags:     append([]string{}, targetLinkerFlags...),
				Dependencies:    append([]string{}, target.Dependencies...),
			}
			if manifest.FrameworkLinkage == "" {
				if dir != "" {
					moduleDirs = append(moduleDirs, dir)
				}
				// Classic CocoaPods products expose clang modules as loose public
				// headers/module maps even when the pod itself is Objective-C only.
				// A Swift AppDelegate can import those modules directly (FirebaseCore
				// is a common example), so export every pod module map to the host,
				// not only maps associated with an emitted .swiftmodule.
				hostModuleMap := moduleMap
				if hostModuleMap == "" {
					hostModuleMap = swiftModuleMap
				}
				if hostModuleMap != "" {
					moduleMaps = append(moduleMaps, hostModuleMap)
				}
			}
		default:
			if len(target.Sources) > 0 && target.Name != "Flutter" && !strings.HasPrefix(target.Name, "Pods-") {
				return PluginBuild{}, fmt.Errorf("CocoaPod target %s has unsupported product type %s", target.Name, target.ProductType)
			}
		}
	}

	if manifest.FrameworkLinkage == "static" {
		staticLinkerFlags = append(staticAggregatePodLinkerFlags(manifest), staticLinkerFlags...)
	}

	var frameworks []string
	switch manifest.FrameworkLinkage {
	case "dynamic":
		frameworks = append(frameworks, podFrameworks...)
		for _, vendored := range binaryArtifacts.DynamicFrameworks {
			frameworks = append(frameworks, vendored.Path)
			embeddedFrameworks = append(embeddedFrameworks, vendored.Path)
		}
	case "static":
		frameworks = append(frameworks, podFrameworks...)
		for _, vendored := range binaryArtifacts.StaticFrameworks {
			frameworks = append(frameworks, vendored.Path)
		}
		for _, vendored := range binaryArtifacts.DynamicFrameworks {
			frameworks = append(frameworks, vendored.Path)
			embeddedFrameworks = append(embeddedFrameworks, vendored.Path)
		}
	default:
		frameworks, err = linkPodProducts(tc, manifest, built, binaryArtifacts, configDir, minOS)
		if err != nil {
			return PluginBuild{}, err
		}
		embeddedFrameworks = append(embeddedFrameworks, frameworks...)
	}
	// A Flutter app may mix CocoaPods-backed plugins with SwiftPM-only plugins.
	// CocoaPods legitimately omits plugins that have no podspec; they must not
	// disappear merely because some other plugin required CocoaPods. Build modern
	// Package.swift plugins with SwiftPM's static product model and keep a direct
	// framework fallback for older native plugins that have neither manifest.
	var swiftPackagePlugins []Plugin
	var directPlugins []Plugin
	for _, plugin := range plugins {
		podspec, _, err := podspecForPlugin(plugin)
		if err != nil {
			return PluginBuild{}, err
		}
		if podspec != "" {
			continue
		}
		packageRoot, err := flutterPluginSwiftPackageRoot(plugin)
		if err != nil {
			return PluginBuild{}, err
		}
		if packageRoot != "" {
			swiftPackagePlugins = append(swiftPackagePlugins, plugin)
		} else {
			directPlugins = append(directPlugins, plugin)
		}
	}
	var registrantPluginIncludes []string
	if len(swiftPackagePlugins) > 0 {
		spmPlugins, err := buildFlutterPluginSwiftPMPackages(tc, projectRoot, swiftPackagePlugins, minOS)
		if err != nil {
			return PluginBuild{}, err
		}
		pluginObjects = append(pluginObjects, spmPlugins.Build.Objects...)
		resources = append(resources, spmPlugins.Build.Resources...)
		moduleDirs = append(moduleDirs, spmPlugins.Build.ModuleSearchPaths...)
		pluginLinkerLibraries = append(pluginLinkerLibraries, spmPlugins.Build.LinkerLibraries...)
		pluginLinkerFrameworks = append(pluginLinkerFrameworks, spmPlugins.Build.LinkerFrameworks...)
		registrantPluginIncludes = append(registrantPluginIncludes, spmPlugins.ObjCHeaderDirs...)
		// Binary SwiftPM dependencies follow the same link/embed contract as the
		// app-level SwiftPM builder.
		frameworks = append(frameworks, spmPlugins.Build.Frameworks...)
		embeddedFrameworks = append(embeddedFrameworks, spmPlugins.Build.Frameworks...)
	}
	work := filepath.Join(projectRoot, "build", "ripley_ios", "plugins")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return PluginBuild{}, err
	}
	for _, plugin := range directPlugins {
		built, err := compilePlugin(tc, plugin, work, minOS)
		if err != nil {
			return PluginBuild{}, fmt.Errorf("plugin %s failed to compile: %w", plugin.Name, err)
		}
		if built.Framework != "" {
			frameworks = append(frameworks, built.Framework)
			embeddedFrameworks = append(embeddedFrameworks, built.Framework)
			fmt.Printf("  built %s\n", filepath.Base(built.Framework))
		}
		for _, vendored := range built.EmbeddedFrameworks {
			frameworks = append(frameworks, vendored)
			embeddedFrameworks = append(embeddedFrameworks, vendored)
		}
	}
	frameworks = uniqueStrings(frameworks)
	embeddedFrameworks = uniqueStrings(embeddedFrameworks)

	// The historical merged product keeps resource bundles inside RipleyPods for
	// compatibility with existing builds. With use_frameworks!, CocoaPods treats
	// resource-bundle targets as separate application resources, so do not copy
	// every bundle into every per-pod framework.
	if len(frameworks) == 1 && filepath.Base(frameworks[0]) == "RipleyPods.framework" {
		if err := embedPodResourceBundles(frameworks[0], resources); err != nil {
			return PluginBuild{}, err
		}
	}

	// GeneratedPluginRegistrant needs CocoaPods' public header/module view. Swift
	// pods additionally need ripley's post-build derived module map, which exposes
	// the generated <Module>-Swift.h without mutating CocoaPods' canonical map.
	// Keep those derived directories first: clangModuleMaps resolves duplicate
	// module declarations by header-path order, so the registrant sees Swift
	// plugin classes while ordinary public headers still resolve from Pods.
	includeDirs := podRegistrantIncludeDirs(manifest, ordered, configDir)
	includeDirs = append(includeDirs, registrantPluginIncludes...)
	registrantFrameworkDirs := append([]string{}, binaryArtifacts.FrameworkSearchPaths...)
	if manifest.FrameworkLinkage != "" {
		for _, framework := range podFrameworks {
			registrantFrameworkDirs = append(registrantFrameworkDirs, filepath.Dir(framework))
		}
	}
	for _, framework := range frameworks {
		registrantFrameworkDirs = append(registrantFrameworkDirs, filepath.Dir(framework))
	}
	registrant, err := BuildRegistrant(tc, plugins, work, sourceDir, minOS, includeDirs, uniqueStrings(registrantFrameworkDirs))
	if err != nil {
		return PluginBuild{}, err
	}

	return PluginBuild{
		Frameworks:         frameworks,
		EmbeddedFrameworks: embeddedFrameworks,
		Objects:            uniqueStrings(pluginObjects),
		Resources:          uniqueStrings(resources),
		RegistrantObject:   registrant,
		ModuleSearchPaths:  uniqueStrings(moduleDirs),
		ModuleMaps:         uniqueStrings(moduleMaps),
		LinkerFlags:        staticLinkerFlags,
		LinkerLibraries:    uniqueStrings(pluginLinkerLibraries),
		LinkerFrameworks:   uniqueStrings(pluginLinkerFrameworks),
		FrameworkLinkage:   manifest.FrameworkLinkage,
		Plugins:            plugins,
		PodModules:         podModules,
	}, nil
}

func topologicalPodTargets(targets []podTarget) ([]podTarget, error) {
	byName := make(map[string]podTarget)
	for _, target := range targets {
		byName[target.Name] = target
	}
	state := make(map[string]int)
	var out []podTarget
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 2 {
			return nil
		}
		if state[name] == 1 {
			return fmt.Errorf("CocoaPods target dependency cycle at %s", name)
		}
		target, ok := byName[name]
		if !ok {
			return nil // aggregate targets are intentionally absent from the native manifest
		}
		state[name] = 1
		for _, dependency := range target.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, target)
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func discoverPodBinaryArtifacts(manifest podManifest) (podBinaryArtifacts, error) {
	var result podBinaryArtifacts
	seenRoots := make(map[string]bool)
	seenFrameworks := make(map[string]bool)
	seenLibraries := make(map[string]bool)
	generatedBuildRoot := filepath.Join(filepath.Dir(manifest.PodsRoot), "build")

	var scanFrameworkRoot func(string) error
	scanFrameworkRoot = func(root string) error {
		if root == "" || seenRoots[root] || !dirExists(root) || pathWithin(root, generatedBuildRoot) {
			return nil
		}
		seenRoots[root] = true
		return filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !d.IsDir() || path == root {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			switch ext {
			case ".xcframework":
				selected, _, err := selectXCFrameworkSlice(path)
				if err != nil {
					return err
				}
				switch strings.ToLower(filepath.Ext(selected)) {
				case ".framework":
					if !seenFrameworks[selected] {
						dynamic, err := frameworkIsDynamic(selected)
						if err != nil {
							return err
						}
						artifact := vendoredFramework{Path: selected, Name: strings.TrimSuffix(filepath.Base(selected), ".framework"), Dynamic: dynamic}
						if dynamic {
							result.DynamicFrameworks = append(result.DynamicFrameworks, artifact)
						} else {
							result.StaticFrameworks = append(result.StaticFrameworks, artifact)
						}
						result.FrameworkSearchPaths = append(result.FrameworkSearchPaths, filepath.Dir(selected))
						seenFrameworks[selected] = true
					}
				case ".a":
					if !seenLibraries[selected] {
						result.StaticLibraries = append(result.StaticLibraries, selected)
						seenLibraries[selected] = true
					}
				default:
					return fmt.Errorf("unsupported XCFramework library slice %s", selected)
				}
				return filepath.SkipDir
			case ".framework":
				if !seenFrameworks[path] {
					dynamic, err := frameworkIsDynamic(path)
					if err != nil {
						return err
					}
					artifact := vendoredFramework{Path: path, Name: strings.TrimSuffix(filepath.Base(path), ".framework"), Dynamic: dynamic}
					if dynamic {
						result.DynamicFrameworks = append(result.DynamicFrameworks, artifact)
					} else {
						result.StaticFrameworks = append(result.StaticFrameworks, artifact)
					}
					result.FrameworkSearchPaths = append(result.FrameworkSearchPaths, filepath.Dir(path))
					seenFrameworks[path] = true
				}
				return filepath.SkipDir
			}
			return nil
		})
	}

	for _, target := range manifest.Targets {
		for _, root := range existingPaths(target.FrameworkSearchPaths) {
			if err := scanFrameworkRoot(root); err != nil {
				return podBinaryArtifacts{}, fmt.Errorf("discover vendored frameworks under %s: %w", root, err)
			}
		}
		for _, root := range existingPaths(target.LibrarySearchPaths) {
			if !dirExists(root) || pathWithin(root, generatedBuildRoot) {
				continue
			}
			entries, err := filepath.Glob(filepath.Join(root, "*.a"))
			if err != nil {
				return podBinaryArtifacts{}, err
			}
			for _, library := range entries {
				// CocoaPods build-output paths also appear here but do not exist yet at
				// discovery time. Existing archives in resolver paths are vendored.
				if !seenLibraries[library] {
					result.StaticLibraries = append(result.StaticLibraries, library)
					seenLibraries[library] = true
				}
			}
		}
	}
	result.FrameworkSearchPaths = uniqueStrings(result.FrameworkSearchPaths)
	result.StaticLibraries = uniqueStrings(result.StaticLibraries)
	return result, nil
}

func pathWithin(path, root string) bool {
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func podObjectName(path string) string {
	sum := sha1.Sum([]byte(path))
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) + "-" + hex.EncodeToString(sum[:6]) + ".o"
}

func existingPaths(values []string) []string {
	var out []string
	for _, value := range values {
		if value == "" || strings.Contains(value, "$(") || strings.Contains(value, "${") {
			continue
		}
		if dirExists(value) || fileExists(value) {
			out = append(out, value)
		}
	}
	return uniqueStrings(out)
}

func podDependencyHeaderPaths(manifest podManifest, target podTarget) []string {
	byName := make(map[string]podTarget, len(manifest.Targets))
	for _, item := range manifest.Targets {
		byName[item.Name] = item
	}
	seen := make(map[string]bool)
	var out []string
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		dependency, ok := byName[name]
		if !ok {
			return
		}
		privateDir := filepath.Join(manifest.PodsRoot, "Headers", "Private", dependency.Name)
		if dirExists(privateDir) {
			out = append(out, privateDir)
		}
		// Framework-mode CocoaPods uses Xcode header maps instead of populating
		// Pods/Headers/Public for every target. Reproduce the dependency header
		// visibility from the PBXHeadersBuildPhase itself.
		for _, header := range dependency.Headers {
			if header.Path != "" && fileExists(header.Path) {
				out = append(out, filepath.Dir(header.Path))
			}
		}
		moduleMap := podPublicModuleMap(dependency, manifest.PodsRoot)
		if fileExists(moduleMap) {
			out = append(out, filepath.Dir(moduleMap))
		}
		for _, transitive := range dependency.Dependencies {
			visit(transitive)
		}
	}
	for _, dependency := range target.Dependencies {
		visit(dependency)
	}
	return uniqueStrings(out)
}

func podDependencyModuleMaps(manifest podManifest, target podTarget) []string {
	byName := make(map[string]podTarget, len(manifest.Targets))
	for _, item := range manifest.Targets {
		byName[item.Name] = item
	}
	var out []string
	for _, name := range target.Dependencies {
		dependency, ok := byName[name]
		if !ok || dependency.ProductType == "com.apple.product-type.bundle" || dependency.Name == "Flutter" {
			continue
		}
		if moduleMap := podClangModuleMap(manifest, dependency); moduleMap != "" {
			out = append(out, moduleMap)
		}
	}
	return uniqueStrings(out)
}

func cleanPodFlags(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || value == "$(inherited)" || value == "${inherited}" {
			continue
		}
		if strings.Contains(value, "$(") || strings.Contains(value, "${") {
			continue
		}
		out = append(out, value)
	}
	return out
}

// podLinkerFlags converts clang-driver linker forwarding syntax into arguments
// accepted by the direct ld64.lld invocations used by ripley. CocoaPods podspecs
// frequently express force-load/rpath options as -Wl,... or -Xlinker because
// Xcode normally invokes clang as the linker driver.
func podLinkerFlags(values []string) []string {
	values = cleanPodFlags(values)
	out := make([]string, 0, len(values))
	for i := 0; i < len(values); i++ {
		value := values[i]
		if strings.HasPrefix(value, "-Wl,") {
			for _, forwarded := range strings.Split(strings.TrimPrefix(value, "-Wl,"), ",") {
				if forwarded != "" {
					out = append(out, forwarded)
				}
			}
			continue
		}
		if value == "-Xlinker" && i+1 < len(values) {
			i++
			out = append(out, values[i])
			continue
		}
		out = append(out, value)
	}
	return out
}

func podTargetNeedsCXXRuntime(target podTarget) bool {
	for _, source := range target.Sources {
		switch podSourceLanguage(source.Path) {
		case podTUCXX, podTUObjCXX:
			return true
		}
	}
	return false
}

// podAuxiliaryModuleMaps finds module maps a dependency keeps outside the
// CocoaPods header tree. A pod can ship a private clang module next to its
// public one — Sentry's `_SentryPrivate` is the canonical example, preserved
// via the podspec's `preserve_paths` and surfaced to the pod's own compile
// through `SWIFT_INCLUDE_PATHS`. When the dependency's Swift sources use
// `@_implementationOnly import` on that module, its serialized interface still
// references the private module's declarations, so a dependent pod's compile
// must be able to load the same map or swiftc silently drops every
// declaration whose signature crosses the boundary.
func podAuxiliaryModuleMaps(manifest podManifest, target podTarget) []string {
	byName := make(map[string]podTarget, len(manifest.Targets))
	for _, item := range manifest.Targets {
		byName[item.Name] = item
	}
	seen := make(map[string]bool)
	var out []string
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		dependency, ok := byName[name]
		if !ok {
			return
		}
		for _, dir := range existingPaths(dependency.SwiftIncludePaths) {
			candidate := filepath.Join(dir, "module.modulemap")
			if fileExists(candidate) {
				out = append(out, candidate)
			}
		}
		for _, transitive := range dependency.Dependencies {
			visit(transitive)
		}
	}
	for _, dependency := range target.Dependencies {
		visit(dependency)
	}
	return uniqueStrings(out)
}

// moduleMapTopLevelNames returns the names of the top-level modules a module
// map declares, skipping nested or explicit submodules.
func moduleMapTopLevelNames(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var names []string
	depth := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if depth == 0 && len(fields) >= 2 && fields[0] == "module" {
			names = append(names, fields[1])
		}
		if depth == 0 && len(fields) >= 3 && fields[0] == "framework" && fields[1] == "module" {
			names = append(names, fields[2])
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	return names
}

// canonicalModuleMaps preserves the first physical map that claims each logical
// clang module name. CocoaPods can surface the same module through public,
// private/preserve_paths, and generated build views; clang never merges those
// declarations and instead fails with "redefinition of module". Callers order
// paths by authority, with canonical public/source maps first.
func canonicalModuleMaps(paths []string) []string {
	seenPaths := make(map[string]bool)
	claimed := make(map[string]bool)
	var out []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		clean := filepath.Clean(path)
		if seenPaths[clean] {
			continue
		}
		seenPaths[clean] = true
		names := moduleMapTopLevelNames(path)
		conflict := false
		for _, name := range names {
			if claimed[name] {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		out = append(out, path)
		for _, name := range names {
			claimed[name] = true
		}
	}
	return out
}

func cleanPodCompilerFlags(manifest podManifest, configDir string, values []string, preferDerived bool) []string {
	values = cleanPodFlags(values)
	out := make([]string, 0, len(values))
	seenModules := make(map[string]bool)
	for i := 0; i < len(values); i++ {
		value := values[i]
		if value == "-Xcc" && i+1 < len(values) {
			rewritten, moduleName := canonicalizePodModuleMapFlag(manifest, configDir, values[i+1], preferDerived)
			if rewritten == "" {
				i++
				continue
			}
			if moduleName != "" && seenModules[moduleName] {
				i++
				continue
			}
			if moduleName != "" {
				seenModules[moduleName] = true
			}
			out = append(out, value, rewritten)
			i++
			continue
		}
		rewritten, moduleName := canonicalizePodModuleMapFlag(manifest, configDir, value, preferDerived)
		if rewritten == "" {
			continue
		}
		if moduleName != "" && seenModules[moduleName] {
			continue
		}
		if moduleName != "" {
			seenModules[moduleName] = true
		}
		out = append(out, rewritten)
	}
	return out
}

func canonicalizePodModuleMapFlag(manifest podManifest, configDir, value string, preferDerived bool) (string, string) {
	const prefix = "-fmodule-map-file="
	if !strings.HasPrefix(value, prefix) {
		return value, ""
	}
	if isResolverFlutterModuleMapFlag(value) {
		return "", "Flutter"
	}
	path := strings.TrimPrefix(value, prefix)
	names := moduleMapTopLevelNames(path)
	moduleName := ""
	if len(names) > 0 {
		moduleName = names[0]
	}
	if moduleName == "" {
		moduleName = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	// Paths under the CocoaPods configuration build directory are Xcode
	// DerivedData-style mirrors. ripley owns a different artifact layout, so never
	// preserve those physical paths: resolve the logical module back to the
	// canonical CocoaPods public/source map (or ripley's isolated derived map).
	if pathWithinDir(path, configDir) {
		if canonical := canonicalPodModuleMap(manifest, configDir, moduleName, preferDerived); canonical != "" {
			return prefix + canonical, moduleName
		}
	}
	return value, moduleName
}

func canonicalPodModuleMap(manifest podManifest, configDir, moduleName string, preferDerived bool) string {
	for _, target := range manifest.Targets {
		name := target.ModuleName
		if name == "" {
			name = target.Name
		}
		if name != moduleName {
			continue
		}
		derived := podDerivedModuleMapPath(configDir, target)
		if preferDerived && fileExists(derived) {
			return derived
		}
		if public := podClangModuleMap(manifest, target); public != "" {
			return public
		}
		if fileExists(derived) {
			return derived
		}
	}
	return ""
}

// cleanupLegacyPodModuleArtifacts removes the old ripley layout where a derived
// <Module>.modulemap and generated <Module>-Swift.h were written directly into
// <config>/<Pod>, the same directory later passed to swiftc as a module search
// path. Keeping migration at graph scope makes an existing incremental tree
// safe before any downstream target can observe stale producer artifacts.
func cleanupLegacyPodModuleArtifacts(manifest podManifest, configDir string) {
	for _, target := range manifest.Targets {
		moduleName := target.ModuleName
		if moduleName == "" {
			moduleName = target.Name
		}
		targetOut := filepath.Join(configDir, target.Name)
		_ = os.Remove(filepath.Join(targetOut, moduleName+".modulemap"))
		_ = os.Remove(filepath.Join(targetOut, moduleName+"-Swift.h"))
	}
}

// podSwiftIncludeSearchPaths keeps CocoaPods source/private include paths, but
// treats directories under the configuration build root as Swift product
// directories. Those are meaningful to swiftc only when they actually contain
// an emitted .swiftmodule. This prevents pure C/ObjC pod product directories —
// and any stale module maps left in them by an older build — from becoming
// implicit clang-module search roots.
func podSwiftIncludeSearchPaths(configDir string, paths []string) []string {
	var out []string
	for _, path := range uniqueStrings(paths) {
		if !dirExists(path) {
			continue
		}
		if !pathWithinDir(path, configDir) || samePath(path, configDir) {
			out = append(out, path)
			continue
		}
		modules, _ := filepath.Glob(filepath.Join(path, "*.swiftmodule"))
		if len(modules) > 0 {
			out = append(out, path)
		}
	}
	return out
}

func moduleMapPathsFromCompilerFlags(values []string) []string {
	const prefix = "-fmodule-map-file="
	var out []string
	for i := 0; i < len(values); i++ {
		value := values[i]
		if value == "-Xcc" && i+1 < len(values) {
			i++
			value = values[i]
		}
		if strings.HasPrefix(value, prefix) {
			out = append(out, strings.TrimPrefix(value, prefix))
		}
	}
	return uniqueStrings(out)
}

func pathWithinDir(path, dir string) bool {
	pathAbs, err1 := filepath.Abs(path)
	dirAbs, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(dirAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func isResolverFlutterModuleMapFlag(value string) bool {
	return strings.Contains(value, "Headers/Public/Flutter/Flutter.modulemap")
}

// podTULanguage classifies a CocoaPods translation unit by extension. The
// language drives module support: clang rejects -fmodules for a pure C++ TU
// (Apple's SDK headers wrap C declarations in `extern "C"`, and importing a
// module inside a language-linkage specification is an error), while Xcode's
// CLANG_ENABLE_MODULES applies to C, ObjC and ObjC++ only.
type podTULanguage int

const (
	podTUUnknown podTULanguage = iota
	podTUC
	podTUObjC
	podTUCXX
	podTUObjCXX
)

func podSourceLanguage(path string) podTULanguage {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".c":
		return podTUC
	case ".m":
		return podTUObjC
	case ".cc", ".cpp", ".cxx", ".c++", ".cp":
		return podTUCXX
	case ".mm":
		return podTUObjCXX
	}
	return podTUUnknown
}

// podTUSupportsModules reports whether -fmodules may be passed for this TU.
func podTUSupportsModules(lang podTULanguage) bool { return lang != podTUCXX }

// podTUUsesCXXFlags reports whether OTHER_CPLUSPLUSFLAGS applies to this TU.
func podTUUsesCXXFlags(lang podTULanguage) bool {
	return lang == podTUCXX || lang == podTUObjCXX
}

var podHeaderDirectiveRE = regexp.MustCompile(`(?m)^[\t ]*#[\t ]*(?:include|import)[\t ]*[<"]([^">]+)[">]`)

// podCaseInsensitiveHeaderAliases reproduces the case-insensitive header lookup
// CocoaPods normally gets from the default macOS filesystem. Linux build hosts
// are case-sensitive, so a pod source such as `#import "SYMetadataEXIF.h"`
// cannot see a declared `SYMetadataExif.h` even though the same source builds
// under Xcode. Create aliases only for spellings that the target actually
// imports, and keep them in ripley's derived build tree rather than mutating the
// pod source (which may live in Flutter's shared pub cache).
func podCaseInsensitiveHeaderAliases(target podTarget, work string) (string, error) {
	root := filepath.Join(work, "case-insensitive-headers")
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}

	type candidate struct {
		path string
		base string
	}
	byFoldedBase := make(map[string][]candidate)
	seenCandidate := make(map[string]bool)
	var inputs []string
	for _, header := range target.Headers {
		if header.Path == "" || !fileExists(header.Path) {
			continue
		}
		path, err := filepath.Abs(header.Path)
		if err != nil {
			return "", err
		}
		real := path
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			real = resolved
		}
		key := strings.ToLower(filepath.Base(path))
		identity := key + "\x00" + real
		if !seenCandidate[identity] {
			byFoldedBase[key] = append(byFoldedBase[key], candidate{path: path, base: filepath.Base(path)})
			seenCandidate[identity] = true
		}
		inputs = append(inputs, path)
	}
	for _, source := range target.Sources {
		if source.Path != "" && fileExists(source.Path) {
			inputs = append(inputs, source.Path)
		}
	}

	aliases := make(map[string]string)
	for _, input := range uniqueStrings(inputs) {
		data, err := os.ReadFile(input)
		if err != nil {
			return "", err
		}
		for _, match := range podHeaderDirectiveRE.FindAllSubmatch(data, -1) {
			if len(match) < 2 {
				continue
			}
			requested := filepath.Clean(filepath.FromSlash(strings.TrimSpace(string(match[1]))))
			if requested == "." || filepath.IsAbs(requested) || requested == ".." || strings.HasPrefix(requested, ".."+string(filepath.Separator)) {
				continue
			}
			requestedBase := filepath.Base(requested)
			candidates := byFoldedBase[strings.ToLower(requestedBase)]
			if len(candidates) != 1 || candidates[0].base == requestedBase {
				continue
			}
			// EqualFold is intentional rather than generating arbitrary case
			// permutations: we mirror only a lookup that succeeds on macOS.
			if !strings.EqualFold(candidates[0].base, requestedBase) {
				continue
			}
			aliases[requested] = candidates[0].path
		}
	}
	if len(aliases) == 0 {
		return "", nil
	}
	for requested, source := range aliases {
		dst := filepath.Join(root, requested)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.Symlink(source, dst); err != nil {
			return "", err
		}
	}
	return root, nil
}

func podOwnTargetHeaderMap(target podTarget, work string) (root, moduleDir string, err error) {
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = target.Name
	}
	root = filepath.Join(work, "own-header-map")
	moduleDir = filepath.Join(root, moduleName)
	if err := os.RemoveAll(root); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		return "", "", err
	}
	type candidate struct {
		path     string
		priority int
	}
	byName := make(map[string]candidate)
	for _, header := range target.Headers {
		priority := 1 // Project/compile-only headers are still visible to the target itself.
		if len(header.Attributes) == 0 || containsString(header.Attributes, "Public") {
			priority = 3
		} else if containsString(header.Attributes, "Private") {
			priority = 2
		}
		name := filepath.Base(header.Path)
		if name == "" || !fileExists(header.Path) {
			continue
		}
		if old, ok := byName[name]; ok && old.priority >= priority {
			continue
		}
		byName[name] = candidate{path: header.Path, priority: priority}
	}
	for name, item := range byName {
		// A symlink is intentional: both quote imports from the source tree and
		// <Module/Header.h> imports through this header-map surrogate resolve to
		// the same physical file identity. Copying would recreate the duplicate
		// declaration problem this mapping exists to avoid.
		dst := filepath.Join(moduleDir, name)
		if err := os.Symlink(item.path, dst); err != nil {
			return "", "", err
		}
	}
	return root, moduleDir, nil
}

func exposeOwnTargetGeneratedHeader(moduleDir, header string) error {
	if moduleDir == "" || header == "" || !fileExists(header) {
		return nil
	}
	dst := filepath.Join(moduleDir, filepath.Base(header))
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(header, dst)
}

func podOwnTargetModuleMap(target podTarget, sourceMap, work string) (string, error) {
	if sourceMap == "" || !fileExists(sourceMap) {
		return "", nil
	}
	data, err := os.ReadFile(sourceMap)
	if err != nil {
		return "", err
	}
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = target.Name
	}
	text := string(data)
	frameworkDecl := regexp.MustCompile(`(?m)^([\t ]*)framework[\t ]+module[\t ]+` + regexp.QuoteMeta(moduleName) + `\b`)
	text = frameworkDecl.ReplaceAllString(text, `${1}module `+moduleName)

	dir := filepath.Join(work, "own-clang-module")
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Generated CocoaPods module maps normally reference an umbrella header next
	// to the map. Keep those relative references valid without mutating Pods/.
	headers, err := filepath.Glob(filepath.Join(filepath.Dir(sourceMap), "*.h"))
	if err != nil {
		return "", err
	}
	for _, header := range headers {
		if err := copyFile(header, filepath.Join(dir, filepath.Base(header))); err != nil {
			return "", err
		}
	}
	derived := filepath.Join(dir, "module.modulemap")
	if err := os.WriteFile(derived, []byte(text), 0o644); err != nil {
		return "", err
	}
	return derived, nil
}

func compilePodStaticTarget(tc toolchain.Toolchain, manifest podManifest, target podTarget, configDir, appMinOS string, extraFrameworkPaths []string) (string, error) {
	if err := runPodShellPhases(tc, manifest, target, configDir, appMinOS, podScriptBeforeHeaders); err != nil {
		return "", err
	}
	if err := runPodShellPhases(tc, manifest, target, configDir, appMinOS, podScriptAfterHeaders); err != nil {
		return "", err
	}
	var swiftSources []podSource
	var clangSources []podSource
	for _, source := range target.Sources {
		if strings.EqualFold(filepath.Ext(source.Path), ".swift") {
			swiftSources = append(swiftSources, source)
			continue
		}
		if podSourceLanguage(source.Path) != podTUUnknown {
			clangSources = append(clangSources, source)
		}
	}
	if len(swiftSources) == 0 && len(clangSources) == 0 {
		if err := runPodShellPhases(tc, manifest, target, configDir, appMinOS, podScriptAfterSources); err != nil {
			return "", err
		}
		return "", nil
	}
	minOS := maxVersion(target.DeploymentTarget, appMinOS)
	targetOut := filepath.Join(configDir, target.Name)
	objectsDir := filepath.Join(targetOut, "objects")
	if err := os.MkdirAll(objectsDir, 0o755); err != nil {
		return "", err
	}
	var ownHeaderRoot, ownHeaderModuleDir string
	if manifest.FrameworkLinkage != "" && target.ProductType == "com.apple.product-type.framework" && len(target.Headers) > 0 {
		var err error
		ownHeaderRoot, ownHeaderModuleDir, err = podOwnTargetHeaderMap(target, objectsDir)
		if err != nil {
			return "", err
		}
	}

	// Keep directories passed to Swift as `-I` free of clang module maps. Older
	// ripley builds wrote a derived <Module>.modulemap beside the emitted
	// .swiftmodule; downstream swiftc then auto-discovered that mirror while ripley
	// also passed CocoaPods' canonical public map explicitly, causing a hard
	// "redefinition of module". Clean the legacy layout for every product mode.
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = target.Name
	}
	_ = os.Remove(filepath.Join(targetOut, moduleName+".modulemap"))
	_ = os.Remove(filepath.Join(targetOut, moduleName+"-Swift.h"))

	if manifest.FrameworkLinkage != "" && target.ProductType == "com.apple.product-type.framework" {
		product := target.ProductName
		if product == "" {
			product = target.Name
		}
		if err := os.RemoveAll(filepath.Join(targetOut, product+".framework")); err != nil {
			return "", err
		}
		if _, err := stagePodFrameworkMetadata(manifest, target, configDir, minOS); err != nil {
			return "", err
		}
	}

	caseInsensitiveHeaders, err := podCaseInsensitiveHeaderAliases(target, objectsDir)
	if err != nil {
		return "", err
	}
	var headerPaths []string
	if caseInsensitiveHeaders != "" {
		headerPaths = append(headerPaths, caseInsensitiveHeaders)
	}
	headerPaths = append(headerPaths, existingPaths(target.HeaderSearchPaths)...)
	headerPaths = append(headerPaths, podDependencyHeaderPaths(manifest, target)...)
	// Xcode gives CocoaPods targets a generated header map, so source files can
	// include another header from the same target by basename even when it lives
	// in a sibling directory. ripley invokes clang directly and has no .hmap reader;
	// expose the parent directories of the target's declared headers to reproduce
	// that lookup behavior (framework-mode pods rely on it heavily).
	for _, header := range target.Headers {
		if header.Path != "" && fileExists(header.Path) {
			headerPaths = append(headerPaths, filepath.Dir(header.Path))
		}
	}
	frameworkPaths := append(existingPaths(target.FrameworkSearchPaths), extraFrameworkPaths...)
	if manifest.FrameworkLinkage != "" && target.ProductType == "com.apple.product-type.framework" {
		frameworkPaths = append(frameworkPaths, targetOut)
	}
	frameworkPaths = uniqueStrings(frameworkPaths)
	for _, fallback := range []string{
		filepath.Join(manifest.PodsRoot, "Headers", "Public"),
		filepath.Join(manifest.PodsRoot, "Headers", "Private"),
		filepath.Join(manifest.PodsRoot, "Headers", "Private", target.Name),
	} {
		if dirExists(fallback) {
			headerPaths = append(headerPaths, fallback)
		}
	}
	headerPaths = uniqueStrings(headerPaths)

	var objects []string
	if len(swiftSources) > 0 {
		swiftc, err := swiftCompiler(tc)
		if err != nil {
			return "", err
		}
		flags, err := commonSwiftFlags(tc, minOS)
		if err != nil {
			return "", err
		}
		moduleName := target.ModuleName
		if moduleName == "" {
			moduleName = target.Name
		}
		obj := filepath.Join(objectsDir, moduleName+"-swift.o")
		module := filepath.Join(targetOut, moduleName+".swiftmodule")
		publicModuleMap := podPublicModuleMap(target, manifest.PodsRoot)
		publicHeaders := filepath.Dir(publicModuleMap)
		if err := os.MkdirAll(publicHeaders, 0o755); err != nil {
			return "", err
		}
		ownModuleMap, err := podOwnTargetModuleMap(target, publicModuleMap, objectsDir)
		if err != nil {
			return "", err
		}
		swiftHeader := filepath.Join(publicHeaders, moduleName+"-Swift.h")
		args := append(flags, "-module-name", moduleName)
		baseFrameworkSearch, err := frameworkSearch(tc)
		if err != nil {
			return "", err
		}
		args = append(args, baseFrameworkSearch...)
		if target.SwiftVersion != "" {
			args = append(args, "-swift-version", swiftLanguageVersion(target.SwiftVersion))
		}
		if ownHeaderRoot != "" {
			args = append(args, "-Xcc", "-I", "-Xcc", ownHeaderRoot, "-Xcc", "-I", "-Xcc", ownHeaderModuleDir)
		}
		for _, path := range headerPaths {
			args = append(args, "-Xcc", "-I", "-Xcc", path)
		}
		for _, path := range podSwiftIncludeSearchPaths(configDir, target.SwiftIncludePaths) {
			args = append(args, "-I", path)
		}
		// Direct dependency Swift modules are emitted into
		// <config>/<Pod>/<Module>.swiftmodule by this builder. Xcode normally
		// exposes the same products through its target dependency graph; make
		// them explicit for direct swiftc invocations, especially under
		// use_frameworks! where CocoaPods no longer relies on Headers/Public.
		var dependencyModuleDirs []string
		for _, dependency := range target.Dependencies {
			dependencyModuleDirs = append(dependencyModuleDirs, filepath.Join(configDir, dependency))
		}
		for _, depDir := range podSwiftIncludeSearchPaths(configDir, dependencyModuleDirs) {
			args = append(args, "-I", depDir)
		}
		args = append(args, "-I", configDir)
		for _, path := range frameworkPaths {
			// A framework target's own Swift compile must not import the staged
			// product as its underlying ObjC module. Xcode builds that module from
			// the source/header-map view. Importing the staged copy while a private
			// source module includes the same public headers by path produces two
			// physical header identities (Sentry is a real example) and duplicate
			// declarations. Downstream targets still consume the finished framework.
			if ownModuleMap != "" && samePath(path, targetOut) {
				continue
			}
			args = append(args, "-F", path)
		}
		swiftExtraFlags := cleanPodCompilerFlags(manifest, configDir, target.OtherSwiftFlags, false)
		args = append(args, swiftExtraFlags...)
		if target.ProductType == "com.apple.product-type.framework" && len(target.Headers) > 0 {
			if ownModuleMap != "" {
				args = append(args, "-Xcc", "-fmodule-map-file="+ownModuleMap, "-Xcc", "-I"+filepath.Dir(ownModuleMap))
			}
			args = append(args, "-import-underlying-module")
		}
		// Dependencies may declare private clang modules outside the header
		// tree (e.g. Sentry's `_SentryPrivate`, shipped via preserve_paths and
		// SWIFT_INCLUDE_PATHS). When such a dependency's Swift interface was
		// built with `@_implementationOnly import`, its serialized decls still
		// reference the private module, so this compile must load the same map
		// or swiftc silently drops every declaration whose signature crosses
		// the boundary. The generated stub performs the import; the map flag
		// makes the module findable.
		var dependencyMaps []string
		if manifest.FrameworkLinkage == "" {
			dependencyMaps = podDependencyModuleMaps(manifest, target)
		}
		auxiliaryMaps := podAuxiliaryModuleMaps(manifest, target)
		flagMaps := moduleMapPathsFromCompilerFlags(swiftExtraFlags)
		allMaps := append(append(append([]string{}, flagMaps...), dependencyMaps...), auxiliaryMaps...)
		selectedMaps := canonicalModuleMaps(allMaps)
		flagMapSet := make(map[string]bool, len(flagMaps))
		for _, moduleMap := range flagMaps {
			flagMapSet[filepath.Clean(moduleMap)] = true
		}
		selectedSet := make(map[string]bool, len(selectedMaps))
		for _, moduleMap := range selectedMaps {
			clean := filepath.Clean(moduleMap)
			selectedSet[clean] = true
			if !flagMapSet[clean] {
				args = append(args, "-Xcc", "-fmodule-map-file="+moduleMap)
			}
		}
		var auxImports []string
		for _, moduleMap := range auxiliaryMaps {
			if selectedSet[filepath.Clean(moduleMap)] {
				auxImports = append(auxImports, moduleMapTopLevelNames(moduleMap)...)
			}
		}
		if len(auxImports) > 0 {
			stub := filepath.Join(objectsDir, moduleName+"-aux-imports.swift")
			var lines []string
			for _, name := range uniqueStrings(auxImports) {
				lines = append(lines, "@_implementationOnly import "+name)
			}
			if err := os.WriteFile(stub, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				return "", err
			}
			args = append(args, stub)
		}
		for _, source := range swiftSources {
			args = append(args, source.Path)
		}
		args = append(args, "-emit-module", "-emit-module-path", module, "-emit-objc-header-path", swiftHeader, "-o", obj)
		if err := run("", nil, swiftc, args...); err != nil {
			return "", err
		}
		if err := exposeOwnTargetGeneratedHeader(ownHeaderModuleDir, swiftHeader); err != nil {
			return "", err
		}
		objects = append(objects, obj)
		if err := exposeSwiftPodModule(configDir, target, publicModuleMap, swiftHeader); err != nil {
			return "", err
		}
	}

	if len(clangSources) > 0 {
		clang, err := pluginClang(tc)
		if err != nil {
			return "", err
		}
		sdk, err := tc.IOSSDK()
		if err != nil {
			return "", err
		}
		search, err := frameworkSearch(tc)
		if err != nil {
			return "", err
		}
		for _, path := range frameworkPaths {
			// The current target's staged framework is a product, not an input to
			// its own ObjC/C-family compilation. Keeping it on -F makes angle
			// imports resolve to copied public headers while quote imports resolve
			// to source headers, producing duplicate declarations in mixed targets
			// such as Sentry. Dependency framework paths remain intact.
			if manifest.FrameworkLinkage != "" && target.ProductType == "com.apple.product-type.framework" && samePath(path, targetOut) {
				continue
			}
			search = append(search, "-F", path)
		}
		definitions := cleanPodFlags(target.PreprocessorDefinitions)
		for _, source := range clangSources {
			obj := filepath.Join(objectsDir, podObjectName(source.Path))
			lang := podSourceLanguage(source.Path)
			args := []string{"-target", "arm64-apple-ios" + minOS, "-isysroot", sdk, "-O2"}
			if podTUSupportsModules(lang) {
				args = append(args, "-fmodules")
			}
			if target.ArcEnabled {
				args = append(args, "-fobjc-arc")
			}
			for _, definition := range definitions {
				args = append(args, "-D"+definition)
			}
			if ownHeaderRoot != "" {
				args = append(args, "-I", ownHeaderRoot, "-I", ownHeaderModuleDir)
			}
			for _, path := range headerPaths {
				args = append(args, "-I", path)
			}
			if target.PrefixHeader != "" && fileExists(target.PrefixHeader) {
				args = append(args, "-include", target.PrefixHeader)
			}
			args = append(args, search...)
			args = append(args, cleanPodCompilerFlags(manifest, configDir, target.OtherCFlags, true)...)
			if podTUUsesCXXFlags(lang) {
				args = append(args, cleanPodCompilerFlags(manifest, configDir, target.OtherCXXFlags, true)...)
			}
			args = append(args, cleanPodCompilerFlags(manifest, configDir, source.Flags, true)...)
			args = append(args, "-c", source.Path, "-o", obj)
			if err := run("", nil, clang, args...); err != nil {
				return "", err
			}
			objects = append(objects, obj)
		}
	}

	if len(objects) == 0 {
		if err := runPodShellPhases(tc, manifest, target, configDir, appMinOS, podScriptAfterSources); err != nil {
			return "", err
		}
		return "", nil
	}
	ar, err := exec.LookPath("llvm-ar")
	if err != nil {
		ar, err = exec.LookPath("ar")
		if err != nil {
			return "", errorsNew("llvm-ar/ar not found for CocoaPods static library build")
		}
	}
	product := target.ProductName
	if product == "" {
		product = target.Name
	}
	archiveBase := "lib" + strings.TrimPrefix(product, "lib")
	// A framework target may have a script phase that produces a static archive
	// with the conventional lib<Product>.a name and then force-loads it from
	// OTHER_LDFLAGS (cargokit does exactly this). Xcode never overwrites that
	// generated input with the framework target's compiled objects. Keep ripley's
	// intermediate object archive under a private name so the two products stay
	// distinct and the generated archive is linked exactly through its declared
	// linker flag.
	if manifest.FrameworkLinkage != "" && target.ProductType == "com.apple.product-type.framework" {
		archiveBase += "-ripley-target"
	}
	archive := filepath.Join(targetOut, archiveBase+".a")
	args := append([]string{"crs", archive}, objects...)
	if err := run("", nil, ar, args...); err != nil {
		return "", err
	}
	if err := runPodShellPhases(tc, manifest, target, configDir, appMinOS, podScriptAfterSources); err != nil {
		return "", err
	}
	return archive, nil
}

func errorsNew(message string) error { return fmt.Errorf("%s", message) }

func podPublicModuleMap(target podTarget, podsRoot string) string {
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = target.Name
	}
	declaresModule := func(path string) bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		text := string(data)
		return strings.Contains(text, "module "+moduleName+" {") || strings.Contains(text, "framework module "+moduleName+" {")
	}
	for _, flags := range [][]string{target.OtherSwiftFlags, target.OtherCFlags, target.OtherCXXFlags} {
		for _, flag := range flags {
			const prefix = "-fmodule-map-file="
			if !strings.HasPrefix(flag, prefix) {
				continue
			}
			path := strings.TrimPrefix(flag, prefix)
			if strings.Contains(filepath.ToSlash(path), "/Headers/Public/") && declaresModule(path) {
				return path
			}
		}
	}
	publicRoot := filepath.Join(podsRoot, "Headers", "Public")
	var found string
	_ = filepath.WalkDir(publicRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" || d.IsDir() || filepath.Ext(path) != ".modulemap" {
			return nil
		}
		if declaresModule(path) {
			found = path
		}
		return nil
	})
	if found != "" {
		return found
	}
	// Under use_frameworks! CocoaPods keeps the generated framework module map
	// in Target Support Files instead of the public header symlink tree.
	supportMap := filepath.Join(podsRoot, "Target Support Files", target.Name, moduleName+".modulemap")
	if fileExists(supportMap) {
		return supportMap
	}
	return filepath.Join(publicRoot, moduleName, moduleName+".modulemap")
}

var objcPublicDeclRE = regexp.MustCompile(`(?m)^[\t ]*@(interface|protocol)[\t ]+([A-Za-z_][A-Za-z0-9_]*)\b`)

// swiftHeaderConflictsWithPublicHeaders reports whether adding a generated
// <Module>-Swift.h to a pod's clang module would redeclare an Objective-C class
// or protocol that the pod already publishes by hand. cupertino_http is a real
// example: CUPHTTPStreamingTask.h declares CUPHTTPStreamingTask while Swift's
// generated header exports the same @objc class. Xcode keeps those views
// separate; putting both headers in one clang module produces "duplicate
// interface definition" when Swift imports the pod.
func swiftHeaderConflictsWithPublicHeaders(publicDir, swiftHeader string) bool {
	generated, err := os.ReadFile(swiftHeader)
	if err != nil {
		return false
	}
	generatedNames := make(map[string]bool)
	for _, match := range objcPublicDeclRE.FindAllSubmatch(generated, -1) {
		if len(match) >= 3 {
			generatedNames[string(match[2])] = true
		}
	}
	if len(generatedNames) == 0 {
		return false
	}

	headers, _ := filepath.Glob(filepath.Join(publicDir, "*.h"))
	for _, header := range headers {
		if samePath(header, swiftHeader) {
			continue
		}
		data, err := os.ReadFile(header)
		if err != nil {
			continue
		}
		for _, match := range objcPublicDeclRE.FindAllSubmatch(data, -1) {
			if len(match) >= 3 && generatedNames[string(match[2])] {
				return true
			}
		}
	}
	return false
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(aa) == filepath.Clean(bb)
}

func exposeSwiftPodModule(configDir string, target podTarget, publicModuleMap, swiftHeader string) error {
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = target.Name
	}
	publicDir := filepath.Dir(publicModuleMap)
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		return err
	}

	// Treat CocoaPods' generated module map as immutable input. ripley used to add
	// <Module>-Swift.h to that file in place; the next incremental build then
	// consumed the modified map before swiftc had regenerated the header and
	// failed while importing its own underlying module. Build a derived map in
	// targetOut instead, and use that map only after the Swift compile finishes.
	moduleText := ""
	if data, err := os.ReadFile(publicModuleMap); err == nil {
		moduleText = string(data)
	}
	if moduleText == "" {
		moduleText = "module " + moduleName + " {\n  export *\n}\n"
	}
	headerLine := "  header \"" + filepath.Base(swiftHeader) + "\""
	// Strip the exact member written by older ripley versions before deriving the
	// current map. This also makes an existing contaminated Pods tree harmless.
	moduleText = strings.ReplaceAll(moduleText, headerLine+"\n", "")
	if !swiftHeaderConflictsWithPublicHeaders(publicDir, swiftHeader) {
		idx := strings.LastIndex(moduleText, "}")
		if idx < 0 {
			return fmt.Errorf("invalid module map %s: missing closing brace", publicModuleMap)
		}
		moduleText = moduleText[:idx] + headerLine + "\n" + moduleText[idx:]
	}

	builtMapDir := podDerivedModuleMapDir(configDir, target)
	if err := os.MkdirAll(builtMapDir, 0o755); err != nil {
		return err
	}
	if err := replaceFile(swiftHeader, filepath.Join(builtMapDir, filepath.Base(swiftHeader))); err != nil {
		return err
	}
	// A derived module map may name an umbrella or explicit public headers. Keep
	// those relative paths valid without touching CocoaPods' source tree.
	headers, _ := filepath.Glob(filepath.Join(publicDir, "*.h"))
	for _, header := range headers {
		if samePath(header, swiftHeader) {
			continue
		}
		// Preserve header identity rather than copying CocoaPods' public view.
		// Generated Swift headers often import the same ObjC/Pigeon headers via
		// <Module/Header.h>; a physical copy here makes #import see two distinct
		// files and can redeclare every enum/interface (camera_avfoundation is a
		// concrete example). Symlinks mirror Xcode/CocoaPods header maps and also
		// make incremental re-staging safe for owner-read-only public headers.
		dst := filepath.Join(builtMapDir, filepath.Base(header))
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return err
		}
		absHeader, err := filepath.Abs(header)
		if err != nil {
			return err
		}
		if err := os.Symlink(absHeader, dst); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(builtMapDir, moduleName+".modulemap"), []byte(moduleText), 0o644)
}

// podSwiftModuleInputs reports what a downstream compile needs in order to
// resolve `import <PodModule>` for a pod target's Swift module: the directory
// holding the emitted `.swiftmodule`/`.swiftdoc`, and the CocoaPods public
// module map describing the target's ObjC half (which exposeSwiftPodModule has
// augmented with the generated `-Swift.h`). Both are empty for a target that
// emitted no Swift module.
//
// The module map must be passed explicitly. CocoaPods names its per-target map
// `<Module>.modulemap` rather than `module.modulemap`, so clang never discovers
// it from a header search path; measured against immich, `-I <dir>` alone gets
// as far as "cannot load underlying module for 'native_video_player'".
//
// The public map is returned rather than the mirror exposeSwiftPodModule writes
// in its isolated clang-module directory: an umbrella header resolves its `#import`s
// relative to the map's own directory, and only the public header tree holds
// the pod's other public headers.
// podClangModuleMap returns the public clang module map for any modular pod,
// independent of whether that pod also emits a Swift module. Downstream ObjC
// targets can @import ObjC-only pods just as Swift-backed pods can.
func podClangModuleMap(manifest podManifest, target podTarget) string {
	moduleMap := podPublicModuleMap(target, manifest.PodsRoot)
	if !fileExists(moduleMap) {
		return ""
	}
	return moduleMap
}

func podSwiftModuleInputs(manifest podManifest, target podTarget, configDir string) (dir, moduleMap string) {
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = target.Name
	}
	dir = filepath.Join(configDir, target.Name)
	if !fileExists(filepath.Join(dir, moduleName+".swiftmodule")) {
		return "", ""
	}
	// The compiled Swift module directory is intentionally module-map-free.
	// Prefer CocoaPods' canonical public/source map and only fall back to ripley's
	// derived map for a Swift target that has no public clang map at all.
	moduleMap = podPublicModuleMap(target, manifest.PodsRoot)
	if !fileExists(moduleMap) {
		moduleMap = podDerivedModuleMapPath(configDir, target)
	}
	if !fileExists(moduleMap) {
		moduleMap = ""
	}
	return dir, moduleMap
}

func podRegistrantIncludeDirs(manifest podManifest, targets []podTarget, configDir string) []string {
	var dirs []string
	for _, target := range targets {
		derivedMap := podDerivedModuleMapPath(configDir, target)
		if fileExists(derivedMap) {
			dirs = append(dirs, filepath.Dir(derivedMap))
		}
	}

	publicRoot := filepath.Join(manifest.PodsRoot, "Headers", "Public")
	dirs = append(dirs, publicRoot)
	if entries, err := os.ReadDir(publicRoot); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(publicRoot, entry.Name()))
			}
		}
	}
	return uniqueStrings(dirs)
}

func podDerivedModuleMapDir(configDir string, target podTarget) string {
	return filepath.Join(configDir, target.Name, "clang-module")
}

func podDerivedModuleMapPath(configDir string, target podTarget) string {
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = target.Name
	}
	return filepath.Join(podDerivedModuleMapDir(configDir, target), moduleName+".modulemap")
}

func embedPodResourceBundles(framework string, bundles []string) error {
	if framework == "" || len(bundles) == 0 {
		return nil
	}
	for _, bundle := range uniqueStrings(bundles) {
		if !dirExists(bundle) {
			continue
		}
		dst := filepath.Join(framework, filepath.Base(bundle))
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := copyDir(bundle, dst); err != nil {
			return fmt.Errorf("embed CocoaPods resource bundle %s in %s: %w", bundle, framework, err)
		}
	}
	return nil
}

func buildPodResourceBundle(target podTarget, configDir, minOS string) (string, error) {
	if len(target.Resources) == 0 {
		return "", nil
	}
	name := target.ProductName
	if name == "" {
		name = strings.TrimSuffix(target.Name, ".bundle")
	}
	bundle := filepath.Join(configDir, name+".bundle")
	if err := os.RemoveAll(bundle); err != nil {
		return "", err
	}
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		return "", err
	}
	for _, resource := range target.Resources {
		if filepath.Ext(resource) == ".xcassets" {
			// Pod resource bundles carry plain image/data/color sets; an
			// appiconset belongs to an application target, so the loose files
			// and CFBundleIcons keys Compile also produces have nothing to
			// contribute here and are intentionally ignored.
			res, err := assetcatalog.Compile([]string{resource}, bundle, assetcatalog.Options{
				DeploymentTarget: maxVersion(target.DeploymentTarget, minOS),
			})
			if err != nil {
				return "", fmt.Errorf("compile asset catalog %s: %w", resource, err)
			}
			if res.CarPath == "" {
				return "", fmt.Errorf("asset catalog %s produced no compiled archive", resource)
			}
			continue
		}
		dst := filepath.Join(bundle, filepath.Base(resource))
		if dirExists(resource) {
			if err := copyDir(resource, dst); err != nil {
				return "", err
			}
		} else if fileExists(resource) {
			if err := copyFile(resource, dst); err != nil {
				return "", err
			}
		}
	}
	info := map[string]any{
		"CFBundleIdentifier":         "org.cocoapods." + strings.ReplaceAll(name, "_", "-"),
		"CFBundleName":               name,
		"CFBundlePackageType":        "BNDL",
		"CFBundleShortVersionString": "1.0",
		"CFBundleVersion":            "1",
		"MinimumOSVersion":           maxVersion(target.DeploymentTarget, minOS),
	}
	if err := writePlist(filepath.Join(bundle, "Info.plist"), info); err != nil {
		return "", err
	}
	return bundle, nil
}

func staticFrameworkLinkInput(framework vendoredFramework, configDir string) (string, bool, error) {
	binaryPath := filepath.Join(framework.Path, framework.Name)
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return "", false, err
	}
	payload, universal, err := universalArm64Slice(data)
	if err != nil {
		return "", false, fmt.Errorf("select arm64 slice for %s: %w", binaryPath, err)
	}
	if !universal {
		payload = data
	}

	forceLoad := len(payload) >= 8 && string(payload[:8]) == "!<arch>\n"
	if !forceLoad {
		file, openErr := macho.NewFile(bytes.NewReader(payload))
		if openErr != nil {
			return "", false, fmt.Errorf("inspect static framework %s: %w", binaryPath, openErr)
		}
		typ := file.Type
		_ = file.Close()
		if typ != macho.TypeObj {
			return "", false, fmt.Errorf("static framework %s contains unsupported Mach-O type %v", binaryPath, typ)
		}
	}
	if !universal {
		return binaryPath, forceLoad, nil
	}

	ext := ".o"
	if forceLoad {
		ext = ".a"
	}
	outDir := filepath.Join(filepath.Dir(configDir), "vendor-slices", framework.Name)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", false, err
	}
	out := filepath.Join(outDir, framework.Name+"-arm64"+ext)
	if err := os.WriteFile(out, payload, 0o644); err != nil {
		return "", false, err
	}
	return out, forceLoad, nil
}

func linkPodProducts(tc toolchain.Toolchain, manifest podManifest, built []builtPodTarget, binaryArtifacts podBinaryArtifacts, configDir, minOS string) ([]string, error) {
	if manifest.FrameworkLinkage == "dynamic" {
		return linkDynamicPodFrameworks(tc, manifest, built, binaryArtifacts, configDir, minOS)
	}
	framework, vendored, err := linkPodsFramework(tc, manifest, built, binaryArtifacts, configDir, minOS)
	if err != nil {
		return nil, err
	}
	var out []string
	if framework != "" {
		out = append(out, framework)
	}
	out = append(out, vendored...)
	return uniqueStrings(out), nil
}

func podFrameworkProductHeaders(target podTarget) (publicHeaders, privateHeaders []string) {
	for _, header := range target.Headers {
		// Xcode's Headers build phase controls what a framework product exports.
		// Public headers are copied to Headers, Private headers to PrivateHeaders,
		// while Project headers stay compile-only. A header without attributes is
		// treated as public for compatibility with hand-authored/minimal projects.
		if len(header.Attributes) == 0 || containsString(header.Attributes, "Public") {
			publicHeaders = append(publicHeaders, header.Path)
		}
		if containsString(header.Attributes, "Private") {
			privateHeaders = append(privateHeaders, header.Path)
		}
	}
	return uniqueStrings(publicHeaders), uniqueStrings(privateHeaders)
}

func stagePodFrameworkMetadata(manifest podManifest, target podTarget, configDir, minOS string) (string, error) {
	product := target.ProductName
	if product == "" {
		product = target.Name
	}
	framework := filepath.Join(configDir, target.Name, product+".framework")
	if err := os.MkdirAll(framework, 0o755); err != nil {
		return "", err
	}
	publicHeaders, privateHeaders := podFrameworkProductHeaders(target)
	if err := writeFrameworkSkeleton(framework, product, pluginSources{Headers: publicHeaders, DeploymentTarget: maxVersion(target.DeploymentTarget, minOS), ModuleName: target.ModuleName}); err != nil {
		return "", err
	}
	if len(privateHeaders) > 0 {
		privateDir := filepath.Join(framework, "PrivateHeaders")
		if err := os.MkdirAll(privateDir, 0o755); err != nil {
			return "", err
		}
		for _, header := range privateHeaders {
			if err := replaceFile(header, filepath.Join(privateDir, filepath.Base(header))); err != nil {
				return "", err
			}
		}
	}
	if moduleMap := podClangModuleMap(manifest, target); moduleMap != "" {
		moduleName := target.ModuleName
		if moduleName == "" {
			moduleName = target.Name
		}
		// After swiftc completes, exposeSwiftPodModule writes an immutable
		// derived map in its isolated clang-module directory. Prefer it for the final
		// framework. Before compilation it is deliberately absent, so stage the
		// CocoaPods base map with any legacy ripley-generated Swift header member
		// stripped; otherwise an incremental build imports a header that has not
		// been regenerated yet.
		mapToStage := moduleMap
		derivedMap := podDerivedModuleMapPath(configDir, target)
		if fileExists(derivedMap) {
			mapToStage = derivedMap
		}
		data, err := os.ReadFile(mapToStage)
		if err != nil {
			return "", err
		}
		if samePath(mapToStage, moduleMap) {
			headerLine := "  header \"" + moduleName + "-Swift.h\""
			data = []byte(strings.ReplaceAll(string(data), headerLine+"\n", ""))
		}
		if err := os.WriteFile(filepath.Join(framework, "Modules", "module.modulemap"), data, 0o644); err != nil {
			return "", err
		}
		for _, mapDir := range uniqueStrings([]string{filepath.Dir(mapToStage), filepath.Dir(moduleMap)}) {
			for _, umbrella := range []string{
				filepath.Join(mapDir, product+"-umbrella.h"),
				filepath.Join(mapDir, target.Name+"-umbrella.h"),
			} {
				if fileExists(umbrella) {
					if err := replaceFile(umbrella, filepath.Join(framework, "Headers", filepath.Base(umbrella))); err != nil {
						return "", err
					}
				}
			}
		}
	}
	return framework, nil
}

func finishPodFrameworkMetadata(manifest podManifest, target podTarget, configDir, minOS string) (string, error) {
	framework, err := stagePodFrameworkMetadata(manifest, target, configDir, minOS)
	if err != nil {
		return "", err
	}
	product := target.ProductName
	if product == "" {
		product = target.Name
	}
	moduleName := target.ModuleName
	if moduleName == "" {
		moduleName = product
	}
	for _, swiftHeader := range []string{
		filepath.Join(filepath.Dir(podPublicModuleMap(target, manifest.PodsRoot)), moduleName+"-Swift.h"),
		filepath.Join(configDir, target.Name, moduleName+"-Swift.h"),
	} {
		if fileExists(swiftHeader) {
			if err := replaceFile(swiftHeader, filepath.Join(framework, "Headers", filepath.Base(swiftHeader))); err != nil {
				return "", err
			}
			break
		}
	}
	moduleFile := filepath.Join(configDir, target.Name, moduleName+".swiftmodule")
	if fileExists(moduleFile) {
		moduleDir := filepath.Join(framework, "Modules", moduleName+".swiftmodule")
		if err := os.MkdirAll(moduleDir, 0o755); err != nil {
			return "", err
		}
		if err := replaceFile(moduleFile, filepath.Join(moduleDir, "arm64-apple-ios.swiftmodule")); err != nil {
			return "", err
		}
	}
	return framework, nil
}

// stageStaticPodFramework materializes CocoaPods' static-framework product:
// the framework bundle is a compile/link container, but its binary is an ar
// archive rather than an MH_DYLIB and must never be embedded into the app.
func stageStaticPodFramework(manifest podManifest, item builtPodTarget, configDir, minOS string) (string, error) {
	framework, err := finishPodFrameworkMetadata(manifest, item.Target, configDir, minOS)
	if err != nil {
		return "", err
	}
	product := item.Target.ProductName
	if product == "" {
		product = item.Target.Name
	}
	binary := filepath.Join(framework, product)
	if err := replaceFile(item.Archive, binary); err != nil {
		return "", fmt.Errorf("stage static CocoaPod framework %s: %w", item.Target.Name, err)
	}
	return framework, nil
}

// staticPodTargetLinkerFlags carries pod-target link settings to the final
// executable link. Dynamic frameworks consume these flags while producing the
// dylib; static frameworks cannot, so dependencies such as cargokit's generated
// -force_load archive must remain visible to the host (or extension) linker.

// staticAggregatePodLinkerFlags keeps the CocoaPods aggregate target's
// executable-link semantics that are not represented by per-pod framework
// paths. In particular -ObjC is required to pull category-only archive members,
// and podspec-declared system frameworks/libraries live on the aggregate target.
// Per-pod framework names are removed because BuildProjectHost already links
// the concrete framework products by path.
func staticAggregatePodLinkerFlags(manifest podManifest) []string {
	products := map[string]bool{"Flutter": true}
	for _, target := range manifest.Targets {
		if target.ProductType != "com.apple.product-type.framework" || strings.HasPrefix(target.Name, "Pods-") {
			continue
		}
		products[target.Name] = true
		if target.ProductName != "" {
			products[target.ProductName] = true
		}
		if target.ModuleName != "" {
			products[target.ModuleName] = true
		}
	}
	var root podTarget
	for _, target := range manifest.Targets {
		if strings.HasPrefix(target.Name, "Pods-") {
			root = target
			break
		}
	}
	var out []string
	for _, path := range existingPaths(root.BaseFrameworkSearchPaths) {
		out = append(out, "-F", path)
	}
	for _, path := range existingPaths(root.BaseLibrarySearchPaths) {
		out = append(out, "-L", path)
	}
	flags := podLinkerFlags(root.BaseOtherLDFlags)
	for i := 0; i < len(flags); i++ {
		flag := flags[i]
		if (flag == "-framework" || flag == "-weak_framework") && i+1 < len(flags) {
			name := flags[i+1]
			i++
			if products[name] {
				continue
			}
			out = append(out, flag, name)
			continue
		}
		out = append(out, flag)
	}
	return out
}

func staticPodTargetLinkerFlags(target podTarget) []string {
	var out []string
	for _, path := range existingPaths(target.FrameworkSearchPaths) {
		out = append(out, "-F", path)
	}
	for _, path := range existingPaths(target.LibrarySearchPaths) {
		out = append(out, "-L", path)
	}
	out = append(out, podLinkerFlags(target.OtherLDFlags)...)
	if podTargetNeedsCXXRuntime(target) {
		out = append(out, "-lc++")
	}
	return out
}

func linkDynamicPodFramework(tc toolchain.Toolchain, manifest podManifest, item builtPodTarget, binaryArtifacts podBinaryArtifacts, configDir, minOS string) (string, error) {
	target := item.Target
	product := target.ProductName
	if product == "" {
		product = target.Name
	}
	framework := filepath.Join(configDir, target.Name, product+".framework")
	if err := os.RemoveAll(framework); err != nil {
		return "", err
	}
	if err := os.MkdirAll(framework, 0o755); err != nil {
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
	binary := filepath.Join(framework, product)
	args := []string{"-arch", "arm64", "-dylib", "-platform_version", "ios", maxVersion(target.DeploymentTarget, minOS), sdkVersion, "-syslibroot", sdk, "-install_name", "@rpath/" + product + ".framework/" + product, "-o", binary, "-force_load", item.Archive}
	frameworkSearchFlags, err := frameworkSearch(tc)
	if err != nil {
		return "", err
	}
	args = append(args, frameworkSearchFlags...)
	for _, path := range uniqueStrings(append(existingPaths(target.FrameworkSearchPaths), binaryArtifacts.FrameworkSearchPaths...)) {
		args = append(args, "-F", path)
	}
	libSearchFlags, err := libSearch(tc)
	if err != nil {
		return "", err
	}
	args = append(args, libSearchFlags...)
	for _, path := range existingPaths(target.LibrarySearchPaths) {
		args = append(args, "-L", path)
	}
	// CocoaPods' target-specific OTHER_LDFLAGS carries both pod-framework
	// dependencies and generated native archives (cargokit is a real example).
	args = append(args, podLinkerFlags(target.OtherLDFlags)...)
	if podTargetNeedsCXXRuntime(target) {
		args = append(args, "-lc++")
	}
	args = append(args, "-lswiftCore", "-lswiftFoundation", "-lswiftDarwin", "-lswiftObjectiveC", "-lswiftDispatch", "-lswiftCoreFoundation", "-lswiftCompatibility56", "-lswiftCompatibilityPacks", "-lswiftCompatibilityConcurrency", "-rpath", "/usr/lib/swift")
	swiftLibs, err := tc.SwiftIOSLibs()
	if err != nil {
		return "", err
	}
	if runtime := filepath.Join(swiftLibs, "libclang_rt.ios.a"); fileExists(runtime) {
		args = append(args, runtime)
	}
	args = append(args, "-lSystem")
	if err := runLD64(tc, args...); err != nil {
		return "", fmt.Errorf("link CocoaPod framework %s: %w", target.Name, err)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return "", err
	}
	// Re-stage metadata after compilation because exposeSwiftPodModule may have
	// produced the generated Swift compatibility header/module.
	if _, err := finishPodFrameworkMetadata(manifest, target, configDir, minOS); err != nil {
		return "", err
	}
	return framework, nil
}

func linkDynamicPodFrameworks(tc toolchain.Toolchain, manifest podManifest, built []builtPodTarget, binaryArtifacts podBinaryArtifacts, configDir, minOS string) ([]string, error) {
	sdk, err := tc.IOSSDK()
	if err != nil {
		return nil, err
	}
	sdkVersion, err := tc.IOSSDKVersion()
	if err != nil {
		return nil, err
	}
	frameworkSearchFlags, err := frameworkSearch(tc)
	if err != nil {
		return nil, err
	}
	libSearchFlags, err := libSearch(tc)
	if err != nil {
		return nil, err
	}
	swiftLibs, err := tc.SwiftIOSLibs()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]builtPodTarget, len(built))
	for _, item := range built {
		byName[item.Target.Name] = item
	}
	ordered, err := topologicalPodTargets(manifest.Targets)
	if err != nil {
		return nil, err
	}
	var frameworks []string
	for _, target := range ordered {
		item, ok := byName[target.Name]
		if !ok || target.ProductType != "com.apple.product-type.framework" {
			continue
		}
		product := target.ProductName
		if product == "" {
			product = target.Name
		}
		framework := filepath.Join(configDir, target.Name, product+".framework")
		if err := os.RemoveAll(framework); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(framework, 0o755); err != nil {
			return nil, err
		}
		binary := filepath.Join(framework, product)
		args := []string{"-arch", "arm64", "-dylib", "-platform_version", "ios", maxVersion(target.DeploymentTarget, minOS), sdkVersion, "-syslibroot", sdk, "-install_name", "@rpath/" + product + ".framework/" + product, "-o", binary, "-force_load", item.Archive}
		args = append(args, frameworkSearchFlags...)
		for _, path := range uniqueStrings(append(existingPaths(target.FrameworkSearchPaths), binaryArtifacts.FrameworkSearchPaths...)) {
			args = append(args, "-F", path)
		}
		args = append(args, libSearchFlags...)
		for _, path := range existingPaths(target.LibrarySearchPaths) {
			args = append(args, "-L", path)
		}
		// CocoaPods' target-specific OTHER_LDFLAGS carries both pod-framework
		// dependencies and generated native archives (cargokit is a real example).
		// Preserve it rather than flattening all binary artifacts into every pod.
		args = append(args, podLinkerFlags(target.OtherLDFlags)...)
		if podTargetNeedsCXXRuntime(target) {
			args = append(args, "-lc++")
		}
		args = append(args, "-lswiftCore", "-lswiftFoundation", "-lswiftDarwin", "-lswiftObjectiveC", "-lswiftDispatch", "-lswiftCoreFoundation", "-lswiftCompatibility56", "-lswiftCompatibilityPacks", "-lswiftCompatibilityConcurrency", "-rpath", "/usr/lib/swift")
		if runtime := filepath.Join(swiftLibs, "libclang_rt.ios.a"); fileExists(runtime) {
			args = append(args, runtime)
		}
		args = append(args, "-lSystem")
		if err := runLD64(tc, args...); err != nil {
			return nil, fmt.Errorf("link CocoaPod framework %s: %w", target.Name, err)
		}
		if err := os.Chmod(binary, 0o755); err != nil {
			return nil, err
		}
		headers := make([]string, 0, len(target.Headers))
		for _, header := range target.Headers {
			headers = append(headers, header.Path)
		}
		if err := writeFrameworkSkeleton(framework, product, pluginSources{Headers: headers, DeploymentTarget: maxVersion(target.DeploymentTarget, minOS), ModuleName: target.ModuleName}); err != nil {
			return nil, err
		}
		frameworks = append(frameworks, framework)
	}
	for _, vendored := range binaryArtifacts.DynamicFrameworks {
		frameworks = append(frameworks, vendored.Path)
	}
	return uniqueStrings(frameworks), nil
}

func linkPodsFramework(tc toolchain.Toolchain, manifest podManifest, built []builtPodTarget, binaryArtifacts podBinaryArtifacts, configDir, minOS string) (string, []string, error) {
	if len(built) == 0 {
		return "", nil, nil
	}
	framework := filepath.Join(filepath.Dir(configDir), "RipleyPods.framework")
	if err := os.RemoveAll(framework); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(framework, 0o755); err != nil {
		return "", nil, err
	}
	binary := filepath.Join(framework, "RipleyPods")
	sdk, err := tc.IOSSDK()
	if err != nil {
		return "", nil, err
	}
	sdkVersion, err := tc.IOSSDKVersion()
	if err != nil {
		return "", nil, err
	}
	args := []string{"-arch", "arm64", "-dylib", "-platform_version", "ios", minOS, sdkVersion, "-syslibroot", sdk, "-install_name", "@rpath/RipleyPods.framework/RipleyPods", "-o", binary}
	builtNames := make(map[string]bool)
	for _, item := range built {
		args = append(args, "-force_load", item.Archive)
		builtNames[item.Target.Name] = true
		builtNames[item.Target.ProductName] = true
		builtNames[strings.TrimPrefix(item.Target.ProductName, "lib")] = true
	}
	disabledRelativeObjCMethodLists := false
	for _, vendored := range binaryArtifacts.StaticFrameworks {
		linkInput, forceLoad, err := staticFrameworkLinkInput(vendored, configDir)
		if err != nil {
			return "", nil, err
		}
		if forceLoad {
			args = append(args, "-force_load", linkInput)
		} else {
			// Some binary pods (notably MLKitTextRecognitionCommon) ship a huge
			// prelinked MH_OBJECT. LLD 20 crashes while synthesizing relative ObjC
			// method lists for that representation. Traditional method lists are
			// ABI-compatible and match the safe ld64 behavior for this input.
			if !disabledRelativeObjCMethodLists {
				args = append(args, "-no_objc_relative_method_lists")
				disabledRelativeObjCMethodLists = true
			}
			args = append(args, linkInput)
		}
		builtNames[vendored.Name] = true
	}
	for _, library := range binaryArtifacts.StaticLibraries {
		args = append(args, "-force_load", library)
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(library), "lib"), ".a")
		builtNames[name] = true
	}
	for _, vendored := range binaryArtifacts.DynamicFrameworks {
		args = append(args, "-framework", vendored.Name, "-F", filepath.Dir(vendored.Path))
	}
	root := podTarget{}
	for _, target := range manifest.Targets {
		if strings.HasPrefix(target.Name, "Pods-") {
			root = target
			break
		}
	}
	for _, flag := range filteredPodLinkFlags(root.OtherLDFlags, builtNames) {
		args = append(args, flag)
	}
	search, err := frameworkSearch(tc)
	if err != nil {
		return "", nil, err
	}
	args = append(args, search...)
	for _, path := range uniqueStrings(append(existingPaths(root.FrameworkSearchPaths), binaryArtifacts.FrameworkSearchPaths...)) {
		args = append(args, "-F", path)
	}
	libs, err := libSearch(tc)
	if err != nil {
		return "", nil, err
	}
	args = append(args, libs...)
	for _, path := range existingPaths(root.LibrarySearchPaths) {
		args = append(args, "-L", path)
	}
	args = append(args, "-framework", "Flutter", "-framework", "UIKit", "-framework", "Foundation")
	for _, item := range built {
		if podTargetNeedsCXXRuntime(item.Target) {
			args = append(args, "-lc++")
			break
		}
	}
	args = append(args, "-lswiftCore", "-lswiftFoundation", "-lswiftDarwin", "-lswiftObjectiveC", "-lswiftDispatch", "-lswiftCoreFoundation", "-lswiftCompatibility56", "-lswiftCompatibilityPacks", "-lswiftCompatibilityConcurrency", "-rpath", "/usr/lib/swift")
	swiftLibs, err := tc.SwiftIOSLibs()
	if err != nil {
		return "", nil, err
	}
	if runtime := filepath.Join(swiftLibs, "libclang_rt.ios.a"); fileExists(runtime) {
		args = append(args, runtime)
	}
	args = append(args, "-lSystem")
	if err := runLD64(tc, args...); err != nil {
		return "", nil, err
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return "", nil, err
	}
	if err := writeFrameworkSkeleton(framework, "RipleyPods", pluginSources{DeploymentTarget: minOS, ModuleName: "RipleyPods"}); err != nil {
		return "", nil, err
	}
	var dynamic []string
	for _, vendored := range binaryArtifacts.DynamicFrameworks {
		dynamic = append(dynamic, vendored.Path)
	}
	return framework, uniqueStrings(dynamic), nil
}

func filteredPodLinkFlags(flags []string, built map[string]bool) []string {
	flags = podLinkerFlags(flags)
	var out []string
	for i := 0; i < len(flags); i++ {
		flag := flags[i]
		if flag == "-ObjC" || flag == "-all_load" {
			continue
		}
		if strings.HasPrefix(flag, "-l") && len(flag) > 2 {
			name := strings.TrimPrefix(flag, "-l")
			if built[name] || name == "Flutter" {
				continue
			}
		}
		if flag == "-force_load" {
			if i+1 < len(flags) {
				path := flags[i+1]
				name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "lib"), filepath.Ext(path))
				if !built[name] {
					out = append(out, flag, path)
				}
				i++
			}
			continue
		}
		if flag == "-framework" || flag == "-weak_framework" {
			if i+1 < len(flags) {
				out = append(out, flag, flags[i+1])
				i++
			}
			continue
		}
		out = append(out, flag)
	}
	return out
}
