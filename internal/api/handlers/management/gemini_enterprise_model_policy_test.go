package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

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
