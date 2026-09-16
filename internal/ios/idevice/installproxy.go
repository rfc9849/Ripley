package idevice

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const installProxyService = "com.apple.mobile.installation_proxy"

// installProgressTimeout bounds the wait for the *next* progress message from
// installd. Installing a large app on a busy device is slow but never silent for
// this long.
const installProgressTimeout = 10 * time.Minute

// installProxy is an installation_proxy client.
type installProxy struct {
	svc *service
}

func newInstallProxy(svc *service) *installProxy { return &installProxy{svc: svc} }

func (p *installProxy) Close() error { return p.svc.Close() }

// installReply is one message from installd. Progress messages carry Status and
// PercentComplete; the terminal message is either Status "Complete" or an Error
// triple.
type installReply struct {
	Status           string `plist:"Status"`
	PercentComplete  uint64 `plist:"PercentComplete"`
	Error            string `plist:"Error"`
	ErrorDescription string `plist:"ErrorDescription"`
	ErrorDetail      int64  `plist:"ErrorDetail"`
	CFBundleID       string `plist:"CFBundleIdentifier"`
}

// InstallError is a structured installd rejection. installd reports a symbolic
// name plus a human description, and both matter: the name is what callers
// switch on, the description is what a user can act on.
type InstallError struct {
	Name        string
	Description string
	Detail      int64
	BundleID    string
}

func (e *InstallError) Error() string {
	var b strings.Builder
	b.WriteString("installation failed")
	if e.BundleID != "" {
		b.WriteString(" for " + e.BundleID)
	}
	b.WriteString(": ")
	switch {
	case e.Description != "" && e.Name != "":
		b.WriteString(e.Description + " (" + e.Name + ")")
	case e.Description != "":
		b.WriteString(e.Description)
	case e.Name != "":
		b.WriteString(e.Name)
	default:
		b.WriteString("unknown installd error")
	}
	if e.Detail != 0 {
		fmt.Fprintf(&b, " [detail %d]", e.Detail)
	}
	if hint := installHintFor(e.Name); hint != "" {
		b.WriteString("\n" + hint)
	}
	return b.String()
}

// installHintFor turns the installd error names a signing-focused tool actually
// provokes into the fix for them.
func installHintFor(name string) string {
	switch name {
	case "ApplicationVerificationFailed", "MismatchedApplicationIdentifierEntitlement":
		return "the embedded provisioning profile does not cover this bundle identifier, or the signing identity is not trusted on the device"
	case "DeviceNotSupported", "IncorrectArchitecture":
		return "the app was built for a different architecture or a newer iOS than the device runs"
	case "MissingBundleExecutable", "MissingBundleIdentifier", "MissingRequiredKeyInInfoPlist":
		return "the app bundle's Info.plist is incomplete; this is a packaging bug, not a device problem"
	case "DeviceOSVersionTooLow":
		return "raise the device's iOS version or lower MinimumOSVersion in the app's Info.plist"
	case "PackageInspectionFailed":
		return "the uploaded IPA is not a readable zip containing Payload/<App>.app"
	case "NoSuchFile":
		return "installd could not read the staged package; the AFC upload did not land where installd looks"
	case "DeviceLocked", "PasscodeChangeRequired":
		return "unlock the device and retry"
	}
	return ""
}

// clientOptions are the installd options ripley always sets.
func clientOptions() map[string]any {
	return map[string]any{
		// Developer-signed apps are "Developer" package type; installd
		// otherwise assumes an App Store package and rejects the signature.
		"PackageType": "Developer",
	}
}

// install runs Install or Upgrade against an already staged package and pumps
// progress until installd reports a terminal message.
func (p *installProxy) install(command, packagePath string, progress func(status string, percent int)) error {
	if err := p.svc.send(map[string]any{
		"Command":       command,
		"PackagePath":   packagePath,
		"ClientOptions": clientOptions(),
	}); err != nil {
		return err
	}
	return p.pump(command, progress)
}

// uninstall removes a bundle. installd identifies the target by bundle id here,
// not by a package path.
func (p *installProxy) uninstall(bundleID string, progress func(status string, percent int)) error {
	if err := p.svc.send(map[string]any{
		"Command":               "Uninstall",
		"ApplicationIdentifier": bundleID,
	}); err != nil {
		return err
	}
	return p.pump("Uninstall", progress)
}

// pump consumes installd's progress stream until a terminal message arrives.
func (p *installProxy) pump(command string, progress func(status string, percent int)) error {
	for {
		var reply installReply
		if err := p.svc.receiveTimeout(&reply, installProgressTimeout); err != nil {
			return fmt.Errorf("%s: %w", strings.ToLower(command), err)
		}
		if reply.Error != "" || reply.ErrorDescription != "" {
			return &InstallError{
				Name:        reply.Error,
				Description: reply.ErrorDescription,
				Detail:      reply.ErrorDetail,
				BundleID:    reply.CFBundleID,
			}
		}
		if progress != nil && reply.Status != "" {
			progress(reply.Status, int(reply.PercentComplete))
		}
		if reply.Status == "Complete" {
			return nil
		}
	}
}

// AppInfo describes one installed application.
type AppInfo struct {
	BundleID   string
	Name       string
	Version    string
	Build      string
	Path       string
	Executable string
	Container  string
}

// lookup returns the installed application record for bundleID, or nil when the
// app is not installed.
func (p *installProxy) lookup(bundleID string) (*AppInfo, error) {
	if err := p.svc.send(map[string]any{
		"Command": "Lookup",
		"ClientOptions": map[string]any{
			"BundleIDs": []string{bundleID},
			"ReturnAttributes": []string{
				"CFBundleIdentifier",
				"CFBundleDisplayName",
				"CFBundleShortVersionString",
				"CFBundleVersion",
				"CFBundleExecutable",
				"Path",
				"Container",
			},
		},
	}); err != nil {
		return nil, err
	}
	var reply struct {
		LookupResult map[string]struct {
			CFBundleIdentifier         string `plist:"CFBundleIdentifier"`
			CFBundleDisplayName        string `plist:"CFBundleDisplayName"`
			CFBundleShortVersionString string `plist:"CFBundleShortVersionString"`
			CFBundleVersion            string `plist:"CFBundleVersion"`
			CFBundleExecutable         string `plist:"CFBundleExecutable"`
			Path                       string `plist:"Path"`
			Container                  string `plist:"Container"`
		} `plist:"LookupResult"`
		Status           string `plist:"Status"`
		Error            string `plist:"Error"`
		ErrorDescription string `plist:"ErrorDescription"`
	}
	if err := p.svc.receive(&reply); err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, &InstallError{Name: reply.Error, Description: reply.ErrorDescription}
	}
	entry, ok := reply.LookupResult[bundleID]
	if !ok {
		return nil, nil
	}
	return &AppInfo{
		BundleID:   entry.CFBundleIdentifier,
		Name:       entry.CFBundleDisplayName,
		Version:    entry.CFBundleShortVersionString,
		Build:      entry.CFBundleVersion,
		Path:       entry.Path,
		Executable: entry.CFBundleExecutable,
		Container:  entry.Container,
	}, nil
}

// errAppNotInstalled reports a bundle identifier that installd does not know.
var errAppNotInstalled = errors.New("application is not installed")
