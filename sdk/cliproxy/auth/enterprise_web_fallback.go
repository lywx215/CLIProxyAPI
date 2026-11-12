package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

type webRejectedCookies map[[sha256.Size]byte]struct{}

type webCookieRejection struct {
	error
	rejected webRejectedCookies
}

func (e *webCookieRejection) Unwrap() error { return e.error }

// EnterpriseWebHasEligibleCredential is a read-only admission check. A
// temporarily busy or cooling credential is still eligible here.
func (m *Manager) EnterpriseWebHasEligibleCredential(model string) bool {
	return m.enterpriseWebHasEligibleCredential(context.Background(), model, exec.Options{}, nil)
}

func (m *Manager) enterpriseWebHasEligibleCredential(ctx context.Context, model string, opts exec.Options, rejected webRejectedCookies) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.executorLocked(web.Provider); !ok {
		return false
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	reg := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Provider != web.Provider || candidate.Disabled || candidate.Status == StatusDisabled ||
			IsBoundAccountAuth(candidate) || enterpriseRetired(candidate) || executorKeyFromAuth(candidate) != web.Provider ||
			!eligibility.allows(candidate) || !m.authSupportsRouteModelForRequest(ctx, reg, candidate, model) {
			continue
		}
		if len(rejected) != 0 {
			// Use the current cookie material. A replacement published while
			// preflight was running must not inherit an older rejection.
			c, err := web.CredentialFrom(candidate.Metadata)
			if err != nil {
				continue
			}
			if _, failed := rejected[sha256.Sum256([]byte(c.Cookies))]; failed {
				continue
			}
		}
		return true
	}
	return false
}

func (m *Manager) enterpriseMediaFallbackAllowed(ctx context.Context, providers []string, req exec.Request, opts exec.Options, count bool) bool {
	if count || opts.Alt == "countTokens" || m.HomeEnabled() || pinnedAuthIDFromMetadata(opts.Metadata) != "" ||
		enterpriseProbeActive(ctx) || EnterpriseCarbonModelTestActive(ctx) ||
		!containsEnterpriseProvider(providers) && !containsEnterpriseWebProvider(providers) {
		return false
	}
	return opts.SourceFormat.String() == "gemini" &&
		exec.GeminiEnterpriseOperationAllowed("gemini", exec.ResponseFormatOrSource(opts).String(), exec.Operation(opts)) &&
		!web.RequestsImageOutput(req.Payload)
}

func (m *Manager) startEnterpriseMediaFallback(ctx context.Context, providers []string, req *exec.Request, opts *exec.Options, reason web.FallbackReason) (context.Context, []string, error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if reason != "" {
		nextCtx, cleaned, err := m.WebRuntime().ProjectMediaFallback(ctx, req.Payload, reason)
		if err != nil {
			return nextCtx, nil, err
		}
		ctx = nextCtx
		req.Payload, opts.OriginalRequest = cleaned, cleaned
	} else {
		// An interceptor may have replaced the already-cleaned HTTP body.
		if web.HasMedia(req.Payload) || (!bytes.Equal(req.Payload, opts.OriginalRequest) && web.HasMedia(opts.OriginalRequest)) {
			return ctx, nil, mediaFallbackReintroducedError()
		}
	}
	if err := web.ValidateMediaFallback(ctx, req.Payload); err != nil {
		return ctx, nil, err
	}
	model := authSelectionModelFromOptions(*opts, req.Model)
	if !containsEnterpriseProvider(providers) && !m.EnterpriseHydrogenRoute(req.Model) &&
		!containsEnterpriseProvider(registry.GetGlobalRegistry().GetModelProviders(canonicalModelKey(model))) {
		message := "Ordinary Gemini Enterprise has no matching model for media fallback"
		if fallback, _ := web.MediaFallbackFrom(ctx); fallback.Reason == web.FallbackNoCredential {
			if err := m.checkEnterpriseWebRoutePolicy(model, *opts); err != nil {
				message += ". " + err.Error()
			}
		}
		return ctx, nil, web.InputFailure(http.StatusServiceUnavailable, "media_fallback_enterprise_unavailable", "", message)
	}
	ctx = web.WithBody(ctx, req.Payload)
	m.WebRuntime().RecordMediaFallback(ctx)
	return ctx, []string{ge.Provider}, nil
}

func (m *Manager) lateEnterpriseMediaFallback(ctx context.Context, providers []string, req *exec.Request, opts *exec.Options, err error) (context.Context, []string, bool, error) {
	lease := web.ExistingLease(ctx)
	if lease == nil || lease.SessionStarted() || hasUpstreamExecutionAttempt(err) ||
		isRequestTerminatedError(err) || isRequestStopError(err) || ctx.Err() != nil {
		return ctx, nil, false, nil
	}
	var cookieRejection *webCookieRejection
	var rejected webRejectedCookies
	if errors.As(err, &cookieRejection) {
		rejected = cookieRejection.rejected
	} else if !enterpriseMediaFallbackMissingAuth(err) {
		return ctx, nil, false, nil
	}
	if !m.enterpriseMediaFallbackAllowed(ctx, providers, *req, *opts, false) ||
		m.enterpriseWebHasEligibleCredential(ctx, authSelectionModelFromOptions(*opts, req.Model), *opts, rejected) {
		return ctx, nil, false, nil
	}
	ctx, selected, err := m.startEnterpriseMediaFallback(ctx, providers, req, opts, web.FallbackNoCredential)
	if err != nil {
		return ctx, nil, true, err
	}
	selected, err = m.routeEnterpriseHydrogen(ctx, selected, req.Model)
	if err == nil {
		*req, *opts = session.Enrich(*req, *opts)
	}
	return ctx, selected, true, err
}

func enterpriseMediaFallbackMissingAuth(err error) bool {
	var authErr *Error
	return errors.As(err, &authErr) && authErr.Code == "auth_not_found"
}

func enterpriseMediaFallbackExecutionError(ctx context.Context, err error) error {
	if _, fallback := web.MediaFallbackFrom(ctx); !fallback {
		return err
	}
	var authErr *Error
	if errors.As(err, &authErr) && (authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable" && !authErr.Retryable) {
		return web.InputFailure(http.StatusServiceUnavailable, "media_fallback_enterprise_unavailable", "", "Ordinary Gemini Enterprise has no eligible credential for media fallback")
	}
	return err
}

func mediaFallbackReintroducedError() error {
	return web.InputFailure(http.StatusBadRequest, "media_fallback_reintroduced_media", "contents", "The text fallback request cannot include attachments")
}
