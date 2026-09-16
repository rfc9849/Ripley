package ios

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ripley/internal/toolchain"
)

// cargoIOSTarget is the only Rust target triple an iOS device build needs.
// cargokit derives it from CARGOKIT_DARWIN_PLATFORM_NAME + CARGOKIT_DARWIN_ARCHS
// (iphoneos + arm64) and ripley only ever builds arm64 device slices.
const cargoIOSTarget = "aarch64-apple-ios"

// cargoLinkerWrapperName is the clang wrapper generated into the toolchain
// cache. Cargo passes the linker only object/library arguments, so every
// iOS-specific flag has to live inside a wrapper script.
const cargoLinkerWrapperName = "ripley-clang"

// cargoMinimumLinkerVersion makes clang emit `-platform_version ios <min> <sdk>`
// instead of the legacy `-iphoneos_version_min <min>` spelling. This is load
// bearing: with the legacy flag, ld64.lld fails the link with
//
//	ld64.lld: error: must specify -platform_version
//	ld64.lld: error: missing or unsupported -arch arm64
//
// Apple's ld64 gained -platform_version in linker version 520 and clang gates
// the new spelling behind -mlinker-version >= 520.
const cargoMinimumLinkerVersion = "1053"

// iOSMinimumForCargo is the deployment target Rust objects are compiled
// against. Rust's aarch64-apple-ios target sets its own floor; this value only
// has to be no higher than the app's own minimum, because the linker records
// the maximum of the two when the archive is force-loaded into the app.
const iOSMinimumForCargo = "12.0"

// CargoEnv is the environment a cargokit script phase needs so that its
// `cargo build --target aarch64-apple-ios` compiles and links against the
// iPhoneOS SDK with ld64.lld, instead of Apple's ld and Xcode's clang.
type CargoEnv struct {
	// Cargo is the absolute path to the cargo executable.
	Cargo string
	// Rustup is the absolute path to rustup, or "" when cargo was found
	// outside a rustup installation. cargokit's build_tool shells
	// `rustup run <channel> cargo build`, so rustup has to be on PATH.
	Rustup string
	// LinkerWrapper is the generated clang wrapper used both as the cargo
	// linker and as CC/CXX for crates with `cc`-based build scripts.
	LinkerWrapper string
	// SDK is the iPhoneOS SDK root passed as -isysroot and as SDKROOT.
	SDK string
	// LLD is the ld64.lld reached through clang's -fuse-ld.
	LLD string
	// AR is the archiver used for staticlib crates and `cc` build scripts.
	AR string
	// DeploymentTarget is the iOS version the crate is compiled against.
	DeploymentTarget string
	// PathPrepend are the directories prepended to PATH: cargo, rustup, the
	// generated wrappers, the Linux stand-ins for the Darwin tools
	// cargokit's build_pod.dart shells out to, plus the Swift/toolset/Dart
	// directories a pod script phase already relies on.
	PathPrepend []string
}

// Env returns the variables to inject into a cargokit script phase, in
// KEY=value form. Cargo's per-target variables use the underscored triple:
// CARGO_TARGET_AARCH64_APPLE_IOS_LINKER is upper-cased with dashes replaced,
// while CC_aarch64_apple_ios keeps the lower-case triple.
//
// The PATH entry is a superset of the one a pod script phase normally gets, so
// it is safe for the caller to append these entries after its own environment
// even though the last PATH wins in os/exec.
func (c CargoEnv) Env() []string {
	underscored := strings.ReplaceAll(cargoIOSTarget, "-", "_")
	upper := strings.ToUpper(underscored)
	separator := string(os.PathListSeparator)
	return []string{
		"CARGO=" + c.Cargo,
		"CARGO_TARGET_" + upper + "_LINKER=" + c.LinkerWrapper,
		"CC_" + underscored + "=" + c.LinkerWrapper,
		"CXX_" + underscored + "=" + c.LinkerWrapper,
		"AR_" + underscored + "=" + c.AR,
		"SDKROOT=" + c.SDK,
		"IPHONEOS_DEPLOYMENT_TARGET=" + c.DeploymentTarget,
		"PATH=" + strings.Join(c.PathPrepend, separator) + separator + os.Getenv("PATH"),
	}
}

// CargoToolchain locates cargo, verifies the iOS target is installed, and
// materializes the wrapper scripts a cargokit cargo build needs.
func CargoToolchain(tc toolchain.Toolchain) (CargoEnv, error) {
	cargo, rustup, err := findCargo()
	if err != nil {
		return CargoEnv{}, err
	}
	if err := verifyCargoIOSTarget(rustup); err != nil {
		return CargoEnv{}, err
	}

	sdk, err := tc.IOSSDK()
	if err != nil {
		return CargoEnv{}, err
	}
	lld, err := resolveCargoLinker(tc)
	if err != nil {
		return CargoEnv{}, err
	}
	linkerSDK, err := tc.LinkerIOSSDK()
	if err != nil {
		return CargoEnv{}, err
	}
	clang, err := exec.LookPath("clang")
	if err != nil {
		return CargoEnv{}, fmt.Errorf("cargokit needs clang to compile Rust for iOS: %w", err)
	}
	archiver, err := resolveCargoArchiver()
	if err != nil {
		return CargoEnv{}, err
	}

	binDir := cargoWrapperDir(tc)
	lldShim := filepath.Join(binDir, "ld64.lld")
	if err := writeLD64SDKShim(lldShim, lld, sdk, linkerSDK); err != nil {
		return CargoEnv{}, err
	}
	wrapper, err := writeCargoLinkerWrapper(binDir, clang, sdk, lldShim, iOSMinimumForCargo)
	if err != nil {
		return CargoEnv{}, err
	}
	if err := writeCargoDarwinToolShims(binDir); err != nil {
		return CargoEnv{}, err
	}

	return CargoEnv{
		Cargo:            cargo,
		Rustup:           rustup,
		LinkerWrapper:    wrapper,
		SDK:              sdk,
		LLD:              lldShim,
		AR:               archiver,
		DeploymentTarget: iOSMinimumForCargo,
		PathPrepend:      cargoPathPrepend(tc, binDir, cargo, rustup),
	}, nil
}

// cargoPathPrepend builds the PATH prefix. It deliberately re-adds the
// Swift/toolset directories a pod script phase already gets, so that replacing
// the phase's PATH with this one never removes a tool the phase could reach
// before. The Dart SDK directory is added so run_build_tool.sh's bare `dart`
// fallback resolves on hosts where Dart is not otherwise on PATH.
func cargoPathPrepend(tc toolchain.Toolchain, binDir, cargo, rustup string) []string {
	parts := []string{binDir, filepath.Dir(cargo)}
	if rustup != "" {
		parts = append(parts, filepath.Dir(rustup))
	}
	if swiftBin, err := tc.SwiftBin(); err == nil {
		parts = append(parts, swiftBin)
	}
	parts = append(parts, tc.ToolsetBin(), filepath.Join(tc.DartSDK(), "bin"))

	seen := make(map[string]bool, len(parts))
	unique := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		unique = append(unique, part)
	}
	return unique
}

// CargokitScriptEnv returns the environment entries a cargokit script phase
// needs. It always returns the full environment; deciding whether a given pod
// needs it at all is the caller's job (see PodNeedsCargo).
func CargokitScriptEnv(tc toolchain.Toolchain) ([]string, error) {
	env, err := CargoToolchain(tc)
	if err != nil {
		return nil, err
	}
	return env.Env(), nil
}

// cargoNotInstalledError explains how to provision Rust for iOS. It reaches the
// user verbatim, so it names the exact commands to run.
func cargoNotInstalledError() error {
	return fmt.Errorf(`no cargo found: this project has a cargokit-based plugin that compiles Rust for iOS.
Install Rust and the iOS target on this build host:
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path
  ~/.cargo/bin/rustup target add %s
Or set RIPLEY_CARGO to an existing cargo executable.`, cargoIOSTarget)
}

// cargoTargetMissingError names the rustup invocation that installs the iOS
// standard library. Crates pinned by a rust-toolchain.toml need the target
// installed for that channel specifically. That is the common trap: installing
// it only for the default toolchain leaves the pinned channel without libcore
// and every dependency fails with "can't find crate for `core`".
func cargoTargetMissingError(rustup string, installed []string) error {
	command := "rustup"
	if rustup != "" {
		command = rustup
	}
	present := strings.Join(installed, ", ")
	if present == "" {
		present = "none"
	}
	return fmt.Errorf(`rust target %s is not installed (installed: %s).
Install it, including for any channel pinned by a rust-toolchain.toml next to the crate:
  %s target add %s
  %s target add %s --toolchain <channel from rust-toolchain.toml>`,
		cargoIOSTarget, present, command, cargoIOSTarget, command, cargoIOSTarget)
}

// findCargo resolves cargo from RIPLEY_CARGO, then ~/.cargo/bin, then PATH. It also
// reports the sibling rustup when one exists.
func findCargo() (cargo string, rustup string, err error) {
	if override := os.Getenv("RIPLEY_CARGO"); override != "" {
		if !isExecutableFile(override) {
			return "", "", fmt.Errorf("RIPLEY_CARGO=%s is not an executable file", override)
		}
		return override, siblingRustup(override), nil
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		candidate := filepath.Join(home, ".cargo", "bin", "cargo")
		if isExecutableFile(candidate) {
			return candidate, siblingRustup(candidate), nil
		}
	}
	found, lookErr := exec.LookPath("cargo")
	if lookErr != nil {
		return "", "", cargoNotInstalledError()
	}
	return found, siblingRustup(found), nil
}

func siblingRustup(cargo string) string {
	candidate := filepath.Join(filepath.Dir(cargo), "rustup")
	if isExecutableFile(candidate) {
		return candidate
	}
	if found, err := exec.LookPath("rustup"); err == nil {
		return found
	}
	return ""
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// verifyCargoIOSTarget checks that the iOS standard library is present. Without
// rustup the target list cannot be enumerated, so rustc's sysroot is probed for
// the target's rustlib directory instead.
func verifyCargoIOSTarget(rustup string) error {
	if rustup == "" {
		return verifyCargoIOSTargetViaSysroot()
	}
	out, err := exec.Command(rustup, "target", "list", "--installed").Output()
	if err != nil {
		return fmt.Errorf("%s target list --installed: %w", rustup, err)
	}
	installed := parseRustupTargets(out)
	for _, target := range installed {
		if target == cargoIOSTarget {
			return nil
		}
	}
	return cargoTargetMissingError(rustup, installed)
}

func parseRustupTargets(out []byte) []string {
	var targets []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			targets = append(targets, line)
		}
	}
	return targets
}

func verifyCargoIOSTargetViaSysroot() error {
	out, err := exec.Command("rustc", "--print", "sysroot").Output()
	if err != nil {
		return cargoTargetMissingError("", nil)
	}
	sysroot := strings.TrimSpace(string(out))
	if dirExists(filepath.Join(sysroot, "lib", "rustlib", cargoIOSTarget)) {
		return nil
	}
	return cargoTargetMissingError("", nil)
}

func cargoWrapperDir(tc toolchain.Toolchain) string {
	return filepath.Join(tc.Root, "cargo", "bin")
}

// resolveCargoLinker finds ld64.lld, preferring the toolchain cache copy the
// rest of the native pipeline links with.
func resolveCargoLinker(tc toolchain.Toolchain) (string, error) {
	cached := filepath.Join(tc.ToolsetBin(), "ld64.lld")
	if isExecutableFile(cached) {
		return cached, nil
	}
	if found, err := exec.LookPath("ld64.lld"); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("ld64.lld not found in %s or on PATH; it is required to link Rust code for iOS", tc.ToolsetBin())
}

// resolveCargoArchiver finds an archiver that writes Mach-O archives. llvm-ar
// handles Mach-O members; GNU ar does not on every version, so llvm-ar wins and
// versioned Debian/Ubuntu names are accepted.
func resolveCargoArchiver() (string, error) {
	for _, name := range llvmToolCandidates("llvm-ar") {
		if found, err := exec.LookPath(name); err == nil {
			return found, nil
		}
	}
	if found, err := exec.LookPath("ar"); err == nil {
		return found, nil
	}
	return "", errors.New("no llvm-ar or ar found; one is required to archive Rust static libraries for iOS")
}

// llvmToolCandidates expands an LLVM tool name into the unversioned name plus
// the versioned names Debian/Ubuntu ship (clang 20 is used on the reference Ubuntu host).
func llvmToolCandidates(name string) []string {
	candidates := []string{name}
	for _, version := range []string{"22", "21", "20", "19", "18"} {
		candidates = append(candidates, name+"-"+version)
	}
	return candidates
}

// writeCargoLinkerWrapper generates the clang wrapper script used as both the
// cargo linker and CC/CXX.
func writeCargoLinkerWrapper(binDir, clang, sdk, lld, minOS string) (string, error) {
	script := fmt.Sprintf(`#!/bin/sh
# Generated by ripley. Cargo linker/CC wrapper for %s.
exec %q -target arm64-apple-ios%s -isysroot %q -mlinker-version=%s -fuse-ld=%q "$@"
`, cargoIOSTarget, clang, minOS, sdk, cargoMinimumLinkerVersion, lld)
	return writeCargoShim(binDir, cargoLinkerWrapperName, script)
}

// darwinToolShims are the macOS-only binaries cargokit's build_pod.dart runs
// unconditionally. `install_name_tool -id` rewrites a dylib crate's id; LLVM's
// equivalent is drop-in for that use but on Linux is often installed only under
// a versioned name, so the shim puts the Darwin name on PATH. `lipo` is handled
// separately by writeCargoLipoShim.
var darwinToolShims = map[string]string{
	"install_name_tool": "llvm-install-name-tool",
}

func writeCargoDarwinToolShims(binDir string) error {
	for name, llvmName := range darwinToolShims {
		var real string
		for _, candidate := range llvmToolCandidates(llvmName) {
			if found, err := exec.LookPath(candidate); err == nil {
				real = found
				break
			}
		}
		if real == "" {
			if found, err := exec.LookPath(name); err == nil {
				real = found
			}
		}
		if real == "" {
			return fmt.Errorf("cargokit needs %s (or its LLVM equivalent %s) to assemble the Rust library for iOS", name, llvmName)
		}
		script := fmt.Sprintf("#!/bin/sh\n# Generated by ripley. Linux stand-in for Darwin's %s.\nexec %q \"$@\"\n", name, real)
		if _, err := writeCargoShim(binDir, name, script); err != nil {
			return err
		}
	}
	if err := writeCargoLipoShim(binDir); err != nil {
		return err
	}
	return writeCargoShellShim(binDir)
}

// writeCargoLipoShim provides `lipo`. cargokit's build_pod.dart always runs
// `lipo -create <slices> -output <lib>.a`, even for a single architecture, and
// ripley only ever builds the arm64 device slice.
//
// That single-input case cannot be delegated to the system llvm-lipo, because
// rustc carries its own newer LLVM: rustc 1.97.1 writes archive members with
// LLVM 22 metadata and Ubuntu's llvm-lipo 20 refuses to read them with
//
//	llvm-lipo: error: Unknown attribute kind (103)
//	  (Producer: 'LLVM22.1.6-rust-1.97.1-stable' Reader: 'LLVM 20.1.2')
//
// rustup's llvm-tools component ships a matching llvm-ar but no llvm-lipo, so
// there is no version-matched tool to defer to. For one slice the operation is
// just a copy: a thin arm64 archive is exactly what `-force_load` wants, and
// the pod's OTHER_LDFLAGS force_loads it directly. Multi-slice invocations,
// which ripley never issues, still go to a real lipo when one exists.
func writeCargoLipoShim(binDir string) error {
	real := ""
	for _, candidate := range append(llvmToolCandidates("llvm-lipo"), "lipo") {
		if found, err := exec.LookPath(candidate); err == nil {
			real = found
			break
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
# Generated by ripley. Linux stand-in for Darwin's lipo.
# A single-slice `+"`"+`-create`+"`"+` is a copy, which avoids the LLVM version skew
# between rustc's bundled LLVM and the system llvm-lipo.
real=%q
create=0
out=
inputs=0
single=
pending=
for arg in "$@"; do
	if [ "$pending" = "-output" ]; then
		out=$arg
		pending=
		continue
	fi
	case "$arg" in
		-create) create=1 ;;
		-output) pending=-output ;;
		-*) create=2 ;;
		*) inputs=$((inputs + 1)); single=$arg ;;
	esac
done
if [ "$create" = 1 ] && [ "$inputs" = 1 ] && [ -n "$out" ]; then
	exec cp -f "$single" "$out"
fi
if [ -z "$real" ]; then
	echo "ripley: lipo $* needs a real lipo/llvm-lipo, which is not installed on this host" >&2
	exit 1
fi
exec "$real" "$@"
`, real)
	_, err := writeCargoShim(binDir, "lipo", script)
	return err
}

// writeCargoShellShim puts a `sh` that is really bash on PATH. cargokit's
// build_pod.sh declares `#!/bin/sh` and is invoked as `sh .../build_pod.sh`,
// but its body uses bash arrays and `[[ ]]`:
//
//	FLUTTER_EXPORT_BUILD_ENVIRONMENT=( … )
//	if [[ -f "$path" ]]; then
//
// On macOS that is harmless because /bin/sh is bash. On Linux /bin/sh is dash
// and the phase dies with `Syntax error: "(" unexpected`, so the phase's `sh`
// has to resolve to bash for the script to run at all.
func writeCargoShellShim(binDir string) error {
	bash, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("cargokit's build_pod.sh needs bash (it declares #!/bin/sh but uses bash arrays): %w", err)
	}
	script := fmt.Sprintf("#!/bin/sh\n# Generated by ripley. cargokit's build_pod.sh is #!/bin/sh but needs bash.\nexec %q \"$@\"\n", bash)
	_, err = writeCargoShim(binDir, "sh", script)
	return err
}

// writeCargoShim writes an executable script, leaving a byte-identical existing
// file untouched so that repeated builds do not change its mtime.
func writeCargoShim(binDir, name, script string) (string, error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(binDir, name)
	if existing, err := os.ReadFile(path); err == nil && string(existing) == script {
		return path, nil
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// PodNeedsCargo reports whether a pod compiles Rust and therefore needs the
// cargo environment. Both real-world layouts put the pod's podspec in an `ios`
// directory with cargokit and the crate one level up:
//
//	rust_builder/ios/rust_lib_localsend_app.podspec   flutter_rust_bridge
//	rust_builder/cargokit/build_pod.sh
//	../../rust/Cargo.toml
//
//	super_native_extensions/ios/…podspec              native_toolchain_rust
//	super_native_extensions/cargokit/
//	super_native_extensions/rust/Cargo.toml
//
// so the pod directory and its parent are both examined. A podspec whose text
// mentions cargokit counts too, which is how the localsend pod declares its
// 'Build Rust library' script phase.
func PodNeedsCargo(podspecDir string) (bool, error) {
	if podspecDir == "" {
		return false, nil
	}
	for _, dir := range []string{podspecDir, filepath.Dir(podspecDir)} {
		if dirExists(filepath.Join(dir, "cargokit")) {
			return true, nil
		}
		if fileExists(filepath.Join(dir, "rust", "Cargo.toml")) {
			return true, nil
		}
	}
	podspecs, err := filepath.Glob(filepath.Join(podspecDir, "*.podspec"))
	if err != nil {
		return false, err
	}
	for _, podspec := range podspecs {
		data, err := os.ReadFile(podspec)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		if bytes.Contains(data, []byte("cargokit")) {
			return true, nil
		}
	}
	return false, nil
}
