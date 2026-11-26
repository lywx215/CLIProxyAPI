package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const (
	AttributeVertexProjectID = "vertex_project_id"
	AttributeVertexLocation  = "vertex_location"
)

func (a *Auth) VertexProjectRoute() config.VertexProjectRoute {
	if a == nil {
		return config.VertexProjectRoute{}
	}
	return config.VertexProjectRoute{
		ProjectID: strings.TrimSpace(a.Attributes[AttributeVertexProjectID]),
		Location:  strings.TrimSpace(a.Attributes[AttributeVertexLocation]),
	}
}

// ResolveVertexProjectKey matches the complete credential scope even during a config reorder.
func ResolveVertexProjectKey(cfg *config.Config, auth *Auth) *config.VertexCompatKey {
	route := auth.VertexProjectRoute()
	if cfg == nil || !route.Configured() {
		return nil
	}
	apiKey := strings.TrimSpace(auth.Attributes[AttributeAPIKey])
	for i := range cfg.VertexCompatAPIKey {
		entry := &cfg.VertexCompatAPIKey[i]
		if entry.ProjectRoute() == route && strings.TrimSpace(entry.APIKey) == apiKey {
			return entry
		}
	}
	return nil
}
