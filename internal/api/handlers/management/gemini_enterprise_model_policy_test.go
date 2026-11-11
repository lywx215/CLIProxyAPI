package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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
		{ID: ge.ArgonModel}, {ID: ge.CarbonModel}, {ID: "gemini-3-pro-image", EnterpriseMedia: "image"}, {ID: "veo-3.1", EnterpriseMedia: "video"},
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
				if enforced.Raw != `["gemini-4-argon","gemini-4-carbon","gemini-3-pro-image","veo-3.1"]` || reason != "paid_subscription_restricted" || gjson.GetBytes(w.Body.Bytes(), "models.#").Int() != 3 || !gjson.GetBytes(w.Body.Bytes(), "test_candidates.0.test_candidate").Bool() || gjson.GetBytes(w.Body.Bytes(), "carbon_admission.available").Bool() {
					t.Fatalf("paid state was not enforced: %s", w.Body.String())
				}
			case "trial":
				if enforced.Raw != "null" || reason != "" || len(gjson.GetBytes(w.Body.Bytes(), "model_policy_state.enforced_excluded_models").Array()) != 0 || !gjson.GetBytes(w.Body.Bytes(), "carbon_admission.can_test").Bool() || gjson.GetBytes(w.Body.Bytes(), "carbon_admission.available").Bool() {
					t.Fatalf("trial Carbon should remain closed but independently testable: %s", w.Body.String())
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
				if state == "paid" && (!strings.Contains(response.Body.String(), "Paid Enterprise subscriptions permit only") || !strings.Contains(response.Body.String(), ge.CarbonModel)) {
					t.Fatalf("paid alias probe lost the hard-restriction explanation: %s", response.Body.String())
				}
			}
		})
	}
	for _, field := range []string{"enforced_allowed_models", "enforced_excluded_models", "enforced_reason"} {
		payload := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": 1, "fields": map[string]any{field: nil}}
		response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", payload)
		if response.Code != http.StatusBadRequest || gjson.GetBytes(response.Body.Bytes(), "code").String() != "read_only_collection_fields" {
			t.Fatalf("read-only restriction accepted a patch: %s", response.Body.String())
		}
	}
}

func TestEnterpriseForceArgonCollectionPatchAndProbe(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	child := children[0]
	bindingID := coreauth.EnterpriseBindingID(child)
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(filepath.Dir(root.Attributes["path"]))
	if _, err := store.Save(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	h.authManager.SetStore(store)
	expired := ge.SubscriptionObservation{Status: "inactive", State: "EXPIRED", Reason: "subscription_inactive"}
	publishManagementEnterpriseSubscription(t, h.authManager, child, expired, ge.Model{ID: "gemini-3.8-flash"})
	patch := func(revision uint64, fields map[string]any, want int) gjson.Result {
		t.Helper()
		response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, http.MethodPatch, map[string]any{
			"name": root.FileName, "auth_index": child.Index, "binding_id": bindingID, "expected_revision": revision, "fields": fields,
		})
		if response.Code != want {
			t.Fatalf("patch status=%d, want %d: %s", response.Code, want, response.Body.String())
		}
		return gjson.ParseBytes(response.Body.Bytes()).Get("gemini_enterprise")
	}
	for _, invalid := range []any{"true", nil} {
		patch(1, map[string]any{ge.ArgonForceEnabledKey: invalid}, http.StatusBadRequest)
	}
	account := patch(1, map[string]any{ge.ArgonForceEnabledKey: true}, http.StatusOK)
	collection := account.Get(fmt.Sprintf(`collections.#(binding_id==%q)`, bindingID))
	if !account.Get("argon_force_supported").Bool() || !collection.Get("settings.argon_force_enabled").Bool() ||
		!collection.Get("model_policy_state.argon_force_enabled").Bool() || collection.Get("model_policy_state.subscription.state").String() != "EXPIRED" ||
		collection.Get("model_policy_state.effective_models").Raw != `["gemini-4-argon"]` || collection.Get("revision").Uint() != 2 {
		t.Fatalf("wrong forced Argon view: %s", collection.Raw)
	}
	patch(1, map[string]any{ge.ArgonForceEnabledKey: false}, http.StatusConflict)
	const alias = "manual-argon-alias"
	patch(2, map[string]any{"model_aliases": []map[string]any{{"name": ge.ArgonModel, "alias": alias}}}, http.StatusOK)
	for _, current := range h.authManager.EnterpriseCollections(root.ID) {
		if h.authManager.EnterpriseModelPolicyView(current).ArgonForceEnabled != (current.ID == child.ID) {
			t.Fatal("collection edit lost the switch or leaked it to another region")
		}
	}
	registry.GetGlobalRegistry().RegisterClient(child.ID, ge.Provider, []*registry.ModelInfo{{ID: alias, MetadataModelID: ge.ArgonModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
	h.cfg.OAuthExcludedModels = map[string][]string{ge.Provider: {"gemini-4-*"}}
	h.authManager.SetConfig(h.cfg)
	h.authManager.SetRetryConfig(0, 0, 0)
	calls := 0
	h.authManager.RegisterExecutor(&enterpriseProbeExecutor{execute: func(ctx context.Context, selected *coreauth.Auth, _ coreexec.Request, _ coreexec.Options) (coreexec.Response, error) {
		calls++
		if selected.ID != child.ID {
			t.Fatalf("probe used another collection: %s", selected.ID)
		}
		coreexec.MarkUpstreamAttempt(ctx)
		return coreexec.Response{Payload: []byte(`{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`)}, nil
	}})
	probe := map[string]any{
		"name": root.FileName, "auth_index": child.Index, "binding_id": bindingID, "expected_revision": 3,
		"model": alias, "expected": ge.Resources(ge.FromMetadata(child.Metadata)),
	}
	result := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, http.MethodPost, probe)
	if result.Code != http.StatusOK || gjson.GetBytes(result.Body.Bytes(), "status").String() != "ok" || calls != 1 {
		t.Fatalf("forced Argon probe did not execute: %d %s", result.Code, result.Body.String())
	}

	currentRoot, _, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	currentRoot.Metadata["access_token"] = "synthetic-rotated-access"
	if _, err := h.authManager.ReauthorizeEnterpriseAccount(t.Context(), currentRoot); err != nil {
		t.Fatal(err)
	}
	reloaded := coreauth.NewManager(store, nil, nil)
	if err := reloaded.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if saved, ok := reloaded.GetByID(child.ID); !ok || !reloaded.EnterpriseModelPolicyView(saved).ForcesArgon(ge.ArgonModel) {
		t.Fatal("authorization refresh or credential reload lost the switch")
	}
	stale, _ := h.authManager.GetByID(child.ID)
	patch(3, map[string]any{ge.ArgonForceEnabledKey: false}, http.StatusOK)
	if err := coreauth.CommitEnterpriseModelSubmission(t.Context(), stale, ge.ArgonModel); err == nil {
		t.Fatal("a request holding an older credential snapshot retained forced admission after closing the switch")
	}
}
