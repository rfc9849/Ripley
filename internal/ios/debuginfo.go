package ios

import (
	"debug/macho"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ripley/internal/toolchain"
)

// debugSymbolsFileName mirrors flutter_tools' AOTSnapshotter (see
// packages/flutter_tools/lib/src/base/build.dart): the debug/symbol file is
// named after the *target platform and architecture*, never after the product,
// because gen_snapshot always emits a snapshot called "app" and one build
// command may produce several architectures into the same directory.
//
//	final String archName = platform.getName(darwinArch: darwinArch); // ios-arm64
//	final debugFilename = 'app.$archName.symbols';
//
// ripley only targets arm64 devices, so the name is fixed.
const debugSymbolsFileName = "app.ios-arm64.symbols"

// DebugInfoOptions selects Dart-level debug information handling for the AOT
// snapshot, matching the `flutter build ios` flags of the same names.
type DebugInfoOptions struct {
	// SaveDebuggingInfo requests a Dart debugging-information file even when
	// SplitDebugInfo is empty; the file is then written next to the build
	// artifacts (the workDir passed to GenSnapshotDebugArgs). A non-empty
	// SplitDebugInfo implies SaveDebuggingInfo, exactly as in flutter_tools.
	SaveDebuggingInfo bool

	// SplitDebugInfo is the directory that receives the Dart program symbol
	// file. Set from `--split-debug-info=<dir>`.
	SplitDebugInfo string

	// Obfuscate renames Dart identifiers in the snapshot. Set from
	// `--obfuscate`. flutter_tools rejects this without SplitDebugInfo and so
	// does GenSnapshotDebugArgs, because the mapping needed to read a stack
	// trace back would otherwise be discarded.
	Obfuscate bool
}

// GenSnapshotDebugArgs returns the extra gen_snapshot arguments that implement
// opt, creating the symbol output directory when one is needed. workDir is the
// build directory used as the symbol destination when only SaveDebuggingInfo is
// set; productName only identifies the target in diagnostics, since the symbol
// file keeps Flutter's architecture-derived name so that `flutter symbolize`
// and any existing tooling see an identical layout.
//
// The returned arguments deliberately never include `--strip`: on Apple
// platforms flutter_tools strips the Mach-O *after* dSYM extraction (via
// StripMachO) so that the debug map survives long enough to build the dSYM.
// Passing `--strip` here would destroy the DWARF before dsymutil ever runs.
func GenSnapshotDebugArgs(opt DebugInfoOptions, workDir, productName string) ([]string, error) {
	target := strings.TrimSpace(productName)
	if target == "" {
		target = "the application"
	}

	splitDir := strings.TrimSpace(opt.SplitDebugInfo)
	if opt.Obfuscate && splitDir == "" {
		return nil, fmt.Errorf("%q can only be used in combination with %q (building %s)",
			"--obfuscate", "--split-debug-info", target)
	}

	dir := splitDir
	if dir == "" {
		if !opt.SaveDebuggingInfo {
			return nil, nil
		}
		if dir = strings.TrimSpace(workDir); dir == "" {
			return nil, fmt.Errorf("--save-debugging-info needs a build directory or %q (building %s)",
				"--split-debug-info", target)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create debug info directory %s: %w", dir, err)
	}

	// Flag names and order follow flutter_tools' AOTSnapshotter.build.
	args := []string{
		"--dwarf-stack-traces",
		"--resolve-dwarf-paths",
		"--save-debugging-info=" + filepath.Join(dir, debugSymbolsFileName),
	}
	if opt.Obfuscate {
		// gen_snapshot writes the identifier mapping into the
		// --save-debugging-info file; flutter_tools passes no separate
		// --save-obfuscation-map flag and neither do we.
		args = append(args, "--obfuscate")
	}
	return args, nil
}

// EmitDSYM extracts a dSYM bundle for machO into outDir and returns the bundle
// path. The bundle is named the way Xcode and flutter_tools name it: for a
// binary inside a framework the framework directory name is used
// (App.framework/App -> App.framework.dSYM), otherwise the binary's own name
// (Runner -> Runner.dSYM). The DWARF companion always lands at
// Contents/Resources/DWARF/<binary name>.
//
// gen_snapshot emits DWARF into its relocatable object (app.o) and leaves only
// a debug map of absolute N_OSO stabs in the linked Mach-O. dsymutil follows
// those stabs, so EmitDSYM MUST run before the intermediate object files are
// moved or deleted; dsymutil merely warns and exits 0 when they are gone, so
// the result is verified to carry a non-empty __debug_info section rather than
// trusted.
func EmitDSYM(tc toolchain.Toolchain, machO, outDir string) (string, error) {
	if !fileExists(machO) {
		return "", fmt.Errorf("dsymutil: %s does not exist", machO)
	}
	dsymutil, err := debugTool(tc, "dsymutil")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}

	binaryName := filepath.Base(machO)
	bundleName := binaryName + ".dSYM"
	if parent := filepath.Base(filepath.Dir(machO)); strings.HasSuffix(parent, ".framework") {
		bundleName = parent + ".dSYM"
	}
	bundle := filepath.Join(outDir, bundleName)
	if err := os.RemoveAll(bundle); err != nil {
		return "", err
	}

	if err := run("", nil, dsymutil, "-o", bundle, machO); err != nil {
		return "", err
	}

	dwarf := filepath.Join(bundle, "Contents", "Resources", "DWARF", binaryName)
	if !fileExists(dwarf) {
		return "", fmt.Errorf("dsymutil produced no DWARF companion at %s", dwarf)
	}
	size, err := machOSectionSize(dwarf, "__debug_info")
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", dwarf, err)
	}
	if size == 0 {
		return "", fmt.Errorf("dSYM %s carries no DWARF: the intermediate object files "+
			"referenced by %s's debug map were unavailable to dsymutil", bundle, machO)
	}
	return bundle, nil
}

// StripMachO removes local symbols and the debug map from a Mach-O in place,
// mirroring the `strip -x` that flutter_tools runs on iOS AOT snapshots. `-x`
// is what Apple's strip and llvm-strip both use to drop local symbols while
// keeping the exported ones a dylib needs; llvm-objcopy --strip-debug is not a
// substitute, it leaves the local symbol table behind.
//
// Ordering: run this AFTER EmitDSYM (stripping deletes the N_OSO debug map
// dsymutil needs) and BEFORE code signing (stripping rewrites the file, which
// would invalidate any signature already attached).
func StripMachO(tc toolchain.Toolchain, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	strip, err := debugTool(tc, "llvm-strip", "strip")
	if err != nil {
		return err
	}
	if err := run("", nil, strip, "-x", path); err != nil {
		return err
	}
	// llvm-strip writes a fresh file; keep the original permissions so an
	// executable host binary stays executable.
	if err := os.Chmod(path, info.Mode().Perm()); err != nil {
		return err
	}
	size, err := machOSectionSize(path, "__text")
	if err != nil {
		return fmt.Errorf("verify stripped %s: %w", path, err)
	}
	if size == 0 {
		return fmt.Errorf("stripping %s produced a Mach-O without executable code", path)
	}
	return nil
}

// debugTool resolves a binary from the engine toolset first and then from PATH,
// trying each candidate name in order.
func debugTool(tc toolchain.Toolchain, names ...string) (string, error) {
	for _, name := range names {
		if candidate := filepath.Join(tc.ToolsetBin(), name); fileExists(candidate) {
			return candidate, nil
		}
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("none of %s found in %s or on PATH; install the LLVM binutils "+
		"(Debian/Ubuntu: llvm) to emit debug symbols", strings.Join(names, ", "), tc.ToolsetBin())
}

// machOSectionSize reports the size of a named section, 0 when absent. Fat
// binaries are resolved to their first architecture.
func machOSectionSize(path, section string) (uint64, error) {
	f, err := macho.Open(path)
	if err != nil {
		fat, fatErr := macho.OpenFat(path)
		if fatErr != nil {
			return 0, err
		}
		defer fat.Close()
		if len(fat.Arches) == 0 {
			return 0, fmt.Errorf("%s: fat Mach-O with no architectures", path)
		}
		return sectionSize(fat.Arches[0].File, section), nil
	}
	defer f.Close()
	return sectionSize(f, section), nil
}

func sectionSize(f *macho.File, section string) uint64 {
	for _, s := range f.Sections {
		if s.Name == section {
			return s.Size
		}
	}
	return 0
}
