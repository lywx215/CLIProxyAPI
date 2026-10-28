package management

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnterpriseCatalogV8PatchCapabilitiesAndReadOnlyCache(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	var child *coreauth.Auth
	for _, a := range children {
		if !a.Disabled {
			child = a
			break
		}
	}
	if child == nil {
		t.Fatal("no enabled collection")
	}
	h.authManager.PublishEnterpriseCatalogState(child, ge.CatalogState{Source: "none", Status: "unavailable", LastError: &ge.MetadataError{Code: "service_disabled"}})
	patch := func(fields map[string]any, want int) {
		_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
		var revision uint64
		for _, c := range graph.Collections {
			if c.ID == coreauth.EnterpriseBindingID(child) {
				revision = c.Revision
			}
		}
		body, _ := json.Marshal(map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": revision, "fields": fields})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(ConfigV8ContextKey, true)
		c.Request = httptest.NewRequest("PATCH", "/v8/management/credentials/gemini-enterprise/collections", bytes.NewReader(body))
		h.PatchGeminiEnterpriseCollection(c)
		if w.Code != want {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
		child, _ = h.authManager.GetByID(child.ID)
	}
	patch(map[string]any{"catalog_fallback": map[string]any{"mode": "custom", "models": []string{"gemini-3-pro-image", "veo-3.1"}}, "excluded_models": []string{"veo-*"}}, 200)
	v := h.authManager.EnterpriseModelPolicyView(child)
	if v.CatalogState.Source != "custom" || len(v.EffectiveModels) != 1 || v.EffectiveModels[0] != "gemini-3-pro-image" {
		t.Fatal(v)
	}
	patch(map[string]any{"catalog_fallback": map[string]any{"mode": "custom", "models": []string{"veo-*"}}}, 400)
	for _, field := range []string{"catalog_state", "enterprise_catalog_cache", "catalog_fallback_supported"} {
		patch(map[string]any{field: map[string]any{}}, 400)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ConfigV8ContextKey, true)
	c.Request = httptest.NewRequest("GET", "/v8/management/credentials/models?name="+root.FileName+"&auth_index="+child.Index+"&binding_id="+coreauth.EnterpriseBindingID(child), nil)
	h.GetAuthFileModels(c)
	if w.Code != 200 || !gjson.GetBytes(w.Body.Bytes(), "catalog_fallback_supported").Bool() || gjson.GetBytes(w.Body.Bytes(), "model_policy_state.catalog_state.source").String() != "custom" {
		t.Fatal(w.Body.String())
	}
	for _, entry := range gjson.GetBytes(w.Body.Bytes(), "catalog_models").Array() {
		if entry.Get("upstream_listed").Bool() {
			t.Fatal("fallback mislabeled as upstream")
		}
	}
	patch(map[string]any{"catalog_fallback": nil}, 200)
	if v = h.authManager.EnterpriseModelPolicyView(child); v.CatalogState.Source != "default" || len(v.CatalogModels) != 6 {
		t.Fatal(v)
	}
}
