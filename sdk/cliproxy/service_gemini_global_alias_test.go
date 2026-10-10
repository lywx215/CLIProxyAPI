package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type geminiAliasCatalogRefreshHook struct {
	clientID string
	models   chan []*registry.ModelInfo
}

func (h *geminiAliasCatalogRefreshHook) OnModelsRegistered(_ context.Context, _ string, clientID string, models []*registry.ModelInfo) {
	if clientID == h.clientID {
		select {
		case h.models <- models:
		default:
		}
	}
}

func (*geminiAliasCatalogRefreshHook) OnModelsUnregistered(context.Context, string, string) {}

func TestRegisterGeminiGlobalAliasesPreservesCatalogSourceMetadata(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ForceModelPrefix: true},
		GeminiKey: []config.GeminiKey{{
			APIKey: "synthetic-global-catalog", Prefix: "tenant",
			Models: []config.GeminiModel{{
				Name: "source(low)", Alias: "source(low)", DisplayName: "Source", MaxContextLength: 54321, IsCompat: true,
				Thinking: &registry.ThinkingSupport{Levels: []string{"low"}},
			}},
		}},
		APIKeyModelAlias: map[string][]config.OAuthModelAlias{"gemini": {
			{Name: "source", Alias: "public", Fork: true, DisplayName: "Shared"},
			{Name: "source", Alias: "alternate", Fork: false},
			{Name: "not-allowed", Alias: "unavailable", Fork: true},
		}},
	}
	service := &Service{cfg: cfg}
	auth := &coreauth.Auth{ID: "gemini-global-catalog", Provider: "gemini", Prefix: "tenant", Attributes: map[string]string{"api_key": "synthetic-global-catalog"}}
	modelsRegistry := GlobalModelRegistry()
	t.Cleanup(func() { modelsRegistry.UnregisterClient(auth.ID) })
	service.registerModelsForAuth(context.Background(), auth)
	models := modelsRegistry.GetModelsForClient(auth.ID)
	if len(models) != 3 {
		t.Fatalf("registered models = %+v, want source and two aliases", models)
	}
	for _, model := range models {
		if model.MetadataModelID != "source(low)" || model.Thinking == nil || model.Thinking.Levels[0] != "low" || !model.ExplicitThinking || !model.IsCompat || model.MaxContextLength != 54321 {
			t.Fatalf("metadata lost for %q: %+v", model.ID, model)
		}
		if model.ID == "tenant/public" && model.DisplayName != "Shared" {
			t.Fatalf("global display-name = %q", model.DisplayName)
		}
	}
	cfg.APIKeyModelAlias = nil
	service.registerModelsForAuth(context.Background(), auth)
	models = modelsRegistry.GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "tenant/source(low)" {
		t.Fatalf("catalog alias removal = %+v", models)
	}
}

func TestGeminiGlobalCatalogAliasCollisionAndNoFork(t *testing.T) {
	baseline := registry.GetGeminiModels()
	if len(baseline) < 2 {
		t.Fatal("missing baseline Gemini catalog")
	}
	source, alias := baseline[0].ID, baseline[1].ID
	cfg := &config.Config{APIKeyModelAlias: map[string][]config.OAuthModelAlias{"gemini": {{Name: source, Alias: alias, Fork: false}}}}
	models := applyGeminiAPIKeyModelAliases(cfg, &config.GeminiKey{}, baseline)
	aliasCount := 0
	for _, model := range models {
		if model.ID == source {
			t.Fatal("no-fork source stayed in catalog")
		}
		if model.ID == alias {
			aliasCount++
			if model.MetadataModelID != source {
				t.Fatalf("baseline alias collision resolved to wrong source: %+v", model)
			}
		}
	}
	if aliasCount != 1 {
		t.Fatalf("alias count = %d, want one", aliasCount)
	}
}

func TestRegisterGeminiGlobalCrossAliasesUseTheirSourceMetadata(t *testing.T) {
	baseline := registry.GetGeminiModels()
	if len(baseline) < 2 {
		t.Fatal("missing Gemini catalog models")
	}
	a, b := baseline[0].ID, baseline[1].ID
	for _, tc := range []struct {
		name, firstAlias string
	}{
		{name: "swap", firstAlias: b},
		{name: "source-is-an-alias", firstAlias: "global-cross-public"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				GeminiKey: []config.GeminiKey{{APIKey: "cross-catalog-key"}},
				APIKeyModelAlias: map[string][]config.OAuthModelAlias{"gemini": {
					{Name: a, Alias: tc.firstAlias, Fork: true, DisplayName: "From A"},
					{Name: b, Alias: a, Fork: true, DisplayName: "From B"},
				}},
			}
			service := &Service{cfg: cfg}
			auth := &coreauth.Auth{ID: "cross-catalog-" + tc.name, Provider: "gemini", Attributes: map[string]string{"api_key": "cross-catalog-key"}}
			modelsRegistry := GlobalModelRegistry()
			t.Cleanup(func() { modelsRegistry.UnregisterClient(auth.ID) })
			service.registerModelsForAuth(t.Context(), auth)
			models := modelsRegistry.GetModelsForClient(auth.ID)
			for _, mapping := range []struct {
				alias, source, displayName string
			}{
				{tc.firstAlias, a, "From A"},
				{a, b, "From B"},
			} {
				count := 0
				for _, model := range models {
					if model.ID == mapping.alias {
						count++
						if model.MetadataModelID != mapping.source || model.DisplayName != mapping.displayName {
							t.Fatalf("alias %q catalog = %+v, want source %q", mapping.alias, model, mapping.source)
						}
					}
				}
				if count != 1 {
					t.Fatalf("alias %q catalog count = %d, want one", mapping.alias, count)
				}
			}
		})
	}
}

func TestGeminiGlobalAliasCatalogRefreshRebuildsRouting(t *testing.T) {
	originalPath, errAbs := filepath.Abs(filepath.Join("..", "..", "internal", "registry", "models", "models.json"))
	if errAbs != nil {
		t.Fatal(errAbs)
	}
	catalogSources := func(path string) registry.CatalogSources {
		return registry.CatalogSources{
			Catalog: path, CodexCatalog: filepath.Join(filepath.Dir(originalPath), "codex_client_models.json"),
			DevinCatalog: filepath.Join(filepath.Dir(originalPath), "devin_models.json"),
		}
	}
	original, errRead := os.ReadFile(originalPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var catalog map[string]json.RawMessage
	if errDecode := json.Unmarshal(original, &catalog); errDecode != nil {
		t.Fatal(errDecode)
	}
	var gemini []*registry.ModelInfo
	if errDecode := json.Unmarshal(catalog["gemini"], &gemini); errDecode != nil || len(gemini) < 2 {
		t.Fatalf("missing Gemini fixture models: %v", errDecode)
	}
	source := gemini[0].ID
	remaining, errEncode := json.Marshal(gemini[1:])
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	catalog["gemini"] = remaining
	withoutSource, errEncode := json.Marshal(catalog)
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	changedPath := filepath.Join(t.TempDir(), "without-source.json")
	if errWrite := os.WriteFile(changedPath, withoutSource, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg := &config.Config{
		GeminiKey:        []config.GeminiKey{{APIKey: "catalog-refresh-key"}},
		APIKeyModelAlias: map[string][]config.OAuthModelAlias{"gemini": {{Name: source, Alias: "catalog-refresh-alias", Fork: true}}},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth, errRegister := manager.Register(t.Context(), &coreauth.Auth{
		ID: "gemini-global-catalog-refresh", Provider: "gemini", Attributes: map[string]string{"api_key": "catalog-refresh-key"},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	if got := manager.ResolveExecutionModel(auth, "catalog-refresh-alias"); got != source {
		t.Fatalf("initial alias = %q, want %q", got, source)
	}
	service := &Service{cfg: cfg, coreManager: manager}
	modelsRegistry := GlobalModelRegistry()
	hook := &geminiAliasCatalogRefreshHook{clientID: auth.ID, models: make(chan []*registry.ModelInfo, 8)}
	registry.GetGlobalRegistry().SetHook(hook)
	service.registerModelRefreshCallback()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		registry.GetGlobalRegistry().SetHook(nil)
		modelsRegistry.UnregisterClient(auth.ID)
		registry.SetModelRefreshCallback(nil)
		// Restore the catalog through its existing publication path before returning.
		restored := make(chan struct{}, 1)
		registry.SetModelRefreshCallback(func(providers []string) {
			for _, provider := range providers {
				if provider == "gemini" {
					select {
					case restored <- struct{}{}:
					default:
					}
				}
			}
		})
		restoreCtx, restoreCancel := context.WithCancel(context.Background())
		defer restoreCancel()
		defer registry.SetModelRefreshCallback(nil)
		registry.StartModelCatalogUpdaters(restoreCtx, catalogSources(originalPath), false)
		if len(registry.GetGeminiModels()) != len(gemini) {
			select {
			case <-restored:
			case <-time.After(5 * time.Second):
				t.Error("failed to restore Gemini catalog")
			}
		}
	})
	registry.StartModelCatalogUpdaters(ctx, catalogSources(changedPath), false)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case models := <-hook.models:
			aliasPresent := false
			for _, model := range models {
				aliasPresent = aliasPresent || model.ID == "catalog-refresh-alias"
			}
			if aliasPresent {
				continue
			}
			if got := manager.ResolveExecutionModel(auth, "catalog-refresh-alias"); got != "catalog-refresh-alias" {
				t.Fatalf("catalog removed source but compiled routing retained %q", got)
			}
			return
		case <-deadline.C:
			t.Fatal("Gemini catalog refresh did not remove unavailable alias")
		}
	}
}
