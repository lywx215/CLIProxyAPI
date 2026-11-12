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
	model    ge.AdmissionModel
}

// reconcileEnterpriseCarbonAdmissionWindow preserves a request's admission
// window independently of Argon registration epochs and quota ownership. Call
// it under Manager.mu whenever an inference credential is published.
func reconcileEnterpriseCarbonAdmissionWindow(previous, next *Auth) {
	if next == nil || next.Provider != ge.Provider || IsEnterpriseAccountAuth(next) {
		return
	}
	for _, model := range ge.AdmissionModels() {
		window := uint64(1)
		if previous != nil && previous.Provider == ge.Provider {
			window = max(enterpriseAdmissionWindow(previous, model), window)
			oldEnabled, oldExplicit := previous.Metadata[model.EnabledKey].(bool)
			newEnabled, newExplicit := next.Metadata[model.EnabledKey].(bool)
			if oldEnabled != newEnabled || oldExplicit != newExplicit || previous.Disabled != next.Disabled || (previous.Status == StatusDisabled) != (next.Status == StatusDisabled) {
				window++
			}
		}
		if model.ID == ge.HydrogenModel {
			next.enterpriseHydrogenWindow = window
		} else {
			next.enterpriseCarbonWindow = window
		}
	}
}

// WithEnterpriseAdmissionTest is created only by the authenticated management
// test handler. It does not register a public model or bypass paid/user policy.
func WithEnterpriseAdmissionTest(ctx context.Context, authID string, expected ge.ResourceBinding, model ge.AdmissionModel) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = WithEnterpriseModelProbe(ctx, authID, expected, model.ID)
	return context.WithValue(ctx, enterpriseCarbonModelTestKey{}, &enterpriseCarbonModelTest{authID: authID, expected: expected, model: model})
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
		(model == "" || canonicalModelKey(model) == scope.model.ID) &&
		ge.Resources(ge.FromMetadata(auth.Metadata)) == scope.expected
}

// EnterpriseAdmissionTestEligibility evaluates only conditions a management probe
// cannot bypass. It is safe under Manager.mu and performs no network I/O.
func (m *Manager) EnterpriseAdmissionTestEligibility(auth *Auth, model ge.AdmissionModel) error {
	if err := m.enterpriseAdmissionEligibility(auth, m.EnterpriseSubscriptionSnapshot(auth), model); err != nil {
		return err
	}
	if ge.CarbonAuthorizationFingerprint(auth.Metadata) == "" {
		return carbonPolicyError("authorization_unverified", strings.ReplaceAll("A stable OAuth refresh authorization is required to record a Carbon model test", "Carbon", model.Label), http.StatusServiceUnavailable)
	}
	return nil
}

func (m *Manager) enterpriseAdmissionEligibility(auth *Auth, subscription ge.SubscriptionObservation, model ge.AdmissionModel) error {
	if auth == nil || auth.Provider != ge.Provider || IsEnterpriseAccountAuth(auth) || enterpriseRetired(auth) {
		return carbonPolicyError("credential_not_found", strings.ReplaceAll("Select a Gemini Enterprise application before testing Carbon", "Carbon", model.Label), http.StatusServiceUnavailable)
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return carbonPolicyError("credential_disabled", strings.ReplaceAll("Enable this credential and application before using Carbon", "Carbon", model.Label), http.StatusServiceUnavailable)
	}
	if ge.FromMetadata(auth.Metadata).Validate() != nil {
		return carbonPolicyError("invalid_application_binding", "The Enterprise application binding is not valid", http.StatusConflict)
	}
	if (subscription.Status != "ok" && subscription.Status != "stale") || subscription.FreeTrial == nil {
		return carbonPolicyError("subscription_unverified", strings.ReplaceAll("Carbon requires the current user's verified and assigned Enterprise subscription", "Carbon", model.Label), http.StatusServiceUnavailable)
	}
	policy := ge.ModelPolicyFor(auth.Metadata, subscription)
	if policy.Reason == "invalid_model_policy" {
		return carbonPolicyError("invalid_model_policy", "The saved Enterprise model policy is not valid", http.StatusBadRequest)
	}
	if policy.EffectiveMode == "custom" && policy.AllowedModels != nil && !slices.Contains(policy.AllowedModels, model.ID) {
		return carbonPolicyError("model_not_allowed", strings.ReplaceAll("The application's custom whitelist does not allow Carbon", "Carbon", model.Label), http.StatusBadRequest)
	}
	// Hydrogen uses independent admission; Carbon follows the shared group policy.
	if model.ID == ge.CarbonModel && !policy.Allows(model.ID) {
		return policyError(policy, model.ID)
	}
	for _, pattern := range policy.ExcludedModels {
		if ge.MatchModelPattern(pattern, model.ID) {
			return carbonPolicyError("model_excluded", strings.ReplaceAll("Carbon is explicitly excluded for this application", "Carbon", model.Label), http.StatusBadRequest)
		}
	}
	if raw, ok := auth.Attributes["excluded_models"]; ok {
		for _, pattern := range strings.Split(raw, ",") {
			if ge.MatchModelPattern(pattern, model.ID) {
				return carbonPolicyError("model_excluded", strings.ReplaceAll("Carbon is explicitly excluded for this credential", "Carbon", model.Label), http.StatusBadRequest)
			}
		}
	}
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		for _, pattern := range cfg.OAuthExcludedModels[ge.Provider] {
			if ge.MatchModelPattern(pattern, model.ID) {
				return carbonPolicyError("model_excluded", strings.ReplaceAll("Carbon is explicitly excluded for Gemini Enterprise", "Carbon", model.Label), http.StatusBadRequest)
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
func (m *Manager) enterpriseAdmissionRoute(model string, target string) bool {
	if m == nil {
		return false
	}
	base := canonicalModelKey(strings.TrimPrefix(strings.TrimSpace(model), "models/"))
	lookup := func(name string) (bool, bool) {
		if table, ok := m.oauthModelAlias.Load().(*oauthModelAliasTable); ok && table != nil {
			if entry, found := table.reverse[ge.Provider][strings.ToLower(name)]; found {
				return canonicalModelKey(entry.upstreamModel) == target, true
			}
		}
		return name == target, false
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
		if auth.Provider == ge.Provider && !IsEnterpriseAccountAuth(auth) && auth.Prefix != "" && strings.HasPrefix(base, auth.Prefix+"/") && canonicalModelKey(m.selectionModelForAuth(auth, base)) == target {
			return true
		}
	}
	return false
}

func (m *Manager) routeEnterpriseHydrogen(ctx context.Context, providers []string, model string) ([]string, error) {
	if m.HomeEnabled() || !m.EnterpriseHydrogenRoute(model) {
		return providers, nil
	}
	if EnterpriseCarbonModelTestActive(ctx) {
		return []string{ge.Provider}, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth != nil && auth.Provider == ge.Provider && !IsEnterpriseAccountAuth(auth) && canonicalModelKey(m.selectionModelForAuth(auth, model)) == ge.HydrogenModel && m.EnterpriseModelAdmissionView(auth, ge.HydrogenAdmissionModel).Available {
			return []string{ge.Provider}, nil
		}
	}
	return nil, EnterpriseHydrogenUnavailableError()
}

// EnterpriseModelAdmissionView is read-only and never acquires Manager.mu. It
// can be evaluated from the existing selection and account transaction locks.
func (m *Manager) EnterpriseModelAdmissionView(auth *Auth, model ge.AdmissionModel) ge.CarbonAdmission {
	if m == nil {
		return ge.CarbonAdmission{Reason: "credential_not_found"}
	}
	return m.enterpriseAdmissionView(auth, m.EnterpriseSubscriptionSnapshot(auth), model)
}

func (m *Manager) enterpriseAdmissionView(auth *Auth, subscription ge.SubscriptionObservation, model ge.AdmissionModel) ge.CarbonAdmission {
	out := ge.CarbonAdmission{}
	if auth == nil {
		out.Reason = "credential_not_found"
		return out
	}
	enabled, err := model.ReadEnabled(auth.Metadata)
	if err != nil {
		out.Reason = "invalid_" + strings.ToLower(model.Label) + "_settings"
		return out
	}
	if _, configured := auth.Metadata[model.EnabledKey]; !configured && model.ID == ge.CarbonModel {
		enabled = (subscription.Status == "ok" || subscription.Status == "stale") && subscription.FreeTrial != nil && !*subscription.FreeTrial
	}
	out.Enabled = enabled
	// Historical diagnostics never determine business admission. In particular,
	// changing payload rules or a failed test must not revoke a manual opening.
	if settings, errRead := model.ReadSettings(auth.Metadata); errRead == nil && settings.Evidence != nil {
		out.Test = settings.Evidence.Summary()
	}
	if err = m.enterpriseAdmissionEligibility(auth, subscription, model); err != nil {
		var policy *EnterpriseModelPolicyError
		if errors.As(err, &policy) {
			out.Reason = policy.Code
		}
		return out
	}
	out.CanTest = ge.CarbonAuthorizationFingerprint(auth.Metadata) != ""
	out.CanEnable = true
	if !enabled {
		out.Reason = strings.ToLower(model.Label) + "_disabled"
		return out
	}
	out.Available = true
	return out
}

func (m *Manager) validateEnterpriseAdmissionPolicy(ctx context.Context, auth *Auth, model ge.AdmissionModel) error {
	if enterpriseCarbonTestMatches(ctx, auth, model.ID) {
		return m.EnterpriseAdmissionTestEligibility(auth, model)
	}
	view := m.EnterpriseModelAdmissionView(auth, model)
	if view.Available {
		return nil
	}
	if err := m.EnterpriseAdmissionTestEligibility(auth, model); err != nil {
		return err
	}
	return carbonPolicyError(strings.ToLower(model.Label)+"_unavailable", strings.ReplaceAll("Carbon is unavailable for this application: ", "Carbon", model.Label)+view.Reason, http.StatusServiceUnavailable)
}

// CommitEnterpriseAdmissionSubmission is the linearization point immediately before
// one upstream send. Closing or rebinding an application takes Manager.mu for a
// write and therefore either precedes this decision or follows an already
// committed send. No manager lock is held across network I/O, and existing quota
// and concurrency leases retain their normal lifetime.
func CommitEnterpriseAdmissionSubmission(ctx context.Context, auth *Auth, requested string, model ge.AdmissionModel) error {
	if canonicalModelKey(requested) != model.ID {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if auth == nil || auth.enterpriseQuota == nil || auth.enterpriseQuota.manager == nil {
		return carbonPolicyError("subscription_unverified", strings.ReplaceAll("Carbon requires a current managed Enterprise application", "Carbon", model.Label), http.StatusServiceUnavailable)
	}
	m := auth.enterpriseQuota.manager
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[auth.ID]
	if current == nil || current.Provider != ge.Provider || current.Disabled || current.Status == StatusDisabled || current.RegistrationEpoch != auth.RegistrationEpoch || enterpriseAdmissionWindow(current, model) != enterpriseAdmissionWindow(auth, model) ||
		ge.CarbonAuthorizationFingerprint(current.Metadata) != ge.CarbonAuthorizationFingerprint(auth.Metadata) ||
		ge.Resources(ge.FromMetadata(current.Metadata)) != ge.Resources(ge.FromMetadata(auth.Metadata)) {
		return carbonPolicyError("credential_changed", strings.ReplaceAll("The Carbon application changed before generation submission", "Carbon", model.Label), http.StatusConflict)
	}
	if err := m.validateEnterpriseAdmissionPolicy(ctx, current, model); err != nil {
		return err
	}
	if scope := enterpriseCarbonTestScope(ctx); scope != nil && !enterpriseCarbonTestMatches(ctx, current, model.ID) {
		return carbonPolicyError("credential_changed", strings.ReplaceAll("The Carbon test application changed before generation submission", "Carbon", model.Label), http.StatusConflict)
	}
	return nil
}

func enterpriseAdmissionWindow(auth *Auth, model ge.AdmissionModel) uint64 {
	if model.ID == ge.HydrogenModel {
		return auth.enterpriseHydrogenWindow
	}
	return auth.enterpriseCarbonWindow
}

func WithEnterpriseCarbonModelTest(ctx context.Context, authID string, expected ge.ResourceBinding) context.Context {
	return WithEnterpriseAdmissionTest(ctx, authID, expected, ge.CarbonAdmissionModel)
}
func (m *Manager) EnterpriseCarbonTestEligibility(auth *Auth) error {
	return m.EnterpriseAdmissionTestEligibility(auth, ge.CarbonAdmissionModel)
}
func (m *Manager) enterpriseCarbonEligibility(auth *Auth, subscription ge.SubscriptionObservation) error {
	return m.enterpriseAdmissionEligibility(auth, subscription, ge.CarbonAdmissionModel)
}
func (m *Manager) EnterpriseCarbonRoute(model string) bool {
	return m.enterpriseAdmissionRoute(model, ge.CarbonModel)
}
func (m *Manager) EnterpriseHydrogenRoute(model string) bool {
	return m.enterpriseAdmissionRoute(model, ge.HydrogenModel)
}
func EnterpriseHydrogenUnavailableError() error {
	return carbonPolicyError("hydrogen_unavailable", "No enabled and eligible Enterprise application is available for gemini-4-hydrogen", http.StatusServiceUnavailable)
}
func (m *Manager) EnterpriseCarbonAdmissionView(auth *Auth) ge.CarbonAdmission {
	return m.EnterpriseModelAdmissionView(auth, ge.CarbonAdmissionModel)
}
func (m *Manager) enterpriseCarbonAdmissionView(auth *Auth, subscription ge.SubscriptionObservation) ge.CarbonAdmission {
	return m.enterpriseAdmissionView(auth, subscription, ge.CarbonAdmissionModel)
}
func (m *Manager) validateEnterpriseCarbonPolicy(ctx context.Context, auth *Auth) error {
	return m.validateEnterpriseAdmissionPolicy(ctx, auth, ge.CarbonAdmissionModel)
}
func CommitEnterpriseCarbonSubmission(ctx context.Context, auth *Auth, model string) error {
	return CommitEnterpriseAdmissionSubmission(ctx, auth, model, ge.CarbonAdmissionModel)
}

func WithEnterpriseHydrogenModelTest(ctx context.Context, authID string, expected ge.ResourceBinding) context.Context {
	return WithEnterpriseAdmissionTest(ctx, authID, expected, ge.HydrogenAdmissionModel)
}
func (m *Manager) EnterpriseHydrogenAdmissionView(auth *Auth) ge.CarbonAdmission {
	return m.EnterpriseModelAdmissionView(auth, ge.HydrogenAdmissionModel)
}
func (m *Manager) EnterpriseHydrogenTestEligibility(auth *Auth) error {
	return m.EnterpriseAdmissionTestEligibility(auth, ge.HydrogenAdmissionModel)
}
func CommitEnterpriseHydrogenSubmission(ctx context.Context, auth *Auth, model string) error {
	return CommitEnterpriseAdmissionSubmission(ctx, auth, model, ge.HydrogenAdmissionModel)
}
