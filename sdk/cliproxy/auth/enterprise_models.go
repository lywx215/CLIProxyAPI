package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const enterpriseModelNotEnabledCode = "enterprise_model_not_enabled"

func containsEnterpriseProvider(providers []string) bool {
	for _, provider := range providers {
		if canonicalSchedulingProvider(provider) == ge.Provider {
			return true
		}
	}
	return false
}

// This state is deliberately independent of quota pools and persisted auth state.
// Lock order is Manager.mu -> enterpriseModels.mu. Success never clears a rejection.
type enterpriseModelCoordinator struct {
	mu      sync.Mutex
	records map[string]map[string]time.Time
	nowFunc func() time.Time
}

func (c *enterpriseModelCoordinator) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

type enterpriseModelContextKey struct{}
type enterpriseModelProbe struct {
	authID   string
	expected ge.ResourceBinding
	model    string
	version  *enterpriseModelProbeVersion
}

type enterpriseModelProbeVersion struct {
	ownerID               string
	registrationEpoch     uint64
	ownerEpoch            uint64
	collectionRevision    uint64
	authorizationRevision uint64
	applicationWindow     uint64
	fingerprint           string
}
type enterpriseModelAttempts struct {
	mu              sync.Mutex
	location, model string
	generationModel string
	probe           *enterpriseModelProbe
}

// RecordEnterpriseGenerationModel pins the actual model after payload rules.
// It is request-local and does not alter models, credentials or shared cooldowns.
func RecordEnterpriseGenerationModel(ctx context.Context, model string) error {
	s := enterpriseAttempts(ctx)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generationModel != "" && s.generationModel != model {
		return &EnterpriseModelProbeError{Code: "retry_model_changed", Message: "An Enterprise retry must retain the original upstream model", HTTPStatus: http.StatusConflict}
	}
	s.generationModel = model
	return nil
}

func withEnterpriseModelAttempts(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	next := &enterpriseModelAttempts{}
	if previous := enterpriseAttempts(ctx); previous != nil {
		// Nested model executions must not inherit or mutate the parent's retry
		// constraints. Only an explicit immutable administrative probe is retained.
		next.probe = previous.probe
	}
	return context.WithValue(ctx, enterpriseModelContextKey{}, next)
}
func enterpriseAttempts(ctx context.Context) *enterpriseModelAttempts {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(enterpriseModelContextKey{}).(*enterpriseModelAttempts)
	return s
}

// WithEnterpriseModelProbe fixes an administrative probe to one credential and binding.
// It does not bypass disablement, cooldown, payload rules or normal token refresh.
func WithEnterpriseModelProbe(ctx context.Context, authID string, expected ge.ResourceBinding, model string) context.Context {
	return withEnterpriseModelProbe(ctx, &enterpriseModelProbe{authID: authID, expected: expected, model: canonicalModelKey(model)})
}

func withEnterpriseModelProbe(ctx context.Context, probe *enterpriseModelProbe) context.Context {
	return context.WithValue(ctx, enterpriseModelContextKey{}, &enterpriseModelAttempts{probe: probe})
}

// BeginEnterpriseModelProbe pins a versioned test to the current collection and
// authorization while allowing an ordinary refresh of the same OAuth grant.
func (m *Manager) BeginEnterpriseModelProbe(ctx context.Context, selected *Auth, model string, revision uint64) (context.Context, error) {
	if selected == nil {
		return nil, enterpriseModelProbeChanged()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[selected.ID]
	owner := m.auths[EnterpriseOwnerID(selected)]
	if owner == nil || owner.Provider != ge.Provider || owner.Disabled || owner.Status == StatusDisabled || enterpriseRetired(owner) ||
		current == nil || current.Provider != ge.Provider || current.Disabled || current.Status == StatusDisabled ||
		current.RegistrationEpoch != selected.RegistrationEpoch || EnterpriseSubscriptionKey(current) != EnterpriseSubscriptionKey(selected) ||
		ge.Resources(ge.FromMetadata(current.Metadata)) != ge.Resources(ge.FromMetadata(selected.Metadata)) {
		return nil, enterpriseModelProbeChanged()
	}
	bindingID := EnterpriseBindingID(current)
	if currentRevision, ok := carbonCollectionRevision(owner, bindingID); !ok || currentRevision != revision {
		return nil, enterpriseModelProbeChanged()
	}
	probe := &enterpriseModelProbe{
		authID: current.ID, expected: ge.Resources(ge.FromMetadata(current.Metadata)), model: canonicalModelKey(model),
		version: &enterpriseModelProbeVersion{
			ownerID: owner.ID, registrationEpoch: current.RegistrationEpoch, ownerEpoch: owner.RegistrationEpoch,
			collectionRevision: revision, authorizationRevision: m.EnterpriseCarbonAuthorizationSnapshotRevision(owner.ID),
			applicationWindow: current.enterpriseHydrogenWindow,
			fingerprint:       ge.CarbonAuthorizationFingerprint(owner.Metadata),
		},
	}
	return withEnterpriseModelProbe(ctx, probe), nil
}

// EnterpriseModelProbeMatches checks whether the versioned probe still belongs to
// the same collection and grant when a result is ready to be published.
func (m *Manager) EnterpriseModelProbeMatches(ctx context.Context) bool {
	s := enterpriseAttempts(ctx)
	if s == nil || s.probe == nil || s.probe.version == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enterpriseModelProbeMatchesLocked(s.probe, m.auths[s.probe.authID])
}

func (m *Manager) enterpriseModelProbeMatchesLocked(probe *enterpriseModelProbe, current *Auth) bool {
	version := probe.version
	owner := m.auths[version.ownerID]
	if current == nil || current.Provider != ge.Provider || current.Disabled || current.Status == StatusDisabled ||
		current.RegistrationEpoch != version.registrationEpoch || current.enterpriseHydrogenWindow != version.applicationWindow ||
		EnterpriseOwnerID(current) != version.ownerID ||
		ge.Resources(ge.FromMetadata(current.Metadata)) != probe.expected ||
		owner == nil || owner.Provider != ge.Provider || owner.Disabled || owner.Status == StatusDisabled || enterpriseRetired(owner) ||
		owner.RegistrationEpoch != version.ownerEpoch ||
		m.EnterpriseCarbonAuthorizationSnapshotRevision(owner.ID) != version.authorizationRevision {
		return false
	}
	currentFingerprint := ge.CarbonAuthorizationFingerprint(owner.Metadata)
	if currentFingerprint != version.fingerprint && !m.sameCarbonAuthorizationLocked(owner.ID, version.fingerprint, currentFingerprint) {
		return false
	}
	revision, ok := carbonCollectionRevision(owner, EnterpriseBindingID(current))
	return ok && revision == version.collectionRevision
}

// EnterpriseModelProbeError carries only fixed public diagnostics.
type EnterpriseModelProbeError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *EnterpriseModelProbeError) Error() string       { return e.Message }
func (e *EnterpriseModelProbeError) StatusCode() int     { return e.HTTPStatus }
func (*EnterpriseModelProbeError) IsRequestScoped() bool { return true }

func enterpriseModelProbeChanged() *EnterpriseModelProbeError {
	return &EnterpriseModelProbeError{Code: "credential_changed", Message: "The credential or application changed; reload the model list.", HTTPStatus: http.StatusConflict}
}

type enterpriseModelGateError struct{ error }

func (e *enterpriseModelGateError) Unwrap() error   { return e.error }
func (e *enterpriseModelGateError) StatusCode() int { return statusCodeFromError(e.error) }

// An application's policy can reject a model before sending without failing the
// request for other eligible applications or updating the rejecting application's health.
type enterpriseApplicationPolicyError struct{ *EnterpriseModelPolicyError }

func (e *enterpriseApplicationPolicyError) Unwrap() error { return e.EnterpriseModelPolicyError }

// IsEnterpriseApplicationPolicyRejection reports a local skip before upstream submission.
func IsEnterpriseApplicationPolicyRejection(err error) bool {
	var rejection *enterpriseApplicationPolicyError
	return errors.As(err, &rejection)
}

type enterpriseProbeStopError struct{ error }

func (e *enterpriseProbeStopError) Unwrap() error   { return e.error }
func (e *enterpriseProbeStopError) StatusCode() int { return statusCodeFromError(e.error) }
func (e *enterpriseProbeStopError) IsRequestScoped() bool {
	// The normal one-time OAuth refresh runs before request-scoped rule matching.
	// A safe initial 401 must reach it; matchRequestScopedErrorAction still stops
	// any probe replay after that refresh opportunity has been consumed.
	return statusCodeFromError(e.error) != http.StatusUnauthorized || isRequestScopedError(e.error) || coreexec.GenerationHasStarted(e.error)
}
func stopEnterpriseProbeRetry(ctx context.Context, err error) error {
	if err != nil && enterpriseProbeActive(ctx) {
		return &enterpriseProbeStopError{err}
	}
	return err
}

func enterpriseModelRejection(err error) *ge.GenerationError {
	var rejection *ge.GenerationError
	if errors.As(err, &rejection) && rejection.ModelNotEnabled {
		return rejection
	}
	return nil
}

func isEnterpriseModelAvailabilityError(err error) bool {
	var policy *EnterpriseModelPolicyError
	if errors.As(err, &policy) {
		return true
	}
	if enterpriseModelRejection(err) != nil {
		return true
	}
	var gate *enterpriseModelGateError
	if errors.As(err, &gate) {
		return true
	}
	var result *Error
	return errors.As(err, &result) && (result.Code == enterpriseModelNotEnabledCode || result.Code == "enterprise_model_gate")
}

func enterpriseRetryCandidateAllowed(ctx context.Context, auth *Auth) bool {
	s := enterpriseAttempts(ctx)
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probe != nil && (auth == nil || auth.ID != s.probe.authID) {
		return false
	}
	return s.location == "" || auth != nil && auth.Provider == ge.Provider && ge.FromMetadata(auth.Metadata).Location == s.location
}

func enterpriseProbeActive(ctx context.Context) bool {
	s := enterpriseAttempts(ctx)
	return s != nil && s.probe != nil
}

func enterpriseRetryRestricted(ctx context.Context) bool {
	s := enterpriseAttempts(ctx)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.location != ""
}

// ValidateEnterpriseModelRequest is read-only and must run after the final payload barrier.
func ValidateEnterpriseModelRequest(ctx context.Context, auth *Auth, model string) error {
	return validateEnterpriseModelRequest(ctx, auth, model, true)
}

// CommitEnterpriseModelSubmission rechecks policy at the last local step before sending.
func CommitEnterpriseModelSubmission(ctx context.Context, auth *Auth, model string) error {
	modelKey := strings.TrimPrefix(canonicalModelKey(model), "models/")
	if scope := enterpriseCarbonTestScope(ctx); scope != nil {
		return CommitEnterpriseAdmissionSubmission(ctx, auth, model, scope.model)
	}
	if auth == nil {
		return nil
	}
	allModels, _ := ge.ReadAllModelsEnabled(auth.Metadata)
	if allModels || ge.IsArgonFamilyModel(modelKey) || modelKey == ge.HydrogenModel {
		return ValidateEnterpriseModelRequest(ctx, auth, model)
	}
	return nil
}

func validateEnterpriseModelRequest(ctx context.Context, auth *Auth, model string, final bool) error {
	if auth == nil || auth.Provider != ge.Provider {
		return nil
	}
	model = strings.TrimPrefix(canonicalModelKey(model), "models/")
	binding := ge.FromMetadata(auth.Metadata)
	s := enterpriseAttempts(ctx)
	var probe *enterpriseModelProbe
	if s != nil {
		s.mu.Lock()
		probe = s.probe
		location, requiredModel := s.location, s.model
		s.mu.Unlock()
		if probe != nil {
			if auth.ID != probe.authID || ge.Resources(binding) != probe.expected {
				return enterpriseModelProbeChanged()
			}
			if final && model != strings.TrimPrefix(probe.model, "models/") {
				return &EnterpriseModelProbeError{Code: "model_changed", Message: "Request configuration changed the model being tested.", HTTPStatus: http.StatusConflict}
			}
		}
		if location != "" && (binding.Location != location || final && model != strings.TrimPrefix(requiredModel, "models/")) {
			return &enterpriseModelGateError{&Error{Code: "enterprise_model_gate", Message: "Enterprise retry requires the same region and upstream model", HTTPStatus: http.StatusConflict}}
		}
	}
	g := auth.enterpriseQuota
	if g == nil {
		if final {
			return policyError(ge.ModelPolicyFor(auth.Metadata, ge.SubscriptionObservation{}), model)
		}
		return nil
	}
	// Validate the live resource snapshot at the actual dispatch boundary.
	m := g.manager
	m.mu.RLock()
	current := m.auths[auth.ID]
	changed := current == nil || current.Provider != ge.Provider || current.Disabled || current.Status == StatusDisabled || current.RegistrationEpoch != auth.RegistrationEpoch || EnterpriseSubscriptionKey(current) != EnterpriseSubscriptionKey(auth) || ge.ApplicationBinding(ge.FromMetadata(current.Metadata)) != ge.ApplicationBinding(binding)
	if !changed && probe != nil && probe.version != nil {
		changed = !m.enterpriseModelProbeMatchesLocked(probe, current)
	}
	disabledCooling := m.cooldownDisabledForAuth(current)
	if current != nil {
		current = current.Clone()
	}
	m.mu.RUnlock()
	if changed {
		return enterpriseModelProbeChanged()
	}
	if final {
		if err := m.validateEnterprisePolicyForRequest(ctx, current, model, true); err != nil {
			var policy *EnterpriseModelPolicyError
			if s != nil && !enterpriseProbeActive(ctx) && enterpriseCarbonTestScope(ctx) == nil && errors.As(err, &policy) {
				return &enterpriseApplicationPolicyError{policy}
			}
			return err
		}
	}
	if final && !disabledCooling {
		if until := m.enterpriseModels.retryAt(auth, model); !until.IsZero() {
			return &enterpriseModelGateError{newModelCooldownErrorWithCause(model, ge.Provider, until.Sub(m.enterpriseModels.now()), &Error{Code: enterpriseModelNotEnabledCode, Message: "Google has not enabled this model for the application", HTTPStatus: 400})}
		}
	}
	return nil
}

func (c *enterpriseModelCoordinator) retryAt(auth *Auth, model string) time.Time {
	if auth == nil || auth.Provider != ge.Provider {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := ge.ApplicationBinding(ge.FromMetadata(auth.Metadata))
	until := c.records[key][strings.TrimPrefix(canonicalModelKey(model), "models/")]
	if !until.After(c.now()) {
		return time.Time{}
	}
	return until
}

func (m *Manager) observeEnterpriseModelError(ctx context.Context, auth *Auth, model string, err error) {
	rejection := enterpriseModelRejection(err)
	if rejection == nil || auth == nil || auth.Provider != ge.Provider {
		return
	}
	if rejection.Model != "" {
		model = rejection.Model
	}
	model = strings.TrimPrefix(canonicalModelKey(model), "models/")
	if model == "" {
		return
	}
	// Routing constraints belong to this request, even when a refreshed or
	// rebound credential fences the failure out of shared availability state.
	if s := enterpriseAttempts(ctx); s != nil && !rejection.Started && !coreexec.GenerationHasStarted(err) {
		s.mu.Lock()
		if s.location == "" {
			s.location, s.model = ge.FromMetadata(auth.Metadata).Location, model
		}
		s.mu.Unlock()
	}
	// Resource updates and registrations fence stale failures. A normal refresh
	// retains existing application records; no token is part of their key.
	m.mu.RLock()
	current := m.auths[auth.ID]
	if current == nil || isStaleExecutionResult(Result{CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch}, current) || ge.ApplicationBinding(ge.FromMetadata(current.Metadata)) != ge.ApplicationBinding(ge.FromMetadata(auth.Metadata)) {
		m.mu.RUnlock()
		return
	}
	c := &m.enterpriseModels
	c.mu.Lock()
	now := c.now()
	if c.records == nil {
		c.records = make(map[string]map[string]time.Time)
	}
	for key, models := range c.records {
		for id, until := range models {
			if !until.After(now) {
				delete(models, id)
			}
		}
		if len(models) == 0 {
			delete(c.records, key)
		}
	}
	key := ge.ApplicationBinding(ge.FromMetadata(auth.Metadata))
	if c.records[key] == nil {
		c.records[key] = make(map[string]time.Time)
	}
	c.records[key][model] = now.Add(time.Minute)
	c.mu.Unlock()
	m.mu.RUnlock()
	if model == ge.CarbonModel {
		// The request lease is already released from all coordinator locks before
		// persisting a model-specific review state. A newer proof must be fenced by
		// its original test identity in the evidence store.
		_ = m.MarkEnterpriseCarbonNeedsReview(ctx, auth, "model_not_enabled", rejection.HTTPStatus)
	}
}

func observeEnterpriseModelAttempt(ctx context.Context, auth *Auth, model string, err error) {
	if auth != nil && auth.enterpriseQuota != nil {
		auth.enterpriseQuota.manager.observeEnterpriseModelError(ctx, auth, model, err)
	}
}

func (m *Manager) enterpriseModelSelectionAuth(auth *Auth, model string) *Auth {
	if auth == nil || auth.Provider != ge.Provider || m.cooldownDisabledForAuth(auth) {
		return auth
	}
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		p := cfg.Payload
		if len(p.Default)+len(p.DefaultRaw)+len(p.Override)+len(p.OverrideRaw)+len(p.Filter) > 0 {
			// Request/header-scoped payload rules may replace the entire model spec.
			// Resolve admission at the final executor barrier instead of rejecting
			// a different actual model from an early logical-model projection.
			return auth
		}
	}
	until := m.enterpriseModels.retryAt(auth, model)
	if until.IsZero() {
		return auth
	}
	copyAuth := auth.Clone()
	state := ensureModelState(copyAuth, model)
	if !state.NextRetryAfter.After(until) {
		state.Unavailable, state.NextRetryAfter = true, until
		state.LastError = &Error{Code: enterpriseModelNotEnabledCode, Message: "Google has not enabled this model for the application", HTTPStatus: 400}
	}
	return copyAuth
}

func (g *enterpriseQuotaGate) modelAvailabilityViews(now time.Time) []CooldownView {
	if g == nil || !g.enabled() {
		return nil
	}
	c := &g.manager.enterpriseModels
	c.mu.Lock()
	defer c.mu.Unlock()
	var views []CooldownView
	for model, until := range c.records[ge.ApplicationBinding(ge.FromMetadata(g.policy.Metadata))] {
		if !until.After(now) {
			continue
		}
		views = append(views, CooldownView{Scope: "model", ModelKey: model, Reason: "model_not_enabled", RetryAt: until.UTC(), RemainingSeconds: int64((until.Sub(now) + time.Second - 1) / time.Second), HTTPStatus: 400})
	}
	sort.Slice(views, func(i, j int) bool { return strings.Compare(views[i].ModelKey, views[j].ModelKey) < 0 })
	return views
}
