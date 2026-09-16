package idevice

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Device is a physical iOS device reachable through usbmuxd, enriched with the
// lockdownd values a build tool needs to pick a target.
type Device struct {
	UDID           string
	Name           string
	ProductType    string
	ProductVersion string
	// ConnectionType is "USB" or "Network".
	ConnectionType string
}

// String renders a device the way `ripley device list` shows it.
func (d Device) String() string {
	name := d.Name
	if name == "" {
		name = d.ProductType
	}
	return fmt.Sprintf("%s (%s, iOS %s, %s) %s", name, d.ProductType, d.ProductVersion, d.ConnectionType, d.UDID)
}

// majorVersion returns the major component of ProductVersion, or 0 when it is
// not parseable.
func (d Device) majorVersion() int {
	major, _, _ := strings.Cut(d.ProductVersion, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// ListDevices returns every device usbmuxd currently knows about.
//
// A running usbmuxd with nothing plugged in yields an empty slice and a nil
// error. An unreachable usbmuxd is an error that names the socket that was tried
// and how to get one. Devices whose lockdownd cannot be queried (locked, not yet
// trusted) are still listed, with only the fields usbmuxd itself provides.
func ListDevices() ([]Device, error) {
	muxDevices, err := listMuxDevices()
	if err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(muxDevices))
	seen := make(map[string]bool, len(muxDevices))
	for _, md := range muxDevices {
		if seen[md.UDID] {
			continue
		}
		seen[md.UDID] = true
		d := Device{UDID: md.UDID, ConnectionType: md.ConnectionType}
		// lockdownd answers GetValue for these keys without a session, so a
		// device that has never been trusted still reports its identity.
		if ld, err := dialLockdown(md); err == nil {
			d.Name = ld.getString("DeviceName")
			d.ProductType = ld.getString("ProductType")
			d.ProductVersion = ld.getString("ProductVersion")
			ld.Close()
		}
		devices = append(devices, d)
	}
	return devices, nil
}

// resolve finds the mux device for udid. An empty udid selects the only attached
// device, which is what a developer with one phone plugged in expects.
func resolve(udid string) (muxDevice, error) {
	muxDevices, err := listMuxDevices()
	if err != nil {
		return muxDevice{}, err
	}
	if udid == "" {
		// Prefer USB over a network-visible duplicate of the same device.
		var usb []muxDevice
		unique := map[string]bool{}
		for _, d := range muxDevices {
			unique[d.UDID] = true
			if d.ConnectionType == "USB" {
				usb = append(usb, d)
			}
		}
		switch {
		case len(muxDevices) == 0:
			return muxDevice{}, errors.New("no iOS device is connected")
		case len(unique) > 1:
			var names []string
			for u := range unique {
				names = append(names, u)
			}
			return muxDevice{}, fmt.Errorf("more than one device is connected (%s); pass a UDID", strings.Join(names, ", "))
		case len(usb) > 0:
			return usb[0], nil
		default:
			return muxDevices[0], nil
		}
	}
	want := normalizeUDID(udid)
	var networked muxDevice
	for _, d := range muxDevices {
		if d.UDID != want {
			continue
		}
		if d.ConnectionType == "USB" {
			return d, nil
		}
		networked = d
	}
	if networked.DeviceID != 0 {
		return networked, nil
	}
	return muxDevice{}, fmt.Errorf("device %s is not connected", udid)
}

// session bundles an authenticated lockdownd connection with the device
// description, which is what every operation past ListDevices needs.
type session struct {
	device Device
	mux    muxDevice
	ld     *lockdownConn
}

func openSession(udid string) (*session, error) {
	md, err := resolve(udid)
	if err != nil {
		return nil, err
	}
	ld, err := dialLockdown(md)
	if err != nil {
		return nil, err
	}
	pair, err := readPairRecord(md.UDID)
	if err != nil {
		ld.Close()
		return nil, err
	}
	d := Device{
		UDID:           md.UDID,
		ConnectionType: md.ConnectionType,
		Name:           ld.getString("DeviceName"),
		ProductType:    ld.getString("ProductType"),
		ProductVersion: ld.getString("ProductVersion"),
	}
	if err := ld.StartSession(pair); err != nil {
		ld.Close()
		return nil, err
	}
	return &session{device: d, mux: md, ld: ld}, nil
}

func (s *session) Close() {
	s.ld.StopSession()
	s.ld.Close()
}

// InstallIPA uploads ipaPath to the device and installs it, reporting nothing.
//
// This is the form the `ripley device install` command binds to; use
// InstallIPAProgress to follow the upload and installation as they happen.
func InstallIPA(udid, ipaPath string) error {
	return InstallIPAProgress(udid, ipaPath, nil)
}

// InstallIPAProgress uploads ipaPath to the device and installs it.
//
// The IPA is staged into the device's PublicStaging directory over AFC and then
// handed to installd, which is exactly the sequence Xcode uses; installd needs
// the package to already be on the device, so the upload cannot be skipped.
// Progress is reported for both phases when progress is non-nil.
//
// If the bundle is already installed, installd performs an upgrade in place,
// preserving the app's container.
func InstallIPAProgress(udid, ipaPath string, progress func(string)) error {
	abs, err := filepath.Abs(ipaPath)
	if err != nil {
		return err
	}
	report := func(format string, args ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, args...))
		}
	}

	s, err := openSession(udid)
	if err != nil {
		return err
	}
	defer s.Close()
	report("Target %s", s.device)

	afcSvc, err := s.ld.StartService(afcServiceName)
	if err != nil {
		return err
	}
	afc := newAFC(afcSvc)

	remote, err := afc.uploadPackage(abs, func(sent, total int64) {
		report("Uploading %s %s", filepath.Base(abs), progressBar(sent, total))
	})
	afc.Close()
	if err != nil {
		return err
	}
	report("Staged at %s", remote)

	ipSvc, err := s.ld.StartService(installProxyService)
	if err != nil {
		return err
	}
	proxy := newInstallProxy(ipSvc)
	defer proxy.Close()

	// installd resolves PackagePath relative to the AFC media root.
	if err := proxy.install("Install", remote, func(status string, percent int) {
		report("Installing %d%% %s", percent, status)
	}); err != nil {
		return err
	}
	report("Installed")
	return nil
}

// progressBar renders "45% (12.3/27.0 MiB)".
func progressBar(sent, total int64) string {
	const mib = 1024 * 1024
	if total <= 0 {
		return fmt.Sprintf("%.1f MiB", float64(sent)/mib)
	}
	return fmt.Sprintf("%d%% (%.1f/%.1f MiB)", sent*100/total, float64(sent)/mib, float64(total)/mib)
}

// LookupApp returns the installed application record for bundleID, or nil when
// the device does not have it installed.
func LookupApp(udid, bundleID string) (*AppInfo, error) {
	s, err := openSession(udid)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	svc, err := s.ld.StartService(installProxyService)
	if err != nil {
		return nil, err
	}
	proxy := newInstallProxy(svc)
	defer proxy.Close()
	return proxy.lookup(bundleID)
}

// Uninstall removes bundleID from the device.
func Uninstall(udid, bundleID string) error {
	s, err := openSession(udid)
	if err != nil {
		return err
	}
	defer s.Close()

	svc, err := s.ld.StartService(installProxyService)
	if err != nil {
		return err
	}
	proxy := newInstallProxy(svc)
	defer proxy.Close()
	return proxy.uninstall(bundleID, nil)
}

// ProbeService reports whether lockdownd will start a named service on the
// device. It exists so that callers (and the test suite) can establish which
// developer services a given iOS version still exposes through lockdownd
// instead of inferring it from the version number.
func ProbeService(udid, name string) error {
	s, err := openSession(udid)
	if err != nil {
		return err
	}
	defer s.Close()

	svc, err := s.ld.StartService(name)
	if err != nil {
		return err
	}
	return svc.Close()
}
