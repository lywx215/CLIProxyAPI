package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestGeminiGlobalAliasRoutingMetadataAndReload(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{{
			APIKey: "synthetic-global-key", Prefix: "tenant",
			Models: []internalconfig.GeminiModel{{
				Name: "source(low)", Alias: "source(low)", MaxContextLength: 12345, IsCompat: true,
				Thinking: &registry.ThinkingSupport{Levels: []string{"low"}},
			}},
		}},
		APIKeyModelAlias: map[string][]internalconfig.OAuthModelAlias{
			"gemini": {{Name: "source", Alias: "public", Fork: true, ForceMapping: true, DisplayName: "Shared model"}},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &Auth{ID: "gemini-global", Provider: "gemini", Prefix: "tenant", Attributes: map[string]string{"api_key": "synthetic-global-key"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	models, _, result, snapshot := manager.executionModelCandidatesWithAlias(auth, "tenant/public(high)")
	if len(models) != 1 || models[0] != "source(low)" || !result.ForceMapping || result.OriginalAlias != "public" {
		t.Fatalf("execution models/result = %v/%+v", models, result)
	}
	if slow := resolveUpstreamModelForGeminiAPIKey(snapshot.config, auth, "public(high)"); slow != models[0] {
		t.Fatalf("slow routing = %q, want %q", slow, models[0])
	}
	req := attachResolvedAPIKeyModelInfo(snapshot, cliproxyexecutor.Request{}, auth, "tenant/public(high)", models[0])
	info, ok := ResolvedAPIKeyModelInfo(req)
	if !ok || info.Thinking == nil || len(info.Thinking.Levels) != 1 || info.Thinking.Levels[0] != "low" || !info.IsCompat || info.MaxContextLength != 12345 || info.DisplayName != "Shared model" {
		t.Fatalf("selected source capabilities = %+v", info)
	}
	cfg.APIKeyModelAlias["gemini"][0].Alias = "changed-in-place"
	if resolved := manager.lookupAPIKeyUpstreamModel(auth.ID, "public(high)"); resolved != "source(low)" {
		t.Fatal("published alias routing shares mutable config")
	}
	next := cfg.CloneForRuntime()
	next.APIKeyModelAlias = nil
	manager.SetConfig(next)
	if resolved := manager.lookupAPIKeyUpstreamModel(auth.ID, "public"); resolved != "" {
		t.Fatalf("removed global alias survived reload: %q", resolved)
	}
	oldReq := attachResolvedAPIKeyModelInfo(snapshot, cliproxyexecutor.Request{}, auth, "tenant/public", "source(low)")
	if _, oldOK := ResolvedAPIKeyModelInfo(oldReq); !oldOK {
		t.Fatal("reload invalidated an in-flight capability snapshot")
	}
	if len(next.GeminiKey[0].Models) != 1 || next.GeminiKey[0].Models[0].Alias != "source(low)" {
		t.Fatal("global aliases were copied into credential model rows")
	}
}

func TestGeminiGlobalCrossAliasesKeepCompiledRoutingConsistent(t *testing.T) {
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
			cfg := &internalconfig.Config{
				GeminiKey: []internalconfig.GeminiKey{{APIKey: "cross-alias-key"}},
				APIKeyModelAlias: map[string][]internalconfig.OAuthModelAlias{"gemini": {
					{Name: a, Alias: tc.firstAlias, Fork: true, ForceMapping: true, DisplayName: "From A"},
					{Name: b, Alias: a, Fork: true, ForceMapping: true, DisplayName: "From B"},
				}},
			}
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			auth := &Auth{ID: "gemini-cross-" + tc.name, Provider: "gemini", Attributes: map[string]string{"api_key": "cross-alias-key"}}
			if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			for _, mapping := range []struct {
				alias, source, displayName string
			}{
				{tc.firstAlias, a, "From A"},
				{a, b, "From B"},
			} {
				models, _, result, snapshot := manager.executionModelCandidatesWithAlias(auth, mapping.alias)
				if len(models) != 1 || models[0] != mapping.source || result.UpstreamModel != mapping.source || !result.ForceMapping || result.OriginalAlias != mapping.alias {
					t.Fatalf("alias %q fast/result = %v/%+v, want %q", mapping.alias, models, result, mapping.source)
				}
				if slow := resolveUpstreamModelForGeminiAPIKey(snapshot.config, auth, mapping.alias); slow != mapping.source {
					t.Fatalf("alias %q slow route = %q, want %q", mapping.alias, slow, mapping.source)
				}
				req := attachResolvedAPIKeyModelInfo(snapshot, cliproxyexecutor.Request{}, auth, mapping.alias, models[0])
				info, ok := ResolvedAPIKeyModelInfo(req)
				if !ok || info.ID != mapping.source || info.DisplayName != mapping.displayName {
					t.Fatalf("alias %q capability = %+v, want source %q", mapping.alias, info, mapping.source)
				}
			}
		})
	}
}

func TestGeminiGlobalAliasLocalPriorityAndCredentialAvailability(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{APIKey: "global", Models: []internalconfig.GeminiModel{{Name: "source"}}},
			{APIKey: "local", Models: []internalconfig.GeminiModel{{Name: "source"}, {Name: "override", Alias: "public", ForceMapping: true}}},
			{APIKey: "unavailable", Models: []internalconfig.GeminiModel{{Name: "other"}}},
			{APIKey: "excluded", Models: []internalconfig.GeminiModel{{Name: "source"}}, ExcludedModels: []string{"sou*"}},
			{APIKey: "partial", Models: []internalconfig.GeminiModel{{Name: "source"}, {Name: "source", Alias: "remaining-local"}}},
		},
		InteractionsKey:  []internalconfig.GeminiKey{{APIKey: "interactions", Models: []internalconfig.GeminiModel{{Name: "source"}}}},
		APIKeyModelAlias: map[string][]internalconfig.OAuthModelAlias{"gemini": {{Name: "source", Alias: "public", Fork: true}}},
	})
	for _, tc := range []struct {
		key, provider, want string
	}{
		{"global", "gemini", "source(high)"},
		{"local", "gemini", "override(high)"},
		{"unavailable", "gemini", ""},
		{"excluded", "gemini", ""},
		{"partial", "gemini", ""},
		{"interactions", "gemini-interactions", ""},
	} {
		t.Run(tc.key, func(t *testing.T) {
			auth := &Auth{ID: "global-scope-" + tc.key, Provider: tc.provider, Attributes: map[string]string{"api_key": tc.key}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			if got := manager.lookupAPIKeyUpstreamModel(auth.ID, "public(high)"); got != tc.want {
				t.Fatalf("resolved = %q, want %q", got, tc.want)
			}
			if tc.key == "local" {
				result := manager.resolveAPIKeyModelAliasWithResult(auth, "public(high)")
				if !result.ForceMapping || result.OriginalAlias != "public" || result.UpstreamModel != tc.want {
					t.Fatalf("local force-mapping priority lost: %+v", result)
				}
			}
		})
	}
}
