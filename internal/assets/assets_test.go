package assets

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestEncodeAssetManifestKnownEncoding(t *testing.T) {
	manifest := map[string][]manifestVariant{
		"images/a.png": {
			{asset: "images/a.png"},
			{asset: "images/2.0x/a.png", DPR: 2, HasDPR: true},
		},
	}
	got := encodeAssetManifest(manifest, []string{"images/a.png"})
	want, err := hex.DecodeString("0d01070c696d616765732f612e706e670c020d0107056173736574070c696d616765732f612e706e670d02070561737365740711696d616765732f322e30782f612e706e6707036470720600000000000000000000000040")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("SMC mismatch\n got: %x\nwant: %x", got, want)
	}
}

func TestFontManifestFieldOrder(t *testing.T) {
	weight := 700
	data, err := json.Marshal([]font{{
		Family: "Demo",
		Fonts:  []fontAsset{{Weight: &weight, Style: "italic", Asset: "fonts/a.ttf"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"family":"Demo","fonts":[{"weight":700,"style":"italic","asset":"fonts/a.ttf"}]}]`
	if string(data) != want {
		t.Fatalf("fontManifest JSON mismatch\n got: %s\nwant: %s", data, want)
	}
}

func TestPubspecFlutterPresence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pubspec.yaml")
	if err := os.WriteFile(path, []byte("name: demo\nflutter:\n  generate: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := loadPubspec(path)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.FlutterNonEmpty {
		t.Fatal("flutter section with an unmodeled key must be non-empty")
	}
	if err := os.WriteFile(path, []byte("name: demo\nflutter: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err = loadPubspec(path)
	if err != nil {
		t.Fatal(err)
	}
	if spec.FlutterNonEmpty {
		t.Fatal("empty flutter mapping must be empty")
	}
}

func TestGeneratedAssetFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".dart_tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pubspec.yaml"), []byte("name: demo\nflutter:\n  generate: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dart_tool", "package_config.json"), []byte(`{"packages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := NewAssetBundle(root, root, "ios", "", filepath.Join(root, ".dart_tool", "package_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Build(); err != nil {
		t.Fatal(err)
	}
	if got := string(bundle.Entries["NativeAssetsManifest.json"].Data); got != `{"format-version":[1,0,0],"native-assets":{}}` {
		t.Fatalf("NativeassetsManifest mismatch: %q", got)
	}
	notices := bundle.Entries["NOTICES.Z"].Data
	reader, err := gzip.NewReader(bytes.NewReader(notices))
	if err != nil {
		t.Fatalf("NOTICES.Z is not valid gzip: %v", err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if len(decompressed) != 0 {
		t.Fatalf("expected empty notices, got %q", decompressed)
	}
}

// A pub workspace member has no member-local .dart_tool/package_config.json:
// pub writes one shared config at the workspace root. The bundle must read the
// config it is given rather than deriving a member-local path, otherwise every
// workspace member fails asset generation.
func TestAssetBundleUsesWorkspacePackageConfig(t *testing.T) {
	workspace := t.TempDir()
	member := filepath.Join(workspace, "app")
	dependency := filepath.Join(workspace, "packages", "widgets")
	if err := os.MkdirAll(filepath.Join(dependency, "lib", "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".dart_tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(member, "pubspec.yaml"), []byte("name: demo\nflutter:\n  assets:\n    - packages/widgets/images/logo.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependency, "pubspec.yaml"), []byte("name: widgets\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependency, "lib", "images", "logo.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(workspace, ".dart_tool", "package_config.json")
	// build() normalizes relative rootUri values to absolute file:// URIs before
	// the asset layer runs, so that is the shape this code actually receives.
	contents := fmt.Sprintf(`{"packages":[
		{"name":"demo","rootUri":%q,"packageUri":"lib/"},
		{"name":"widgets","rootUri":%q,"packageUri":"lib/"}
	]}`, "file://"+filepath.ToSlash(member), "file://"+filepath.ToSlash(dependency))
	if err := os.WriteFile(config, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	bundle, err := NewAssetBundle(member, member, "ios", "", config)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Build(); err != nil {
		t.Fatalf("Build with workspace package config: %v", err)
	}
	if _, ok := bundle.Entries["packages/widgets/images/logo.png"]; !ok {
		t.Fatalf("asset from workspace dependency missing; entries: %v", bundleEntryNames(bundle))
	}
}

func bundleEntryNames(b *AssetBundle) []string {
	names := make([]string, 0, len(b.Entries))
	for name := range b.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
