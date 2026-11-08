package cliproxy

import (
	"strings"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestAntigravityEnterpriseModelPublicationIsBindingFenced(t *testing.T) {
	l := ae.License{ProjectID: "project-model-test", Location: "global", UserTier: "gcp-ge-plus-tier"}
	graph := ae.Account{ID: ae.AccountID("sdk-user", "sdk-client"), Version: 1, Revision: 1}
	graph.Bindings = []ae.Binding{{ID: ae.BindingID(graph.ID, l), Revision: 1, License: l, Settings: map[string]any{"prefix": "ae-team"}}}
	m := coreauth.NewManager(nil, nil, nil)
	parent, err := m.Register(coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{ID: "ae-sdk-fixture.json", Provider: ae.Provider, Status: coreauth.StatusActive, Metadata: ae.PutAccount(map[string]any{"email": "sdk@example.test", "google_user_id": "sdk-user", "client_id": "sdk-client", "access_token": "fixture-only", "expired": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}, graph)})
	if err != nil {
		t.Fatal(err)
	}
	child := m.AntigravityEnterpriseBindings(parent.ID)[0]
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(child.ID) })
	s := &Service{coreManager: m, cfg: &config.Config{}}
	s.applyAntigravityEnterpriseModels(t.Context(), child, ae.Config{}, GlobalModelRegistry().ClientRegistrationEpoch(child.ID))
	models := GlobalModelRegistry().GetModelsForClient(child.ID)
	if len(models) < 14 {
		t.Fatalf("default enterprise models missing: %d", len(models))
	}
	prefixed := 0
	for _, model := range models {
		if strings.HasPrefix(model.ID, "ae-team/") {
			prefixed++
		}
	}
	if prefixed != 14 {
		t.Fatalf("binding model prefix lost: %d", prefixed)
	}
	if len(GlobalModelRegistry().GetModelsForClient(parent.ID)) != 0 {
		t.Fatal("parent registered inference models")
	}
	_, current, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
	if _, err = m.UpdateAntigravityEnterpriseAccount(t.Context(), parent, current.Revision, func(a *ae.Account) error { a.Bindings[0].Settings["prefix"] = "new-prefix"; return nil }); err != nil {
		t.Fatal(err)
	}
	epoch := GlobalModelRegistry().ClientRegistrationEpoch(child.ID)
	s.applyAntigravityEnterpriseModels(t.Context(), child, ae.Config{AllowedModels: []string{"gemini-4-argon"}}, epoch)
	if got := GlobalModelRegistry().ClientRegistrationEpoch(child.ID); got != epoch {
		t.Fatal("stale binding replaced catalog")
	}
	fresh := m.AntigravityEnterpriseBindings(parent.ID)[0]
	s.applyAntigravityEnterpriseModels(t.Context(), fresh, ae.Config{AllowedModels: []string{"gemini-4-argon"}}, epoch)
	models = GlobalModelRegistry().GetModelsForClient(child.ID)
	if len(models) < 3 || len(models) > 6 {
		t.Fatalf("authoritative model filter ignored: %d", len(models))
	}
	for _, model := range models {
		if !strings.Contains(model.ID, "gemini-4-argon") {
			t.Fatal("unexpected model retained")
		}
	}
	latestParent, latestGraph, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
	if _, err = m.UpdateAntigravityEnterpriseAccount(t.Context(), latestParent, latestGraph.Revision, func(a *ae.Account) error {
		a.Bindings[0].Settings["model_aliases"] = []any{map[string]any{"name": "gemini-4-argon-high", "alias": "local-high"}}
		a.Bindings[0].Settings["excluded_models"] = []string{"gemini-4-argon-medium"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{ae.Provider: {{Name: "gemini-4-argon-low", Alias: "global-low"}}}
	fresh = m.AntigravityEnterpriseBindings(parent.ID)[0]
	s.applyAntigravityEnterpriseModels(t.Context(), fresh, ae.Config{AllowedModels: []string{"gemini-4-argon"}}, GlobalModelRegistry().ClientRegistrationEpoch(child.ID))
	seen := map[string]bool{}
	for _, model := range GlobalModelRegistry().GetModelsForClient(child.ID) {
		seen[model.ID] = true
		if strings.Contains(model.ID, "-medium") {
			t.Fatal("binding exclusion ignored")
		}
	}
	if !seen["new-prefix/local-high"] || !seen["new-prefix/global-low"] {
		t.Fatalf("scoped/global aliases or prefix missing: %v", seen)
	}
}
