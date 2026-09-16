package ios

import (
	"debug/macho"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ripley/internal/toolchain"
)

// EmbedSwiftRuntimeLibraries mirrors Xcode's Swift stdlib embedding step for
// back-deployment libraries. Apple SDK stubs may deliberately rewrite a Swift
// runtime install name to @rpath when the deployment target predates the OS
// version that first shipped that runtime (for example libswift_Concurrency for
// iOS < 15). Those dylibs must live in <App>.app/Frameworks at runtime.
//
// We inspect the finished app and all of its nested Mach-O binaries rather than
// guessing from source language: Swift dependencies can arrive through CocoaPods,
// SwiftPM packages, native frameworks, or app extensions.
// swiftExecutableRPathArgs orders Swift runtime lookup the same way Xcode's
// executable link needs for back deployment. On a modern OS the system Swift
// runtime must win; the app's Frameworks directory is only a fallback for
// back-deployment dylibs absent from /usr/lib/swift. Reversing this order loads
// the bundled compatibility Concurrency runtime next to StoreKit's system
// Concurrency runtime and can crash async task allocation.
func swiftExecutableRPathArgs(fallbacks ...string) []string {
	out := []string{"-rpath", "/usr/lib/swift"}
	for _, path := range fallbacks {
		if path != "" {
			out = append(out, "-rpath", path)
		}
	}
	return out
}

func EmbedSwiftRuntimeLibraries(tc toolchain.Toolchain, app string) error {
	frameworks := filepath.Join(app, "Frameworks")
	if err := os.MkdirAll(frameworks, 0o755); err != nil {
		return err
	}
	// This directory is ripley's equivalent of Xcode's Embed Swift Standard
	// Libraries output. Recompute it on every build so a runtime copied by an
	// older build (notably libswiftCore from the former transitive-closure bug)
	// cannot survive after the dependency graph changes.
	stale, err := filepath.Glob(filepath.Join(frameworks, "libswift*.dylib"))
	if err != nil {
		return err
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale Swift runtime %s: %w", path, err)
		}
	}

	deps, err := swiftRuntimeDependencies(app)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		return nil
	}
	for _, name := range deps {
		dst := filepath.Join(frameworks, name)
		if fileExists(dst) {
			continue
		}
		src, err := swiftRuntimeLibrary(tc, name)
		if err != nil {
			return err
		}
		fmt.Printf("swift runtime: %s\n", name)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("embed Swift runtime %s: %w", name, err)
		}
	}
	return nil
}

func swiftRuntimeDependencies(root string) ([]string, error) {
	set := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, "libswift") && strings.HasSuffix(base, ".dylib") {
			return nil
		}
		libs, err := importedMachOLibraries(path)
		if err != nil {
			return fmt.Errorf("inspect Mach-O dependencies of %s: %w", path, err)
		}
		for _, name := range rpathSwiftRuntimeNames(libs) {
			set[name] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func rpathSwiftRuntimeNames(libs []string) []string {
	set := make(map[string]bool)
	for _, lib := range libs {
		if !strings.HasPrefix(lib, "@rpath/libswift_") || !strings.HasSuffix(lib, ".dylib") {
			continue
		}
		name := filepath.Base(lib)
		if name != "." && name != string(filepath.Separator) {
			set[name] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// importedMachOLibraries returns nil for ordinary non-Mach-O files. A malformed
// file that merely is not Mach-O is expected while walking an app bundle; a
// recognized thin/fat Mach-O with a corrupt load-command table is still
// reported by ImportedLibraries.
func importedMachOLibraries(path string) ([]string, error) {
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		return machOLibraries(f)
	}
	fat, err := macho.OpenFat(path)
	if err != nil {
		return nil, nil
	}
	defer fat.Close()
	set := make(map[string]bool)
	for _, arch := range fat.Arches {
		libs, err := machOLibraries(arch.File)
		if err != nil {
			return nil, err
		}
		for _, lib := range libs {
			set[lib] = true
		}
	}
	out := make([]string, 0, len(set))
	for lib := range set {
		out = append(out, lib)
	}
	sort.Strings(out)
	return out, nil
}

// debug/macho only promotes LC_LOAD_DYLIB to *macho.Dylib. Swift SDK stubs
// commonly emit LC_LOAD_WEAK_DYLIB for back-deployment runtimes, so inspect raw
// load commands as well. All dylib-family commands share dylib_command's
// uint32 name offset at byte 8.
func machOLibraries(f *macho.File) ([]string, error) {
	const (
		lcLoadDylib       = uint32(0x0000000c)
		lcLoadWeakDylib   = uint32(0x80000018)
		lcReexportDylib   = uint32(0x8000001f)
		lcLazyLoadDylib   = uint32(0x00000020)
		lcLoadUpwardDylib = uint32(0x80000023)
	)
	var out []string
	for _, load := range f.Loads {
		raw := load.Raw()
		if len(raw) < 12 {
			continue
		}
		cmd := f.ByteOrder.Uint32(raw[0:4])
		switch cmd {
		case lcLoadDylib, lcLoadWeakDylib, lcReexportDylib, lcLazyLoadDylib, lcLoadUpwardDylib:
		default:
			continue
		}
		nameOffset := f.ByteOrder.Uint32(raw[8:12])
		if nameOffset >= uint32(len(raw)) {
			return nil, fmt.Errorf("invalid dylib name offset %d in load command %#x", nameOffset, cmd)
		}
		nameBytes := raw[nameOffset:]
		if nul := strings.IndexByte(string(nameBytes), 0); nul >= 0 {
			nameBytes = nameBytes[:nul]
		}
		if len(nameBytes) > 0 {
			out = append(out, string(nameBytes))
		}
	}
	return out, nil
}

func swiftRuntimeLibrary(tc toolchain.Toolchain, name string) (string, error) {
	if filepath.Base(name) != name || !strings.HasPrefix(name, "libswift_") || !strings.HasSuffix(name, ".dylib") {
		return "", fmt.Errorf("invalid Swift runtime library name %q", name)
	}

	var roots []string
	if libs, err := tc.SwiftIOSLibs(); err == nil && libs != "" {
		roots = append(roots, libs)
	}
	if swiftRoot, err := tc.SwiftToolchainRoot(); err == nil && swiftRoot != "" {
		base := filepath.Join(swiftRoot, "usr", "lib")
		versioned, _ := filepath.Glob(filepath.Join(base, "swift*", "iphoneos"))
		sort.Sort(sort.Reverse(sort.StringSlice(versioned)))
		roots = append(roots, versioned...)
	}

	seen := make(map[string]bool)
	for _, root := range roots {
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		candidate := filepath.Join(root, name)
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("Swift back-deployment runtime %s is required but missing; provision the full Xcode iphoneos Swift runtime payload (XcodeDefault.xctoolchain/usr/lib/swift/iphoneos, including back-deploy dylibs) in %s or set RIPLEY_SWIFT_IOS_LIBS", name, filepath.Join(toolchain.RipleyHome(), "sdks", "iphoneos"))
}
