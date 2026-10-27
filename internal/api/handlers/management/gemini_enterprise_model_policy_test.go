package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func publishManagementEnterpriseTrial(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth, models ...ge.Model) {
	t.Helper()
	trial := true
	publishManagementEnterpriseSubscription(t, manager, auth, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()}, models...)
}

func publishManagementEnterpriseSubscription(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth, observation ge.SubscriptionObservation, models ...ge.Model) {
	t.Helper()
	manager.InvalidateEnterpriseSubscription(auth)
	flight := manager.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil || !flight.Complete(observation) {
		t.Fatal("could not publish the synthetic runtime subscription")
	}
	if models != nil {
		manager.PublishEnterprisePolicyCatalog(auth, models)
	}
}

func TestEnterpriseModelPolicyProbePreservesLocalRejection(t *testing.T) {
	for _, rejection := range []*coreauth.EnterpriseModelPolicyError{
		{Code: "model_not_allowed", Message: "The collection policy does not allow this model", HTTPStatus: http.StatusBadRequest},
		{Code: "subscription_unverified", Message: "The collection subscription is not confirmed", HTTPStatus: http.StatusServiceUnavailable},
	} {
		result := enterpriseModelTestError(enterpriseModelTestResult{ElapsedMS: 123}, fmt.Errorf("final model check: %w", rejection))
		if result.Code != rejection.Code || result.Message != rejection.Message || result.HTTPStatus != rejection.HTTPStatus || result.ElapsedMS != 0 || result.Status != "failed" {
			t.Fatalf("local rejection became an upstream failure: %+v", result)
		}
	}
}

func TestEnterpriseModelPolicyCollectionPatchAndCatalogResponse(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	var child *coreauth.Auth
	for _, entry := range children {
		if !entry.Disabled {
			child = entry
			break
		}
	}
	if child == nil {
		t.Fatal("missing enabled fixture collection")
	}
	h.authManager.PublishEnterprisePolicyCatalog(child, []ge.Model{{ID: ge.ArgonModel}, {ID: "gemini-3.8-flash"}})
	patch := func(fields map[string]any, status int) {
		_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
		var revision uint64
		for _, collection := range graph.Collections {
			if collection.ID == coreauth.EnterpriseBindingID(child) {
				revision = collection.Revision
			}
		}
		payload, _ := json.Marshal(map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": revision, "fields": fields})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(ConfigV8ContextKey, true)
		c.Request = httptest.NewRequest("PATCH", "/v8/management/credentials/gemini-enterprise/collections", bytes.NewReader(payload))
		h.PatchGeminiEnterpriseCollection(c)
		if w.Code != status {
			t.Fatalf("patch status=%d body=%s", w.Code, w.Body.String())
		}
		current, _ := h.authManager.GetByID(child.ID)
		child = current
	}
	patch(map[string]any{"model_policy": "custom", "allowed_models": []string{}, "excluded_models": []string{"veo-*"}}, 200)
	if view := h.authManager.EnterpriseModelPolicyView(child); len(view.EffectiveModels) != 0 || view.AllowedModels == nil {
		t.Fatalf("empty whitelist lost: %+v", view)
	}
	patch(map[string]any{"model_policy": "argon_only"}, 200)
	view := h.authManager.EnterpriseModelPolicyView(child)
	if len(view.EffectiveModels) != 1 || view.EffectiveModels[0] != ge.ArgonModel {
		t.Fatalf("manual policy not applied: %+v", view)
	}
	data, _ := json.Marshal(child.Metadata["allowed_models"])
	if string(data) != "[]" {
		t.Fatalf("dormant whitelist overwritten: %s", data)
	}
	patch(map[string]any{"allowed_models": []string{"gemini-*"}}, 400)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ConfigV8ContextKey, true)
	c.Request = httptest.NewRequest("GET", "/v8/management/credentials/models?name="+root.FileName+"&auth_index="+child.Index+"&binding_id="+coreauth.EnterpriseBindingID(child), nil)
	h.GetAuthFileModels(c)
	var response struct {
		Supported bool                             `json:"model_policy_supported"`
		Catalog   []coreauth.EnterprisePolicyModel `json:"catalog_models"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || !response.Supported || len(response.Catalog) != 2 {
		t.Fatalf("catalog response=%s", w.Body.String())
	}
	if response.Catalog[1].Allowed {
		t.Fatal("blocked upstream model was advertised as allowed")
	}
}

func TestEnterprisePaidPolicyV8ReadOnlyStateAndProbe(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	child := children[0]
	catalog := []ge.Model{{ID: ge.ArgonModel}, {ID: "gemini-3-pro-image", MediaKind: "image"}, {ID: "veo-3.1", MediaKind: "video"}, {ID: "gemini-3.8-flash"}}
	registry.GetGlobalRegistry().RegisterClient(child.ID, ge.Provider, []*registry.ModelInfo{
		{ID: ge.ArgonModel}, {ID: "gemini-3-pro-image", EnterpriseMedia: "image"}, {ID: "veo-3.1", EnterpriseMedia: "video"},
		{ID: "paid-team/flash(high)", MetadataModelID: "gemini-3.8-flash"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
	for _, state := range []string{"paid", "trial", "blocked"} {
		t.Run(state, func(t *testing.T) {
			trial := state == "trial"
			observation := ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()}
			if state == "blocked" {
				observation = ge.SubscriptionObservation{Status: "unassigned", Reason: "license_unassigned"}
			}
			publishManagementEnterpriseSubscription(t, h.authManager, child, observation, catalog...)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set(ConfigV8ContextKey, true)
			c.Request = httptest.NewRequest("GET", "/v8/management/credentials/models?name="+root.FileName+"&auth_index="+child.Index+"&binding_id="+coreauth.EnterpriseBindingID(child), nil)
			h.GetAuthFileModels(c)
			if w.Code != http.StatusOK {
				t.Fatalf("state response: %s", w.Body.String())
			}
			enforced := gjson.GetBytes(w.Body.Bytes(), "model_policy_state.enforced_allowed_models")
			reason := gjson.GetBytes(w.Body.Bytes(), "model_policy_state.enforced_reason").String()
			switch state {
			case "paid":
				if enforced.Raw != `["gemini-4-argon","gemini-3-pro-image","veo-3.1"]` || reason != "paid_subscription_restricted" || gjson.GetBytes(w.Body.Bytes(), "models.#").Int() != 3 {
					t.Fatalf("paid state was not enforced: %s", w.Body.String())
				}
			case "trial":
				if enforced.Raw != "null" || reason != "" {
					t.Fatalf("trial hard restriction changed: %s", w.Body.String())
				}
			case "blocked":
				if enforced.Raw != "[]" || reason != "license_unassigned" || gjson.GetBytes(w.Body.Bytes(), "models.#").Int() != 0 {
					t.Fatalf("blocked state was not empty: %s", w.Body.String())
				}
			}
			probe := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": 1, "model": "paid-team/flash(high)", "expected": ge.Resources(ge.FromMetadata(child.Metadata))}
			if state != "trial" {
				response := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", probe)
				if response.Code != http.StatusConflict {
					t.Fatalf("restricted alias probe: %s", response.Body.String())
				}
				if state == "paid" && !strings.Contains(response.Body.String(), "Paid Enterprise subscriptions permit only") {
					t.Fatalf("paid alias probe lost the hard-restriction explanation: %s", response.Body.String())
				}
			}
		})
	}
	for _, field := range []string{"enforced_allowed_models", "enforced_reason"} {
		payload := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": 1, "fields": map[string]any{field: nil}}
		response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", payload)
		if response.Code != http.StatusBadRequest || gjson.GetBytes(response.Body.Bytes(), "code").String() != "read_only_collection_fields" {
			t.Fatalf("read-only restriction accepted a patch: %s", response.Body.String())
		}
	}
}
