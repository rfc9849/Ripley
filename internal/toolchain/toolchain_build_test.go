package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchDartTree(t *testing.T) {
	sdk := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sdk, "runtime", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeBuild := `config("dart_linux_riscv64_config") {
  defines = [ "TARGET_ARCH_RISCV64" ]
}
`
	configs := `_all_configs = [
  {
    suffix = "_precompiler_product_linux_riscv64"
    configs = _precompiler_product_linux_riscv64_config
    snapshot = false
    compiler = true
    is_product = true
  },
]
`
	binBuild := `build_libdart_builtin("libdart_builtin_product_linux_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_linux_arm64_config",
  ]
}
build_gen_snapshot("gen_snapshot_product_linux_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_linux_arm64_config",
  ]
  extra_deps = [
    ":gen_snapshot_dart_io_product_linux_arm64",
    ":libdart_builtin_product_linux_arm64",
    "..:libdart_precompiler_product_linux_arm64",
    "../platform:libdart_platform_precompiler_product_linux_arm64",
  ]
}
build_gen_snapshot_dart_io("gen_snapshot_dart_io_product_linux_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_linux_arm64_config",
  ]
}
`
	files := map[string]string{
		filepath.Join(sdk, "runtime", "BUILD.gn"):        runtimeBuild,
		filepath.Join(sdk, "runtime", "configs.gni"):     configs,
		filepath.Join(sdk, "runtime", "bin", "BUILD.gn"): binBuild,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := patchDartTree(sdk); err != nil {
		t.Fatal(err)
	}
	if err := patchDartTree(sdk); err != nil {
		t.Fatalf("patch must be idempotent: %v", err)
	}
	checks := map[string]string{
		filepath.Join(sdk, "runtime", "BUILD.gn"):        "dart_ios_arm64_config",
		filepath.Join(sdk, "runtime", "configs.gni"):     "_precompiler_product_ios_arm64_config",
		filepath.Join(sdk, "runtime", "bin", "BUILD.gn"): "gen_snapshot_product_ios_arm64",
	}
	for path, needle := range checks {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), needle) == 0 {
			t.Fatalf("%s was not added to %s", needle, path)
		}
	}
}

func TestGNHostCPU(t *testing.T) {
	for _, tc := range []struct {
		arch string
		want string
	}{
		{arch: "x64", want: "x64"},
		{arch: "arm64", want: "arm64"},
	} {
		got, err := gnHostCPU(tc.arch)
		if err != nil {
			t.Fatalf("gnHostCPU(%q): %v", tc.arch, err)
		}
		if got != tc.want {
			t.Fatalf("gnHostCPU(%q) = %q, want %q", tc.arch, got, tc.want)
		}
	}
	if _, err := gnHostCPU("riscv64"); err == nil || !strings.Contains(err.Error(), "x86_64 and arm64") {
		t.Fatalf("unsupported architecture error = %v", err)
	}
}

func TestGenSnapshotBuildOutputPrefersStrippedBinary(t *testing.T) {
	out := t.TempDir()
	unstripped := filepath.Join(out, "gen_snapshot_product_ios_arm64")
	if err := os.WriteFile(unstripped, []byte("debug"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := genSnapshotBuildOutput(out); got != unstripped {
		t.Fatalf("output without stripped binary = %q, want %q", got, unstripped)
	}
	stripped := filepath.Join(out, "exe.stripped", "gen_snapshot_product_ios_arm64")
	if err := os.MkdirAll(filepath.Dir(stripped), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stripped, []byte("release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := genSnapshotBuildOutput(out); got != stripped {
		t.Fatalf("output with stripped binary = %q, want %q", got, stripped)
	}
}

func TestGclientConfigPinsRequestedRevision(t *testing.T) {
	got := gclientConfig("deadbeef")
	for _, want := range []string{dartGit + "@deadbeef", `target_os=["linux"]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("gclient config missing %q:\n%s", want, got)
		}
	}
}
