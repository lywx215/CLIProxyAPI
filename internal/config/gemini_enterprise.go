package config

import (
	"fmt"
	"regexp"
	"strings"
)

type OAuthConfig struct {
	Providers OAuthProviders `yaml:"providers,omitempty" json:"providers,omitempty"`
}

type OAuthProviders struct {
	GeminiEnterprise *GeminiEnterpriseOAuth `yaml:"gemini-enterprise,omitempty" json:"gemini-enterprise,omitempty"`
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

func (c GeminiEnterpriseOAuth) WithDefaults() GeminiEnterpriseOAuth {
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecretEnv = strings.TrimSpace(c.ClientSecretEnv)
	c.ProjectID = strings.TrimSpace(c.ProjectID)
	c.EngineID = strings.TrimSpace(c.EngineID)
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
	if strings.TrimSpace(c.ClientID) == "" {
		return fmt.Errorf("oauth.providers.gemini-enterprise.client-id is required")
	}
	if c.ClientSecretEnv != "" && !enterpriseEnvName.MatchString(c.ClientSecretEnv) {
		return fmt.Errorf("gemini-enterprise.client-secret-env must be an environment variable name")
	}
	return nil
}

// ValidateDiscovery requires the user-supplied application and assistant only.
func (c GeminiEnterpriseOAuth) ValidateDiscovery() error {
	c = c.WithDefaults()
	if err := c.ValidateApplication(); err != nil {
		return err
	}
	for _, value := range []string{c.EngineID, c.AssistantID} {
		if !enterpriseResourceComponent.MatchString(value) {
			return fmt.Errorf("gemini-enterprise: a valid engine ID and assistant ID are required")
		}
	}
	return nil
}
