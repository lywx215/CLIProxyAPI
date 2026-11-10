package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// EnterpriseWebNativeModel identifies native requests that need media admission
// before body logging, using the same public model resolution as the handler.
func EnterpriseWebNativeModel(manager *coreauth.Manager, model string) (string, bool) {
	if manager == nil {
		return model, false
	}
	if manager.EnterpriseWebModel(model) {
		return model, true
	}
	parsed := thinking.ParseSuffix(model)
	base := strings.TrimSpace(parsed.ModelName)
	reg := registry.GetGlobalRegistry()
	if base == "auto" && !manager.HomeEnabled() {
		resolved, err := reg.GetFirstAvailableModel("")
		if err != nil {
			return model, false
		}
		base, model = resolved, resolved
		if parsed.HasSuffix {
			model += "(" + parsed.RawSuffix + ")"
		}
		if manager.EnterpriseWebModel(model) {
			return model, true
		}
	}
	providers := reg.GetModelProviders(base)
	if lower := strings.ToLower(base); len(providers) == 0 && lower != base {
		providers = reg.GetModelProviders(lower)
	}
	if len(providers) == 0 && base != model {
		providers = reg.GetModelProviders(model)
		if lower := strings.ToLower(model); len(providers) == 0 && lower != model {
			providers = reg.GetModelProviders(lower)
		}
	}
	for _, provider := range providers {
		if provider == "gemini-enterprise" || provider == web.Provider {
			return model, true
		}
	}
	return model, false
}

func mediaFallbackExecutionPayload(ctx context.Context, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Request, coreexecutor.Options) {
	if cleaned := web.MediaFallbackPayload(ctx); cleaned != nil {
		req.Payload = cleaned
		opts.OriginalRequest = cleaned
	}
	return req, opts
}

func mediaFallbackPluginRouteError() *interfaces.ErrorMessage {
	err := web.InputFailure(http.StatusServiceUnavailable, "media_fallback_enterprise_unavailable", "", "Media fallback requires Gemini Enterprise for the requested model")
	return executionErrorMessage(err)
}
