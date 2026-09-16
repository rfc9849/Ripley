package ios

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"howett.net/plist"
)

func TestProvisioningProfileMatching(t *testing.T) {
	dir := t.TempDir()
	exact := filepath.Join(dir, "exact.mobileprovision")
	wildcard := filepath.Join(dir, "wildcard.mobileprovision")
	expired := filepath.Join(dir, "expired.mobileprovision")

	writeTestProfile(t, exact, "Exact", "TEAM123.dev.example.app", time.Now().Add(24*time.Hour), nil)
	writeTestProfile(t, wildcard, "Wildcard", "TEAM123.dev.example.*", time.Now().Add(48*time.Hour), nil)
	writeTestProfile(t, expired, "Expired", "TEAM123.dev.example.app", time.Now().Add(-time.Hour), nil)

	if err := ValidateProvisioningProfile(exact, "dev.example.app"); err != nil {
		t.Fatalf("exact profile should match: %v", err)
	}
	if err := ValidateProvisioningProfile(exact, "dev.example.other"); err == nil {
		t.Fatal("explicit App ID must not match another bundle id")
	}
	if err := ValidateProvisioningProfile(wildcard, "dev.example.other"); err != nil {
		t.Fatalf("wildcard profile should match: %v", err)
	}
	if err := ValidateProvisioningProfile(expired, "dev.example.app"); err == nil {
		t.Fatal("expired profile must be rejected")
	}

	selected, err := SelectProvisioningProfile([]string{wildcard, exact}, "dev.example.app")
	if err != nil {
		t.Fatal(err)
	}
	if selected != exact {
		t.Fatalf("selected %q, want exact profile %q", selected, exact)
	}
}

func TestEntitlementsUseProjectAsSourceOfTruth(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.mobileprovision")
	projectEntitlements := filepath.Join(dir, "App.entitlements")
	profileEntitlements := map[string]any{
		"application-identifier":                 "TEAM123.dev.example.app",
		"com.apple.developer.team-identifier":    "TEAM123",
		"get-task-allow":                         false,
		"keychain-access-groups":                 []string{"TEAM123.*"},
		"com.apple.developer.associated-domains": []string{"applinks:*"},
		"aps-environment":                        "production",
	}
	writeTestProfile(t, profilePath, "Profile", "TEAM123.dev.example.app", time.Now().Add(24*time.Hour), profileEntitlements)
	writeTestPlist(t, projectEntitlements, map[string]any{
		"com.apple.developer.associated-domains": []string{"applinks:$(ASSOCIATED_DOMAIN)"},
	})

	generated, err := entitlementsForProfile(profilePath, "dev.example.app", projectEntitlements, map[string]string{"ASSOCIATED_DOMAIN": "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if _, err := plist.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["aps-environment"]; exists {
		t.Fatal("unrequested aps-environment leaked from provisioning profile")
	}
	if got["application-identifier"] != "TEAM123.dev.example.app" {
		t.Fatalf("application-identifier = %#v", got["application-identifier"])
	}
	domains, ok := entitlementSlice(got["com.apple.developer.associated-domains"])
	if !ok || len(domains) != 1 || domains[0] != "applinks:example.com" {
		t.Fatalf("associated domains = %#v", got["com.apple.developer.associated-domains"])
	}

	writeTestPlist(t, projectEntitlements, map[string]any{"aps-environment": "development"})
	if _, err := entitlementsForProfile(profilePath, "dev.example.app", projectEntitlements, nil); err == nil {
		t.Fatal("project entitlement not allowed by profile must be rejected")
	}
}

func writeTestProfile(t *testing.T, path, name, appID string, expiration time.Time, entitlements map[string]any) {
	t.Helper()
	if entitlements == nil {
		entitlements = map[string]any{
			"application-identifier":              appID,
			"com.apple.developer.team-identifier": "TEAM123",
			"get-task-allow":                      false,
			"keychain-access-groups":              []string{"TEAM123.*"},
		}
	}
	profile := provisioningProfile{
		Name:           name,
		UUID:           name + "-uuid",
		TeamIdentifier: []string{"TEAM123"},
		Entitlements:   entitlements,
		ExpirationDate: expiration,
	}
	writeTestPlist(t, path, profile)
}

func writeTestPlist(t *testing.T, path string, value any) {
	t.Helper()
	data, err := plist.Marshal(value, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
