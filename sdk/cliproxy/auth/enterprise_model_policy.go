package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const enterpriseSubscriptionTTL = 5 * time.Minute
const enterpriseSubscriptionMaxStale = 30 * time.Minute

type enterpriseSubscriptionEntry struct {
	observation ge.SubscriptionObservation
	lastSuccess time.Time
	next        time.Time
	attempts    int
	pending     uint64
	failed      bool
	revision    uint64
}

// Lock order: Manager.mu -> enterprisePolicies.mu -> enterpriseQuota.mu.
type enterprisePolicyCoordinator struct {
	mu            sync.Mutex
	subscriptions map[string]enterpriseSubscriptionEntry
	catalogs      map[string][]ge.Model
	catalogStates map[string]ge.CatalogState
	aliases       map[string]string
	sequence      uint64
	nowFunc       func() time.Time
}

func (c *enterprisePolicyCoordinator) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

// EnterpriseSubscriptionKey excludes short-lived access tokens and engine choices.
func EnterpriseSubscriptionKey(auth *Auth) string {
	if auth == nil || auth.Provider != ge.Provider {
		return ""
	}
	b := ge.FromMetadata(auth.Metadata).WithDefaults()
	principal := ge.AccountID(ge.String(auth.Metadata, "email"), b.ClientID)
	if principal == "" {
		principal = EnterpriseOwnerID(auth)
	}
	authorization := ge.String(auth.Metadata, "refresh_token")
	if authorization == "" {
		authorization = ge.String(auth.Metadata, "access_token")
	}
	sum := sha256.Sum256([]byte(principal + "\x00" + b.ClientID + "\x00" + authorization + "\x00" + b.ProjectID + "\x00" + b.Location))
	return hex.EncodeToString(sum[:])
}

func enterprisePolicyCatalogKey(auth *Auth) string {
	sum := sha256.Sum256([]byte(EnterpriseOwnerID(auth) + "\x00" + EnterpriseSubscriptionKey(auth) + "\x00" + ge.ApplicationBinding(ge.FromMetadata(auth.Metadata))))
	return hex.EncodeToString(sum[:])
}

// EnterpriseSubscriptionRefresh is a single-flight lease; obsolete completions are ignored.
type EnterpriseSubscriptionRefresh struct {
	manager *Manager
	auth    *Auth
	key     string
	rawKey  string
	lease   uint64
}

func (c *enterprisePolicyCoordinator) subscriptionKey(auth *Auth) string {
	key := EnterpriseSubscriptionKey(auth)
	if canonical := c.aliases[key]; canonical != "" {
		return canonical
	}
	return key
}

func (c *enterprisePolicyCoordinator) carrySubscriptionRotationLocked(previous, current *Auth) {
	entry, ok := c.subscriptions[c.subscriptionKey(previous)]
	if !ok || entry.lastSuccess.IsZero() {
		return
	}
	key := EnterpriseSubscriptionKey(current)
	if project := entry.observation.ProjectNumber; project != "" {
		canonical := current.Clone()
		canonical.Metadata["project_id"] = project
		numberKey := EnterpriseSubscriptionKey(canonical)
		if key != numberKey {
			if c.aliases == nil {
				c.aliases = map[string]string{}
			}
			c.aliases[key] = numberKey
			key = numberKey
		}
	}
	if other := c.subscriptions[key]; other.revision >= entry.revision {
		return
	}
	entry.pending, entry.next = 0, time.Time{}
	c.subscriptions[key] = entry
}

func (m *Manager) BeginEnterpriseSubscriptionRefresh(auth *Auth) *EnterpriseSubscriptionRefresh {
	rawKey := EnterpriseSubscriptionKey(auth)
	if rawKey == "" || IsEnterpriseAccountAuth(auth) {
		return nil
	}
	c := &m.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.subscriptionKey(auth)
	if c.subscriptions == nil {
		c.subscriptions = map[string]enterpriseSubscriptionEntry{}
	}
	entry := c.subscriptions[key]
	if entry.pending != 0 || c.now().Before(entry.next) {
		return nil
	}
	c.sequence++
	entry.pending = c.sequence
	c.subscriptions[key] = entry
	return &EnterpriseSubscriptionRefresh{manager: m, auth: auth.Clone(), key: key, rawKey: rawKey, lease: entry.pending}
}

func (f *EnterpriseSubscriptionRefresh) Cancel() {
	if f == nil {
		return
	}
	c := &f.manager.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.subscriptions[f.key]
	if entry.pending == f.lease {
		entry.pending = 0
		c.subscriptions[f.key] = entry
	}
}

func (f *EnterpriseSubscriptionRefresh) Complete(observation ge.SubscriptionObservation) bool {
	if f == nil {
		return false
	}
	m := f.manager
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[f.auth.ID]
	valid := current != nil && EnterpriseSubscriptionKey(current) == f.rawKey && current.RegistrationEpoch == f.auth.RegistrationEpoch && ge.String(current.Metadata, "access_token") == ge.String(f.auth.Metadata, "access_token")
	c := &m.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.subscriptions[f.key]
	if entry.pending != f.lease {
		return false
	}
	entry.pending = 0
	if !valid {
		c.subscriptions[f.key] = entry
		return false
	}
	key := f.key
	if observation.ProjectNumber != "" {
		canonical := f.auth.Clone()
		canonical.Metadata["project_id"] = observation.ProjectNumber
		canonicalKey := EnterpriseSubscriptionKey(canonical)
		if key != canonicalKey {
			if c.aliases == nil {
				c.aliases = map[string]string{}
			}
			c.aliases[f.rawKey] = canonicalKey
			delete(c.subscriptions, key)
			other := c.subscriptions[canonicalKey]
			if other.pending > f.lease || other.revision > f.lease {
				return false
			}
			if other.revision > entry.revision {
				entry = other
				entry.pending = 0
			}
			key = canonicalKey
		}
	}
	entry.revision = f.lease
	now := c.now()
	observation = cloneEnterpriseSubscription(observation)
	if !observation.Authoritative() {
		entry.failed = true
		entry.attempts = min(entry.attempts+1, 7)
		entry.next = now.Add(min(5*time.Second*time.Duration(1<<(entry.attempts-1)), enterpriseSubscriptionTTL))
		if entry.lastSuccess.IsZero() {
			entry.observation = observation
		}
	} else {
		entry.observation, entry.lastSuccess = observation, now
		entry.failed, entry.attempts, entry.next = false, 0, now.Add(enterpriseSubscriptionTTL)
	}
	c.subscriptions[key] = entry
	return true
}

func (m *Manager) InvalidateEnterpriseSubscription(auth *Auth) {
	c := &m.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.subscriptionKey(auth)
	if c.subscriptions == nil {
		return
	}
	entry := c.subscriptions[key]
	entry.next, entry.pending = time.Time{}, 0
	c.subscriptions[key] = entry
}

func (m *Manager) EnterpriseSubscriptionSnapshot(auth *Auth) ge.SubscriptionObservation {
	c := &m.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subscriptionSnapshotLocked(auth)
}

func cloneEnterpriseSubscription(observation ge.SubscriptionObservation) ge.SubscriptionObservation {
	if observation.FreeTrial != nil {
		value := *observation.FreeTrial
		observation.FreeTrial = &value
	}
	if observation.LastSuccessAt != nil {
		value := *observation.LastSuccessAt
		observation.LastSuccessAt = &value
	}
	return observation
}

func (c *enterprisePolicyCoordinator) subscriptionSnapshotLocked(auth *Auth) ge.SubscriptionObservation {
	now := c.now()
	entry := c.subscriptions[c.subscriptionKey(auth)]
	observation := cloneEnterpriseSubscription(entry.observation)
	if entry.lastSuccess.IsZero() {
		if observation.Status == "" {
			observation.Status, observation.Reason = "unknown", "subscription_unverified"
		}
		return observation
	}
	last := entry.lastSuccess.UTC()
	observation.LastSuccessAt = &last
	if observation.Status == "ok" {
		if !now.Before(entry.lastSuccess.Add(enterpriseSubscriptionMaxStale)) {
			observation.Status, observation.Reason = "unknown", "subscription_cache_expired"
		} else if entry.failed || !now.Before(entry.lastSuccess.Add(enterpriseSubscriptionTTL)) {
			observation.Status, observation.Reason = "stale", "subscription_refresh_failed"
		}
	}
	return observation
}

type EnterprisePolicyModel struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	MediaKind      string `json:"media_kind,omitempty"`
	Allowed        bool   `json:"allowed"`
	Reason         string `json:"reason,omitempty"`
	CatalogSource  string `json:"catalog_source"`
	UpstreamListed bool   `json:"upstream_listed"`
}

type EnterpriseModelPolicyView struct {
	ge.ModelPolicy
	CarbonAdmissionSupported bool                       `json:"carbon_admission_supported"`
	CarbonAdmission          ge.CarbonAdmission         `json:"carbon_admission"`
	GlobalExcludedModels     []string                   `json:"global_excluded_models"`
	Subscription             ge.SubscriptionObservation `json:"subscription"`
	ImageGenerationDisabled  bool                       `json:"image_generation_disabled"`
	CatalogState             ge.CatalogState            `json:"catalog_state"`
	CatalogKnown             bool                       `json:"catalog_known"`
	CatalogModels            []EnterprisePolicyModel    `json:"catalog_models"`
	EffectiveModels          []string                   `json:"effective_models"`
}

func (m *Manager) PublishEnterprisePolicyCatalog(auth *Auth, models []ge.Model) {
	c := &m.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogs == nil {
		c.catalogs = map[string][]ge.Model{}
	}
	c.catalogs[enterprisePolicyCatalogKey(auth)] = append([]ge.Model{}, models...)
	if c.catalogStates == nil {
		c.catalogStates = map[string]ge.CatalogState{}
	}
	now := c.now().UTC()
	c.catalogStates[enterprisePolicyCatalogKey(auth)] = ge.CatalogState{Source: "upstream", Status: "ok", CheckedAt: &now, LastSuccessAt: &now}
}

func (m *Manager) EnterpriseModelPolicyView(auth *Auth) EnterpriseModelPolicyView {
	return m.enterpriseModelPolicyView(auth, true)
}

func (m *Manager) enterpriseModelPolicyView(auth *Auth, includeCarbon bool) EnterpriseModelPolicyView {
	subscription := m.EnterpriseSubscriptionSnapshot(auth)
	policy := ge.ModelPolicyFor(auth.Metadata, subscription)
	var excluded []string
	if value, ok := auth.Attributes["excluded_models"]; ok {
		excluded = strings.Split(value, ",")
	} else if raw, ok := auth.Metadata["excluded_models"]; ok {
		data, _ := json.Marshal(raw)
		_ = json.Unmarshal(data, &excluded)
	}
	globalExcluded := []string{}
	cfg := m.runtimeConfigSnapshot()
	if cfg != nil {
		globalExcluded = append(globalExcluded, cfg.OAuthExcludedModels[ge.Provider]...)
		excluded = append(excluded, globalExcluded...)
	}
	for _, pattern := range excluded {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			policy.ExcludedModels = append(policy.ExcludedModels, pattern)
		}
	}
	var admission ge.CarbonAdmission
	if includeCarbon {
		admission = m.enterpriseCarbonAdmissionView(auth, subscription)
	}
	policy.CarbonAllowed = admission.Available
	view := EnterpriseModelPolicyView{ModelPolicy: policy, CarbonAdmissionSupported: includeCarbon, CarbonAdmission: admission, GlobalExcludedModels: globalExcluded, Subscription: subscription, CatalogModels: []EnterprisePolicyModel{}, EffectiveModels: []string{}}
	c := &m.enterprisePolicies
	c.mu.Lock()
	models, known := c.catalogs[enterprisePolicyCatalogKey(auth)]
	models = append([]ge.Model(nil), models...)
	state := cloneEnterpriseCatalogState(c.catalogStates[enterprisePolicyCatalogKey(auth)])
	c.mu.Unlock()
	if state.Source == "" {
		state.Source, state.Status = "none", "pending"
	}
	if known {
		if state.LastError != nil || state.Source == "cached" {
			state.Source, state.Status = "cached", "stale"
		} else {
			state.Source, state.Status = "upstream", "ok"
		}
	} else if state.LastError != nil {
		if setting, err := ge.CatalogFallbackSettings(auth.Metadata); err == nil {
			var source string
			models, source = ge.FallbackCatalog(setting, subscription, policy.AllModelsEnabled)
			if source != "none" {
				known, state.Source, state.Status = true, source, "fallback"
			}
		}
	}
	view.CatalogState, view.CatalogKnown = state, known
	manualModels := map[string]bool{}
	supplement := func(id string) {
		if !slices.ContainsFunc(models, func(model ge.Model) bool { return model.ID == id }) {
			models = append(models, ge.Model{ID: id, DisplayName: id})
			manualModels[id] = true
		}
	}
	if policy.ArgonForceEnabled {
		supplement(ge.ArgonModel)
		supplement(ge.CarbonModel)
	}
	if enabled, err := ge.ReadCarbonEnabled(auth.Metadata); err == nil && enabled {
		supplement(ge.CarbonModel)
	}
	view.ImageGenerationDisabled = cfg != nil && cfg.DisableImageGeneration == config.DisableImageGenerationAll
	for _, model := range models {
		automatic := strings.EqualFold(strings.TrimSpace(model.DisplayName), "auto")
		allowed := policy.Allows(model.ID) && !(policy.Restricted() && automatic)
		if model.ID == ge.HydrogenModel && (IsEnterpriseAccountAuth(auth) || auth.Disabled || auth.Status == StatusDisabled || enterpriseRetired(auth)) {
			allowed = false
		}
		if policy.ForcesArgon(model.ID) {
			allowed = true
			model.MediaKind = ""
		}
		mediaDisabled := model.MediaKind == "image" && cfg != nil && cfg.DisableImageGeneration == config.DisableImageGenerationAll
		if mediaDisabled {
			allowed = false
		}
		reason := ""
		if !allowed {
			if mediaDisabled {
				reason = "image_generation_disabled"
			}
			if reason == "" {
				reason = policy.Reason
			}
			if reason == "" && policy.EnforcedAllowedModels != nil && (!slices.Contains(policy.EnforcedAllowedModels, model.ID) || automatic) {
				reason = policy.EnforcedReason
			}
			if reason == "" {
				reason = "model_not_allowed"
			}
		}
		source := state.Source
		if manualModels[model.ID] {
			source = "manual"
		}
		view.CatalogModels = append(view.CatalogModels, EnterprisePolicyModel{ID: model.ID, DisplayName: model.DisplayName, MediaKind: model.MediaKind, Allowed: allowed, Reason: reason, CatalogSource: source, UpstreamListed: source == "upstream" || source == "cached"})
		if allowed {
			view.EffectiveModels = append(view.EffectiveModels, model.ID)
		}
	}
	return view
}

func (m *Manager) EnterpriseModelPolicySignature(auth *Auth) string {
	return m.EnterpriseModelPolicyView(auth).Signature()
}

func (view EnterpriseModelPolicyView) Signature() string {
	data, _ := json.Marshal(struct {
		Policy ge.ModelPolicy
		Source string
		Models []EnterprisePolicyModel
		Carbon ge.CarbonAdmission
	}{view.ModelPolicy, view.CatalogState.Source, view.CatalogModels, view.CarbonAdmission})
	return string(data)
}

type EnterpriseModelPolicyError struct {
	Code, Message string
	HTTPStatus    int
}

func EnterpriseModelPolicyForRequest(auth *Auth) ge.ModelPolicy {
	if auth != nil && auth.enterpriseQuota != nil {
		manager := auth.enterpriseQuota.manager
		if current, ok := manager.GetByID(auth.ID); ok && current.Provider == ge.Provider && current.RegistrationEpoch == auth.RegistrationEpoch && EnterpriseSubscriptionKey(current) == EnterpriseSubscriptionKey(auth) {
			return manager.EnterpriseModelPolicyView(current).ModelPolicy
		}
		return ge.ModelPolicy{Status: "blocked", Reason: "credential_changed", EnforcedAllowedModels: []string{}, EnforcedReason: "credential_changed"}
	}
	if auth == nil {
		return ge.ModelPolicy{Status: "blocked", Reason: "subscription_unverified", EnforcedAllowedModels: []string{}, EnforcedReason: "subscription_unverified"}
	}
	return ge.ModelPolicyFor(auth.Metadata, ge.SubscriptionObservation{})
}

func (e *EnterpriseModelPolicyError) Error() string       { return e.Message }
func (e *EnterpriseModelPolicyError) StatusCode() int     { return e.HTTPStatus }
func (*EnterpriseModelPolicyError) IsRequestScoped() bool { return true }

func policyError(policy ge.ModelPolicy, model string) error {
	if policy.Allows(canonicalModelKey(model)) {
		return nil
	}
	if policy.Status == "blocked" {
		return &EnterpriseModelPolicyError{"subscription_unverified", "The Enterprise collection is paused until the current user's assigned subscription is verified", 503}
	}
	if policy.EnforcedReason == "paid_subscription_restricted" {
		return &EnterpriseModelPolicyError{"model_not_allowed", "Paid Enterprise subscriptions permit only gemini-4-argon, gemini-4-carbon, gemini-4-hydrogen, gemini-3-pro-image and veo-3.1; collection model policies may further restrict these models", 400}
	}
	return &EnterpriseModelPolicyError{"model_not_allowed", "The Enterprise collection model policy does not allow this upstream model", 400}
}

// ValidateEnterpriseModelCatalog checks whether a bound model is eligible for a pinned test.
func (m *Manager) ValidateEnterpriseModelCatalog(auth *Auth, model string) error {
	if auth == nil || auth.Provider != ge.Provider || IsEnterpriseAccountAuth(auth) || enterpriseRetired(auth) {
		return &EnterpriseModelPolicyError{"credential_not_found", "Select a current Gemini Enterprise application", 409}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return &EnterpriseModelPolicyError{"credential_disabled", "Enable the Enterprise application before testing a model", 409}
	}
	return m.validateEnterpriseCatalogPolicy(auth, m.enterpriseModelPolicyView(auth, false), model, true)
}

func (m *Manager) validateEnterprisePolicyForRequest(ctx context.Context, auth *Auth, model string, catalog bool) error {
	if scope := enterpriseCarbonTestScope(ctx); scope != nil && enterpriseCarbonTestMatches(ctx, auth, model) {
		return m.EnterpriseAdmissionTestEligibility(auth, scope.model)
	}
	view := m.enterpriseModelPolicyView(auth, false)
	return m.validateEnterpriseCatalogPolicy(auth, view, model, catalog)
}

func (m *Manager) validateEnterpriseCatalogPolicy(auth *Auth, view EnterpriseModelPolicyView, model string, catalog bool) error {
	model = strings.TrimPrefix(canonicalModelKey(model), "models/")
	// A deferred model is checked again after payload rules resolve its final ID.
	if view.ForcesArgon(model) || model == "" && !catalog && view.ArgonForceEnabled {
		return nil
	}
	if model == "" && view.Status != "blocked" && (!catalog || !view.Restricted()) {
		return nil
	}
	if err := policyError(view.ModelPolicy, model); err != nil {
		return err
	}
	if !catalog {
		return nil
	}
	if view.CatalogKnown {
		for _, entry := range view.CatalogModels {
			if entry.ID == model && entry.Allowed {
				return nil
			}
		}
	}
	if view.CatalogKnown || model == ge.HydrogenModel && view.CatalogState.LastError != nil {
		return &EnterpriseModelPolicyError{"model_not_in_catalog", "The upstream model is not allowed by this collection's current catalog", 400}
	}
	// Hydrogen requires a catalog for the current application, including for tests.
	if model == ge.HydrogenModel {
		return &EnterpriseModelPolicyError{"model_catalog_unavailable", "The Enterprise collection model catalog is not available yet", 503}
	}
	// Keep the legacy registry fallback for embedders that do not publish a catalog.
	for _, entry := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if entry == nil || view.Restricted() && strings.EqualFold(strings.TrimSpace(entry.DisplayName), "auto") {
			continue
		}
		actual := strings.TrimSpace(entry.MetadataModelID)
		if actual == "" {
			actual = m.selectionModelForAuth(auth, entry.ID)
		}
		if strings.TrimPrefix(canonicalModelKey(actual), "models/") == model {
			return nil
		}
	}
	return &EnterpriseModelPolicyError{"model_catalog_unavailable", "The Enterprise collection model catalog is not available yet", 503}
}

// PublishEnterpriseSubscriptionIdentity updates runtime quota attribution without persistence.
func (m *Manager) PublishEnterpriseSubscriptionIdentity(source *Auth) {
	c := &m.enterprisePolicies
	var snapshots []*Auth
	m.mu.Lock()
	c.mu.Lock()
	observation := c.subscriptionSnapshotLocked(source)
	if !observation.Authoritative() {
		c.mu.Unlock()
		m.mu.Unlock()
		return
	}
	key := c.subscriptionKey(source)
	for _, current := range m.auths {
		currentKey := c.subscriptionKey(current)
		if current.Provider != ge.Provider || IsEnterpriseAccountAuth(current) || currentKey != key {
			continue
		}
		binding := ge.FromMetadata(current.Metadata)
		identity := &ge.QuotaIdentity{Binding: ge.Binding(binding), CheckedAt: observation.CheckedAt, Invalidated: observation.Status != "ok"}
		if observation.Status == "ok" {
			identity.Verified, identity.ProjectNumber, identity.Location, identity.Edition, identity.PrincipalHash = true, observation.ProjectNumber, binding.Location, observation.Edition, observation.PrincipalHash
			if !identity.ValidFor(binding) {
				continue
			}
		}
		next := current.Clone()
		next.EnterpriseQuotaIdentity, next.Generation = identity, current.Generation+1
		m.attachEnterpriseQuotaLocked(next)
		m.auths[next.ID] = next
		m.notifyAuthChangeLocked(next.ID)
		snapshots = append(snapshots, next.Clone())
	}
	c.mu.Unlock()
	m.mu.Unlock()
	for _, snapshot := range snapshots {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
	}
}
