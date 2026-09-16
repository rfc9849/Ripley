package toolchain

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	elfMachineX8664   = 62
	elfMachineAArch64 = 183
)

// ensureFrontendServerHostArch fixes a Flutter engine packaging quirk observed
// on Linux arm64: linux-arm64/artifacts.zip can contain an x86_64
// frontend_server_aot.dart.snapshot. The Dart SDK bundled for the same host
// contains a matching frontend server snapshot, so use it as a verified host
// fallback instead of attempting to execute a foreign ELF image.
func (t Toolchain) ensureFrontendServerHostArch() error {
	expected := t.Arch
	if expected == "" {
		expected = hostArch()
	}
	if got, err := elfHostArch(t.FrontendServer()); err == nil && got == expected {
		return nil
	}

	fallback := filepath.Join(t.DartSDK(), "bin", "snapshots", "frontend_server_aot.dart.snapshot")
	got, err := elfHostArch(fallback)
	if err != nil {
		return fmt.Errorf("frontend server for Linux %s is unusable and fallback %s cannot be inspected: %w", expected, fallback, err)
	}
	if got != expected {
		return fmt.Errorf("frontend server for Linux %s is unusable and Dart SDK fallback is %s", expected, got)
	}
	if err := copyFileAtomic(fallback, t.FrontendServer(), 0o644); err != nil {
		return fmt.Errorf("install %s frontend server fallback: %w", expected, err)
	}
	return nil
}

func elfHostArch(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", err
	}
	if string(header[:4]) != "\x7fELF" {
		return "", fmt.Errorf("%s is not an ELF file", path)
	}
	var order binary.ByteOrder
	switch header[5] {
	case 1:
		order = binary.LittleEndian
	case 2:
		order = binary.BigEndian
	default:
		return "", fmt.Errorf("%s has unsupported ELF byte order %d", path, header[5])
	}
	switch order.Uint16(header[18:20]) {
	case elfMachineX8664:
		return "x64", nil
	case elfMachineAArch64:
		return "arm64", nil
	default:
		return "", fmt.Errorf("%s has unsupported ELF machine %d", path, order.Uint16(header[18:20]))
	}
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".host-artifact-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
