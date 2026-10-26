package geminienterprise

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// SubscriptionObservation contains verified license data, never OAuth material.
type SubscriptionObservation struct {
	Status        string     `json:"status"`
	Edition       string     `json:"edition,omitempty"`
	Tier          string     `json:"subscription_tier,omitempty"`
	FreeTrial     *bool      `json:"free_trial"`
	State         string     `json:"state,omitempty"`
	CheckedAt     time.Time  `json:"checked_at"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	ProjectNumber string     `json:"-"`
	PrincipalHash string     `json:"-"`
}

// Authoritative distinguishes verified subscription changes from temporary read failures.
func (o SubscriptionObservation) Authoritative() bool {
	switch o.Status {
	case "ok", "unassigned", "inactive", "conflict":
		return true
	case "unknown":
		return o.Reason == "unknown_subscription_tier" || o.Reason == "invalid_license_binding"
	}
	return false
}

// ResolveSubscription lists all pages before deciding whether this principal is assigned.
// A numeric project does not require Cloud Resource Manager to be enabled.
func ResolveSubscription(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string, now time.Time) SubscriptionObservation {
	binding = binding.WithDefaults()
	result := SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable", CheckedAt: now.UTC()}
	if binding.Validate() != nil || client == nil || token == "" {
		return result
	}
	email, errEmail := Email(ctx, client, token)
	if errEmail != nil || strings.TrimSpace(email) == "" {
		return result
	}
	hash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	result.PrincipalHash = hex.EncodeToString(hash[:])
	number := binding.ProjectID
	if !numberProject.MatchString(number) {
		var project resourceProject
		status, errGet := resourceJSON(ctx, client, "https://cloudresourcemanager.googleapis.com/v3/projects/"+url.PathEscape(binding.ProjectID), token, binding.ProjectID, &project)
		number = strings.TrimPrefix(project.Name, "projects/")
		if errGet != nil || status != http.StatusOK {
			return result
		}
		if !numberProject.MatchString(number) || project.ProjectID != binding.ProjectID {
			result.Reason = "invalid_license_binding"
			return result
		}
	}
	result.ProjectNumber = number
	origin := OriginForLocation(binding.Location)
	parent := "projects/" + number + "/locations/" + binding.Location
	canonicalName := func(name string) string {
		parts := strings.Split(name, "/")
		if len(parts) != 6 || parts[0] != "projects" || (parts[1] != number && parts[1] != binding.ProjectID) || parts[2] != "locations" || parts[3] != binding.Location || parts[4] != "licenseConfigs" || !resourcePart.MatchString(parts[5]) {
			return ""
		}
		parts[1] = number
		return strings.Join(parts, "/")
	}
	configs := map[string]string{}
	params := url.Values{"pageSize": {"100"}}
	seen := map[string]bool{}
	complete := false
	for range 100 {
		var page struct {
			Licenses []struct {
				Principal string `json:"userPrincipal"`
				State     string `json:"licenseAssignmentState"`
				Config    string `json:"licenseConfig"`
			} `json:"userLicenses"`
			Next string `json:"nextPageToken"`
		}
		status, errGet := resourceJSON(ctx, client, origin+"/v1/"+parent+"/userStores/default_user_store/userLicenses?"+params.Encode(), token, binding.ProjectID, &page)
		if errGet != nil || status != http.StatusOK {
			return result
		}
		for _, license := range page.Licenses {
			if !strings.EqualFold(strings.TrimSpace(license.Principal), strings.TrimSpace(email)) || license.State != "ASSIGNED" {
				continue
			}
			name := canonicalName(license.Config)
			if name == "" {
				result.Reason = "invalid_license_binding"
				return result
			}
			configs[name] = license.Config
		}
		if page.Next == "" {
			complete = true
			break
		}
		if seen[page.Next] {
			return result
		}
		seen[page.Next] = true
		params.Set("pageToken", page.Next)
	}
	if !complete {
		return result
	}
	if len(configs) == 0 {
		result.Status, result.Reason = "unassigned", "license_unassigned"
		return result
	}
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	var signature string
	for _, name := range names {
		var license struct {
			Name      string `json:"name"`
			Tier      string `json:"subscriptionTier"`
			State     string `json:"state"`
			FreeTrial bool   `json:"freeTrial"`
		}
		status, errGet := resourceJSON(ctx, client, origin+"/v1/"+configs[name], token, binding.ProjectID, &license)
		if errGet != nil || status != http.StatusOK {
			return SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable", CheckedAt: now.UTC(), ProjectNumber: number, PrincipalHash: result.PrincipalHash}
		}
		if canonicalName(license.Name) != name {
			return SubscriptionObservation{Status: "unknown", Reason: "invalid_license_binding", CheckedAt: now.UTC(), ProjectNumber: number, PrincipalHash: result.PrincipalHash}
		}
		edition := SubscriptionEdition(license.Tier)
		if edition == "" {
			return SubscriptionObservation{Status: "unknown", Reason: "unknown_subscription_tier", CheckedAt: now.UTC(), ProjectNumber: number, PrincipalHash: result.PrincipalHash}
		}
		active := license.State == "ACTIVE" || license.State == "DEACTIVATING"
		current := edition + "/" + map[bool]string{true: "trial", false: "paid"}[license.FreeTrial] + "/" + map[bool]string{true: "active", false: "inactive"}[active]
		if signature != "" && signature != current {
			return SubscriptionObservation{Status: "conflict", Reason: "conflicting_subscriptions", CheckedAt: now.UTC(), ProjectNumber: number, PrincipalHash: result.PrincipalHash}
		}
		signature = current
		trial := license.FreeTrial
		result.Edition, result.Tier, result.State, result.FreeTrial = edition, license.Tier, license.State, &trial
		result.Status, result.Reason = "ok", ""
		if !active {
			result.Status, result.Reason = "inactive", "subscription_inactive"
		}
	}
	return result
}
