package app

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"ripley/internal/ios"
	"ripley/internal/toolchain"
)

const Version = "0.1.0"

type buildOptions struct {
	Dir              string
	Mode             string
	IPA              bool
	NoSign           bool
	NoPlugins        bool
	Flavor           string
	Target           string
	BundleID         string
	NoTreeShakeIcons bool
	SplitDebugInfo   string
	Obfuscate        bool
	SaveDebugInfo    bool
	Key              string
	Cert             string
	Provision        string
	Password         string
	Host             string
}

func Main(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	if args[0] == "--version" || args[0] == "-version" {
		fmt.Println(Version)
		return nil
	}

	switch args[0] {
	case "setup":
		return setupCommand(args[1:])
	case "toolchain":
		return toolchainCommand(args[1:])
	case "build":
		return buildCommand(args[1:])
	case "device":
		return deviceCommand(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return usageError(fmt.Sprintf("unknown command %q", args[0]), "ripley")
	}
}

func printUsage() {
	fmt.Printf("ripley %s — build Flutter apps for iOS on Linux\n", Version)
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  ripley <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  setup       configure the iPhoneOS SDK and Swift toolchain")
	fmt.Println("  toolchain   fetch or build the cross gen_snapshot")
	fmt.Println("  build ios   build an iOS app or IPA")
	fmt.Println("  device      list, install, launch, run, uninstall, or stream logs")
	fmt.Println()
	fmt.Println("Run 'ripley <command> --help' for command-specific help.")
}

func usageError(message, helpCommand string) error {
	return fmt.Errorf("%s\nRun '%s --help' for usage", message, helpCommand)
}

func newCommandFlagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s\n", usage)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(fs.Output(), "\nOptions:")
			fs.PrintDefaults()
		}
	}
	return fs
}

func parseCommandFlags(fs *flag.FlagSet, args []string) (bool, error) {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			previous := fs.Output()
			fs.SetOutput(os.Stdout)
			fs.Usage()
			fs.SetOutput(previous)
			return true, nil
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func setupCommand(args []string) error {
	fs := newCommandFlagSet("setup", "ripley setup [--sdk path] [--swift path]")
	sdk := fs.String("sdk", "", "path to iPhoneOS.sdk or host:path")
	swift := fs.String("swift", "", "path to a Linux Swift toolchain")
	help, err := parseCommandFlags(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}

	flutterRoot, err := findFlutterRoot()
	if err != nil {
		return err
	}
	engine, dart, err := toolchain.FlutterEngineRevision(flutterRoot)
	if err != nil {
		return err
	}
	fmt.Printf("Flutter engine %s  Dart %s\n", engine, dart)
	tc := toolchain.NewToolchain(engine, dart)
	if err := tc.Ensure(false); err != nil {
		return err
	}
	fmt.Printf("Toolchain ready at %s\n", tc.Root)

	if *sdk != "" {
		if err := provisionIOSSDK(*sdk); err != nil {
			return err
		}
	}
	if *swift != "" {
		return provisionSwift(*swift)
	}
	return nil
}

func toolchainCommand(args []string) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		fmt.Println("Usage: ripley toolchain <fetch|build> [options]")
		fmt.Println()
		fmt.Println("  fetch   download a prebuilt cross gen_snapshot for the current Flutter/Dart SDK")
		fmt.Println("  build   build the cross gen_snapshot from Dart SDK source")
		fmt.Println()
		fmt.Println("Run 'ripley toolchain build --help' for source-build options.")
		return nil
	}

	switch args[0] {
	case "fetch":
		if len(args) != 1 {
			return usageError("fetch takes no arguments", "ripley toolchain")
		}
		flutterRoot, err := findFlutterRoot()
		if err != nil {
			return err
		}
		engine, dart, err := toolchain.FlutterEngineRevision(flutterRoot)
		if err != nil {
			return err
		}
		tc := toolchain.NewToolchain(engine, dart)
		fmt.Printf("Fetching prebuilt gen_snapshot for Dart %s ...\n", dart)
		if err := tc.FetchPrebuiltGenSnapshot(); err != nil {
			return err
		}
		fmt.Printf("gen_snapshot installed to %s\n", tc.GenSnapshot())
		return nil

	case "build":
		fs := newCommandFlagSet("toolchain build", "ripley toolchain build [--dart-version version] [--output path] [--workdir path]")
		dartVersion := fs.String("dart-version", "", "Dart SDK version/tag to build without requiring a Flutter SDK")
		output := fs.String("output", "", "write the built gen_snapshot to this path instead of the Flutter toolchain cache")
		workdir := fs.String("workdir", "", "Dart/depot_tools checkout and build directory (default: ~/.ripley/dartbuild)")
		help, err := parseCommandFlags(fs, args[1:])
		if err != nil {
			return err
		}
		if help {
			return nil
		}
		if fs.NArg() != 0 {
			return usageError("toolchain build takes no positional arguments", "ripley toolchain build")
		}

		var engine, dart string
		if *dartVersion != "" {
			dart = *dartVersion
			if *output == "" {
				return usageError("--dart-version requires --output so the build does not depend on a Flutter SDK", "ripley toolchain build")
			}
		} else {
			flutterRoot, err := findFlutterRoot()
			if err != nil {
				return err
			}
			engine, dart, err = toolchain.FlutterEngineRevision(flutterRoot)
			if err != nil {
				return err
			}
		}

		rev, err := dartRevision(dart)
		if err != nil {
			return err
		}
		fmt.Printf("Building gen_snapshot for ios-arm64 from Dart SDK %s ...\n", rev)
		built, err := toolchain.BuildGenSnapshot(rev, *workdir)
		if err != nil {
			return err
		}

		destination := *output
		if destination == "" {
			tc := toolchain.NewToolchain(engine, dart)
			destination = tc.GenSnapshot()
		}
		if err := copyFile(built, destination); err != nil {
			return err
		}
		if err := os.Chmod(destination, 0o755); err != nil {
			return err
		}
		fmt.Printf("gen_snapshot installed to %s\n", destination)
		return nil

	default:
		return usageError("expected 'fetch' or 'build'", "ripley toolchain")
	}
}

func buildCommand(args []string) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		fmt.Println("Usage: ripley build ios [options]")
		return nil
	}
	if args[0] != "ios" {
		return usageError(fmt.Sprintf("unsupported build platform %q", args[0]), "ripley build")
	}
	fs := newCommandFlagSet("build ios", "ripley build ios [options]")
	var opt buildOptions
	fs.StringVar(&opt.Dir, "dir", ".", "project directory")
	fs.StringVar(&opt.Mode, "mode", "release", "release or profile")
	fs.BoolVar(&opt.IPA, "ipa", false, "also produce .ipa")
	fs.BoolVar(&opt.NoSign, "no-sign", false, "skip signing")
	fs.BoolVar(&opt.NoPlugins, "no-plugins", false, "skip building iOS plugins")
	fs.StringVar(&opt.Flavor, "flavor", "", "Xcode/Flutter flavor")
	fs.StringVar(&opt.Target, "target", "", "Dart entrypoint (Flutter-compatible; default: FLUTTER_TARGET or lib/main.dart)")
	fs.StringVar(&opt.Target, "t", "", "shorthand for --target")
	fs.StringVar(&opt.BundleID, "bundle-id", "", "override the application bundle identifier (child extension suffixes are preserved)")
	fs.BoolVar(&opt.NoTreeShakeIcons, "no-tree-shake-icons", false, "disable icon font tree shaking")
	fs.StringVar(&opt.SplitDebugInfo, "split-debug-info", "", "directory for the Dart program symbol file")
	fs.BoolVar(&opt.Obfuscate, "obfuscate", false, "obfuscate Dart identifiers (requires --split-debug-info)")
	fs.BoolVar(&opt.SaveDebugInfo, "save-debugging-info", false, "emit a Dart debugging-information file and a dSYM")
	fs.StringVar(&opt.Key, "key", "", "private key or .p12")
	fs.StringVar(&opt.Cert, "cert", "", "signing certificate")
	fs.StringVar(&opt.Provision, "prov", "", ".mobileprovision profile")
	fs.StringVar(&opt.Password, "password", "", "password for .p12 (prefer RIPLEY_SIGN_PASSWORD or ~/.ripley-signing/password)")
	fs.StringVar(&opt.Host, "host", "", "remote Linux build host")
	help, err := parseCommandFlags(fs, args[1:])
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if opt.Mode != "release" && opt.Mode != "profile" {
		return fmt.Errorf("unsupported mode %q: expected release or profile", opt.Mode)
	}
	if opt.Host != "" {
		return buildRemote(opt)
	}
	return build(opt)
}

func findFlutterRoot() (string, error) {
	if root := os.Getenv("FLUTTER_ROOT"); root != "" {
		root, err := filepath.Abs(expandHome(root))
		if err != nil {
			return "", err
		}
		if fileExists(filepath.Join(root, "bin", "cache", "flutter.version.json")) {
			return root, nil
		}
		return "", fmt.Errorf("FLUTTER_ROOT %s is not a Flutter SDK", root)
	}
	if flutter, err := exec.LookPath("flutter"); err == nil {
		resolved, resolveErr := filepath.EvalSymlinks(flutter)
		if resolveErr != nil {
			resolved = flutter
		}
		root := filepath.Clean(filepath.Join(filepath.Dir(resolved), ".."))
		if fileExists(filepath.Join(root, "bin", "cache", "flutter.version.json")) {
			return root, nil
		}
	}
	home := userHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "share", "flutter"),
		filepath.Join(home, "flutter"),
		filepath.Join(home, "flutter-sdk"),
		filepath.Join(home, ".fvm", "default"),
		filepath.Join(home, "fvm", "default"),
		"/opt/homebrew/share/flutter",
		"/usr/local/share/flutter",
	}
	for _, root := range candidates {
		if fileExists(filepath.Join(root, "bin", "cache", "flutter.version.json")) {
			return filepath.Clean(root), nil
		}
	}
	return "", errors.New("could not locate Flutter SDK; run flutter pub get with the project's Flutter SDK or set FLUTTER_ROOT")
}

func provisionIOSSDK(src string) error {
	dest := filepath.Join(toolchain.RipleyHome(), "sdks")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err == nil && info.IsDir() {
		target := filepath.Join(dest, filepath.Base(filepath.Clean(src)))
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := copyDir(src, target); err != nil {
			return err
		}
		fmt.Printf("iOS SDK installed at %s\n", target)
		return nil
	}
	if err := run("", nil, "rsync", "-a", src, dest+string(os.PathSeparator)); err != nil {
		return err
	}
	fmt.Printf("iOS SDK synced to %s\n", dest)
	return nil
}

func provisionSwift(path string) error {
	swiftc := filepath.Join(path, "usr", "bin", "swiftc")
	if st, err := os.Stat(swiftc); err == nil && !st.IsDir() {
		fmt.Printf("Using Swift toolchain at %s (set RIPLEY_SWIFT to make permanent)\n", path)
		return nil
	}
	return errors.New("swift toolchain path must contain usr/bin/swiftc; install Swift for Linux and set RIPLEY_SWIFT")
}

func dartRevision(version string) (string, error) {
	cmd := exec.Command("git", "ls-remote", "--tags", "https://github.com/dart-lang/sdk", "refs/tags/"+version)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve Dart %s tag: %w", version, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("dart tag %s not found", version)
	}
	return fields[0], nil
}

type signingMaterial struct {
	Key       string
	Cert      string
	Provision string
	Password  string
}

func resolveSigning(opt buildOptions, bundleID string) (signingMaterial, error) {
	material := signingMaterial{
		Key:       expandHome(opt.Key),
		Cert:      expandHome(opt.Cert),
		Provision: expandHome(opt.Provision),
		Password:  opt.Password,
	}
	dir := filepath.Join(userHomeDir(), ".ripley-signing")
	var p12s, provisions []string
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return signingMaterial{}, err
	}
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			switch strings.ToLower(filepath.Ext(entry.Name())) {
			case ".p12":
				p12s = append(p12s, filepath.Join(dir, entry.Name()))
			case ".mobileprovision":
				provisions = append(provisions, filepath.Join(dir, entry.Name()))
			}
		}
	}
	sort.Strings(p12s)
	sort.Strings(provisions)

	if material.Key == "" {
		if len(p12s) > 1 {
			return signingMaterial{}, fmt.Errorf("multiple .p12 files in %s; select one with --key", dir)
		}
		if len(p12s) == 1 {
			material.Key = p12s[0]
		}
	}
	if material.Provision == "" && len(provisions) > 0 {
		material.Provision, err = ios.SelectProvisioningProfile(provisions, bundleID)
		if err != nil {
			return signingMaterial{}, signingProfileError(err, bundleID, dir)
		}
	}
	if material.Provision != "" {
		if err := ios.ValidateProvisioningProfile(material.Provision, bundleID); err != nil {
			return signingMaterial{}, signingProfileError(err, bundleID, dir)
		}
	}
	if material.Key == "" && material.Provision == "" {
		return material, nil
	}
	if material.Key == "" || material.Provision == "" {
		return signingMaterial{}, fmt.Errorf("real signing for %s requires both a .p12 key and a matching provisioning profile", bundleID)
	}
	if material.Password == "" {
		material.Password = os.Getenv("RIPLEY_SIGN_PASSWORD")
		if material.Password == "" {
			data, err := os.ReadFile(filepath.Join(dir, "password"))
			if err == nil {
				material.Password = strings.TrimSpace(string(data))
			} else if !errors.Is(err, os.ErrNotExist) {
				return signingMaterial{}, err
			}
		}
	}
	fmt.Printf("signing: using %s + %s for %s\n", filepath.Base(material.Key), filepath.Base(material.Provision), bundleID)
	return material, nil
}

func signingProfileError(err error, bundleID, dir string) error {
	return fmt.Errorf("%w\nSigning hint: add a non-expired profile for %s to %s, pass --prov <profile>, or use --no-sign. Use --bundle-id only when you intentionally want to change the app identifier", err, bundleID, dir)
}

func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
