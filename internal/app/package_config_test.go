package app

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizePackageConfigRootURIsMakesRelativePathsAbsolute(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, ".dart_tool", "package_config.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{
  "configVersion": 2,
  "packages": [
    {"name":"local_pkg","rootUri":"../packages/local pkg","packageUri":"lib/"},
    {"name":"hosted_pkg","rootUri":"file:///tmp/hosted_pkg","packageUri":"lib/"}
  ],
  "generator": "flutter"
}`
	if err := os.WriteFile(config, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := normalizePackageConfigRootURIs(config); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	out, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	packages := got["packages"].([]any)
	local := packages[0].(map[string]any)["rootUri"].(string)
	parsed, err := url.Parse(local)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(root, "packages", "local pkg")
	if parsed.Scheme != "file" || filepath.Clean(parsed.Path) != filepath.Clean(wantPath) {
		t.Fatalf("normalized rootUri = %q, want file URI for %q", local, wantPath)
	}
	hosted := packages[1].(map[string]any)["rootUri"].(string)
	if hosted != "file:///tmp/hosted_pkg" {
		t.Fatalf("absolute rootUri changed to %q", hosted)
	}
	if got["generator"] != "flutter" {
		t.Fatalf("unknown package_config fields were not preserved: %#v", got)
	}
}

func TestFindProjectPackageConfigUsesWorkspaceAncestor(t *testing.T) {
	workspace := t.TempDir()
	member := filepath.Join(workspace, "app")
	config := filepath.Join(workspace, ".dart_tool", "package_config.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{
  "configVersion": 2,
  "packages": [
    {"name":"member","rootUri":"../app","packageUri":"lib/"}
  ]
}`
	if err := os.WriteFile(config, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got := findProjectPackageConfig(member)
	if got != config {
		t.Fatalf("findProjectPackageConfig = %q, want workspace config %q", got, config)
	}
	project, err := newProject(member)
	if err != nil {
		t.Fatal(err)
	}
	if project.packageConfig() != config {
		t.Fatalf("project.packageConfig = %q, want %q", project.packageConfig(), config)
	}
	if project.DartTool != filepath.Join(member, ".dart_tool") {
		t.Fatalf("member-local DartTool changed to %q", project.DartTool)
	}
}

func TestFindProjectPackageConfigIgnoresUnrelatedAncestor(t *testing.T) {
	workspace := t.TempDir()
	member := filepath.Join(workspace, "app")
	other := filepath.Join(workspace, "other")
	config := filepath.Join(workspace, ".dart_tool", "package_config.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"configVersion":2,"packages":[{"name":"other","rootUri":"../other","packageUri":"lib/"}]}`
	if err := os.WriteFile(config, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got := findProjectPackageConfig(member)
	want := filepath.Join(member, ".dart_tool", "package_config.json")
	if got != want {
		t.Fatalf("findProjectPackageConfig = %q, want member fallback %q", got, want)
	}
	_ = other
}
