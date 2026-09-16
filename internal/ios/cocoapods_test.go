package ios

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPodManifestExtractorIncludesUserHeaderSearchPaths(t *testing.T) {
	want := "effective_list(raw, 'USER_HEADER_SEARCH_PATHS', vars)"
	if !strings.Contains(podManifestRuby, want) {
		t.Fatalf("pod target extractor does not preserve USER_HEADER_SEARCH_PATHS")
	}
}

func TestWriteResolverPodfileCarriesAppSwiftVersion(t *testing.T) {
	root := t.TempDir()
	iosDir := filepath.Join(root, "ios")
	if err := os.MkdirAll(iosDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iosDir, "Podfile"), []byte("target 'Runner' do\n  use_frameworks!\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolver := filepath.Join(root, "build", "ripley_ios", "cocoapods", "Podfile")
	if err := os.MkdirAll(filepath.Dir(resolver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeResolverPodfile(resolver, root, nil, "15.5", "5.0"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(resolver)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	want := "current_target_definition.swift_version = '5.0'"
	if !strings.Contains(text, want) {
		t.Fatalf("resolver Podfile missing %q:\n%s", want, text)
	}
}

func TestProjectPathPodsResolvesFlutterPluginSymlinkBeforePodInstall(t *testing.T) {
	root := t.TempDir()
	iosDir := filepath.Join(root, "ios")
	pluginRoot := filepath.Join(root, "pub-cache", "share_handler_ios")
	models := filepath.Join(pluginRoot, "ios", "Models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(models, "share_handler_ios_models.podspec"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(iosDir, 0o755); err != nil {
		t.Fatal(err)
	}
	podfile := `target 'Runner' do
  target 'ShareExtension' do
    pod "share_handler_ios_models", :path => ".symlinks/plugins/share_handler_ios/ios/Models"
  end
end
`
	if err := os.WriteFile(filepath.Join(iosDir, "Podfile"), []byte(podfile), 0o644); err != nil {
		t.Fatal(err)
	}

	pods, err := projectPathPods(root, []Plugin{{Name: "share_handler_ios", Path: pluginRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 {
		t.Fatalf("pods = %#v, want one local pod", pods)
	}
	if pods[0].Name != "share_handler_ios_models" || pods[0].Path != models {
		t.Fatalf("pod = %#v, want share_handler_ios_models at %s", pods[0], models)
	}
}

func TestProjectPodFrameworkLinkage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		podfile   string
		framework bool
		linkage   string
	}{
		{name: "none", podfile: "target 'Runner' do\nend\n"},
		{name: "dynamic default", podfile: "target 'Runner' do\n  use_frameworks!\nend\n", framework: true},
		{name: "dynamic explicit", podfile: "use_frameworks! :linkage => :dynamic\n", framework: true, linkage: "dynamic"},
		{name: "static explicit", podfile: "  use_frameworks! :linkage => :static\n", framework: true, linkage: "static"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			iosDir := filepath.Join(root, "ios")
			if err := os.MkdirAll(iosDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(iosDir, "Podfile"), []byte(tc.podfile), 0o644); err != nil {
				t.Fatal(err)
			}
			framework, linkage, err := projectPodFrameworkLinkage(root)
			if err != nil {
				t.Fatal(err)
			}
			if framework != tc.framework || linkage != tc.linkage {
				t.Fatalf("got framework=%v linkage=%q, want framework=%v linkage=%q", framework, linkage, tc.framework, tc.linkage)
			}
		})
	}
}

func TestProjectPathPodsPrefersExistingLiteralPath(t *testing.T) {
	root := t.TempDir()
	iosDir := filepath.Join(root, "ios")
	local := filepath.Join(iosDir, "LocalPod")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iosDir, "Podfile"), []byte(`pod 'LocalPod', :path => 'LocalPod'\n`), 0o644); err != nil {
		t.Fatal(err)
	}
	pods, err := projectPathPods(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Path != local {
		t.Fatalf("pods = %#v, want literal path %s", pods, local)
	}
}
