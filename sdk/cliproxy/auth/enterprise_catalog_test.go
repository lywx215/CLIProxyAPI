package auth

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func catalogFixture(t *testing.T, edition string, trial bool) (*Manager, *Auth) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	a, err := m.Register(t.Context(), regionalTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	publishSubscription(t, m, a, edition, trial)
	now := time.Now().UTC()
	m.PublishEnterpriseCatalogState(a, ge.CatalogState{Source: "none", Status: "unavailable", CheckedAt: &now, LastError: &ge.MetadataError{Code: "service_disabled", Message: "Google API disabled."}})
	return m, a
}

func TestEnterpriseFallbackPolicyAdmissionAndMedia(t *testing.T) {
	for _, trial := range []bool{false, true} {
		for _, edition := range []string{"enterprise_plus", "business_standard", "enterprise_standard"} {
			for _, mode := range []string{"auto", "catalog", "custom", "argon_only", "exclude_argon"} {
				t.Run(edition+"/"+mode+map[bool]string{true: "/trial", false: "/paid"}[trial], func(t *testing.T) {
					m, a := catalogFixture(t, edition, trial)
					a.Metadata["model_policy"] = mode
					a.Metadata["catalog_fallback"] = map[string]any{"mode": "custom", "models": []string{ge.ArgonModel, "gemini-3.8-flash", "gemini-3-pro-image", "veo-3.1"}}
					var err error
					a, err = m.Update(t.Context(), a)
					if err != nil {
						t.Fatal(err)
					}
					view := m.EnterpriseModelPolicyView(a)
					if !view.CatalogKnown || view.CatalogState.Source != "custom" || len(view.CatalogModels) != 4 {
						t.Fatalf("%+v", view)
					}
					for _, id := range []string{ge.ArgonModel, "gemini-3.8-flash", "gemini-3-pro-image", "veo-3.1", "gemini-4-future", "auto"} {
						err = ValidateEnterpriseModelRequest(t.Context(), a, id)
						want := slices.Contains(view.EffectiveModels, id)
						if (err == nil) != want || (!trial && id == "gemini-3.8-flash" && err == nil) {
							t.Fatalf("%s admission=%v view=%+v", id, err, view)
						}
					}
					cfg := &config.Config{}
					cfg.DisableImageGeneration = config.DisableImageGenerationAll
					m.SetConfig(cfg)
					if err = ValidateEnterpriseModelRequest(t.Context(), a, "gemini-3-pro-image"); err == nil {
						t.Fatal("local image disablement bypassed")
					}
				})
			}
		}
	}
}

func TestEnterpriseFallbackDefaultTransitionsAndAuthoritativeEmpty(t *testing.T) {
	m, a := catalogFixture(t, "enterprise_plus", true)
	if v := m.EnterpriseModelPolicyView(a); len(v.EffectiveModels) != 6 || v.CatalogState.Source != "default" {
		t.Fatal(v)
	}
	publishSubscription(t, m, a, "business_standard", true)
	if v := m.EnterpriseModelPolicyView(a); len(v.EffectiveModels) != 1 || v.EffectiveModels[0] != ge.ArgonModel {
		t.Fatal(v)
	}
	publishSubscription(t, m, a, "enterprise_plus", false)
	if v := m.EnterpriseModelPolicyView(a); len(v.EffectiveModels) != 1 || v.EffectiveModels[0] != ge.ArgonModel {
		t.Fatal(v)
	}
	m.InvalidateEnterpriseSubscription(a)
	flight := m.BeginEnterpriseSubscriptionRefresh(a)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unassigned", Reason: "license_unassigned"}) {
		t.Fatal("failed to revoke subscription")
	}
	if v := m.EnterpriseModelPolicyView(a); len(v.EffectiveModels) != 0 || v.Status != "blocked" || v.CatalogKnown {
		t.Fatal(v)
	}
	publishSubscription(t, m, a, "enterprise_plus", true)
	m.PublishEnterprisePolicyCatalog(a, []ge.Model{})
	now := time.Now()
	m.PublishEnterpriseCatalogState(a, ge.CatalogState{Source: "cached", Status: "stale", LastError: &ge.MetadataError{Code: "catalog_unavailable"}, CheckedAt: &now})
	if v := m.EnterpriseModelPolicyView(a); !v.CatalogKnown || len(v.CatalogModels) != 0 || v.CatalogState.Source != "cached" {
		t.Fatal("empty success replaced by defaults", v)
	}
	// Subscription stale limits still apply with default and custom candidates.
	m.enterprisePolicies.nowFunc = func() time.Time { return now.Add(31 * time.Minute) }
	if v := m.EnterpriseModelPolicyView(a); len(v.EffectiveModels) != 0 || v.Status != "blocked" {
		t.Fatal(v)
	}
}

func TestEnterpriseCatalogPersistenceRoundTripRevisionAndIdentity(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "account"}[account], func(t *testing.T) {
			store := &enterpriseAccountStore{}
			m := NewManager(store, nil, nil)
			a := regionalTestAuth()
			if account {
				a = enterpriseAccountFixture(t, 2)
			}
			root, err := m.Register(t.Context(), a)
			if err != nil {
				t.Fatal(err)
			}
			a = root
			if account {
				a = m.EnterpriseCollections(root.ID)[0]
			}
			before, _ := ge.ReadAccount(root.Metadata)
			models := []ge.Model{{ID: "gemini-3.8-flash", DisplayName: "Flash"}}
			if err = m.PersistEnterpriseCatalog(t.Context(), a, models, time.Now()); err != nil {
				t.Fatal(err)
			}
			root, _ = m.GetByID(root.ID)
			after, _ := ge.ReadAccount(root.Metadata)
			if account && (before.Revision != after.Revision || before.Collections[0].Revision != after.Collections[0].Revision) {
				t.Fatal("cache changed settings revisions")
			}
			data, _ := json.Marshal(root.Metadata[enterpriseCatalogCacheKey])
			if strings.Contains(string(data), "synthetic-refresh") || strings.Contains(string(data), "synthetic-old") {
				t.Fatal("secret persisted in catalog")
			}
			// Serialize as real stores do, then start a new manager without runtime observations.
			encoded, _ := json.Marshal(root.Metadata)
			_ = json.Unmarshal(encoded, &root.Metadata)
			restarted := NewManager(store, nil, nil)
			loaded, err := restarted.Register(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			a = loaded
			if account {
				a = restarted.EnterpriseCollections(loaded.ID)[0]
			}
			snapshot, ok := restarted.StoredEnterpriseCatalog(a)
			if !ok || len(snapshot.Models) != 1 {
				t.Fatal("restart cache missing", snapshot)
			}
			if restarted.EnterpriseSubscriptionSnapshot(a).Status == "ok" {
				t.Fatal("subscription persisted with catalog")
			}
			rotated := a.Clone()
			rotated.Metadata["access_token"] = "synthetic-renewed"
			if _, ok = restarted.StoredEnterpriseCatalog(rotated); !ok {
				t.Fatal("access refresh invalidated cache")
			}
			rotated.Metadata["refresh_token"] = "synthetic-new-authorization"
			if _, ok = restarted.StoredEnterpriseCatalog(rotated); ok {
				t.Fatal("replaced authorization reused cache")
			}
			rotated = a.Clone()
			rotated.Metadata["engine_id"] = "other-app"
			if _, ok = restarted.StoredEnterpriseCatalog(rotated); ok {
				t.Fatal("resource change reused cache")
			}
			if err = restarted.PersistEnterpriseCatalog(t.Context(), a, []ge.Model{}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if snapshot, ok = restarted.StoredEnterpriseCatalog(a); !ok || snapshot.Models == nil || len(snapshot.Models) != 0 {
				t.Fatal("empty success not persisted")
			}
			store.failID = loaded.ID
			if err = restarted.PersistEnterpriseCatalog(t.Context(), a, models, time.Now()); err == nil {
				t.Fatal("save failure hidden")
			}
			if snapshot, ok = restarted.StoredEnterpriseCatalog(a); !ok || len(snapshot.Models) != 0 {
				t.Fatal("failed write overwrote saved cache")
			}
		})
	}
}

func TestEnterpriseCatalogCorruptionAndFailedRefreshDoNotForgeSuccess(t *testing.T) {
	m, a := catalogFixture(t, "enterprise_plus", true)
	root, _ := m.GetByID(a.ID)
	root.Metadata[enterpriseCatalogCacheKey] = map[string]any{enterprisePolicyCatalogKey(a): map[string]any{"version": 1, "fingerprint": enterprisePolicyCatalogKey(a), "models": "corrupt"}}
	if _, err := m.Update(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.StoredEnterpriseCatalog(a); ok {
		t.Fatal("corrupt snapshot restored")
	}
	for _, mode := range []string{"disabled", "custom"} {
		a.Metadata["catalog_fallback"] = map[string]any{"mode": mode, "models": []string{}}
		v := m.EnterpriseModelPolicyView(a)
		if len(v.EffectiveModels) != 0 {
			t.Fatal(v)
		}
	}
	var err *EnterpriseModelPolicyError
	if !errors.As(m.validateEnterprisePolicy(a, "gemini-3.8-flash", true), &err) {
		t.Fatal("empty custom fallback admitted generation")
	}
}

func TestEnterpriseCatalogCacheWriteSerializesWithSettings(t *testing.T) {
	store := &enterpriseBlockingStore{started: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(store, nil, nil)
	root := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 1))
	child := m.EnterpriseCollections(root.ID)[0]
	store.block.Store(true)
	saved := make(chan error, 1)
	go func() {
		saved <- m.PersistEnterpriseCatalog(t.Context(), child, []ge.Model{{ID: "gemini-3.8-flash"}}, time.Now())
	}()
	<-store.started
	patched := make(chan error, 1)
	go func() {
		_, err := m.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error {
			account.Collections[0].Settings["note"] = "concurrent setting"
			return nil
		})
		patched <- err
	}()
	close(store.release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if err := <-patched; err != nil {
		t.Fatal(err)
	}
	_, graph, _ := m.EnterpriseAccountSnapshot(root.ID)
	if graph.Collections[0].Settings["note"] != "concurrent setting" {
		t.Fatal("settings overwritten by cache persistence")
	}
	current := m.EnterpriseCollections(root.ID)[0]
	if snapshot, ok := m.StoredEnterpriseCatalog(current); !ok || len(snapshot.Models) != 1 {
		t.Fatal("concurrent patch erased cache")
	}
}
