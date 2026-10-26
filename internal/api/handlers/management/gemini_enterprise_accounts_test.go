package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"golang.org/x/oauth2"
)

func enterpriseAccountFixture(t *testing.T) (*Handler, *coreauth.Auth, []*coreauth.Auth) {
	t.Helper()
	metadata := ge.Metadata((config.GeminiEnterpriseOAuth{ClientID: "collection-fixture", ProjectID: "123", EngineID: "app"}).WithDefaults(), "account@example.test", &oauth2.Token{AccessToken: "synthetic-private-access", RefreshToken: "synthetic-private-refresh", Expiry: time.Now().Add(time.Hour)})
	graph, err := ge.AccountFromLegacy(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{"us", "eu"} {
		resources := graph.Collections[0].ResourceBinding
		resources.Location = region
		graph.Collections = append(graph.Collections, ge.Collection{ID: ge.CollectionID(resources), Revision: 1, ResourceBinding: resources})
	}
	graph.Collections[1].Disabled = true
	metadata = ge.PutAccount(metadata, graph)
	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	root, err := manager.Register(t.Context(), &coreauth.Auth{ID: "collection-account.json", FileName: "collection-account.json", Provider: ge.Provider, Metadata: metadata, Attributes: map[string]string{"path": path}, Status: coreauth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	children := manager.EnterpriseCollections(root.ID)
	if len(children) != 3 {
		t.Fatalf("missing projected collections: %d", len(children))
	}
	return &Handler{authManager: manager, cfg: &config.Config{}}, root, children
}

func enterpriseManagementRequest(t *testing.T, handler gin.HandlerFunc, method string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Handle(method, "/", handler)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/", strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestEnterpriseAccountListIsNestedAndContainsNoOAuthMaterial(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	for _, query := range []string{"", "?page=1&page_size=1"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set(ConfigV8ContextKey, true)
		c.Request = httptest.NewRequest("GET", "/"+query, nil)
		h.ListAuthFiles(c)
		var body struct {
			Files []struct {
				Index      string `json:"auth_index"`
				Enterprise struct {
					ID          string `json:"account_id"`
					Collections []struct {
						ID       string `json:"binding_id"`
						Index    string `json:"auth_index"`
						Disabled bool   `json:"disabled"`
					} `json:"collections"`
				} `json:"gemini_enterprise"`
			} `json:"files"`
		}
		if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &body) != nil || len(body.Files) != 1 || len(body.Files[0].Enterprise.Collections) != 3 {
			t.Fatalf("not one account with three collections: %d %s", recorder.Code, recorder.Body.String())
		}
		if body.Files[0].Index != lockedAuthIndex(root) || body.Files[0].Enterprise.ID == "" {
			t.Fatal("account identity lost")
		}
		seen := map[string]bool{}
		for _, collection := range body.Files[0].Enterprise.Collections {
			if collection.Index == "" || collection.Index == root.Index || seen[collection.Index] {
				t.Fatal("collection indices collapsed")
			}
			seen[collection.Index] = true
		}
		for _, child := range children {
			if !seen[child.Index] {
				t.Fatal("collection disappeared")
			}
		}
		for _, secret := range []string{"synthetic-private-access", "synthetic-private-refresh", `"access_token":`, `"refresh_token":`} {
			if strings.Contains(recorder.Body.String(), secret) {
				t.Fatal("OAuth material leaked into account summary")
			}
		}
	}
}

func TestEnterpriseAccountTrafficAggregatesCollectionsForDashboard(t *testing.T) {
	h, _, children := enterpriseAccountFixture(t)
	for _, result := range []coreauth.Result{
		{AuthID: children[0].ID, Provider: ge.Provider, Model: "fixture", Success: true},
		{AuthID: children[0].ID, Provider: ge.Provider, Model: "fixture", Success: true},
		{AuthID: children[1].ID, Provider: ge.Provider, Model: "fixture", Error: &coreauth.Error{HTTPStatus: 400, Message: "synthetic failure"}},
		{AuthID: children[2].ID, Provider: ge.Provider, Model: "fixture", Success: true},
	} {
		h.authManager.MarkResult(t.Context(), result)
	}
	for _, query := range []string{"", "?page=1&page_size=1"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set(ConfigV8ContextKey, true)
		c.Request = httptest.NewRequest("GET", "/"+query, nil)
		h.ListAuthFiles(c)
		var body struct {
			Files []struct {
				Success int64                          `json:"success"`
				Failed  int64                          `json:"failed"`
				Recent  []coreauth.RecentRequestBucket `json:"recent_requests"`
			} `json:"files"`
		}
		if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &body) != nil || len(body.Files) != 1 {
			t.Fatalf("account list failed: %d %s", recorder.Code, recorder.Body.String())
		}
		row := body.Files[0]
		var success, failed int64
		for _, bucket := range row.Recent {
			success += bucket.Success
			failed += bucket.Failed
		}
		if row.Success != 3 || row.Failed != 1 || success != 3 || failed != 1 || len(row.Recent) != 20 {
			t.Fatalf("account traffic lost or counted twice: %+v", row)
		}
	}
}

func TestEnterpriseCollectionToggleIsScopedAndRevisionProtected(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	child := children[0]
	bindingID := coreauth.EnterpriseBindingID(child)
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	var revision uint64
	for _, collection := range graph.Collections {
		if collection.ID == bindingID {
			revision = collection.Revision
		}
	}
	request := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": bindingID, "expected_revision": revision, "disabled": true}
	recorder := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", request)
	if recorder.Code != 200 {
		t.Fatalf("toggle failed: %d %s", recorder.Code, recorder.Body.String())
	}
	after, afterGraph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if after.Disabled {
		t.Fatal("collection switch disabled the account")
	}
	for _, collection := range afterGraph.Collections {
		for _, old := range graph.Collections {
			if old.ID == collection.ID && old.ID != bindingID && old.Disabled != collection.Disabled {
				t.Fatal("sibling switch changed")
			}
		}
	}
	if childAfter, _ := h.authManager.GetByID(child.ID); !childAfter.Disabled || childAfter.Index != child.Index {
		t.Fatal("child switch or stable index lost")
	}
	stale := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", request)
	if stale.Code != 409 {
		t.Fatalf("stale settings overwrote current state: %d", stale.Code)
	}
	request["auth_index"] = children[1].Index
	if wrong := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", request); wrong.Code != 409 {
		t.Fatal("foreign collection index was accepted")
	}
}

func TestEnterpriseCollectionEditorKeepsUnifiedSettingsAndOAuth(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	child := children[0]
	request := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": 1, "fields": map[string]any{
		"prefix": "changed", "proxy_url": "http://127.0.0.1:9090", "note": "collection note", "weight": 2,
		"excluded-models": []string{"blocked-*"}, "disable-cooling": true, "request_retry": 0, "headers": map[string]string{"X-Fixture": "present"},
		"model_aliases":         []any{map[string]any{"name": "actual-model", "alias": "public-model", "fork": true}},
		"request_scoped_errors": []any{map[string]any{"status": 400, "match": []string{"fixture"}, "action": "stop"}},
	}}
	response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", request)
	if response.Code != 200 {
		t.Fatalf("unified settings rejected: %d %s", response.Code, response.Body.String())
	}
	current, _ := h.authManager.GetByID(child.ID)
	if current.Index != child.Index || current.Prefix != "changed" || current.ProxyURL != "http://127.0.0.1:9090" || current.Attributes["weight"] != "2" || current.Attributes["note"] != "collection note" || current.Attributes["excluded_models"] != "blocked-*" || ge.String(current.Metadata, "access_token") != "synthetic-private-access" {
		t.Fatalf("collection settings or OAuth changed unexpectedly: %v", current.Attributes)
	}
	parent, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if parent.Prefix != "" || parent.ProxyURL != "" || ge.String(parent.Metadata, "refresh_token") != "synthetic-private-refresh" {
		t.Fatal("child editor changed account settings or OAuth")
	}
	var revision uint64
	for _, collection := range graph.Collections {
		if collection.ID == coreauth.EnterpriseBindingID(child) {
			revision = collection.Revision
		}
	}
	request["expected_revision"] = revision
	request["fields"] = map[string]any{"weight": nil, "headers": map[string]string{"X-Fixture": ""}}
	response = enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", request)
	if response.Code != 200 {
		t.Fatalf("removal failed: %s", response.Body.String())
	}
	current, _ = h.authManager.GetByID(child.ID)
	if _, exists := current.Metadata["weight"]; exists {
		t.Fatal("weight unset became an effective zero")
	}
	if _, exists := current.Metadata["headers"]; exists {
		t.Fatal("header removal was not preserved")
	}
}

func TestEnterpriseCollectionResourceEditorVerifiesAssistantAndKeepsIndex(t *testing.T) {
	previous := newGeminiEnterpriseResourceClient
	t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
	for _, scenario := range []string{"ok", "missing-assistant", "wrong-collection", "changed-during-read"} {
		t.Run(scenario, func(t *testing.T) {
			h, root, children := enterpriseAccountFixture(t)
			child := children[0]
			resources := ge.Resources(ge.FromMetadata(child.Metadata))
			resources.EngineID = "new-app"
			if scenario == "wrong-collection" {
				resources.CollectionID = "another"
			}
			reads := 0
			newGeminiEnterpriseResourceClient = func(string) (*http.Client, error) {
				return &http.Client{Transport: enterpriseTestTransport(func(request *http.Request) (*http.Response, error) {
					reads++
					status := 200
					body := "{}"
					resource := ge.Engine(resources.Apply(ge.FromMetadata(child.Metadata)))
					switch {
					case strings.Contains(request.URL.Path, "/widgetConfigs/"):
						body = `{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"test-model"}]}}}`
					case strings.Contains(request.URL.Path, "/assistants/"):
						body = `{"name":"` + resource + `/assistants/default_assistant"}`
						if scenario == "missing-assistant" {
							status = 404
						}
						if scenario == "changed-during-read" {
							_, err := h.authManager.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error {
								account.Collections[0].Disabled = true
								account.Collections[0].Revision++
								return nil
							})
							if err != nil {
								t.Error(err)
							}
						}
					default:
						body = `{"name":"` + resource + `"}`
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}, nil
			}
			request := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": 1, "resources": resources}
			response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", request)
			want := map[string]int{"ok": 200, "missing-assistant": 422, "wrong-collection": 400, "changed-during-read": 409}[scenario]
			if response.Code != want {
				t.Fatalf("resource validation outcome: %d %s", response.Code, response.Body.String())
			}
			current, _ := h.authManager.GetByID(child.ID)
			if current.Index != child.Index {
				t.Fatal("application switch changed collection index")
			}
			if scenario == "ok" && ge.FromMetadata(current.Metadata).EngineID != "new-app" {
				t.Fatal("validated app was not selected")
			}
			if scenario != "ok" && ge.FromMetadata(current.Metadata).EngineID == "new-app" {
				t.Fatal("unvalidated or stale app was saved")
			}
			if scenario == "wrong-collection" && reads != 0 {
				t.Fatal("invalid collection identity reached Google")
			}
		})
	}
}

func TestEnterpriseModelCatalogRequiresSelectedCollection(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	for i, child := range children {
		id := []string{"fixture-global", "fixture-us", "fixture-eu"}[i]
		registry.GetGlobalRegistry().RegisterClient(child.ID, ge.Provider, []*registry.ModelInfo{{ID: id}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
	}
	for _, query := range []string{"?name=" + root.FileName, "?name=" + root.FileName + "&auth_index=wrong", "?name=" + root.FileName + "&auth_index=" + children[0].Index + "&binding_id=" + coreauth.EnterpriseBindingID(children[0])} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set(ConfigV8ContextKey, true)
		c.Request = httptest.NewRequest("GET", "/"+query, nil)
		h.GetAuthFileModels(c)
		if !strings.Contains(query, "binding_id") {
			if recorder.Code != 409 {
				t.Fatal("ambiguous model catalog silently selected a collection")
			}
			continue
		}
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "fixture-global") || strings.Contains(recorder.Body.String(), "fixture-us") {
			t.Fatalf("catalog escaped the selected collection: %s", recorder.Body.String())
		}
	}
}

func TestEnterpriseModelTestPinsCollectionAndRejectsStaleRevision(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	child := children[0]
	model := "multi-collection-probe"
	registry.GetGlobalRegistry().RegisterClient(child.ID, ge.Provider, []*registry.ModelInfo{{ID: model, Type: "gemini"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
	calls := 0
	h.authManager.RegisterExecutor(&enterpriseProbeExecutor{execute: func(ctx context.Context, selected *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
		calls++
		coreexec.MarkUpstreamAttempt(ctx)
		if selected.ID != child.ID || opts.Metadata[coreexec.PinnedAuthMetadataKey] != child.ID || !strings.Contains(string(req.Payload), "请只回复 OK。") {
			t.Fatal("probe escaped collection pin")
		}
		return coreexec.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"OK"}]}}]}`)}, nil
	}})
	request := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": 1, "model": model, "expected": ge.Resources(ge.FromMetadata(child.Metadata))}
	result := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"status":"ok"`) || calls != 1 {
		t.Fatalf("probe failed: %d %s", result.Code, result.Body.String())
	}
	request["expected_revision"] = 999
	result = enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
	if result.Code != 409 || calls != 1 {
		t.Fatal("stale revision called a model")
	}
	request["expected_revision"] = 1
	h.authManager.RegisterExecutor(&enterpriseProbeExecutor{execute: func(ctx context.Context, selected *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
		calls++
		coreexec.MarkUpstreamAttempt(ctx)
		_, err := h.authManager.UpdateEnterpriseAccount(ctx, root, 0, func(account *ge.Account) error {
			for i := range account.Collections {
				if account.Collections[i].ID == coreauth.EnterpriseBindingID(child) {
					account.Collections[i].Settings = map[string]any{"note": "changed during inference"}
					account.Collections[i].Revision++
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		return coreexec.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"OK"}]}}]}`)}, nil
	}})
	result = enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
	if result.Code != 409 || calls != 2 {
		t.Fatalf("late result survived a settings revision change: %d %s calls=%d", result.Code, result.Body.String(), calls)
	}
}

func TestEnterpriseDiscoverAddsNewCollectionsAndKeepsDisabledPreferences(t *testing.T) {
	h, root, _ := enterpriseAccountFixture(t)
	// Legacy project slugs becoming canonical numbers must not be counted as a
	// newly discovered project, or lose the saved disabled US collection.
	var setupErr error
	root, setupErr = h.authManager.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error {
		for i := range account.Collections {
			account.Collections[i].ProjectID = "old-project"
			account.Collections[i].ID = ge.CollectionID(account.Collections[i].ResourceBinding)
		}
		return nil
	})
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	previous := newGeminiEnterpriseResourceClient
	t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
	var calls atomic.Int32
	newGeminiEnterpriseResourceClient = func(string) (*http.Client, error) {
		return &http.Client{Transport: enterpriseTestTransport(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.Method != "GET" {
				t.Error("discovery performed a mutation")
			}
			body := "{}"
			path := request.URL.Path
			switch {
			case request.URL.Host == "www.googleapis.com":
				body = `{"email":"account@example.test","verified_email":true}`
			case strings.HasSuffix(path, "projects:search"):
				body = `{"projects":[{"name":"projects/123","projectId":"old-project","state":"ACTIVE"},{"name":"projects/456","projectId":"new-project","state":"ACTIVE"}]}`
			case strings.HasSuffix(path, "/collections"):
				body = `{"collections":[{"name":"` + strings.TrimPrefix(path, "/v1alpha/") + `/default_collection"}]}`
			case strings.HasSuffix(path, "/engines"):
				body = `{"engines":[{"name":"` + strings.TrimPrefix(path, "/v1/") + `/app"}]}`
			case strings.HasSuffix(path, "/assistants"):
				body = `{"assistants":[{"name":"` + strings.TrimPrefix(path, "/v1/") + `/default_assistant"}]}`
			default:
				body = `{"name":"` + strings.TrimPrefix(path, "/v1/") + `"}`
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}, nil
	}
	request := map[string]any{"name": root.FileName, "auth_index": root.Index}
	response := enterpriseManagementRequest(t, h.DiscoverGeminiEnterpriseResources, "POST", request)
	var result struct {
		Added    int `json:"added_collection_count"`
		Projects int `json:"added_project_count"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Added != 3 || result.Projects != 1 {
		_, _, _, _, diagnostic := h.discoverEnterpriseAccount(t.Context(), root)
		t.Fatalf("discovery did not add new project: %d %s (%v)", response.Code, response.Body.String(), diagnostic)
	}
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if len(graph.Collections) != 6 {
		t.Fatalf("wrong collection count %d", len(graph.Collections))
	}
	for _, collection := range graph.Collections {
		if collection.ProjectID == "123" && collection.Location == "us" && !collection.Disabled {
			t.Fatal("discovery reset an existing disabled collection")
		}
		if collection.ProjectID == "456" && collection.Disabled {
			t.Fatal("new collection was not enabled")
		}
	}
	response = enterpriseManagementRequest(t, h.DiscoverGeminiEnterpriseResources, "POST", request)
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Added != 0 {
		t.Fatalf("rediscovery was not idempotent: %s", response.Body.String())
	}
	if calls.Load() == 0 {
		t.Fatal("discovery did not read Google metadata")
	}
}

func TestEnterpriseQuotaCacheSingleflightRefreshAndDayIsolation(t *testing.T) {
	var cache enterpriseQuotaCache
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	start := now.Add(-5 * time.Hour)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) enterpriseQuotaEntry {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return enterpriseQuotaEntry{snapshot: ge.QuotaSnapshot{Status: "ok", ProjectID: "123", Location: "global", ObservedAt: now, PeriodStart: &start, Pools: []ge.QuotaPool{}}, expires: now.Add(2 * time.Minute)}
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, ok := cache.read(t.Context(), "same-pool", false, now, fetch); !ok {
				t.Error("shared read failed")
			}
		})
	}
	<-entered
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("pool was read %d times", calls.Load())
	}
	_, _ = cache.read(t.Context(), "same-pool", true, now, fetch)
	if calls.Load() != 2 {
		t.Fatal("manual refresh did not force one read")
	}
	_, _ = cache.read(t.Context(), "next-day", false, now.Add(24*time.Hour), fetch)
	if calls.Load() != 3 {
		t.Fatal("next day reused yesterday's pool")
	}
	for _, tc := range []struct{ date, want string }{{"2026-03-08T07:59:00Z", "2026-03-07"}, {"2026-03-08T08:00:00Z", "2026-03-08"}, {"2026-11-01T07:00:00Z", "2026-11-01"}} {
		instant, _ := time.Parse(time.RFC3339, tc.date)
		if enterpriseQuotaDay(instant) != tc.want {
			t.Fatal("Pacific quota day ignored DST")
		}
	}
}

func TestEnterpriseCollectionQuotaSharesPoolAndAllowsDisabledReads(t *testing.T) {
	h, root, _ := enterpriseAccountFixture(t)
	_, err := h.authManager.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error {
		resources := account.Collections[0].ResourceBinding
		resources.CollectionID = "another_collection"
		resources.EngineID = "another_app"
		account.Collections = append(account.Collections, ge.Collection{ID: ge.CollectionID(resources), Revision: 1, ResourceBinding: resources})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	previousClient, previousIdentity := newGeminiEnterpriseQuotaClient, resolveGeminiEnterpriseQuotaIdentity
	t.Cleanup(func() {
		newGeminiEnterpriseQuotaClient, resolveGeminiEnterpriseQuotaIdentity = previousClient, previousIdentity
	})
	resolveGeminiEnterpriseQuotaIdentity = func(_ context.Context, _ *http.Client, binding config.GeminiEnterpriseOAuth, _ string, now time.Time) *ge.QuotaIdentity {
		return &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(binding), ProjectNumber: "123", Location: binding.Location, Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: now}
	}
	reads := 0
	newGeminiEnterpriseQuotaClient = func(string) (*http.Client, error) {
		return &http.Client{Transport: enterpriseTestTransport(func(request *http.Request) (*http.Response, error) {
			reads++
			body := map[string]any{}
			if strings.HasSuffix(request.URL.Path, "/licenseConfigs") {
				body["licenseConfigs"] = []any{map[string]string{"state": "ACTIVE", "subscriptionTier": "SEARCH_AND_ASSISTANT"}, map[string]string{"state": "ACTIVE", "subscriptionTier": "ENTERPRISE"}}
			} else {
				filter := request.URL.Query().Get("filter")
				metric := "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional"
				if strings.Contains(filter, "enterprise_standard") {
					metric = strings.ReplaceAll(metric, "enterprise_plus", "enterprise_standard")
				}
				suffix, kind, value := "usage", "DELTA", "0"
				if strings.Contains(filter, "/limit") {
					suffix, kind, value = "limit", "GAUGE", "1000"
				}
				region := "global"
				if strings.Contains(filter, `regional_location = "us"`) {
					region = "us"
				}
				now := time.Now().UTC().Add(-time.Second)
				body["timeSeries"] = []any{map[string]any{"metric": map[string]any{"type": metric + "/" + suffix, "labels": map[string]string{"regional_location": region, "limit_name": "TextAnswerGenPerDayPerProjectRegional"}}, "resource": map[string]any{"type": "discoveryengine.googleapis.com/Location"}, "metricKind": kind, "points": []any{map[string]any{"interval": map[string]any{"startTime": now.Add(-time.Minute), "endTime": now}, "value": map[string]string{"int64Value": value}}}}}
			}
			data, _ := json.Marshal(body)
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		})}, nil
	}
	children := h.authManager.EnterpriseCollections(root.ID)
	var global []*coreauth.Auth
	var disabled *coreauth.Auth
	for _, child := range children {
		binding := ge.FromMetadata(child.Metadata)
		if binding.Location == "global" {
			global = append(global, child)
		}
		if binding.Location == "us" {
			disabled = child
		}
	}
	call := func(child *coreauth.Auth, force bool) enterpriseQuotaResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		query := "/?auth_index=" + child.Index + "&binding_id=" + coreauth.EnterpriseBindingID(child)
		if force {
			query += "&refresh=true"
		}
		c.Request = httptest.NewRequest("GET", query, nil)
		h.GetGeminiEnterpriseQuota(c)
		var result enterpriseQuotaResponse
		if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.SelectedPool == nil || result.SelectedPool.Used == nil || *result.SelectedPool.Used != 0 || result.SelectedPool.Remaining == nil || *result.SelectedPool.Remaining != 1000 || result.BindingID != coreauth.EnterpriseBindingID(child) || result.AccountID == "" || result.Revision == 0 {
			t.Fatalf("wrong quota or identity: %d %s", recorder.Code, recorder.Body.String())
		}
		return result
	}
	call(global[0], false)
	firstReads := reads
	call(global[1], false)
	if reads != firstReads {
		t.Fatal("same account/project/region pool was read twice")
	}
	call(global[1], true)
	if reads != 2*firstReads {
		t.Fatal("manual pool refresh was not forced")
	}
	if disabled == nil || !disabled.Disabled {
		t.Fatal("fixture did not retain disabled collection")
	}
	call(disabled, false)
	after, _ := h.authManager.GetByID(disabled.ID)
	if !after.Disabled || after.Success != 0 || after.Failed != 0 || after.LastError != nil {
		t.Fatal("quota management read mutated collection health or counts")
	}
}
