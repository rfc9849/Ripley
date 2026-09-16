package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// genSnapshotServer serves an asset plus its sibling checksum and redistribution
// notices the way a GitHub release does. digestOverride, when non-empty, is
// published instead of the real digest so checksum enforcement can be exercised.
func genSnapshotServer(t *testing.T, assetName string, payload []byte, digestOverride string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	if digestOverride != "" {
		digest = digestOverride
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	})
	mux.HandleFunc("/"+assetName+".sha256", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", digest, assetName)
	})
	mux.HandleFunc("/"+assetName+".LICENSES.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Dart SDK redistribution notices")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func prebuiltToolchain(t *testing.T) Toolchain {
	t.Helper()
	root := t.TempDir()
	t.Setenv("RIPLEY_HOME", root)
	t.Setenv("RIPLEY_GEN_SNAPSHOT_SHA256", "")
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", "")
	return NewToolchain("06a2e2a110089dff50fe635cffd2a61e1b24fbcd", "3.13.3")
}

func TestPrebuiltGenSnapshotInstalls(t *testing.T) {
	tc := prebuiltToolchain(t)
	payload := []byte("\x7fELF fake gen_snapshot_ios_arm64")
	asset := GenSnapshotAssetName(tc.DartVersion, tc.genSnapshotArch())
	srv := genSnapshotServer(t, asset, payload, "")
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", srv.URL+"/"+asset)

	if err := tc.FetchPrebuiltGenSnapshot(); err != nil {
		t.Fatalf("FetchPrebuiltGenSnapshot: %v", err)
	}

	got, err := os.ReadFile(tc.GenSnapshot())
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed content = %q, want %q", got, payload)
	}
	if notices, err := os.ReadFile(tc.GenSnapshotLicenses()); err != nil || !strings.Contains(string(notices), "Dart SDK redistribution notices") {
		t.Fatalf("installed redistribution notices = %q, err=%v", notices, err)
	}
	st, err := os.Stat(tc.GenSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o755 {
		t.Fatalf("installed mode = %v, want 0755", perm)
	}

	// No download temp files may survive a successful install.
	entries, err := os.ReadDir(tc.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".download-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestPrebuiltGenSnapshotRejectsChecksumMismatch(t *testing.T) {
	tc := prebuiltToolchain(t)
	asset := GenSnapshotAssetName(tc.DartVersion, tc.genSnapshotArch())
	srv := genSnapshotServer(t, asset, []byte("payload"), strings.Repeat("ab", 32))
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", srv.URL+"/"+asset)

	err := tc.FetchPrebuiltGenSnapshot()
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	// A corrupt download must never be installed.
	if fileExists(tc.GenSnapshot()) {
		t.Fatal("gen_snapshot installed despite checksum mismatch")
	}
	entries, _ := os.ReadDir(tc.Root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".download-") {
			t.Fatalf("leftover temp file %s after mismatch", e.Name())
		}
	}
}

func TestPrebuiltGenSnapshotSkipsWhenUpToDate(t *testing.T) {
	tc := prebuiltToolchain(t)
	payload := []byte("already here")
	asset := GenSnapshotAssetName(tc.DartVersion, tc.genSnapshotArch())

	var assetHits int
	sum := sha256.Sum256(payload)
	mux := http.NewServeMux()
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, r *http.Request) {
		assetHits++
		w.Write(payload)
	})
	mux.HandleFunc("/"+asset+".sha256", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
	})
	mux.HandleFunc("/"+asset+".LICENSES.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Dart SDK redistribution notices")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", srv.URL+"/"+asset)

	if err := os.MkdirAll(tc.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tc.GenSnapshot(), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := tc.FetchPrebuiltGenSnapshot(); err != nil {
		t.Fatalf("FetchPrebuiltGenSnapshot: %v", err)
	}
	if assetHits != 0 {
		t.Fatalf("asset fetched %d times, want 0 (matching binary already installed)", assetHits)
	}
	st, err := os.Stat(tc.GenSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o755 {
		t.Fatalf("mode = %v, want the skip path to still ensure 0755", perm)
	}
}

func TestPrebuiltGenSnapshotMissingReleaseMessage(t *testing.T) {
	tc := prebuiltToolchain(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", srv.URL+"/missing")

	err := tc.FetchPrebuiltGenSnapshot()
	if err == nil {
		t.Fatal("expected an error for an unpublished Dart version")
	}
	want := "no prebuilt gen_snapshot published for Dart 3.13.3; run `ripley toolchain build`"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestPrebuiltGenSnapshotEnvChecksumSkipsSidecar(t *testing.T) {
	tc := prebuiltToolchain(t)
	payload := []byte("env checksum path")
	sum := sha256.Sum256(payload)
	asset := GenSnapshotAssetName(tc.DartVersion, tc.genSnapshotArch())

	// The checksum comes from the environment, but redistribution notices are
	// still fetched alongside the asset.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + asset:
			w.Write(payload)
		case "/" + asset + ".LICENSES.txt":
			fmt.Fprintln(w, "Dart SDK redistribution notices")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", srv.URL+"/"+asset)
	t.Setenv("RIPLEY_GEN_SNAPSHOT_SHA256", hex.EncodeToString(sum[:]))

	if err := tc.FetchPrebuiltGenSnapshot(); err != nil {
		t.Fatalf("FetchPrebuiltGenSnapshot: %v", err)
	}
	got, err := os.ReadFile(tc.GenSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed content = %q, want %q", got, payload)
	}
}

func TestPrebuiltGenSnapshotRejectsBadDigestSidecar(t *testing.T) {
	tc := prebuiltToolchain(t)
	asset := GenSnapshotAssetName(tc.DartVersion, tc.genSnapshotArch())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintln(w, "<!doctype html>not a digest")
			return
		}
		w.Write([]byte("payload"))
	}))
	defer srv.Close()
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", srv.URL+"/"+asset)

	err := tc.FetchPrebuiltGenSnapshot()
	if err == nil {
		t.Fatal("expected an error for a non-digest sidecar")
	}
	if !strings.Contains(err.Error(), "is not a sha256 digest") {
		t.Fatalf("error = %v, want a digest parse failure", err)
	}
}

func TestGenSnapshotReleaseNaming(t *testing.T) {
	if got := GenSnapshotAssetName("3.13.3", "x64"); got != "gen_snapshot_ios_arm64-dart3.13.3-linux-x64" {
		t.Fatalf("asset name = %q", got)
	}
	if got := GenSnapshotAssetName("3.13.3", "arm64"); got != "gen_snapshot_ios_arm64-dart3.13.3-linux-arm64" {
		t.Fatalf("arm64 asset name = %q", got)
	}
	if got := GenSnapshotReleaseTag("3.13.3"); got != "gen-snapshot-dart3.13.3" {
		t.Fatalf("release tag = %q", got)
	}

	root := t.TempDir()
	t.Setenv("RIPLEY_HOME", root)
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", "")
	t.Setenv("RIPLEY_GEN_SNAPSHOT_REPO", "someone/ripley")
	tc := NewToolchain("engine", "3.14.0")
	want := "https://github.com/someone/ripley/releases/download/gen-snapshot-dart3.14.0/" +
		GenSnapshotAssetName("3.14.0", tc.genSnapshotArch())
	if got := tc.genSnapshotURL(); got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", "https://example.invalid/custom")
	if got := tc.genSnapshotURL(); got != "https://example.invalid/custom" {
		t.Fatalf("RIPLEY_GEN_SNAPSHOT_URL ignored: %q", got)
	}
}

func TestPrebuiltGenSnapshotReplacesStaleBinary(t *testing.T) {
	tc := prebuiltToolchain(t)
	payload := []byte("fresh build")
	asset := GenSnapshotAssetName(tc.DartVersion, tc.genSnapshotArch())
	srv := genSnapshotServer(t, asset, payload, "")
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", srv.URL+"/"+asset)

	if err := os.MkdirAll(tc.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tc.GenSnapshot(), []byte("stale binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tc.FetchPrebuiltGenSnapshot(); err != nil {
		t.Fatalf("FetchPrebuiltGenSnapshot: %v", err)
	}
	got, err := os.ReadFile(tc.GenSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("stale binary not replaced: %q", got)
	}
}

func TestPrebuiltGenSnapshotPathInsideToolchainRoot(t *testing.T) {
	tc := prebuiltToolchain(t)
	if want := filepath.Join(tc.Root, "gen_snapshot_ios_arm64"); tc.GenSnapshot() != want {
		t.Fatalf("GenSnapshot() = %q, want %q", tc.GenSnapshot(), want)
	}
}

func TestPrebuiltGenSnapshotRequiresRepositoryWhenNoOverride(t *testing.T) {
	tc := prebuiltToolchain(t)
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", "")
	t.Setenv("RIPLEY_GEN_SNAPSHOT_REPO", "")
	err := tc.FetchPrebuiltGenSnapshot()
	if err == nil || !strings.Contains(err.Error(), "RIPLEY_GEN_SNAPSHOT_REPO") || !strings.Contains(err.Error(), "ripley toolchain build") {
		t.Fatalf("missing repository error = %v", err)
	}
}

func TestGenSnapshotURLUsesEmbeddedReleaseRepository(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RIPLEY_HOME", root)
	t.Setenv("RIPLEY_GEN_SNAPSHOT_URL", "")
	t.Setenv("RIPLEY_GEN_SNAPSHOT_REPO", "")
	old := defaultGenSnapshotRepo
	defaultGenSnapshotRepo = "example/ripley"
	t.Cleanup(func() { defaultGenSnapshotRepo = old })

	tc := NewToolchain("engine", "3.14.0")
	want := "https://github.com/example/ripley/releases/download/gen-snapshot-dart3.14.0/" +
		GenSnapshotAssetName("3.14.0", tc.genSnapshotArch())
	if got := tc.genSnapshotURL(); got != want {
		t.Fatalf("embedded repository URL = %q, want %q", got, want)
	}
}
