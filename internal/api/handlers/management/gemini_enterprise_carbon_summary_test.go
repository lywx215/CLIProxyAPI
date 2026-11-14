package management

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnterpriseHydrogenCredentialModelsFollowEffectivePolicy(t *testing.T) {
	for _, scenario := range []struct {
		name, mode                            string
		trial, listed, disabled, legacy, want bool
	}{
		{name: "paid-old-off", listed: true, want: true},
		{name: "paid-old-on", listed: true, legacy: true, want: true},
		{name: "unlisted", legacy: true},
		{name: "trial", listed: true, trial: true, legacy: true},
		{name: "disabled", listed: true, disabled: true},
		{name: "policy-excluded", listed: true, mode: "exclude_argon"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, root, children := enterpriseAccountFixture(t)
			bindingID := coreauth.EnterpriseBindingID(children[0])
			_, err := h.authManager.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error {
				collection := &account.Collections[0]
				collection.Disabled = scenario.disabled
				collection.Settings["prefix"] = "team"
				collection.Settings[ge.HydrogenEnabledKey] = scenario.legacy
				if scenario.mode != "" {
					collection.Settings["model_policy"] = scenario.mode
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			child, _ := h.authManager.GetByID(children[0].ID)
			catalog := []ge.Model{{ID: ge.ArgonModel}}
			if scenario.listed {
				catalog = append(catalog, ge.Model{ID: ge.HydrogenModel})
			}
			publishManagementEnterpriseSubscription(t, h.authManager, child, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &scenario.trial}, catalog...)
			// Stale registrations cannot bypass the live policy or a missing catalog entry.
			registry.GetGlobalRegistry().RegisterClient(child.ID, ge.Provider, []*registry.ModelInfo{
				{ID: "team/" + ge.HydrogenModel, MetadataModelID: ge.HydrogenModel, Type: "gemini"},
				{ID: "team/hydrogen-public", MetadataModelID: ge.HydrogenModel, Type: "gemini"},
			})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Set(ConfigV8ContextKey, true)
			ctx.Request = httptest.NewRequest("GET", "/?name="+root.FileName+"&auth_index="+child.Index+"&binding_id="+bindingID, nil)
			h.GetAuthFileModels(ctx)
			body := recorder.Body.Bytes()
			models := gjson.GetBytes(body, "models").Array()
			if recorder.Code != 200 || (len(models) == 2) != scenario.want {
				t.Fatalf("unexpected model list: %d %s", recorder.Code, body)
			}
			for _, model := range models {
				if model.Get("upstream_model").String() != ge.HydrogenModel || !model.Get("upstream_listed").Bool() {
					t.Fatalf("wrong catalog provenance: %s", model.Raw)
				}
			}
			for _, candidate := range gjson.GetBytes(body, "test_candidates").Array() {
				if candidate.Get("id").String() == ge.HydrogenModel {
					t.Fatal("obsolete Hydrogen test candidate remains exposed")
				}
			}
			if gjson.GetBytes(body, "hydrogen_admission_supported").Exists() || gjson.GetBytes(body, "model_policy_state.hydrogen_admission_supported").Exists() {
				t.Fatal("obsolete Hydrogen switch capability remains exposed")
			}
		})
	}
}

type enterpriseCarbonPublicFence struct{ authorization, registration uint64 }

func enterpriseCarbonSummaryFence(t *testing.T, h *Handler, auth *coreauth.Auth) enterpriseCarbonPublicFence {
	t.Helper()
	encoded, err := json.Marshal(h.enterpriseAccountSummary(auth))
	if err != nil {
		t.Fatal(err)
	}
	revision := gjson.GetBytes(encoded, "carbon_authorization_revision")
	epoch := gjson.GetBytes(encoded, "carbon_registration_epoch")
	if !revision.Exists() || !epoch.Exists() {
		t.Fatalf("the root Carbon authorization fence was omitted: %s", encoded)
	}
	return enterpriseCarbonPublicFence{revision.Uint(), epoch.Uint()}
}

func TestEnterpriseAccountCarbonPublicFenceTracksAuthorizationInsteadOfEvidence(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	initial := enterpriseCarbonSummaryFence(t, h, root)
	if initial.authorization != 0 || initial.registration != root.RegistrationEpoch || initial.registration == 0 || enterpriseCarbonSummaryFence(t, h, children[0]) != initial {
		t.Fatal("the initial zero authorization revision or owner registration epoch was lost")
	}
	_, before, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	paid := false
	publishManagementEnterpriseSubscription(t, h.authManager, children[0], ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &paid}, ge.Model{ID: ge.HydrogenModel})
	pending, err := h.authManager.BeginEnterpriseHydrogenTest(children[0], before.Collections[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.authManager.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}}); err != nil {
		t.Fatal(err)
	}
	current, after, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if after.Revision <= before.Revision || enterpriseCarbonSummaryFence(t, h, current) != initial {
		t.Fatal("evidence save changed the authorization fence or did not advance the ordinary account revision")
	}
	patched := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", map[string]any{
		"name": root.FileName, "auth_index": children[0].Index, "binding_id": coreauth.EnterpriseBindingID(children[0]), "expected_revision": after.Collections[0].Revision,
		"fields": map[string]any{"note": "unrelated local note"},
	})
	if patched.Code != 200 || !gjson.GetBytes(patched.Body.Bytes(), "gemini_enterprise.carbon_authorization_revision").Exists() || gjson.GetBytes(patched.Body.Bytes(), "gemini_enterprise.carbon_authorization_revision").Uint() != initial.authorization || gjson.GetBytes(patched.Body.Bytes(), "gemini_enterprise.carbon_registration_epoch").Uint() != initial.registration {
		t.Fatalf("PATCH omitted or advanced the authorization fence: %d %s", patched.Code, patched.Body.String())
	}
	current, _, _ = h.authManager.EnterpriseAccountSnapshot(root.ID)
	refreshed := current.Clone()
	refreshed.Metadata["access_token"], refreshed.Metadata["refresh_token"] = "synthetic-refreshed-access", "synthetic-trusted-refresh"
	if _, err = h.authManager.UpdateRefreshedAuth(t.Context(), current, refreshed); err != nil {
		t.Fatal(err)
	}
	current, _, _ = h.authManager.EnterpriseAccountSnapshot(root.ID)
	if enterpriseCarbonSummaryFence(t, h, current) != initial {
		t.Fatal("trusted OAuth refresh changed the authorization fence")
	}
	if _, err = h.authManager.ReauthorizeEnterpriseAccount(t.Context(), current.Clone()); err != nil {
		t.Fatal(err)
	}
	current, _, _ = h.authManager.EnterpriseAccountSnapshot(root.ID)
	reauthorized := enterpriseCarbonSummaryFence(t, h, current)
	if reauthorized.authorization <= initial.authorization {
		t.Fatal("explicit same-material reauthorization retained the old fence")
	}
	replacement := current.Clone()
	replacement.Metadata["refresh_token"] = "synthetic-manual-replacement"
	if _, err = h.authManager.Update(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	current, _, _ = h.authManager.EnterpriseAccountSnapshot(root.ID)
	replaced := enterpriseCarbonSummaryFence(t, h, current)
	if replaced.authorization <= reauthorized.authorization {
		t.Fatal("manual authorization replacement retained the old fence")
	}
	h.authManager.Remove(t.Context(), current.ID)
	current, err = h.authManager.Register(t.Context(), current)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt := enterpriseCarbonSummaryFence(t, h, current); rebuilt.registration <= replaced.registration {
		t.Fatal("recreating the account reused an old registration fence")
	}
	list := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(list)
	ctx.Set(ConfigV8ContextKey, true)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	h.ListAuthFiles(ctx)
	if list.Code != 200 || !gjson.GetBytes(list.Body.Bytes(), "files.0.gemini_enterprise.carbon_authorization_revision").Exists() || !gjson.GetBytes(list.Body.Bytes(), "files.0.gemini_enterprise.carbon_registration_epoch").Exists() {
		t.Fatal("the account list omitted the root authorization fence")
	}
}
