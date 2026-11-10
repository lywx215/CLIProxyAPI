package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
)

// parseModelTokenLimit extracts an input-token limit from a -Nm model suffix.
// N is measured in units of 10,000 tokens, so -20m represents 200,000 tokens.
func parseModelTokenLimit(model string) int {
	model = strings.TrimSpace(model)
	if len(model) < 3 || model[len(model)-1] != 'm' {
		return 0
	}
	dashIndex := strings.LastIndex(model[:len(model)-1], "-")
	if dashIndex < 0 {
		return 0
	}
	units, err := strconv.Atoi(model[dashIndex+1 : len(model)-1])
	if err != nil || units <= 0 {
		return 0
	}
	return units * 10000
}

func checkModelTokenEstimate(ctx context.Context, protocol, model string, body []byte) (context.Context, error) {
	limit := parseModelTokenLimit(model)
	if limit <= 0 {
		return ctx, nil
	}
	if protocol == "gemini" {
		media := web.HasMediaInContext(ctx, body)
		_, projected := web.MediaFallbackFrom(ctx)
		if media || projected {
			ctx = web.WithMediaFallbackValidator(ctx, func(cleaned []byte) error {
				if estimated := len(cleaned) / 4; estimated > limit {
					return web.InputFailure(400, "media_fallback_text_limit", "contents", fmt.Sprintf("Estimated text size (%d tokens) exceeds the model limit (%d)", estimated, limit))
				}
				return nil
			})
		}
		if media {
			return ctx, nil
		}
	}
	if estimated := len(body) / 4; estimated > limit {
		return ctx, fmt.Errorf("estimated input size (%d tokens) exceeds limit (%d) for model %q", estimated, limit, model)
	}
	return ctx, nil
}
