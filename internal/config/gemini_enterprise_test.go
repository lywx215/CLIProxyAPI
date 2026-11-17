package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEnterpriseV8ConfigLoadSaveAndClone(t *testing.T) {
	const raw = `config-version: 8
oauth:
  providers:
    gemini-enterprise:
      client-id: own-client
      client-secret-env: ENTERPRISE_SECRET
      project-id: project
      engine-id: engine
  model-alias:
    gemini-enterprise:
      - name: gemini-4-argon
        alias: my-model
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	enterprise := cfg.OAuth.Providers.GeminiEnterprise
	if enterprise == nil || enterprise.Location != "global" || enterprise.CollectionID != "default_collection" || enterprise.AssistantID != "default_assistant" {
		t.Fatalf("defaults=%+v", enterprise)
	}
	clone := cfg.CloneForRuntime()
	clone.OAuth.Providers.GeminiEnterprise.EngineID = "next-engine"
	if enterprise.EngineID != "engine" {
		t.Fatal("runtime clone shares mutable provider config")
	}
	if err = SaveConfigPreserveComments(path, clone); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.OAuth.Providers.GeminiEnterprise.EngineID != "next-engine" || len(reloaded.OAuthModelAlias["gemini-enterprise"]) != 1 {
		t.Fatal("save/migration lost Enterprise or host OAuth fields")
	}
	data, err := yaml.Marshal(reloaded)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err = yaml.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.OAuth.Providers.GeminiEnterprise.ClientSecretEnv != "ENTERPRISE_SECRET" {
		t.Fatal("watcher snapshot lost environment reference")
	}
}

func TestEnterpriseConfigRejectsInvalidBinding(t *testing.T) {
	for _, fields := range []string{"project-id: project", "client-id: own-client\n      project-id: ../bad\n      engine-id: engine", "client-id: own-client\n      project-id: project\n      engine-id: engine\n      client-secret-env: 'not an env name'"} {
		var cfg Config
		err := yaml.Unmarshal([]byte("oauth:\n  providers:\n    gemini-enterprise:\n      "+fields+"\n"), &cfg)
		if err == nil || !strings.Contains(err.Error(), "gemini-enterprise") {
			t.Fatalf("invalid binding accepted: %v", err)
		}
	}
}

func TestEnterpriseAutomaticLoginConfigurationDoesNotRequireEngine(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("oauth:\n  providers:\n    gemini-enterprise:\n      client-id: own-client\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	application := cfg.OAuth.Providers.GeminiEnterprise
	if application == nil || application.ValidateLogin() != nil || application.ValidateDiscovery() != nil || application.Validate() == nil {
		t.Fatal("automatic OAuth and strict inference binding validation were conflated")
	}
}

func TestEnterpriseOAuthApplicationsPreserveLegacyDefault(t *testing.T) {
	const raw = `config-version: 8
oauth:
  providers:
    gemini-enterprise:
      client-id: old-client
      client-secret-env: OLD_OAUTH_SECRET
      engine-id: old-engine
  gemini-enterprise-applications:
    new-account-default-client-id: new-client
    entries:
      - name: New application
        client-id: new-client
        client-secret-env: NEW_OAUTH_SECRET
      - name: Third application
        client-id: third-client
        client-secret-env: THIRD_OAUTH_SECRET
`
	if err := ValidateV8Config([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	clone := cfg.CloneForRuntime()
	legacy, ok := clone.OAuth.GeminiEnterpriseApplication("")
	if !ok || legacy.ClientID != "old-client" || legacy.EngineID != "old-engine" {
		t.Fatal("the default provider binding changed")
	}
	if app, ok := clone.OAuth.GeminiEnterpriseApplication("new-client"); !ok || app.ClientSecretEnv != "NEW_OAUTH_SECRET" {
		t.Fatal("the additional application was not available")
	}
	if _, ok := clone.OAuth.GeminiEnterpriseApplication("third-client"); !ok ||
		clone.OAuth.GeminiEnterpriseApplications.NewAccountDefaultClientID != "new-client" {
		t.Fatal("the third application or new-account default did not survive cloning")
	}
	for _, invalid := range []string{
		strings.Replace(raw, "default-client-id: new-client", "default-client-id: unknown", 1),
		strings.Replace(raw, "client-id: third-client", "client-id: old-client", 1),
		strings.Replace(raw, "THIRD_OAUTH_SECRET", "OLD_OAUTH_SECRET", 1),
	} {
		if ValidateV8Config([]byte(invalid)) == nil || yaml.Unmarshal([]byte(invalid), &Config{}) == nil {
			t.Fatal("invalid OAuth application configuration accepted")
		}
	}
}
