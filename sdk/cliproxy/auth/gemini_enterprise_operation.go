package auth

import (
	"net/http"
	"strings"

	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func filterEnterpriseProviders(providers []string, opts exec.Options, count, home bool) ([]string, error) {
	allowed := !count && !home && exec.GeminiEnterpriseOperationAllowed(opts.SourceFormat.String(), exec.ResponseFormatOrSource(opts).String(), exec.Operation(opts))
	if allowed {
		return providers, nil
	}
	out := make([]string, 0, len(providers))
	removed := false
	for _, p := range providers {
		if strings.EqualFold(p, "gemini-enterprise") {
			removed = true
			continue
		}
		out = append(out, p)
	}
	if removed && len(out) == 0 {
		return nil, &Error{Code: "operation_not_supported", Message: "Gemini Enterprise supports native Chat Completions and Gemini Generate Content only; Home and countTokens are unsupported", HTTPStatus: http.StatusBadRequest}
	}
	return out, nil
}
