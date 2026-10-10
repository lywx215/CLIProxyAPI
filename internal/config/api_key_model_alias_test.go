package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAPIKeyModelAliasV8RoundTripAndRemoval(t *testing.T) {
	raw := []byte(`config-version: 8
routing:
  # Shared aliases remain separate from credential model allowlists.
  api-key-model-alias:
    gemini:
      - name: ' source '
        alias: ' public '
        fork: true
        display-name: ' Shared model '
        force-mapping: true
api-keys:
  gemini:
    - name: grouped
      models: [{name: source, alias: source}]
      keys:
        - api-key: inherited
          models: null
        - api-key: overridden
          models: [{name: other, alias: other}]
`)
	if err := ValidateV8Config(raw); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	aliases := cfg.APIKeyModelAlias["gemini"]
	if len(aliases) != 1 || aliases[0].Name != "source" || aliases[0].Alias != "public" || aliases[0].DisplayName != "Shared model" || !aliases[0].Fork || !aliases[0].ForceMapping {
		t.Fatalf("parsed aliases = %#v", aliases)
	}
	if len(cfg.GeminiKey) != 2 || cfg.GeminiKey[0].Models[0].Alias != "source" || cfg.GeminiKey[1].Models[0].Name != "other" {
		t.Fatal("global aliases changed effective credential model lists")
	}
	clone := cfg.CloneForRuntime()
	clone.APIKeyModelAlias["gemini"][0].Alias = "clone-only"
	if cfg.APIKeyModelAlias["gemini"][0].Alias != "public" {
		t.Fatal("runtime clone shares global aliases")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(saved); err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(saved, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	if yamlPath(root, "routing.api-key-model-alias.gemini") == nil || yamlPath(root, "api-key-model-alias") != nil || !strings.Contains(string(saved), "# Shared aliases") {
		t.Fatalf("canonical alias path/comments lost: %s", saved)
	}
	group := yamlPath(root, "api-keys.gemini").Content[0]
	if yamlPath(group, "keys").Content[0].Kind != yaml.MappingNode || yamlPath(yamlPath(group, "keys").Content[0], "models").Tag != "!!null" {
		t.Fatal("save flattened inherited credential models")
	}
	cfg.APIKeyModelAlias = nil
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	removed, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed.APIKeyModelAlias) != 0 || len(removed.GeminiKey[0].Models) != 1 {
		t.Fatalf("removal resurrected aliases or changed source allowlist: %+v", removed.APIKeyModelAlias)
	}
}

func TestSanitizeAPIKeyModelAlias(t *testing.T) {
	cfg := &Config{APIKeyModelAlias: map[string][]OAuthModelAlias{
		" Gemini ": {
			{Name: " source ", Alias: " shared ", DisplayName: " display ", Fork: true, ForceMapping: true},
			{Name: "other", Alias: "SHARED"},
			{Name: "source", Alias: "extra"},
			{Name: "source", Alias: "SOURCE"},
			{Name: "", Alias: "empty"},
		},
	}}
	cfg.SanitizeAPIKeyModelAlias()
	aliases := cfg.APIKeyModelAlias["gemini"]
	if len(aliases) != 2 || aliases[0].Name != "source" || aliases[0].Alias != "shared" || aliases[0].DisplayName != "display" || !aliases[0].Fork || !aliases[0].ForceMapping || aliases[1].Alias != "extra" {
		t.Fatalf("normalized aliases = %#v", aliases)
	}
}
