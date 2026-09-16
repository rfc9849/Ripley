package idevice

import (
	"errors"
	"fmt"
	"time"
)

// Launching an installed app from a non-Apple host uses one of two transports,
// and which one a device offers is decided by its iOS major version.
//
// Up to and including iOS 16, lockdownd starts
// `com.apple.instruments.remoteserver.DVTSecureSocketProxy`, a DTX endpoint whose
// `processControl` channel launches a bundle identifier directly.
//
// From iOS 17 on, Apple moved every developer service off lockdownd into
// RemoteXPC behind the CoreDevice tunnel. That tunnel does NOT require root or a
// kernel utun interface: lockdownd still starts
// `com.apple.internal.devicecompute.CoreDeviceProxy`, and after a small handshake
// that service becomes a point-to-point IPv6 link carried over the same usbmuxd
// socket. This package brings its own TCP/IPv6 (tcpip.go) so the entire path
// stays in user space, then reaches `com.apple.coredevice.appservice` through
// Remote Service Discovery (remotexpc.go, appservice.go).
//
// Which transport is used is detected, never guessed: the DTX service is tried
// first, and the tunnel is used when the device refuses it.
const (
	instrumentsSecureService = "com.apple.instruments.remoteserver.DVTSecureSocketProxy"
	instrumentsService       = "com.apple.instruments.remoteserver"

	processControlChannel = "com.apple.instruments.server.services.processcontrol"

	// launchTimeout bounds the processControl round trip. Cold-starting a large
	// Flutter app on a busy device takes a while.
	launchTimeout = 3 * time.Minute
)

// ErrTunnelRequired reports that the device only exposes its developer services
// through the CoreDevice/RemoteXPC tunnel and that the tunnel itself could not be
// established.
var ErrTunnelRequired = errors.New("developer services require the CoreDevice tunnel")

// LaunchApp starts an installed application on the device and returns the
// process id the device assigned it.
//
// udid may be empty when exactly one device is attached. The app must already be
// installed; use InstallIPA first.
//
// On iOS 16 and older this launches through the instruments processControl
// service. On iOS 17 and newer it opens the CoreDevice tunnel over usbmuxd,
// discovers `com.apple.coredevice.appservice` through RSD and launches there.
// Either way LaunchApp returns once the device has reported the new process id.
func LaunchApp(udid, bundleID string) (int, error) {
	return LaunchAppProgress(udid, bundleID, nil)
}

// LaunchAppProgress is LaunchApp with optional progress reporting for bootstrap
// work such as mounting the iOS 17+ Personalized Developer Disk Image.
func LaunchAppProgress(udid, bundleID string, progress func(string)) (int, error) {
	if bundleID == "" {
		return 0, errors.New("launch: no bundle identifier given")
	}
	s, err := openSession(udid)
	if err != nil {
		return 0, err
	}
	defer s.Close()

	// Refuse to launch something that is not there: installd's own error is far
	// clearer than the instruments service's generic failure.
	ipSvc, err := s.ld.StartService(installProxyService)
	if err != nil {
		return 0, err
	}
	proxy := newInstallProxy(ipSvc)
	app, lookupErr := proxy.lookup(bundleID)
	proxy.Close()
	if lookupErr != nil {
		return 0, lookupErr
	}
	if app == nil {
		return 0, fmt.Errorf("launch %s: %w on %s", bundleID, errAppNotInstalled, s.device.UDID)
	}

	if s.device.majorVersion() >= 17 {
		if err := s.ensurePersonalizedDeveloperImage(progress); err != nil {
			return 0, fmt.Errorf("prepare developer services: %w", err)
		}
	}

	pid, err := s.launch(app)
	if err != nil {
		return 0, fmt.Errorf("launch %s: %w", bundleID, err)
	}
	if pid == 0 {
		return 0, fmt.Errorf("launch %s: device did not report a process id", bundleID)
	}
	return int(pid), nil
}

// launch runs whichever launch transport the device offers.
func (s *session) launch(app *AppInfo) (uint64, error) {
	svc, instrErr := s.startInstruments()
	if instrErr == nil {
		defer svc.Close()
		return launchViaProcessControl(newDTX(svc.conn), app)
	}
	if s.device.majorVersion() < 17 {
		return 0, s.legacyLaunchError(instrErr)
	}
	return s.launchViaTunnel(app.BundleID)
}

// launchViaTunnel launches through CoreDevice: tunnel, then RSD, then appservice.
func (s *session) launchViaTunnel(bundleID string) (uint64, error) {
	tun, err := s.startTunnel()
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrTunnelRequired, err)
	}
	defer tun.Close()

	stack, err := newTunnelStack(tun)
	if err != nil {
		return 0, err
	}
	defer stack.Close()

	rsd, err := dialRSD(stack, tun.info.RSDPort)
	if err != nil {
		return 0, err
	}
	defer rsd.Close()

	return launchViaAppService(rsd, stack, bundleID)
}

// startInstruments opens the DTX developer service. iOS 17 and newer answer
// InvalidService for every name here, and that refusal is what selects the
// tunnel transport.
func (s *session) startInstruments() (*service, error) {
	svc, secureErr := s.ld.StartService(instrumentsSecureService)
	if secureErr == nil {
		return svc, nil
	}
	// Pre-iOS-10 devices only have the non-"DVTSecureSocketProxy" name.
	svc, plainErr := s.ld.StartService(instrumentsService)
	if plainErr == nil {
		return svc, nil
	}
	return nil, secureErr
}

// legacyLaunchError explains an instruments refusal on a device old enough that
// the service should still have been there.
func (s *session) legacyLaunchError(cause error) error {
	return fmt.Errorf("%s is unavailable on %s (iOS %s): %w. "+
		"Mount a Developer Disk Image on the device first (Xcode does this automatically "+
		"the first time a device is used for development)",
		instrumentsSecureService, s.device.UDID, s.device.ProductVersion, cause)
}

// launchViaProcessControl runs the processControl launch selector and returns the
// new process id.
func launchViaProcessControl(dtx *dtxConn, app *AppInfo) (uint64, error) {
	channel, err := dtx.makeChannel(processControlChannel)
	if err != nil {
		return 0, err
	}

	aux := &dtxAux{}
	// launchSuspendedProcessWithDevicePath:bundleIdentifier:environment:arguments:options:
	// The device path is unused for an installed bundle but must be present.
	if err := aux.AddObject(app.Path); err != nil {
		return 0, err
	}
	if err := aux.AddObject(app.BundleID); err != nil {
		return 0, err
	}
	if err := aux.AddObject(map[string]any{}); err != nil {
		return 0, err
	}
	if err := aux.AddObject([]any{}); err != nil {
		return 0, err
	}
	if err := aux.AddObject(map[string]any{
		// Start the process running rather than suspended for a debugger, and
		// let the app keep running after we disconnect.
		"StartSuspendedKey": false,
		"KillExisting":      true,
	}); err != nil {
		return 0, err
	}

	const selector = "launchSuspendedProcessWithDevicePath:bundleIdentifier:environment:arguments:options:"
	result, err := dtx.call(channel, selector, aux, launchTimeout)
	if err != nil {
		return 0, err
	}
	pid, ok := toInt64(result)
	if !ok {
		return 0, fmt.Errorf("processControl returned %T (%v) instead of a process id", result, result)
	}
	return uint64(pid), nil
}
