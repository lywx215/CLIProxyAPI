package geminienterprise

import (
	"context"
	"encoding/hex"
	"net/http"
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

// ResolveQuotaIdentity shares the verified, paginated subscription resolver.
func ResolveQuotaIdentity(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string, now time.Time) *QuotaIdentity {
	binding = binding.WithDefaults()
	observation := ResolveSubscription(ctx, client, binding, token, now)
	result := &QuotaIdentity{Binding: Binding(binding), CheckedAt: now.UTC(), Invalidated: observation.Authoritative() && observation.Status != "ok"}
	if observation.Status == "ok" {
		result.Verified, result.ProjectNumber, result.Location, result.Edition, result.PrincipalHash = true, observation.ProjectNumber, binding.Location, observation.Edition, observation.PrincipalHash
	}
	return result
}
