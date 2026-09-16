package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLinkerSDKFixture(t *testing.T, root, tbd string) string {
	t.Helper()
	sdk := filepath.Join(root, "iPhoneOS27.0.sdk")
	if err := os.MkdirAll(filepath.Join(sdk, "System", "Library", "Frameworks", "UIKit.framework"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sdk, "usr", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "SDKSettings.json"), []byte(`{"Version":"27.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "System", "Library", "Frameworks", "UIKit.framework", "UIKit.tbd"), []byte(tbd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "usr", "lib", "libCompat.a"), []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sdk
}

func TestLinkerIOSSDKReturnsOriginalWhenNoCompatNeeded(t *testing.T) {
	root := t.TempDir()
	sdk := writeLinkerSDKFixture(t, root, "--- !tapi-tbd\ntargets: [ arm64-ios, arm64e-ios ]\n")
	t.Setenv("RIPLEY_IOS_SDK", sdk)
	tc := Toolchain{Root: filepath.Join(root, "toolchain")}
	got, err := tc.LinkerIOSSDK()
	if err != nil {
		t.Fatal(err)
	}
	if got != sdk {
		t.Fatalf("LinkerIOSSDK() = %q, want original %q", got, sdk)
	}
}

func TestLinkerIOSSDKSparseViewSanitizesArm64EX1(t *testing.T) {
	root := t.TempDir()
	sdk := writeLinkerSDKFixture(t, root, "--- !tapi-tbd\ntargets: [ arm64e-ios, arm64e.x1-ios ]\n")
	t.Setenv("RIPLEY_IOS_SDK", sdk)
	tc := Toolchain{Root: filepath.Join(root, "toolchain")}
	view, err := tc.LinkerIOSSDK()
	if err != nil {
		t.Fatal(err)
	}
	if view == sdk {
		t.Fatal("expected a linker compatibility view")
	}
	data, err := os.ReadFile(filepath.Join(view, "System", "Library", "Frameworks", "UIKit.framework", "UIKit.tbd"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, unsupportedTAPITarget) || !strings.Contains(text, "arm64e-ios") {
		t.Fatalf("sanitized tbd = %q", text)
	}
	archive := filepath.Join(view, "usr", "lib", "libCompat.a")
	info, err := os.Lstat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", archive)
	}
	view2, err := tc.LinkerIOSSDK()
	if err != nil {
		t.Fatal(err)
	}
	if view2 != view {
		t.Fatalf("cached view changed: %q -> %q", view, view2)
	}
}

func TestSanitizeTAPIRejectsStandaloneArm64EX1(t *testing.T) {
	_, _, err := sanitizeTAPIForLLD([]byte("targets: [ arm64e.x1-ios ]\n"))
	if err == nil || !strings.Contains(err.Error(), "without a compatible sibling") {
		t.Fatalf("error = %v", err)
	}
}
