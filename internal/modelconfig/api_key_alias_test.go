package modelconfig

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestGeminiAPIKeyAliasesRespectCredentialScope(t *testing.T) {
	cfg := &config.Config{APIKeyModelAlias: map[string][]config.OAuthModelAlias{
		"gemini": {{Name: "source", Alias: "public", Fork: true}},
	}}
	for _, tc := range []struct {
		name string
		key  config.GeminiKey
		want int
	}{
		{"identity source", config.GeminiKey{Models: []config.GeminiModel{{Name: "source", Alias: "source"}}}, 1},
		{"implicit identity", config.GeminiKey{Models: []config.GeminiModel{{Name: "source"}}}, 1},
		{"source unavailable", config.GeminiKey{Models: []config.GeminiModel{{Name: "other"}}}, 0},
		{"local source mapping", config.GeminiKey{Models: []config.GeminiModel{{Name: "source", Alias: "local"}}}, 0},
		{"local alias collision", config.GeminiKey{Models: []config.GeminiModel{{Name: "source"}, {Name: "other", Alias: "public"}}}, 0},
		{"local direct name collision", config.GeminiKey{Models: []config.GeminiModel{{Name: "source"}, {Name: "public"}}}, 0},
		{"source exclusion", config.GeminiKey{Models: []config.GeminiModel{{Name: "source"}}, ExcludedModels: []string{"SOURCE"}}, 0},
		{"alias exclusion", config.GeminiKey{Models: []config.GeminiModel{{Name: "source"}}, ExcludedModels: []string{"pub*"}}, 0},
		{"disabled credential", config.GeminiKey{Models: []config.GeminiModel{{Name: "source"}}, ExcludedModels: []string{"*"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GeminiAPIKeyAliases(cfg, &tc.key); len(got) != tc.want {
				t.Fatalf("eligible aliases = %#v, want %d", got, tc.want)
			}
		})
	}
}

func TestGeminiAPIKeyAliasesPreserveSourceMetadataAndSuffix(t *testing.T) {
	cfg := &config.Config{APIKeyModelAlias: map[string][]config.OAuthModelAlias{
		"gemini": {{Name: "source", Alias: "public", Fork: true, ForceMapping: true, DisplayName: "Shared"}},
	}}
	key := &config.GeminiKey{Models: []config.GeminiModel{{
		Name: "source(low)", Alias: "source(low)", MaxContextLength: 12345, IsCompat: true,
		Thinking: &registry.ThinkingSupport{Levels: []string{"low"}},
	}}}
	aliases := GeminiAPIKeyAliases(cfg, key)
	if len(aliases) != 1 || aliases[0].SourceName != "source(low)" || aliases[0].Model.Name != "source(low)" || aliases[0].Model.Alias != "public" || !aliases[0].Model.ForceMapping || aliases[0].Model.DisplayName != "Shared" || aliases[0].Model.MaxContextLength != 12345 || !aliases[0].Model.IsCompat || aliases[0].Model.Thinking.Levels[0] != "low" {
		t.Fatalf("source metadata lost: %#v", aliases)
	}
	models := GeminiAPIKeyRoutingModels(cfg, key)
	models[1].Alias = "transient-change"
	if len(key.Models) != 1 || key.Models[0].Alias != "source(low)" || cfg.APIKeyModelAlias["gemini"][0].Alias != "public" {
		t.Fatal("routing projection mutated persisted model rules")
	}
	key.ExcludedModels = []string{"source"}
	if got := GeminiAPIKeyAliases(cfg, key); len(got) != 0 {
		t.Fatal("thinking suffix bypassed source exclusion")
	}
}

func TestGeminiAPIKeyAliasesUseDefaultCatalogOnlyWhenNoAllowlist(t *testing.T) {
	models := registry.GetGeminiModels()
	if len(models) == 0 {
		t.Fatal("missing baseline Gemini catalog")
	}
	cfg := &config.Config{APIKeyModelAlias: map[string][]config.OAuthModelAlias{
		"gemini": {{Name: models[0].ID, Alias: "shared-default"}, {Name: "missing-catalog-model", Alias: "missing"}},
	}}
	key := &config.GeminiKey{}
	if aliases := GeminiAPIKeyAliases(cfg, key); len(aliases) != 1 || aliases[0].Model.Name != models[0].ID {
		t.Fatalf("default catalog aliases = %#v", aliases)
	}
	if len(key.Models) != 0 {
		t.Fatal("default aliases were copied into credential models")
	}
}
