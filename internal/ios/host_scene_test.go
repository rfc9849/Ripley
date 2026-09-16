package ios

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSwiftSourcesSynthesizesSceneDelegate(t *testing.T) {
	sourceDir := t.TempDir()
	work := filepath.Join(t.TempDir(), "host")
	appDelegate := filepath.Join(sourceDir, "AppDelegate.swift")
	if err := os.WriteFile(appDelegate, []byte("import Flutter\n@main class AppDelegate: FlutterAppDelegate {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sources, err := projectSwiftSources(sourceDir, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2: %v", len(sources), sources)
	}
	var generated string
	for _, path := range sources {
		if filepath.Dir(path) == work && filepath.Base(path) == "SceneDelegate.swift" {
			generated = path
		}
	}
	if generated == "" {
		t.Fatalf("generated SceneDelegate.swift missing from %v", sources)
	}
	data, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "FlutterViewController") || !strings.Contains(string(data), "UIWindow(windowScene:") {
		t.Fatalf("generated SceneDelegate does not bootstrap Flutter: %s", data)
	}
}

func TestProjectSwiftSourcesDoesNotDuplicateSceneDelegate(t *testing.T) {
	sourceDir := t.TempDir()
	work := filepath.Join(t.TempDir(), "host")
	appDelegate := filepath.Join(sourceDir, "AppDelegate.swift")
	sceneDelegate := filepath.Join(sourceDir, "Custom.swift")
	if err := os.WriteFile(appDelegate, []byte("import Flutter\n@main class AppDelegate: FlutterAppDelegate {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sceneDelegate, []byte("import UIKit\nfinal class SceneDelegate: UIResponder {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sources, err := projectSwiftSources(sourceDir, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want original 2: %v", len(sources), sources)
	}
	for _, path := range sources {
		if filepath.Dir(path) == work {
			t.Fatalf("unexpected generated source %s", path)
		}
	}
}
