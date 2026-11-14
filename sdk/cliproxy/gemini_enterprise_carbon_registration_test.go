package cliproxy

import (
	"context"
	"slices"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseHydrogenRegistrationUsesSubscriptionAndCatalog(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		trial, listed bool
	}{
		{"paid-listed", false, true},
		{"paid-unlisted", false, false},
		{"trial-listed", true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, auth := enterpriseTestService(t, "hydrogen-policy-registration")
			d := s.enterpriseState()
			catalogCalls := 0
			d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
				catalogCalls++
				models := []ge.Model{{ID: ge.ArgonModel}}
				if scenario.listed {
					models = append(models, ge.Model{ID: ge.HydrogenModel})
				}
				return models, nil
			}
			auth.Prefix = "team"
			auth.Attributes = map[string]string{"model_aliases": `[{"name":"gemini-4-hydrogen","alias":"hydrogen-public","fork":true}]`}
			var err error
			auth, err = s.coreManager.Update(t.Context(), auth)
			if err != nil {
				t.Fatal(err)
			}
			flight := s.coreManager.BeginEnterpriseSubscriptionRefresh(auth)
			if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &scenario.trial}) {
				t.Fatal("verified fixture subscription was not published")
			}
			for _, legacy := range []any{nil, false, true} {
				if legacy == nil {
					delete(auth.Metadata, ge.HydrogenEnabledKey)
				} else {
					auth.Metadata[ge.HydrogenEnabledKey] = legacy
				}
				auth, err = s.coreManager.Update(t.Context(), auth)
				if err != nil {
					t.Fatal(err)
				}
				s.registerEnterpriseModels(t.Context(), auth)
				d.wg.Wait()
				var ids []string
				for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
					ids = append(ids, model.ID)
				}
				want := scenario.listed && !scenario.trial
				if !slices.Contains(ids, "team/"+ge.ArgonModel) || slices.Contains(ids, "team/"+ge.HydrogenModel) != want || slices.Contains(ids, "team/hydrogen-public") != want {
					t.Fatalf("unexpected publication with historical flag %v: %v", legacy, ids)
				}
				if want {
					view := s.coreManager.EnterpriseModelPolicyView(auth)
					if len(view.CatalogModels) != 2 || view.CatalogModels[1].CatalogSource != "upstream" || !view.CatalogModels[1].UpstreamListed {
						t.Fatalf("listed Hydrogen lost upstream catalog attribution: %+v", view.CatalogModels)
					}
				}
			}
			if catalogCalls != 1 {
				t.Fatalf("historical flags caused %d discoveries, want one", catalogCalls)
			}
		})
	}
}
