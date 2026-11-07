// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
// Package contract contains the narrow boundary between the host and Business.
// It deliberately has no dependency on the AI Studio service or authentication.
package contract

import (
	"context"
	"encoding/json"
	"net/http"
)

const ProviderHeader = "X-AIStudio-Provider"

type principalKey struct{}

// WithPrincipal is called only by the host after its model-key authentication.
func WithPrincipal(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, principalKey{}, id)
}

func Principal(ctx context.Context) string {
	id, _ := ctx.Value(principalKey{}).(string)
	return id
}

// Error uses the Google JSON error envelope. Messages must not contain upstream
// bodies, cookies, tokens, paths to credentials, or user request contents.
func Error(w http.ResponseWriter, code int, status, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "status": status, "message": message,
	}})
}
