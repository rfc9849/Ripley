package idevice

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestPersonalizedImageMountedFallsBackToCopyDevices(t *testing.T) {
	svc, device := newPlistPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lookup := device.read()
		if lookup["Command"] != "LookupImage" || lookup["ImageType"] != personalizedImageType {
			t.Errorf("LookupImage request = %#v", lookup)
			return
		}
		// iOS 27 can report the image as present but leave ImageSignature empty.
		device.write(map[string]any{"ImagePresent": true, "ImageSignature": []any{}})
		copyDevices := device.read()
		if copyDevices["Command"] != "CopyDevices" {
			t.Errorf("second request = %#v, want CopyDevices", copyDevices)
			return
		}
		device.write(map[string]any{"EntryList": []any{
			map[string]any{"DiskImageType": personalizedImageType, "MountPath": "/System/Developer"},
		}})
	}()

	mounted, err := personalizedImageMounted(svc)
	if err != nil {
		t.Fatal(err)
	}
	if !mounted {
		t.Fatal("mounted = false, want true from CopyDevices fallback")
	}
	<-done
}

func TestDDITSSRequestAppliesRestoreRules(t *testing.T) {
	identity := ddiBuildIdentity{
		BoardID: "0x0c",
		ChipID:  "0x8110",
		Manifest: map[string]ddiManifestEntry{
			"PersonalizedDMG": {
				Digest:  bytes.Repeat([]byte{1}, 48),
				Trusted: true,
				Name:    developerDiskImageType,
			},
			"LoadableTrustCache": {
				Digest:  bytes.Repeat([]byte{2}, 48),
				Trusted: true,
			},
		},
	}
	trust := identity.Manifest["LoadableTrustCache"]
	trust.Info.RestoreRequestRules = []ddiRestoreRequestRule{
		{Conditions: map[string]any{"ApCurrentProductionMode": true, "ApRequiresImage4": true}, Actions: map[string]any{"EPRO": true}},
		{Conditions: map[string]any{"ApRawSecurityMode": true, "ApRequiresImage4": true}, Actions: map[string]any{"ESEC": true}},
	}
	identity.Manifest["LoadableTrustCache"] = trust

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request map[string]any
		if _, err := plist.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		for _, key := range []string{"PersonalizedDMG", "LoadableTrustCache"} {
			component, ok := request[key].(map[string]any)
			if !ok {
				t.Errorf("component %s missing: %#v", key, request[key])
				continue
			}
			if component["EPRO"] != true || component["ESEC"] != true {
				t.Errorf("component %s restore flags = %#v, want EPRO/ESEC true", key, component)
			}
		}
		responsePlist, _ := plist.Marshal(map[string]any{"ApImg4Ticket": []byte("ticket")}, plist.XMLFormat)
		_, _ = w.Write(append([]byte("STATUS=0&MESSAGE=SUCCESS&REQUEST_STRING="), responsePlist...))
	}))
	defer server.Close()

	oldEndpoint, oldClient := tssEndpoint, tssHTTPClient
	tssEndpoint, tssHTTPClient = server.URL, server.Client()
	t.Cleanup(func() { tssEndpoint, tssHTTPClient = oldEndpoint, oldClient })

	ticket, err := requestDDITicket(identity, ddiIdentifiers{
		BoardID:        0x0c,
		ChipID:         0x8110,
		SecurityDomain: 1,
		Additional:     map[string]any{"Ap,SikaFuse": uint64(0)},
	}, bytes.Repeat([]byte{3}, 48), 1234)
	if err != nil {
		t.Fatal(err)
	}
	if string(ticket) != "ticket" {
		t.Fatalf("ticket = %q", ticket)
	}
}

func TestSelectDDIIdentityMatchesBoardAndChip(t *testing.T) {
	manifest := ddiBuildManifest{ProductBuildVersion: "test", BuildIdentities: []ddiBuildIdentity{
		{BoardID: "0x08", ChipID: "0x8110"},
		{BoardID: "0x0C", ChipID: "0x8110"},
	}}
	got, err := selectDDIIdentity(manifest, ddiIdentifiers{BoardID: 0x0c, ChipID: 0x8110})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got.BoardID, "0x0c") {
		t.Fatalf("selected board = %s", got.BoardID)
	}
}

func TestParseTSSResponseKeepsWholePlistPayload(t *testing.T) {
	payload, _ := plist.Marshal(map[string]any{"ApImg4Ticket": []byte("abc")}, plist.XMLFormat)
	status, message, got, err := parseTSSResponse(append([]byte("STATUS=0&MESSAGE=SUCCESS&REQUEST_STRING="), payload...))
	if err != nil {
		t.Fatal(err)
	}
	if status != 0 || message != "SUCCESS" || !bytes.Equal(got, payload) {
		t.Fatalf("status=%d message=%q payloadEqual=%v", status, message, bytes.Equal(got, payload))
	}
}

func TestUploadDDIStreamsRawBytesBetweenPlists(t *testing.T) {
	svc, device := newPlistPair(t)
	payload := bytes.Repeat([]byte("ddi"), 4096)
	path := filepath.Join(t.TempDir(), "Image.dmg")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := device.read()
		if req["Command"] != "ReceiveBytes" || req["ImageType"] != personalizedImageType {
			t.Errorf("ReceiveBytes request = %#v", req)
			return
		}
		device.write(map[string]any{"Status": "ReceiveBytesAck"})
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(device.conn, got); err != nil {
			t.Errorf("read raw image: %v", err)
			return
		}
		if !bytes.Equal(got, payload) {
			t.Error("raw image payload differs")
			return
		}
		device.write(map[string]any{"Status": "Complete"})
	}()
	if err := uploadDDI(svc, path, []byte("ticket"), func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestMountPersonalizedDDISendsTrustCache(t *testing.T) {
	svc, device := newPlistPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := device.read()
		if req["Command"] != "MountImage" || req["ImageType"] != personalizedImageType {
			t.Errorf("MountImage request = %#v", req)
			return
		}
		if got, _ := req["ImageSignature"].([]byte); string(got) != "ticket" {
			t.Errorf("ImageSignature = %q", got)
		}
		if got, _ := req["ImageTrustCache"].([]byte); string(got) != "trust" {
			t.Errorf("ImageTrustCache = %q", got)
		}
		device.write(map[string]any{"Status": "Complete"})
	}()
	if err := mountPersonalizedDDI(svc, []byte("ticket"), []byte("trust")); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestDDICacheRootHonorsRipleyHomeAndExplicitOverride(t *testing.T) {
	t.Setenv("RIPLEY_DDI_CACHE", "")
	t.Setenv("RIPLEY_HOME", filepath.Join(t.TempDir(), "ripley-home"))
	got, err := ddiCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(os.Getenv("RIPLEY_HOME"), "ddi")
	if got != want {
		t.Fatalf("ddiCacheRoot() = %q, want %q", got, want)
	}

	override := filepath.Join(t.TempDir(), "custom-ddi-cache")
	t.Setenv("RIPLEY_DDI_CACHE", override)
	got, err = ddiCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != override {
		t.Fatalf("explicit RIPLEY_DDI_CACHE = %q, want %q", got, override)
	}
}
