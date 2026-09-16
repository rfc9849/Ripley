package idevice

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"howett.net/plist"
)

const (
	mobileImageMounterService = "com.apple.mobile.mobile_image_mounter"
	personalizedImageType     = "Personalized"
	developerDiskImageType    = "DeveloperDiskImage"
	personalizedDDIBuildID    = "27A5228h"
)

// personalizedDDICommit pins the public DeveloperDiskImage repository rather
// than following a mutable branch. The three files are verified against their
// SHA-256 before they become cache entries.
const personalizedDDICommit = "5423e4e955fbb3a9eef3e1212acfbfc6e7a26236"

var personalizedDDIFiles = []ddiRemoteFile{
	{Name: "BuildManifest.plist", SHA256: "8edd4a2f4f4ef1fbd7bfe49785d8badc673d1395d1d94d85b132ca8ab5ecaf54"},
	{Name: "Image.dmg", SHA256: "05fd807da5e19f030fa4941f24800c965c6c77982ab572dd5d1ef778fb69f9ca"},
	{Name: "Image.dmg.trustcache", SHA256: "36af60889ff5a737874a26daeb8e1a0139ebfebec6ec2e4d8f6a3c1bf1dce35c"},
}

var (
	personalizedDDIBaseURL = "https://raw.githubusercontent.com/doronz88/DeveloperDiskImage/" + personalizedDDICommit + "/PersonalizedImages/Xcode_iOS_DDI_Personalized"
	ddiHTTPClient          = &http.Client{Timeout: 5 * time.Minute}
	tssHTTPClient          = &http.Client{Timeout: time.Minute}
	tssEndpoint            = "https://gs.apple.com/TSS/controller?action=2"
)

type ddiRemoteFile struct {
	Name   string
	SHA256 string
}

type ddiBuildManifest struct {
	ProductBuildVersion string             `plist:"ProductBuildVersion"`
	BuildIdentities     []ddiBuildIdentity `plist:"BuildIdentities"`
}

type ddiBuildIdentity struct {
	BoardID  string                      `plist:"ApBoardID"`
	ChipID   string                      `plist:"ApChipID"`
	Manifest map[string]ddiManifestEntry `plist:"Manifest"`
}

type ddiManifestEntry struct {
	Digest  []byte `plist:"Digest"`
	Trusted bool   `plist:"Trusted"`
	EPRO    *bool  `plist:"EPRO"`
	ESEC    *bool  `plist:"ESEC"`
	Name    string `plist:"Name"`
	Info    struct {
		Path                string                  `plist:"Path"`
		RestoreRequestRules []ddiRestoreRequestRule `plist:"RestoreRequestRules"`
	} `plist:"Info"`
}

type ddiRestoreRequestRule struct {
	Actions    map[string]any `plist:"Actions"`
	Conditions map[string]any `plist:"Conditions"`
}

type ddiIdentifiers struct {
	BoardID        int64
	ChipID         int64
	SecurityDomain int64
	Additional     map[string]any
}

// EnsureDeveloperImage makes the iOS 17+ Personalized Developer Disk Image
// available on the device. It is idempotent: an already-mounted image returns
// without touching the network. progress is optional and receives human-readable
// state transitions suitable for `ripley device launch`.
func EnsureDeveloperImage(udid string, progress func(string)) error {
	s, err := openSession(udid)
	if err != nil {
		return err
	}
	defer s.Close()
	if s.device.majorVersion() < 17 {
		return nil
	}
	return s.ensurePersonalizedDeveloperImage(progress)
}

func (s *session) ensurePersonalizedDeveloperImage(progress func(string)) error {
	report := func(format string, args ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, args...))
		}
	}

	svc, err := s.ld.StartService(mobileImageMounterService)
	if err != nil {
		return fmt.Errorf("start developer image mounter: %w", err)
	}
	mounted, err := personalizedImageMounted(svc)
	if err != nil {
		svc.Close()
		return fmt.Errorf("check Developer Disk Image: %w", err)
	}
	if mounted {
		svc.Close()
		return nil
	}

	mode, err := queryDeveloperMode(svc)
	if err != nil {
		svc.Close()
		return fmt.Errorf("query Developer Mode: %w", err)
	}
	if !mode {
		svc.Close()
		return errors.New("Developer Mode is disabled on the device; enable Settings > Privacy & Security > Developer Mode, reboot, and confirm the prompt")
	}

	restoreDir, err := resolvePersonalizedDDI(report)
	if err != nil {
		svc.Close()
		return err
	}
	manifest, err := loadDDIBuildManifest(filepath.Join(restoreDir, "BuildManifest.plist"))
	if err != nil {
		svc.Close()
		return err
	}
	identifiers, err := queryDDIIdentifiers(svc)
	if err != nil {
		svc.Close()
		return err
	}
	identity, err := selectDDIIdentity(manifest, identifiers)
	if err != nil {
		svc.Close()
		return err
	}
	dmgPath := filepath.Join(restoreDir, identity.componentPath("PersonalizedDMG", "PersonalizedDmg"))
	trustPath := filepath.Join(restoreDir, identity.componentPath("LoadableTrustCache"))
	if identity.componentPath("PersonalizedDMG", "PersonalizedDmg") == "" || identity.componentPath("LoadableTrustCache") == "" {
		svc.Close()
		return errors.New("Developer Disk Image manifest has no PersonalizedDMG or LoadableTrustCache path")
	}

	report("Preparing Developer Disk Image (%s)", manifest.ProductBuildVersion)
	ticket, err := queryCachedDDITicket(svc, dmgPath)
	if err != nil {
		// QueryPersonalizationManifest closes the service on a cache miss on
		// current iOS. Re-open it before asking for nonce/identifiers.
		svc.Close()
		svc, err = s.ld.StartService(mobileImageMounterService)
		if err != nil {
			return fmt.Errorf("reconnect developer image mounter: %w", err)
		}
		nonce, nonceErr := queryDDINonce(svc)
		if nonceErr != nil {
			svc.Close()
			return nonceErr
		}
		ecidValue, ecidErr := s.ld.GetValue("", "UniqueChipID")
		if ecidErr != nil {
			svc.Close()
			return fmt.Errorf("query device ECID: %w", ecidErr)
		}
		ecid, ok := anyUint64(ecidValue)
		if !ok {
			svc.Close()
			return fmt.Errorf("device ECID has unexpected type %T", ecidValue)
		}
		report("Personalizing Developer Disk Image with Apple TSS")
		ticket, err = requestDDITicket(identity, identifiers, nonce, ecid)
		if err != nil {
			svc.Close()
			return err
		}
	} else {
		report("Reusing device personalization manifest")
	}

	if err := uploadDDI(svc, dmgPath, ticket, report); err != nil {
		svc.Close()
		return err
	}
	trustCache, err := os.ReadFile(trustPath)
	if err != nil {
		svc.Close()
		return fmt.Errorf("read Developer Disk Image trust cache: %w", err)
	}
	if err := mountPersonalizedDDI(svc, ticket, trustCache); err != nil {
		svc.Close()
		return err
	}
	svc.Close()
	report("Developer Disk Image mounted")
	return nil
}

func personalizedImageMounted(svc *service) (bool, error) {
	if err := svc.send(map[string]any{"Command": "LookupImage", "ImageType": personalizedImageType}); err != nil {
		return false, err
	}
	var reply map[string]any
	if err := svc.receive(&reply); err != nil {
		return false, err
	}
	if present, ok := reply["ImagePresent"].(bool); ok && !present {
		return false, nil
	}
	if imageSignaturePresent(reply["ImageSignature"]) {
		return true, nil
	}

	// iOS 27 has been observed to return an empty ImageSignature from
	// LookupImage while a Personalized image is mounted. CopyDevices reports
	// the actual mount table and avoids an erroneous second MountImage.
	if err := svc.send(map[string]any{"Command": "CopyDevices"}); err != nil {
		return false, err
	}
	reply = nil
	if err := svc.receive(&reply); err != nil {
		return false, err
	}
	entries, _ := reply["EntryList"].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["DiskImageType"] == personalizedImageType {
			return true, nil
		}
	}
	return false, nil
}

func imageSignaturePresent(v any) bool {
	switch sig := v.(type) {
	case []byte:
		return len(sig) > 0
	case []any:
		return len(sig) > 0
	default:
		return false
	}
}

func queryDeveloperMode(svc *service) (bool, error) {
	if err := svc.send(map[string]any{"Command": "QueryDeveloperModeStatus"}); err != nil {
		return false, err
	}
	var reply map[string]any
	if err := svc.receive(&reply); err != nil {
		return false, err
	}
	mode, ok := reply["DeveloperModeStatus"].(bool)
	if !ok {
		return false, fmt.Errorf("image mounter returned no DeveloperModeStatus (%v)", reply)
	}
	return mode, nil
}

func queryDDIIdentifiers(svc *service) (ddiIdentifiers, error) {
	if err := svc.send(map[string]any{
		"Command":               "QueryPersonalizationIdentifiers",
		"PersonalizedImageType": developerDiskImageType,
	}); err != nil {
		return ddiIdentifiers{}, err
	}
	var reply map[string]any
	if err := svc.receive(&reply); err != nil {
		return ddiIdentifiers{}, err
	}
	raw, ok := reply["PersonalizationIdentifiers"].(map[string]any)
	if !ok {
		return ddiIdentifiers{}, fmt.Errorf("image mounter returned no PersonalizationIdentifiers (%v)", reply)
	}
	board, okBoard := anyInt64(raw["BoardId"])
	chip, okChip := anyInt64(raw["ChipID"])
	security, okSecurity := anyInt64(raw["SecurityDomain"])
	if !okBoard || !okChip || !okSecurity {
		return ddiIdentifiers{}, fmt.Errorf("incomplete personalization identifiers (%v)", raw)
	}
	additional := map[string]any{}
	for key, value := range raw {
		if strings.HasPrefix(key, "Ap,") {
			additional[key] = value
		}
	}
	return ddiIdentifiers{BoardID: board, ChipID: chip, SecurityDomain: security, Additional: additional}, nil
}

func queryDDINonce(svc *service) ([]byte, error) {
	if err := svc.send(map[string]any{
		"Command":               "QueryNonce",
		"HostProcessName":       "CoreDeviceService",
		"PersonalizedImageType": developerDiskImageType,
	}); err != nil {
		return nil, err
	}
	var reply map[string]any
	if err := svc.receive(&reply); err != nil {
		return nil, err
	}
	nonce, ok := reply["PersonalizationNonce"].([]byte)
	if !ok || len(nonce) == 0 {
		return nil, fmt.Errorf("image mounter returned no personalization nonce (%v)", reply)
	}
	return nonce, nil
}

func queryCachedDDITicket(svc *service, dmgPath string) ([]byte, error) {
	f, err := os.Open(dmgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha512.New384()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	if err := svc.send(map[string]any{
		"Command":               "QueryPersonalizationManifest",
		"PersonalizedImageType": developerDiskImageType,
		"ImageType":             developerDiskImageType,
		"ImageSignature":        h.Sum(nil),
	}); err != nil {
		return nil, err
	}
	var reply map[string]any
	if err := svc.receive(&reply); err != nil {
		return nil, err
	}
	ticket, ok := reply["ImageSignature"].([]byte)
	if !ok || len(ticket) == 0 {
		return nil, fmt.Errorf("no cached personalization manifest (%v)", reply)
	}
	return ticket, nil
}

func uploadDDI(svc *service, dmgPath string, ticket []byte, progress func(string, ...any)) error {
	f, err := os.Open(dmgPath)
	if err != nil {
		return fmt.Errorf("open Developer Disk Image: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := svc.send(map[string]any{
		"Command":        "ReceiveBytes",
		"ImageType":      personalizedImageType,
		"ImageSize":      uint64(st.Size()),
		"ImageSignature": ticket,
	}); err != nil {
		return err
	}
	var reply map[string]any
	if err := svc.receive(&reply); err != nil {
		return err
	}
	if reply["Status"] != "ReceiveBytesAck" {
		return fmt.Errorf("image mounter refused DDI upload (%v)", reply)
	}
	progress("Uploading Developer Disk Image (%.1f MiB)", float64(st.Size())/(1024*1024))
	if err := svc.conn.SetWriteDeadline(time.Now().Add(5 * time.Minute)); err != nil {
		return err
	}
	if _, err := io.Copy(svc.conn, f); err != nil {
		return fmt.Errorf("upload Developer Disk Image: %w", err)
	}
	_ = svc.conn.SetWriteDeadline(time.Time{})
	reply = nil
	if err := svc.receiveTimeout(&reply, 5*time.Minute); err != nil {
		return err
	}
	if reply["Status"] != "Complete" {
		return fmt.Errorf("Developer Disk Image upload did not complete (%v)", reply)
	}
	return nil
}

func mountPersonalizedDDI(svc *service, ticket, trustCache []byte) error {
	if err := svc.send(map[string]any{
		"Command":         "MountImage",
		"ImageSignature":  ticket,
		"ImageType":       personalizedImageType,
		"ImageTrustCache": trustCache,
	}); err != nil {
		return err
	}
	var reply map[string]any
	if err := svc.receiveTimeout(&reply, 3*time.Minute); err != nil {
		return err
	}
	if reply["Status"] == "Complete" {
		return nil
	}
	detail, _ := reply["DetailedError"].(string)
	if strings.Contains(strings.ToLower(detail), "already mounted") {
		return nil
	}
	if strings.Contains(strings.ToLower(detail), "developer mode is not enabled") {
		return errors.New("Developer Mode is disabled on the device")
	}
	return fmt.Errorf("mount Developer Disk Image failed (%v)", reply)
}

func loadDDIBuildManifest(path string) (ddiBuildManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ddiBuildManifest{}, fmt.Errorf("read Developer Disk Image manifest: %w", err)
	}
	var manifest ddiBuildManifest
	if _, err := plist.Unmarshal(data, &manifest); err != nil {
		return ddiBuildManifest{}, fmt.Errorf("decode Developer Disk Image manifest: %w", err)
	}
	if len(manifest.BuildIdentities) == 0 {
		return ddiBuildManifest{}, errors.New("Developer Disk Image manifest contains no build identities")
	}
	return manifest, nil
}

func selectDDIIdentity(manifest ddiBuildManifest, ids ddiIdentifiers) (ddiBuildIdentity, error) {
	for _, identity := range manifest.BuildIdentities {
		board, err1 := parseHexID(identity.BoardID)
		chip, err2 := parseHexID(identity.ChipID)
		if err1 == nil && err2 == nil && int64(board) == ids.BoardID && int64(chip) == ids.ChipID {
			return identity, nil
		}
	}
	return ddiBuildIdentity{}, fmt.Errorf("Developer Disk Image %s has no identity for board 0x%x / chip 0x%x", manifest.ProductBuildVersion, ids.BoardID, ids.ChipID)
}

func (i ddiBuildIdentity) componentPath(keys ...string) string {
	for _, key := range keys {
		if entry, ok := i.Manifest[key]; ok && entry.Info.Path != "" {
			return entry.Info.Path
		}
	}
	return ""
}

func parseHexID(value string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(strings.ToLower(value), "0x"), 16, 64)
}

func requestDDITicket(identity ddiBuildIdentity, ids ddiIdentifiers, nonce []byte, ecid uint64) ([]byte, error) {
	uuid, err := randomUUID()
	if err != nil {
		return nil, err
	}
	request := map[string]any{
		"@ApImg4Ticket":     true,
		"@BBTicket":         true,
		"@HostPlatformInfo": "mac",
		"@VersionInfo":      "libauthinstall-1104.0.9",
		"@UUID":             uuid,
		"ApBoardID":         ids.BoardID,
		"ApChipID":          ids.ChipID,
		"ApECID":            ecid,
		"ApNonce":           nonce,
		"ApProductionMode":  true,
		"ApSecurityDomain":  ids.SecurityDomain,
		"ApSecurityMode":    true,
		"SepNonce":          make([]byte, 20),
		"UID_MODE":          false,
	}
	for key, value := range ids.Additional {
		request[key] = value
	}

	rules := identity.Manifest["LoadableTrustCache"].Info.RestoreRequestRules
	params := map[string]any{
		"ApProductionMode": true,
		"ApSecurityMode":   true,
		"ApSupportsImg4":   true,
	}
	for key, entry := range identity.Manifest {
		if !entry.Trusted {
			continue
		}
		component := map[string]any{"Digest": entry.Digest, "Trusted": true}
		if entry.EPRO != nil {
			component["EPRO"] = *entry.EPRO
		}
		if entry.ESEC != nil {
			component["ESEC"] = *entry.ESEC
		}
		applyDDIRestoreRules(component, params, rules)
		if key == "PersonalizedDMG" || key == "PersonalizedDmg" {
			if entry.Name != "" {
				component["Name"] = entry.Name
			} else {
				component["Name"] = developerDiskImageType
			}
		}
		request[key] = component
	}

	body, err := plist.Marshal(request, plist.XMLFormat)
	if err != nil {
		return nil, fmt.Errorf("encode Apple TSS request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, tssEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	resp, err := tssHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Apple TSS request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Apple TSS returned HTTP %s", resp.Status)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	status, message, responsePlist, err := parseTSSResponse(responseBody)
	if err != nil {
		return nil, err
	}
	if status != 0 {
		return nil, fmt.Errorf("Apple TSS refused Developer Disk Image personalization: status %d (%s)", status, message)
	}
	var ticket map[string]any
	if _, err := plist.Unmarshal(responsePlist, &ticket); err != nil {
		return nil, fmt.Errorf("decode Apple TSS ticket: %w", err)
	}
	img4, ok := ticket["ApImg4Ticket"].([]byte)
	if !ok || len(img4) == 0 {
		return nil, errors.New("Apple TSS response contains no ApImg4Ticket")
	}
	return img4, nil
}

func applyDDIRestoreRules(component, params map[string]any, rules []ddiRestoreRequestRule) {
	for _, rule := range rules {
		if !ddiRuleMatches(rule.Conditions, params) {
			continue
		}
		for key, value := range rule.Actions {
			if n, ok := anyInt64(value); ok && n == 255 {
				continue
			}
			component[key] = value
		}
	}
}

func ddiRuleMatches(conditions, params map[string]any) bool {
	for key, want := range conditions {
		var got any
		switch key {
		case "ApRawProductionMode", "ApCurrentProductionMode":
			got = params["ApProductionMode"]
		case "ApRawSecurityMode":
			got = params["ApSecurityMode"]
		case "ApRequiresImage4":
			got = params["ApSupportsImg4"]
		case "ApDemotionPolicyOverride":
			got = params["DemotionPolicy"]
		case "ApInRomDFU":
			got = params["ApInRomDFU"]
		default:
			return false
		}
		if got == nil || fmt.Sprint(got) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

func parseTSSResponse(body []byte) (int, string, []byte, error) {
	text := string(body)
	statusText := responseField(text, "STATUS=")
	if statusText == "" {
		return 0, "", nil, fmt.Errorf("Apple TSS response has no STATUS: %q", truncate(text, 200))
	}
	status, err := strconv.Atoi(statusText)
	if err != nil {
		return 0, "", nil, fmt.Errorf("parse Apple TSS status %q: %w", statusText, err)
	}
	message := responseField(text, "MESSAGE=")
	const marker = "REQUEST_STRING="
	idx := strings.Index(text, marker)
	if idx < 0 {
		return status, message, nil, fmt.Errorf("Apple TSS response has no REQUEST_STRING")
	}
	payload := []byte(text[idx+len(marker):])
	if len(payload) == 0 {
		return status, message, nil, errors.New("Apple TSS response has an empty REQUEST_STRING")
	}
	return status, message, payload, nil
}

func responseField(text, marker string) string {
	idx := strings.Index(text, marker)
	if idx < 0 {
		return ""
	}
	value := text[idx+len(marker):]
	if end := strings.IndexByte(value, '&'); end >= 0 {
		value = value[:end]
	}
	return value
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func resolvePersonalizedDDI(progress func(string, ...any)) (string, error) {
	if configured := os.Getenv("RIPLEY_DDI_DIR"); configured != "" {
		dir, err := normalizeDDIDir(configured)
		if err != nil {
			return "", fmt.Errorf("RIPLEY_DDI_DIR: %w", err)
		}
		return dir, nil
	}
	cacheRoot, err := ddiCacheRoot()
	if err != nil {
		return "", err
	}
	restoreDir := filepath.Join(cacheRoot, personalizedDDIBuildID, "Restore")
	if cachedDDIValid(restoreDir) {
		return restoreDir, nil
	}
	if err := os.MkdirAll(restoreDir, 0o755); err != nil {
		return "", err
	}
	progress("Downloading Developer Disk Image %s", personalizedDDIBuildID)
	for _, file := range personalizedDDIFiles {
		if err := downloadVerifiedDDIFile(personalizedDDIBaseURL+"/"+file.Name, filepath.Join(restoreDir, file.Name), file.SHA256); err != nil {
			return "", err
		}
	}
	return restoreDir, nil
}

func ddiCacheRoot() (string, error) {
	if root := os.Getenv("RIPLEY_DDI_CACHE"); root != "" {
		return root, nil
	}
	ripleyHome := os.Getenv("RIPLEY_HOME")
	if ripleyHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		ripleyHome = filepath.Join(home, ".ripley")
	}
	return filepath.Join(ripleyHome, "ddi"), nil
}

func normalizeDDIDir(path string) (string, error) {
	path = filepath.Clean(path)
	if st, err := os.Stat(filepath.Join(path, "BuildManifest.plist")); err == nil && !st.IsDir() {
		return path, nil
	}
	restore := filepath.Join(path, "Restore")
	if st, err := os.Stat(filepath.Join(restore, "BuildManifest.plist")); err == nil && !st.IsDir() {
		return restore, nil
	}
	return "", fmt.Errorf("%s is not a DDI Restore directory (BuildManifest.plist not found)", path)
}

func cachedDDIValid(restoreDir string) bool {
	for _, file := range personalizedDDIFiles {
		if !fileSHA256Matches(filepath.Join(restoreDir, file.Name), file.SHA256) {
			return false
		}
	}
	return true
}

func downloadVerifiedDDIFile(url, path, wantSHA string) error {
	if fileSHA256Matches(path, wantSHA) {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := ddiHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download Developer Disk Image %s: %w", filepath.Base(path), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download Developer Disk Image %s: HTTP %s", filepath.Base(path), resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ddi-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantSHA) {
		return fmt.Errorf("Developer Disk Image %s checksum mismatch: got %s, want %s", filepath.Base(path), got, wantSHA)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func fileSHA256Matches(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want)
}

func anyInt64(v any) (int64, bool) {
	return toInt64(v)
}

func anyUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case uint64:
		return n, true
	case int64:
		if n >= 0 {
			return uint64(n), true
		}
	case int:
		if n >= 0 {
			return uint64(n), true
		}
	case float64:
		if n >= 0 {
			return uint64(n), true
		}
	}
	return 0, false
}
