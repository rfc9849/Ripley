// Portions of the GN build configuration snippets in patchDartTree are
// adapted from Dart SDK build files. Copyright 2012, the Dart project authors.
// Those portions remain under Dart's BSD-3-Clause terms; see THIRD_PARTY_NOTICES.
package toolchain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	dartGit    = "https://dart.googlesource.com/sdk.git"
	depotTools = "https://chromium.googlesource.com/chromium/tools/depot_tools.git"
)

const iosConfigSnippet = `
config("dart_ios_arm64_config") {
  defines = [
    "DART_TARGET_OS_MACOS",
    "DART_TARGET_OS_MACOS_IOS",
    "TARGET_ARCH_ARM64",
  ]
}
`

func patchDartTree(sdk string) error {
	runtimeBuild := filepath.Join(sdk, "runtime", "BUILD.gn")
	data, err := os.ReadFile(runtimeBuild)
	if err != nil {
		return err
	}
	text := string(data)
	if !strings.Contains(text, "dart_ios_arm64_config") {
		anchor := `config("dart_linux_riscv64_config")`
		start := strings.Index(text, anchor)
		if start < 0 {
			return fmt.Errorf("%s: dart_linux_riscv64_config anchor not found", runtimeBuild)
		}
		targetArch := strings.Index(text[start:], "TARGET_ARCH_RISCV64")
		if targetArch < 0 {
			return fmt.Errorf("%s: TARGET_ARCH_RISCV64 anchor not found", runtimeBuild)
		}
		endRel := strings.Index(text[start+targetArch:], "}\n")
		if endRel < 0 {
			return fmt.Errorf("%s: end of riscv64 config not found", runtimeBuild)
		}
		end := start + targetArch + endRel + 2
		text = text[:end] + iosConfigSnippet + text[end:]
		if err := os.WriteFile(runtimeBuild, []byte(text), 0o644); err != nil {
			return err
		}
	}

	configs := filepath.Join(sdk, "runtime", "configs.gni")
	data, err = os.ReadFile(configs)
	if err != nil {
		return err
	}
	text = string(data)
	if !strings.Contains(text, "_precompiler_product_ios_arm64_config") {
		config := `_precompiler_product_ios_arm64_config =
    [
      "//runtime:dart_config",
      "//runtime:dart_ios_arm64_config",
    ] + _product + _precompiler_base

_all_configs = [`
		if !strings.Contains(text, "_all_configs = [") {
			return fmt.Errorf("%s: _all_configs anchor not found", configs)
		}
		text = strings.Replace(text, "_all_configs = [", config, 1)
		anchor := `    configs = _precompiler_product_linux_riscv64_config
    snapshot = false
    compiler = true
    is_product = true
  },`
		entry := anchor + `
  {
    suffix = "_precompiler_product_ios_arm64"
    configs = _precompiler_product_ios_arm64_config
    snapshot = false
    compiler = true
    is_product = true
  },`
		if !strings.Contains(text, anchor) {
			return fmt.Errorf("%s: linux riscv64 precompiler entry not found", configs)
		}
		text = strings.Replace(text, anchor, entry, 1)
		if err := os.WriteFile(configs, []byte(text), 0o644); err != nil {
			return err
		}
	}

	binBuild := filepath.Join(sdk, "runtime", "bin", "BUILD.gn")
	data, err = os.ReadFile(binBuild)
	if err != nil {
		return err
	}
	text = string(data)
	if !strings.Contains(text, "gen_snapshot_product_ios_arm64") {
		builtin := `build_libdart_builtin("libdart_builtin_product_linux_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_linux_arm64_config",
  ]
}
`
		builtinIOS := builtin + `
build_libdart_builtin("libdart_builtin_product_ios_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_ios_arm64_config",
  ]
}
`
		if !strings.Contains(text, builtin) {
			return fmt.Errorf("%s: linux arm64 builtin target not found", binBuild)
		}
		text = strings.Replace(text, builtin, builtinIOS, 1)

		snapshot := `build_gen_snapshot("gen_snapshot_product_linux_arm64") {
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
`
		snapshotIOS := snapshot + `
build_gen_snapshot("gen_snapshot_product_ios_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_ios_arm64_config",
  ]
  extra_deps = [
    ":gen_snapshot_dart_io_product_ios_arm64",
    ":libdart_builtin_product_ios_arm64",
    "..:libdart_precompiler_product_ios_arm64",
    "../platform:libdart_platform_precompiler_product_ios_arm64",
  ]
}
`
		if !strings.Contains(text, snapshot) {
			return fmt.Errorf("%s: linux arm64 gen_snapshot target not found", binBuild)
		}
		text = strings.Replace(text, snapshot, snapshotIOS, 1)

		dartIO := `build_gen_snapshot_dart_io("gen_snapshot_dart_io_product_linux_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_linux_arm64_config",
  ]
}
`
		dartIOIOS := dartIO + `
build_gen_snapshot_dart_io("gen_snapshot_dart_io_product_ios_arm64") {
  extra_configs = [
    "..:dart_product_config",
    "..:dart_ios_arm64_config",
  ]
}
`
		if !strings.Contains(text, dartIO) {
			return fmt.Errorf("%s: linux arm64 dart_io target not found", binBuild)
		}
		text = strings.Replace(text, dartIO, dartIOIOS, 1)
		if err := os.WriteFile(binBuild, []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func gnHostCPU(arch string) (string, error) {
	switch arch {
	case "x64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported Linux host architecture %q; ripley supports x86_64 and arm64", arch)
	}
}

func gclientConfig(dartRevision string) string {
	return fmt.Sprintf("solutions = [{\"name\":\"sdk\",\"url\":\"%s@%s\",\"managed\":False,\"custom_deps\":{},\"custom_vars\":{}}]\ntarget_os=[\"linux\"]\n", dartGit, dartRevision)
}

func BuildGenSnapshot(dartRevision, workdir string) (string, error) {
	if workdir == "" {
		workdir = filepath.Join(RipleyHome(), "dartbuild")
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", err
	}
	depot := filepath.Join(workdir, "depot_tools")
	if !dirExists(depot) {
		if err := run("", nil, "git", "clone", "--depth", "1", depotTools, depot); err != nil {
			return "", err
		}
	}
	env := append(os.Environ(), "PATH="+depot+string(os.PathListSeparator)+os.Getenv("PATH"))

	gclientDir := filepath.Join(workdir, "dartsdk")
	if err := os.MkdirAll(gclientDir, 0o755); err != nil {
		return "", err
	}
	sdk := filepath.Join(gclientDir, "sdk")
	gclientFile := filepath.Join(gclientDir, ".gclient")
	content := gclientConfig(dartRevision)
	if current, err := os.ReadFile(gclientFile); err != nil || string(current) != content {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := os.WriteFile(gclientFile, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	gclient := filepath.Join(depot, "gclient")
	if err := run(gclientDir, env, gclient, "sync", "--no-history", "--shallow", "--nohooks", "-j4"); err != nil {
		return "", err
	}
	if err := patchDartTree(sdk); err != nil {
		return "", err
	}

	gn := filepath.Join(sdk, "buildtools", "gn")
	ninja := filepath.Join(sdk, "buildtools", "ninja", "ninja")
	out := filepath.Join(sdk, "out", "RelIOS")
	cpu, err := gnHostCPU(hostArch())
	if err != nil {
		return "", err
	}
	gnArgs := fmt.Sprintf(`target_os="linux" target_cpu=%q host_cpu=%q is_debug=false is_release=true dart_runtime_mode="release" is_clang=true dart_snapshot_kind="kernel" use_custom_libcxx=false`, cpu, cpu)
	if err := run(sdk, nil, gn, "gen", out, "--args="+gnArgs); err != nil {
		return "", err
	}
	jobs := runtime.NumCPU()
	if jobs < 1 {
		jobs = 4
	}
	if err := run(sdk, nil, ninja, "-C", out, "-j", strconv.Itoa(jobs), "gen_snapshot_product_ios_arm64"); err != nil {
		return "", err
	}
	return genSnapshotBuildOutput(out), nil
}

func genSnapshotBuildOutput(out string) string {
	stripped := filepath.Join(out, "exe.stripped", "gen_snapshot_product_ios_arm64")
	if fileExists(stripped) {
		return stripped
	}
	return filepath.Join(out, "gen_snapshot_product_ios_arm64")
}
