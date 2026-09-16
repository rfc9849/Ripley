package toolchain

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func writeELFHeader(t *testing.T, path string, machine uint16) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 64)
	copy(header[:4], []byte("\x7fELF"))
	header[4] = 2 // ELFCLASS64
	header[5] = 1 // little endian
	header[6] = 1 // current version
	binary.LittleEndian.PutUint16(header[16:18], 3)
	binary.LittleEndian.PutUint16(header[18:20], machine)
	if err := os.WriteFile(path, header, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureFrontendServerHostArchReplacesForeignEngineSnapshot(t *testing.T) {
	root := t.TempDir()
	tc := Toolchain{Root: root, Arch: "arm64"}
	writeELFHeader(t, tc.FrontendServer(), elfMachineX8664)
	fallback := filepath.Join(tc.DartSDK(), "bin", "snapshots", "frontend_server_aot.dart.snapshot")
	writeELFHeader(t, fallback, elfMachineAArch64)
	if err := tc.ensureFrontendServerHostArch(); err != nil {
		t.Fatal(err)
	}
	if got, err := elfHostArch(tc.FrontendServer()); err != nil || got != "arm64" {
		t.Fatalf("frontend server arch = %q, err=%v", got, err)
	}
}

func TestEnsureFrontendServerHostArchKeepsMatchingEngineSnapshot(t *testing.T) {
	root := t.TempDir()
	tc := Toolchain{Root: root, Arch: "x64"}
	writeELFHeader(t, tc.FrontendServer(), elfMachineX8664)
	before, err := os.ReadFile(tc.FrontendServer())
	if err != nil {
		t.Fatal(err)
	}
	if err := tc.ensureFrontendServerHostArch(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(tc.FrontendServer())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("matching frontend server was unexpectedly replaced")
	}
}
