package ios

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"howett.net/plist"

	"ripley/internal/toolchain"
)

type provisioningProfile struct {
	Name               string         `plist:"Name"`
	UUID               string         `plist:"UUID"`
	TeamIdentifier     []string       `plist:"TeamIdentifier"`
	Entitlements       map[string]any `plist:"Entitlements"`
	ExpirationDate     time.Time      `plist:"ExpirationDate"`
	ProvisionedDevices []string       `plist:"ProvisionedDevices"`
}

type profileInfo struct {
	Name         string
	UUID         string
	TeamID       string
	AppID        string
	Expiration   time.Time
	Devices      []string
	GetTaskAllow bool
	Entitlements map[string]any
}

func decodeProfile(path string) (provisioningProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return provisioningProfile{}, err
	}
	if start := bytes.Index(data, []byte("<?xml")); start >= 0 {
		end := bytes.LastIndex(data, []byte("</plist>"))
		if end < 0 {
			return provisioningProfile{}, fmt.Errorf("%s: incomplete plist payload", path)
		}
		var profile provisioningProfile
		if _, err := plist.Unmarshal(data[start:end+len("</plist>")], &profile); err != nil {
			return provisioningProfile{}, fmt.Errorf("parse provisioning profile: %w", err)
		}
		return profile, nil
	}
	start := bytes.Index(data, []byte("bplist00"))
	if start < 0 {
		return provisioningProfile{}, fmt.Errorf("%s: no plist payload found", path)
	}
	for end := len(data); end > start; end-- {
		var profile provisioningProfile
		if _, err := plist.Unmarshal(data[start:end], &profile); err == nil {
			return profile, nil
		}
	}
	return provisioningProfile{}, fmt.Errorf("%s: could not parse embedded plist", path)
}

func readprofileInfo(path string) (profileInfo, error) {
	profile, err := decodeProfile(path)
	if err != nil {
		return profileInfo{}, err
	}
	if len(profile.TeamIdentifier) == 0 || profile.TeamIdentifier[0] == "" {
		return profileInfo{}, errors.New("provisioning profile has no TeamIdentifier")
	}
	appID, _ := profile.Entitlements["application-identifier"].(string)
	if appID == "" {
		return profileInfo{}, errors.New("provisioning profile has no application-identifier entitlement")
	}
	getTaskAllow, _ := profile.Entitlements["get-task-allow"].(bool)
	return profileInfo{
		Name:         profile.Name,
		UUID:         profile.UUID,
		TeamID:       profile.TeamIdentifier[0],
		AppID:        appID,
		Expiration:   profile.ExpirationDate,
		Devices:      profile.ProvisionedDevices,
		GetTaskAllow: getTaskAllow,
		Entitlements: profile.Entitlements,
	}, nil
}

func profileAllowsBundleID(info profileInfo, bundleID string) bool {
	prefix := info.TeamID + "."
	if !strings.HasPrefix(info.AppID, prefix) {
		return false
	}
	pattern := strings.TrimPrefix(info.AppID, prefix)
	if pattern == bundleID {
		return true
	}
	if !strings.HasSuffix(pattern, "*") {
		return false
	}
	return strings.HasPrefix(bundleID, strings.TrimSuffix(pattern, "*"))
}

func ValidateProvisioningProfile(path, bundleID string) error {
	info, err := readprofileInfo(path)
	if err != nil {
		return fmt.Errorf("provisioning profile %s: %w", path, err)
	}
	if !info.Expiration.IsZero() && time.Now().After(info.Expiration) {
		return fmt.Errorf("provisioning profile %s expired at %s", info.Name, info.Expiration.Format(time.RFC3339))
	}
	if !profileAllowsBundleID(info, bundleID) {
		return fmt.Errorf("provisioning profile %s (%s) does not allow bundle id %s", info.Name, info.AppID, bundleID)
	}
	return nil
}

func SelectProvisioningProfile(paths []string, bundleID string) (string, error) {
	type candidate struct {
		path       string
		info       profileInfo
		exactMatch bool
	}
	var candidates []candidate
	for _, path := range paths {
		info, err := readprofileInfo(path)
		if err != nil {
			continue
		}
		if !info.Expiration.IsZero() && time.Now().After(info.Expiration) {
			continue
		}
		if !profileAllowsBundleID(info, bundleID) {
			continue
		}
		pattern := strings.TrimPrefix(info.AppID, info.TeamID+".")
		candidates = append(candidates, candidate{path: path, info: info, exactMatch: pattern == bundleID})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no non-expired provisioning profile matches bundle id %s", bundleID)
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.exactMatch && !best.exactMatch {
			best = candidate
			continue
		}
		if candidate.exactMatch == best.exactMatch && candidate.info.Expiration.After(best.info.Expiration) {
			best = candidate
		}
	}
	return best.path, nil
}

func entitlementsForProfile(provision, bundleID, extra string, variables map[string]string) (string, error) {
	if err := ValidateProvisioningProfile(provision, bundleID); err != nil {
		return "", err
	}
	info, err := readprofileInfo(provision)
	if err != nil {
		return "", err
	}
	appID := info.TeamID + "." + bundleID
	entitlements := make(map[string]any)
	if extra != "" {
		data, err := os.ReadFile(extra)
		if err != nil {
			return "", err
		}
		var requested map[string]any
		if _, err := plist.Unmarshal(data, &requested); err != nil {
			return "", fmt.Errorf("parse entitlements %s: %w", extra, err)
		}
		for key, value := range requested {
			value = expandEntitlementValue(value, info.TeamID, bundleID, variables)
			if key == "application-identifier" || key == "com.apple.developer.team-identifier" || key == "get-task-allow" {
				continue
			}
			allowed, ok := info.Entitlements[key]
			if !ok || !entitlementValueAllowed(value, allowed) {
				return "", fmt.Errorf("project entitlement %s=%v is not allowed by provisioning profile %s", key, value, info.Name)
			}
			entitlements[key] = value
		}
	}

	entitlements["application-identifier"] = appID
	if _, ok := info.Entitlements["com.apple.developer.team-identifier"]; ok {
		entitlements["com.apple.developer.team-identifier"] = info.TeamID
	}
	if value, ok := info.Entitlements["get-task-allow"]; ok {
		entitlements["get-task-allow"] = value
	}
	if _, ok := info.Entitlements["keychain-access-groups"]; ok {
		if _, explicit := entitlements["keychain-access-groups"]; !explicit {
			entitlements["keychain-access-groups"] = []string{appID}
		}
	}

	out := strings.TrimSuffix(provision, filepath.Ext(provision)) + ".entitlements.plist"
	if err := writePlist(out, entitlements); err != nil {
		return "", err
	}
	return out, nil
}

func expandEntitlementValue(value any, teamID, bundleID string, variables map[string]string) any {
	replace := func(value string) string {
		replacements := map[string]string{
			"AppIdentifierPrefix":       teamID + ".",
			"TeamIdentifierPrefix":      teamID + ".",
			"PRODUCT_BUNDLE_IDENTIFIER": bundleID,
			"CFBundleIdentifier":        bundleID,
			"DEVELOPMENT_TEAM":          teamID,
		}
		for key, replacement := range variables {
			replacements[key] = replacement
		}
		for key, replacement := range replacements {
			value = strings.ReplaceAll(value, "$("+key+")", replacement)
			value = strings.ReplaceAll(value, "${"+key+"}", replacement)
		}
		return value
	}
	switch typed := value.(type) {
	case string:
		return replace(typed)
	case []string:
		out := make([]string, len(typed))
		for i, item := range typed {
			out[i] = replace(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = expandEntitlementValue(item, teamID, bundleID, variables)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = expandEntitlementValue(item, teamID, bundleID, variables)
		}
		return out
	default:
		return value
	}
}

func entitlementValueAllowed(requested, allowed any) bool {
	if requestedString, ok := requested.(string); ok {
		if allowedString, ok := allowed.(string); ok {
			return entitlementStringAllowed(requestedString, allowedString)
		}
	}
	requestedSlice, requestedIsSlice := entitlementSlice(requested)
	allowedSlice, allowedIsSlice := entitlementSlice(allowed)
	if requestedIsSlice && allowedIsSlice {
		for _, requestedItem := range requestedSlice {
			matched := false
			for _, allowedItem := range allowedSlice {
				if entitlementValueAllowed(requestedItem, allowedItem) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
		return true
	}
	requestedMap, requestedIsMap := requested.(map[string]any)
	allowedMap, allowedIsMap := allowed.(map[string]any)
	if requestedIsMap && allowedIsMap {
		for key, requestedValue := range requestedMap {
			allowedValue, ok := allowedMap[key]
			if !ok || !entitlementValueAllowed(requestedValue, allowedValue) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(requested, allowed)
}

func entitlementSlice(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []string:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = item
		}
		return out, true
	default:
		return nil, false
	}
}

func entitlementStringAllowed(requested, allowed string) bool {
	if allowed == "*" || requested == allowed {
		return true
	}
	if strings.HasSuffix(allowed, "*") {
		return strings.HasPrefix(requested, strings.TrimSuffix(allowed, "*"))
	}
	return false
}

type SignOptions struct {
	Entitlements string
	Variables    map[string]string
	Key          string
	Cert         string
	Provision    string
	Password     string
	BundleID     string
}

func SignApp(tc toolchain.Toolchain, app string, opt SignOptions) error {
	zsign := filepath.Join(tc.ToolsetBin(), "zsign")
	args := []string{"-f"}
	if opt.Key != "" || opt.Provision != "" {
		if opt.Key == "" || opt.Provision == "" {
			return errors.New("real signing needs both --key and --prov (a .p12 also works as --key)")
		}
		args = append(args, "-k", opt.Key, "-m", opt.Provision)
		if opt.Cert != "" {
			args = append(args, "-c", opt.Cert)
		}
		if opt.Password != "" {
			args = append(args, "-p", opt.Password)
		}
		if opt.BundleID != "" {
			entitlements, err := entitlementsForProfile(opt.Provision, opt.BundleID, opt.Entitlements, opt.Variables)
			if err != nil {
				return err
			}
			args = append(args, "-e", entitlements)
		} else if opt.Entitlements != "" {
			args = append(args, "-e", opt.Entitlements)
		}
	} else {
		args = append(args, "-a")
		if opt.Entitlements != "" {
			args = append(args, "-e", opt.Entitlements)
		}
	}
	args = append(args, app)
	return run("", nil, zsign, args...)
}
