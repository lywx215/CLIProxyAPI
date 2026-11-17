package config

import (
	"fmt"
	"regexp"
	"strings"
)

type OAuthConfig struct {
	Providers                    OAuthProviders                `yaml:"providers,omitempty" json:"providers,omitempty"`
	GeminiEnterpriseApplications *GeminiEnterpriseApplications `yaml:"gemini-enterprise-applications,omitempty" json:"gemini-enterprise-applications,omitempty"`
}

type OAuthProviders struct {
	AntigravityEnterprise *AntigravityEnterpriseOAuth `yaml:"antigravity-enterprise,omitempty" json:"antigravity-enterprise,omitempty"`
	GeminiEnterprise      *GeminiEnterpriseOAuth      `yaml:"gemini-enterprise,omitempty" json:"gemini-enterprise,omitempty"`
}

// AntigravityEnterpriseOAuth uses the packaged Antigravity desktop OAuth client.
type AntigravityEnterpriseOAuth struct {
	DiscoveryQuotaProject string `yaml:"discovery-quota-project,omitempty" json:"discovery-quota-project,omitempty"`
}

// GeminiEnterpriseOAuth contains no secret values. Saved accounts retain their own binding.
type GeminiEnterpriseOAuth struct {
	ClientID        string `yaml:"client-id" json:"client-id"`
	ClientSecretEnv string `yaml:"client-secret-env,omitempty" json:"client-secret-env,omitempty"`
	ProjectID       string `yaml:"project-id" json:"project-id"`
	Location        string `yaml:"location,omitempty" json:"location,omitempty"`
	CollectionID    string `yaml:"collection-id,omitempty" json:"collection-id,omitempty"`
	EngineID        string `yaml:"engine-id" json:"engine-id"`
	AssistantID     string `yaml:"assistant-id,omitempty" json:"assistant-id,omitempty"`
}

type GeminiEnterpriseApplications struct {
	NewAccountDefaultClientID string                             `yaml:"new-account-default-client-id,omitempty" json:"new-account-default-client-id,omitempty"`
	Entries                   []GeminiEnterpriseApplicationEntry `yaml:"entries,omitempty" json:"entries,omitempty"`
}

type GeminiEnterpriseApplicationEntry struct {
	Name            string `yaml:"name" json:"name"`
	ClientID        string `yaml:"client-id" json:"client-id"`
	ClientSecretEnv string `yaml:"client-secret-env" json:"client-secret-env"`
}

func (entry GeminiEnterpriseApplicationEntry) OAuth() GeminiEnterpriseOAuth {
	return (GeminiEnterpriseOAuth{ClientID: entry.ClientID, ClientSecretEnv: entry.ClientSecretEnv}).WithDefaults()
}

// GeminiEnterpriseApplication resolves a configured client. An empty ID selects the legacy application.
func (c OAuthConfig) GeminiEnterpriseApplication(clientID string) (GeminiEnterpriseOAuth, bool) {
	if configured := c.Providers.GeminiEnterprise; configured != nil {
		legacy := configured.WithDefaults()
		if clientID == "" || clientID == legacy.ClientID {
			return legacy, true
		}
	}
	if clientID != "" && c.GeminiEnterpriseApplications != nil {
		for _, entry := range c.GeminiEnterpriseApplications.Entries {
			if app := entry.OAuth(); app.ClientID == clientID {
				return app, true
			}
		}
	}
	return GeminiEnterpriseOAuth{}, false
}

func (c OAuthConfig) ValidateGeminiEnterpriseApplications() error {
	applications := c.GeminiEnterpriseApplications
	if applications == nil {
		return nil
	}
	ids, secretRefs := make(map[string]bool), make(map[string]bool)
	if legacy, ok := c.GeminiEnterpriseApplication(""); ok {
		ids[legacy.ClientID] = true
		secretRefs[strings.ToUpper(legacy.ClientSecretEnv)] = true
	}
	for index, entry := range applications.Entries {
		app := entry.OAuth()
		if strings.TrimSpace(entry.Name) == "" || app.ClientID == "" || !enterpriseEnvName.MatchString(app.ClientSecretEnv) {
			return fmt.Errorf("oauth.gemini-enterprise-applications.entries[%d] requires a name, client-id and client-secret-env", index)
		}
		secretRef := strings.ToUpper(app.ClientSecretEnv)
		if ids[app.ClientID] || secretRefs[secretRef] {
			return fmt.Errorf("oauth.gemini-enterprise-applications.entries[%d] reuses a client-id or client-secret-env", index)
		}
		ids[app.ClientID], secretRefs[secretRef] = true, true
	}
	if preferred := strings.TrimSpace(applications.NewAccountDefaultClientID); preferred != "" && !ids[preferred] {
		return fmt.Errorf("oauth.gemini-enterprise-applications.new-account-default-client-id must name a configured client")
	}
	return nil
}

func (c GeminiEnterpriseOAuth) WithDefaults() GeminiEnterpriseOAuth {
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecretEnv = strings.TrimSpace(c.ClientSecretEnv)
	c.ProjectID = strings.TrimSpace(c.ProjectID)
	c.EngineID = strings.TrimSpace(c.EngineID)
	c.Location = strings.TrimSpace(c.Location)
	c.CollectionID = strings.TrimSpace(c.CollectionID)
	c.AssistantID = strings.TrimSpace(c.AssistantID)
	if c.Location == "" {
		c.Location = "global"
	}
	if c.CollectionID == "" {
		c.CollectionID = "default_collection"
	}
	if c.AssistantID == "" {
		c.AssistantID = "default_assistant"
	}
	return c
}

var enterpriseResourceComponent = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var enterpriseEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c GeminiEnterpriseOAuth) Validate() error {
	c = c.WithDefaults()
	if err := c.ValidateApplication(); err != nil {
		return err
	}
	for _, item := range []struct{ name, value string }{{"project-id", c.ProjectID}, {"location", c.Location}, {"collection-id", c.CollectionID}, {"engine-id", c.EngineID}, {"assistant-id", c.AssistantID}} {
		if !enterpriseResourceComponent.MatchString(item.value) {
			return fmt.Errorf("oauth.providers.gemini-enterprise.%s must be a nonempty resource component", item.name)
		}
	}
	return nil
}

// ValidateApplication permits OAuth before an account's resource is known.
func (c GeminiEnterpriseOAuth) ValidateApplication() error {
	c = c.WithDefaults()
	if strings.TrimSpace(c.ClientID) == "" {
		return fmt.Errorf("oauth.providers.gemini-enterprise.client-id is required")
	}
	if c.ClientSecretEnv != "" && !enterpriseEnvName.MatchString(c.ClientSecretEnv) {
		return fmt.Errorf("gemini-enterprise.client-secret-env must be an environment variable name")
	}
	return nil
}

// ValidateLogin accepts automatic account discovery without a resource binding.
// Optional resource hints must still be safe components when supplied.
func (c GeminiEnterpriseOAuth) ValidateLogin() error {
	c = c.WithDefaults()
	if err := c.ValidateApplication(); err != nil {
		return err
	}
	for _, item := range []struct{ name, value string }{{"project-id", c.ProjectID}, {"location", c.Location}, {"collection-id", c.CollectionID}, {"engine-id", c.EngineID}, {"assistant-id", c.AssistantID}} {
		if item.value != "" && !enterpriseResourceComponent.MatchString(item.value) {
			return fmt.Errorf("oauth.providers.gemini-enterprise.%s must be a resource component", item.name)
		}
	}
	return nil
}

// ValidateDiscovery permits automatic enumeration without an engine hint.
func (c GeminiEnterpriseOAuth) ValidateDiscovery() error {
	return c.ValidateLogin()
}
