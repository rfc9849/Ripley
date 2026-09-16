package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateDartPluginRegistrantForIOS(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "cache", "sample_foundation")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	pubspec := `name: sample_foundation
flutter:
  plugin:
    implements: sample
    platforms:
      ios:
        pluginClass: SamplePlugin
        dartPluginClass: SampleFoundation
        dartFileName: src/sample_foundation.dart
`
	if err := os.WriteFile(filepath.Join(pluginRoot, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := `{"plugins":{"ios":[{"name":"sample_foundation","path":` + quoteJSON(pluginRoot) + `}]}}`
	if err := os.WriteFile(filepath.Join(root, ".flutter-plugins-dependencies"), []byte(deps), 0o644); err != nil {
		t.Fatal(err)
	}
	project := project{Root: root, DartTool: filepath.Join(root, ".dart_tool")}
	path, err := generateDartPluginRegistrant(project)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected generated registrant")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"import 'package:sample_foundation/src/sample_foundation.dart' as sample_foundation;",
		"sample_foundation.SampleFoundation.registerWith();",
		"if (Platform.isIOS)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("registrant missing %q:\n%s", want, text)
		}
	}
	args := dartPluginRegistrantFrontendArgs(path)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--source package:flutter/src/dart_plugin_registrant.dart") {
		t.Fatalf("frontend args missing Flutter registrant source: %v", args)
	}
	if !strings.Contains(joined, "-Dflutter.dart_plugin_registrant=file://") {
		t.Fatalf("frontend args missing registrant define: %v", args)
	}
}

func TestGenerateDartPluginRegistrantUsesDefaultDartFileName(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "cache", "sample_foundation")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	pubspec := `name: sample_foundation
flutter:
  plugin:
    platforms:
      ios:
        dartPluginClass: SampleFoundation
`
	if err := os.WriteFile(filepath.Join(pluginRoot, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := `{"plugins":{"ios":[{"name":"sample_foundation","path":` + quoteJSON(pluginRoot) + `}]}}`
	if err := os.WriteFile(filepath.Join(root, ".flutter-plugins-dependencies"), []byte(deps), 0o644); err != nil {
		t.Fatal(err)
	}
	project := project{Root: root, DartTool: filepath.Join(root, ".dart_tool")}
	path, err := generateDartPluginRegistrant(project)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "package:sample_foundation/sample_foundation.dart") {
		t.Fatalf("default dart file name not used:\n%s", data)
	}
}

func TestGenerateDartPluginRegistrantSkipsNativeOnlyPlugins(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "cache", "native_only")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	pubspec := `name: native_only
flutter:
  plugin:
    platforms:
      ios:
        pluginClass: NativeOnlyPlugin
`
	if err := os.WriteFile(filepath.Join(pluginRoot, "pubspec.yaml"), []byte(pubspec), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := `{"plugins":{"ios":[{"name":"native_only","path":` + quoteJSON(pluginRoot) + `}]}}`
	if err := os.WriteFile(filepath.Join(root, ".flutter-plugins-dependencies"), []byte(deps), 0o644); err != nil {
		t.Fatal(err)
	}
	project := project{Root: root, DartTool: filepath.Join(root, ".dart_tool")}
	path, err := generateDartPluginRegistrant(project)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("native-only plugin unexpectedly generated Dart registrant at %s", path)
	}
}

func quoteJSON(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}
