package geminienterprise

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// QuotaIdentity is established from OAuth userinfo and that user's assigned license.
// It is not populated from inference parameters or an imported email field.
type QuotaIdentity struct {
	Invalidated   bool      `json:"invalidated,omitempty"`
	Verified      bool      `json:"verified"`
	Binding       string    `json:"binding"`
	ProjectNumber string    `json:"project_number,omitempty"`
	Location      string    `json:"location,omitempty"`
	Edition       string    `json:"edition,omitempty"`
	PrincipalHash string    `json:"principal_hash,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

func (i *QuotaIdentity) ValidFor(binding config.GeminiEnterpriseOAuth) bool {
	if i == nil || !i.Verified || i.Invalidated || i.Binding != Binding(binding) || !numberProject.MatchString(i.ProjectNumber) || i.Location != binding.WithDefaults().Location || len(i.PrincipalHash) != 64 {
		return false
	}
	if _, err := hex.DecodeString(i.PrincipalHash); err != nil {
		return false
	}
	for _, tier := range []string{"SEARCH_AND_ASSISTANT", "ENTERPRISE", "ENTERPRISE_EMERGING", "FRONTLINE_WORKER", "FRONTLINE_STARTER", "AGENTSPACE_STARTER", "AGENTSPACE_BUSINESS", "EDU", "EDU_PRO", "EDU_EMERGING", "EDU_PRO_EMERGING"} {
		if SubscriptionEdition(tier) == i.Edition {
			return true
		}
	}
	return false
}

func SubscriptionEdition(tier string) string {
	return map[string]string{"SEARCH_AND_ASSISTANT": "enterprise_plus", "ENTERPRISE": "enterprise_standard", "ENTERPRISE_EMERGING": "enterprise_emerging", "FRONTLINE_WORKER": "frontline_worker", "FRONTLINE_STARTER": "frontline_starter", "AGENTSPACE_STARTER": "starter", "AGENTSPACE_BUSINESS": "business_standard", "EDU": "edu", "EDU_PRO": "edu_pro", "EDU_EMERGING": "edu_emerging", "EDU_PRO_EMERGING": "edu_pro_emerging"}[strings.TrimPrefix(tier, "SUBSCRIPTION_TIER_")]
}

// ResolveQuotaIdentity is optional and read-only. Failure never prevents inference.
func ResolveQuotaIdentity(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string, now time.Time) *QuotaIdentity {
	result := &QuotaIdentity{Binding: Binding(binding), CheckedAt: now.UTC()}
	if binding.Validate() != nil || client == nil || token == "" {
		return result
	}
	email, errEmail := Email(ctx, client, token)
	if errEmail != nil || strings.ContainsAny(email, "\"\\\r\n") {
		return result
	}
	var project resourceProject
	status, errGet := resourceJSON(ctx, client, "https://cloudresourcemanager.googleapis.com/v3/projects/"+url.PathEscape(binding.ProjectID), token, binding.ProjectID, &project)
	number := strings.TrimPrefix(project.Name, "projects/")
	if errGet != nil || status != 200 || !numberProject.MatchString(number) || (binding.ProjectID != number && binding.ProjectID != project.ProjectID) {
		return result
	}
	parent := "projects/" + number + "/locations/" + binding.Location
	params := url.Values{"filter": {"userPrincipal = " + email}, "pageSize": {"100"}}
	var licenses struct {
		Licenses []struct {
			Principal string `json:"userPrincipal"`
			State     string `json:"licenseAssignmentState"`
			Config    string `json:"licenseConfig"`
		} `json:"userLicenses"`
		Next string `json:"nextPageToken"`
	}
	status, errGet = resourceJSON(ctx, client, Origin+"/v1/"+parent+"/userStores/default_user_store/userLicenses?"+params.Encode(), token, binding.ProjectID, &licenses)
	if errGet == nil && status == 200 && licenses.Next == "" && len(licenses.Licenses) == 0 {
		result.Invalidated = true
	}
	if errGet != nil || status != 200 || licenses.Next != "" || len(licenses.Licenses) != 1 {
		return result
	}
	license := licenses.Licenses[0]
	if !strings.EqualFold(license.Principal, email) || license.State != "ASSIGNED" {
		result.Invalidated = true
		return result
	}
	parts := strings.Split(license.Config, "/")
	if len(parts) != 6 || parts[0] != "projects" || (parts[1] != number && parts[1] != binding.ProjectID) || parts[2] != "locations" || parts[3] != binding.Location || parts[4] != "licenseConfigs" || !resourcePart.MatchString(parts[5]) {
		return result
	}
	var subscription struct {
		Name  string `json:"name"`
		Tier  string `json:"subscriptionTier"`
		State string `json:"state"`
	}
	status, errGet = resourceJSON(ctx, client, Origin+"/v1/"+license.Config, token, binding.ProjectID, &subscription)
	edition := SubscriptionEdition(subscription.Tier)
	if errGet == nil && status == 200 && (edition == "" || (subscription.State != "ACTIVE" && subscription.State != "DEACTIVATING")) {
		result.Invalidated = true
	}
	canonicalName := func(name string) string {
		parts := strings.Split(name, "/")
		if len(parts) == 6 && parts[0] == "projects" && (parts[1] == binding.ProjectID || parts[1] == number) {
			parts[1] = number
			return strings.Join(parts, "/")
		}
		return ""
	}
	if errGet != nil || status != 200 || canonicalName(subscription.Name) != canonicalName(license.Config) || canonicalName(subscription.Name) == "" || edition == "" || (subscription.State != "ACTIVE" && subscription.State != "DEACTIVATING") {
		return result
	}
	hash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	result.Verified, result.ProjectNumber, result.Location, result.Edition, result.PrincipalHash = true, number, binding.Location, edition, hex.EncodeToString(hash[:])
	return result
}
