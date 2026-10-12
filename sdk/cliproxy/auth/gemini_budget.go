package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budgetbackup"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

const GeminiOfficialOrigin = "https://generativelanguage.googleapis.com"

// CanonicalBudgetModel strips both native thinking suffixes and common colon spellings.
func CanonicalBudgetModel(model string) string {
	model = strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(model)).ModelName)
	if i := strings.LastIndexByte(model, ':'); i >= 0 {
		switch strings.ToLower(model[i+1:]) {
		case "thinking", "low", "medium", "high", "minimal", "none", "auto", "off":
			model = model[:i]
		}
	}
	return model
}

// NormalizeGeminiBudgetThinkingModel accepts colon spellings at the request boundary.
func NormalizeGeminiBudgetThinkingModel(model string) string {
	if i := strings.LastIndexByte(model, ':'); i >= 0 {
		suffix := strings.ToLower(model[i+1:])
		switch suffix {
		case "thinking":
			return model[:i] + "(auto)"
		case "off":
			return model[:i] + "(none)"
		case "low", "medium", "high", "minimal", "none", "auto":
			return model[:i] + "(" + suffix + ")"
		}
	}
	return model
}

// ResolveCanonicalBudgetModel uses the same configured alias source as execution.
func ResolveCanonicalBudgetModel(model string, auth *Auth, configs ...*config.Config) string {
	base := CanonicalBudgetModel(model)
	if auth != nil && auth.Prefix != "" {
		base = strings.TrimPrefix(base, auth.Prefix+"/")
	}
	if len(configs) > 0 && configs[0] != nil {
		resolved := resolveAPIKeyModelAliasWithResult(configs[0], auth, base)
		if resolved.UpstreamModel != "" {
			base = resolved.UpstreamModel
		}
	}
	return CanonicalBudgetModel(base)
}

func IsOfficialGeminiAuth(auth *Auth) bool {
	if auth == nil || (auth.Provider != "gemini" && auth.Provider != "gemini-interactions") || strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return false
	}
	base := strings.TrimSpace(auth.Attributes["base_url"])
	if base == "" {
		return true
	}
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, "generativelanguage.googleapis.com") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

// GeminiBudgetProject hashes configured project identifiers too, keeping them out of client errors.
func GeminiBudgetProject(auth *Auth) string {
	if auth == nil {
		return ""
	}
	identity := strings.TrimSpace(auth.Attributes["quota_project"])
	if identity == "" {
		identity = "key:" + strings.TrimSpace(auth.Attributes["api_key"])
	} else {
		identity = "project:" + identity
	}
	sum := sha256.Sum256([]byte(GeminiOfficialOrigin + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

func GeminiBudgetScope(auth *Auth, realModel string) budget.Scope {
	index := ""
	if auth != nil {
		index = auth.Index
	}
	credential := ""
	if auth != nil {
		sum := sha256.Sum256([]byte(GeminiOfficialOrigin + "\x00credential:" + strings.TrimSpace(auth.Attributes["api_key"])))
		credential = hex.EncodeToString(sum[:])
	}
	return budget.Scope{Project: GeminiBudgetProject(auth), Model: CanonicalBudgetModel(realModel), AuthIndex: index, CredentialID: credential, Origin: GeminiOfficialOrigin}
}

func (m *Manager) GeminiBudgetPolicy() config.GeminiFreeTierPolicyConfig {
	if m == nil {
		return config.GeminiFreeTierPolicyConfig{}.WithDefaults()
	}
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg == nil {
		return config.GeminiFreeTierPolicyConfig{}.WithDefaults()
	}
	return cfg.GeminiFreeTierPolicy.WithDefaults()
}

func (m *Manager) GeminiBudgetEngine() *budget.Engine {
	if m == nil {
		return nil
	}
	m.geminiBudgetMu.Lock()
	defer m.geminiBudgetMu.Unlock()
	if m.geminiBudgetOpenError != nil {
		return nil
	}
	return m.geminiBudget
}

// EnsureGeminiBudget is called at startup or explicit initialization, never on each request.
func (m *Manager) EnsureGeminiBudget(ctx context.Context, initialize bool) (*budget.Engine, error) {
	if m == nil {
		return nil, errors.New("Gemini budget manager unavailable")
	}
	p := m.GeminiBudgetPolicy()
	m.geminiBudgetMu.Lock()
	defer m.geminiBudgetMu.Unlock()
	if m.geminiBudget != nil {
		if filepath.Clean(m.geminiBudgetPath) != filepath.Clean(p.StoragePath) {
			m.geminiBudgetOpenError = errors.New("Gemini budget storage path changes require restart")
			return nil, m.geminiBudgetOpenError
		}
		models := make(map[string]budget.ModelPolicy, len(p.ManagedModels))
		for model, limit := range p.ManagedModels {
			models[model] = budget.ModelPolicy{DailyLimit: limit}
		}
		m.geminiBudgetOpenError = m.geminiBudget.UpdateModels(models)
		if m.geminiBudgetOpenError != nil {
			return nil, m.geminiBudgetOpenError
		}
		return m.geminiBudget, nil
	}
	if !p.Enabled && !initialize {
		if _, err := os.Stat(filepath.Join(p.StoragePath, "init.meta")); os.IsNotExist(err) {
			return nil, nil
		}
	}
	models := make(map[string]budget.ModelPolicy, len(p.ManagedModels))
	for model, limit := range p.ManagedModels {
		models[model] = budget.ModelPolicy{DailyLimit: limit}
	}
	engine, err := budget.Open(ctx, budget.Options{Path: p.StoragePath, Models: models, LockTimeout: 30 * time.Second, ExpectedInitialized: p.Enabled})
	m.geminiBudgetOpenError = err
	if err != nil {
		return nil, errors.New("Gemini budget ledger unavailable")
	}
	m.geminiBudget = engine
	m.geminiBudgetPath = p.StoragePath
	return engine, nil
}

func (m *Manager) CloseGeminiBudget() error {
	if m == nil {
		return nil
	}
	m.geminiBudgetMu.Lock()
	defer m.geminiBudgetMu.Unlock()
	if m.geminiBudget == nil {
		return nil
	}
	err := m.geminiBudget.Close()
	m.geminiBudget = nil
	return err
}

func (m *Manager) geminiBudgetEnabled() bool { return m != nil && m.GeminiBudgetPolicy().Enabled }

func (m *Manager) GeminiBudgetModelManaged(model string) bool {
	if !m.geminiBudgetEnabled() {
		return false
	}
	model = CanonicalBudgetModel(model)
	policy := m.GeminiBudgetPolicy()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if IsOfficialGeminiAuth(auth) {
			for _, candidate := range m.executionModelCandidates(auth, model) {
				if _, ok := policy.ManagedModels[CanonicalBudgetModel(candidate)]; ok {
					return true
				}
			}
		}
	}
	return false
}

func (m *Manager) geminiBudgetAllows(ctx context.Context, auth *Auth, model string) bool {
	if !m.geminiBudgetEnabled() || !IsOfficialGeminiAuth(auth) {
		return true
	}
	candidates := m.executionModelCandidates(auth, model)
	policy := m.GeminiBudgetPolicy()
	engine := m.GeminiBudgetEngine()
	for _, candidate := range candidates {
		real := CanonicalBudgetModel(candidate)
		if _, managed := policy.ManagedModels[real]; !managed {
			return true
		}
		scope := GeminiBudgetScope(auth, real)
		if !budget.HasAttemptedProject(ctx, scope) && engine != nil && engine.Available(scope) {
			return true
		}
	}
	return false
}

// geminiBudgetSelectionAuth projects ledger-owned routes without mutating the
// credential or its persisted legacy cooldowns. Other channels retain their
// existing cooldown semantics, and explicit administrative restrictions survive.
func (m *Manager) geminiBudgetSelectionAuth(auth *Auth, routeModel string) *Auth {
	if !m.geminiBudgetEnabled() || !IsOfficialGeminiAuth(auth) {
		return auth
	}
	policy := m.GeminiBudgetPolicy()
	candidates := m.executionModelCandidates(auth, routeModel)
	if len(candidates) == 0 {
		return auth
	}
	stateModels := []string{m.selectionModelForAuth(auth, routeModel)}
	for _, model := range candidates {
		if _, managed := policy.ManagedModels[CanonicalBudgetModel(model)]; !managed {
			// Mixed pools still need their unmanaged models' legacy admission.
			return auth
		}
		stateModels = append(stateModels, model)
	}
	projected := m.geminiBudgetExecutionAuth(auth, candidates[0], stateModels...)
	allDisabled := true
	for _, model := range candidates {
		state := existingModelState(projected, canonicalModelKey(model))
		if state == nil || state.Status != StatusDisabled {
			allDisabled = false
			break
		}
	}
	if allDisabled {
		// An alias must not bypass administrative disables on all actual models.
		projected.Status = StatusDisabled
	}
	return projected
}

// geminiBudgetExecutionAuth removes only transient generic state in the
// request-local projection. Terminal invalid credentials remain blocked, but
// historical Google error bodies must never become client error causes.
func (m *Manager) geminiBudgetExecutionAuth(auth *Auth, realModel string, stateModels ...string) *Auth {
	if !m.geminiBudgetEnabled() || !IsOfficialGeminiAuth(auth) {
		return auth
	}
	if _, managed := m.GeminiBudgetPolicy().ManagedModels[CanonicalBudgetModel(realModel)]; !managed {
		return auth
	}
	projected := auth.Clone()
	terminal := hasUnauthorizedAuthFailure(auth)
	if projected.LastError != nil {
		projected.LastError = geminiBudgetLegacyError(projected.LastError)
	}
	projected.StatusMessage = ""
	if !projected.Disabled && projected.Status != StatusDisabled && !terminal {
		projected.Unavailable = false
		projected.NextRetryAfter = time.Time{}
		applyCooldownFields(&projected.Quota, QuotaState{})
		projected.Status = StatusActive
	}
	keys := make(map[string]bool, len(stateModels)+1)
	keys[canonicalModelKey(realModel)] = true
	for _, model := range stateModels {
		keys[canonicalModelKey(model)] = true
	}
	for key, state := range projected.ModelStates {
		if !keys[canonicalModelKey(key)] || state == nil {
			continue
		}
		if state.Status == StatusDisabled {
			state.StatusMessage = "The requested model is disabled by an administrator."
			if state.LastError != nil {
				state.LastError = geminiBudgetLegacyError(state.LastError)
			}
			continue
		}
		delete(projected.ModelStates, key)
	}
	return projected
}

func geminiBudgetLegacyError(prior *Error) *Error {
	status := prior.StatusCode()
	code := "gemini_budget_legacy_state"
	if status == http.StatusUnauthorized || strings.EqualFold(prior.Code, "unauthorized") {
		code = "unauthorized"
	}
	return &Error{Code: code, Message: "The upstream credential requires administrator attention.", HTTPStatus: status, Retryable: prior.Retryable}
}

type geminiBudgetAdmissionError struct {
	base  *Error
	delay time.Duration
}

func (e *geminiBudgetAdmissionError) Error() string   { return e.base.Error() }
func (e *geminiBudgetAdmissionError) Unwrap() error   { return e.base }
func (e *geminiBudgetAdmissionError) StatusCode() int { return e.base.StatusCode() }

func (e *geminiBudgetAdmissionError) RetryAfter() *time.Duration {
	if e.delay <= 0 {
		return nil
	}
	delay := e.delay
	return &delay
}

func (m *Manager) geminiBudgetUnavailableError(models ...string) error {
	engine := m.GeminiBudgetEngine()
	if engine == nil || engine.Snapshot().State != budget.StateReady {
		var delay time.Duration
		if engine != nil {
			delay = time.Until(engine.Snapshot().FreezeUntil)
		}
		return &geminiBudgetAdmissionError{base: &Error{Code: "gemini_budget_unavailable", Message: "Request quota state is temporarily unavailable.", HTTPStatus: http.StatusServiceUnavailable, Retryable: true}, delay: delay}
	}
	var delay time.Duration
	quotaBlocked := false
	if len(models) > 0 {
		m.mu.RLock()
		for _, a := range m.auths {
			if !IsOfficialGeminiAuth(a) || a.Disabled {
				continue
			}
			for _, model := range m.executionModelCandidates(a, models[0]) {
				view := engine.View(GeminiBudgetScope(a, model))
				if view.Available || view.State == "unmanaged" {
					continue
				}
				if view.State == "daily_exhausted" || view.State == "cooldown" {
					quotaBlocked = true
				}
				wait := time.Until(view.RetryAt)
				if view.State == "busy" {
					wait = time.Second
				}
				if wait > 0 && (delay <= 0 || wait < delay) {
					delay = wait
				}
			}
		}
		m.mu.RUnlock()
	}
	status := http.StatusTooManyRequests
	if len(models) > 0 && !quotaBlocked {
		status = http.StatusServiceUnavailable
	}
	return &geminiBudgetAdmissionError{base: &Error{Code: "gemini_budget_unavailable", Message: "Request quota is temporarily unavailable.", HTTPStatus: status, Retryable: true}, delay: delay}
}

func (m *Manager) geminiBudgetRequestLimitReached(ctx context.Context) bool {
	return budget.Attempts(ctx) >= budget.MaxRequestAttempts
}

func (m *Manager) geminiBudgetOwnsResult(result Result) bool {
	if !m.geminiBudgetEnabled() {
		return false
	}
	m.mu.RLock()
	auth := m.auths[result.AuthID]
	if auth != nil {
		auth = auth.Clone()
	}
	m.mu.RUnlock()
	if !IsOfficialGeminiAuth(auth) {
		return false
	}
	model := result.Model
	if model == "" {
		model = result.RouteModel
	}
	policy := m.GeminiBudgetPolicy()
	for _, actual := range m.executionModelCandidates(auth, model) {
		if _, managed := policy.ManagedModels[CanonicalBudgetModel(actual)]; managed {
			return true
		}
	}
	return false
}

func (m *Manager) SetGeminiBudgetBackup(service *budgetbackup.Service) {
	m.geminiBudgetMu.Lock()
	defer m.geminiBudgetMu.Unlock()
	m.geminiBudgetBackup = service
}
func (m *Manager) BackupGeminiBudget(ctx context.Context) error {
	m.geminiBudgetMu.Lock()
	service := m.geminiBudgetBackup
	m.geminiBudgetMu.Unlock()
	if service == nil {
		return nil
	}
	_, err := service.Backup(ctx)
	return err
}
