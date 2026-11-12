package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type enterpriseCarbonEvidenceCoordinator struct {
	mu                    sync.Mutex
	tests                 map[string]string
	authorizations        map[string]string
	authorizationVersions map[string]uint64
}

// EnterpriseCarbonTestSnapshot is an internal fixed-application test lease.
// Runtime versions fence late writes; persisted evidence never depends on them.
type EnterpriseCarbonTestSnapshot struct {
	Model            ge.AdmissionModel
	TestID           string
	AuthID           string
	OwnerID          string
	BindingID        string
	Resources        ge.ResourceBinding
	ExpectedRevision uint64
	Fingerprint      ge.CarbonEvidenceFingerprint
	StartedAt        time.Time
	ownerEpoch       uint64
	targetEpoch      uint64
}

type EnterpriseCarbonEvidenceError struct{ Code, Message string }

func (e *EnterpriseCarbonEvidenceError) Error() string       { return e.Message }
func (*EnterpriseCarbonEvidenceError) StatusCode() int       { return 409 }
func (*EnterpriseCarbonEvidenceError) IsRequestScoped() bool { return true }

func carbonEvidenceChanged() error {
	return &EnterpriseCarbonEvidenceError{"credential_changed", "The Enterprise application or authorization changed; reload and test again"}
}

// EnterpriseAdmissionEvidenceFingerprint reads immutable snapshots only, including
// from callers that already hold Manager.mu during credential selection.
func (m *Manager) EnterpriseAdmissionEvidenceFingerprint(auth *Auth, model ge.AdmissionModel) ge.CarbonEvidenceFingerprint {
	if auth == nil || auth.Provider != ge.Provider {
		return ge.CarbonEvidenceFingerprint{}
	}
	var cfg *config.Config
	if m != nil {
		cfg = m.runtimeConfigSnapshot()
	}
	proxy := strings.TrimSpace(auth.ProxyURL)
	if proxy == "" {
		proxy = ge.String(auth.Metadata, "proxy_url")
	}
	if proxy == "" && cfg != nil {
		proxy = strings.TrimSpace(cfg.ProxyURL)
	}
	headers := map[string]string{}
	for key, value := range auth.Attributes {
		if strings.HasPrefix(strings.ToLower(key), "header:") {
			headers[strings.ToLower(key)] = value
		}
	}
	var payload config.PayloadConfig
	if cfg != nil {
		payload.Default = admissionPayloadRules(cfg.Payload.Default, model.ID)
		payload.DefaultRaw = admissionPayloadRules(cfg.Payload.DefaultRaw, model.ID)
		payload.Override = admissionPayloadRules(cfg.Payload.Override, model.ID)
		payload.OverrideRaw = admissionPayloadRules(cfg.Payload.OverrideRaw, model.ID)
		for _, rule := range cfg.Payload.Filter {
			if models := admissionPayloadModels(rule.Models, model.ID); len(models) > 0 {
				rule.Models = models
				payload.Filter = append(payload.Filter, rule)
			}
		}
	}
	configuration := struct {
		Version         int
		Proxy           string
		Headers         map[string]string
		MetadataHeaders any
		SecretReference string
		Payload         config.PayloadConfig
	}{1, proxy, headers, auth.Metadata["headers"], ge.String(auth.Metadata, "client_secret_env"), payload}
	return ge.CarbonEvidenceFingerprint{Authorization: ge.CarbonAuthorizationFingerprint(auth.Metadata), Binding: ge.CarbonBindingFingerprint(ge.FromMetadata(auth.Metadata)), Configuration: ge.CarbonFingerprintValue(configuration)}
}

func admissionPayloadModels(models []config.PayloadModelRule, modelID string) []config.PayloadModelRule {
	var matching []config.PayloadModelRule
	for _, model := range models {
		if ge.MatchModelPattern(model.Name, modelID) {
			matching = append(matching, model)
		}
	}
	return matching
}

func admissionPayloadRules(rules []config.PayloadRule, modelID string) []config.PayloadRule {
	var matching []config.PayloadRule
	for _, rule := range rules {
		if models := admissionPayloadModels(rule.Models, modelID); len(models) > 0 {
			rule.Models = models
			matching = append(matching, rule)
		}
	}
	return matching
}

func (m *Manager) EnterpriseAdmissionEvidenceState(auth *Auth, model ge.AdmissionModel) ge.CarbonEvidenceState {
	if auth == nil {
		return ge.CarbonEvidenceState{Reason: "test_required"}
	}
	settings, err := model.ReadSettings(auth.Metadata)
	if err != nil {
		return ge.CarbonEvidenceState{Reason: "invalid_settings"}
	}
	evidence := settings.Evidence
	if evidence == nil {
		return ge.CarbonEvidenceState{Reason: "test_required"}
	}
	state := ge.CarbonEvidenceState{Test: evidence, NeedsReview: evidence.Status == "needs_review"}
	fingerprint := m.EnterpriseAdmissionEvidenceFingerprint(auth, model)
	switch {
	case fingerprint.Authorization == "":
		state.Reason = "authorization_unverified"
	case evidence.AuthorizationFingerprint != fingerprint.Authorization:
		state.Reason = "authorization_changed"
	case evidence.BindingFingerprint != fingerprint.Binding:
		state.Reason = "binding_changed"
	case evidence.ConfigFingerprint != fingerprint.Configuration:
		state.Reason = "configuration_changed"
	case state.NeedsReview:
		state.Reason = "needs_review"
	case evidence.Status == "incomplete":
		state.Reason = "test_incomplete"
	case evidence.Status != "verified":
		state.Reason = "test_failed"
	default:
		state.Valid = model.EvidenceMatches(evidence, fingerprint)
	}
	return state
}

// EnterpriseCarbonAuthorizationSnapshotRevision is a runtime-only fence for
// explicit reauthorization, including reauthorization with unchanged material.
func (m *Manager) EnterpriseCarbonAuthorizationSnapshotRevision(ownerID string) uint64 {
	if m == nil {
		return 0
	}
	m.enterpriseCarbonEvidence.mu.Lock()
	defer m.enterpriseCarbonEvidence.mu.Unlock()
	return m.enterpriseCarbonEvidence.authorizationVersions[ownerID]
}

// EnterpriseCarbonAuthorizationSnapshotMatches confines a management operation
// to its original grant, while allowing only manager-observed OAuth refreshes.
// The fingerprint is internal server state, never a client-provided admission.
func (m *Manager) EnterpriseCarbonAuthorizationSnapshotMatches(ownerID string, registrationEpoch, authorizationRevision uint64, fingerprint string) bool {
	if m == nil || ownerID == "" || fingerprint == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	owner := m.auths[ownerID]
	return owner != nil && owner.Provider == ge.Provider && !enterpriseRetired(owner) && owner.RegistrationEpoch == registrationEpoch && m.EnterpriseCarbonAuthorizationSnapshotRevision(ownerID) == authorizationRevision &&
		m.sameCarbonAuthorizationLocked(ownerID, fingerprint, ge.CarbonAuthorizationFingerprint(owner.Metadata))
}

func (m *Manager) BeginEnterpriseCarbonTest(auth *Auth, expectedRevision uint64) (*EnterpriseCarbonTestSnapshot, error) {
	return m.BeginEnterpriseCarbonTestContext(context.Background(), auth, expectedRevision)
}

func (m *Manager) BeginEnterpriseAdmissionTestContext(ctx context.Context, auth *Auth, expectedRevision uint64, model ge.AdmissionModel) (*EnterpriseCarbonTestSnapshot, error) {
	if m == nil || auth == nil || auth.Provider != ge.Provider || expectedRevision == 0 {
		return nil, carbonEvidenceChanged()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := m.lockAuthMutationContext(ctx, EnterpriseOwnerID(auth))
	if err != nil {
		return nil, err
	}
	defer release()
	m.mu.RLock()
	current := m.carbonTargetLocked(auth.ID, EnterpriseBindingID(auth))
	var owner *Auth
	if current != nil {
		current = current.Clone()
		owner = m.auths[EnterpriseOwnerID(current)].Clone()
	}
	m.mu.RUnlock()
	if current == nil || owner == nil {
		return nil, carbonEvidenceChanged()
	}
	if err := m.EnterpriseAdmissionTestEligibility(current, model); err != nil {
		return nil, err
	}
	revision, ok := carbonCollectionRevision(owner, EnterpriseBindingID(current))
	if !ok || revision != expectedRevision {
		return nil, carbonEvidenceChanged()
	}
	fingerprint := m.EnterpriseAdmissionEvidenceFingerprint(current, model)
	if fingerprint.Authorization == "" || fingerprint.Binding == "" || fingerprint.Configuration == "" {
		return nil, &EnterpriseCarbonEvidenceError{"authorization_unverified", "A stable OAuth refresh authorization is required to verify " + model.Label}
	}
	snapshot := &EnterpriseCarbonTestSnapshot{Model: model, TestID: uuid.NewString(), AuthID: current.ID, OwnerID: owner.ID, BindingID: EnterpriseBindingID(current), Resources: ge.Resources(ge.FromMetadata(current.Metadata)), ExpectedRevision: expectedRevision, Fingerprint: fingerprint, StartedAt: m.enterprisePolicies.now().UTC(), ownerEpoch: owner.RegistrationEpoch, targetEpoch: current.RegistrationEpoch}
	c := &m.enterpriseCarbonEvidence
	c.mu.Lock()
	if c.tests == nil {
		c.tests = map[string]string{}
	}
	c.tests[snapshot.testKey()] = snapshot.TestID
	c.mu.Unlock()
	return snapshot, nil
}

func (m *Manager) CancelEnterpriseCarbonTest(snapshot *EnterpriseCarbonTestSnapshot) {
	if m == nil || snapshot == nil {
		return
	}
	release, err := m.lockAuthMutationContext(context.Background(), snapshot.OwnerID)
	if err != nil {
		return
	}
	defer release()
	coordinator := &m.enterpriseCarbonEvidence
	coordinator.mu.Lock()
	key := snapshot.testKey()
	if coordinator.tests[key] == snapshot.TestID {
		delete(coordinator.tests, key)
	}
	coordinator.mu.Unlock()
}

func carbonCollectionRevision(owner *Auth, bindingID string) (uint64, bool) {
	if owner == nil {
		return 0, false
	}
	account, err := ge.AccountFromLegacy(owner.Metadata)
	if err != nil {
		return 0, false
	}
	for _, collection := range account.Collections {
		if collection.ID == bindingID {
			return collection.Revision, true
		}
	}
	return 0, false
}

// carbonTargetLocked also handles a legacy credential migrated while an
// administrative test was running; resources and runtime versions are rechecked.
func (m *Manager) carbonTargetLocked(id, bindingID string) *Auth {
	target := m.auths[id]
	if target == nil {
		return nil
	}
	if IsEnterpriseAccountAuth(target) {
		account, err := ge.ReadAccount(target.Metadata)
		if err != nil || account == nil {
			return nil
		}
		for _, collection := range account.Collections {
			if collection.ID == bindingID {
				return m.auths[enterpriseChildID(account.ID, bindingID)]
			}
		}
		return nil
	}
	return target
}

func carbonEvidenceTemporary(evidence *ge.CarbonTestEvidence) bool {
	if evidence == nil || evidence.Status == "verified" || evidence.Status == "needs_review" {
		return false
	}
	if evidence.Status == "incomplete" {
		return false
	}
	switch evidence.Reason {
	case "verification_incomplete", "model_verification_incomplete", "model_verification_mismatch", "model_changed":
		return false
	}
	if evidence.HTTPStatus == 408 || evidence.HTTPStatus == 429 || evidence.HTTPStatus >= 500 {
		return true
	}
	switch evidence.Reason {
	case "cancelled", "canceled", "timeout", "rate_limited", "upstream_unavailable", "upstream_error", "upstream_overloaded", "transport_error", "incomplete_upstream":
		return true
	}
	return false
}

func carbonSafeReason(reason string) string {
	if len(reason) == 0 || len(reason) > 64 {
		return "test_failed"
	}
	for _, ch := range reason {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
			return "test_failed"
		}
	}
	return reason
}

// RecordEnterpriseCarbonTestEvidence accepts only results from a current
// server-created lease. It never modifies the user's model enablement choice.
func (m *Manager) RecordEnterpriseCarbonTestEvidence(ctx context.Context, snapshot *EnterpriseCarbonTestSnapshot, outcome *ge.CarbonTestEvidence) (*Auth, error) {
	if m == nil || snapshot == nil || outcome == nil {
		return nil, carbonEvidenceChanged()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model := snapshot.admissionModel()
	result, err := m.mutateEnterpriseAdmissionMetadata(ctx, model, snapshot.OwnerID, func(owner, target *Auth, cache map[string]any) (bool, error) {
		if target == nil || owner.RegistrationEpoch != snapshot.ownerEpoch || target.RegistrationEpoch != snapshot.targetEpoch || ge.Resources(ge.FromMetadata(target.Metadata)) != snapshot.Resources || target.Disabled || owner.Disabled || target.Status == StatusDisabled || owner.Status == StatusDisabled {
			return false, carbonEvidenceChanged()
		}
		if revision, ok := carbonCollectionRevision(owner, snapshot.BindingID); !ok || revision != snapshot.ExpectedRevision {
			return false, carbonEvidenceChanged()
		}
		if err := m.EnterpriseAdmissionTestEligibility(target, model); err != nil {
			return false, err
		}
		fingerprint := m.EnterpriseAdmissionEvidenceFingerprint(target, model)
		if fingerprint.Binding != snapshot.Fingerprint.Binding || fingerprint.Configuration != snapshot.Fingerprint.Configuration || !m.sameCarbonAuthorizationLocked(snapshot.OwnerID, snapshot.Fingerprint.Authorization, fingerprint.Authorization) {
			return false, carbonEvidenceChanged()
		}
		coordinator := &m.enterpriseCarbonEvidence
		coordinator.mu.Lock()
		current := coordinator.tests[snapshot.testKey()]
		coordinator.mu.Unlock()
		if current != snapshot.TestID {
			return false, carbonEvidenceChanged()
		}
		evidence := *outcome
		evidence.Version, evidence.TestID, evidence.CheckedAt = 1, snapshot.TestID, m.enterprisePolicies.now().UTC()
		evidence.AuthorizationFingerprint, evidence.BindingFingerprint, evidence.ConfigFingerprint = fingerprint.Authorization, fingerprint.Binding, fingerprint.Configuration
		if evidence.Status == "verified" {
			evidence.Reason = ""
			if evidence.UpstreamModel != model.ID || evidence.HTTPStatus != 200 {
				return false, &EnterpriseCarbonEvidenceError{"test_incomplete", model.Label + " success requires a complete response and the actual upstream " + model.Label + " model"}
			}
		} else {
			evidence.Reason = carbonSafeReason(evidence.Reason)
		}
		if !model.ValidEvidence(&evidence) {
			return false, &EnterpriseCarbonEvidenceError{"test_incomplete", "The " + model.Label + " verification result is incomplete"}
		}
		if carbonEvidenceTemporary(&evidence) && m.EnterpriseAdmissionEvidenceState(target, model).Valid {
			return false, nil
		}
		cache[fingerprint.Binding] = evidence
		return true, nil
	}, snapshot.AuthID, snapshot.BindingID)
	if err == nil {
		m.CancelEnterpriseCarbonTest(snapshot)
	}
	return result, err
}

func (m *Manager) mutateEnterpriseAdmissionMetadata(ctx context.Context, model ge.AdmissionModel, ownerID string, mutate func(*Auth, *Auth, map[string]any) (bool, error), authID, bindingID string) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := m.lockAuthMutationContext(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	defer release()
	m.mu.Lock()
	owner, target := m.auths[ownerID], m.carbonTargetLocked(authID, bindingID)
	if owner == nil || target == nil || owner.Provider != ge.Provider || enterpriseRetired(owner) {
		m.mu.Unlock()
		return nil, carbonEvidenceChanged()
	}
	cache := model.ReadEvidenceStore(owner.Metadata)
	changed, err := mutate(owner, target, cache)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if !changed {
		result := owner.Clone()
		m.mu.Unlock()
		return result, nil
	}
	updated, before := owner.Clone(), owner.Clone()
	encoded, err := json.Marshal(cache)
	if err != nil || len(encoded) > 16*1024*1024 {
		m.mu.Unlock()
		return nil, fmt.Errorf("Carbon evidence store exceeds its metadata budget")
	}
	var stored map[string]any
	if json.Unmarshal(encoded, &stored) != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("invalid %s evidence metadata", model.Label)
	}
	updated.Metadata[model.EvidenceStoreKey], updated.Storage = stored, nil
	storeContext := ctx
	if account, errRead := ge.ReadAccount(owner.Metadata); errRead != nil {
		m.mu.Unlock()
		return nil, errRead
	} else if account != nil {
		storeContext = withEnterpriseStoreRevision(ctx, account.Revision)
		account.Revision++
		updated.Metadata = ge.PutAccount(updated.Metadata, *account)
	}
	updated.Generation, updated.UpdatedAt = owner.Generation+1, m.enterprisePolicies.now().UTC()
	if err = m.persistLocked(storeContext, updated); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("persist Carbon evidence: %w", err)
	}
	currentOwner := m.auths[ownerID]
	if currentOwner == nil {
		m.mu.Unlock()
		return nil, carbonEvidenceChanged()
	}
	mergeAuthSaveDelta(updated, before, currentOwner, false)
	updated.Generation = currentOwner.Generation + 1
	m.auths[ownerID] = updated
	m.notifyAuthChangeLocked(ownerID)
	snapshots := m.reconcileEnterpriseAccountLocked(updated)
	if len(snapshots) == 0 {
		snapshots = []*Auth{updated.Clone()}
	}
	result := updated.Clone()
	m.mu.Unlock()
	release()
	m.publishEnterpriseSnapshots(snapshots)
	m.hook.OnAuthUpdated(ctx, result.Clone())
	m.enterpriseAccountPublished(ctx, result)
	return result, nil
}

// The legacy test ID is accepted for older clients but is not an enablement gate.
func (m *Manager) UpdateEnterpriseAdmissionEnabled(ctx context.Context, auth *Auth, expectedRevision uint64, enabled bool, model ge.AdmissionModel) (*Auth, error) {
	if m == nil || auth == nil || expectedRevision == 0 {
		return nil, carbonEvidenceChanged()
	}
	owner, _, exists := m.EnterpriseAccountSnapshot(auth.ID)
	if !exists {
		current, ok := m.GetByID(EnterpriseOwnerID(auth))
		if !ok {
			return nil, carbonEvidenceChanged()
		}
		if _, err := ge.AccountFromLegacy(current.Metadata); err != nil {
			return nil, carbonEvidenceChanged()
		}
		owner = current
	}
	bindingID := EnterpriseBindingID(auth)
	return m.UpdateEnterpriseAccount(ctx, owner, 0, func(account *ge.Account) error {
		for i := range account.Collections {
			collection := &account.Collections[i]
			if collection.ID != bindingID {
				continue
			}
			if collection.Revision != expectedRevision {
				return carbonEvidenceChanged()
			}
			current := m.carbonTargetLocked(auth.ID, bindingID)
			if current == nil {
				return carbonEvidenceChanged()
			}
			if enabled {
				if err := m.enterpriseAdmissionEligibility(current, m.EnterpriseSubscriptionSnapshot(current), model); err != nil {
					return err
				}
			}
			if collection.Settings == nil {
				collection.Settings = map[string]any{}
			}
			collection.Settings[model.EnabledKey] = enabled
			return nil
		}
		return carbonEvidenceChanged()
	})
}

func (m *Manager) MarkEnterpriseAdmissionNeedsReview(ctx context.Context, auth *Auth, reason string, httpStatus int, model ge.AdmissionModel) error {
	if m == nil || auth == nil || auth.Provider != ge.Provider {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := m.mutateEnterpriseAdmissionMetadata(context.WithoutCancel(ctx), model, EnterpriseOwnerID(auth), func(owner, target *Auth, cache map[string]any) (bool, error) {
		if target.RegistrationEpoch != auth.RegistrationEpoch || ge.CarbonAuthorizationFingerprint(target.Metadata) != ge.CarbonAuthorizationFingerprint(auth.Metadata) || ge.Resources(ge.FromMetadata(target.Metadata)) != ge.Resources(ge.FromMetadata(auth.Metadata)) {
			return false, nil
		}
		state := m.EnterpriseAdmissionEvidenceState(target, model)
		if state.Test == nil || !state.Valid {
			return false, nil
		}
		source, errRead := model.ReadSettings(auth.Metadata)
		if errRead != nil || source.Evidence == nil || source.Evidence.TestID != state.Test.TestID {
			return false, nil
		}
		evidence := *state.Test
		evidence.Status, evidence.Reason, evidence.HTTPStatus = "needs_review", carbonSafeReason(reason), httpStatus
		cache[evidence.BindingFingerprint] = evidence
		return true, nil
	}, auth.ID, EnterpriseBindingID(auth))
	return err
}

func (snapshot *EnterpriseCarbonTestSnapshot) admissionModel() ge.AdmissionModel {
	if snapshot.Model.ID == "" {
		return ge.CarbonAdmissionModel
	}
	return snapshot.Model
}
func (snapshot *EnterpriseCarbonTestSnapshot) testKey() string {
	key := snapshot.OwnerID + "\x00" + snapshot.Fingerprint.Binding
	if snapshot.admissionModel().ID != ge.CarbonModel {
		key += "\x00" + snapshot.Model.ID
	}
	return key
}
func (m *Manager) EnterpriseCarbonEvidenceFingerprint(auth *Auth) ge.CarbonEvidenceFingerprint {
	return m.EnterpriseAdmissionEvidenceFingerprint(auth, ge.CarbonAdmissionModel)
}
func carbonPayloadModels(models []config.PayloadModelRule) []config.PayloadModelRule {
	return admissionPayloadModels(models, ge.CarbonModel)
}
func carbonPayloadRules(rules []config.PayloadRule) []config.PayloadRule {
	return admissionPayloadRules(rules, ge.CarbonModel)
}
func (m *Manager) EnterpriseCarbonEvidenceState(auth *Auth) ge.CarbonEvidenceState {
	return m.EnterpriseAdmissionEvidenceState(auth, ge.CarbonAdmissionModel)
}
func (m *Manager) BeginEnterpriseCarbonTestContext(ctx context.Context, auth *Auth, expectedRevision uint64) (*EnterpriseCarbonTestSnapshot, error) {
	return m.BeginEnterpriseAdmissionTestContext(ctx, auth, expectedRevision, ge.CarbonAdmissionModel)
}
func (m *Manager) mutateEnterpriseCarbonMetadata(ctx context.Context, ownerID string, mutate func(*Auth, *Auth, map[string]any) (bool, error), authID, bindingID string) (*Auth, error) {
	return m.mutateEnterpriseAdmissionMetadata(ctx, ge.CarbonAdmissionModel, ownerID, mutate, authID, bindingID)
}
func (m *Manager) UpdateEnterpriseCarbonEnabled(ctx context.Context, auth *Auth, expectedRevision uint64, enabled bool, _ string) (*Auth, error) {
	return m.UpdateEnterpriseAdmissionEnabled(ctx, auth, expectedRevision, enabled, ge.CarbonAdmissionModel)
}
func (m *Manager) MarkEnterpriseCarbonNeedsReview(ctx context.Context, auth *Auth, reason string, status int) error {
	return m.MarkEnterpriseAdmissionNeedsReview(ctx, auth, reason, status, ge.CarbonAdmissionModel)
}

func (m *Manager) EnterpriseHydrogenEvidenceFingerprint(auth *Auth) ge.CarbonEvidenceFingerprint {
	return m.EnterpriseAdmissionEvidenceFingerprint(auth, ge.HydrogenAdmissionModel)
}
func (m *Manager) EnterpriseHydrogenEvidenceState(auth *Auth) ge.CarbonEvidenceState {
	return m.EnterpriseAdmissionEvidenceState(auth, ge.HydrogenAdmissionModel)
}
func (m *Manager) BeginEnterpriseHydrogenTest(auth *Auth, revision uint64) (*EnterpriseCarbonTestSnapshot, error) {
	return m.BeginEnterpriseAdmissionTestContext(context.Background(), auth, revision, ge.HydrogenAdmissionModel)
}
func (m *Manager) BeginEnterpriseHydrogenTestContext(ctx context.Context, auth *Auth, revision uint64) (*EnterpriseCarbonTestSnapshot, error) {
	return m.BeginEnterpriseAdmissionTestContext(ctx, auth, revision, ge.HydrogenAdmissionModel)
}
func (m *Manager) UpdateEnterpriseHydrogenEnabled(ctx context.Context, auth *Auth, revision uint64, enabled bool, _ string) (*Auth, error) {
	return m.UpdateEnterpriseAdmissionEnabled(ctx, auth, revision, enabled, ge.HydrogenAdmissionModel)
}
func (m *Manager) MarkEnterpriseHydrogenNeedsReview(ctx context.Context, auth *Auth, reason string, status int) error {
	return m.MarkEnterpriseAdmissionNeedsReview(ctx, auth, reason, status, ge.HydrogenAdmissionModel)
}
