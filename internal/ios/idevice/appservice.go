package idevice

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"howett.net/plist"
)

// CoreDevice's "appservice" is the iOS 17+ replacement for the DTX
// processControl channel: it launches, lists and terminates processes.
//
// Requests are XPC dictionaries wrapped in a fixed CoreDevice envelope. The
// envelope names a feature ("com.apple.coredevice.feature.launchapplication"),
// carries a per-invocation UUID and declares the DDI protocol version; the
// service refuses anything whose envelope it cannot parse, so all of it is
// mandatory rather than decorative.
const (
	appServiceName = "com.apple.coredevice.appservice"

	featureLaunchApplication = "com.apple.coredevice.feature.launchapplication"

	// coreDeviceProtocolVersion is the DDI protocol revision the envelope
	// declares. 0 is what the shipped CoreDevice framework sends.
	coreDeviceProtocolVersion int64 = 0
)

// uuidString returns a random RFC 4122 version 4 UUID.
//
// CoreDevice only requires the invocation identifier to be unique and
// well-formed, so there is no need for a UUID dependency.
func uuidString() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// coreDeviceVersion is the framework version the envelope advertises. The
// service compares it against its own to decide which request shapes it accepts.
func coreDeviceVersion() map[string]any {
	return map[string]any{
		"components": []any{
			uint64(348), uint64(1), uint64(0), uint64(0), uint64(0),
		},
		"originalComponentsCount": int64(2),
		"stringValue":             "348.1",
	}
}

// coreDeviceEnvelope wraps one feature invocation.
func coreDeviceEnvelope(feature string, input map[string]any) (map[string]any, error) {
	invocation, err := uuidString()
	if err != nil {
		return nil, err
	}
	device, err := uuidString()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"CoreDevice.CoreDeviceDDIProtocolVersion": coreDeviceProtocolVersion,
		"CoreDevice.action":                       map[string]any{},
		"CoreDevice.coreDeviceVersion":            coreDeviceVersion(),
		"CoreDevice.deviceIdentifier":             device,
		"CoreDevice.featureIdentifier":            feature,
		"CoreDevice.input":                        input,
		"CoreDevice.invocationIdentifier":         invocation,
	}, nil
}

// launchInput builds the launchapplication input for an installed bundle.
func launchInput(bundleID string) (map[string]any, error) {
	// platformSpecificOptions is an embedded plist; an empty dictionary is what
	// a plain launch sends.
	opts, err := plist.Marshal(map[string]any{}, plist.BinaryFormat)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"applicationSpecifier": map[string]any{
			"bundleIdentifier": map[string]any{"_0": bundleID},
		},
		"options": map[string]any{
			"arguments":            []any{},
			"environmentVariables": map[string]any{},
			// A pseudoterminal would require the openstdiosocket service; the
			// app is being started for the user, not for log capture.
			"standardIOUsesPseudoterminals": false,
			"startStopped":                  false,
			"terminateExisting":             true,
			"user":                          map[string]any{"active": true},
			"platformSpecificOptions":       opts,
		},
		"standardIOIdentifiers": map[string]any{},
	}, nil
}

// launchViaAppService launches bundleID through CoreDevice and returns the new
// process id.
func launchViaAppService(rsd *rsdClient, stack *tunnelStack, bundleID string) (uint64, error) {
	svc, ok := rsd.Service(appServiceName)
	if !ok {
		return 0, fmt.Errorf("the device does not advertise %s", appServiceName)
	}
	conn, err := dialXPCService(stack, svc.Port)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	input, err := launchInput(bundleID)
	if err != nil {
		return 0, err
	}
	envelope, err := coreDeviceEnvelope(featureLaunchApplication, input)
	if err != nil {
		return 0, err
	}
	reply, err := conn.request(envelope, launchTimeout)
	if err != nil {
		return 0, err
	}
	return launchedPID(reply)
}

// launchedPID extracts the process id from a launchapplication response, or
// turns the service's error description into a Go error.
func launchedPID(replyValue any) (uint64, error) {
	reply, ok := replyValue.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("launchapplication returned %T, want a dictionary", replyValue)
	}
	if errVal, ok := reply["CoreDevice.error"]; ok {
		return 0, fmt.Errorf("CoreDevice refused the launch: %s", describeCoreDeviceError(errVal))
	}
	output, ok := reply["CoreDevice.output"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("launchapplication returned no output (%v)", reply)
	}
	token, ok := output["processToken"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("launchapplication returned no process token (%v)", output)
	}
	pid, ok := token["processIdentifier"]
	if !ok {
		return 0, fmt.Errorf("launchapplication process token has no pid (%v)", token)
	}
	switch v := pid.(type) {
	case int64:
		return uint64(v), nil
	case uint64:
		return v, nil
	default:
		return 0, fmt.Errorf("launchapplication returned pid of type %T", pid)
	}
}

// describeCoreDeviceError renders the error dictionary CoreDevice returns.
//
// The dictionary arrives with lowercase keys ("code", "domain", "userInfo"), not
// the NSError-style capitals, and it buries the useful part: the outermost
// description is always a generic "The application failed to launch", while the
// reason a user can act on sits in a chain of NSUnderlyingError values nested
// inside userInfo. The whole chain is reported, outermost first, because that is
// what names both the failure and its cause.
func describeCoreDeviceError(v any) string {
	root, ok := v.(map[string]any)
	if !ok {
		return fmt.Sprint(v)
	}
	var parts []string
	// The chain is three deep in practice; the bound only stops a malformed cycle.
	for dict := root; dict != nil; {
		if s := coreDeviceErrorSummary(dict); s != "" && !covered(parts, s) {
			parts = append(parts, s)
		}
		info, _ := coreDeviceErrorField(dict, "userInfo", "NSUserInfo").(map[string]any)
		if info == nil || len(parts) >= 8 {
			break
		}
		dict, _ = info["NSUnderlyingError"].(map[string]any)
	}
	if len(parts) == 0 {
		return fmt.Sprint(root)
	}
	msg := strings.Join(parts, ": ")
	if hint := coreDeviceLaunchHint(msg); hint != "" {
		msg += "\n" + hint
	}
	return msg
}

// coreDeviceErrorField reads one field of an error dictionary, tolerating both
// the lowercase spelling the XPC wire format uses and the NSError capitals that
// the archived (pre-iOS-17) representation uses.
func coreDeviceErrorField(dict map[string]any, lower, upper string) any {
	if v, ok := dict[lower]; ok {
		return v
	}
	return dict[upper]
}

// coreDeviceErrorSummary describes one link of the error chain, preferring the
// specific failure reason over the generic description.
func coreDeviceErrorSummary(dict map[string]any) string {
	if info, ok := coreDeviceErrorField(dict, "userInfo", "NSUserInfo").(map[string]any); ok {
		for _, key := range []string{"NSLocalizedFailureReason", "NSLocalizedDescription", "NSDebugDescription"} {
			if s, ok := info[key].(string); ok && s != "" {
				return s
			}
		}
	}
	domain, _ := coreDeviceErrorField(dict, "domain", "NSDomain").(string)
	if code, ok := toInt64(coreDeviceErrorField(dict, "code", "NSCode")); ok && domain != "" {
		return fmt.Sprintf("%s error %d", domain, code)
	}
	return ""
}

// covered reports whether an already-collected part says everything s says. The
// nested reasons repeat each other verbatim, so this keeps the message readable.
func covered(parts []string, s string) bool {
	for _, p := range parts {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

// coreDeviceLaunchHint turns a launch refusal into the fix for it. A locked
// screen is the one every developer hits: iOS will not start an app there, and
// Apple's own tooling fails the same way.
func coreDeviceLaunchHint(msg string) string {
	if strings.Contains(msg, "unlocked") || strings.Contains(msg, "Locked") {
		return "unlock the device and keep it unlocked, then retry: iOS refuses to launch an app while the screen is locked"
	}
	return ""
}

// dialXPCService opens a RemoteXPC channel to a service advertised by RSD.
func dialXPCService(stack *tunnelStack, port int) (*xpcConn, error) {
	conn, err := stack.dialTunnelTCP(port, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to service port %d: %w", port, err)
	}
	h, err := newHTTP2(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &xpcConn{h: h}
	if err := c.handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}
