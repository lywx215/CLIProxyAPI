package auth

import (
	"context"
	"errors"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

var ErrEnterpriseResourcesChanged = errors.New("gemini-enterprise credential changed during resource validation")

// UpdateEnterpriseResources compares the validated snapshot under the credential
// mutation gate and merges only resource fields into the latest saved auth.
// Concurrent token rotation, counters and unrelated user settings are retained.
func (m *Manager) UpdateEnterpriseResources(ctx context.Context, base *Auth, resources ge.ResourceBinding) (*Auth, error) {
	if base == nil || base.Provider != ge.Provider {
		return nil, ErrEnterpriseResourcesChanged
	}
	updated := base.Clone()
	binding := resources.Apply(ge.FromMetadata(base.Metadata))
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	putEnterpriseResources(updated, ge.Resources(binding))
	return m.updateInternal(ctx, base, updated, updateModeEnterpriseResources)
}

func sameEnterpriseResourceSnapshot(base, current *Auth) bool {
	return base != nil && current != nil && current.Provider == ge.Provider &&
		base.RegistrationEpoch == current.RegistrationEpoch && base.ProxyURL == current.ProxyURL &&
		ge.Binding(ge.FromMetadata(base.Metadata)) == ge.Binding(ge.FromMetadata(current.Metadata)) &&
		ge.String(base.Metadata, "email") == ge.String(current.Metadata, "email")
}

func putEnterpriseResources(auth *Auth, resources ge.ResourceBinding) {
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	for key, value := range map[string]string{"project_id": resources.ProjectID, "location": resources.Location, "collection_id": resources.CollectionID, "engine_id": resources.EngineID, "assistant_id": resources.AssistantID} {
		auth.Metadata[key] = value
	}
}

func resetEnterpriseResourceState(auth *Auth) {
	auth.EnterpriseQuotaIdentity = nil
	auth.ModelStates = nil
	auth.Quota = QuotaState{}
	auth.LastError = nil
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.StatusMessage = ""
	auth.RejectedAccessToken = ""
	if auth.Disabled || auth.Status == StatusDisabled {
		auth.Status = StatusDisabled
	} else {
		auth.Status = StatusActive
	}
}
