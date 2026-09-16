package ios

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ripley/internal/toolchain"
)

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// isolateCargoLookup removes every ambient cargo/rustup from the test's view so
// that discovery outcomes are the ones the test set up. A real bash is linked
// into the isolated PATH because cargokit's build_pod.sh genuinely requires one.
func isolateCargoLookup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		if err := os.Symlink(bash, filepath.Join(bin, "bash")); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", dir)
	t.Setenv("PATH", bin)
	t.Setenv("RIPLEY_CARGO", "")
	return bin
}

func TestCargoToolchainWithoutCargoExplainsHowToInstallRust(t *testing.T) {
	isolateCargoLookup(t)

	_, err := CargoToolchain(toolchain.Toolchain{Root: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error when no cargo is installed")
	}
	message := err.Error()
	for _, want := range []string{"rustup.rs", "rustup target add " + cargoIOSTarget, "RIPLEY_CARGO"} {
		if !strings.Contains(message, want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, message)
		}
	}
}

func TestCargoToolchainRejectsNonExecutableFLCargo(t *testing.T) {
	isolateCargoLookup(t)
	fake := filepath.Join(t.TempDir(), "cargo")
	if err := os.WriteFile(fake, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPLEY_CARGO", fake)

	_, err := CargoToolchain(toolchain.Toolchain{Root: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error for a non-executable RIPLEY_CARGO")
	}
	if !strings.Contains(err.Error(), fake) {
		t.Errorf("diagnostic does not name the rejected path:\n%s", err)
	}
}

func TestCargoToolchainWithoutIOSTargetNamesTheToolchainPinnedInstall(t *testing.T) {
	bin := isolateCargoLookup(t)
	cargo := filepath.Join(bin, "cargo")
	rustup := filepath.Join(bin, "rustup")
	writeExecutable(t, cargo, "#!/bin/sh\nexit 0\n")
	writeExecutable(t, rustup, "#!/bin/sh\necho x86_64-unknown-linux-gnu\n")
	t.Setenv("RIPLEY_CARGO", cargo)

	_, err := CargoToolchain(toolchain.Toolchain{Root: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error when the iOS target is not installed")
	}
	message := err.Error()
	if !strings.Contains(message, cargoIOSTarget) {
		t.Errorf("diagnostic does not name the missing target:\n%s", message)
	}
	if !strings.Contains(message, "x86_64-unknown-linux-gnu") {
		t.Errorf("diagnostic does not report which targets are installed:\n%s", message)
	}
	// Installing the target only for the default toolchain is the trap that
	// breaks crates pinned by rust-toolchain.toml, so the diagnostic must
	// point at --toolchain as well.
	if !strings.Contains(message, "--toolchain") {
		t.Errorf("diagnostic does not mention the pinned-channel install:\n%s", message)
	}
	if !strings.Contains(message, rustup+" target add") {
		t.Errorf("diagnostic does not give a runnable rustup command:\n%s", message)
	}
}

func TestCargoToolchainAcceptsRustupReportingTheIOSTarget(t *testing.T) {
	bin := isolateCargoLookup(t)
	cargo := filepath.Join(bin, "cargo")
	writeExecutable(t, cargo, "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(bin, "rustup"), "#!/bin/sh\necho x86_64-unknown-linux-gnu\necho "+cargoIOSTarget+"\n")
	writeExecutable(t, filepath.Join(bin, "clang"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(bin, "llvm-ar"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(bin, "llvm-lipo"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(bin, "llvm-install-name-tool"), "#!/bin/sh\nexit 0\n")
	t.Setenv("RIPLEY_CARGO", cargo)

	root := t.TempDir()
	sdk := filepath.Join(root, "iPhoneOS26.5.sdk")
	if err := os.MkdirAll(sdk, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPLEY_IOS_SDK", sdk)
	tc := toolchain.Toolchain{Root: root}
	writeExecutable(t, filepath.Join(tc.ToolsetBin(), "ld64.lld"), "#!/bin/sh\nexit 0\n")

	env, err := CargoToolchain(tc)
	if err != nil {
		t.Fatalf("CargoToolchain: %v", err)
	}
	if env.SDK != sdk {
		t.Errorf("SDK = %q, want %q", env.SDK, sdk)
	}
	if !isExecutableFile(env.LinkerWrapper) {
		t.Errorf("linker wrapper %q is not executable", env.LinkerWrapper)
	}

	// Cargo only honours the underscored upper-case triple for the linker
	// override and the lower-case triple for CC/AR; a consumer breaks
	// silently if these names are wrong.
	vars := make(map[string]string)
	for _, entry := range env.Env() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("environment entry is not KEY=value: %q", entry)
		}
		vars[key] = value
	}
	if vars["CARGO_TARGET_AARCH64_APPLE_IOS_LINKER"] != env.LinkerWrapper {
		t.Errorf("CARGO_TARGET_AARCH64_APPLE_IOS_LINKER = %q, want the wrapper %q",
			vars["CARGO_TARGET_AARCH64_APPLE_IOS_LINKER"], env.LinkerWrapper)
	}
	if vars["CC_aarch64_apple_ios"] != env.LinkerWrapper {
		t.Errorf("CC_aarch64_apple_ios = %q, want the wrapper %q", vars["CC_aarch64_apple_ios"], env.LinkerWrapper)
	}
	if vars["AR_aarch64_apple_ios"] != env.AR {
		t.Errorf("AR_aarch64_apple_ios = %q, want %q", vars["AR_aarch64_apple_ios"], env.AR)
	}
	if vars["SDKROOT"] != sdk {
		t.Errorf("SDKROOT = %q, want %q", vars["SDKROOT"], sdk)
	}

	// cargokit's build_pod.dart runs bare `lipo`/`install_name_tool`, and
	// build_tool shells `rustup run <channel> cargo`, so all of those have to
	// resolve through the injected PATH.
	pathDirs := filepath.SplitList(vars["PATH"])
	for _, tool := range []string{"lipo", "install_name_tool", "cargo", "rustup", cargoLinkerWrapperName} {
		if !resolvesInPath(pathDirs, tool) {
			t.Errorf("%s does not resolve through the injected PATH %q", tool, vars["PATH"])
		}
	}
}

func resolvesInPath(dirs []string, name string) bool {
	for _, dir := range dirs {
		if isExecutableFile(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

func TestCargoLinkerWrapperForcesPlatformVersionAndTheIOSSDK(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")

	path, err := writeCargoLinkerWrapper(bin, "/usr/bin/clang", "/sdks/iPhoneOS26.5.sdk", "/toolset/ld64.lld", "12.0")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{
		"-target arm64-apple-ios12.0",
		`-isysroot "/sdks/iPhoneOS26.5.sdk"`,
		`-fuse-ld="/toolset/ld64.lld"`,
		`"/usr/bin/clang"`,
		`"$@"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("wrapper is missing %q:\n%s", want, script)
		}
	}
	// Without -mlinker-version clang emits the legacy -iphoneos_version_min
	// flag and ld64.lld refuses the link with "must specify -platform_version".
	if !strings.Contains(script, "-mlinker-version="+cargoMinimumLinkerVersion) {
		t.Errorf("wrapper does not force the modern -platform_version linker flag:\n%s", script)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("wrapper mode %v is not executable", info.Mode())
	}
}

// cargokit's build_pod.sh is invoked as `sh build_pod.sh` yet uses bash arrays,
// which only works on macOS because /bin/sh is bash there. On Linux the shim has
// to make `sh` bash or the phase dies with `Syntax error: "(" unexpected`.
func TestCargoShellShimRunsBashOnlySyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to exercise the shell shim")
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := writeCargoShellShim(bin); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(t.TempDir(), "build_pod.sh")
	body := "#!/bin/sh\nARR=( one two )\nif [[ -n \"${ARR[1]}\" ]]; then echo \"ok ${ARR[1]}\"; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(filepath.Join(bin, "sh"), script).CombinedOutput()
	if err != nil {
		t.Fatalf("shim failed to run bash-only syntax: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok two") {
		t.Errorf("shim did not evaluate bash array syntax, got: %q", out)
	}
}

// cargokit's build_pod.dart always runs `lipo -create <one slice> -output x.a`.
// Delegating that to Ubuntu's llvm-lipo fails on archives rustc built with its
// own newer LLVM, so the single-slice case must be served as a copy.
func TestCargoLipoShimCopiesASingleSlice(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is required to exercise the lipo shim")
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := writeCargoLipoShim(bin); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	input := filepath.Join(work, "librust.a")
	// Bytes that no LLVM tool can parse: if the shim shells out to a real
	// lipo the copy fails, which is exactly the regression being guarded.
	payload := []byte("!<arch>\nnot-parseable-by-llvm\n")
	if err := os.WriteFile(input, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(work, "out", "libfinal.a")
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(filepath.Join(bin, "lipo"), "-create", input, "-output", output).CombinedOutput()
	if err != nil {
		t.Fatalf("lipo shim failed: %v\n%s", err, out)
	}
	copied, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != string(payload) {
		t.Errorf("shim did not copy the slice byte for byte: %q", copied)
	}
}

func TestCargoLinkerWrapperRewriteLeavesUnchangedFileAlone(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	path, err := writeCargoLinkerWrapper(bin, "/usr/bin/clang", "/sdk", "/lld", "12.0")
	if err != nil {
		t.Fatal(err)
	}
	// Cargo keys rebuilds off the linker's mtime, so regenerating identical
	// content must not touch the file.
	stale := os.Chtimes(path, time.Unix(1000, 0), time.Unix(1000, 0))
	if stale != nil {
		t.Fatal(stale)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writeCargoLinkerWrapper(bin, "/usr/bin/clang", "/sdk", "/lld", "12.0"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("identical regeneration touched the wrapper: %v -> %v", before.ModTime(), after.ModTime())
	}

	if _, err := writeCargoLinkerWrapper(bin, "/usr/bin/clang", "/other-sdk", "/lld", "12.0"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/other-sdk") {
		t.Errorf("changed configuration did not rewrite the wrapper:\n%s", data)
	}
}

func TestPodNeedsCargoForFlutterRustBridgeLayout(t *testing.T) {
	// rust_builder/ios/<pod>.podspec with cargokit and the crate above it, as
	// localsend's rust_lib_localsend_app is laid out.
	pkg := t.TempDir()
	podspecDir := filepath.Join(pkg, "ios")
	if err := os.MkdirAll(filepath.Join(pkg, "cargokit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(podspecDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podspecDir, "rust_lib_app.podspec"), []byte("Pod::Spec.new do |s|\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	needs, err := PodNeedsCargo(podspecDir)
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Error("pod with a sibling cargokit directory should need cargo")
	}
}

func TestPodNeedsCargoForCrateBesidePodspec(t *testing.T) {
	pkg := t.TempDir()
	podspecDir := filepath.Join(pkg, "ios")
	if err := os.MkdirAll(podspecDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pkg, "rust"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "rust", "Cargo.toml"), []byte("[package]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	needs, err := PodNeedsCargo(podspecDir)
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Error("pod whose package ships rust/Cargo.toml should need cargo")
	}
}

func TestPodNeedsCargoFromPodspecScriptPhase(t *testing.T) {
	// The crate and cargokit can live outside the pod's package entirely; the
	// podspec's script phase is then the only local evidence.
	podspecDir := t.TempDir()
	script := `Pod::Spec.new do |s|
  s.script_phase = {
    :name => 'Build Rust library',
    :script => 'sh "$PODS_TARGET_SRCROOT/../cargokit/build_pod.sh" ../../rust rust_lib_app',
  }
end
`
	if err := os.WriteFile(filepath.Join(podspecDir, "rust_lib_app.podspec"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

	needs, err := PodNeedsCargo(podspecDir)
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Error("podspec whose script phase runs cargokit/build_pod.sh should need cargo")
	}
}

func TestPodNeedsCargoIsFalseForOrdinaryPod(t *testing.T) {
	pkg := t.TempDir()
	podspecDir := filepath.Join(pkg, "ios")
	if err := os.MkdirAll(filepath.Join(podspecDir, "Classes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podspecDir, "path_provider.podspec"), []byte("Pod::Spec.new do |s|\n  s.source_files = 'Classes/**/*'\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	needs, err := PodNeedsCargo(podspecDir)
	if err != nil {
		t.Fatal(err)
	}
	if needs {
		t.Error("ordinary Objective-C pod should not need cargo")
	}

	missing, err := PodNeedsCargo(filepath.Join(pkg, "nonexistent"))
	if err != nil {
		t.Fatalf("absent directory should not be an error: %v", err)
	}
	if missing {
		t.Error("absent pod directory should not need cargo")
	}
}
