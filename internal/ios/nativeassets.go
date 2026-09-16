package ios

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ripley/internal/toolchain"

	"gopkg.in/yaml.v3"
)

// NativeAssetBuild is the result of Dart package build/link hooks for the
// arm64 iOS application. Frameworks are already normalized as iOS dynamic
// frameworks and can be copied directly into <App>.app/Frameworks. Manifest is
// the NativeAssetsManifest.json payload consumed by the Dart VM at runtime.
type NativeAssetBuild struct {
	Frameworks []string
	Manifest   []byte
	Packages   []string
}

type nativeAssetHelperResult struct {
	Packages []string            `json:"packages"`
	Assets   []nativeAssetRecord `json:"assets"`
}

type nativeAssetRecord struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	File string `json:"file,omitempty"`
	Path string `json:"path,omitempty"`
}

const dartNativeAssetsHelper = `import 'dart:convert';
import 'dart:io';

import 'package:code_assets/code_assets.dart';
import 'package:file/local.dart';
import 'package:hooks_runner/hooks_runner.dart';
import 'package:logging/logging.dart';
import 'package:package_config/package_config.dart';

Future<void> main(List<String> args) async {
  if (args.length != 8) {
    stderr.writeln('usage: native_assets.dart <project> <package-config> <app-name> <sdk> <cc> <ar> <ld> <output>');
    exit(64);
  }
  final project = Directory(args[0]).absolute;
  final packageConfigUri = File(args[1]).absolute.uri;
  final appName = args[2];
  final sdk = args[3];
  final cc = args[4];
  final ar = args[5];
  final ld = args[6];
  final output = File(args[7]);

  final packageConfig = await loadPackageConfigUri(packageConfigUri);
  final fs = LocalFileSystem();
  final layout = PackageLayout.fromPackageConfig(
    fs,
    packageConfig,
    packageConfigUri,
    appName,
    includeDevDependencies: false,
  );

  Logger.root.level = Level.INFO;
  Logger.root.onRecord.listen((record) {
    stderr.writeln('${record.level.name}: ${record.message}');
  });

  final runner = NativeAssetsBuildRunner(
    logger: Logger('ripley.native_assets'),
    dartExecutable: File(Platform.resolvedExecutable).uri,
    fileSystem: fs,
    packageLayout: layout,
    hookEnvironment: Platform.environment,
    userDefines: UserDefines(workspacePubspec: project.uri.resolve('pubspec.yaml')),
  );
  final target = CodeAssetExtension(
    targetArchitecture: Architecture.arm64,
    targetOS: OS.iOS,
    linkModePreference: LinkModePreference.dynamic,
    cCompiler: CCompilerConfig(
      compiler: File(cc).uri,
      archiver: File(ar).uri,
      linker: File(ld).uri,
    ),
    iOS: IOSCodeConfig(targetSdk: IOSSdk.iPhoneOS, targetVersion: int.parse(Platform.environment['RIPLEY_NATIVE_IOS_MAJOR']!)),
  );

  final packages = await runner.packagesWithBuildHooks();
  if (packages.isEmpty) {
    output.parent.createSync(recursive: true);
    output.writeAsStringSync(jsonEncode({'packages': <String>[], 'assets': <Object>[]}));
    return;
  }

  final build = await runner.build(extensions: [target], linkingEnabled: true);
  if (!build.isSuccess) {
    stderr.writeln('Dart native asset build hooks failed');
    exit(2);
  }
  final buildResult = build.success;
  final link = await runner.link(extensions: [target], buildResult: buildResult);
  if (!link.isSuccess) {
    stderr.writeln('Dart native asset link hooks failed');
    exit(3);
  }

  final encodedAssets = [...buildResult.encodedAssets, ...link.success.encodedAssets];
  final assets = <Map<String, Object?>>[];
  for (final encoded in encodedAssets) {
    if (!encoded.isCodeAsset) continue;
    final asset = encoded.asCodeAsset;
    final mode = asset.linkMode;
    if (mode is DynamicLoadingBundled) {
      final file = asset.file;
      if (file == null) {
        throw StateError('bundled code asset ${asset.id} has no file');
      }
      assets.add({'id': asset.id, 'kind': 'bundled', 'file': file.toFilePath()});
    } else if (mode is DynamicLoadingSystem) {
      assets.add({'id': asset.id, 'kind': 'system', 'path': mode.uri.toFilePath()});
    } else if (mode is LookupInProcess) {
      assets.add({'id': asset.id, 'kind': 'process'});
    } else if (mode is LookupInExecutable) {
      assets.add({'id': asset.id, 'kind': 'executable'});
    } else {
      throw UnsupportedError('unsupported iOS native asset link mode $mode for ${asset.id}');
    }
  }
  output.parent.createSync(recursive: true);
  output.writeAsStringSync(jsonEncode({'packages': packages, 'assets': assets}));
}
`

// BuildNativeAssets executes Dart package build/link hooks using Flutter's own
// hooks_runner protocol while supplying the Linux-hosted Darwin toolchain. The
// resulting dylibs are converted to the framework layout Flutter uses on iOS.
func BuildNativeAssets(tc toolchain.Toolchain, projectRoot, flutterRoot, packageConfig, buildDir, minOS string) (NativeAssetBuild, error) {
	name, err := dartPackageName(filepath.Join(projectRoot, "pubspec.yaml"))
	if err != nil {
		return NativeAssetBuild{}, err
	}
	flutterToolsPackages := filepath.Join(flutterRoot, "packages", "flutter_tools", ".dart_tool", "package_config.json")
	if !fileExists(flutterToolsPackages) {
		return NativeAssetBuild{}, fmt.Errorf("Flutter tool package config not found at %s; run flutter once so its tool dependencies are available", flutterToolsPackages)
	}
	dart := filepath.Join(flutterRoot, "bin", "cache", "dart-sdk", "bin", "dart")
	if !isExecutableFile(dart) {
		return NativeAssetBuild{}, fmt.Errorf("Flutter Dart executable not found at %s", dart)
	}
	sdk, err := tc.IOSSDK()
	if err != nil {
		return NativeAssetBuild{}, err
	}
	clang, err := hostClang(tc)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	ar, err := resolveCargoArchiver()
	if err != nil {
		return NativeAssetBuild{}, err
	}
	lld, err := resolveCargoLinker(tc)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	linkerSDK, err := tc.LinkerIOSSDK()
	if err != nil {
		return NativeAssetBuild{}, err
	}
	darwinTools := map[string]string{"ar": ar}
	for name, llvmName := range map[string]string{
		"nm":      "llvm-nm",
		"ranlib":  "llvm-ranlib",
		"strip":   "llvm-strip",
		"objdump": "llvm-objdump",
	} {
		tool, err := findLLVMTool(llvmName, "")
		if err != nil {
			return NativeAssetBuild{}, fmt.Errorf("resolve Darwin native-asset %s: %w", name, err)
		}
		darwinTools[name] = tool
	}

	work := filepath.Join(buildDir, "native_assets")
	if err := os.RemoveAll(work); err != nil {
		return NativeAssetBuild{}, err
	}
	binDir := filepath.Join(work, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return NativeAssetBuild{}, err
	}
	swiftc, err := swiftCompiler(tc)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	swiftResourceDir, err := iosResourceDir(tc)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	swiftIOSLibs, err := tc.SwiftIOSLibs()
	if err != nil {
		return NativeAssetBuild{}, err
	}
	sdkVersion, err := tc.IOSSDKVersion()
	if err != nil {
		return NativeAssetBuild{}, err
	}
	swiftPrebuiltModules, err := swiftPrebuiltModulesDir(tc)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	compiler, err := writeNativeAssetToolShims(binDir, clang, sdk, linkerSDK, lld, minOS, darwinTools, swiftc, swiftResourceDir, swiftIOSLibs, swiftPrebuiltModules, sdkVersion)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	helper := filepath.Join(work, "native_assets.dart")
	if err := os.WriteFile(helper, []byte(dartNativeAssetsHelper), 0o644); err != nil {
		return NativeAssetBuild{}, err
	}
	resultPath := filepath.Join(work, "result.json")
	major := strings.Split(strings.TrimSpace(minOS), ".")[0]
	if _, err := strconv.Atoi(major); err != nil {
		return NativeAssetBuild{}, fmt.Errorf("invalid iOS deployment target %q", minOS)
	}
	env := nativeAssetEnvironment(binDir, major, compiler, ar)
	if err := run(projectRoot, env, dart,
		"--packages="+flutterToolsPackages,
		helper,
		projectRoot,
		packageConfig,
		name,
		sdk,
		compiler,
		ar,
		lld,
		resultPath,
	); err != nil {
		return NativeAssetBuild{}, fmt.Errorf("build Dart native assets: %w", err)
	}

	data, err := os.ReadFile(resultPath)
	if err != nil {
		return NativeAssetBuild{}, fmt.Errorf("read native asset hook result: %w", err)
	}
	var result nativeAssetHelperResult
	if err := json.Unmarshal(data, &result); err != nil {
		return NativeAssetBuild{}, fmt.Errorf("decode native asset hook result: %w", err)
	}
	build, err := packageNativeAssets(tc, result, work, minOS)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	build.Packages = append([]string{}, result.Packages...)
	sort.Strings(build.Packages)
	return build, nil
}

func dartPackageName(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var spec struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("%s has no package name", path)
	}
	return strings.TrimSpace(spec.Name), nil
}

func writeNativeAssetToolShims(binDir, clang, sdk, linkerSDK, lld, minOS string, darwinTools map[string]string, swiftc, swiftResourceDir, swiftIOSLibs, swiftPrebuiltModules, sdkVersion string) (string, error) {
	// Some Dart native-asset hooks (notably sodium) discover Apple's SDK via
	// `xcode-select -p` and then append the normal Xcode platform path. Provide
	// a tiny synthetic Xcode developer tree whose iPhoneOS.sdk points at ripley's
	// extracted SDK. No Apple executable is required or shipped.
	developerDir := filepath.Join(filepath.Dir(binDir), "Xcode", "Contents", "Developer")
	platformDeveloper := filepath.Join(developerDir, "Platforms", "iPhoneOS.platform", "Developer")
	platformBin := filepath.Join(platformDeveloper, "usr", "bin")
	for _, dir := range []string{
		filepath.Join(platformDeveloper, "SDKs"),
		platformBin,
		filepath.Join(platformDeveloper, "usr", "sbin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	sdkLink := filepath.Join(platformDeveloper, "SDKs", "iPhoneOS.sdk")
	_ = os.Remove(sdkLink)
	if err := os.Symlink(sdk, sdkLink); err != nil {
		return "", err
	}
	// Autoconf/libtool selects Darwin behavior from --host, so every binutil it
	// discovers must understand Mach-O. GNU nm/ranlib can corrupt or reject the
	// Mach-O archives produced by llvm-ar. Put LLVM implementations both on the
	// hook PATH and in the synthetic platform bin directory used by hooks such as
	// sodium.
	for name, target := range darwinTools {
		if strings.TrimSpace(target) == "" {
			continue
		}
		for _, dir := range []string{binDir, platformBin} {
			link := filepath.Join(dir, name)
			_ = os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				return "", err
			}
		}
	}
	xcodeSelect := filepath.Join(binDir, "xcode-select")
	xcodeSelectScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  -p|--print-path) echo %q; exit 0 ;;
esac
echo "ripley xcode-select shim: unsupported arguments: $*" >&2
exit 2
`, developerDir)
	if err := os.WriteFile(xcodeSelect, []byte(xcodeSelectScript), 0o755); err != nil {
		return "", err
	}

	lldShim := filepath.Join(binDir, "ld64.lld")
	if err := writeLD64SDKShim(lldShim, lld, sdk, linkerSDK); err != nil {
		return "", err
	}
	// native_toolchain_c recognizes an explicitly configured Clang compiler by
	// executable basename. Keep the wrapper named exactly "clang"; names such as
	// "clang-ios" are rejected by CompilerRecognizer even when they execute Clang.
	compiler := filepath.Join(binDir, "clang")
	compilerScript := fmt.Sprintf(`#!/bin/sh
# Generated by ripley for package build hooks targeting arm64 iOS.
exec %q -B%q -fuse-ld=lld -isysroot %q "$@" -target arm64-apple-ios%s
`, clang, binDir, sdk, minOS)
	if err := os.WriteFile(compiler, []byte(compilerScript), 0o755); err != nil {
		return "", err
	}
	// Swift-native Dart build hooks commonly invoke `xcrun --sdk iphoneos
	// swiftc -emit-library ...`. Linux swiftc can compile Darwin Mach-O objects,
	// but its sibling clang tries to link them with the host ELF linker. Keep
	// Swift's frontend semantics, then perform the final Darwin link explicitly
	// with ld64.lld. Swift Mach-O objects carry LC_LINKER_OPTION load commands,
	// so ld64.lld still consumes the framework and Swift-library autolinks that
	// swiftc would have passed through Apple's clang driver.
	swiftcShim := filepath.Join(binDir, "swiftc")
	swiftcScript := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
real_swiftc=%q
lld=%q
sdk=%q
resource_dir=%q
swift_ios_libs=%q
swift_prebuilt_modules=%q
min_os=%q
sdk_version=%q

emit_library=0
out=""
compile_args=()
link_args=()
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
  arg="${args[$i]}"
  case "$arg" in
    -emit-library) emit_library=1 ;;
    -o)
      ((i+=1))
      out="${args[$i]}"
      ;;
    -Xlinker)
      ((i+=1))
      link_args+=("${args[$i]}")
      ;;
    -resource-dir)
      ((i+=1))
      ;;
    *) compile_args+=("$arg") ;;
  esac
done

module_args=()
if [[ -n "$swift_prebuilt_modules" && -d "$swift_prebuilt_modules" ]]; then
  module_args=(-I "$swift_prebuilt_modules")
fi

shim_args=()
shim_map="$resource_dir/shims/module.modulemap"
if [[ -f "$shim_map" ]]; then
  shim_args=(-Xcc -I -Xcc "$resource_dir/shims")
  if [[ -d "$sdk/usr/include/c++/v1" ]]; then
    shim_args+=(-Xcc -I -Xcc "$sdk/usr/include/c++/v1")
  fi
  shim_args+=(-Xcc "-fmodule-map-file=$shim_map")
fi

if [[ $emit_library -eq 0 ]]; then
  exec "$real_swiftc" -resource-dir "$resource_dir" "${module_args[@]}" "${shim_args[@]}" "$@"
fi
if [[ -z "$out" ]]; then
  echo "ripley swiftc shim: -emit-library requires -o" >&2
  exit 2
fi
mkdir -p "$(dirname "$out")"
obj="${out}.ripley-swift.o"
rm -f "$obj"
"$real_swiftc" -resource-dir "$resource_dir" "${module_args[@]}" "${shim_args[@]}" -parse-as-library -emit-object -whole-module-optimization "${compile_args[@]}" -o "$obj"
"$lld" -arch arm64 -dylib -fixup_chains \
  -platform_version ios "$min_os" "$sdk_version" \
  -syslibroot "$sdk" -o "$out" "$obj" \
  -L "$sdk/usr/lib" -L "$sdk/usr/lib/swift" -L "$swift_ios_libs" \
  -F "$sdk/System/Library/Frameworks" -rpath /usr/lib/swift \
  "${link_args[@]}" -lSystem
rm -f "$obj"
`, swiftc, lldShim, sdk, swiftResourceDir, swiftIOSLibs, swiftPrebuiltModules, minOS, sdkVersion)
	if err := os.WriteFile(swiftcShim, []byte(swiftcScript), 0o755); err != nil {
		return "", err
	}

	xcrun := filepath.Join(binDir, "xcrun")
	xcrunScript := fmt.Sprintf(`#!/bin/bash
# Minimal xcrun compatibility for iOS package hooks on Linux.
args=("$@")
for arg in "${args[@]}"; do
  if [[ "$arg" == "--show-sdk-path" ]]; then
    echo %q
    exit 0
  fi
done
if [[ "${1:-}" == "--find" || "${1:-}" == "-f" ]]; then
  case "${2:-}" in
    clang|cc|clang++|c++) echo %q ; exit 0 ;;
    ld|ld64|ld64.lld) echo %q ; exit 0 ;;
    swiftc) echo %q ; exit 0 ;;
  esac
fi
# Consume xcrun's SDK selector, then execute the requested tool.
i=0
while (( i < ${#args[@]} )); do
  case "${args[$i]}" in
    --sdk|-sdk) ((i+=2)) ;;
    --toolchain|-toolchain) ((i+=2)) ;;
    --run) ((i+=1)); break ;;
    --*) ((i+=1)) ;;
    *) break ;;
  esac
done
if (( i < ${#args[@]} )); then
  tool="${args[$i]}"
  rest=("${args[@]:$((i+1))}")
  case "$tool" in
    swiftc) exec %q "${rest[@]}" ;;
    clang|cc|clang++|c++) exec %q "${rest[@]}" ;;
    ld|ld64|ld64.lld) exec %q "${rest[@]}" ;;
  esac
fi
echo "ripley xcrun shim: unsupported arguments: $*" >&2
exit 2
`, sdk, compiler, lldShim, swiftcShim, swiftcShim, compiler, lldShim)
	if err := os.WriteFile(xcrun, []byte(xcrunScript), 0o755); err != nil {
		return "", err
	}
	return compiler, nil
}

func nativeAssetEnvironment(binDir, major, compiler, archiver string) []string {
	path := binDir
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cargo := filepath.Join(home, ".cargo", "bin")
		if dirExists(cargo) {
			path += string(os.PathListSeparator) + cargo
		}
	}
	if current := os.Getenv("PATH"); current != "" {
		path += string(os.PathListSeparator) + current
	}
	env := make([]string, 0, len(os.Environ())+6)
	filtered := []string{
		"PATH=",
		"RIPLEY_NATIVE_IOS_MAJOR=",
		"CARGO_TARGET_AARCH64_APPLE_IOS_LINKER=",
		"CC_aarch64_apple_ios=",
		"CXX_aarch64_apple_ios=",
		"AR_aarch64_apple_ios=",
		"lt_cv_sys_max_cmd_len=",
	}
	for _, item := range os.Environ() {
		skip := false
		for _, prefix := range filtered {
			if strings.HasPrefix(item, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, item)
		}
	}
	return append(env,
		"PATH="+path,
		"RIPLEY_NATIVE_IOS_MAJOR="+major,
		"CARGO_TARGET_AARCH64_APPLE_IOS_LINKER="+compiler,
		"CC_aarch64_apple_ios="+compiler,
		"CXX_aarch64_apple_ios="+compiler,
		"AR_aarch64_apple_ios="+archiver,
		// Libtool's Darwin probe calls /sbin/sysctl -n kern.argmax. On Linux that
		// absolute command exists but cannot answer the Darwin key, leaving an
		// empty value that later breaks expr arithmetic. 49152 is libtool's own
		// BSD fallback (65536 with its 25%% safety margin).
		"lt_cv_sys_max_cmd_len=49152",
	)
}

func packageNativeAssets(tc toolchain.Toolchain, result nativeAssetHelperResult, work, minOS string) (NativeAssetBuild, error) {
	manifest := map[string]any{
		"format-version": []int{1, 0, 0},
		"native-assets": map[string]any{
			"ios_arm64": map[string]any{},
		},
	}
	entries := manifest["native-assets"].(map[string]any)["ios_arm64"].(map[string]any)
	if len(result.Assets) == 0 {
		manifest["native-assets"] = map[string]any{}
		data, err := json.Marshal(manifest)
		return NativeAssetBuild{Manifest: data}, err
	}

	frameworkRoot := filepath.Join(work, "frameworks")
	if err := os.MkdirAll(frameworkRoot, 0o755); err != nil {
		return NativeAssetBuild{}, err
	}
	installNameTool, err := findLLVMTool("llvm-install-name-tool", "install_name_tool")
	if err != nil {
		return NativeAssetBuild{}, err
	}
	objdump, err := findLLVMTool("llvm-objdump", "otool")
	if err != nil {
		return NativeAssetBuild{}, err
	}

	taken := make(map[string]bool)
	idTargets := make(map[string][]string)
	type packaged struct {
		binary string
		oldID  string
		newID  string
	}
	var binaries []packaged
	var frameworks []string
	for _, asset := range result.Assets {
		if asset.ID == "" {
			return NativeAssetBuild{}, errors.New("native asset hook returned an asset without an id")
		}
		switch asset.Kind {
		case "system":
			entries[asset.ID] = []string{"system", asset.Path}
		case "process":
			entries[asset.ID] = []string{"process"}
		case "executable":
			entries[asset.ID] = []string{"executable"}
		case "bundled":
			if !fileExists(asset.File) {
				return NativeAssetBuild{}, fmt.Errorf("native asset %s file not found: %s", asset.ID, asset.File)
			}
			if existing, ok := idTargets[asset.ID]; ok {
				entries[asset.ID] = existing
				continue
			}
			name := nativeAssetFrameworkName(filepath.Base(asset.File), taken)
			framework := filepath.Join(frameworkRoot, name+".framework")
			binary := filepath.Join(framework, name)
			if err := os.MkdirAll(framework, 0o755); err != nil {
				return NativeAssetBuild{}, err
			}
			if err := copyFile(asset.File, binary); err != nil {
				return NativeAssetBuild{}, err
			}
			if err := os.Chmod(binary, 0o755); err != nil {
				return NativeAssetBuild{}, err
			}
			oldID, err := machoDylibID(objdump, binary)
			if err != nil {
				return NativeAssetBuild{}, fmt.Errorf("read install name of %s: %w", asset.ID, err)
			}
			newID := "@rpath/" + name + ".framework/" + name
			if err := run("", nil, installNameTool, "-id", newID, binary); err != nil {
				return NativeAssetBuild{}, err
			}
			if err := writeNativeFrameworkInfo(framework, name, minOS); err != nil {
				return NativeAssetBuild{}, err
			}
			target := []string{"absolute", name + ".framework/" + name}
			idTargets[asset.ID] = target
			entries[asset.ID] = target
			frameworks = append(frameworks, framework)
			binaries = append(binaries, packaged{binary: binary, oldID: oldID, newID: newID})
		default:
			return NativeAssetBuild{}, fmt.Errorf("native asset %s has unsupported kind %q", asset.ID, asset.Kind)
		}
	}

	oldToNew := make(map[string]string)
	for _, binary := range binaries {
		if binary.oldID != "" {
			oldToNew[binary.oldID] = binary.newID
		}
	}
	for _, binary := range binaries {
		deps, err := machoDylibsUsed(objdump, binary.binary)
		if err != nil {
			return NativeAssetBuild{}, err
		}
		for _, dep := range deps {
			replacement, ok := oldToNew[dep]
			if !ok || dep == binary.oldID {
				continue
			}
			if err := run("", nil, installNameTool, "-change", dep, replacement, binary.binary); err != nil {
				return NativeAssetBuild{}, err
			}
		}
	}

	sort.Strings(frameworks)
	data, err := json.Marshal(manifest)
	if err != nil {
		return NativeAssetBuild{}, err
	}
	return NativeAssetBuild{Frameworks: frameworks, Manifest: data}, nil
}

func nativeAssetFrameworkName(fileName string, taken map[string]bool) string {
	if strings.HasSuffix(fileName, ".dylib") {
		fileName = strings.TrimSuffix(fileName, ".dylib")
		fileName = strings.TrimPrefix(fileName, "lib")
	}
	var b strings.Builder
	for _, r := range fileName {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	base := b.String()
	if base == "" {
		base = "native_asset"
	}
	name := base
	for i := 1; taken[name]; i++ {
		name = base + strconv.Itoa(i)
	}
	taken[name] = true
	return name
}

func writeNativeFrameworkInfo(framework, name, minOS string) error {
	return writePlist(filepath.Join(framework, "Info.plist"), map[string]any{
		"CFBundleDevelopmentRegion":     "en",
		"CFBundleExecutable":            name,
		"CFBundleIdentifier":            "io.flutter.native-assets." + strings.ReplaceAll(name, "_", "-"),
		"CFBundleInfoDictionaryVersion": "6.0",
		"CFBundleName":                  name,
		"CFBundlePackageType":           "FMWK",
		"CFBundleShortVersionString":    "1.0",
		"CFBundleVersion":               "1",
		"MinimumOSVersion":              minOS,
		"NSPrincipalClass":              "",
	})
}

func findLLVMTool(primary, fallback string) (string, error) {
	for _, candidate := range llvmToolCandidates(primary) {
		if found, err := exec.LookPath(candidate); err == nil {
			return found, nil
		}
	}
	if fallback != "" {
		if found, err := exec.LookPath(fallback); err == nil {
			return found, nil
		}
	}
	return "", fmt.Errorf("%s not found; install LLVM binutils", primary)
}

func machoDylibID(objdump, binary string) (string, error) {
	out, err := exec.Command(objdump, "--macho", "--dylib-id", binary).Output()
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasSuffix(line, ":") {
			continue
		}
		return line, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no LC_ID_DYLIB", binary)
}

func machoDylibsUsed(objdump, binary string) ([]string, error) {
	out, err := exec.Command(objdump, "--macho", "--dylibs-used", binary).Output()
	if err != nil {
		return nil, err
	}
	var deps []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasSuffix(line, ":") {
			continue
		}
		if i := strings.Index(line, " (compatibility version "); i >= 0 {
			line = line[:i]
		}
		deps = append(deps, line)
	}
	return deps, scanner.Err()
}
