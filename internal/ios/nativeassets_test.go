package ios

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAssetCompilerShimIsRecognizableAsClang(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTool := func(name string) string {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tools := map[string]string{
		"ar": fakeTool("llvm-ar"), "nm": fakeTool("llvm-nm"), "ranlib": fakeTool("llvm-ranlib"),
		"strip": fakeTool("llvm-strip"), "objdump": fakeTool("llvm-objdump"),
	}
	compiler, err := writeNativeAssetToolShims(bin, fakeTool("host-clang"), filepath.Join(tmp, "sdk"), filepath.Join(tmp, "sdk"), fakeTool("host-ld"), "15.0", tools, fakeTool("swiftc"), filepath.Join(tmp, "swift-resource-ios"), filepath.Join(tmp, "swift-ios-libs"), "", "26.5")
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(compiler); got != "clang" {
		t.Fatalf("compiler shim basename = %q; native_toolchain_c only recognizes an explicit Clang path ending in clang", got)
	}
}

func TestNativeAssetShimsProvideSyntheticXcodeSDK(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(tmp, "real-sdk")
	if err := os.MkdirAll(sdk, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeTool := func(name string) string {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tools := map[string]string{
		"ar": fakeTool("llvm-ar"), "nm": fakeTool("llvm-nm"), "ranlib": fakeTool("llvm-ranlib"),
		"strip": fakeTool("llvm-strip"), "objdump": fakeTool("llvm-objdump"),
	}
	if _, err := writeNativeAssetToolShims(bin, fakeTool("host-clang"), sdk, sdk, fakeTool("host-ld"), "15.0", tools, fakeTool("swiftc"), filepath.Join(tmp, "swift-resource-ios"), filepath.Join(tmp, "swift-ios-libs"), "", "26.5"); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(bin, "xcode-select"), "-p").Output()
	if err != nil {
		t.Fatal(err)
	}
	developer := strings.TrimSpace(string(out))
	wantLink := filepath.Join(developer, "Platforms", "iPhoneOS.platform", "Developer", "SDKs", "iPhoneOS.sdk")
	resolved, err := filepath.EvalSymlinks(wantLink)
	if err != nil {
		t.Fatal(err)
	}
	wantSDK, err := filepath.EvalSymlinks(sdk)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != wantSDK {
		t.Fatalf("synthetic iPhoneOS.sdk resolves to %q, want %q", resolved, wantSDK)
	}
	platformBin := filepath.Join(developer, "Platforms", "iPhoneOS.platform", "Developer", "usr", "bin")
	for _, name := range []string{"ar", "nm", "ranlib", "strip", "objdump"} {
		if _, err := os.Stat(filepath.Join(platformBin, name)); err != nil {
			t.Fatalf("synthetic Darwin tool %s: %v", name, err)
		}
	}
}

func TestNativeAssetEnvironmentCachesDarwinArgMax(t *testing.T) {
	env := nativeAssetEnvironment(t.TempDir(), "15", "/tmp/clang", "/tmp/llvm-ar")
	found := false
	for _, item := range env {
		if item == "lt_cv_sys_max_cmd_len=49152" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("native asset environment does not bypass Darwin kern.argmax probe")
	}
}

func TestNativeAssetXcrunBuildsSwiftDynamicLibraryWithDarwinLinker(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(tmp, "sdk")
	if err := os.MkdirAll(filepath.Join(sdk, "System", "Library", "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sdk, "usr", "lib", "swift"), 0o755); err != nil {
		t.Fatal(err)
	}
	resourceDir := filepath.Join(tmp, "swift-resource-ios")
	swiftLibs := filepath.Join(tmp, "swift-ios-libs")
	prebuiltModules := filepath.Join(swiftLibs, "prebuilt-modules", "26.5")
	for _, dir := range []string{resourceDir, swiftLibs, prebuiltModules, filepath.Join(resourceDir, "shims")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shimMap := filepath.Join(resourceDir, "shims", "module.modulemap")
	if err := os.WriteFile(shimMap, []byte("module SwiftShims {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log := filepath.Join(tmp, "tool.log")
	fakeOutputTool := func(name string) string {
		path := filepath.Join(tmp, name)
		script := `#!/bin/bash
set -e
printf '%s' '` + name + `' >> ` + shellQuote(log) + `
for arg in "$@"; do printf ' <%s>' "$arg" >> ` + shellQuote(log) + `; done
printf '\n' >> ` + shellQuote(log) + `
out=''
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
  if [[ "${args[$i]}" == '-o' ]]; then ((i+=1)); out="${args[$i]}"; fi
done
if [[ -n "$out" ]]; then mkdir -p "$(dirname "$out")"; : > "$out"; fi
`
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fakeTool := func(name string) string {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	lld := fakeOutputTool("ld64.lld-real")
	swiftc := fakeOutputTool("swiftc-real")
	tools := map[string]string{
		"ar": fakeTool("llvm-ar"), "nm": fakeTool("llvm-nm"), "ranlib": fakeTool("llvm-ranlib"),
		"strip": fakeTool("llvm-strip"), "objdump": fakeTool("llvm-objdump"),
	}
	if _, err := writeNativeAssetToolShims(bin, fakeTool("host-clang"), sdk, sdk, lld, "15.0", tools, swiftc, resourceDir, swiftLibs, prebuiltModules, "26.5"); err != nil {
		t.Fatal(err)
	}

	xcrun := filepath.Join(bin, "xcrun")
	found, err := exec.Command(xcrun, "--find", "swiftc").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(found)); got != filepath.Join(bin, "swiftc") {
		t.Fatalf("xcrun --find swiftc = %q, want generated shim", got)
	}

	out := filepath.Join(tmp, "libnative.dylib")
	cmd := exec.Command(xcrun,
		"--sdk", "iphoneos", "swiftc",
		"-emit-library", "-O", "-sdk", sdk, "-target", "arm64-apple-ios15",
		filepath.Join(tmp, "a.swift"), "-o", out,
		"-Xlinker", "-install_name", "-Xlinker", "@rpath/libnative.dylib",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("xcrun swiftc: %v\n%s", err, output)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("linked dylib missing: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"swiftc-real", "<-resource-dir> <" + resourceDir + ">", "<-I> <" + prebuiltModules + ">", "<-Xcc> <-I> <-Xcc> <" + filepath.Join(resourceDir, "shims") + ">", "<-Xcc> <-fmodule-map-file=" + shimMap + ">", "<-emit-object>", "<-whole-module-optimization>",
		"ld64.lld-real", "<-platform_version> <ios> <15.0> <26.5>", "<-arch> <arm64>",
		"<-install_name> <@rpath/libnative.dylib>", "<-L> <" + swiftLibs + ">",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("tool log missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(strings.Split(text, "\n")[0], "<-emit-library>") {
		t.Fatalf("swift compile still received -emit-library:\n%s", text)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
