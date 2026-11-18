package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

type enterpriseModelProofPublicationKey struct{}

func IsEnterpriseModelProofPublication(ctx context.Context) bool {
	return ctx != nil && ctx.Value(enterpriseModelProofPublicationKey{}) == true
}

// EnterpriseModelProof records the scope of a complete, session-verified response.
type EnterpriseModelProof struct {
	ModelID               string                             `json:"model_id"`
	Target                config.GeminiEnterpriseModelTarget `json:"target"`
	AuthorizationRevision uint64                             `json:"authorization_revision"`
	CollectionRevision    uint64                             `json:"collection_revision"`
	Configuration         string                             `json:"configuration"`
}

type enterpriseConfiguredModelProbe struct {
	proof EnterpriseModelProof
	query string
	sends atomic.Uint32
}

// EnterpriseModelVerificationJournalStore reserves a request with an empty
// ETag and conditionally updates it using the version returned by Read.
type EnterpriseModelVerificationJournalStore interface {
	SupportsEnterpriseModelVerificationJournal() bool
	ReadEnterpriseModelVerification(context.Context, string) ([]byte, string, error)
	WriteEnterpriseModelVerification(context.Context, string, []byte, string) error
}

func (m *Manager) EnterpriseModelVerificationJournalStore() EnterpriseModelVerificationJournalStore {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	store, ok := m.store.(EnterpriseModelVerificationJournalStore)
	m.mu.RUnlock()
	if !ok || !store.SupportsEnterpriseModelVerificationJournal() {
		return nil
	}
	return store
}

func (m *Manager) enterpriseModelDefinitions() config.GeminiEnterpriseModels {
	if m != nil {
		if cfg := m.runtimeConfigSnapshot(); cfg != nil {
			return cfg.GeminiEnterpriseModels
		}
	}
	return config.GeminiEnterpriseModels{}
}

func (m *Manager) AppliedEnterpriseModelDefinitionsRevision() uint64 {
	if m == nil {
		return 0
	}
	return m.enterpriseDefinitionsApplied.Load()
}

func (m *Manager) SupportsLocalEnterpriseModelVerifications() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	store, ok := m.store.(interface{ SupportsLocalEnterpriseModelVerifications() bool })
	return ok && store.SupportsLocalEnterpriseModelVerifications()
}

func (m *Manager) MarkEnterpriseModelDefinitionsApplied(revision uint64) {
	if m != nil && m.enterpriseModelDefinitions().Revision == revision {
		m.enterpriseDefinitionsApplied.Store(revision)
	}
}

func configuredEnterpriseTarget(auth *Auth, definition config.GeminiEnterpriseModelDefinition) (config.GeminiEnterpriseModelTarget, bool) {
	if auth == nil || !IsEnterpriseCollectionAuth(auth) {
		return config.GeminiEnterpriseModelTarget{}, false
	}
	account, binding := ge.String(auth.Metadata, "enterprise_account_id"), EnterpriseBindingID(auth)
	resources := ge.Resources(ge.FromMetadata(auth.Metadata))
	for _, target := range definition.Targets {
		if target.AccountID == account && target.BindingID == binding &&
			target.ProjectID == resources.ProjectID && target.Location == resources.Location &&
			target.CollectionID == resources.CollectionID && target.EngineID == resources.EngineID && target.AssistantID == resources.AssistantID {
			return target, true
		}
	}
	return config.GeminiEnterpriseModelTarget{}, false
}

func (m *Manager) configuredEnterpriseDefinition(auth *Auth, model string) (config.GeminiEnterpriseModelDefinition, bool) {
	definition, exists := m.enterpriseModelDefinitions().Definition(model)
	if !exists {
		return config.GeminiEnterpriseModelDefinition{}, false
	}
	_, targeted := configuredEnterpriseTarget(auth, definition)
	return definition, targeted
}

func configuredModelPolicyAllows(auth *Auth, view EnterpriseModelPolicyView, model string, target config.GeminiEnterpriseModelTarget) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || enterpriseRetired(auth) || view.Status == "blocked" {
		return false
	}
	paid := view.Subscription.ConfirmedPaid()
	if view.Subscription.FreeTrial == nil || (target.LicenseMode == "paid") != paid || (model == ge.HydrogenModel && !paid) {
		return false
	}
	mode, whitelist, err := ge.ModelPolicySettings(auth.Metadata)
	family := ge.IsArgonFamilyModel(model) || (paid && model == ge.HydrogenModel)
	if err != nil || (mode == "custom" && !slices.Contains(whitelist, model)) ||
		(mode == "argon_only" && !family) || (mode == "exclude_argon" && family) {
		return false
	}
	if model == ge.CarbonModel {
		if enabled, explicit := auth.Metadata[ge.CarbonEnabledKey].(bool); explicit && !enabled {
			return false
		}
	}
	policy := view.ModelPolicy
	policy.ArgonForceEnabled = false
	if paid && policy.EnforcedAllowedModels != nil {
		policy.EnforcedAllowedModels = append(slices.Clone(policy.EnforcedAllowedModels), model)
	}
	return policy.Allows(model)
}

func (m *Manager) configuredModelProof(auth *Auth, definition config.GeminiEnterpriseModelDefinition, target config.GeminiEnterpriseModelTarget) bool {
	stored, _ := auth.Metadata[ge.ConfiguredModelProofKey].(map[string]any)
	data, err := json.Marshal(stored[definition.ModelID+":"+target.BindingID])
	if err != nil {
		return false
	}
	var proof EnterpriseModelProof
	if json.Unmarshal(data, &proof) != nil || proof.Target != target || proof.ModelID != definition.ModelID {
		return false
	}
	return m.configuredProofCurrent(auth, proof)
}

func (m *Manager) configuredProofCurrent(auth *Auth, proof EnterpriseModelProof) bool {
	if auth == nil || enterpriseOAuthAuthorizationRevision(auth) != proof.AuthorizationRevision || proof.Configuration == "" {
		return false
	}
	revision, _ := auth.Metadata["enterprise_collection_revision"].(uint64)
	if revision != proof.CollectionRevision {
		return false
	}
	definition, ok := m.enterpriseModelDefinitions().Definition(proof.ModelID)
	if !ok {
		return false
	}
	target, matches := configuredEnterpriseTarget(auth, definition)
	return matches && target == proof.Target && m.EnterpriseAdmissionEvidenceFingerprint(auth, ge.AdmissionModel{ID: proof.ModelID}).Configuration == proof.Configuration
}

func (m *Manager) configuredModelGate(auth *Auth, view EnterpriseModelPolicyView, definition config.GeminiEnterpriseModelDefinition, model ge.Model) string {
	if model.MediaKind != "" || strings.EqualFold(strings.TrimSpace(model.DisplayName), "auto") {
		return "model_test_unsupported"
	}
	target, ok := configuredEnterpriseTarget(auth, definition)
	if !ok {
		return "model_target_mismatch"
	}
	if !configuredModelPolicyAllows(auth, view, model.ID, target) {
		return "model_not_allowed"
	}
	return ""
}

func configuredVerificationError(code string, status int) *EnterpriseModelPolicyError {
	return &EnterpriseModelPolicyError{code, "The selected Gemini Enterprise application is not eligible for this model verification", status}
}

func (m *Manager) validateConfiguredModelVerification(auth *Auth, proof EnterpriseModelProof) error {
	if !m.configuredProofCurrent(auth, proof) {
		return configuredVerificationError("model_scope_changed", http.StatusConflict)
	}
	view := m.enterpriseModelPolicyView(auth, false)
	if view.Subscription.Status != "ok" {
		return configuredVerificationError("subscription_unverified", http.StatusConflict)
	}
	definition := config.GeminiEnterpriseModelDefinition{ModelID: proof.ModelID, Targets: []config.GeminiEnterpriseModelTarget{proof.Target}}
	model := ge.Model{ID: proof.ModelID, DisplayName: proof.ModelID, MediaKind: registry.EnterpriseMediaKind(proof.ModelID)}
	for _, entry := range view.CatalogModels {
		if entry.ID == proof.ModelID {
			model.DisplayName = entry.DisplayName
			if entry.MediaKind != "" {
				model.MediaKind = entry.MediaKind
			}
			break
		}
	}
	if reason := m.configuredModelGate(auth, view, definition, model); reason != "" {
		return configuredVerificationError(reason, http.StatusConflict)
	}
	return nil
}

// BeginConfiguredEnterpriseModelProbe binds a single generation to an applied rule.
func (m *Manager) BeginConfiguredEnterpriseModelProbe(ctx context.Context, auth *Auth, model string, target config.GeminiEnterpriseModelTarget, revision, authorization uint64, query string) (context.Context, EnterpriseModelProof, error) {
	if current := m.enterpriseModelDefinitions().Revision; current == 0 || current != m.AppliedEnterpriseModelDefinitionsRevision() {
		return ctx, EnterpriseModelProof{}, configuredVerificationError("definitions_not_applied", http.StatusConflict)
	}
	proof := EnterpriseModelProof{ModelID: model, Target: target, CollectionRevision: revision, AuthorizationRevision: authorization,
		Configuration: m.EnterpriseAdmissionEvidenceFingerprint(auth, ge.AdmissionModel{ID: model}).Configuration}
	if err := m.validateConfiguredModelVerification(auth, proof); err != nil {
		return ctx, proof, err
	}
	probeCtx, err := m.BeginEnterpriseModelProbe(ctx, auth, model, revision)
	if err != nil {
		return ctx, proof, err
	}
	enterpriseAttempts(probeCtx).probe.configured = &enterpriseConfiguredModelProbe{proof: proof, query: query}
	return probeCtx, proof, nil
}

func configuredEnterpriseProbe(ctx context.Context) *enterpriseConfiguredModelProbe {
	if attempts := enterpriseAttempts(ctx); attempts != nil && attempts.probe != nil {
		return attempts.probe.configured
	}
	return nil
}

func ConfiguredEnterpriseModelProbeActive(ctx context.Context) bool {
	return configuredEnterpriseProbe(ctx) != nil
}

// CommitConfiguredEnterpriseProbeSend fences the initial submission and an explicitly
// authorized compatibility retry at the transport boundary.
func CommitConfiguredEnterpriseProbeSend(ctx context.Context, query string, sessionless, compatibilityRetry bool) error {
	probe := configuredEnterpriseProbe(ctx)
	if probe == nil {
		if compatibilityRetry {
			return configuredVerificationError("verification_scope_missing", http.StatusConflict)
		}
		return nil
	}
	if query != probe.query || sessionless {
		return configuredVerificationError("verification_payload_changed", http.StatusConflict)
	}
	var expected uint32
	if compatibilityRetry {
		expected = 1
	}
	if !probe.sends.CompareAndSwap(expected, expected+1) {
		return configuredVerificationError("verification_already_sent", http.StatusConflict)
	}
	return nil
}

func ConfiguredEnterpriseProbeSent(ctx context.Context) bool {
	probe := configuredEnterpriseProbe(ctx)
	return probe != nil && probe.sends.Load() > 0
}

func ConfiguredEnterpriseProbeRetried(ctx context.Context) bool {
	probe := configuredEnterpriseProbe(ctx)
	return probe != nil && probe.sends.Load() > 1
}

// SaveConfiguredEnterpriseModelProof stores only server-proven scope and leaves user settings intact.
func (m *Manager) SaveConfiguredEnterpriseModelProof(ctx context.Context, auth *Auth, proof EnterpriseModelProof) (*Auth, error) {
	if auth == nil {
		return nil, enterpriseModelProbeChanged()
	}
	ctx = context.WithValue(ctx, enterpriseModelProofPublicationKey{}, true)
	return m.mutateEnterpriseAdmissionMetadata(ctx, ge.AdmissionModel{Label: "Enterprise model", EvidenceStoreKey: ge.ConfiguredModelProofKey},
		EnterpriseOwnerID(auth), func(owner, target *Auth, cache map[string]any) (bool, error) {
			if owner.Disabled || owner.Status == StatusDisabled || EnterpriseOAuthAuthorizationRevision(owner) != proof.AuthorizationRevision ||
				!m.configuredProofCurrent(target, proof) || target.Disabled || target.Status == StatusDisabled {
				return false, enterpriseModelProbeChanged()
			}
			key := proof.ModelID + ":" + proof.Target.BindingID
			if existing, err := json.Marshal(cache[key]); err == nil {
				var prior EnterpriseModelProof
				if json.Unmarshal(existing, &prior) == nil && prior == proof {
					return false, nil
				}
			}
			cache[key] = proof
			return true, nil
		}, auth.ID, proof.Target.BindingID)
}

func (m *Manager) SyncConfiguredEnterpriseModelProof(ctx context.Context, auth *Auth) error {
	return m.syncEnterpriseAccount(context.WithValue(ctx, enterpriseModelProofPublicationKey{}, true), auth)
}

func (m *Manager) ConfiguredEnterpriseModelPublished(auth *Auth, proof EnterpriseModelProof) bool {
	if auth == nil {
		return false
	}
	current, ok := m.GetByID(auth.ID)
	if !ok || !m.configuredProofCurrent(current, proof) || !slices.Contains(m.EnterpriseModelPolicyView(current).EffectiveModels, proof.ModelID) {
		return false
	}
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(current.ID) {
		if model != nil && (model.MetadataModelID == proof.ModelID || m.selectionModelForAuth(current, model.ID) == proof.ModelID) {
			return true
		}
	}
	return false
}

func (m *Manager) HasActiveConfiguredEnterpriseModel(auth *Auth) bool {
	if auth == nil || auth.Metadata[ge.ConfiguredModelProofKey] == nil || len(m.enterpriseModelDefinitions().Definitions) == 0 {
		return false
	}
	for _, model := range m.EnterpriseModelPolicyView(auth).CatalogModels {
		if model.Configured && model.Allowed {
			return true
		}
	}
	return false
}
