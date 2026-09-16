package toolchain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	engineBase = "https://storage.googleapis.com/flutter_infra_release/flutter"
	dartBase   = "https://storage.googleapis.com/dart-archive/channels/stable/release"
)

func RipleyHome() string {
	if home := os.Getenv("RIPLEY_HOME"); home != "" {
		return home
	}
	return filepath.Join(homeDir(), ".ripley")
}

type Toolchain struct {
	EngineRevision string
	DartVersion    string
	Root           string
	Arch           string
}

func NewToolchain(engineRevision, dartVersion string) Toolchain {
	return Toolchain{
		EngineRevision: engineRevision,
		DartVersion:    dartVersion,
		Root:           filepath.Join(RipleyHome(), engineRevision),
		Arch:           hostArch(),
	}
}

func (t Toolchain) DartSDK() string { return filepath.Join(t.Root, "dart-sdk") }
func (t Toolchain) DartAOTRuntime() string {
	return filepath.Join(t.DartSDK(), "bin", "dartaotruntime")
}
func (t Toolchain) Dart() string { return filepath.Join(t.DartSDK(), "bin", "dart") }
func (t Toolchain) FrontendServer() string {
	return filepath.Join(t.Root, "frontend_server_aot.dart.snapshot")
}
func (t Toolchain) ImpellerC() string           { return filepath.Join(t.Root, "impellerc") }
func (t Toolchain) FontSubset() string          { return filepath.Join(t.Root, "font-subset") }
func (t Toolchain) ConstFinder() string         { return filepath.Join(t.Root, "const_finder.dart.snapshot") }
func (t Toolchain) PatchedSDK() string          { return filepath.Join(t.Root, "flutter_patched_sdk_product") }
func (t Toolchain) GenSnapshot() string         { return filepath.Join(t.Root, "gen_snapshot_ios_arm64") }
func (t Toolchain) GenSnapshotLicenses() string { return t.GenSnapshot() + ".LICENSES.txt" }
func (t Toolchain) FlutterXCFramework() string  { return filepath.Join(t.Root, "Flutter.xcframework") }
func (t Toolchain) ToolsetBin() string          { return filepath.Join(t.Root, "toolset", "bin") }
func (t Toolchain) SwiftBin() (string, error) {
	root, err := t.SwiftToolchainRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "usr", "bin"), nil
}
func (t Toolchain) SwiftLib() (string, error) {
	root, err := t.SwiftToolchainRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "usr", "lib", "swift"), nil
}

func (t Toolchain) IOSSDK() (string, error) {
	if sdk := os.Getenv("RIPLEY_IOS_SDK"); sdk != "" {
		return sdk, nil
	}
	matches, err := filepath.Glob(filepath.Join(RipleyHome(), "sdks", "iPhoneOS*.sdk"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", errors.New("no iPhoneOS SDK found; copy one from Xcode to ~/.ripley/sdks or set RIPLEY_IOS_SDK")
	}
	sort.Slice(matches, func(i, j int) bool { return naturalVersionLess(matches[i], matches[j]) })
	return matches[len(matches)-1], nil
}

func (t Toolchain) IOSSDKVersion() (string, error) {
	sdk, err := t.IOSSDK()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(sdk, "SDKSettings.json"))
	if err != nil {
		return "", err
	}
	var settings struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", fmt.Errorf("decode SDKSettings.json: %w", err)
	}
	if settings.Version == "" {
		return "", fmt.Errorf("%s has no Version", filepath.Join(sdk, "SDKSettings.json"))
	}
	return settings.Version, nil
}

func (t Toolchain) SwiftToolchainRoot() (string, error) {
	if root := os.Getenv("RIPLEY_SWIFT"); root != "" {
		return root, nil
	}
	patterns := []string{
		filepath.Join(homeDir(), "swift*", "usr", "bin", "swiftc"),
		filepath.Join(RipleyHome(), "swift*", "usr", "bin", "swiftc"),
	}
	var matches []string
	for _, pattern := range patterns {
		found, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		matches = append(matches, found...)
	}
	if len(matches) > 0 {
		sort.Slice(matches, func(i, j int) bool { return naturalVersionLess(matches[i], matches[j]) })
		return filepath.Clean(filepath.Join(filepath.Dir(matches[len(matches)-1]), "..", "..")), nil
	}
	return "", errors.New("no Swift toolchain found; install Swift for Linux and set RIPLEY_SWIFT")
}

var versionNumberRE = regexp.MustCompile(`\d+`)

func naturalVersionLess(a, b string) bool {
	left := versionNumberRE.FindAllString(a, -1)
	right := versionNumberRE.FindAllString(b, -1)
	for i := 0; i < len(left) || i < len(right); i++ {
		var l, r int
		if i < len(left) {
			l, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			r, _ = strconv.Atoi(right[i])
		}
		if l != r {
			return l < r
		}
	}
	return a < b
}

func (t Toolchain) SwiftIOSLibs() (string, error) {
	if libs := os.Getenv("RIPLEY_SWIFT_IOS_LIBS"); libs != "" {
		return libs, nil
	}
	p := filepath.Join(RipleyHome(), "sdks", "iphoneos")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p, nil
	}
	sdk, err := t.IOSSDK()
	if err != nil {
		return "", err
	}
	return filepath.Join(sdk, "usr", "lib", "swift"), nil
}

func (t Toolchain) Ensure(needIOSSDK bool) error {
	if err := os.MkdirAll(t.Root, 0o755); err != nil {
		return err
	}
	if err := t.ensureDartSDK(); err != nil {
		return err
	}
	if err := t.ensureEngineArtifacts(); err != nil {
		return err
	}
	if err := t.ensureGenSnapshot(); err != nil {
		return err
	}
	if err := t.ensureDarwinToolset(); err != nil {
		return err
	}
	if err := t.ensureZsign(); err != nil {
		return err
	}
	if needIOSSDK {
		if _, err := t.IOSSDK(); err != nil {
			return err
		}
		if _, err := t.SwiftToolchainRoot(); err != nil {
			return err
		}
	}
	return nil
}

func (t Toolchain) ensureDartSDK() error {
	if _, err := os.Stat(t.DartAOTRuntime()); errors.Is(err, os.ErrNotExist) {
		host := "linux-x64"
		if t.Arch == "arm64" {
			host = "linux-arm64"
		}
		z := filepath.Join(t.Root, "dartsdk.zip")
		url := fmt.Sprintf("%s/%s/sdk/dartsdk-%s-release.zip", dartBase, t.DartVersion, host)
		if err := download(url, z); err != nil {
			return err
		}
		if err := unzip(z, t.Root, nil); err != nil {
			return err
		}
		if err := os.Remove(z); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	entries, err := os.ReadDir(filepath.Join(t.DartSDK(), "bin"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != "" {
			continue
		}
		path := filepath.Join(t.DartSDK(), "bin", entry.Name())
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err := os.Chmod(path, st.Mode().Perm()|0o111); err != nil {
			return err
		}
	}
	return nil
}

func (t Toolchain) ensureEngineArtifacts() error {
	host := "linux-x64"
	if t.Arch == "arm64" {
		host = "linux-arm64"
	}
	shader := filepath.Join(t.Root, "shader_lib", "flutter", "runtime_effect.glsl")
	if !fileExists(t.FrontendServer()) || !fileExists(t.ImpellerC()) || !fileExists(shader) {
		z := filepath.Join(t.Root, "eng-linux.zip")
		if err := download(fmt.Sprintf("%s/%s/%s/artifacts.zip", engineBase, t.EngineRevision, host), z); err != nil {
			return err
		}
		keep := func(name string) bool {
			return name == "frontend_server_aot.dart.snapshot" || name == "impellerc" || strings.HasPrefix(name, "shader_lib/")
		}
		if err := unzip(z, t.Root, keep); err != nil {
			return err
		}
		if err := os.Remove(z); err != nil {
			return err
		}
		if fileExists(t.ImpellerC()) {
			_ = os.Chmod(t.ImpellerC(), 0o755)
		}
	}
	if !dirExists(t.PatchedSDK()) {
		z := filepath.Join(t.Root, "patched-sdk.zip")
		if err := download(fmt.Sprintf("%s/%s/flutter_patched_sdk_product.zip", engineBase, t.EngineRevision), z); err != nil {
			return err
		}
		if err := unzip(z, t.Root, nil); err != nil {
			return err
		}
		if err := os.Remove(z); err != nil {
			return err
		}
	}
	if !fileExists(t.FontSubset()) || !fileExists(t.ConstFinder()) {
		z := filepath.Join(t.Root, "font-subset.zip")
		if err := download(fmt.Sprintf("%s/%s/%s/font-subset.zip", engineBase, t.EngineRevision, host), z); err != nil {
			return err
		}
		if err := unzip(z, t.Root, nil); err != nil {
			return err
		}
		if err := os.Remove(z); err != nil {
			return err
		}
		if fileExists(t.FontSubset()) {
			_ = os.Chmod(t.FontSubset(), 0o755)
		}
	}
	if err := t.ensureFrontendServerHostArch(); err != nil {
		return err
	}
	if !dirExists(t.FlutterXCFramework()) {
		z := filepath.Join(t.Root, "eng-ios.zip")
		if err := download(fmt.Sprintf("%s/%s/ios-release/artifacts.zip", engineBase, t.EngineRevision), z); err != nil {
			return err
		}
		if err := unzip(z, t.Root, nil); err != nil {
			return err
		}
		if err := os.Remove(z); err != nil {
			return err
		}
	}
	return nil
}

func (t Toolchain) ensureGenSnapshot() error {
	if fileExists(t.GenSnapshot()) {
		return nil
	}
	built := filepath.Join(t.Root, "out", "RelIOS", "gen_snapshot_product_ios_arm64")
	if fileExists(built) {
		if err := os.Rename(built, t.GenSnapshot()); err != nil {
			return err
		}
		return os.Chmod(t.GenSnapshot(), 0o755)
	}
	return errors.New("gen_snapshot for ios-arm64 not found; run `ripley toolchain build` once")
}

func (t Toolchain) ensureDarwinToolset() error {
	if fileExists(filepath.Join(t.ToolsetBin(), "ld64.lld")) {
		return nil
	}
	arch := "x86_64"
	if t.Arch == "arm64" {
		arch = "aarch64"
	}
	url := fmt.Sprintf("https://github.com/xtool-org/darwin-tools-linux-llvm/releases/download/v1.0.1/toolset-%s.tar.gz", arch)
	tarball := filepath.Join(t.Root, "toolset.tar.gz")
	if err := download(url, tarball); err != nil {
		return err
	}
	if err := run("", nil, "tar", "xzf", tarball, "-C", t.Root); err != nil {
		return err
	}
	if err := os.Remove(tarball); err != nil {
		return err
	}
	bin := filepath.Join(t.Root, "bin")
	if !dirExists(bin) {
		return nil
	}
	if err := os.MkdirAll(t.ToolsetBin(), 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(bin)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(bin, entry.Name()), filepath.Join(t.ToolsetBin(), entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(bin)
}

func FlutterEngineRevision(flutterRoot string) (string, string, error) {
	data, err := os.ReadFile(filepath.Join(flutterRoot, "bin", "cache", "flutter.version.json"))
	if err != nil {
		return "", "", err
	}
	var version struct {
		EngineRevision string `json:"engineRevision"`
		DartSDKVersion string `json:"dartSdkVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return "", "", fmt.Errorf("decode flutter.version.json: %w", err)
	}
	if version.EngineRevision == "" || version.DartSDKVersion == "" {
		return "", "", errors.New("flutter.version.json is missing engineRevision or dartSdkVersion")
	}
	return version.EngineRevision, version.DartSDKVersion, nil
}
