package management

import (
	"encoding/json"
	"net/http"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestEnterpriseAccountRoutingDefaultsAndCollectionOverrides(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	inheriting, overriding := children[0], children[2]
	childPatch := func(child *coreauth.Auth, revision uint64, fields map[string]any, want int) {
		t.Helper()
		recorder := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, http.MethodPatch, map[string]any{
			"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child),
			"expected_revision": revision, "fields": fields,
		})
		if recorder.Code != want {
			t.Fatalf("collection patch HTTP %d, want %d: %s", recorder.Code, want, recorder.Body)
		}
	}
	currentCollection := func(child *coreauth.Auth) *coreauth.Auth {
		t.Helper()
		current, ok := h.authManager.GetByID(child.ID)
		if !ok {
			t.Fatal("collection disappeared")
		}
		return current
	}
	childPatch(overriding, 1, map[string]any{"priority": 0, "weight": 5}, http.StatusOK)
	_, graph, _ = h.authManager.EnterpriseAccountSnapshot(root.ID)
	accountPatch := map[string]any{
		"name": root.FileName, "auth_index": root.Index, "account_id": graph.ID,
		"expected_revision": graph.Revision, "fields": map[string]any{"priority": -1, "weight": 2},
	}
	response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseAccountRouting, http.MethodPatch, accountPatch)
	if response.Code != http.StatusOK {
		t.Fatalf("account routing patch HTTP %d: %s", response.Code, response.Body)
	}
	var body struct {
		Enterprise struct {
			Routing struct {
				Priority int   `json:"priority"`
				Weight   int64 `json:"weight"`
			} `json:"routing"`
			Collections []struct {
				ID       string `json:"binding_id"`
				Revision uint64 `json:"revision"`
				Routing  struct {
					Priority          int   `json:"priority"`
					Weight            int64 `json:"weight"`
					PriorityInherited bool  `json:"priority_inherited"`
					WeightInherited   bool  `json:"weight_inherited"`
				} `json:"routing"`
			} `json:"collections"`
		} `json:"gemini_enterprise"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := body.Enterprise.Routing; got.Priority != -1 || got.Weight != 2 {
		t.Fatalf("account defaults = %+v", got)
	}
	for _, collection := range body.Enterprise.Collections {
		got := collection.Routing
		if collection.ID == coreauth.EnterpriseBindingID(overriding) {
			if got.Priority != 0 || got.Weight != 5 || got.PriorityInherited || got.WeightInherited || collection.Revision != 2 {
				t.Fatalf("explicit override changed: %+v", collection)
			}
		} else if got.Priority != -1 || got.Weight != 2 || !got.PriorityInherited || !got.WeightInherited || collection.Revision != 2 {
			t.Fatalf("default not inherited: %+v", collection)
		}
	}

	// The updated values must reach actual selection before the account is reloaded.
	eligible := []*coreauth.Auth{currentCollection(inheriting), currentCollection(overriding)}
	selected, err := (&coreauth.RoundRobinSelector{}).Pick(t.Context(), ge.Provider, "fixture", coreexec.Options{}, eligible)
	if err != nil || selected == nil || selected.ID != overriding.ID {
		t.Fatalf("lower priority was not used for fallback: picked %v, %v", selected != nil, err)
	}
	if stale := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseAccountRouting, http.MethodPatch, accountPatch); stale.Code != http.StatusConflict {
		t.Fatalf("stale account patch HTTP %d", stale.Code)
	}
	childPatch(inheriting, 1, map[string]any{"priority": 4}, http.StatusConflict)
	childPatch(overriding, 2, map[string]any{"priority": nil, "weight": nil}, http.StatusOK)
	if priority, weight := coreauth.CredentialRoutingValues(currentCollection(overriding)); priority != -1 || weight != 2 {
		t.Fatalf("restored inheritance = %d / %d", priority, weight)
	}
	if !currentCollection(children[1]).Disabled {
		t.Fatal("routing settings enabled a disabled application")
	}

	// A newly discovered collection inherits the saved account defaults.
	root, graph, _ = h.authManager.EnterpriseAccountSnapshot(root.ID)
	resources := graph.Collections[0].ResourceBinding
	resources.Location = "ca"
	binding := ge.CollectionID(resources)
	if _, err = h.authManager.UpdateEnterpriseAccount(t.Context(), root, graph.Revision, func(next *ge.Account) error {
		next.Collections = append(next.Collections, ge.Collection{ID: binding, ResourceBinding: resources, Revision: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, child := range h.authManager.EnterpriseCollections(root.ID) {
		if coreauth.EnterpriseBindingID(child) == binding {
			if priority, weight := coreauth.CredentialRoutingValues(child); priority != -1 || weight != 2 {
				t.Fatalf("new collection routing = %d / %d", priority, weight)
			}
			return
		}
	}
	t.Fatal("new collection was not projected")
}
