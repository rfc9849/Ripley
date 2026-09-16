package app

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ripley/internal/ios/idevice"

	"howett.net/plist"
)

// deviceCommand implements `ripley device`, the Linux equivalent of ios-deploy.
// Every subcommand talks to usbmuxd directly, so no Apple tooling is involved.
func deviceCommand(args []string) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help")) {
		printDeviceUsage()
		return nil
	}
	switch args[0] {
	case "list", "ls":
		return deviceListCommand(args[1:])
	case "install":
		return deviceInstallCommand(args[1:], false)
	case "run":
		return deviceInstallCommand(args[1:], true)
	case "launch":
		return deviceLaunchCommand(args[1:])
	case "uninstall":
		return deviceUninstallCommand(args[1:])
	case "logs", "syslog":
		return deviceLogsCommand(args[1:])
	default:
		return usageError(fmt.Sprintf("unknown device subcommand %q", args[0]), "ripley device")
	}
}

func printDeviceUsage() {
	fmt.Println("Usage: ripley device <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  list                 list connected iOS devices")
	fmt.Println("  install <app.ipa>    install an IPA")
	fmt.Println("  run <app.ipa>        install and launch an IPA")
	fmt.Println("  launch <bundle-id>   launch an installed app")
	fmt.Println("  logs                 stream device syslog")
	fmt.Println("  uninstall <bundle>   uninstall an app")
}

func deviceListCommand(args []string) error {
	fs := newCommandFlagSet("device list", "ripley device list")
	help, err := parseCommandFlags(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	devices, err := idevice.ListDevices()
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Println("no devices connected")
		return nil
	}
	for _, device := range devices {
		fmt.Println(device.String())
	}
	return nil
}

// resolveDeviceUDID picks the target device. An explicit --udid always wins;
// otherwise exactly one connected device is required, because silently choosing
// among several would install to an arbitrary phone.
func resolveDeviceUDID(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	devices, err := idevice.ListDevices()
	if err != nil {
		return "", err
	}
	switch len(devices) {
	case 0:
		return "", errors.New("no iOS device connected; check the cable and that usbmuxd is running")
	case 1:
		return devices[0].UDID, nil
	default:
		names := make([]string, 0, len(devices))
		for _, device := range devices {
			names = append(names, device.UDID)
		}
		return "", fmt.Errorf("multiple devices connected; select one with --udid (%s)", strings.Join(names, ", "))
	}
}

func deviceInstallCommand(args []string, launch bool) error {
	name := "device install"
	if launch {
		name = "device run"
	}
	fs := newCommandFlagSet(name, "ripley "+name+" [--udid id] <app.ipa>")
	udid := fs.String("udid", "", "target device UDID")
	bundleID := fs.String("bundle-id", "", "bundle id to launch (default: read from the IPA)")
	help, err := parseCommandFlags(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if fs.NArg() != 1 {
		return usageError("expected exactly one IPA path", "ripley "+name)
	}
	ipa, err := filepath.Abs(expandHome(fs.Arg(0)))
	if err != nil {
		return err
	}
	if _, err := os.Stat(ipa); err != nil {
		return err
	}
	target, err := resolveDeviceUDID(*udid)
	if err != nil {
		return err
	}
	fmt.Printf("installing %s on %s\n", filepath.Base(ipa), target)
	if err := idevice.InstallIPAProgress(target, ipa, func(status string) {
		fmt.Printf("  %s\n", status)
	}); err != nil {
		return err
	}
	fmt.Println("installed")
	if !launch {
		return nil
	}
	id := *bundleID
	if id == "" {
		id, err = ipaBundleID(ipa)
		if err != nil {
			return err
		}
	}
	return launchOnDevice(target, id)
}

func deviceLaunchCommand(args []string) error {
	fs := newCommandFlagSet("device launch", "ripley device launch [--udid id] <bundle-id>")
	udid := fs.String("udid", "", "target device UDID")
	help, err := parseCommandFlags(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if fs.NArg() != 1 {
		return usageError("expected exactly one bundle identifier", "ripley device launch")
	}
	target, err := resolveDeviceUDID(*udid)
	if err != nil {
		return err
	}
	return launchOnDevice(target, fs.Arg(0))
}

func launchOnDevice(udid, bundleID string) error {
	pid, err := idevice.LaunchAppProgress(udid, bundleID, func(status string) {
		fmt.Printf("  %s\n", status)
	})
	if err != nil {
		return err
	}
	fmt.Printf("launched %s (pid %d)\n", bundleID, pid)
	return nil
}

func deviceLogsCommand(args []string) error {
	fs := newCommandFlagSet("device logs", "ripley device logs [--udid id]")
	udid := fs.String("udid", "", "target device UDID")
	help, err := parseCommandFlags(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if fs.NArg() != 0 {
		return usageError("device logs takes no positional arguments", "ripley device logs")
	}
	target, err := resolveDeviceUDID(*udid)
	if err != nil {
		return err
	}
	return idevice.StreamSyslog(target, os.Stdout)
}

func deviceUninstallCommand(args []string) error {
	fs := newCommandFlagSet("device uninstall", "ripley device uninstall [--udid id] <bundle-id>")
	udid := fs.String("udid", "", "target device UDID")
	help, err := parseCommandFlags(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if fs.NArg() != 1 {
		return usageError("expected exactly one bundle identifier", "ripley device uninstall")
	}
	target, err := resolveDeviceUDID(*udid)
	if err != nil {
		return err
	}
	if err := idevice.Uninstall(target, fs.Arg(0)); err != nil {
		return err
	}
	fmt.Printf("uninstalled %s\n", fs.Arg(0))
	return nil
}

// ipaBundleID reads CFBundleIdentifier from the payload app's Info.plist so
// `ripley device run` can launch what it just installed without the caller
// repeating the bundle id.
func ipaBundleID(ipa string) (string, error) {
	archive, err := zip.OpenReader(ipa)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	for _, file := range archive.File {
		name := strings.TrimPrefix(file.Name, "./")
		parts := strings.Split(name, "/")
		// Payload/<Product>.app/Info.plist — nested bundles live deeper and must
		// not be mistaken for the application itself.
		if len(parts) != 3 || parts[0] != "Payload" || parts[2] != "Info.plist" || !strings.HasSuffix(parts[1], ".app") {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return "", err
		}
		var info struct {
			BundleID string `plist:"CFBundleIdentifier"`
		}
		if _, err := plist.Unmarshal(data, &info); err != nil {
			return "", fmt.Errorf("parse %s: %w", file.Name, err)
		}
		if info.BundleID == "" {
			return "", fmt.Errorf("%s has no CFBundleIdentifier", file.Name)
		}
		return info.BundleID, nil
	}
	return "", fmt.Errorf("%s contains no Payload/*.app/Info.plist", ipa)
}
