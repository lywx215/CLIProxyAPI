package management

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnterpriseCarbonCredentialModelsFollowEffectiveAdmission(t *testing.T) {
	for _, scenario := range []string{"available", "closed", "missing-proof", "excluded", "incomplete-proof"} {
		t.Run(scenario, func(t *testing.T) {
			h, root, children := enterpriseAccountFixture(t)
			bindingID := coreauth.EnterpriseBindingID(children[0])
			root, err := h.authManager.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error {
				account.Collections[0].Settings["model_policy"] = "exclude_argon"
				account.Collections[0].Settings["prefix"] = "team"
				if scenario == "missing-proof" {
					account.Collections[0].Settings[ge.HydrogenEnabledKey] = true
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			child, _ := h.authManager.GetByID(children[0].ID)
			catalog := []ge.Model{{ID: ge.ArgonModel}}
			models := []*registry.ModelInfo{}
			for i := range 8 {
				id := fmt.Sprintf("gemini-fixture-%d", i)
				catalog = append(catalog, ge.Model{ID: id})
				models = append(models, &registry.ModelInfo{ID: "team/" + id, MetadataModelID: id, Type: "gemini"})
			}
			h.authManager.PublishEnterprisePolicyCatalog(child, catalog)
			if scenario != "missing-proof" {
				owner, account, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
				revision := account.Collections[0].Revision
				pending, err := h.authManager.BeginEnterpriseHydrogenTest(child, revision)
				if err != nil {
					t.Fatal(err)
				}
				_, err = h.authManager.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}})
				if err != nil {
					t.Fatal(err)
				}
				child, _ = h.authManager.GetByID(child.ID)
				if scenario != "closed" {
					if _, err = h.authManager.UpdateEnterpriseHydrogenEnabled(t.Context(), child, revision, true, pending.TestID); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "excluded" {
					owner, _, _ = h.authManager.EnterpriseAccountSnapshot(owner.ID)
					if _, err = h.authManager.UpdateEnterpriseAccount(t.Context(), owner, 0, func(account *ge.Account) error {
						account.Collections[0].Settings["excluded_models"] = []string{ge.HydrogenModel}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "incomplete-proof" {
					child, _ = h.authManager.GetByID(child.ID)
					_, account, _ = h.authManager.EnterpriseAccountSnapshot(owner.ID)
					pending, err = h.authManager.BeginEnterpriseHydrogenTest(child, account.Collections[0].Revision)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = h.authManager.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "incomplete", Reason: "model_verification_incomplete", HTTPStatus: 502}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			child, _ = h.authManager.GetByID(child.ID)
			// Deliberately include stale Carbon registry rows in rejected cases. The
			// read API must never promote a test candidate into business capability.
			models = append(models,
				&registry.ModelInfo{ID: "team/" + ge.HydrogenModel, MetadataModelID: ge.HydrogenModel, Type: "gemini"},
				&registry.ModelInfo{ID: "team/carbon-public", MetadataModelID: ge.HydrogenModel, Type: "gemini"},
			)
			registry.GetGlobalRegistry().RegisterClient(child.ID, ge.Provider, models)
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Set(ConfigV8ContextKey, true)
			ctx.Request = httptest.NewRequest("GET", "/?name="+root.FileName+"&auth_index="+child.Index+"&binding_id="+bindingID, nil)
			h.GetAuthFileModels(ctx)
			body := recorder.Body.Bytes()
			available := scenario == "available" || scenario == "missing-proof" || scenario == "incomplete-proof"
			wantModels := 8
			if available {
				wantModels = len(models)
			}
			if recorder.Code != 200 || int(gjson.GetBytes(body, "models.#").Int()) != wantModels || gjson.GetBytes(body, "hydrogen_admission.available").Bool() != available || gjson.GetBytes(body, "test_candidates.1.business_registered").Bool() != available {
				t.Fatalf("credential models diverged from effective Carbon capability: %d %s", recorder.Code, recorder.Body.String())
			}
			carbonRows := 0
			for _, model := range gjson.GetBytes(body, "models").Array() {
				if model.Get("upstream_model").String() != ge.HydrogenModel {
					continue
				}
				carbonRows++
				if !model.Get("policy_allowed").Bool() || model.Get("catalog_source").String() != "manual" || model.Get("upstream_listed").Bool() {
					t.Fatalf("the manually admitted Carbon model was mislabeled: %s", model.Raw)
				}
			}
			if available && carbonRows != 2 || !available && carbonRows != 0 {
				t.Fatalf("native and alias Carbon business rows=%d; available=%v", carbonRows, available)
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
