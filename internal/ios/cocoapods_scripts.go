package ios

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ripley/internal/toolchain"
)

var xcodeVariablePattern = regexp.MustCompile(`\$\(([^)]+)\)|\$\{([^}]+)\}`)

func expandXcodeVariables(value string, values map[string]string) string {
	result := value
	for range 16 {
		before := result
		result = xcodeVariablePattern.ReplaceAllStringFunc(result, func(match string) string {
			parts := xcodeVariablePattern.FindStringSubmatch(match)
			key := parts[1]
			if key == "" {
				key = parts[2]
			}
			if colon := strings.IndexByte(key, ':'); colon >= 0 {
				key = key[:colon]
			}
			if replacement, ok := values[key]; ok {
				return replacement
			}
			return match
		})
		if result == before {
			break
		}
	}
	return result
}

func podScriptPhaseHandledInternally(phase podShellScriptPhase) bool {
	return strings.Contains(phase.Name, "[CP] Copy XCFrameworks") ||
		strings.Contains(phase.Name, "Copy generated compatibility header")
}

func podScriptEnvironment(tc toolchain.Toolchain, manifest podManifest, target podTarget, configDir, minOS string, phase podShellScriptPhase) ([]string, map[string]string, error) {
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

	values := make(map[string]string, len(target.BuildSettings)+64)
	for key, value := range target.BuildSettings {
		values[key] = value
	}
	targetOut := filepath.Join(configDir, target.Name)
	derived := filepath.Join(targetOut, "DerivedSources")
	intermediates := filepath.Join(targetOut, "Intermediates")
	objects := filepath.Join(targetOut, "objects")
	for _, dir := range []string{targetOut, derived, intermediates, objects, filepath.Join(configDir, "XCFrameworkIntermediates")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
	}

	product := target.ProductName
	if product == "" {
		product = target.Name
	}
	fullProduct := "lib" + strings.TrimPrefix(product, "lib") + ".a"
	values["ACTION"] = "build"
	values["ARCHS"] = "arm64"
	values["CURRENT_ARCH"] = "arm64"
	values["NATIVE_ARCH"] = "arm64"
	values["ONLY_ACTIVE_ARCH"] = "YES"
	values["CONFIGURATION"] = "Release"
	values["PLATFORM_NAME"] = "iphoneos"
	values["EFFECTIVE_PLATFORM_NAME"] = "-iphoneos"
	values["SDKROOT"] = sdk
	values["SDK_NAME"] = "iphoneos" + sdkVersion
	values["IPHONEOS_DEPLOYMENT_TARGET"] = maxVersion(target.DeploymentTarget, minOS)
	values["SRCROOT"] = manifest.PodsRoot
	values["SOURCE_ROOT"] = manifest.PodsRoot
	values["PROJECT_DIR"] = manifest.PodsRoot
	values["PROJECT_FILE_PATH"] = filepath.Join(manifest.PodsRoot, "Pods.xcodeproj")
	values["PODS_ROOT"] = manifest.PodsRoot
	values["BUILD_ROOT"] = filepath.Dir(configDir)
	values["BUILD_DIR"] = filepath.Dir(configDir)
	values["PODS_BUILD_DIR"] = filepath.Dir(configDir)
	values["PODS_CONFIGURATION_BUILD_DIR"] = configDir
	values["PODS_XCFRAMEWORKS_BUILD_DIR"] = filepath.Join(configDir, "XCFrameworkIntermediates")
	// Xcode builds each CocoaPods native target in its own configuration dir.
	// BUILT_PRODUCTS_DIR follows CONFIGURATION_BUILD_DIR for that target; this
	// matters for script phases such as cargokit, whose podspec both writes and
	// force-loads ${BUILT_PRODUCTS_DIR}/lib<crate>.a.
	values["BUILT_PRODUCTS_DIR"] = targetOut
	values["CONFIGURATION_BUILD_DIR"] = targetOut
	values["TARGET_BUILD_DIR"] = targetOut
	values["TARGET_NAME"] = target.Name
	values["TARGET_TEMP_DIR"] = intermediates
	values["CONFIGURATION_TEMP_DIR"] = intermediates
	values["PROJECT_TEMP_DIR"] = filepath.Join(filepath.Dir(configDir), "Intermediates")
	values["TEMP_DIR"] = intermediates
	values["DERIVED_FILE_DIR"] = derived
	values["DERIVED_SOURCES_DIR"] = derived
	values["OBJECT_FILE_DIR_normal"] = objects
	values["PRODUCT_NAME"] = product
	values["PRODUCT_MODULE_NAME"] = target.ModuleName
	if values["PRODUCT_MODULE_NAME"] == "" {
		values["PRODUCT_MODULE_NAME"] = target.Name
	}
	values["FULL_PRODUCT_NAME"] = fullProduct
	values["EXECUTABLE_NAME"] = fullProduct
	values["EXECUTABLE_PATH"] = fullProduct
	values["MACH_O_TYPE"] = "staticlib"
	values["PRODUCT_TYPE"] = target.ProductType
	values["TOOLCHAIN_DIR"] = toolchainRoot
	values["DT_TOOLCHAIN_DIR"] = toolchainRoot

	// Re-expand target settings after all concrete ripley/Xcode-compatible values are
	// available. CocoaPods settings frequently chain through PODS_TARGET_SRCROOT,
	// CONFIGURATION_BUILD_DIR, and custom xcconfig variables.
	for range 16 {
		changed := false
		for key, value := range values {
			expanded := expandXcodeVariables(value, values)
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
			values[fmt.Sprintf("%s_%d", prefix, index)] = expandXcodeVariables(path, values)
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

	// cargokit pods (rust_builder-style plugins) shell out to cargo from their
	// script phase. The Rust cross-toolchain is only injected when the pod
	// actually needs it, so projects without a Rust pod still build on hosts
	// that have no cargo installed.
	if podSrcRoot := values["PODS_TARGET_SRCROOT"]; podSrcRoot != "" {
		needsCargo, err := PodNeedsCargo(podSrcRoot)
		if err != nil {
			return nil, nil, err
		}
		if needsCargo {
			cargoEnv, err := CargokitScriptEnv(tc)
			if err != nil {
				return nil, nil, fmt.Errorf("pod %s needs a Rust toolchain: %w", target.Name, err)
			}
			// Applied last so cargokit's PATH (a superset of the one above)
			// wins and its wrapper shims shadow the host tools.
			for _, entry := range cargoEnv {
				if index := strings.IndexByte(entry, '='); index >= 0 {
					base[entry[:index]] = entry[index+1:]
				}
			}
		}
	}

	keys := make([]string, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+base[key])
	}
	return env, values, nil
}

func fileListEntries(path string, values map[string]string) ([]string, error) {
	return ReadScriptFileList(path, func(line string) string { return expandXcodeVariables(line, values) })
}

func runPodShellPhase(tc toolchain.Toolchain, manifest podManifest, target podTarget, configDir, minOS string, phase podShellScriptPhase) error {
	if podScriptPhaseHandledInternally(phase) || phase.RunOnlyForDeploymentPostprocessing || strings.TrimSpace(phase.Script) == "" {
		return nil
	}
	env, values, err := podScriptEnvironment(tc, manifest, target, configDir, minOS, phase)
	if err != nil {
		return err
	}
	shell, err := ResolveScriptShell(phase.ShellPath)
	if err != nil {
		return fmt.Errorf("CocoaPods script phase %q: %w", phase.Name, err)
	}
	scriptDir := filepath.Join(configDir, target.Name, "Intermediates", "Scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return err
	}
	scriptFile := filepath.Join(scriptDir, fmt.Sprintf("%03d-%s", phase.Order, podSafeScriptName(phase.Name)))
	if err := os.WriteFile(scriptFile, []byte(phase.Script+"\n"), 0o755); err != nil {
		return err
	}

	io, err := podScriptPhaseIO(phase, values, shell, scriptFile)
	if err != nil {
		return err
	}
	upToDate, err := ScriptPhaseUpToDate(io)
	if err != nil {
		return err
	}
	if upToDate {
		fmt.Printf("  script %s: %s (up to date)\n", target.Name, phase.Name)
		return nil
	}

	fmt.Printf("  script %s: %s\n", target.Name, phase.Name)
	cmd := exec.Command(shell, scriptFile)
	// CocoaPods normally runs user phases from ios/Pods, which lets Flutter
	// package-aware scripts walk ../.. back to the package root. Our resolver
	// Pods directory lives under build/ripley_ios/cocoapods, so that filesystem
	// geometry does not exist. Run from the real package root instead while
	// preserving CocoaPods/Xcode paths through SRCROOT, PODS_ROOT and
	// PODS_TARGET_SRCROOT in the environment. Scripts using those settings keep
	// Xcode semantics; Dart/Flutter scripts can resolve pubspec.yaml and
	// .dart_tool/package_config.json from their working directory.
	cmd.Dir = manifest.ProjectRoot
	if cmd.Dir == "" {
		cmd.Dir = manifest.PodsRoot
	}
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		err = AnnotateScriptPhaseError(err, phase.Script, values["PATH"])
		return fmt.Errorf("CocoaPods script phase %q for target %s failed: %w", phase.Name, target.Name, err)
	}
	if err := RecordScriptPhaseState(io); err != nil {
		return err
	}
	return nil
}

// podScriptPhaseIO resolves the phase's declared dependency graph. Xcode treats
// the contents of an .xcfilelist as real inputs/outputs, not just the list file
// itself, so both are collected.
func podScriptPhaseIO(phase podShellScriptPhase, values map[string]string, shell, scriptFile string) (ScriptPhaseIO, error) {
	io := ScriptPhaseIO{
		Name:            phase.Name,
		Script:          phase.Script,
		ShellPath:       shell,
		AlwaysOutOfDate: phase.AlwaysOutOfDate,
		DependencyFile:  expandXcodeVariables(phase.DependencyFile, values),
		StateFile:       scriptFile + ".state",
		SearchPath:      values["PATH"],
	}
	for _, path := range phase.InputPaths {
		io.Inputs = append(io.Inputs, expandXcodeVariables(path, values))
	}
	for _, path := range phase.OutputPaths {
		io.Outputs = append(io.Outputs, expandXcodeVariables(path, values))
	}
	for _, list := range phase.InputFileListPaths {
		list = expandXcodeVariables(list, values)
		io.Inputs = append(io.Inputs, list)
		entries, err := fileListEntries(list, values)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ScriptPhaseIO{}, err
		}
		io.Inputs = append(io.Inputs, entries...)
	}
	for _, list := range phase.OutputFileListPaths {
		list = expandXcodeVariables(list, values)
		io.Inputs = append(io.Inputs, list)
		entries, err := fileListEntries(list, values)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ScriptPhaseIO{}, err
		}
		io.Outputs = append(io.Outputs, entries...)
	}
	return io, nil
}

func podSafeScriptName(name string) string {
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

// podScriptStage mirrors CocoaPods' execution_position semantics. The recorded
// PBX order is authoritative; :any collapses onto whichever boundary the phase
// actually sits on.
type podScriptStage int

const (
	podScriptBeforeHeaders podScriptStage = iota
	podScriptAfterHeaders
	podScriptAfterSources
)

func podPhaseStage(phase podShellScriptPhase) podScriptStage {
	if !phase.BeforeSources {
		return podScriptAfterSources
	}
	if phase.BeforeHeaders {
		return podScriptBeforeHeaders
	}
	return podScriptAfterHeaders
}

func runPodShellPhases(tc toolchain.Toolchain, manifest podManifest, target podTarget, configDir, minOS string, stage podScriptStage) error {
	phases := append([]podShellScriptPhase(nil), target.ShellScriptPhases...)
	sort.SliceStable(phases, func(i, j int) bool { return phases[i].Order < phases[j].Order })
	for _, phase := range phases {
		if podPhaseStage(phase) != stage {
			continue
		}
		if err := runPodShellPhase(tc, manifest, target, configDir, minOS, phase); err != nil {
			return err
		}
	}
	return nil
}
