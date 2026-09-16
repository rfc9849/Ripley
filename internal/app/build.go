package app

import (
	"archive/zip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ripley/internal/assets"
	"ripley/internal/ios"
	"ripley/internal/ios/assetcatalog"
	"ripley/internal/ios/storyboard"
	"ripley/internal/toolchain"

	"howett.net/plist"
)

type project struct {
	Root          string
	BuildDir      string
	DartTool      string
	PackageConfig string
}

func newProject(root string) (project, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return project{}, err
	}
	return project{
		Root:          abs,
		BuildDir:      filepath.Join(abs, "build", "ripley_ios"),
		DartTool:      filepath.Join(abs, ".dart_tool"),
		PackageConfig: findProjectPackageConfig(abs),
	}, nil
}

func (p project) packageConfig() string {
	if p.PackageConfig != "" {
		return p.PackageConfig
	}
	return filepath.Join(p.DartTool, "package_config.json")
}

// findProjectPackageConfig resolves Dart pub workspace semantics. A workspace
// member intentionally has no member-local .dart_tool/package_config.json;
// pub writes the shared package config at the workspace root and leaves only a
// workspace_ref.json below the member. Walk ancestors and accept the first
// package config that actually contains projectRoot as one of its package
// roots. If nothing is generated yet, return the traditional local path so the
// caller reports the familiar missing-package-config error.
func findProjectPackageConfig(projectRoot string) string {
	projectRoot = filepath.Clean(projectRoot)
	for dir := projectRoot; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".dart_tool", "package_config.json")
		if fileExists(candidate) {
			if dir == projectRoot || packageConfigContainsRoot(candidate, projectRoot) {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return filepath.Join(projectRoot, ".dart_tool", "package_config.json")
}

func packageConfigContainsRoot(configPath, projectRoot string) bool {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	var config struct {
		Packages []struct {
			RootURI string `json:"rootUri"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return false
	}
	projectRoot = filepath.Clean(projectRoot)
	for _, pkg := range config.Packages {
		root, err := packageRootFromURI(configPath, pkg.RootURI)
		if err == nil && filepath.Clean(root) == projectRoot {
			return true
		}
	}
	return false
}

func normalizePackageConfigRootURIs(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	packages, _ := doc["packages"].([]any)
	changed := false
	for _, raw := range packages {
		pkg, _ := raw.(map[string]any)
		rootURI, _ := pkg["rootUri"].(string)
		if rootURI == "" {
			continue
		}
		parsed, err := url.Parse(rootURI)
		if err != nil {
			return fmt.Errorf("parse package rootUri %q: %w", rootURI, err)
		}
		if parsed.IsAbs() {
			continue
		}
		rel, err := url.PathUnescape(parsed.Path)
		if err != nil {
			return fmt.Errorf("decode package rootUri %q: %w", rootURI, err)
		}
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(path), filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		pkg["rootUri"] = (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("normalize package root URIs in %s: %w", path, err)
	}
	return nil
}

func build(opt buildOptions) error {
	project, err := newProject(opt.Dir)
	if err != nil {
		return err
	}
	iosProject, err := resolveIOSProject(project.Root, opt.Mode, opt.Flavor, opt.Target)
	if err != nil {
		return err
	}
	applyBundleIDOverride(&iosProject, opt.BundleID)
	engine, dart, err := toolchain.FlutterEngineRevision(iosProject.FlutterRoot)
	if err != nil {
		return err
	}
	tc := toolchain.NewToolchain(engine, dart)
	fmt.Printf("project: target=%s configuration=%s product=%s bundle=%s min-ios=%s\n", iosProject.TargetName, iosProject.Configuration, iosProject.ProductName, iosProject.BundleID, iosProject.MinOS)
	fmt.Printf("flutter: %s engine=%s dart=%s target=%s\n", iosProject.FlutterRoot, engine, dart, iosProject.BuildSettings["FLUTTER_TARGET"])

	plugins, err := ios.DiscoverPlugins(project.Root, project.packageConfig())
	if err != nil {
		return err
	}
	hasPlugins := len(plugins) > 0 && !opt.NoPlugins
	if err := tc.Ensure(hasPlugins); err != nil {
		return err
	}
	// CocoaPods' podhelper.rb hard-requires Flutter.xcframework inside the
	// Flutter SDK cache, and `flutter precache --ios` refuses to fetch iOS
	// artifacts on Linux. Stage ripley's own cached copy so pod resolution works.
	if err := ios.EnsureFlutterIOSArtifacts(tc, iosProject.FlutterRoot); err != nil {
		return err
	}

	var pluginBuild ios.PluginBuild
	if !opt.NoPlugins {
		pluginBuild, err = ios.BuildAllPlugins(tc, project.Root, iosProject.SourceDir, iosProject.MinOS, iosProject.BuildSettings["SWIFT_VERSION"], project.packageConfig())
		if err != nil {
			return err
		}
		if len(pluginBuild.Plugins) > 0 {
			names := make([]string, 0, len(pluginBuild.Plugins))
			for _, plugin := range pluginBuild.Plugins {
				names = append(names, plugin.Name)
			}
			fmt.Printf("plugins: %s\n", strings.Join(names, ", "))
		}
	}
	swiftpm, err := ios.BuildSwiftPMPackages(tc, project.Root, iosProject.SourceDir, iosProject.MinOS)
	if err != nil {
		return err
	}
	if len(swiftpm.Packages) > 0 {
		fmt.Printf("swiftpm: %d package(s)\n", len(swiftpm.Packages))
	}
	// With plugins disabled there are no plugin modules to import, and Flutter's
	// generated registrant @imports them by module name, so compiling it would
	// fail on the first missing module.
	if pluginBuild.RegistrantObject == "" && !opt.NoPlugins {
		needs, err := needsRegistrant(iosProject.SourceDir)
		if err != nil {
			return err
		}
		if needs {
			work := filepath.Join(project.BuildDir, "plugins")
			if err := os.MkdirAll(work, 0o755); err != nil {
				return err
			}
			pluginBuild.RegistrantObject, err = ios.BuildRegistrant(tc, nil, work, iosProject.SourceDir, iosProject.MinOS, nil, nil)
			if err != nil {
				return err
			}
		}
	}
	if err := runApplicationShellPhases(tc, project, iosProject, applicationScriptBeforeFlutter); err != nil {
		return err
	}

	if err := normalizePackageConfigRootURIs(project.packageConfig()); err != nil {
		return err
	}
	kernelPackageConfig, err := prepareKernelPackageConfig(project, pluginBuild)
	if err != nil {
		return err
	}
	dill, err := compileKernel(tc, project, iosProject, opt.Mode, kernelPackageConfig)
	if err != nil {
		return err
	}
	debugInfo := ios.DebugInfoOptions{
		SaveDebuggingInfo: opt.SaveDebugInfo,
		SplitDebugInfo:    opt.SplitDebugInfo,
		Obfuscate:         opt.Obfuscate,
	}
	if _, err := aotCompile(tc, project, iosProject, dill, debugInfo); err != nil {
		return err
	}
	if _, err := stageFlutterFramework(tc, project); err != nil {
		return err
	}
	if _, err := buildAssets(tc, project, iosProject.FlutterRoot, dill, opt.Flavor, !opt.NoTreeShakeIcons, opt.Mode); err != nil {
		return err
	}
	nativeAssets, err := ios.BuildNativeAssets(tc, project.Root, iosProject.FlutterRoot, project.packageConfig(), project.BuildDir, iosProject.MinOS)
	if err != nil {
		return err
	}
	if len(nativeAssets.Packages) > 0 {
		fmt.Printf("native assets: %s\n", strings.Join(nativeAssets.Packages, ", "))
	}
	if len(nativeAssets.Manifest) > 0 {
		if err := os.WriteFile(filepath.Join(project.BuildDir, "flutter_assets", "NativeAssetsManifest.json"), nativeAssets.Manifest, 0o644); err != nil {
			return err
		}
	}
	if err := runApplicationShellPhases(tc, project, iosProject, applicationScriptAfterFlutter); err != nil {
		return err
	}
	app, err := assembleApp(tc, project, iosProject, pluginBuild, swiftpm, nativeAssets)
	if err != nil {
		return err
	}
	if err := runApplicationShellPhases(tc, project, iosProject, applicationScriptAfterSources); err != nil {
		return err
	}

	if !opt.NoSign {
		material, err := resolveSigning(opt, iosProject.BundleID)
		if err != nil {
			return err
		}
		if err := ios.SignApp(tc, app, ios.SignOptions{
			Entitlements: iosProject.Entitlements,
			Variables:    iosProject.BuildSettings,
			Key:          material.Key,
			Cert:         material.Cert,
			Provision:    material.Provision,
			Password:     material.Password,
			BundleID:     iosProject.BundleID,
		}); err != nil {
			return err
		}
	}

	if opt.IPA {
		ipa := filepath.Join(project.BuildDir, iosProject.ProductName+".ipa")
		if err := makeIPA(app, ipa); err != nil {
			return err
		}
		fmt.Printf("\nBuilt %s\n", ipa)
	} else {
		fmt.Printf("\nBuilt %s\n", app)
	}
	return nil
}

func compileKernel(tc toolchain.Toolchain, project project, iosProject iosProject, mode, packageConfig string) (string, error) {
	out := filepath.Join(project.BuildDir, "app.dill")
	if err := os.MkdirAll(project.BuildDir, 0o755); err != nil {
		return "", err
	}
	dartPluginRegistrant, err := generateDartPluginRegistrant(project)
	if err != nil {
		return "", err
	}
	profile := mode == "profile"
	args := []string{
		tc.FrontendServer(),
		"--sdk-root", tc.PatchedSDK(),
		"--target=flutter",
		"--no-print-incremental-dependencies",
		fmt.Sprintf("-Ddart.vm.profile=%t", profile),
		fmt.Sprintf("-Ddart.vm.product=%t", !profile),
		"--aot", "--tfa",
		"--target-os", "ios",
		"--packages", packageConfig,
		"--output-dill", out,
	}
	args = append(args, dartPluginRegistrantFrontendArgs(dartPluginRegistrant)...)
	args = append(args, "--verbosity=error")
	defines, err := decodeDartDefines(iosProject.BuildSettings["DART_DEFINES"])
	if err != nil {
		return "", err
	}
	for _, define := range defines {
		args = append(args, "-D"+define)
	}
	if strings.EqualFold(iosProject.BuildSettings["TRACK_WIDGET_CREATION"], "true") {
		args = append(args, "--track-widget-creation")
	}
	args = append(args, iosProject.FlutterTarget)
	if err := run("", nil, tc.DartAOTRuntime(), args...); err != nil {
		return "", err
	}
	return out, nil
}

func decodeDartDefines(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	defines := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			return nil, fmt.Errorf("decode DART_DEFINES entry %q: %w", part, err)
		}
		defines = append(defines, string(decoded))
	}
	return defines, nil
}

func aotCompile(tc toolchain.Toolchain, project project, iosProject iosProject, dill string, debug ios.DebugInfoOptions) (string, error) {
	appFramework := filepath.Join(project.BuildDir, "App.framework")
	if err := os.MkdirAll(appFramework, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(appFramework, "App")
	obj := filepath.Join(project.BuildDir, "app.o")
	args := []string{
		"--snapshot_kind=app-aot-macho-dylib",
		"--macho=" + out,
		"--macho-object=" + obj,
		"--macho-min-os-version=" + iosProject.MinOS,
		"--macho-rpath=@executable_path/Frameworks,@loader_path/Frameworks",
		"--macho-install-name=@rpath/App.framework/App",
		"--deterministic",
	}
	debugArgs, err := ios.GenSnapshotDebugArgs(debug, project.BuildDir, iosProject.ProductName)
	if err != nil {
		return "", err
	}
	args = append(args, debugArgs...)
	// gen_snapshot takes the kernel as its positional argument, so it stays last.
	args = append(args, dill)
	if err := run("", nil, tc.GenSnapshot(), args...); err != nil {
		return "", err
	}
	// dsymutil follows the absolute N_OSO stabs gen_snapshot left in the dylib
	// back into app.o, so the dSYM must be extracted before anything strips the
	// binary or removes the intermediate object.
	if debug.SaveDebuggingInfo || debug.SplitDebugInfo != "" {
		dsym, err := ios.EmitDSYM(tc, out, project.BuildDir)
		if err != nil {
			return "", err
		}
		fmt.Printf("debug info: %s\n", dsym)
		if err := ios.StripMachO(tc, out); err != nil {
			return "", err
		}
	}
	infoPath := filepath.Join(iosProject.IOSRoot, "Flutter", "AppFrameworkInfo.plist")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return "", fmt.Errorf("read Flutter AppFrameworkInfo.plist: %w", err)
	}
	var info map[string]any
	if _, err := plist.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("parse %s: %w", infoPath, err)
	}
	info["MinimumOSVersion"] = iosProject.MinOS
	if err := writePlist(filepath.Join(appFramework, "Info.plist"), info); err != nil {
		return "", err
	}
	return out, nil
}

func buildAssets(tc toolchain.Toolchain, project project, flutterRoot, dill, flavor string, treeShakeIcons bool, mode string) (string, error) {
	bundle, err := assets.NewAssetBundle(project.Root, flutterRoot, "ios", flavor, project.packageConfig())
	if err != nil {
		return "", err
	}
	if err := bundle.Build(); err != nil {
		return "", err
	}
	out := filepath.Join(project.BuildDir, "flutter_assets")
	if err := assets.CopyAssets(bundle, out, tc, dill, treeShakeIcons, mode); err != nil {
		return "", err
	}
	return out, nil
}

func stageFlutterFramework(tc toolchain.Toolchain, project project) (string, error) {
	src := filepath.Join(tc.FlutterXCFramework(), "ios-arm64", "Flutter.framework")
	dst := filepath.Join(project.BuildDir, "Flutter.framework")
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := copyDir(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func moduleMapHeaderSearchPaths(moduleMaps []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, moduleMap := range moduleMaps {
		if moduleMap == "" {
			continue
		}
		dir := filepath.Dir(moduleMap)
		for _, candidate := range []string{dir, filepath.Dir(dir)} {
			if candidate == "" || candidate == "." || seen[candidate] {
				continue
			}
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	sort.Strings(out)
	return out
}

// hostInputs collects everything beyond the target's own sources that the
// application host binary must compile and link: plugin objects, the outputs of
// custom PBXBuildRules, and the sources contributed by Xcode 16 folder-synced
// groups (which carry no PBXFileReference and are therefore absent from the
// legacy Sources build phase). The returned resources are rule outputs destined
// for the bundle root.
func hostInputs(tc toolchain.Toolchain, project project, config iosProject, extraObjects []string, plugins ios.PluginBuild, swiftpm ios.SwiftPMBuild) (ios.HostInputs, []string, error) {
	inputs := ios.HostInputs{
		Objects: append(append(append([]string{}, extraObjects...), plugins.Objects...), swiftpm.Objects...),
		// CocoaPods names its per-target map `<Module>.modulemap` rather than
		// `module.modulemap`, so clang cannot discover a pod module from a header
		// search path alone and the map must be named explicitly.
		ModuleSearchPaths: append(append([]string{}, swiftpm.ModuleSearchPaths...), plugins.ModuleSearchPaths...),
		ModuleMaps:        plugins.ModuleMaps,
		MacroPlugins:      swiftpm.MacroPlugins,
		LinkerLibraries:   append(append([]string{}, plugins.LinkerLibraries...), swiftpm.LinkerLibraries...),
		LinkerFrameworks:  append(append([]string{}, plugins.LinkerFrameworks...), swiftpm.LinkerFrameworks...),
		LinkerFlags:       append([]string{}, plugins.LinkerFlags...),
		TargetSources:     config.SourceFiles,
	}
	// Explicit CocoaPods module maps still need their public header roots on
	// Clang's search path. This is load-bearing for Swift host sources that
	// directly import an Objective-C-only pod module such as FirebaseCore.
	inputs.HeaderSearchPaths = append(inputs.HeaderSearchPaths, moduleMapHeaderSearchPaths(plugins.ModuleMaps)...)

	rules, err := projectBuildRules(config)
	if err != nil {
		return ios.HostInputs{}, nil, err
	}
	var generated buildRuleOutputs
	if len(rules) > 0 {
		// applicationScriptEnvironment resolves the target's build settings; an
		// empty phase is correct because runBuildRules layers the per-file rule
		// variables on top.
		_, values, err := applicationScriptEnvironment(tc, project, config, xcodeShellScriptPhase{})
		if err != nil {
			return ios.HostInputs{}, nil, err
		}
		candidates := append(ruleInputCandidates(config.SourceDir), config.SyncedSources...)
		generated, err = runBuildRules(rules, candidates, values, filepath.Join(project.BuildDir, "buildrules"))
		if err != nil {
			return ios.HostInputs{}, nil, err
		}
		inputs.Sources = append(inputs.Sources, generated.Sources...)
		inputs.HeaderSearchPaths = append(inputs.HeaderSearchPaths, generated.HeaderSearchPaths...)
		inputs.Objects = append(inputs.Objects, generated.Objects...)
	}

	// Folder-synced sources a rule already consumed must not be compiled twice.
	claimed := make(map[string]bool, len(generated.ClaimedInputs))
	for _, path := range generated.ClaimedInputs {
		claimed[path] = true
	}
	headerDirs := make(map[string]bool)
	for _, path := range config.SyncedSources {
		if claimed[path] {
			continue
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".swift", ".m", ".mm", ".c", ".cc", ".cpp", ".cxx":
			inputs.Sources = append(inputs.Sources, path)
		case ".h", ".hpp", ".hh":
			headerDirs[filepath.Dir(path)] = true
		}
	}
	for dir := range headerDirs {
		inputs.HeaderSearchPaths = append(inputs.HeaderSearchPaths, dir)
	}
	sort.Strings(inputs.HeaderSearchPaths)
	return inputs, generated.Resources, nil
}

// ruleInputCandidates lists the target's own source-directory files that a
// custom build rule may claim. Rules match on file type, so every regular file
// is a candidate; the rule set decides.
func ruleInputCandidates(sourceDir string) []string {
	var out []string
	filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if name := d.Name(); name == "Assets.xcassets" || strings.HasSuffix(name, ".framework") {
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	return out
}

func assembleApp(tc toolchain.Toolchain, project project, iosProject iosProject, plugins ios.PluginBuild, swiftpm ios.SwiftPMBuild, nativeAssets ios.NativeAssetBuild) (string, error) {
	app := filepath.Join(project.BuildDir, iosProject.ProductName+".app")
	if err := os.RemoveAll(app); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(app, "Frameworks"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(app, "flutter_assets"), 0o755); err != nil {
		return "", err
	}

	var extraObjects []string
	if plugins.RegistrantObject != "" {
		extraObjects = append(extraObjects, plugins.RegistrantObject)
	}
	hostInputs, ruleResources, err := hostInputs(tc, project, iosProject, extraObjects, plugins, swiftpm)
	if err != nil {
		return "", err
	}
	frameworks := append(append([]string{}, plugins.Frameworks...), swiftpm.Frameworks...)
	if _, err := ios.BuildProjectHost(tc, iosProject.SourceDir, filepath.Join(app, iosProject.ExecutableName), iosProject.MinOS, iosProject.ModuleName, iosProject.BridgingHeader, hostInputs, frameworks); err != nil {
		return "", err
	}
	for _, resource := range ruleResources {
		dst := filepath.Join(app, filepath.Base(resource))
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

	if err := copyIOSResources(iosProject, app); err != nil {
		return "", err
	}
	for _, framework := range []string{"App.framework", "Flutter.framework"} {
		if err := copyDir(filepath.Join(project.BuildDir, framework), filepath.Join(app, "Frameworks", framework)); err != nil {
			return "", err
		}
	}
	embedFrameworks := append(append([]string{}, plugins.EmbeddedFrameworks...), swiftpm.Frameworks...)
	for _, framework := range embedFrameworks {
		if err := copyDir(framework, filepath.Join(app, "Frameworks", filepath.Base(framework))); err != nil {
			return "", err
		}
	}
	for _, framework := range nativeAssets.Frameworks {
		if err := copyDir(framework, filepath.Join(app, "Frameworks", filepath.Base(framework))); err != nil {
			return "", err
		}
	}
	for _, resource := range append(append([]string{}, plugins.Resources...), swiftpm.Resources...) {
		dst := filepath.Join(app, filepath.Base(resource))
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
	assets := filepath.Join(project.BuildDir, "flutter_assets")
	if dirExists(assets) {
		if err := copyDir(assets, filepath.Join(app, "flutter_assets")); err != nil {
			return "", err
		}
	}
	info, launch, err := loadProjectInfoPlist(iosProject)
	if err != nil {
		return "", err
	}
	if err := stageMainStoryboard(iosProject, app, info); err != nil {
		return "", err
	}
	launchForCatalog := launch
	if launch != nil {
		if err := stageLaunchStoryboard(iosProject, app, launch); err != nil {
			if !storyboard.IsUnsupportedCompileError(err) {
				return "", err
			}
			applyLaunchScreenFallback(info, launch)
			delete(info, "UILaunchStoryboardName")
		} else {
			// The storyboardc resolves its own literal/system colours and project
			// asset names. Generated colorsets are only needed by the plist fallback.
			launchForCatalog = nil
		}
	}
	ensurePlistLaunchScreen(info)
	if err := compileAssetCatalogs(tc, project, iosProject, app, info, launchForCatalog); err != nil {
		return "", err
	}
	if err := writePlist(filepath.Join(app, "Info.plist"), info); err != nil {
		return "", err
	}
	// Extensions are built last: they resolve their imports against the
	// frameworks already staged in <App>.app/Frameworks.
	appex, err := buildAppExtensions(tc, project, iosProject, app, plugins, swiftpm.MacroPlugins)
	if err != nil {
		return "", err
	}
	for _, bundle := range appex {
		fmt.Printf("extension: %s\n", filepath.Base(bundle))
	}
	// Xcode's Swift stdlib embedding step runs after every app/extension
	// executable exists. Do the same so @rpath Swift back-deployment runtimes
	// referenced by pods, packages, native frameworks, or appex binaries are
	// staged once in the host app's Frameworks directory before signing.
	if err := ios.EmbedSwiftRuntimeLibraries(tc, app); err != nil {
		return "", err
	}
	return app, nil
}

// loadProjectInfoPlist builds the app's Info.plist from the project's own plist
// plus resolved build settings and analyses its launch storyboard. assembleApp
// first compiles that storyboard to a real `.storyboardc`; the parsed
// LaunchScreen also carries the explicit UILaunchScreen compatibility fallback
// used when the launch-only compiler encounters unsupported content.
func loadProjectInfoPlist(iosProject iosProject) (map[string]any, *storyboard.LaunchScreen, error) {
	data, err := os.ReadFile(iosProject.InfoPlist)
	if err != nil {
		return nil, nil, err
	}
	text := string(data)
	for iteration := 0; iteration < 12; iteration++ {
		expanded := expandBuildValue(text, iosProject.BuildSettings)
		if expanded == text {
			break
		}
		text = expanded
	}
	info := make(map[string]any)
	if _, err := plist.Unmarshal([]byte(text), &info); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", iosProject.InfoPlist, err)
	}
	for key, value := range iosProject.BuildSettings {
		if !strings.HasPrefix(key, "INFOPLIST_KEY_") {
			continue
		}
		plistKey := strings.TrimPrefix(key, "INFOPLIST_KEY_")
		if plistKey == "" {
			continue
		}
		info[plistKey] = plistBuildSettingValue(value)
	}
	if bundleID, _ := info["CFBundleIdentifier"].(string); bundleID != "" && bundleID != iosProject.BundleID {
		return nil, nil, fmt.Errorf("Info.plist CFBundleIdentifier %q disagrees with PRODUCT_BUNDLE_IDENTIFIER %q", bundleID, iosProject.BundleID)
	}
	if executable, _ := info["CFBundleExecutable"].(string); executable != "" && executable != iosProject.ExecutableName {
		return nil, nil, fmt.Errorf("Info.plist CFBundleExecutable %q disagrees with EXECUTABLE_NAME %q", executable, iosProject.ExecutableName)
	}
	if _, ok := info["CFBundleIdentifier"]; !ok {
		info["CFBundleIdentifier"] = iosProject.BundleID
	}
	if _, ok := info["CFBundleExecutable"]; !ok {
		info["CFBundleExecutable"] = iosProject.ExecutableName
	}
	if _, ok := info["CFBundleName"]; !ok {
		info["CFBundleName"] = iosProject.ProductName
	}
	if _, ok := info["CFBundleShortVersionString"]; !ok {
		info["CFBundleShortVersionString"] = iosProject.BuildName
	}
	if _, ok := info["CFBundleVersion"]; !ok {
		info["CFBundleVersion"] = iosProject.BuildNumber
	}
	info["MinimumOSVersion"] = iosProject.MinOS
	info["CFBundleSupportedPlatforms"] = []string{"iPhoneOS"}
	if families := deviceFamilies(iosProject.BuildSettings["TARGETED_DEVICE_FAMILY"]); len(families) > 0 {
		info["UIDeviceFamily"] = families
	}

	// Parse the launch storyboard now, but do not lower it yet. assembleApp first
	// tries the native Linux storyboardc compiler; UILaunchScreen is only the
	// compatibility fallback when that compiler reports unsupported content.
	launch, err := lowerLaunchStoryboard(iosProject, info)
	if err != nil {
		return nil, nil, err
	}
	if launch == nil {
		delete(info, "UILaunchStoryboardName")
	}
	// Flutter's UIScene migration may point directly at the framework's
	// FlutterSceneDelegate and rely on Main.storyboard to instantiate the
	// FlutterViewController. Since ripley cannot run ibtool on Linux, redirect that
	// shape to the generated programmatic SceneDelegate compiled into the app.
	rewriteDirectFlutterSceneDelegate(info, iosProject.ModuleName)
	stripSceneStoryboard(info)
	if launch == nil {
		ensurePlistLaunchScreen(info)
	}
	return info, launch, nil
}

// lowerLaunchStoryboard resolves and analyzes the project's
// UILaunchStoryboardName document. The name is historical: the actual plist
// lowering is applied only if the launch-only storyboardc compiler cannot encode
// the document.
func lowerLaunchStoryboard(iosProject iosProject, info map[string]any) (*storyboard.LaunchScreen, error) {
	name, _ := info["UILaunchStoryboardName"].(string)
	path := storyboard.LaunchStoryboardPath(iosProject.SourceDir, name)
	if path == "" {
		return nil, nil
	}
	launch, err := storyboard.ParseLaunchScreen(path)
	if err != nil {
		return nil, fmt.Errorf("parse launch screen %s: %w", path, err)
	}
	return launch, nil
}

func applyLaunchScreenFallback(info map[string]any, launch *storyboard.LaunchScreen) {
	if launch == nil {
		return
	}
	for key, value := range launch.InfoPlistKeys {
		info[key] = value
	}
	for _, reason := range launch.Unsupported {
		fmt.Printf("launch screen fallback: %s\n", reason)
	}
}

// stageMainStoryboard preserves Xcode's UIMainStoryboardFile semantics for the
// standard Flutter Main.storyboard. UIKit loads this storyboard before calling
// AppDelegate.application(_:didFinishLaunchingWithOptions:), so apps that read
// AppDelegate.window in that callback need a real compiled storyboard rather
// than a window created later by SceneDelegate.
//
// Rich/custom main interfaces remain outside ripley's Linux-only compiler. They
// fall back to the programmatic root explicitly; the stock Flutter shape is
// compiled losslessly to storyboardc.
func stageMainStoryboard(config iosProject, app string, info map[string]any) error {
	name, _ := info["UIMainStoryboardFile"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	path := storyboard.LaunchStoryboardPath(config.SourceDir, name)
	if path == "" {
		fmt.Printf("main storyboard fallback: %s was not found under %s; using programmatic Flutter root\n", name, config.SourceDir)
		delete(info, "UIMainStoryboardFile")
		return nil
	}
	analysis, err := storyboard.AnalyzeMainInterface(path)
	if err != nil {
		return fmt.Errorf("analyze main storyboard %s: %w", path, err)
	}
	if !analysis.EquivalentToProgrammaticFlutterRoot {
		for _, reason := range analysis.Reasons {
			fmt.Printf("main storyboard fallback: %s\n", reason)
		}
		delete(info, "UIMainStoryboardFile")
		return nil
	}

	rel, err := filepath.Rel(config.SourceDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(path)
	}
	resourceDir := app
	if dir := filepath.Dir(rel); dir != "." && strings.HasSuffix(dir, ".lproj") {
		resourceDir = filepath.Join(app, dir)
	}
	out := filepath.Join(resourceDir, storyboard.DocumentName(path)+".storyboardc")
	compiled, err := storyboard.CompileMainStoryboard(path, out)
	if err != nil {
		return fmt.Errorf("compile main storyboard %s: %w", path, err)
	}
	// copyIOSResources copied the source XML as part of the localization
	// directory; Xcode ships the compiled storyboard, not the source document.
	_ = os.Remove(filepath.Join(resourceDir, filepath.Base(path)))
	fmt.Printf("main storyboard: compiled %s (%s, %s)\n", filepath.Base(out), compiled.ControllerNib, compiled.ViewNib)
	return nil
}

// stageLaunchStoryboard compiles a source launch storyboard into the same
// localized .storyboardc resource layout Xcode installs in an app bundle.
func stageLaunchStoryboard(config iosProject, app string, launch *storyboard.LaunchScreen) error {
	if launch == nil || launch.Path == "" {
		return nil
	}
	rel, err := filepath.Rel(config.SourceDir, launch.Path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(launch.Path)
	}
	resourceDir := app
	if dir := filepath.Dir(rel); dir != "." && strings.HasSuffix(dir, ".lproj") {
		resourceDir = filepath.Join(app, dir)
	}
	name := storyboard.DocumentName(launch.Path)
	out := filepath.Join(resourceDir, name+".storyboardc")
	compiled, err := storyboard.CompileLaunchStoryboard(launch.Path, out)
	if err != nil {
		return err
	}
	// copyIOSResources copied the source XML as part of Base.lproj; Xcode ships
	// only the compiled resource, so remove the unused source copy.
	_ = os.Remove(filepath.Join(resourceDir, filepath.Base(launch.Path)))
	fmt.Printf("launch storyboard: compiled %s (%s, %s)\n", filepath.Base(out), compiled.ControllerNib, compiled.ViewNib)
	return nil
}

// compileAssetCatalogs lowers the target's `.xcassets` directories into a real
// `Assets.car` plus the loose icon files and `CFBundleIcons` keys UIKit falls
// back to, replacing Xcode's `actool`. Colors the lowered launch screen resolves
// by name are synthesized into an extra catalog first, because
// `UILaunchScreen`'s `UIColorName` can only name an asset-catalog color and
// `UIColor(named:)` never reads loose files.
func compileAssetCatalogs(tc toolchain.Toolchain, project project, config iosProject, app string, info map[string]any, launch *storyboard.LaunchScreen) error {
	catalogs, err := filepath.Glob(filepath.Join(config.SourceDir, "*.xcassets"))
	if err != nil {
		return err
	}
	work := filepath.Join(project.BuildDir, "assetcatalog")
	generated, err := synthesizeLaunchColors(work, launch)
	if err != nil {
		return err
	}
	if generated != "" {
		catalogs = append(catalogs, generated)
	}
	if len(catalogs) == 0 {
		return nil
	}
	result, err := assetcatalog.Compile(catalogs, work, assetcatalog.Options{
		DeploymentTarget: config.MinOS,
		AppIconName:      strings.Trim(config.BuildSettings["ASSETCATALOG_COMPILER_APPICON_NAME"], `"`),
	})
	if err != nil {
		return err
	}
	for _, item := range result.Skipped {
		fmt.Printf("asset catalog: skipped %s (%s): %s\n", item.Item, item.Kind, item.Reason)
	}
	if result.CarPath != "" {
		if err := copyFile(result.CarPath, filepath.Join(app, "Assets.car")); err != nil {
			return err
		}
	}
	for _, file := range result.LooseFiles {
		if err := copyFile(file, filepath.Join(app, filepath.Base(file))); err != nil {
			return err
		}
	}
	// Icon keys describe files that now exist in the bundle, so they replace any
	// stale project values.
	for key, value := range result.InfoPlistKeys {
		info[key] = value
	}
	return nil
}

// synthesizeLaunchColors materializes a `.xcassets` holding the colorsets a
// lowered launch screen needs and the project does not already author. It
// returns "" when nothing must be generated.
//
// Only colors flagged Generated are synthesized: a color the project already
// ships as a colorset carries appearance data the storyboard's `<namedColor>`
// mirror omits, so overwriting it would silently drop the dark variant.
func synthesizeLaunchColors(work string, launch *storyboard.LaunchScreen) (string, error) {
	if launch == nil {
		return "", nil
	}
	var needed []storyboard.RequiredColor
	for _, color := range launch.RequiredColors {
		if color.Generated {
			needed = append(needed, color)
		}
	}
	if len(needed) == 0 {
		return "", nil
	}
	root := filepath.Join(work, "GeneratedLaunch.xcassets")
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "Contents.json"), []byte(`{"info":{"author":"ripley","version":1}}`), 0o644); err != nil {
		return "", err
	}
	for _, color := range needed {
		dir := filepath.Join(root, color.Name+".colorset")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		entries := []map[string]any{colorsetEntry(color.R, color.G, color.B, color.A, false)}
		if color.Dynamic {
			entries = append(entries, colorsetEntry(color.DarkR, color.DarkG, color.DarkB, color.DarkA, true))
		}
		contents := map[string]any{
			"info":   map[string]any{"author": "ripley", "version": 1},
			"colors": entries,
		}
		data, err := json.Marshal(contents)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "Contents.json"), data, 0o644); err != nil {
			return "", err
		}
	}
	return root, nil
}

func colorsetEntry(r, g, b, a float64, dark bool) map[string]any {
	entry := map[string]any{
		"idiom": "universal",
		"color": map[string]any{
			"color-space": "srgb",
			"components": map[string]any{
				"red":   strconv.FormatFloat(r, 'f', 6, 64),
				"green": strconv.FormatFloat(g, 'f', 6, 64),
				"blue":  strconv.FormatFloat(b, 'f', 6, 64),
				"alpha": strconv.FormatFloat(a, 'f', 6, 64),
			},
		},
	}
	if dark {
		entry["appearances"] = []map[string]any{{"appearance": "luminosity", "value": "dark"}}
	}
	return entry
}

func plistBuildSettingValue(value string) any {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	if strings.EqualFold(value, "YES") {
		return true
	}
	if strings.EqualFold(value, "NO") {
		return false
	}
	if number, err := strconv.Atoi(value); err == nil {
		return number
	}
	return value
}

func deviceFamilies(value string) []int {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	if value == "" {
		return nil
	}
	var families []int
	for _, part := range strings.Split(value, ",") {
		number, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil {
			families = append(families, number)
		}
	}
	return families
}

func rewriteDirectFlutterSceneDelegate(info map[string]any, moduleName string) {
	if moduleName == "" {
		return
	}
	manifest, ok := info["UIApplicationSceneManifest"].(map[string]any)
	if !ok {
		return
	}
	configs, ok := manifest["UISceneConfigurations"].(map[string]any)
	if !ok {
		return
	}
	for _, raw := range configs {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				continue
			}
			delegate, _ := entry["UISceneDelegateClassName"].(string)
			if delegate == "FlutterSceneDelegate" {
				entry["UISceneDelegateClassName"] = moduleName + ".SceneDelegate"
			}
		}
	}
}

func stripSceneStoryboard(info map[string]any) {
	manifest, ok := info["UIApplicationSceneManifest"].(map[string]any)
	if !ok {
		return
	}
	configs, ok := manifest["UISceneConfigurations"].(map[string]any)
	if !ok {
		return
	}
	for _, raw := range configs {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if ok {
				delete(entry, "UISceneStoryboardFile")
			}
		}
	}
}

// ensurePlistLaunchScreen keeps UIKit out of the legacy/letterboxed compatibility
// mode for projects that declare no usable launch storyboard. A successfully
// compiled `.storyboardc` keeps UILaunchStoryboardName and returns above; when
// compilation falls back, iOS 14+ UILaunchScreen keys are already present here.
// Preserve any project-supplied plist launch description.
func ensurePlistLaunchScreen(info map[string]any) {
	if name, _ := info["UILaunchStoryboardName"].(string); strings.TrimSpace(name) != "" {
		return
	}
	if _, ok := info["UILaunchScreen"]; ok {
		return
	}
	if _, ok := info["UILaunchScreens"]; ok {
		return
	}
	info["UILaunchScreen"] = map[string]any{}
}

func copyIOSResources(iosProject iosProject, app string) error {
	entries, err := os.ReadDir(iosProject.SourceDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	skip := map[string]bool{".storyboard": true, ".xcassets": true, ".swift": true, ".m": true, ".h": true, ".plist": true}
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if skip[ext] {
			continue
		}
		src := filepath.Join(iosProject.SourceDir, entry.Name())
		dst := filepath.Join(app, entry.Name())
		if entry.IsDir() {
			if ext != ".lproj" {
				continue
			}
			if err := copyDir(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func writePlist(path string, value any) error {
	data, err := plist.Marshal(value, plist.XMLFormat)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func makeIPA(appPath, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	var paths []string
	err = filepath.WalkDir(appPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != appPath {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		zw.Close()
		f.Close()
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			zw.Close()
			f.Close()
			return err
		}
		rel, err := filepath.Rel(appPath, path)
		if err != nil {
			zw.Close()
			f.Close()
			return err
		}
		header, err := zip.FileInfoHeader(st)
		if err != nil {
			zw.Close()
			f.Close()
			return err
		}
		header.Name = filepath.ToSlash(filepath.Join("Payload", filepath.Base(appPath), rel))
		if st.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}
		w, err := zw.CreateHeader(header)
		if err != nil {
			zw.Close()
			f.Close()
			return err
		}
		if st.IsDir() {
			continue
		}
		r, err := os.Open(path)
		if err != nil {
			zw.Close()
			f.Close()
			return err
		}
		_, copyErr := io.Copy(w, r)
		r.Close()
		if copyErr != nil {
			zw.Close()
			f.Close()
			return copyErr
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func needsRegistrant(sourceDir string) (bool, error) {
	entries, err := os.ReadDir(sourceDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".swift" && filepath.Ext(entry.Name()) != ".m") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sourceDir, entry.Name()))
		if err != nil {
			return false, err
		}
		if strings.Contains(string(data), "GeneratedPluginRegistrant") {
			return true, nil
		}
	}
	return false, nil
}
