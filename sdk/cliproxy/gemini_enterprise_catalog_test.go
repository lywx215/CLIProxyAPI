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

func TestEnterpriseRefreshRotationRetainsCatalogAndPaidSubscription(t *testing.T) {
	for _, account := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "account"}[account], func(t *testing.T) {
			svc, root := enterpriseTestService(t, "enterprise-rotation-"+t.Name())
			apps := []*coreauth.Auth{root}
			if account {
				graph, err := ge.AccountFromLegacy(root.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				resources := graph.Collections[0].ResourceBinding
				resources.Location = "us"
				graph.Collections = append(graph.Collections, ge.Collection{
					ID: ge.CollectionID(resources), Revision: 1, ResourceBinding: resources,
				})
				root.Metadata = ge.PutAccount(root.Metadata, graph)
				root, err = svc.coreManager.Update(t.Context(), root)
				if err != nil {
					t.Fatal(err)
				}
				apps = svc.coreManager.EnterpriseCollections(root.ID)
			}
			d := svc.enterpriseState()
			d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
				return nil, errors.New("directory unavailable during token rotation")
			}
			d.fetchSubscription = func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation {
				return ge.SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable"}
			}
			d.fetchQuota = func(context.Context, *coreauth.Auth, string) ge.QuotaSnapshot {
				return ge.QuotaSnapshot{}
			}
			isListed := func(app *coreauth.Auth) bool {
				return ge.FromMetadata(app.Metadata).Location == "us"
			}
			var leases []*coreauth.EnterpriseSubscriptionRefresh
			for _, app := range apps {
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(app.ID) })
				trial := false
				// Resolve a project name to a number to exercise the canonical subscription key.
				flight := svc.coreManager.BeginEnterpriseSubscriptionRefresh(app)
				if flight == nil || !flight.Complete(ge.SubscriptionObservation{
					Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial, ProjectNumber: "123",
				}) {
					t.Fatal("paid fixture subscription was not published")
				}
				models := []ge.Model{}
				if isListed(app) {
					models = append(models, ge.Model{ID: ge.HydrogenModel})
				}
				if isListed(app) {
					// Keep one result only in memory, as when discovery completed
					// just before the credential write.
					svc.coreManager.PublishEnterprisePolicyCatalog(app, models)
				} else if err := svc.coreManager.PersistEnterpriseCatalog(t.Context(), app, models, time.Now()); err != nil {
					t.Fatal(err)
				}
				svc.coreManager.InvalidateEnterpriseSubscription(app)
				lease := svc.coreManager.BeginEnterpriseSubscriptionRefresh(app)
				if lease == nil {
					t.Fatal("could not start pre-rotation subscription read")
				}
				leases = append(leases, lease)
			}
			base, _ := svc.coreManager.GetByID(root.ID)
			next := base.Clone()
			next.Metadata["access_token"], next.Metadata["refresh_token"] = "new-access", "rotated-refresh"
			rotated, err := svc.coreManager.UpdateRefreshedAuth(t.Context(), base, next)
			if err != nil {
				t.Fatal(err)
			}
			for _, lease := range leases {
				if lease.Complete(ge.SubscriptionObservation{Status: "inactive"}) {
					t.Fatal("the old token's subscription response was accepted")
				}
			}
			for _, first := range apps {
				app, _ := svc.coreManager.GetByID(first.ID)
				expected := isListed(app)
				snapshot, ok := svc.coreManager.StoredEnterpriseCatalog(app)
				if !ok || snapshot.Models == nil || (len(snapshot.Models) == 1) != expected {
					t.Fatalf("successful catalog did not follow the refreshed token: found=%t models=%v", ok, snapshot.Models)
				}
				if view := svc.coreManager.EnterpriseModelPolicyView(app); view.Subscription.Status != "ok" || (isListed(app) && !view.CatalogKnown) {
					t.Fatalf("current policy lost verified data during rotation: %+v", view)
				}
			}
			// The running service must use the carried cache before its periodic
			// discovery. Exercise this entry point before normal registration.
			svc.refreshEnterpriseModels(t.Context())
			d.wg.Wait()
			for _, first := range apps {
				app, _ := svc.coreManager.GetByID(first.ID)
				expected := isListed(app)
				view := svc.coreManager.EnterpriseModelPolicyView(app)
				err = svc.coreManager.ValidateEnterpriseModelCatalog(app, ge.HydrogenModel)
				registered := len(GlobalModelRegistry().GetModelsForClient(app.ID)) == 1
				if view.Subscription.Status != "stale" || view.CatalogState.Source != "cached" || (err == nil) != expected || registered != expected {
					t.Fatalf("temporary upstream failure lost rotated cache: source=%s subscription=%s admission=%v registered=%t", view.CatalogState.Source, view.Subscription.Status, err, registered)
				}
			}
			restarted := coreauth.NewManager(nil, nil, nil)
			loaded, err := restarted.Register(t.Context(), rotated)
			if err != nil {
				t.Fatal(err)
			}
			for _, app := range append([]*coreauth.Auth{loaded}, restarted.EnterpriseCollections(loaded.ID)...) {
				if coreauth.IsEnterpriseAccountAuth(app) {
					continue
				}
				if _, ok := restarted.StoredEnterpriseCatalog(app); !ok || restarted.EnterpriseSubscriptionSnapshot(app).Status != "unknown" {
					t.Fatal("restart lost the rotated catalog or persisted an entitlement")
				}
			}
			replacement := rotated.Clone()
			replacement.Metadata["refresh_token"] = "replacement-authorization"
			if _, err = svc.coreManager.Update(t.Context(), replacement); err != nil {
				t.Fatal(err)
			}
			for _, first := range apps {
				app, _ := svc.coreManager.GetByID(first.ID)
				if _, ok := svc.coreManager.StoredEnterpriseCatalog(app); ok || svc.coreManager.EnterpriseSubscriptionSnapshot(app).Status != "unknown" {
					t.Fatal("a different grant inherited the old data")
				}
			}
		})
	}
}
