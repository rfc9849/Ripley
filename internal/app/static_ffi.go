package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"ripley/internal/ios"
)

// prepareKernelPackageConfig adapts Dart FFI loaders to real CocoaPods static
// frameworks. A static framework is a link-time container: its symbols live in
// the final executable and the framework must not be embedded for dyld to open.
// flutter_rust_bridge normally opens <stem>.framework/<stem> on iOS, so for
// static linkage we compile against a build-local copy whose final fallback is
// ExternalLibrary.process(). The source package and pub cache remain untouched.
func prepareKernelPackageConfig(project project, plugins ios.PluginBuild) (string, error) {
	configPath := project.packageConfig()
	if plugins.FrameworkLinkage != "static" {
		return configPath, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("decode Dart package config for static FFI overlay: %w", err)
	}
	packages, ok := config["packages"].([]any)
	if !ok {
		return "", fmt.Errorf("Dart package config %s has no packages array", configPath)
	}

	var packageEntry map[string]any
	for _, raw := range packages {
		entry, ok := raw.(map[string]any)
		if ok && entry["name"] == "flutter_rust_bridge" {
			packageEntry = entry
			break
		}
	}
	if packageEntry == nil {
		return configPath, nil
	}
	rootURI, _ := packageEntry["rootUri"].(string)
	if rootURI == "" {
		return "", fmt.Errorf("flutter_rust_bridge package entry has no rootUri")
	}
	root, err := packageRootFromURI(configPath, rootURI)
	if err != nil {
		return "", fmt.Errorf("resolve flutter_rust_bridge package root: %w", err)
	}

	overlay := filepath.Join(project.BuildDir, "dart_overlays", "flutter_rust_bridge")
	if err := os.RemoveAll(overlay); err != nil {
		return "", err
	}
	if err := copyDir(root, overlay); err != nil {
		return "", fmt.Errorf("copy flutter_rust_bridge static-link overlay: %w", err)
	}
	loader := filepath.Join(overlay, "lib", "src", "loader", "_io.dart")
	if err := patchFlutterRustBridgeStaticLoader(loader); err != nil {
		return "", err
	}

	packageEntry["rootUri"] = (&url.URL{Scheme: "file", Path: filepath.ToSlash(overlay)}).String()
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	overlayConfig := filepath.Join(project.BuildDir, "dart_overlays", "package_config.static_frameworks.json")
	if err := os.WriteFile(overlayConfig, append(out, '\n'), 0o644); err != nil {
		return "", err
	}
	fmt.Println("dart ffi: flutter_rust_bridge uses process image for static CocoaPods frameworks")
	return overlayConfig, nil
}

func patchFlutterRustBridgeStaticLoader(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read flutter_rust_bridge loader: %w", err)
	}
	text := string(data)
	if strings.Contains(text, "ripley static CocoaPods process image") {
		return nil
	}
	needle := `(debugInfo) =>
            ExternalLibrary.open('$stem.framework/$stem', debugInfo: debugInfo),`
	replacement := `(debugInfo) => _tryOpen(
        '$stem.framework/$stem',
        debugInfo,
        (debugInfo) => ExternalLibrary.process(
          iKnowHowToUseIt: true,
          debugInfo: '$debugInfo (ripley static CocoaPods process image)',
        ),
      ),`
	if !strings.Contains(text, needle) {
		return fmt.Errorf("flutter_rust_bridge loader %s has an unsupported iOS loading shape", path)
	}
	text = strings.Replace(text, needle, replacement, 1)
	return os.WriteFile(path, []byte(text), 0o644)
}
