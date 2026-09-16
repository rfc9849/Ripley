package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ripley/internal/ios"
)

func TestPrepareKernelPackageConfigStaticFrameworksOverlaysFlutterRustBridge(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "app")
	bridgeRoot := filepath.Join(root, "flutter_rust_bridge")
	loader := filepath.Join(bridgeRoot, "lib", "src", "loader", "_io.dart")
	if err := os.MkdirAll(filepath.Dir(loader), 0o755); err != nil {
		t.Fatal(err)
	}
	original := `ExternalLibrary loadExternalLibraryRaw({required String stem}) {
  return _tryOpen(
    'rust_builder.framework/rust_builder',
    'debug',
    (debugInfo) =>
            ExternalLibrary.open('$stem.framework/$stem', debugInfo: debugInfo),
  );
}
`
	if err := os.WriteFile(loader, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(projectRoot, ".dart_tool", "package_config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"configVersion": 2,
		"packages": []any{
			map[string]any{"name": "flutter_rust_bridge", "rootUri": "file://" + filepath.ToSlash(bridgeRoot), "packageUri": "lib/"},
			map[string]any{"name": "app", "rootUri": "file://" + filepath.ToSlash(projectRoot), "packageUri": "lib/"},
		},
		"generator": "test",
	}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	p := project{Root: projectRoot, BuildDir: filepath.Join(projectRoot, "build", "ripley_ios"), DartTool: filepath.Join(projectRoot, ".dart_tool"), PackageConfig: configPath}

	got, err := prepareKernelPackageConfig(p, ios.PluginBuild{FrameworkLinkage: "static"})
	if err != nil {
		t.Fatal(err)
	}
	if got == configPath {
		t.Fatal("static framework build reused original package config")
	}
	patched, err := os.ReadFile(filepath.Join(p.BuildDir, "dart_overlays", "flutter_rust_bridge", "lib", "src", "loader", "_io.dart"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), "ExternalLibrary.process(") || !strings.Contains(string(patched), "ripley static CocoaPods process image") {
		t.Fatalf("overlay loader was not patched for process image:\n%s", patched)
	}
	unchanged, err := os.ReadFile(loader)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != original {
		t.Fatal("source flutter_rust_bridge package was modified")
	}

	var over map[string]any
	out, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &over); err != nil {
		t.Fatal(err)
	}
	if over["generator"] != "test" {
		t.Fatalf("package config metadata was lost: %#v", over)
	}
}

func TestPrepareKernelPackageConfigDynamicKeepsOriginal(t *testing.T) {
	p := project{PackageConfig: "/tmp/original-package-config.json"}
	got, err := prepareKernelPackageConfig(p, ios.PluginBuild{FrameworkLinkage: "dynamic"})
	if err != nil {
		t.Fatal(err)
	}
	if got != p.PackageConfig {
		t.Fatalf("dynamic linkage package config = %q, want %q", got, p.PackageConfig)
	}
}
