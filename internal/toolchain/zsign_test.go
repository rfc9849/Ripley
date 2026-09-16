package toolchain

import (
	"strings"
	"testing"
)

func TestZsignReleaseNamingAndChecksum(t *testing.T) {
	tests := []struct {
		arch      string
		wantAsset string
		wantSHA   string
	}{
		{
			arch:      "x64",
			wantAsset: "zsign-linux-x86_64.tar.gz",
			wantSHA:   "65873b256b8902715cc81c8c467e9dddb315a2b484c6d0f5fb2031bad819d8ba",
		},
		{
			arch:      "arm64",
			wantAsset: "zsign-linux-aarch64.tar.gz",
			wantSHA:   "810e9115fffbbfcdca37ed09321a4db5ddcb85135542e38db7fcd74cb7d0e2c6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			url, sha, err := zsignRelease(tt.arch)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(url, "/v"+zsignVersion+"/"+tt.wantAsset) {
				t.Fatalf("url = %q, want asset %q", url, tt.wantAsset)
			}
			if sha != tt.wantSHA {
				t.Fatalf("sha256 = %q, want %q", sha, tt.wantSHA)
			}
		})
	}
}

func TestZsignReleaseRejectsUnsupportedHost(t *testing.T) {
	if _, _, err := zsignRelease("riscv64"); err == nil {
		t.Fatal("expected unsupported architecture error")
	}
}
