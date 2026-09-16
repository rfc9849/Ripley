package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Prebuilt gen_snapshot distribution.
//
// Building the cross `gen_snapshot` from Dart source takes ~30 minutes on a
// 4-core machine (see BuildGenSnapshot). Releases publish the resulting
// Linux host ELF (x64 or arm64) so users can skip that build. The release layout is:
//
//	tag:   gen-snapshot-dart<dartVersion>            e.g. gen-snapshot-dart3.13.3
//	asset: gen_snapshot_ios_arm64-dart<dartVersion>-linux-<arch>
//	       gen_snapshot_ios_arm64-dart<dartVersion>-linux-<arch>.sha256
//	       gen_snapshot_ios_arm64-dart<dartVersion>-linux-<arch>.LICENSES.txt
//
// where <arch> is the *host* architecture running gen_snapshot ("x64" or
// "arm64"); the emitted Mach-O is always ios-arm64.
const genSnapshotFetchTimeout = 10 * time.Minute

// defaultGenSnapshotRepo is injected into official release binaries with
// -ldflags "-X ripley/internal/toolchain.defaultGenSnapshotRepo=owner/repo".
// Source builds intentionally leave it empty rather than guessing a repository.
var defaultGenSnapshotRepo string

var sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// GenSnapshotAssetName returns the release asset file name for a Dart version
// and host architecture. The workflow that publishes the asset and
// FetchPrebuiltGenSnapshot must agree on this name.
func GenSnapshotAssetName(dartVersion, arch string) string {
	return fmt.Sprintf("gen_snapshot_ios_arm64-dart%s-linux-%s", dartVersion, arch)
}

// GenSnapshotReleaseTag returns the git tag of the release holding the asset.
func GenSnapshotReleaseTag(dartVersion string) string {
	return "gen-snapshot-dart" + dartVersion
}

func (t Toolchain) genSnapshotArch() string {
	if t.Arch != "" {
		return t.Arch
	}
	return hostArch()
}

// genSnapshotURL is the download URL for this toolchain's prebuilt binary.
// RIPLEY_GEN_SNAPSHOT_URL overrides it wholesale; RIPLEY_GEN_SNAPSHOT_REPO overrides
// only the owner/repo of the default GitHub Releases layout.
func (t Toolchain) genSnapshotURL() string {
	if url := os.Getenv("RIPLEY_GEN_SNAPSHOT_URL"); url != "" {
		return url
	}
	repo := os.Getenv("RIPLEY_GEN_SNAPSHOT_REPO")
	if repo == "" {
		repo = defaultGenSnapshotRepo
	}
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s",
		repo, GenSnapshotReleaseTag(t.DartVersion),
		GenSnapshotAssetName(t.DartVersion, t.genSnapshotArch()))
}

// FetchPrebuiltGenSnapshot downloads the published cross gen_snapshot for this
// toolchain's Dart version, verifies its SHA-256, and installs it at
// t.GenSnapshot() with mode 0755. It is a no-op when the installed binary
// already matches the expected checksum. The install is atomic: the download
// lands in a temp file next to the target and is renamed only after the
// checksum verifies.
func (t Toolchain) FetchPrebuiltGenSnapshot() error {
	if t.genSnapshotURL() == "" {
		return fmt.Errorf("prebuilt gen_snapshot repository is not configured; set RIPLEY_GEN_SNAPSHOT_REPO=owner/ripley or run `ripley toolchain build`")
	}
	url := t.genSnapshotURL()
	expected, err := t.expectedGenSnapshotSHA256(url)
	if err != nil {
		return err
	}

	if fileExists(t.GenSnapshot()) {
		have, err := sha256File(t.GenSnapshot())
		if err != nil {
			return err
		}
		if have == expected {
			if err := t.ensureGenSnapshotLicenses(url); err != nil {
				return err
			}
			fmt.Printf("  gen_snapshot_ios_arm64 up to date (sha256 %s)\n", expected[:12])
			return os.Chmod(t.GenSnapshot(), 0o755)
		}
	}

	if err := os.MkdirAll(t.Root, 0o755); err != nil {
		return err
	}
	fmt.Printf("  download %s\n", url)
	body, err := t.getGenSnapshotAsset(url)
	if err != nil {
		return err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(t.Root, "gen_snapshot_ios_arm64.download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, hasher), body)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return closeErr
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != expected {
		os.Remove(tmpName)
		return fmt.Errorf("gen_snapshot checksum mismatch for %s: expected %s, got %s", url, expected, got)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, t.GenSnapshot()); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := t.ensureGenSnapshotLicenses(url); err != nil {
		return err
	}
	fmt.Printf("  installed %s (sha256 %s)\n", t.GenSnapshot(), got)
	return nil
}

func (t Toolchain) ensureGenSnapshotLicenses(url string) error {
	if fileExists(t.GenSnapshotLicenses()) {
		return nil
	}
	licensesURL := url + ".LICENSES.txt"
	body, err := t.getGenSnapshotAsset(licensesURL)
	if err != nil {
		return fmt.Errorf("fetch gen_snapshot license notices: %w", err)
	}
	defer body.Close()
	if err := os.MkdirAll(t.Root, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(t.Root, "gen_snapshot_ios_arm64.LICENSES.download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, copyErr := io.Copy(tmp, io.LimitReader(body, 32<<20))
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("download %s: %w", licensesURL, copyErr)
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return closeErr
	}
	if err := os.Rename(tmpName, t.GenSnapshotLicenses()); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// expectedGenSnapshotSHA256 resolves the expected checksum from
// RIPLEY_GEN_SNAPSHOT_SHA256 or from the sibling "<asset>.sha256" file.
func (t Toolchain) expectedGenSnapshotSHA256(url string) (string, error) {
	if sum := os.Getenv("RIPLEY_GEN_SNAPSHOT_SHA256"); sum != "" {
		return parseSHA256Digest(strings.TrimSpace(sum), "RIPLEY_GEN_SNAPSHOT_SHA256")
	}
	sumURL := url + ".sha256"
	body, err := t.getGenSnapshotAsset(sumURL)
	if err != nil {
		return "", err
	}
	defer body.Close()
	// A sha256sum line is at most "<64 hex>  <name>\n"; cap the read.
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", sumURL, err)
	}
	return parseSHA256Digest(string(data), sumURL)
}

// parseSHA256Digest accepts a bare digest or a `sha256sum` output line.
func parseSHA256Digest(text, source string) (string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", fmt.Errorf("%s: empty sha256 digest", source)
	}
	digest := strings.ToLower(fields[0])
	if !sha256HexRE.MatchString(digest) {
		return "", fmt.Errorf("%s: %q is not a sha256 digest", source, fields[0])
	}
	return digest, nil
}

func (t Toolchain) getGenSnapshotAsset(url string) (io.ReadCloser, error) {
	client := &http.Client{Timeout: genSnapshotFetchTimeout}
	response, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		if strings.HasSuffix(url, ".LICENSES.txt") {
			return nil, fmt.Errorf("prebuilt gen_snapshot for Dart %s is missing its redistribution notices", t.DartVersion)
		}
		return nil, fmt.Errorf("no prebuilt gen_snapshot published for Dart %s; run `ripley toolchain build`", t.DartVersion)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, fmt.Errorf("download %s: %s", url, response.Status)
	}
	return response.Body, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
