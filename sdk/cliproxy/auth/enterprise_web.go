package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func (m *Manager) WebRuntime() *web.Runtime { return m.webRuntime }

// ConfigureEnterpriseWeb applies hot settings and wakes the credential scheduler.
func (m *Manager) ConfigureEnterpriseWeb(cfg internalconfig.GeminiEnterpriseWebConfig) {
	if m.webRuntime.Configure(cfg) {
		m.rescheduleEnterpriseWebChecks()
	}
}

func (m *Manager) rescheduleEnterpriseWebChecks() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.refreshLoop == nil {
		return
	}
	for id, a := range m.auths {
		if a.Provider == web.Provider {
			m.refreshLoop.queueReschedule(id)
		}
	}
}

func (m *Manager) nextEnterpriseWebCookieCheck(now time.Time, a *Auth) (time.Time, bool) {
	if a.Disabled || a.Status == StatusDisabled || !m.webRuntime.Enabled() {
		return time.Time{}, false
	}
	credential, err := web.CredentialFrom(a.Metadata)
	if err != nil {
		return time.Time{}, false
	}
	next, ok := m.webRuntime.NextCookieCheck(a.ID, credential, now)
	if ok && a.NextRefreshAfter.After(next) {
		next = a.NextRefreshAfter
	}
	return next, ok
}

func (m *Manager) checkEnterpriseWebCookie(ctx context.Context, job *authRefreshJob) bool {
	m.mu.RLock()
	a := m.auths[job.id].Clone()
	executor, _ := m.executorLocked(web.Provider)
	m.mu.RUnlock()
	if a == nil || a.Provider != web.Provider || a.Disabled || a.Status == StatusDisabled || a.RegistrationEpoch != job.registrationEpoch {
		return false
	}
	credential, err := web.CredentialFrom(a.Metadata)
	if err != nil {
		return false
	}
	now := time.Now()
	if next, ok := m.webRuntime.NextCookieCheck(a.ID, credential, now); !ok || next.After(now) {
		return true
	}
	checker, ok := executor.(interface {
		CheckCookie(context.Context, *Auth) bool
	})
	return ok && checker.CheckCookie(exec.WithoutRequestProxyURL(ctx), a)
}

// Invalidate at publication, so watcher and management edits share the same barrier.
func (m *Manager) invalidateEnterpriseWebCredentialLocked(previous, next *Auth, verifiedRotation, preserveEvidence bool) {
	if previous == nil || next == nil || m.webRuntime == nil || (previous.Provider != web.Provider && next.Provider != web.Provider) {
		return
	}
	old, oldErr := web.CredentialFrom(previous.Metadata)
	updated, newErr := web.CredentialFrom(next.Metadata)
	if previous.Disabled != next.Disabled || (previous.Status == StatusDisabled) != (next.Status == StatusDisabled) {
		m.webRuntime.ForgetCredential(next.ID)
	} else if previous.Provider != next.Provider || previous.ProxyURL != next.ProxyURL || oldErr != nil || newErr != nil || old.Fingerprint() != updated.Fingerprint() {
		if verifiedRotation && oldErr == nil && newErr == nil && previous.Provider == next.Provider && previous.ProxyURL == next.ProxyURL && old.Policy() == updated.Policy() && old.CookieKey() == updated.CookieKey() {
			m.webRuntime.ReplaceVerifiedCookies(next.ID, old, updated, previous.CredentialVersion, next.CredentialVersion, preserveEvidence)
		} else {
			m.webRuntime.ForgetCredential(next.ID)
		}
	} else if old.Policy() != updated.Policy() {
		m.webRuntime.ForgetModelEvidence(next.ID)
	}
}

// resolveEnterpriseWebCookie runs after the Cookie gate is acquired. Only a
// committed rotation of the same registration can update a queued selection.
func (m *Manager) resolveEnterpriseWebCookie(selection web.CookieSelection, selected web.Credential) (web.Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	changed := func() (web.Credential, error) {
		return web.Credential{}, web.InputFailure(409, "credential_changed", "", "Web credential changed before generation; reload and retry")
	}
	a := m.auths[selection.ID]
	if a == nil || a.Provider != web.Provider || (a.Disabled || a.Status == StatusDisabled) != selection.Disabled || a.RegistrationEpoch != selection.RegistrationEpoch {
		return changed()
	}
	current, err := web.CredentialFrom(a.Metadata)
	if err != nil || current.Policy() != selected.Policy() {
		return changed()
	}
	if a.CredentialVersion == selection.CredentialVersion && current.Fingerprint() == selected.Fingerprint() {
		return current, nil
	}
	if current.CookieKey() != selected.CookieKey() || !m.webRuntime.CanRebaseVerifiedCookies(a.ID, selection.CredentialVersion, a.CredentialVersion) {
		return changed()
	}
	return current, nil
}

func (m *Manager) reconcileEnterpriseWebLoadLocked(previous map[string]*Auth) {
	for id, old := range previous {
		if old == nil || old.Provider != web.Provider {
			continue
		}
		if next := m.auths[id]; next != nil {
			m.invalidateEnterpriseWebCredentialLocked(old, next, false, false)
		} else {
			m.webRuntime.ForgetCredential(id)
		}
	}
	m.rebuildEnterpriseWebModelsLocked()
}

// EnterpriseWebModel preserves existing model routing outside the Enterprise pair.
func (m *Manager) EnterpriseWebModel(model string) bool {
	names, _ := m.webModels.Load().(map[string]bool)
	return names[thinking.ParseSuffix(model).ModelName]
}
func (m *Manager) routeEnterpriseWeb(ctx context.Context, providers []string, req *exec.Request, opts *exec.Options, count bool) (context.Context, []string, func(), error) {
	done := func() {}
	if opts.SourceFormat.String() != "gemini" {
		return ctx, excludeWebProvider(providers), done, nil
	}
	if fallback, projected := web.MediaFallbackFrom(ctx); projected {
		if !m.enterpriseMediaFallbackAllowed(ctx, providers, *req, *opts, count) {
			if fallback.Reason == web.FallbackWebDisabled {
				return ctx, nil, done, web.Fail(503, "Media input channel is disabled")
			}
			if err := m.checkEnterpriseWebRoutePolicy(authSelectionModelFromOptions(*opts, req.Model), *opts); err != nil {
				return ctx, nil, done, err
			}
			return ctx, nil, done, web.Fail(503, "No enabled Web credential is available for this model")
		}
		ctx, providers, err := m.startEnterpriseMediaFallback(ctx, providers, req, opts, "")
		return ctx, providers, done, err
	}
	media := web.HasMediaInContext(ctx, req.Payload)
	if !media {
		return ctx, excludeWebProvider(providers), done, nil
	}
	eligible := containsEnterpriseProvider(providers) || m.EnterpriseWebModel(req.Model)
	for _, p := range providers {
		eligible = eligible || p == web.Provider
	}
	if !eligible {
		return ctx, providers, done, nil
	}
	if opts.Alt == "countTokens" || exec.Operation(*opts) == "count_tokens" {
		return ctx, nil, done, web.Fail(501, "Media token counting is unavailable")
	}
	if m.HomeEnabled() {
		return ctx, nil, done, web.Fail(400, "Media input is unavailable in this execution mode")
	}
	selectionModel := authSelectionModelFromOptions(*opts, req.Model)
	allowFallback := m.enterpriseMediaFallbackAllowed(ctx, providers, *req, *opts, count)
	if allowFallback && !m.enterpriseWebHasEligibleCredential(ctx, selectionModel, *opts, nil) {
		reason := web.FallbackNoCredential
		if web.ExistingLease(ctx) == nil && !m.WebRuntime().Enabled() {
			reason = web.FallbackWebDisabled
		}
		ctx, providers, err := m.startEnterpriseMediaFallback(ctx, providers, req, opts, reason)
		return ctx, providers, done, err
	}
	if web.ExistingLease(ctx) == nil {
		var lease *web.Lease
		var e error
		ctx, lease, e = m.WebRuntime().Acquire(ctx, int64(len(req.Payload)), allowFallback)
		if e != nil {
			if allowFallback && web.IsChannelDisabled(e) {
				ctx, providers, err := m.startEnterpriseMediaFallback(ctx, providers, req, opts, web.FallbackWebDisabled)
				return ctx, providers, done, err
			}
			return ctx, nil, done, e
		}
		done = lease.Release
	}
	if errHistory := web.ValidateHistoryMode(ctx, req.Payload); errHistory != nil {
		m.WebRuntime().RecordPreflight(ctx, errHistory)
		done()
		return ctx, nil, func() {}, errHistory
	}
	if errPolicy := m.checkEnterpriseWebRoutePolicy(selectionModel, *opts); errPolicy != nil {
		if allowFallback && !m.enterpriseWebHasEligibleCredential(ctx, selectionModel, *opts, nil) {
			nextCtx, nextProviders, errFallback := m.startEnterpriseMediaFallback(ctx, providers, req, opts, web.FallbackNoCredential)
			if errFallback == nil {
				return nextCtx, nextProviders, done, nil
			}
			m.WebRuntime().RecordPreflight(ctx, errFallback)
			done()
			return nextCtx, nil, func() {}, errFallback
		}
		m.WebRuntime().RecordPreflight(ctx, errPolicy)
		done()
		return ctx, nil, func() {}, errPolicy
	}
	ctx = web.WithPolicyValidator(ctx, m.validateEnterpriseWebDispatchPolicy)
	return ctx, []string{web.Provider}, done, nil
}

func containsEnterpriseWebProvider(providers []string) bool {
	for _, provider := range providers {
		if provider == web.Provider {
			return true
		}
	}
	return false
}

func webPreflightLimit(providers []string, limit int) int {
	if containsEnterpriseWebProvider(providers) && (limit <= 0 || limit > 2) {
		return 2
	}
	return limit
}

func (m *Manager) nextWebPreflightError(account *Auth, opts exec.Options, tried map[string]struct{}, previous, err error) (next error, retry bool, handled bool) {
	var failure *web.PreflightFailure
	if account.Provider != web.Provider || !errors.As(err, &failure) {
		return nil, false, false
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return err, false, true
	}
	var rejected webRejectedCookies
	// Imported duplicates of the failed session cannot serve as its backup.
	if c, credErr := web.CredentialFrom(account.Metadata); credErr == nil {
		if web.IsCookieLoginRequired(err) {
			var earlier *webCookieRejection
			if errors.As(previous, &earlier) {
				rejected = earlier.rejected
			} else {
				rejected = make(webRejectedCookies)
			}
			rejected[sha256.Sum256([]byte(c.Cookies))] = struct{}{}
		}
		key := c.CookieKey()
		m.mu.RLock()
		for id, candidate := range m.auths {
			if id == account.ID || candidate.Provider != web.Provider {
				continue
			}
			if c, err := web.CredentialFrom(candidate.Metadata); err == nil && c.CookieKey() == key {
				tried[id] = struct{}{}
			}
		}
		m.mu.RUnlock()
	}
	if !failure.SearchModeMismatch || statusCodeFromError(previous) == 503 {
		next := web.Fail(503, "No available Web credential could authenticate this model request")
		if rejected != nil {
			next = &webCookieRejection{error: next, rejected: rejected}
		}
		return next, true, true
	}
	return err, true, true
}

func (m *Manager) enterpriseWebAuthAllowsModel(a *Auth, model string) bool {
	c, err := web.CredentialFrom(a.Metadata)
	var excluded []string
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		excluded = cfg.OAuthExcludedModels[web.Provider]
	}
	return err == nil && c.AllowsModelWithExclusions(m.selectionModelForAuth(a, model), excluded) &&
		!m.webRuntime.CookieReloginRequired(a.ID, c)
}

func (m *Manager) checkEnterpriseWebRoutePolicy(model string, opts exec.Options) error {
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	found, unclassified, rejected := false, false, false
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.auths {
		if a.Provider != web.Provider || a.Disabled || a.Status == StatusDisabled || (pinned != "" && a.ID != pinned) {
			continue
		}
		c, err := web.CredentialFrom(a.Metadata)
		if err != nil {
			continue
		}
		found = true
		actual := m.selectionModelForAuth(a, model)
		if err = c.CheckModelPolicy(actual); err == nil {
			if m.webRuntime.CookieReloginRequired(a.ID, c) {
				if pinned != "" {
					return web.Fail(401, "The selected Web credential needs a new Google login")
				}
				rejected = true
				continue
			}
			return nil
		} else if pinned != "" {
			return err
		}
		unclassified = unclassified || c.Policy() == web.PolicyUnclassified
	}
	if rejected {
		return web.Fail(503, "No available Web credential for this model; update the rejected Google session")
	}
	if found && unclassified {
		return web.Fail(503, "No eligible Web credential; manually classify pending credentials before generation")
	}
	if found {
		return web.Fail(503, "No classified Web credential permits this upstream model")
	}
	return web.Fail(503, "No enabled Web credential is available for this model")
}

// Dispatch rechecks policy after uploads; submitted generations retain their response lease.
func (m *Manager) validateEnterpriseWebDispatchPolicy(id string, selected web.Credential, model string) error {
	a, ok := m.GetByID(id)
	if !ok || a.Provider != web.Provider || a.Disabled || a.Status == StatusDisabled {
		return web.Fail(409, "Web credential changed before generation; reload and retry")
	}
	c, err := web.CredentialFrom(a.Metadata)
	if err != nil || c.Fingerprint() != selected.Fingerprint() {
		return web.InputFailure(409, "credential_changed", "", "Web credential changed before generation; reload and retry")
	}
	var excluded []string
	if cfg := m.runtimeConfigSnapshot(); cfg != nil {
		excluded = cfg.OAuthExcludedModels[web.Provider]
	}
	if err != nil || !c.AllowsModelWithExclusions(model, excluded) {
		return web.Fail(409, "Web credential model policy changed before generation; reload and retry")
	}
	return nil
}
func excludeWebProvider(in []string) []string {
	for _, p := range in {
		if strings.EqualFold(p, web.Provider) {
			out := make([]string, 0, len(in))
			for _, v := range in {
				if !strings.EqualFold(v, web.Provider) {
					out = append(out, v)
				}
			}
			return out
		}
	}
	return in
}

func (m *Manager) rebuildEnterpriseWebModelsLocked() {
	names := map[string]bool{}
	table, _ := m.oauthModelAlias.Load().(*oauthModelAliasTable)
	for _, a := range m.auths {
		if a.Provider != web.Provider {
			continue
		}
		c, err := web.CredentialFrom(a.Metadata)
		if err != nil {
			continue
		}
		add := func(name string) {
			names[name] = true
			if a.Prefix != "" {
				names[a.Prefix+"/"+name] = true
			}
		}
		matches := func(name string) bool {
			for _, mapping := range c.Catalog() {
				if mapping.ID == name {
					return true
				}
			}
			return false
		}
		for _, mapping := range c.Catalog() {
			add(mapping.ID)
		}
		if table != nil {
			for _, alias := range table.reverse[web.Provider] {
				if matches(alias.upstreamModel) {
					add(alias.configAlias)
				}
			}
		}
	}
	m.webModels.Store(names)
}
