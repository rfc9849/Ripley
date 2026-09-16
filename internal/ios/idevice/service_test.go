package idevice

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

// testPairCerts mints a throwaway certificate/key pair in the same PEM shape
// lockdownd stores in a pairing record, so the TLS plumbing is exercised with
// material crypto/tls actually accepts.
func testPairCerts(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ripley-test-host"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// afcPeer is the device end of an AFC connection: it decodes what the client
// really put on the wire so tests can assert the packet layout.
type afcPeer struct {
	t    *testing.T
	conn net.Conn
}

type afcPacket struct {
	Entire, This, Packet, Op uint64
	Body                     []byte
}

func (p *afcPeer) read() afcPacket {
	p.t.Helper()
	var hdr [afcHeaderSize]byte
	if _, err := io.ReadFull(p.conn, hdr[:]); err != nil {
		p.t.Fatalf("device read header: %v", err)
	}
	if got := string(hdr[:8]); got != afcMagic {
		p.t.Fatalf("afc magic = %q, want %q", got, afcMagic)
	}
	pkt := afcPacket{
		Entire: binary.LittleEndian.Uint64(hdr[8:]),
		This:   binary.LittleEndian.Uint64(hdr[16:]),
		Packet: binary.LittleEndian.Uint64(hdr[24:]),
		Op:     binary.LittleEndian.Uint64(hdr[32:]),
	}
	// `this` covers header+fixed args, `entire` also covers the bulk payload.
	pkt.Body = make([]byte, pkt.Entire-afcHeaderSize)
	if _, err := io.ReadFull(p.conn, pkt.Body); err != nil {
		p.t.Fatalf("device read body: %v", err)
	}
	return pkt
}

func (p *afcPeer) writeStatus(status afcStatus) {
	p.t.Helper()
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, uint64(status))
	p.writePacket(afcOpStatus, body)
}

func (p *afcPeer) writePacket(op uint64, body []byte) {
	p.t.Helper()
	frame := make([]byte, afcHeaderSize+len(body))
	copy(frame, afcMagic)
	binary.LittleEndian.PutUint64(frame[8:], uint64(afcHeaderSize+len(body)))
	binary.LittleEndian.PutUint64(frame[16:], uint64(afcHeaderSize+len(body)))
	binary.LittleEndian.PutUint64(frame[24:], 0)
	binary.LittleEndian.PutUint64(frame[32:], op)
	copy(frame[afcHeaderSize:], body)
	if _, err := p.conn.Write(frame); err != nil {
		p.t.Fatalf("device write: %v", err)
	}
}

func newAFCPair(t *testing.T) (*afcClient, *afcPeer) {
	t.Helper()
	client, device := net.Pipe()
	t.Cleanup(func() { client.Close(); device.Close() })
	return &afcClient{conn: client}, &afcPeer{t: t, conn: device}
}

// TestAFCUploadPacketLayout asserts the exact byte layout of the packets an IPA
// upload produces: NUL-terminated paths, the 8-byte mode/handle prefixes, and a
// FILE_WRITE whose `this` length excludes the bulk payload while `entire`
// includes it. Getting any of those wrong makes AFC hang rather than fail.
func TestAFCUploadPacketLayout(t *testing.T) {
	afc, device := newAFCPair(t)

	payload := bytes.Repeat([]byte{0xAB}, 5000)
	dir := t.TempDir()
	ipa := filepath.Join(dir, "Runner.ipa")
	if err := os.WriteFile(ipa, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	const handle uint64 = 0x1122334455667788
	type result struct {
		remote string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		remote, err := afc.uploadPackage(ipa, nil)
		done <- result{remote, err}
	}()

	mkdir := device.read()
	if mkdir.Op != afcOpMakeDir {
		t.Fatalf("first op = %d, want MakeDir(%d)", mkdir.Op, afcOpMakeDir)
	}
	if got, want := string(mkdir.Body), stagingDir+"\x00"; got != want {
		t.Fatalf("mkdir path = %q, want %q", got, want)
	}
	device.writeStatus(afcStatusSuccess)

	rm := device.read()
	if rm.Op != afcOpRemovePath {
		t.Fatalf("second op = %d, want RemovePath(%d)", rm.Op, afcOpRemovePath)
	}
	if got, want := string(rm.Body), stagingDir+"/Runner.ipa\x00"; got != want {
		t.Fatalf("remove path = %q, want %q", got, want)
	}
	// A missing staged file must not abort the upload.
	device.writeStatus(8)

	open := device.read()
	if open.Op != afcOpFileOpen {
		t.Fatalf("third op = %d, want FileOpen(%d)", open.Op, afcOpFileOpen)
	}
	if mode := binary.LittleEndian.Uint64(open.Body); mode != afcModeWrOnly {
		t.Fatalf("open mode = %#x, want %#x (w)", mode, afcModeWrOnly)
	}
	if got, want := string(open.Body[8:]), stagingDir+"/Runner.ipa\x00"; got != want {
		t.Fatalf("open path = %q, want %q", got, want)
	}
	openRes := make([]byte, 8)
	binary.LittleEndian.PutUint64(openRes, handle)
	device.writePacket(afcOpFileOpenRes, openRes)

	var got []byte
	for len(got) < len(payload) {
		w := device.read()
		if w.Op != afcOpFileWrite {
			t.Fatalf("write op = %d, want FileWrite(%d)", w.Op, afcOpFileWrite)
		}
		if h := binary.LittleEndian.Uint64(w.Body); h != handle {
			t.Fatalf("write handle = %#x, want %#x", h, handle)
		}
		// The header-vs-payload split is what the device relies on.
		if w.This != afcHeaderSize+8 {
			t.Fatalf("write `this` = %d, want %d (header+handle only)", w.This, afcHeaderSize+8)
		}
		if w.Entire != w.This+uint64(len(w.Body)-8) {
			t.Fatalf("write `entire` = %d inconsistent with this=%d body=%d", w.Entire, w.This, len(w.Body))
		}
		got = append(got, w.Body[8:]...)
		device.writeStatus(afcStatusSuccess)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("uploaded %d bytes, want %d identical bytes", len(got), len(payload))
	}

	cl := device.read()
	if cl.Op != afcOpFileClose {
		t.Fatalf("close op = %d, want FileClose(%d)", cl.Op, afcOpFileClose)
	}
	if h := binary.LittleEndian.Uint64(cl.Body); h != handle {
		t.Fatalf("close handle = %#x, want %#x", h, handle)
	}
	device.writeStatus(afcStatusSuccess)

	res := <-done
	if res.err != nil {
		t.Fatalf("uploadPackage: %v", res.err)
	}
	if res.remote != stagingDir+"/Runner.ipa" {
		t.Fatalf("staged path = %q, want %s/Runner.ipa", res.remote, stagingDir)
	}
}

// TestAFCWriteErrorPropagates ensures a device-side write failure (a full disk
// is the realistic case) surfaces as the named AFC status.
func TestAFCWriteErrorPropagates(t *testing.T) {
	afc, device := newAFCPair(t)

	dir := t.TempDir()
	ipa := filepath.Join(dir, "big.ipa")
	if err := os.WriteFile(ipa, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() {
		_, err := afc.uploadPackage(ipa, nil)
		errc <- err
	}()

	device.read()
	device.writeStatus(afcStatusSuccess)
	device.read()
	device.writeStatus(afcStatusSuccess)
	device.read()
	openRes := make([]byte, 8)
	binary.LittleEndian.PutUint64(openRes, 7)
	device.writePacket(afcOpFileOpenRes, openRes)
	device.read()
	device.writeStatus(18) // no space left on device
	device.read()          // the client still closes its handle
	device.writeStatus(afcStatusSuccess)

	err := <-errc
	if err == nil {
		t.Fatal("expected an error when the device reports a write failure")
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("error = %v, want it to name the AFC status", err)
	}
}

// TestAFCOpenRejection covers a device that refuses the open outright: the
// client must not then treat the status packet as a file handle.
func TestAFCOpenRejection(t *testing.T) {
	afc, device := newAFCPair(t)

	errc := make(chan error, 1)
	go func() {
		_, err := afc.open("PublicStaging/x.ipa", afcModeWrOnly)
		errc <- err
	}()

	device.read()
	device.writeStatus(10) // permission denied

	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want permission denied", err)
	}
}

// plistPeer is the device end of a length-prefixed plist service.
type plistPeer struct {
	t    *testing.T
	conn net.Conn
}

func (p *plistPeer) read() map[string]any {
	p.t.Helper()
	var length [4]byte
	if _, err := io.ReadFull(p.conn, length[:]); err != nil {
		p.t.Fatalf("device read length: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(length[:]))
	if _, err := io.ReadFull(p.conn, body); err != nil {
		p.t.Fatalf("device read body: %v", err)
	}
	var out map[string]any
	if _, err := plist.Unmarshal(body, &out); err != nil {
		p.t.Fatalf("device decode: %v", err)
	}
	return out
}

func (p *plistPeer) write(v any) {
	p.t.Helper()
	body, err := plist.Marshal(v, plist.XMLFormat)
	if err != nil {
		p.t.Fatalf("device encode: %v", err)
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if _, err := p.conn.Write(frame); err != nil {
		p.t.Fatalf("device write: %v", err)
	}
}

func newPlistPair(t *testing.T) (*service, *plistPeer) {
	t.Helper()
	client, device := net.Pipe()
	t.Cleanup(func() { client.Close(); device.Close() })
	return &service{plistConn: plistConn{conn: client}}, &plistPeer{t: t, conn: device}
}

// TestInstallProxyProgressAndCompletion asserts the request installd receives
// (Developer package type, the staged relative path) and that the progress
// stream is pumped until the terminal Complete message.
func TestInstallProxyProgressAndCompletion(t *testing.T) {
	svc, device := newPlistPair(t)
	proxy := newInstallProxy(svc)

	errc := make(chan error, 1)
	var seen []string
	go func() {
		errc <- proxy.install("Install", "PublicStaging/Runner.ipa", func(status string, percent int) {
			seen = append(seen, status)
		})
	}()

	req := device.read()
	if req["Command"] != "Install" {
		t.Fatalf("Command = %v, want Install", req["Command"])
	}
	if req["PackagePath"] != "PublicStaging/Runner.ipa" {
		t.Fatalf("PackagePath = %v, want the staged relative path", req["PackagePath"])
	}
	opts, ok := req["ClientOptions"].(map[string]any)
	if !ok {
		t.Fatalf("ClientOptions missing, got %#v", req["ClientOptions"])
	}
	// Without this installd treats the payload as an App Store package and
	// rejects a developer signature.
	if opts["PackageType"] != "Developer" {
		t.Fatalf("PackageType = %v, want Developer", opts["PackageType"])
	}

	device.write(map[string]any{"Status": "CreatingStagingDirectory", "PercentComplete": 5})
	device.write(map[string]any{"Status": "VerifyingApplication", "PercentComplete": 40})
	device.write(map[string]any{"Status": "Complete", "PercentComplete": 100})

	if err := <-errc; err != nil {
		t.Fatalf("install: %v", err)
	}
	want := []string{"CreatingStagingDirectory", "VerifyingApplication", "Complete"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("progress = %v, want %v", seen, want)
	}
}

// TestInstallProxyErrorMapping checks the installd error triple is preserved
// and that a signing rejection carries the actionable hint, because that is the
// failure a developer actually hits.
func TestInstallProxyErrorMapping(t *testing.T) {
	svc, device := newPlistPair(t)
	proxy := newInstallProxy(svc)

	errc := make(chan error, 1)
	go func() { errc <- proxy.install("Install", "PublicStaging/Runner.ipa", nil) }()

	device.read()
	device.write(map[string]any{
		"Error":            "ApplicationVerificationFailed",
		"ErrorDescription": "Failed to verify code signature",
		"ErrorDetail":      -402620394,
	})

	err := <-errc
	var ie *InstallError
	if !errors.As(err, &ie) {
		t.Fatalf("error = %T (%v), want *InstallError", err, err)
	}
	if ie.Name != "ApplicationVerificationFailed" {
		t.Fatalf("Name = %q", ie.Name)
	}
	if ie.Description != "Failed to verify code signature" {
		t.Fatalf("Description = %q", ie.Description)
	}
	if ie.Detail != -402620394 {
		t.Fatalf("Detail = %d, want the raw installd detail", ie.Detail)
	}
	msg := ie.Error()
	for _, want := range []string{"Failed to verify code signature", "ApplicationVerificationFailed", "provisioning profile"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

// TestInstallProxyUninstallUsesApplicationIdentifier pins the one asymmetry in
// the installd API: Uninstall is keyed by bundle id, not by a package path.
func TestInstallProxyUninstallUsesApplicationIdentifier(t *testing.T) {
	svc, device := newPlistPair(t)
	proxy := newInstallProxy(svc)

	errc := make(chan error, 1)
	go func() { errc <- proxy.uninstall("com.example.app", nil) }()

	req := device.read()
	if req["Command"] != "Uninstall" {
		t.Fatalf("Command = %v", req["Command"])
	}
	if req["ApplicationIdentifier"] != "com.example.app" {
		t.Fatalf("ApplicationIdentifier = %v, want the bundle id", req["ApplicationIdentifier"])
	}
	if _, ok := req["PackagePath"]; ok {
		t.Fatal("Uninstall must not send PackagePath")
	}
	device.write(map[string]any{"Status": "Complete"})
	if err := <-errc; err != nil {
		t.Fatalf("uninstall: %v", err)
	}
}

// TestInstallProxyLookupMissingApp distinguishes "not installed" (empty result,
// no error) from a real failure; LaunchApp depends on that distinction.
func TestInstallProxyLookupMissingApp(t *testing.T) {
	svc, device := newPlistPair(t)
	proxy := newInstallProxy(svc)

	type result struct {
		app *AppInfo
		err error
	}
	done := make(chan result, 1)
	go func() {
		app, err := proxy.lookup("com.example.absent")
		done <- result{app, err}
	}()

	req := device.read()
	opts := req["ClientOptions"].(map[string]any)
	ids, ok := opts["BundleIDs"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "com.example.absent" {
		t.Fatalf("BundleIDs = %#v, want the queried bundle id", opts["BundleIDs"])
	}
	device.write(map[string]any{"LookupResult": map[string]any{}, "Status": "Complete"})

	res := <-done
	if res.err != nil {
		t.Fatalf("lookup: %v", res.err)
	}
	if res.app != nil {
		t.Fatalf("app = %+v, want nil for an app that is not installed", res.app)
	}
}

// TestInstallProxyLookupDecodesRecord covers the populated case, which is what
// LaunchApp feeds to processControl.
func TestInstallProxyLookupDecodesRecord(t *testing.T) {
	svc, device := newPlistPair(t)
	proxy := newInstallProxy(svc)

	type result struct {
		app *AppInfo
		err error
	}
	done := make(chan result, 1)
	go func() {
		app, err := proxy.lookup("com.example.app")
		done <- result{app, err}
	}()

	device.read()
	device.write(map[string]any{
		"LookupResult": map[string]any{
			"com.example.app": map[string]any{
				"CFBundleIdentifier":         "com.example.app",
				"CFBundleDisplayName":        "Example",
				"CFBundleShortVersionString": "1.2.3",
				"CFBundleVersion":            "42",
				"CFBundleExecutable":         "Runner",
				"Path":                       "/private/var/containers/Bundle/Application/X/Runner.app",
				"Container":                  "/private/var/mobile/Containers/Data/Application/Y",
			},
		},
	})

	res := <-done
	if res.err != nil {
		t.Fatalf("lookup: %v", res.err)
	}
	if res.app == nil {
		t.Fatal("app = nil, want a record")
	}
	if res.app.Path == "" || res.app.BundleID != "com.example.app" || res.app.Version != "1.2.3" {
		t.Fatalf("decoded record = %+v", *res.app)
	}
}

// TestLockdownStartServiceHonoursSSLFlag pins the EnableServiceSSL contract:
// when the device does not ask for TLS the client must stay in cleartext, and
// when it does ask, a client without pairing certificates must refuse rather
// than silently speak plaintext to a TLS port (which just hangs).
func TestLockdownStartServiceHonoursSSLFlag(t *testing.T) {
	// A real StartService needs a usbmuxd Connect for the returned port, so
	// drive the reply decoding directly against the lockdown framing.
	client, device := net.Pipe()
	t.Cleanup(func() { client.Close(); device.Close() })
	ld := &lockdownConn{plistConn: plistConn{conn: client}, sessionID: "session", tls: nil}
	peer := &plistPeer{t: t, conn: device}

	errc := make(chan error, 1)
	go func() {
		_, err := ld.StartService(installProxyService)
		errc <- err
	}()

	req := peer.read()
	if req["Request"] != "StartService" {
		t.Fatalf("Request = %v", req["Request"])
	}
	if req["Service"] != installProxyService {
		t.Fatalf("Service = %v, want %s", req["Service"], installProxyService)
	}
	peer.write(map[string]any{"Request": "StartService", "Port": 49152, "EnableServiceSSL": true})

	err := <-errc
	if err == nil {
		t.Fatal("expected StartService to fail: SSL demanded but no pair certificates")
	}
	if !strings.Contains(err.Error(), "requires SSL") {
		t.Fatalf("error = %v, want it to explain the SSL requirement", err)
	}
}

// TestLockdownStartServiceRequiresSession guards the ordering rule: lockdownd
// rejects StartService outside a session, so the client refuses locally with a
// clearer message.
func TestLockdownStartServiceRequiresSession(t *testing.T) {
	client, device := net.Pipe()
	t.Cleanup(func() { client.Close(); device.Close() })
	ld := &lockdownConn{plistConn: plistConn{conn: client}}

	if _, err := ld.StartService(afcServiceName); err == nil {
		t.Fatal("expected StartService without a session to fail")
	} else if !strings.Contains(err.Error(), "no active lockdownd session") {
		t.Fatalf("error = %v", err)
	}
}

// TestLockdownErrorHints checks the lockdownd error names a user can act on
// carry the fix, especially a stale pairing record.
func TestLockdownErrorHints(t *testing.T) {
	client, device := net.Pipe()
	t.Cleanup(func() { client.Close(); device.Close() })
	ld := &lockdownConn{plistConn: plistConn{conn: client}, device: muxDevice{UDID: "UDID"}}
	peer := &plistPeer{t: t, conn: device}
	certPEM, keyPEM := testPairCerts(t)

	errc := make(chan error, 1)
	go func() {
		errc <- ld.StartSession(&PairRecord{
			HostID:          "host",
			SystemBUID:      "buid",
			HostCertificate: certPEM,
			HostPrivateKey:  keyPEM,
		})
	}()

	req := peer.read()
	if req["Request"] != "StartSession" {
		t.Fatalf("Request = %v", req["Request"])
	}
	if req["HostID"] != "host" || req["SystemBUID"] != "buid" {
		t.Fatalf("StartSession sent HostID=%v SystemBUID=%v, want the pair record values", req["HostID"], req["SystemBUID"])
	}
	peer.write(map[string]any{"Request": "StartSession", "Error": "InvalidHostID"})

	err := <-errc
	if err == nil {
		t.Fatal("expected StartSession to fail")
	}
	if !strings.Contains(err.Error(), "InvalidHostID") || !strings.Contains(err.Error(), "trust this computer") {
		t.Fatalf("error = %v, want the InvalidHostID hint", err)
	}
}

// TestArchiveRoundTrip checks the NSKeyedArchiver encoder against its own
// decoder for the value shapes the launch request carries.
func TestArchiveRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"string", "com.example.app"},
		{"bool", true},
		{"int", int64(1234)},
		{"empty dict", map[string]any{}},
		{"empty array", []any{}},
		{"launch options", map[string]any{"StartSuspendedKey": false, "KillExisting": true}},
		{"nested", map[string]any{"a": []any{"x", int64(2)}, "b": map[string]any{"c": "d"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := archive(tc.v)
			if err != nil {
				t.Fatalf("archive: %v", err)
			}
			// The device only accepts a real NSKeyedArchiver container.
			var envelope map[string]any
			if _, err := plist.Unmarshal(data, &envelope); err != nil {
				t.Fatalf("archive output is not a plist: %v", err)
			}
			if envelope["$archiver"] != "NSKeyedArchiver" {
				t.Fatalf("$archiver = %v", envelope["$archiver"])
			}
			objects, ok := envelope["$objects"].([]any)
			if !ok || len(objects) == 0 || objects[0] != "$null" {
				t.Fatalf("$objects malformed: %#v", envelope["$objects"])
			}

			got, err := unarchive(data)
			if err != nil {
				t.Fatalf("unarchive: %v", err)
			}
			if !equalValue(got, tc.v) {
				t.Fatalf("round trip = %#v, want %#v", got, tc.v)
			}
		})
	}
}

// TestArchiveRejectsUnsupported makes sure an unsupported input is a loud error
// rather than a silently archived null the device would misinterpret.
func TestArchiveRejectsUnsupported(t *testing.T) {
	if _, err := archive(struct{ A int }{1}); err == nil {
		t.Fatal("expected archive of an unsupported type to fail")
	}
}

// TestUnarchiveNSError decodes the failure shape the instruments services
// return, which is how a launch rejection reaches the user.
func TestUnarchiveNSError(t *testing.T) {
	a := &archiver{objects: []any{"$null"}, interned: map[string]plist.UID{}}
	desc, err := a.encode("Failed to launch")
	if err != nil {
		t.Fatal(err)
	}
	userInfo, err := a.encode(map[string]any{"NSLocalizedDescription": "Failed to launch"})
	if err != nil {
		t.Fatal(err)
	}
	_ = desc
	root := a.reserve()
	a.objects[root] = map[string]any{
		"$class":     a.class("NSError", "NSObject"),
		"NSCode":     int64(3),
		"NSDomain":   a.mustEncode(t, "FBSOpenApplicationServiceErrorDomain"),
		"NSUserInfo": userInfo,
	}
	data, err := plist.Marshal(map[string]any{
		"$version":  nsKeyedArchiverVersion,
		"$archiver": "NSKeyedArchiver",
		"$top":      map[string]any{"root": root},
		"$objects":  a.objects,
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}

	got, err := unarchive(data)
	if err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	nsErr, ok := got.(*NSError)
	if !ok {
		t.Fatalf("decoded %T, want *NSError", got)
	}
	if nsErr.Code != 3 || nsErr.Domain != "FBSOpenApplicationServiceErrorDomain" {
		t.Fatalf("decoded = %+v", *nsErr)
	}
	if !strings.Contains(nsErr.Error(), "Failed to launch") {
		t.Fatalf("Error() = %q, want the localized description", nsErr.Error())
	}
}

// TestLaunchedPIDDecodesProcessToken covers the success shape of a CoreDevice
// launchapplication reply, which is where LaunchApp's returned pid comes from.
func TestLaunchedPIDDecodesProcessToken(t *testing.T) {
	pid, err := launchedPID(map[string]any{
		"CoreDevice.output": map[string]any{
			"processToken": map[string]any{
				"processIdentifier": int64(1234),
				"executableURL":     map[string]any{"relative": "file:///private/var/x/Runner.app"},
			},
		},
	})
	if err != nil {
		t.Fatalf("launchedPID: %v", err)
	}
	if pid != 1234 {
		t.Fatalf("pid = %d, want 1234", pid)
	}
}

// TestLaunchedPIDReportsLockedDevice pins the rendering of a real CoreDevice
// refusal. The payload below is exactly what iPhone14,2 on iOS 26.6.1 returned
// for a launch attempted against a locked screen.
//
// Two things must hold. The keys are lowercase ("code", "domain", "userInfo"),
// not the NSError capitals, so a reader that only knows the capitals falls
// through and dumps a raw Go map at the user. And the actionable reason is not
// in the outermost description ("The application failed to launch.") but nested
// two levels down under NSUnderlyingError, so the chain has to be walked.
func TestLaunchedPIDReportsLockedDevice(t *testing.T) {
	const bundleID = "com.example.ripley.fixture"
	reply := map[string]any{
		"CoreDevice.error": map[string]any{
			"code":   int64(10002),
			"domain": "com.apple.dt.CoreDeviceError",
			"userInfo": map[string]any{
				"BundleIdentifier":       bundleID,
				"NSLocalizedDescription": "The application failed to launch.",
				"NSUnderlyingError": map[string]any{
					"code":   int64(1),
					"domain": "FBSOpenApplicationServiceErrorDomain",
					"userInfo": map[string]any{
						"BSErrorCodeDescription": "RequestDenied",
						"NSLocalizedDescription": `The request to open "` + bundleID + `" failed.`,
						"NSLocalizedFailureReason": `The request was denied by service delegate (SBMainWorkspace) ` +
							`for reason: Locked ("Unable to launch ` + bundleID +
							` because the device was not, or could not be, unlocked").`,
						"NSUnderlyingError": map[string]any{
							"code":   int64(7),
							"domain": "FBSOpenApplicationErrorDomain",
							"userInfo": map[string]any{
								"BSErrorCodeDescription": "Locked",
								"NSLocalizedFailureReason": "Unable to launch " + bundleID +
									" because the device was not, or could not be, unlocked.",
							},
						},
					},
				},
			},
		},
	}

	pid, err := launchedPID(reply)
	if err == nil {
		t.Fatalf("launchedPID returned pid %d, want an error for a refused launch", pid)
	}
	msg := err.Error()
	if strings.Contains(msg, "map[") {
		t.Fatalf("error dumps a raw Go map instead of the device's reason:\n%s", msg)
	}
	// The cause, not just the generic outer description.
	if !strings.Contains(msg, "could not be, unlocked") {
		t.Fatalf("error does not name the underlying reason:\n%s", msg)
	}
	// And the fix, because "Locked" alone does not tell a developer what to do.
	if !strings.Contains(msg, "unlock the device") {
		t.Fatalf("error does not tell the user to unlock the device:\n%s", msg)
	}
}

func (a *archiver) mustEncode(t *testing.T, v any) plist.UID {
	t.Helper()
	uid, err := a.encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

// equalValue compares decoded values structurally, tolerating the integer
// widening the plist round trip performs.
func equalValue(got, want any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case string:
		g, ok := got.(string)
		return ok && g == w
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case int64:
		g, ok := toInt64(got)
		return ok && g == w
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !equalValue(g[i], w[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok || !equalValue(gv, wv) {
				return false
			}
		}
		return true
	}
	return false
}
