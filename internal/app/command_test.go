package app

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	callErr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data), callErr
}

func TestMainWithoutArgsPrintsHelp(t *testing.T) {
	out, err := captureStdout(t, func() error { return Main(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "build ios") || !strings.Contains(out, "device") {
		t.Fatalf("unexpected help output:\n%s", out)
	}
}

func TestBuildHelpDoesNotRequireToolchain(t *testing.T) {
	out, err := captureStdout(t, func() error { return Main([]string{"build", "ios", "--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Usage: ripley build ios [options]", "-no-sign", "-bundle-id", "-target"} {
		if !strings.Contains(out, want) {
			t.Fatalf("build help missing %q:\n%s", want, out)
		}
	}
}

func TestDeviceHelpDoesNotConnectToDevice(t *testing.T) {
	out, err := captureStdout(t, func() error { return Main([]string{"device", "--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "install <app.ipa>") || !strings.Contains(out, "logs") {
		t.Fatalf("unexpected device help:\n%s", out)
	}
}

func TestUnknownCommandPointsToHelp(t *testing.T) {
	err := Main([]string{"wat"})
	if err == nil {
		t.Fatal("unknown command must fail")
	}
	if !strings.Contains(err.Error(), "Run 'ripley --help' for usage") {
		t.Fatalf("error has no help hint: %v", err)
	}
}

func TestSigningProfileErrorExplainsRecovery(t *testing.T) {
	err := signingProfileError(errors.New("no profile matches"), "com.example.app", "/tmp/signing")
	text := err.Error()
	for _, want := range []string{"com.example.app", "--prov <profile>", "--no-sign", "--bundle-id"} {
		if !strings.Contains(text, want) {
			t.Fatalf("signing hint missing %q: %s", want, text)
		}
	}
}

func TestToolchainBuildHelpDoesNotRequireFlutter(t *testing.T) {
	out, err := captureStdout(t, func() error { return Main([]string{"toolchain", "build", "--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Usage: ripley toolchain build", "-dart-version", "-output", "-workdir"} {
		if !strings.Contains(out, want) {
			t.Fatalf("toolchain build help missing %q:\n%s", want, out)
		}
	}
}

func TestToolchainBuildExplicitDartVersionRequiresOutput(t *testing.T) {
	err := Main([]string{"toolchain", "build", "--dart-version", "3.13.3"})
	if err == nil || !strings.Contains(err.Error(), "--dart-version requires --output") {
		t.Fatalf("error = %v, want explicit-output guidance", err)
	}
}
