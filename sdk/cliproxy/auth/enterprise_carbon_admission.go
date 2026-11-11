package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

type enterpriseCarbonModelTestKey struct{}
type enterpriseCarbonModelTest struct {
	authID   string
	expected ge.ResourceBinding
}

// reconcileEnterpriseCarbonAdmissionWindow preserves a request's admission
// window independently of Argon registration epochs and quota ownership. Call
// it under Manager.mu whenever an inference credential is published.
func reconcileEnterpriseCarbonAdmissionWindow(previous, next *Auth) {
	if next == nil || next.Provider != ge.Provider || IsEnterpriseAccountAuth(next) {
		return
	}
	window := uint64(1)
	if previous != nil && previous.Provider == ge.Provider {
		window = max(previous.enterpriseCarbonWindow, window)
		oldEnabled, oldExplicit := previous.Metadata[ge.CarbonEnabledKey].(bool)
		newEnabled, newExplicit := next.Metadata[ge.CarbonEnabledKey].(bool)
		if oldEnabled != newEnabled || oldExplicit != newExplicit || previous.Disabled != next.Disabled || (previous.Status == StatusDisabled) != (next.Status == StatusDisabled) {
			window++
		}
	}
	next.enterpriseCarbonWindow = window
}

// WithEnterpriseCarbonModelTest is created only by the authenticated management
// test handler. It does not register a public model or bypass paid/user policy.
func WithEnterpriseCarbonModelTest(ctx context.Context, authID string, expected ge.ResourceBinding) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = WithEnterpriseModelProbe(ctx, authID, expected, ge.CarbonModel)
	return context.WithValue(ctx, enterpriseCarbonModelTestKey{}, &enterpriseCarbonModelTest{authID: authID, expected: expected})
}

func enterpriseCarbonTestScope(ctx context.Context) *enterpriseCarbonModelTest {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(enterpriseCarbonModelTestKey{}).(*enterpriseCarbonModelTest)
	return scope
}

// EnterpriseCarbonModelTestActive allows the executor to return private test
// evidence through in-process response metadata, without changing public replies.
func EnterpriseCarbonModelTestActive(ctx context.Context) bool {
	scope := enterpriseCarbonTestScope(ctx)
	return scope != nil && scope.authID != "" && scope.expected.ProjectID != "" && scope.expected.EngineID != ""
}

func enterpriseCarbonTestMatches(ctx context.Context, auth *Auth, model string) bool {
	scope := enterpriseCarbonTestScope(ctx)
	return scope != nil && auth != nil && auth.Provider == ge.Provider && auth.ID == scope.authID &&
		(model == "" || canonicalModelKey(model) == ge.CarbonModel) &&
		ge.Resources(ge.FromMetadata(auth.Metadata)) == scope.expected
}

// EnterpriseCarbonTestEligibility evaluates only conditions a management probe
// cannot bypass. It is safe under Manager.mu and performs no network I/O.
func (m *Manager) EnterpriseCarbonTestEligibility(auth *Auth) error {
	if err := m.enterpriseCarbonEligibility(auth, m.EnterpriseSubscriptionSnapshot(auth)); err != nil {
		return err
	}
	if ge.CarbonAuthorizationFingerprint(auth.Metadata) == "" {
		return carbonPolicyError("authorization_unverified", "A stable OAuth refresh authorization is required to record a Carbon model test", http.StatusServiceUnavailable)
	}
	return nil
}

func (m *Manager) enterpriseCarbonEligibility(auth *Auth, subscription ge.SubscriptionObservation) error {
	if auth == nil || auth.Provider != ge.Provider || IsEnterpriseAccountAuth(auth) || enterpriseRetired(auth) {
		return carbonPolicyError("credential_not_found", "Select a Gemini Enterprise application before testing Carbon", http.StatusServiceUnavailable)
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return carbonPolicyError("credential_disabled", "Enable this credential and application before using Carbon", http.StatusServiceUnavailable)
	}
	if ge.FromMetadata(auth.Metadata).Validate() != nil {
		return carbonPolicyError("invalid_application_binding", "The Enterprise application binding is not valid", http.StatusConflict)
	}
	if (subscription.Status != "ok" && subscription.Status != "stale") || subscription.FreeTrial == nil {
		return carbonPolicyError("subscription_unverified", "Carbon requires the current user's verified and assigned Enterprise subscription", http.StatusServiceUnavailable)
	}
	policy := ge.ModelPolicyFor(auth.Metadata, subscription)
	if policy.Reason == "invalid_model_policy" {
		return carbonPolicyError("invalid_model_policy", "The saved Enterprise model policy is not valid", http.StatusBadRequest)
	}
	if policy.EffectiveMode == "custom" && policy.AllowedModels != nil && !slices.Contains(policy.AllowedModels, ge.CarbonModel) {
		return carbonPolicyError("model_not_allowed", "The application's custom whitelist does not allow Carbon", http.StatusBadRequest)
	}
	// Argon templates do not imply Carbon permission or exclusion. Only explicit
	// credential/provider patterns and a custom whitelist constrain this model.
	for _, pattern := range policy.ExcludedModels {
		if ge.MatchModelPattern(pattern, ge.CarbonModel) {
			return carbonPolicyError("model_excluded", "Carbon is explicitly excluded for this application", http.StatusBadRequest)
		}
	}
	if raw, ok := auth.Attributes["excluded_models"]; ok {
		for _, pattern := range strings.Split(raw, ",") {
			if ge.MatchModelPattern(pattern, ge.CarbonModel) {
				return carbonPolicyError("model_excluded", "Carbon is explicitly excluded for this credential", http.StatusBadRequest)
			}
		}
	}
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		for _, pattern := range cfg.OAuthExcludedModels[ge.Provider] {
			if ge.MatchModelPattern(pattern, ge.CarbonModel) {
				return carbonPolicyError("model_excluded", "Carbon is explicitly excluded for Gemini Enterprise", http.StatusBadRequest)
			}
		}
	}
	return nil
}

func carbonPolicyError(code, message string, status int) *EnterpriseModelPolicyError {
	return &EnterpriseModelPolicyError{Code: code, Message: message, HTTPStatus: status}
}

// EnterpriseCarbonUnavailableError is independent of other provider candidates.
func EnterpriseCarbonUnavailableError() error {
	return carbonPolicyError("carbon_unavailable", "No enabled and eligible Enterprise application is available for gemini-4-carbon", http.StatusServiceUnavailable)
}

// EnterpriseCarbonRoute recognizes the actual OAuth model behind shared aliases
// and credential prefixes without scanning credentials on ordinary text routes.
func (m *Manager) EnterpriseCarbonRoute(model string) bool {
	if m == nil {
		return false
	}
	base := canonicalModelKey(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
	lookup := func(name string) (bool, bool) {
		if table, ok := m.oauthModelAlias.Load().(*oauthModelAliasTable); ok && table != nil {
			if entry, found := table.reverse[ge.Provider][strings.ToLower(name)]; found {
				return canonicalModelKey(entry.upstreamModel) == ge.CarbonModel, true
			}
		}
		return name == ge.CarbonModel, false
	}
	if carbon, mapped := lookup(base); carbon || mapped {
		return carbon
	}
	separator := strings.LastIndexByte(base, '/')
	if separator < 1 {
		return false
	}
	if carbon, _ := lookup(base[separator+1:]); !carbon {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth.Provider == ge.Provider && !IsEnterpriseAccountAuth(auth) && auth.Prefix != "" && strings.HasPrefix(base, auth.Prefix+"/") && canonicalModelKey(m.selectionModelForAuth(auth, base)) == ge.CarbonModel {
			return true
		}
	}
	return false
}

func (m *Manager) routeEnterpriseCarbon(ctx context.Context, providers []string, model string) ([]string, error) {
	if m.HomeEnabled() || !m.EnterpriseCarbonRoute(model) {
		return providers, nil
	}
	if EnterpriseCarbonModelTestActive(ctx) {
		return []string{ge.Provider}, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth != nil && auth.Provider == ge.Provider && !IsEnterpriseAccountAuth(auth) && canonicalModelKey(m.selectionModelForAuth(auth, model)) == ge.CarbonModel && m.EnterpriseCarbonAdmissionView(auth).Available {
			return []string{ge.Provider}, nil
		}
	}
	return nil, EnterpriseCarbonUnavailableError()
}

// EnterpriseCarbonAdmissionView is read-only and never acquires Manager.mu. It
// can be evaluated from the existing selection and account transaction locks.
func (m *Manager) EnterpriseCarbonAdmissionView(auth *Auth) ge.CarbonAdmission {
	if m == nil {
		return ge.CarbonAdmission{Reason: "credential_not_found"}
	}
	return m.enterpriseCarbonAdmissionView(auth, m.EnterpriseSubscriptionSnapshot(auth))
}

func (m *Manager) enterpriseCarbonAdmissionView(auth *Auth, subscription ge.SubscriptionObservation) ge.CarbonAdmission {
	out := ge.CarbonAdmission{}
	if auth == nil {
		out.Reason = "credential_not_found"
		return out
	}
	enabled, err := ge.ReadCarbonEnabled(auth.Metadata)
	if err != nil {
		out.Reason = "invalid_carbon_settings"
		return out
	}
	if _, configured := auth.Metadata[ge.CarbonEnabledKey]; !configured {
		enabled = (subscription.Status == "ok" || subscription.Status == "stale") && subscription.FreeTrial != nil && !*subscription.FreeTrial
	}
	out.Enabled = enabled
	// Historical diagnostics never determine business admission. In particular,
	// changing payload rules or a failed test must not revoke a manual opening.
	if settings, errRead := ge.ReadCarbonSettings(auth.Metadata); errRead == nil && settings.Evidence != nil {
		out.Test = settings.Evidence.Summary()
	}
	if err = m.enterpriseCarbonEligibility(auth, subscription); err != nil {
		var policy *EnterpriseModelPolicyError
		if errors.As(err, &policy) {
			out.Reason = policy.Code
		}
		return out
	}
	out.CanTest = ge.CarbonAuthorizationFingerprint(auth.Metadata) != ""
	out.CanEnable = true
	if !enabled {
		out.Reason = "carbon_disabled"
		return out
	}
	out.Available = true
	return out
}

func (m *Manager) validateEnterpriseCarbonPolicy(ctx context.Context, auth *Auth) error {
	if enterpriseCarbonTestMatches(ctx, auth, ge.CarbonModel) {
		return m.EnterpriseCarbonTestEligibility(auth)
	}
	view := m.EnterpriseCarbonAdmissionView(auth)
	if view.Available {
		return nil
	}
	if err := m.EnterpriseCarbonTestEligibility(auth); err != nil {
		return err
	}
	return carbonPolicyError("carbon_unavailable", "Carbon is unavailable for this application: "+view.Reason, http.StatusServiceUnavailable)
}

// CommitEnterpriseCarbonSubmission is the linearization point immediately before
// one upstream send. Closing or rebinding an application takes Manager.mu for a
// write and therefore either precedes this decision or follows an already
// committed send. No manager lock is held across network I/O, and existing quota
// and concurrency leases retain their normal lifetime.
func CommitEnterpriseCarbonSubmission(ctx context.Context, auth *Auth, model string) error {
	if canonicalModelKey(model) != ge.CarbonModel {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if auth == nil || auth.enterpriseQuota == nil || auth.enterpriseQuota.manager == nil {
		return carbonPolicyError("subscription_unverified", "Carbon requires a current managed Enterprise application", http.StatusServiceUnavailable)
	}
	m := auth.enterpriseQuota.manager
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[auth.ID]
	if current == nil || current.Provider != ge.Provider || current.Disabled || current.Status == StatusDisabled || current.RegistrationEpoch != auth.RegistrationEpoch || current.enterpriseCarbonWindow != auth.enterpriseCarbonWindow ||
		ge.CarbonAuthorizationFingerprint(current.Metadata) != ge.CarbonAuthorizationFingerprint(auth.Metadata) ||
		ge.Resources(ge.FromMetadata(current.Metadata)) != ge.Resources(ge.FromMetadata(auth.Metadata)) {
		return carbonPolicyError("credential_changed", "The Carbon application changed before generation submission", http.StatusConflict)
	}
	if err := m.validateEnterpriseCarbonPolicy(ctx, current); err != nil {
		return err
	}
	if scope := enterpriseCarbonTestScope(ctx); scope != nil && !enterpriseCarbonTestMatches(ctx, current, ge.CarbonModel) {
		return carbonPolicyError("credential_changed", "The Carbon test application changed before generation submission", http.StatusConflict)
	}
	return nil
}
