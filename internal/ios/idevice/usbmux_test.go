package idevice

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

// fakeMux is an in-process usbmuxd. It speaks the real framing (16 byte little
// endian header + XML plist) so the client code under test is exercised exactly
// as it would be against usbmuxd itself.
type fakeMux struct {
	t    *testing.T
	ln   net.Listener
	path string

	// handle answers one request. Returning a nil reply closes the connection
	// without answering, which is how a raw device pipe is simulated: after
	// Connect the socket stops being framed.
	handle func(conn net.Conn, req map[string]any) (reply any, keepOpen bool)
}

// startFakeMux listens on a short-path unix socket and points
// USBMUXD_SOCKET_ADDRESS at it for the duration of the test.
func startFakeMux(t *testing.T, handle func(conn net.Conn, req map[string]any) (any, bool)) *fakeMux {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes; t.TempDir() embeds the test
	// name and on macOS is already close to that, so use a short temp root.
	// An empty dir argument honours TMPDIR and falls back to /tmp.
	dir, err := os.MkdirTemp("", "flmux")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Setenv("USBMUXD_SOCKET_ADDRESS", "UNIX:"+sock)
	t.Cleanup(func() {
		ln.Close()
		os.RemoveAll(dir)
	})

	f := &fakeMux{t: t, ln: ln, path: sock, handle: handle}
	go f.serve()
	return f
}

func (f *fakeMux) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.session(conn)
	}
}

func (f *fakeMux) session(conn net.Conn) {
	for {
		req, err := readMuxFrame(conn)
		if err != nil {
			conn.Close()
			return
		}
		reply, keepOpen := f.handle(conn, req)
		if reply != nil {
			if err := writeMuxFrame(conn, reply); err != nil {
				conn.Close()
				return
			}
		}
		if !keepOpen {
			// Leave the socket open so the client can keep using it as a device
			// pipe; the client owns closing it.
			return
		}
	}
}

func readMuxFrame(conn net.Conn) (map[string]any, error) {
	var hdr [muxHeaderSize]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(hdr[0:])
	if length < muxHeaderSize {
		return nil, fmt.Errorf("bad length %d", length)
	}
	if v := binary.LittleEndian.Uint32(hdr[4:]); v != muxVersionPlist {
		return nil, fmt.Errorf("bad version %d", v)
	}
	if m := binary.LittleEndian.Uint32(hdr[8:]); m != muxMessagePlist {
		return nil, fmt.Errorf("bad message type %d", m)
	}
	body := make([]byte, length-muxHeaderSize)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	var req map[string]any
	if _, err := plist.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return req, nil
}

func writeMuxFrame(conn net.Conn, payload any) error {
	body, err := plist.Marshal(payload, plist.XMLFormat)
	if err != nil {
		return err
	}
	buf := make([]byte, muxHeaderSize+len(body))
	binary.LittleEndian.PutUint32(buf[0:], uint32(muxHeaderSize+len(body)))
	binary.LittleEndian.PutUint32(buf[4:], muxVersionPlist)
	binary.LittleEndian.PutUint32(buf[8:], muxMessagePlist)
	binary.LittleEndian.PutUint32(buf[12:], 1)
	copy(buf[muxHeaderSize:], body)
	_, err = conn.Write(buf)
	return err
}

func TestSocketAddress(t *testing.T) {
	tests := []struct {
		spec        string
		wantNetwork string
		wantAddress string
	}{
		{"", "unix", DefaultSocket},
		{"UNIX:/tmp/muxsock", "unix", "/tmp/muxsock"},
		{"127.0.0.1:27015", "tcp", "127.0.0.1:27015"},
		{"/run/usbmuxd", "unix", "/run/usbmuxd"},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			t.Setenv("USBMUXD_SOCKET_ADDRESS", tc.spec)
			network, address := SocketAddress()
			if network != tc.wantNetwork || address != tc.wantAddress {
				t.Fatalf("SocketAddress() = %q %q, want %q %q", network, address, tc.wantNetwork, tc.wantAddress)
			}
		})
	}
}

// A missing socket must produce an error a Linux user can act on, naming both
// the socket that was tried and the package that provides it. This is the very
// first thing every Linux user without usbmuxd hits.
func TestListDevicesWithoutUSBMuxd(t *testing.T) {
	dir, err := os.MkdirTemp("", "flmux")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	missing := filepath.Join(dir, "absent")
	t.Setenv("USBMUXD_SOCKET_ADDRESS", "UNIX:"+missing)

	_, err = ListDevices()
	if err == nil {
		t.Fatal("expected an error when the usbmuxd socket is absent")
	}
	if !errors.Is(err, ErrNoUSBMuxd) {
		t.Errorf("error does not wrap ErrNoUSBMuxd: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{missing, "apt install usbmuxd", "USBMUXD_SOCKET_ADDRESS"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

// ListDevices against a running usbmuxd with nothing plugged in is not an
// error: it is an empty result.
func TestListDevicesEmpty(t *testing.T) {
	startFakeMux(t, func(_ net.Conn, req map[string]any) (any, bool) {
		if req["MessageType"] != "ListDevices" {
			t.Errorf("unexpected request %v", req)
		}
		return map[string]any{"DeviceList": []any{}}, false
	})

	devices, err := ListDevices()
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("expected no devices, got %d", len(devices))
	}
	if devices == nil {
		t.Error("expected a non-nil empty slice")
	}
}

func TestListDevicesRequestEncodingAndDecoding(t *testing.T) {
	var gotReq map[string]any
	startFakeMux(t, func(_ net.Conn, req map[string]any) (any, bool) {
		// ListDevices enriches each device by dialling lockdownd through a
		// Connect on a fresh socket. Refuse those so the enrichment fails fast
		// and the usbmuxd-level decoding stays the subject of this test.
		if req["MessageType"] == "Connect" {
			return map[string]any{"MessageType": "Result", "Number": uint64(3)}, false
		}
		gotReq = req
		return map[string]any{"DeviceList": []any{
			map[string]any{
				"MessageType": "Attached",
				"DeviceID":    uint64(7),
				"Properties": map[string]any{
					"ConnectionType": "USB",
					"DeviceID":       uint64(7),
					// 24 characters, no dash: the modern form with the
					// separator stripped.
					"SerialNumber": "000081100000000000000000",
				},
			},
			// A network duplicate of the same device must collapse into one
			// entry rather than being listed twice.
			map[string]any{
				"MessageType": "Attached",
				"DeviceID":    uint64(8),
				"Properties": map[string]any{
					"ConnectionType": "Network",
					"DeviceID":       uint64(8),
					"SerialNumber":   "000081100000000000000000",
				},
			},
			// Junk entries without an identity are dropped.
			map[string]any{"MessageType": "Attached", "DeviceID": uint64(0)},
		}}, false
	})

	devices, err := ListDevices()
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}

	// The request must carry the client identification usbmuxd expects.
	if gotReq["MessageType"] != "ListDevices" {
		t.Errorf("MessageType = %v", gotReq["MessageType"])
	}
	if gotReq["ClientVersionString"] == nil || gotReq["ProgName"] == nil || gotReq["kLibUSBMuxVersion"] == nil {
		t.Errorf("request is missing client identification: %v", gotReq)
	}

	if len(devices) != 1 {
		t.Fatalf("expected 1 unique device, got %d: %+v", len(devices), devices)
	}
	// The dash must be restored in the 24-character serial.
	if got, want := devices[0].UDID, "00008110-0000000000000000"; got != want {
		t.Errorf("UDID = %q, want %q", got, want)
	}
}

func TestNormalizeUDID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"000081100000000000000000", "00008110-0000000000000000"},
		{"00008110-0000000000000000", "00008110-0000000000000000"},
		// The legacy 40-character UDID has no separator and must be untouched.
		{strings.Repeat("a", 40), strings.Repeat("a", 40)},
	}
	for _, tc := range tests {
		if got := normalizeUDID(tc.in); got != tc.want {
			t.Errorf("normalizeUDID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// usbmuxd expects the device port in network byte order inside a host-order
// field. Getting this wrong silently connects to the wrong port.
func TestConnectDevicePortByteOrder(t *testing.T) {
	var gotPort uint64
	var gotDeviceID uint64
	startFakeMux(t, func(_ net.Conn, req map[string]any) (any, bool) {
		gotPort, _ = toUint64(req["PortNumber"])
		gotDeviceID, _ = toUint64(req["DeviceID"])
		return map[string]any{"MessageType": "Result", "Number": uint64(0)}, true
	})

	conn, err := connectDevice(42, lockdownPort)
	if err != nil {
		t.Fatalf("connectDevice: %v", err)
	}
	conn.Close()

	if gotDeviceID != 42 {
		t.Errorf("DeviceID = %d, want 42", gotDeviceID)
	}
	// 62078 == 0xF27E, byte-swapped 0x7EF2 == 32498.
	if gotPort != 32498 {
		t.Errorf("PortNumber = %d, want 32498 (byte-swapped %d)", gotPort, lockdownPort)
	}
}

func TestConnectDeviceResultErrors(t *testing.T) {
	tests := []struct {
		number uint64
		want   string
	}{
		{2, "device not connected"},
		{3, "connection refused"},
		{6, "bad request"},
		{99, "result 99"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			startFakeMux(t, func(_ net.Conn, _ map[string]any) (any, bool) {
				return map[string]any{"MessageType": "Result", "Number": tc.number}, false
			})
			_, err := connectDevice(1, lockdownPort)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The pair record must come from usbmuxd, never from the root-only lockdown
// directory.
func TestReadPairRecord(t *testing.T) {
	record := map[string]any{
		"HostID":            "1B2C3D4E-5F60-7182-9394-A5B6C7D8E9FA",
		"SystemBUID":        "AABBCCDD-1122-3344-5566-778899AABBCC",
		"HostCertificate":   []byte("-----BEGIN CERTIFICATE-----host"),
		"HostPrivateKey":    []byte("-----BEGIN RSA PRIVATE KEY-----host"),
		"DeviceCertificate": []byte("-----BEGIN CERTIFICATE-----device"),
		"RootCertificate":   []byte("-----BEGIN CERTIFICATE-----root"),
	}
	blob, err := plist.Marshal(record, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}

	var gotReq map[string]any
	startFakeMux(t, func(_ net.Conn, req map[string]any) (any, bool) {
		gotReq = req
		return map[string]any{"PairRecordData": blob}, false
	})

	const udid = "00008110-0000000000000000"
	rec, err := readPairRecord(udid)
	if err != nil {
		t.Fatalf("readPairRecord: %v", err)
	}
	if gotReq["MessageType"] != "ReadPairRecord" {
		t.Errorf("MessageType = %v", gotReq["MessageType"])
	}
	if gotReq["PairRecordID"] != udid {
		t.Errorf("PairRecordID = %v, want %s", gotReq["PairRecordID"], udid)
	}
	if rec.HostID != record["HostID"] {
		t.Errorf("HostID = %q", rec.HostID)
	}
	if string(rec.HostCertificate) != string(record["HostCertificate"].([]byte)) {
		t.Errorf("HostCertificate not round-tripped")
	}
}

// An unpaired device must say so in terms a user can act on, because this is
// what every first-time Linux user sees before tapping Trust.
func TestReadPairRecordUnpaired(t *testing.T) {
	startFakeMux(t, func(_ net.Conn, _ map[string]any) (any, bool) {
		return map[string]any{"MessageType": "Result", "Number": uint64(6)}, false
	})

	_, err := readPairRecord("00008110-0000000000000000")
	if err == nil {
		t.Fatal("expected an error for a device with no pair record")
	}
	if !errors.Is(err, ErrNoPairRecord) {
		t.Errorf("error does not wrap ErrNoPairRecord: %v", err)
	}
	if !strings.Contains(err.Error(), "Trust") {
		t.Errorf("error should tell the user to trust the computer: %v", err)
	}
}

// A pair record without a usable host identity cannot open a TLS session, and
// saying so up front beats a confusing handshake failure later.
func TestParsePairRecordRejectsIncomplete(t *testing.T) {
	blob, err := plist.Marshal(map[string]any{"SystemBUID": "x"}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePairRecord(blob); err == nil {
		t.Fatal("expected an error for a record with no HostID")
	}

	blob, err = plist.Marshal(map[string]any{"HostID": "h"}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	_, err = parsePairRecord(blob)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a certificate complaint, got %v", err)
	}
}

func TestMuxFrameRejectsForeignProtocol(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Announce the binary (pre-plist) protocol, which this client never speaks.
	go func() {
		buf := make([]byte, muxHeaderSize)
		binary.LittleEndian.PutUint32(buf[0:], muxHeaderSize)
		binary.LittleEndian.PutUint32(buf[4:], 0)
		binary.LittleEndian.PutUint32(buf[8:], 3)
		server.Write(buf)
	}()

	m := &muxConn{conn: client}
	var out map[string]any
	err := m.receive(&out)
	if err == nil || !strings.Contains(err.Error(), "plist protocol") {
		t.Fatalf("expected a protocol complaint, got %v", err)
	}
}

func toUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case int64:
		return uint64(n), true
	case int:
		return uint64(n), true
	}
	return 0, false
}
