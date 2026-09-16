package toolchain

import (
	"fmt"
	"os"
	"path/filepath"
)

const zsignVersion = "1.1.1"

func zsignRelease(arch string) (url, sha256 string, err error) {
	var assetArch string
	switch arch {
	case "x64":
		assetArch = "x86_64"
		sha256 = "65873b256b8902715cc81c8c467e9dddb315a2b484c6d0f5fb2031bad819d8ba"
	case "arm64":
		assetArch = "aarch64"
		sha256 = "810e9115fffbbfcdca37ed09321a4db5ddcb85135542e38db7fcd74cb7d0e2c6"
	default:
		return "", "", fmt.Errorf("unsupported Linux host architecture %q for zsign", arch)
	}
	return fmt.Sprintf("https://github.com/zhlynn/zsign/releases/download/v%s/zsign-linux-%s.tar.gz", zsignVersion, assetArch), sha256, nil
}

func (t Toolchain) ensureZsign() error {
	dst := filepath.Join(t.ToolsetBin(), "zsign")
	if fileExists(dst) {
		return nil
	}
	url, expected, err := zsignRelease(t.Arch)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.ToolsetBin(), 0o755); err != nil {
		return err
	}
	tarball := filepath.Join(t.Root, "zsign.tar.gz")
	if err := download(url, tarball); err != nil {
		return err
	}
	defer os.Remove(tarball)
	got, err := sha256File(tarball)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("zsign checksum mismatch for %s: expected %s, got %s", url, expected, got)
	}
	if err := run("", nil, "tar", "xzf", tarball, "-C", t.ToolsetBin(), "zsign"); err != nil {
		return err
	}
	if !fileExists(dst) {
		return fmt.Errorf("zsign archive %s did not contain zsign", url)
	}
	return os.Chmod(dst, 0o755)
}
