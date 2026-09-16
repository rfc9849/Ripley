package ios

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ripley/internal/toolchain"
)

func TestSwiftExecutableRPathArgsSystemRuntimeFirst(t *testing.T) {
	got := swiftExecutableRPathArgs("@executable_path/Frameworks", "@executable_path/../../Frameworks")
	want := []string{
		"-rpath", "/usr/lib/swift",
		"-rpath", "@executable_path/Frameworks",
		"-rpath", "@executable_path/../../Frameworks",
	}
	if len(got) != len(want) {
		t.Fatalf("swift executable rpaths = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("swift executable rpaths = %#v, want %#v", got, want)
		}
	}
}

func TestSwiftRuntimeDependenciesFindsRPathSwiftLibraries(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "Runner")
	writeMachOWithDylibs(t, binary,
		"@rpath/libswift_Concurrency.dylib",
		"/usr/lib/swift/libswiftCore.dylib",
		"@rpath/Flutter.framework/Flutter",
	)
	if err := os.WriteFile(filepath.Join(root, "Info.plist"), []byte("not macho"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := swiftRuntimeDependencies(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "libswift_Concurrency.dylib" {
		t.Fatalf("swiftRuntimeDependencies = %v, want [libswift_Concurrency.dylib]", got)
	}
}

func TestEmbedSwiftRuntimeLibrariesDoesNotFollowRuntimeDependencies(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Runner.app")
	frameworks := filepath.Join(app, "Frameworks")
	if err := os.MkdirAll(frameworks, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMachOWithDylibs(t, filepath.Join(app, "Runner"), "@rpath/libswift_Concurrency.dylib")
	writeMachOWithDylibs(t, filepath.Join(frameworks, "libswiftCore.dylib")) // stale from an older ripley build

	libs := t.TempDir()
	writeMachOWithDylibs(t, filepath.Join(libs, "libswift_Concurrency.dylib"), "@rpath/libswiftCore.dylib")
	writeMachOWithDylibs(t, filepath.Join(libs, "libswiftCore.dylib"))
	t.Setenv("RIPLEY_SWIFT_IOS_LIBS", libs)
	t.Setenv("RIPLEY_SWIFT", filepath.Join(t.TempDir(), "unused-swift-toolchain"))

	if err := EmbedSwiftRuntimeLibraries(toolchain.Toolchain{}, app); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(frameworks, "libswift_Concurrency.dylib")); err != nil {
		t.Fatalf("back-deployment Swift runtime was not embedded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(frameworks, "libswiftCore.dylib")); !os.IsNotExist(err) {
		t.Fatalf("system Swift runtime must not be embedded transitively; stat err=%v", err)
	}
}

func TestSwiftRuntimeDependenciesIgnorePreviouslyEmbeddedRuntime(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Runner.app")
	frameworks := filepath.Join(app, "Frameworks")
	if err := os.MkdirAll(frameworks, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMachOWithDylibs(t, filepath.Join(app, "Runner"), "@rpath/libswift_Concurrency.dylib")
	writeMachOWithDylibs(t, filepath.Join(frameworks, "libswift_Concurrency.dylib"), "@rpath/libswiftCore.dylib")

	got, err := swiftRuntimeDependencies(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "libswift_Concurrency.dylib" {
		t.Fatalf("swiftRuntimeDependencies = %v, want only libswift_Concurrency.dylib", got)
	}
}

func TestSwiftRuntimeLibraryUsesProvisionedIOSPayload(t *testing.T) {
	libs := t.TempDir()
	want := filepath.Join(libs, "libswift_Concurrency.dylib")
	if err := os.WriteFile(want, []byte("runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPLEY_SWIFT_IOS_LIBS", libs)

	got, err := swiftRuntimeLibrary(toolchain.Toolchain{}, "libswift_Concurrency.dylib")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("swiftRuntimeLibrary = %q, want %q", got, want)
	}
}

func TestSwiftRuntimeLibraryMissingExplainsPayload(t *testing.T) {
	libs := t.TempDir()
	t.Setenv("RIPLEY_SWIFT_IOS_LIBS", libs)
	t.Setenv("RIPLEY_SWIFT", filepath.Join(t.TempDir(), "no-swift-runtime"))

	_, err := swiftRuntimeLibrary(toolchain.Toolchain{}, "libswift_Concurrency.dylib")
	if err == nil {
		t.Fatal("expected missing runtime error")
	}
	if !strings.Contains(err.Error(), "full Xcode iphoneos Swift runtime payload") {
		t.Fatalf("missing runtime error = %q", err)
	}
}

func writeMachOWithDylibs(t *testing.T, path string, dylibs ...string) {
	t.Helper()
	const (
		mhMagic64   = uint32(0xfeedfacf)
		cpuArm64    = uint32(0x0100000c)
		mhExecute   = uint32(2)
		lcLoadDylib = uint32(0x80000018) // LC_LOAD_WEAK_DYLIB
		headerSize  = 32
		dylibHeader = 24
	)

	commands := make([][]byte, 0, len(dylibs))
	var sizeofcmds uint32
	for _, name := range dylibs {
		size := dylibHeader + len(name) + 1
		size = (size + 7) &^ 7
		cmd := make([]byte, size)
		binary.LittleEndian.PutUint32(cmd[0:4], lcLoadDylib)
		binary.LittleEndian.PutUint32(cmd[4:8], uint32(size))
		binary.LittleEndian.PutUint32(cmd[8:12], dylibHeader)
		copy(cmd[dylibHeader:], name)
		commands = append(commands, cmd)
		sizeofcmds += uint32(size)
	}

	data := make([]byte, headerSize, headerSize+int(sizeofcmds))
	binary.LittleEndian.PutUint32(data[0:4], mhMagic64)
	binary.LittleEndian.PutUint32(data[4:8], cpuArm64)
	binary.LittleEndian.PutUint32(data[8:12], 0)
	binary.LittleEndian.PutUint32(data[12:16], mhExecute)
	binary.LittleEndian.PutUint32(data[16:20], uint32(len(commands)))
	binary.LittleEndian.PutUint32(data[20:24], sizeofcmds)
	for _, cmd := range commands {
		data = append(data, cmd...)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
}
