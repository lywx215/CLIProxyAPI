package cliproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseFallbackRegistrationRecoveryAndRestart(t *testing.T) {
	svc, a := enterpriseTestService(t, "enterprise-fallback-lifecycle")
	d := svc.enterpriseState()
	now := time.Now().Add(-time.Hour)
	d.now = func() time.Time { return now }
	fail := true
	empty := false
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		if fail {
			return nil, errors.New("synthetic directory failure")
		}
		if empty {
			return []ge.Model{}, nil
		}
		return []ge.Model{{ID: "gemini-3.8-flash"}}, nil
	}
	svc.registerEnterpriseModels(t.Context(), a)
	d.wg.Wait()
	if v := svc.coreManager.EnterpriseModelPolicyView(a); v.CatalogState.Source != "default" || len(GlobalModelRegistry().GetModelsForClient(a.ID)) != 6 {
		t.Fatalf("default not registered: %+v", v)
	}
	if _, ok := svc.coreManager.StoredEnterpriseCatalog(a); ok {
		t.Fatal("fallback persisted as upstream success")
	}
	fail = false
	now = now.Add(6 * time.Minute)
	svc.refreshEnterpriseModels(t.Context())
	d.wg.Wait()
	if v := svc.coreManager.EnterpriseModelPolicyView(a); v.CatalogState.Source != "upstream" || len(v.CatalogModels) != 1 {
		t.Fatal(v)
	}
	root, _ := svc.coreManager.GetByID(a.ID)
	fail = true
	now = now.Add(6 * time.Minute)
	svc.refreshEnterpriseModels(t.Context())
	d.wg.Wait()
	if v := svc.coreManager.EnterpriseModelPolicyView(a); v.CatalogState.Source != "cached" || len(v.CatalogModels) != 1 {
		t.Fatal(v)
	}
	// Restart without runtime policy state. The saved catalog cannot authorize a subscription.
	m := coreauth.NewManager(nil, nil, nil)
	restored, err := m.Register(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	other := &Service{cfg: svc.cfg, coreManager: m, antigravityContext: t.Context()}
	other.enterpriseState().fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return nil, errors.New("synthetic restart metadata failure")
	}
	other.restoreEnterpriseCatalog(restored)
	if v := m.EnterpriseModelPolicyView(restored); v.Status != "blocked" || len(v.EffectiveModels) != 0 || v.CatalogState.Source != "cached" {
		t.Fatal(v)
	}
	publishEnterpriseTrialFixture(t, m, restored)
	other.registerEnterpriseModels(t.Context(), restored)
	other.cancelEnterpriseProbes("")
	other.enterpriseState().wg.Wait()
	if got := GlobalModelRegistry().GetModelsForClient(restored.ID); len(got) != 1 || got[0].ID != "gemini-3.8-flash" {
		t.Fatal(got)
	}
	// A subsequent successful empty response remains authoritative through failure.
	fail = false
	empty = true
	now = now.Add(6 * time.Minute)
	svc.refreshEnterpriseModels(t.Context())
	d.wg.Wait()
	fail = true
	now = now.Add(6 * time.Minute)
	svc.refreshEnterpriseModels(t.Context())
	d.wg.Wait()
	if v := svc.coreManager.EnterpriseModelPolicyView(a); !v.CatalogKnown || len(v.CatalogModels) != 0 || len(GlobalModelRegistry().GetModelsForClient(a.ID)) != 0 {
		t.Fatal("empty response replaced", v)
	}
}
