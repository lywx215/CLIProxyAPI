package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// VertexCompatKey represents the configuration for Vertex AI-compatible API keys.
// This supports third-party services that use Vertex AI-style endpoint paths
// (/publishers/google/models/{model}:streamGenerateContent) but authenticate
// with simple API keys instead of Google Cloud service account credentials.
//
// Example services: zenmux.ai and similar Vertex-compatible providers.
type VertexCompatKey struct {
	// APIKey is the authentication key for accessing the Vertex-compatible API.
	// Maps to the x-goog-api-key header.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces model aliases for this credential (e.g., "teamA/vertex-pro").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL optionally overrides the Vertex-compatible API endpoint.
	// The executor will append "/v1/publishers/google/models/{model}:action" to this.
	// When empty, requests fall back to the default Vertex API base URL.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProjectID and Location opt into the official project-scoped Vertex API.
	ProjectID string `yaml:"project-id,omitempty" json:"project-id,omitempty"`
	Location  string `yaml:"location,omitempty" json:"location,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	// Commonly used for cookies, user-agent, and other authentication headers.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []VertexCompatModel `yaml:"models,omitempty" json:"models,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling overrides the global cooling policy for this credential when set.
	// True disables auth/model cooldowns; false explicitly enables them.
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry optionally overrides the global request-retry for this credential.
	// Nil or a negative value means "use the global request-retry". 0 disables additional retry rounds.
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// Interactions optionally enables native Vertex Interactions API routing for this key.
	Interactions *bool `yaml:"interactions,omitempty" json:"interactions,omitempty"`
}

func (k VertexCompatKey) GetAPIKey() string   { return k.APIKey }
func (k VertexCompatKey) GetBaseURL() string  { return k.BaseURL }
func (k VertexCompatKey) GetPrefix() string   { return k.Prefix }
func (k VertexCompatKey) GetProxyURL() string { return k.ProxyURL }

type VertexProjectRoute struct {
	ProjectID string
	Location  string
}

func (k VertexCompatKey) ProjectRoute() VertexProjectRoute {
	return VertexProjectRoute{strings.TrimSpace(k.ProjectID), strings.ToLower(strings.TrimSpace(k.Location))}
}

func (r VertexProjectRoute) Configured() bool { return r.ProjectID != "" || r.Location != "" }

var (
	vertexProjectID      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	vertexLocation       = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	vertexPublisherModel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]{0,127}$`)
)

func (r VertexProjectRoute) Validate() error {
	if r.ProjectID == "" || r.Location == "" {
		return fmt.Errorf("project-id and location must be set together")
	}
	if !vertexProjectID.MatchString(r.ProjectID) {
		return fmt.Errorf("project-id must be a Google Cloud project ID or number")
	}
	if len(r.Location) > 63 || !vertexLocation.MatchString(r.Location) {
		return fmt.Errorf("location must be a Google Cloud location identifier")
	}
	return nil
}

func ValidVertexPublisherModel(model string) bool { return vertexPublisherModel.MatchString(model) }

// RouteKey distinguishes configured API key routes without exposing the key in logs.
func (k VertexCompatKey) RouteKey() [4]string {
	route := k.ProjectRoute()
	return [4]string{strings.TrimSpace(k.APIKey), strings.TrimSpace(k.BaseURL), route.ProjectID, route.Location}
}

func (cfg *Config) ValidateVertexCompatKeys() error {
	if cfg == nil {
		return nil
	}
	seen := make(map[[4]string]struct{})
	aliasLocations := make(map[string]string)
	for i, entry := range cfg.VertexCompatAPIKey {
		route := entry.ProjectRoute()
		if !route.Configured() {
			continue
		}
		if errRoute := route.Validate(); errRoute != nil {
			return fmt.Errorf("vertex-api-key[%d]: %w", i, errRoute)
		}
		if strings.TrimSpace(entry.APIKey) == "" {
			return fmt.Errorf("vertex-api-key[%d]: api-key is required for a project route", i)
		}
		if strings.TrimSpace(entry.BaseURL) != "" || (entry.Interactions != nil && *entry.Interactions) {
			return fmt.Errorf("vertex-api-key[%d]: project routes cannot use base-url or interactions=true", i)
		}
		key := entry.RouteKey()
		if _, exists := seen[key]; exists {
			return fmt.Errorf("vertex-api-key[%d]: duplicate project and location for this API key", i)
		}
		seen[key] = struct{}{}
		prefix := normalizeModelPrefix(entry.Prefix)
		for j, model := range entry.Models {
			alias := strings.TrimSpace(model.Alias)
			if alias == "" || !ValidVertexPublisherModel(strings.TrimSpace(model.Name)) {
				return fmt.Errorf("vertex-api-key[%d].models[%d]: expected a publisher model ID and alias", i, j)
			}
			aliases := []string{alias}
			if prefix != "" {
				aliases = append(aliases, prefix+"/"+alias)
				if cfg.ForceModelPrefix && prefix != alias {
					aliases = aliases[1:]
				}
			}
			for _, publicAlias := range aliases {
				canonical := strings.ToLower(publicAlias)
				if location, exists := aliasLocations[canonical]; exists && location != route.Location {
					return fmt.Errorf("vertex-api-key[%d].models[%d]: alias %q is already bound to %s", i, j, publicAlias, location)
				}
				aliasLocations[canonical] = route.Location
			}
		}
	}
	return nil
}

// VertexCompatModel represents a model configuration for Vertex compatibility,
// including the actual model name and its alias for API routing.
type VertexCompatModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// Thinking configures the thinking/reasoning capability for this model.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m VertexCompatModel) GetName() string        { return m.Name }
func (m VertexCompatModel) GetAlias() string       { return m.Alias }
func (m VertexCompatModel) GetDisplayName() string { return m.DisplayName }
func (m VertexCompatModel) GetForceMapping() bool  { return m.ForceMapping }
func (m VertexCompatModel) GetThinking() *registry.ThinkingSupport {
	return m.Thinking
}

// SanitizeVertexCompatKeys deduplicates and normalizes Vertex-compatible API key credentials.
func (cfg *Config) SanitizeVertexCompatKeys() {
	if cfg == nil {
		return
	}

	seen := make(map[[4]string]struct{}, len(cfg.VertexCompatAPIKey))
	out := cfg.VertexCompatAPIKey[:0]
	for i := range cfg.VertexCompatAPIKey {
		entry := cfg.VertexCompatAPIKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		route := entry.ProjectRoute()
		entry.ProjectID = route.ProjectID
		entry.Location = route.Location
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)

		// Sanitize models: remove entries without valid alias
		sanitizedModels := make([]VertexCompatModel, 0, len(entry.Models))
		for _, model := range entry.Models {
			model.Alias = strings.TrimSpace(model.Alias)
			model.Name = strings.TrimSpace(model.Name)
			if model.Alias != "" && model.Name != "" {
				sanitizedModels = append(sanitizedModels, model)
			}
		}
		entry.Models = sanitizedModels

		uniqueKey := entry.RouteKey()
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.VertexCompatAPIKey = out
}
