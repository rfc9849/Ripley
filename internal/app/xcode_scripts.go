package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ripley/internal/ios"
	"ripley/internal/toolchain"
)

// applicationScriptPhaseHandledInternally reports whether ripley already performs a
// phase's work itself, in which case running the phase would duplicate it.
//
// srcRoot is the target's SRCROOT, needed because a phase may reach Flutter's
// xcode_backend.sh through a project wrapper script rather than naming it
// directly.
func applicationScriptPhaseHandledInternally(phase xcodeShellScriptPhase, srcRoot string) bool {
	if ios.ScriptInvokesFlutterBackend(phase.Script, srcRoot, false) {
		return true
	}
	if ios.ScriptInvokesFlutterBackend(phase.Script, srcRoot, true) {
		return true
	}
	switch phase.Name {
	case "[CP] Check Pods Manifest.lock", "[CP] Embed Pods Frameworks", "[CP] Copy Pods Resources":
		return true
	}
	return false
}

func expandApplicationScriptValue(value string, values map[string]string) string {
	result := value
	for range 16 {
		before := result
		result = buildVariableRE.ReplaceAllStringFunc(result, func(token string) string {
			match := buildVariableRE.FindStringSubmatch(token)
			name := match[1]
			if name == "" {
				name = match[2]
			}
			if colon := strings.IndexByte(name, ':'); colon >= 0 {
				name = name[:colon]
			}
			if replacement, ok := values[name]; ok {
				return replacement
			}
			return token
		})
		if result == before {
			break
		}
	}
	return result
}

func applicationScriptEnvironment(tc toolchain.Toolchain, project project, config iosProject, phase xcodeShellScriptPhase) ([]string, map[string]string, error) {
	sdk, err := tc.IOSSDK()
	if err != nil {
		return nil, nil, err
	}
	sdkVersion, err := tc.IOSSDKVersion()
	if err != nil {
		return nil, nil, err
	}
	toolchainRoot, err := tc.SwiftToolchainRoot()
	if err != nil {
		return nil, nil, err
	}
	values := make(map[string]string, len(config.BuildSettings)+64)
	for key, value := range config.BuildSettings {
		values[key] = value
	}
	appName := config.ProductName + ".app"
	appPath := filepath.Join(project.BuildDir, appName)
	intermediates := filepath.Join(project.BuildDir, ".app_build", "Intermediates")
	derived := filepath.Join(project.BuildDir, ".app_build", "DerivedSources")
	for _, dir := range []string{project.BuildDir, intermediates, derived} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
	}

	values["ACTION"] = "build"
	values["ARCHS"] = "arm64"
	values["CURRENT_ARCH"] = "arm64"
	values["NATIVE_ARCH"] = "arm64"
	values["ONLY_ACTIVE_ARCH"] = "YES"
	values["CONFIGURATION"] = config.Configuration
	values["PLATFORM_NAME"] = "iphoneos"
	values["EFFECTIVE_PLATFORM_NAME"] = "-iphoneos"
	values["SDKROOT"] = sdk
	values["SDK_NAME"] = "iphoneos" + sdkVersion
	values["IPHONEOS_DEPLOYMENT_TARGET"] = config.MinOS
	values["SRCROOT"] = config.IOSRoot
	values["SOURCE_ROOT"] = config.IOSRoot
	values["PROJECT_DIR"] = config.IOSRoot
	values["PROJECT_FILE_PATH"] = config.XcodeProject
	values["PROJECT_NAME"] = strings.TrimSuffix(filepath.Base(config.XcodeProject), ".xcodeproj")
	values["PROJECT_BUILD_DIR"] = project.BuildDir
	values["BUILD_ROOT"] = project.BuildDir
	values["BUILD_DIR"] = project.BuildDir
	values["BUILT_PRODUCTS_DIR"] = project.BuildDir
	values["CONFIGURATION_BUILD_DIR"] = project.BuildDir
	values["TARGET_BUILD_DIR"] = project.BuildDir
	values["TARGET_NAME"] = config.TargetName
	values["TARGET_TEMP_DIR"] = intermediates
	values["CONFIGURATION_TEMP_DIR"] = intermediates
	values["PROJECT_TEMP_DIR"] = filepath.Join(project.BuildDir, ".app_build")
	values["TEMP_DIR"] = intermediates
	values["DERIVED_FILE_DIR"] = derived
	values["DERIVED_SOURCES_DIR"] = derived
	values["PRODUCT_NAME"] = config.ProductName
	values["PRODUCT_MODULE_NAME"] = config.ModuleName
	values["PRODUCT_BUNDLE_IDENTIFIER"] = config.BundleID
	values["FULL_PRODUCT_NAME"] = appName
	values["WRAPPER_NAME"] = appName
	values["CONTENTS_FOLDER_PATH"] = appName
	values["EXECUTABLE_NAME"] = config.ExecutableName
	values["EXECUTABLE_PATH"] = filepath.Join(appName, config.ExecutableName)
	values["INFOPLIST_PATH"] = filepath.Join(appName, "Info.plist")
	values["FRAMEWORKS_FOLDER_PATH"] = filepath.Join(appName, "Frameworks")
	values["UNLOCALIZED_RESOURCES_FOLDER_PATH"] = appName
	values["DWARF_DSYM_FOLDER_PATH"] = project.BuildDir
	values["DWARF_DSYM_FILE_NAME"] = appName + ".dSYM"
	values["FLUTTER_ROOT"] = config.FlutterRoot
	values["FLUTTER_APPLICATION_PATH"] = project.Root
	values["TOOLCHAIN_DIR"] = toolchainRoot
	values["DT_TOOLCHAIN_DIR"] = toolchainRoot
	values["CODESIGNING_FOLDER_PATH"] = appPath

	cocoaRoot := filepath.Join(project.BuildDir, "cocoapods")
	if dirExists(filepath.Join(cocoaRoot, "Pods")) {
		values["PODS_ROOT"] = filepath.Join(cocoaRoot, "Pods")
		values["PODS_PODFILE_DIR_PATH"] = cocoaRoot
		values["PODS_BUILD_DIR"] = filepath.Join(cocoaRoot, "build")
		values["PODS_CONFIGURATION_BUILD_DIR"] = filepath.Join(cocoaRoot, "build", "Release-iphoneos")
		values["PODS_XCFRAMEWORKS_BUILD_DIR"] = filepath.Join(cocoaRoot, "build", "Release-iphoneos", "XCFrameworkIntermediates")
	} else if dirExists(filepath.Join(config.IOSRoot, "Pods")) {
		values["PODS_ROOT"] = filepath.Join(config.IOSRoot, "Pods")
		values["PODS_PODFILE_DIR_PATH"] = config.IOSRoot
	}

	for range 16 {
		changed := false
		for key, value := range values {
			expanded := expandApplicationScriptValue(value, values)
			if expanded != value {
				values[key] = expanded
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	setIndexed := func(prefix string, paths []string) {
		values[prefix+"_COUNT"] = strconv.Itoa(len(paths))
		for index, path := range paths {
			values[fmt.Sprintf("%s_%d", prefix, index)] = expandApplicationScriptValue(path, values)
		}
	}
	setIndexed("SCRIPT_INPUT_FILE", phase.InputPaths)
	setIndexed("SCRIPT_OUTPUT_FILE", phase.OutputPaths)
	setIndexed("SCRIPT_INPUT_FILE_LIST", phase.InputFileListPaths)
	setIndexed("SCRIPT_OUTPUT_FILE_LIST", phase.OutputFileListPaths)

	base := make(map[string]string)
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			base[entry[:index]] = entry[index+1:]
		}
	}
	for key, value := range values {
		base[key] = value
	}
	pathParts := []string{filepath.Join(toolchainRoot, "usr", "bin"), tc.ToolsetBin()}
	if current := base["PATH"]; current != "" {
		pathParts = append(pathParts, current)
	}
	base["PATH"] = strings.Join(pathParts, string(os.PathListSeparator))

	keys := make([]string, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+base[key])
	}
	values["PATH"] = base["PATH"]
	return env, values, nil
}

// applicationScriptPhaseIO resolves the phase's declared dependency graph. Xcode
// treats the contents of an .xcfilelist as real inputs/outputs, not just the
// list file itself, so both are collected.
func applicationScriptPhaseIO(phase xcodeShellScriptPhase, values map[string]string, shell, scriptFile string) (ios.ScriptPhaseIO, error) {
	expand := func(value string) string { return expandApplicationScriptValue(value, values) }
	io := ios.ScriptPhaseIO{
		Name:            phase.Name,
		Script:          phase.Script,
		ShellPath:       shell,
		AlwaysOutOfDate: phase.AlwaysOutOfDate,
		DependencyFile:  expand(phase.DependencyFile),
		StateFile:       scriptFile + ".state",
		SearchPath:      values["PATH"],
	}
	for _, path := range phase.InputPaths {
		io.Inputs = append(io.Inputs, expand(path))
	}
	for _, path := range phase.OutputPaths {
		io.Outputs = append(io.Outputs, expand(path))
	}
	for _, list := range phase.InputFileListPaths {
		list = expand(list)
		io.Inputs = append(io.Inputs, list)
		entries, err := ios.ReadScriptFileList(list, expand)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ios.ScriptPhaseIO{}, err
		}
		io.Inputs = append(io.Inputs, entries...)
	}
	for _, list := range phase.OutputFileListPaths {
		list = expand(list)
		io.Inputs = append(io.Inputs, list)
		entries, err := ios.ReadScriptFileList(list, expand)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ios.ScriptPhaseIO{}, err
		}
		io.Outputs = append(io.Outputs, entries...)
	}
	return io, nil
}

func runApplicationShellPhase(tc toolchain.Toolchain, project project, config iosProject, phase xcodeShellScriptPhase) error {
	if applicationScriptPhaseHandledInternally(phase, config.IOSRoot) || phase.RunOnlyForDeploymentPostprocessing || strings.TrimSpace(phase.Script) == "" {
		return nil
	}
	env, values, err := applicationScriptEnvironment(tc, project, config, phase)
	if err != nil {
		return err
	}
	shell, err := ios.ResolveScriptShell(phase.ShellPath)
	if err != nil {
		return fmt.Errorf("Xcode script phase %q: %w", phase.Name, err)
	}
	scriptDir := filepath.Join(project.BuildDir, ".app_build", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return err
	}
	scriptFile := filepath.Join(scriptDir, fmt.Sprintf("%03d-%s", phase.Order, safeScriptName(phase.Name)))
	if err := os.WriteFile(scriptFile, []byte(phase.Script+"\n"), 0o755); err != nil {
		return err
	}

	io, err := applicationScriptPhaseIO(phase, values, shell, scriptFile)
	if err != nil {
		return err
	}
	upToDate, err := ios.ScriptPhaseUpToDate(io)
	if err != nil {
		return err
	}
	if upToDate {
		fmt.Printf("  script %s: %s (up to date)\n", config.TargetName, phase.Name)
		return nil
	}

	fmt.Printf("  script %s: %s\n", config.TargetName, phase.Name)
	cmd := exec.Command(shell, scriptFile)
	cmd.Dir = config.IOSRoot
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		err = ios.AnnotateScriptPhaseError(err, phase.Script, values["PATH"])
		return fmt.Errorf("Xcode script phase %q for target %s failed: %w", phase.Name, config.TargetName, err)
	}
	return ios.RecordScriptPhaseState(io)
}

type applicationScriptStage int

const (
	applicationScriptBeforeFlutter applicationScriptStage = iota
	applicationScriptAfterFlutter
	applicationScriptAfterSources
)

func applicationPhaseStage(phase xcodeShellScriptPhase) applicationScriptStage {
	if !phase.BeforeSources {
		return applicationScriptAfterSources
	}
	if phase.AfterFlutterBuild {
		return applicationScriptAfterFlutter
	}
	return applicationScriptBeforeFlutter
}

func safeScriptName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "Run_Script"
	}
	return b.String()
}

func runApplicationShellPhases(tc toolchain.Toolchain, project project, config iosProject, stage applicationScriptStage) error {
	phases := append([]xcodeShellScriptPhase(nil), config.ShellScriptPhases...)
	sort.SliceStable(phases, func(i, j int) bool { return phases[i].Order < phases[j].Order })
	for _, phase := range phases {
		if applicationPhaseStage(phase) != stage {
			continue
		}
		if err := runApplicationShellPhase(tc, project, config, phase); err != nil {
			return err
		}
	}
	return nil
}
