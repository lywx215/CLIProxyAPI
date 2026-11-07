package cliproxy

import (
	"context"
	"slices"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseCarbonRegistrationFollowsManualApplicationAdmission(t *testing.T) {
	for _, trial := range []bool{false, true} {
		t.Run(map[bool]string{false: "paid", true: "trial"}[trial], func(t *testing.T) {
			testEnterpriseCarbonRegistration(t, trial)
		})
	}
}

func testEnterpriseCarbonRegistration(t *testing.T, trial bool) {
	s, auth := enterpriseTestService(t, "carbon-manual-registration")
	d := s.enterpriseState()
	catalogCalls := 0
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		catalogCalls++
		return []ge.Model{{ID: ge.ArgonModel}}, nil
	}
	d.fetchSubscription = func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation {
		return ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial}
	}
	auth.Prefix = "carbon-team"
	auth.Attributes = map[string]string{"model_aliases": `[{"name":"gemini-4-carbon","alias":"carbon-public","fork":true}]`}
	var err error
	auth, err = s.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantCarbon bool) {
		t.Helper()
		s.registerEnterpriseModels(t.Context(), auth)
		d.wg.Wait()
		ids := []string{}
		for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
			ids = append(ids, model.ID)
		}
		if !slices.Contains(ids, "carbon-team/"+ge.ArgonModel) || slices.Contains(ids, "carbon-team/"+ge.CarbonModel) != wantCarbon || slices.Contains(ids, "carbon-team/carbon-public") != wantCarbon {
			t.Fatalf("unexpected publication with Carbon available=%v: %v", wantCarbon, ids)
		}
	}
	check(false)
	fingerprint := s.coreManager.EnterpriseCarbonEvidenceFingerprint(auth)
	auth.Metadata[ge.CarbonEvidenceKey] = ge.CarbonTestEvidence{
		CarbonTestSummary: ge.CarbonTestSummary{TestID: "synthetic-registration-proof", Status: "verified", CheckedAt: time.Now().UTC(), UpstreamModel: ge.CarbonModel, HTTPStatus: 200},
		Version:           1, AuthorizationFingerprint: fingerprint.Authorization, BindingFingerprint: fingerprint.Binding, ConfigFingerprint: fingerprint.Configuration,
	}
	auth, err = s.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	check(false)
	auth.Metadata[ge.CarbonEnabledKey] = true
	auth, err = s.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	check(true)
	view := s.coreManager.EnterpriseModelPolicyView(auth)
	if len(view.CatalogModels) != 2 || view.CatalogModels[1].CatalogSource != "manual" || view.CatalogModels[1].UpstreamListed {
		t.Fatalf("manual admission was confused with the raw catalog: %+v", view.CatalogModels)
	}
	auth.Metadata[ge.CarbonEnabledKey] = false
	auth, err = s.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	check(false)
	if catalogCalls != 1 {
		t.Fatalf("application admission caused %d catalog network operations, want 1 initial discovery", catalogCalls)
	}
}
