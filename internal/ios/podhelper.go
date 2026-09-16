package ios

// Flutter's own Podfile helper validates the engine artifact cache before any
// ripley code gets a chance to run. Every Flutter app's ios/Podfile does
//
//	require File.expand_path(File.join('packages', 'flutter_tools', 'bin', 'podhelper'), flutter_root)
//
// and podhelper.rb's flutter_additional_ios_build_settings then hard-fails:
//
//	artifacts_dir = File.join('..', '..', '..', '..', 'bin', 'cache', 'artifacts', 'engine')
//	debug_framework_dir = File.expand_path(File.join(artifacts_dir, 'ios', 'Flutter.xcframework'), __FILE__)
//	unless Dir.exist?(debug_framework_dir)
//	  # iOS artifacts have not been downloaded.
//	  raise "#{debug_framework_dir} must exist. If you're running pod install manually, make sure \"flutter precache --ios\" is executed first"
//	end
//	release_framework_dir = File.expand_path(File.join(artifacts_dir, 'ios-release', 'Flutter.xcframework'), __FILE__)
//
// and, per build configuration,
//
//	configuration_engine_dir = build_configuration.type == :debug ? debug_framework_dir : release_framework_dir
//	Dir.new(configuration_engine_dir).each_child do |xcframework_file|
//
// so `ios` must exist (explicit raise) and `ios-release` must exist too, since
// Dir.new raises ENOENT for every non-debug configuration. There is no
// `ios-profile` requirement: podhelper.rb maps profile onto the release
// directory ("Profile can't be derived from the CocoaPods build configuration.
// Use release framework (for linking only)."), matching the stock Podfile's
// `'Profile' => :release`.
//
// The documented remedy, `flutter precache --ios`, refuses to fetch iOS
// artifacts on Linux, so ripley stages its own cached Flutter.xcframework into
// those two directories instead.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ripley/internal/toolchain"
)

// podhelperArtifactDirs are the engine artifact directory names podhelper.rb
// requires for an iOS `pod install`, in the order it touches them.
var podhelperArtifactDirs = []string{"ios", "ios-release"}

// xcframeworkPlist is the top-level Info.plist of an .xcframework bundle. An
// xcframework without one is invalid: `Dir.new(...).each_child` in podhelper.rb
// tolerates its absence, but every real consumer (and ripley's own
// selectXCFrameworkSlice) reads it to pick a slice.
type xcframeworkPlist struct {
	AvailableLibraries []xcframeworkLibrary `plist:"AvailableLibraries"`
	PackageType        string               `plist:"CFBundlePackageType"`
	FormatVersion      string               `plist:"XCFrameworkFormatVersion"`
}

type xcframeworkLibrary struct {
	BinaryPath               string   `plist:"BinaryPath,omitempty"`
	LibraryIdentifier        string   `plist:"LibraryIdentifier"`
	LibraryPath              string   `plist:"LibraryPath"`
	SupportedArchitectures   []string `plist:"SupportedArchitectures"`
	SupportedPlatform        string   `plist:"SupportedPlatform"`
	SupportedPlatformVariant string   `plist:"SupportedPlatformVariant,omitempty"`
}

// EnsureFlutterIOSArtifacts stages ripley's cached Flutter.xcframework into the
// engine artifact directories under flutterRoot that podhelper.rb requires, so
// that `pod install` succeeds on a Linux host where `flutter precache --ios`
// cannot run.
//
// It is idempotent: a destination that already resolves to a usable iOS arm64
// device slice is left completely untouched, so a macOS user's real
// `flutter precache --ios` output survives and repeat calls write nothing and
// touch no mtimes. Where staging is needed only the missing pieces are added;
// existing slice directories are never replaced.
func EnsureFlutterIOSArtifacts(tc toolchain.Toolchain, flutterRoot string) error {
	if flutterRoot == "" {
		return errors.New("stage Flutter iOS engine artifacts: empty Flutter root")
	}
	engineDir := filepath.Join(flutterRoot, "bin", "cache", "artifacts", "engine")

	missing := make([]string, 0, len(podhelperArtifactDirs))
	for _, name := range podhelperArtifactDirs {
		dest := filepath.Join(engineDir, name, "Flutter.xcframework")
		if _, _, err := selectXCFrameworkSlice(dest); err == nil {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}

	source := tc.FlutterXCFramework()
	slices, err := xcframeworkSlices(source)
	if err != nil {
		return err
	}

	for _, name := range missing {
		dest := filepath.Join(engineDir, name, "Flutter.xcframework")
		fmt.Printf("flutter: staging Flutter.xcframework into bin/cache/artifacts/engine/%s (flutter precache --ios cannot run on Linux)\n", name)
		if err := stageXCFramework(source, slices, dest); err != nil {
			return err
		}
		if _, _, err := selectXCFrameworkSlice(dest); err != nil {
			return fmt.Errorf("stage Flutter iOS engine artifacts: %s is still unusable after staging: %w", dest, err)
		}
	}
	return nil
}

// xcframeworkSlices describes the platform slices of an .xcframework directory
// laid out on disk, deriving each slice's platform, architectures and variant
// from its directory name (ios-arm64, ios-arm64_x86_64-simulator, ...).
func xcframeworkSlices(root string) ([]xcframeworkLibrary, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ripley has no cached Flutter.xcframework at %s to stage; run `ripley toolchain fetch` first", root)
		}
		return nil, fmt.Errorf("stage Flutter iOS engine artifacts: %w", err)
	}
	var slices []xcframeworkLibrary
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.Contains(name, "-") {
			continue
		}
		if !dirExists(filepath.Join(root, name)) {
			continue
		}
		library, ok := describeXCFrameworkSlice(root, name)
		if !ok {
			continue
		}
		slices = append(slices, library)
	}
	if len(slices) == 0 {
		return nil, fmt.Errorf("ripley has no cached Flutter.xcframework slices under %s to stage; run `ripley toolchain fetch` first", root)
	}
	return slices, nil
}

// describeXCFrameworkSlice builds the AvailableLibraries entry for the slice
// directory identifier under root, reporting false when the directory holds no
// framework.
func describeXCFrameworkSlice(root, identifier string) (xcframeworkLibrary, bool) {
	sliceDir := filepath.Join(root, identifier)
	entries, err := os.ReadDir(sliceDir)
	if err != nil {
		return xcframeworkLibrary{}, false
	}
	framework := ""
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".framework") {
			framework = entry.Name()
			break
		}
	}
	if framework == "" {
		return xcframeworkLibrary{}, false
	}

	// ios-arm64 -> platform ios, archs [arm64]; ios-arm64_x86_64-simulator ->
	// platform ios, archs [arm64 x86_64], variant simulator.
	parts := strings.Split(identifier, "-")
	if len(parts) < 2 {
		return xcframeworkLibrary{}, false
	}
	library := xcframeworkLibrary{
		LibraryIdentifier:      identifier,
		LibraryPath:            framework,
		SupportedArchitectures: strings.Split(parts[1], "_"),
		SupportedPlatform:      parts[0],
	}
	if len(parts) > 2 {
		library.SupportedPlatformVariant = strings.Join(parts[2:], "-")
	}
	binary := strings.TrimSuffix(framework, ".framework")
	if fileExists(filepath.Join(sliceDir, framework, binary)) {
		library.BinaryPath = framework + "/" + binary
	}
	return library, true
}

// stageXCFramework makes dest a valid xcframework backed by source. Slice
// directories are symlinked rather than copied: the engine slice carries ~150MB
// of dSYMs, podhelper.rb only stats and lists them, and the search paths it
// bakes into the generated Pods xcconfigs resolve through the link. An entry
// that already resolves to a directory is left as it is.
func stageXCFramework(source string, slices []xcframeworkLibrary, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("stage Flutter iOS engine artifacts: %w", err)
	}
	staged := make([]xcframeworkLibrary, 0, len(slices))
	for _, library := range slices {
		link := filepath.Join(dest, library.LibraryIdentifier)
		switch _, err := os.Lstat(link); {
		case err == nil && dirExists(link):
			// A real slice already present (or a link that still resolves):
			// keep it, and describe what is actually there.
			if existing, ok := describeXCFrameworkSlice(dest, library.LibraryIdentifier); ok {
				staged = append(staged, existing)
				continue
			}
			return fmt.Errorf("stage Flutter iOS engine artifacts: %s exists but holds no framework", link)
		case err == nil:
			// Broken symlink or stray file from an interrupted stage.
			if err := os.Remove(link); err != nil {
				return fmt.Errorf("stage Flutter iOS engine artifacts: %w", err)
			}
		case !os.IsNotExist(err):
			return fmt.Errorf("stage Flutter iOS engine artifacts: %w", err)
		}
		if err := os.Symlink(filepath.Join(source, library.LibraryIdentifier), link); err != nil {
			return fmt.Errorf("stage Flutter iOS engine artifacts: %w", err)
		}
		staged = append(staged, library)
	}

	// Preserve any Info.plist that is already there; only its absence (the case
	// that makes the staged bundle invalid) is repaired.
	info := filepath.Join(dest, "Info.plist")
	if fileExists(info) {
		return nil
	}
	if err := writePlist(info, xcframeworkPlist{
		AvailableLibraries: staged,
		PackageType:        "XFWK",
		FormatVersion:      "1.0",
	}); err != nil {
		return fmt.Errorf("stage Flutter iOS engine artifacts: write %s: %w", info, err)
	}
	return nil
}
