package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
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
	return EnterpriseSubscriptionKey(auth) + "/" + ge.ApplicationBinding(ge.FromMetadata(auth.Metadata))
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
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	MediaKind   string `json:"media_kind,omitempty"`
	Allowed     bool   `json:"allowed"`
	Reason      string `json:"reason,omitempty"`
}

type EnterpriseModelPolicyView struct {
	ge.ModelPolicy
	GlobalExcludedModels []string                   `json:"global_excluded_models"`
	Subscription         ge.SubscriptionObservation `json:"subscription"`
	CatalogKnown         bool                       `json:"catalog_known"`
	CatalogModels        []EnterprisePolicyModel    `json:"catalog_models"`
	EffectiveModels      []string                   `json:"effective_models"`
}

func (m *Manager) PublishEnterprisePolicyCatalog(auth *Auth, models []ge.Model) {
	c := &m.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogs == nil {
		c.catalogs = map[string][]ge.Model{}
	}
	c.catalogs[enterprisePolicyCatalogKey(auth)] = append([]ge.Model{}, models...)
}

func (m *Manager) EnterpriseModelPolicyView(auth *Auth) EnterpriseModelPolicyView {
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
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		globalExcluded = append(globalExcluded, cfg.OAuthExcludedModels[ge.Provider]...)
		excluded = append(excluded, globalExcluded...)
	}
	for _, pattern := range excluded {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			policy.ExcludedModels = append(policy.ExcludedModels, pattern)
		}
	}
	view := EnterpriseModelPolicyView{ModelPolicy: policy, GlobalExcludedModels: globalExcluded, Subscription: subscription, CatalogModels: []EnterprisePolicyModel{}, EffectiveModels: []string{}}
	c := &m.enterprisePolicies
	c.mu.Lock()
	models, known := c.catalogs[enterprisePolicyCatalogKey(auth)]
	models = append([]ge.Model(nil), models...)
	c.mu.Unlock()
	view.CatalogKnown = known
	for _, model := range models {
		automatic := strings.EqualFold(strings.TrimSpace(model.DisplayName), "auto")
		allowed := policy.Allows(model.ID) && !(policy.Restricted() && automatic)
		reason := ""
		if !allowed {
			reason = policy.Reason
			if reason == "" && policy.EnforcedAllowedModels != nil && (!slices.Contains(policy.EnforcedAllowedModels, model.ID) || automatic) {
				reason = policy.EnforcedReason
			}
			if reason == "" {
				reason = "model_not_allowed"
			}
		}
		view.CatalogModels = append(view.CatalogModels, EnterprisePolicyModel{model.ID, model.DisplayName, model.MediaKind, allowed, reason})
		if allowed {
			view.EffectiveModels = append(view.EffectiveModels, model.ID)
		}
	}
	return view
}

func (m *Manager) EnterpriseModelPolicySignature(auth *Auth) string {
	view := m.EnterpriseModelPolicyView(auth)
	data, _ := json.Marshal(view.ModelPolicy)
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
	if policy.Status == "blocked" {
		return &EnterpriseModelPolicyError{"subscription_unverified", "The Enterprise collection is paused until the current user's assigned subscription is verified", 503}
	}
	if !policy.Allows(canonicalModelKey(model)) {
		if policy.EnforcedReason == "paid_subscription_restricted" {
			return &EnterpriseModelPolicyError{"model_not_allowed", "Paid Enterprise subscriptions permit only gemini-4-argon, gemini-3-pro-image and veo-3.1; collection model policies may further restrict these models", 400}
		}
		return &EnterpriseModelPolicyError{"model_not_allowed", "The Enterprise collection model policy does not allow this upstream model", 400}
	}
	return nil
}

func (m *Manager) validateEnterprisePolicy(auth *Auth, model string, catalog bool) error {
	view := m.EnterpriseModelPolicyView(auth)
	if model == "" && view.Status != "blocked" && (!catalog || !view.Restricted()) {
		return nil
	}
	if err := policyError(view.ModelPolicy, model); err != nil {
		return err
	}
	if !catalog {
		return nil
	}
	model = canonicalModelKey(model)
	if view.CatalogKnown {
		for _, entry := range view.CatalogModels {
			if entry.ID == model && entry.Allowed {
				return nil
			}
		}
		return &EnterpriseModelPolicyError{"model_not_in_catalog", "The upstream model is not allowed by this collection's current catalog", 400}
	}
	// Embedders can publish their own registry entries instead of using Service discovery.
	for _, entry := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if entry == nil || view.Restricted() && strings.EqualFold(strings.TrimSpace(entry.DisplayName), "auto") {
			continue
		}
		if canonicalModelKey(entry.ID) == model || canonicalModelKey(entry.MetadataModelID) == model || canonicalModelKey(m.selectionModelForAuth(auth, entry.ID)) == model {
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
