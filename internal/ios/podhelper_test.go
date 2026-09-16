package ios

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"ripley/internal/toolchain"
)

// fakeCachedXCFramework lays out a minimal but structurally real
// Flutter.xcframework under a fake toolchain root, mirroring what ripley caches at
// ~/.ripley/<engine>/Flutter.xcframework: an ios-arm64 slice holding
// Flutter.framework with a binary, plus the dSYMs sibling the real engine
// artifact ships.
func fakeCachedXCFramework(t *testing.T) toolchain.Toolchain {
	t.Helper()
	tc := toolchain.Toolchain{Root: t.TempDir()}
	framework := filepath.Join(tc.FlutterXCFramework(), "ios-arm64", "Flutter.framework")
	writeTestFile(t, filepath.Join(framework, "Flutter"), "MACHO")
	writeTestFile(t, filepath.Join(framework, "Headers", "Flutter.h"), "// header\n")
	writeTestFile(t, filepath.Join(tc.FlutterXCFramework(), "ios-arm64", "dSYMs", "Flutter.framework.dSYM", "Contents", "Info.plist"), "<plist/>")
	return tc
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// podhelperEngineDir is the directory podhelper.rb resolves from
// $FLUTTER_ROOT/packages/flutter_tools/bin via ('..','..','..','..','bin',
// 'cache','artifacts','engine').
func podhelperEngineDir(flutterRoot, name string) string {
	return filepath.Join(flutterRoot, "bin", "cache", "artifacts", "engine", name, "Flutter.xcframework")
}

type treeEntry struct {
	rel     string
	mode    os.FileMode
	size    int64
	modTime int64
	link    string
	content string
}

// snapshotTree records every path under root with the metadata that would
// change if a second Ensure call rewrote anything: mode, size, mtime, symlink
// target and file bytes.
func snapshotTree(t *testing.T, root string) []treeEntry {
	t.Helper()
	var entries []treeEntry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		entry := treeEntry{rel: rel, mode: info.Mode(), modTime: info.ModTime().UnixNano()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}
			entry.link = target
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			entry.size = info.Size()
			entry.content = string(data)
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries
}

// filepath.Walk follows the root but not symlinks, which is exactly what we
// want: a staged slice is a link, and we assert on the link itself.
func TestEnsureFlutterIOSArtifactsStagesBothPodhelperDirs(t *testing.T) {
	tc := fakeCachedXCFramework(t)
	flutterRoot := t.TempDir()

	if err := EnsureFlutterIOSArtifacts(tc, flutterRoot); err != nil {
		t.Fatalf("EnsureFlutterIOSArtifacts: %v", err)
	}

	// podhelper.rb requires exactly these two: `ios` for its explicit
	// Dir.exist? raise, `ios-release` for the Dir.new(...) it performs for
	// every non-debug build configuration.
	for _, name := range []string{"ios", "ios-release"} {
		dest := podhelperEngineDir(flutterRoot, name)
		if !dirExists(dest) {
			t.Fatalf("%s: Dir.exist? would fail, podhelper.rb raises", dest)
		}
		if !fileExists(filepath.Join(dest, "Info.plist")) {
			t.Errorf("%s: staged xcframework has no top-level Info.plist", dest)
		}
		// The slice must be reachable the way podhelper.rb reaches it: as a
		// child entry starting with "ios-" that resolves to a framework dir.
		library, headers, err := selectXCFrameworkSlice(dest)
		if err != nil {
			t.Fatalf("%s: %v", dest, err)
		}
		if got := filepath.Base(library); got != "Flutter.framework" {
			t.Errorf("%s: selected slice %s, want Flutter.framework", dest, got)
		}
		if !fileExists(filepath.Join(library, "Flutter")) {
			t.Errorf("%s: selected slice has no Flutter binary", dest)
		}
		if headers != "" {
			t.Errorf("%s: unexpected HeadersPath %q; the engine xcframework declares none", dest, headers)
		}
	}

	// A profile build reuses ios-release, so there must be no ios-profile
	// directory invented for it.
	if dirExists(filepath.Join(flutterRoot, "bin", "cache", "artifacts", "engine", "ios-profile")) {
		t.Error("ios-profile was staged; podhelper.rb maps profile onto ios-release")
	}
}

func TestEnsureFlutterIOSArtifactsSecondCallChangesNothing(t *testing.T) {
	tc := fakeCachedXCFramework(t)
	flutterRoot := t.TempDir()

	if err := EnsureFlutterIOSArtifacts(tc, flutterRoot); err != nil {
		t.Fatalf("first call: %v", err)
	}
	before := snapshotTree(t, flutterRoot)

	if err := EnsureFlutterIOSArtifacts(tc, flutterRoot); err != nil {
		t.Fatalf("second call: %v", err)
	}
	after := snapshotTree(t, flutterRoot)

	if len(before) != len(after) {
		t.Fatalf("tree size changed: %d entries before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("second call rewrote %s:\n before %+v\n after  %+v", before[i].rel, before[i], after[i])
		}
	}
}

func TestEnsureFlutterIOSArtifactsPreservesRealPrecacheOutput(t *testing.T) {
	tc := fakeCachedXCFramework(t)
	flutterRoot := t.TempDir()

	// What `flutter precache --ios` leaves on a macOS host: real directories
	// (not links), a simulator slice ripley never has, and Apple's own Info.plist.
	realPlist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>AvailableLibraries</key><array>
<dict><key>LibraryIdentifier</key><string>ios-arm64</string><key>LibraryPath</key><string>Flutter.framework</string><key>SupportedArchitectures</key><array><string>arm64</string></array><key>SupportedPlatform</key><string>ios</string></dict>
<dict><key>LibraryIdentifier</key><string>ios-arm64_x86_64-simulator</string><key>LibraryPath</key><string>Flutter.framework</string><key>SupportedArchitectures</key><array><string>arm64</string><string>x86_64</string></array><key>SupportedPlatform</key><string>ios</string><key>SupportedPlatformVariant</key><string>simulator</string></dict>
</array><key>CFBundlePackageType</key><string>XFWK</string></dict></plist>`
	for _, name := range []string{"ios", "ios-release"} {
		dest := podhelperEngineDir(flutterRoot, name)
		writeTestFile(t, filepath.Join(dest, "ios-arm64", "Flutter.framework", "Flutter"), "REAL-DEVICE-"+name)
		writeTestFile(t, filepath.Join(dest, "ios-arm64_x86_64-simulator", "Flutter.framework", "Flutter"), "REAL-SIM-"+name)
		writeTestFile(t, filepath.Join(dest, "Info.plist"), realPlist)
	}
	before := snapshotTree(t, flutterRoot)

	if err := EnsureFlutterIOSArtifacts(tc, flutterRoot); err != nil {
		t.Fatalf("EnsureFlutterIOSArtifacts: %v", err)
	}
	after := snapshotTree(t, flutterRoot)

	if len(before) != len(after) {
		t.Fatalf("a valid precache tree was modified: %d entries before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("clobbered real precache output at %s:\n before %+v\n after  %+v", before[i].rel, before[i], after[i])
		}
	}
	// The real binary must still be the real one, not ripley's cached engine.
	data, err := os.ReadFile(filepath.Join(podhelperEngineDir(flutterRoot, "ios"), "ios-arm64", "Flutter.framework", "Flutter"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "REAL-DEVICE-ios" {
		t.Errorf("device slice binary is %q, want the untouched precache binary", data)
	}
}

func TestEnsureFlutterIOSArtifactsStagesOnlyTheMissingDir(t *testing.T) {
	tc := fakeCachedXCFramework(t)
	flutterRoot := t.TempDir()

	// Half-populated cache: a valid `ios`, no `ios-release`. This is the shape
	// that still breaks every non-debug configuration in podhelper.rb.
	valid := podhelperEngineDir(flutterRoot, "ios")
	writeTestFile(t, filepath.Join(valid, "ios-arm64", "Flutter.framework", "Flutter"), "PRE-EXISTING")
	writeTestFile(t, filepath.Join(valid, "Info.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>AvailableLibraries</key><array>
<dict><key>LibraryIdentifier</key><string>ios-arm64</string><key>LibraryPath</key><string>Flutter.framework</string><key>SupportedArchitectures</key><array><string>arm64</string></array><key>SupportedPlatform</key><string>ios</string></dict>
</array><key>CFBundlePackageType</key><string>XFWK</string></dict></plist>`)
	before := snapshotTree(t, valid)

	if err := EnsureFlutterIOSArtifacts(tc, flutterRoot); err != nil {
		t.Fatalf("EnsureFlutterIOSArtifacts: %v", err)
	}

	after := snapshotTree(t, valid)
	if len(before) != len(after) {
		t.Fatalf("the already-valid ios dir was modified: %d entries before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("touched already-valid %s:\n before %+v\n after  %+v", before[i].rel, before[i], after[i])
		}
	}
	if _, _, err := selectXCFrameworkSlice(podhelperEngineDir(flutterRoot, "ios-release")); err != nil {
		t.Errorf("ios-release was not staged: %v", err)
	}
}

func TestEnsureFlutterIOSArtifactsRepairsMissingInfoPlist(t *testing.T) {
	tc := fakeCachedXCFramework(t)
	flutterRoot := t.TempDir()

	// Slice present, top-level Info.plist absent: an xcframework in this state
	// is invalid, and it is the state the earlier manual workaround produced
	// before the plist was synthesized by hand.
	dest := podhelperEngineDir(flutterRoot, "ios")
	writeTestFile(t, filepath.Join(dest, "ios-arm64", "Flutter.framework", "Flutter"), "PRE-EXISTING")

	if err := EnsureFlutterIOSArtifacts(tc, flutterRoot); err != nil {
		t.Fatalf("EnsureFlutterIOSArtifacts: %v", err)
	}

	if _, _, err := selectXCFrameworkSlice(dest); err != nil {
		t.Fatalf("Info.plist was not repaired: %v", err)
	}
	// The pre-existing slice must be described, not replaced.
	data, err := os.ReadFile(filepath.Join(dest, "ios-arm64", "Flutter.framework", "Flutter"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PRE-EXISTING" {
		t.Errorf("slice binary is %q, want the pre-existing one", data)
	}
}

func TestEnsureFlutterIOSArtifactsReplacesBrokenSliceLink(t *testing.T) {
	tc := fakeCachedXCFramework(t)
	flutterRoot := t.TempDir()

	// An interrupted stage, or a cache moved out from under a previous link.
	dest := podhelperEngineDir(flutterRoot, "ios")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dest, "ios-arm64")); err != nil {
		t.Fatal(err)
	}

	if err := EnsureFlutterIOSArtifacts(tc, flutterRoot); err != nil {
		t.Fatalf("EnsureFlutterIOSArtifacts: %v", err)
	}
	if _, _, err := selectXCFrameworkSlice(dest); err != nil {
		t.Fatalf("broken link was not repaired: %v", err)
	}
}

func TestEnsureFlutterIOSArtifactsErrorsWithoutCachedXCFramework(t *testing.T) {
	// A toolchain root that was never populated with the iOS engine artifact.
	tc := toolchain.Toolchain{Root: t.TempDir()}
	flutterRoot := t.TempDir()

	err := EnsureFlutterIOSArtifacts(tc, flutterRoot)
	if err == nil {
		t.Fatal("staging succeeded with no cached Flutter.xcframework")
	}
	if !strings.Contains(err.Error(), tc.FlutterXCFramework()) {
		t.Errorf("error does not name the missing source: %v", err)
	}
	if !strings.Contains(err.Error(), "ripley toolchain fetch") {
		t.Errorf("error does not say how to recover: %v", err)
	}
	// Nothing half-staged: podhelper.rb must still fail loudly rather than
	// walk into an empty directory.
	if dirExists(podhelperEngineDir(flutterRoot, "ios")) {
		t.Error("an empty ios/Flutter.xcframework was left behind")
	}
}
