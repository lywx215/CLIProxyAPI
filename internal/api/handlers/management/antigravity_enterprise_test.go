package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"golang.org/x/oauth2"
)

type aeManagementExecutor struct {
	calls         int
	quota         ae.Quota
	catalog       ae.Config
	finish        string
	lastModel     string
	expectedModel any
	onCatalog     func(*coreauth.Auth)
	onQuota       func(*coreauth.Auth, bool)
	quotaError    error
	executeError  error
	onExecute     func(*coreauth.Auth) error
}

func (e *aeManagementExecutor) Identifier() string { return ae.Provider }
func (e *aeManagementExecutor) Metadata(context.Context, *coreauth.Auth, bool) (ae.Config, ae.Quota, error) {
	return ae.Config{}, e.quota, nil
}
func (e *aeManagementExecutor) Catalog(_ context.Context, auth *coreauth.Auth, _ bool) (ae.Config, error) {
	if e.onCatalog != nil {
		e.onCatalog(auth)
	}
	return e.catalog, nil
}
func (e *aeManagementExecutor) Quota(_ context.Context, auth *coreauth.Auth, force bool) (ae.Quota, error) {
	if e.onQuota != nil {
		e.onQuota(auth, force)
	}
	return e.quota, e.quotaError
}
func (e *aeManagementExecutor) Execute(_ context.Context, auth *coreauth.Auth, request coreexec.Request, o coreexec.Options) (coreexec.Response, error) {
	e.calls++
	e.lastModel, e.expectedModel = request.Model, o.Metadata["antigravity_enterprise_expected_model"]
	if o.Metadata[coreexec.PinnedAuthMetadataKey] == "" {
		return coreexec.Response{}, errors.New("probe lost pinned identity")
	}
	if e.executeError != nil {
		return coreexec.Response{}, e.executeError
	}
	if e.onExecute != nil {
		if err := e.onExecute(auth); err != nil {
			return coreexec.Response{}, err
		}
	}
	return coreexec.Response{Payload: []byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"` + e.finish + `"}]}`)}, nil
}
func (e *aeManagementExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (*coreexec.StreamResult, error) {
	return nil, errors.New("unexpected stream")
}
func (e *aeManagementExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("unexpected refresh")
}
func (e *aeManagementExecutor) CountTokens(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, errors.New("unexpected count")
}
func (e *aeManagementExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected arbitrary HTTP")
}

func newAEManagementFixture(t *testing.T) (*Handler, *gin.Engine, *coreauth.Auth, []*coreauth.Auth, *aeManagementExecutor) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	root, err := sdkauth.NewAntigravityEnterpriseAccountRecord(ae.Identity{ID: "management-test", Email: "fixture@example.test", Verified: true}, &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ := ae.ReadAccount(root.Metadata)
	for _, location := range []string{"global", "us"} {
		l := ae.License{ProjectID: "project-a", Location: location, UserTier: "gcp-ge-plus-tier"}
		graph.Bindings = append(graph.Bindings, ae.Binding{ID: ae.BindingID(graph.ID, l), Revision: 1, License: l, Settings: map[string]any{}})
	}
	root.Metadata = ae.PutAccount(root.Metadata, *graph)
	root.Attributes = map[string]string{"path": filepath.Join(t.TempDir(), root.FileName)}
	root, err = manager.Register(coreauth.WithSkipPersist(context.Background()), root)
	if err != nil {
		t.Fatal(err)
	}
	children := manager.AntigravityEnterpriseBindings(root.ID)
	var q ae.Quota
	if err = json.Unmarshal([]byte(`{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":0}]}],"overageInfo":{}}`), &q); err != nil {
		t.Fatal(err)
	}
	q.ObservedAt = time.Now()
	ex := &aeManagementExecutor{quota: q, finish: "stop"}
	manager.RegisterExecutor(ex)
	for _, child := range children {
		registry.GetGlobalRegistry().RegisterClient(child.ID, ae.Provider, []*registry.ModelInfo{{ID: "gemini-4-argon-low", Type: ae.Provider}})
		manager.RefreshSchedulerEntry(child.ID)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ConfigV8ContextKey, true); c.Next() })
	r.GET("/credentials", h.ListAuthFiles)
	r.GET("/models", h.GetAuthFileModels)
	r.GET("/quota", h.GetAntigravityEnterpriseQuota)
	r.POST("/test", h.TestGeminiEnterpriseModel)
	r.PATCH("/bindings", h.PatchAntigravityEnterpriseBinding)
	r.PATCH("/status", h.PatchAuthFileStatus)
	r.PATCH("/fields", h.PatchAuthFileFields)
	r.POST("/reset", h.ResetQuotaV8)
	return h, r, root, children, ex
}
func TestAntigravityEnterpriseManagementReadIsGroupedAndNeverGenerates(t *testing.T) {
	_, router, parent, children, executor := newAEManagementFixture(t)
	for _, target := range []string{"/credentials", "/models?name=" + parent.ID, "/models?name=" + parent.ID + "&auth_index=" + children[0].Index + "&binding_id=" + ae.String(children[0].Metadata, "antigravity_enterprise_binding_id"), "/quota?name=" + parent.ID + "&auth_index=" + children[0].Index + "&binding_id=" + ae.String(children[0].Metadata, "antigravity_enterprise_binding_id")} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if target == "/models?name="+parent.ID {
			if response.Code != 409 {
				t.Fatalf("ambiguous selection accepted: %s", response.Body)
			}
			continue
		}
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", target, response.Code, response.Body)
		}
		if strings.Contains(response.Body.String(), "synthetic-access") || strings.Contains(response.Body.String(), "synthetic-refresh") {
			t.Fatal("credential exposed in management response")
		}
		if target == "/credentials" {
			var result struct {
				Files []json.RawMessage `json:"files"`
			}
			if json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Files) != 1 {
				t.Fatalf("children leaked as saved files: %s", response.Body)
			}
			if !strings.Contains(response.Body.String(), "antigravity_enterprise") {
				t.Fatal("account summary missing")
			}
		}
		if strings.HasPrefix(target, "/quota") {
			if !strings.Contains(response.Body.String(), `"remainingFraction":0`) || strings.Contains(response.Body.String(), "resetTime") {
				t.Fatal("zero/missing reset lost")
			}
		}
	}
	if executor.calls != 0 {
		t.Fatal("read-only management invoked a model")
	}
}

func TestAntigravityEnterpriseQuotaKeepsRestrictionSeparateFromReportedBalance(t *testing.T) {
	h, router, parent, children, executor := newAEManagementFixture(t)
	child := children[0]
	positive := 0.9999995
	executor.quota.Groups[0].Buckets[0].Fraction = &positive
	h.authManager.RestrictAntigravityEnterpriseQuota(child, &ae.Error{
		HTTP: http.StatusTooManyRequests, Code: "weekly_quota", RetryAt: time.Now().Add(time.Hour),
	})
	target := "/quota?name=" + parent.ID + "&auth_index=" + child.Index + "&binding_id=" + ae.String(child.Metadata, "antigravity_enterprise_binding_id")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	var result struct {
		Quota     ae.Quota                `json:"quota"`
		Cooldowns []coreauth.CooldownView `json:"cooldowns"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("quota read failed: %d %s", response.Code, response.Body)
	}
	bucket, err := result.Quota.Weekly()
	if err != nil || bucket.Fraction == nil || *bucket.Fraction != positive {
		t.Fatalf("raw observation changed: %+v, %v", bucket, err)
	}
	if len(result.Cooldowns) != 1 || result.Cooldowns[0].Reason != "weekly_quota" || result.Cooldowns[0].Phase != "cooling" {
		t.Fatalf("weekly restriction missing: %+v", result.Cooldowns)
	}
	if executor.calls != 0 {
		t.Fatal("quota read invoked generation")
	}
}

func TestAntigravityEnterpriseModelTestDistinguishesRateLimitAndCooldown(t *testing.T) {
	h, router, parent, children, executor := newAEManagementFixture(t)
	child := children[0]
	positive := 0.9999995
	executor.quota.Groups[0].Buckets[0].Fraction = &positive
	if !h.authManager.ObserveAntigravityEnterpriseQuota(child, executor.quota) {
		t.Fatal("fixture quota observation rejected")
	}
	executor.executeError = &ae.Error{
		HTTP: http.StatusTooManyRequests, Code: "upstream_error", Attempted: true,
		RetryHint: true, RetryAt: time.Now().Add(time.Minute),
	}
	bindingID := ae.String(child.Metadata, "antigravity_enterprise_binding_id")
	data, err := json.Marshal(gin.H{"name": parent.ID, "auth_index": child.Index, "binding_id": bindingID, "expected_revision": 1, "model": "gemini-4-argon-low"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct{ status, code string }{{"failed", "rate_limited"}, {"cooling", "model_cooldown"}} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(string(data))))
		var result struct {
			Status     string            `json:"status"`
			Code       string            `json:"code"`
			HTTPStatus int               `json:"http_status"`
			ElapsedMS  *int64            `json:"elapsed_ms"`
			Resources  map[string]string `json:"resources"`
			RetryAfter int64             `json:"retry_after_seconds"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("model test failed: %d %s", response.Code, response.Body)
		}
		license := coreauth.AntigravityEnterpriseLicense(child)
		if result.Status != expected.status || result.Code != expected.code || result.HTTPStatus != http.StatusTooManyRequests || result.ElapsedMS == nil || result.RetryAfter <= 0 || result.Resources["project_id"] != license.ProjectID || result.Resources["location"] != license.Location {
			t.Fatalf("unexpected %s result: %s", expected.status, response.Body)
		}
	}
	if executor.calls != 1 {
		t.Fatalf("cooldown should avoid a second generation attempt, got %d", executor.calls)
	}
}

func TestAntigravityEnterpriseBindingPatchIsRevisionChecked(t *testing.T) {
	h, router, parent, children, _ := newAEManagementFixture(t)
	child := children[0]
	body, _ := json.Marshal(gin.H{"name": parent.ID, "auth_index": child.Index, "binding_id": ae.String(child.Metadata, "antigravity_enterprise_binding_id"), "revision": 1, "fields": gin.H{"disabled": true, "note": "retained"}})
	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/bindings", strings.NewReader(string(body))))
		expected := 200
		if i == 1 {
			expected = 409
		}
		if response.Code != expected {
			t.Fatalf("patch %d: %d %s", i, response.Code, response.Body)
		}
	}
	updated, _, ok := h.authManager.AntigravityEnterpriseAccountSnapshot(parent.ID)
	if !ok || ae.String(updated.Metadata, "refresh_token") != "synthetic-refresh" {
		t.Fatal("binding update changed account OAuth")
	}
}

func TestAntigravityEnterpriseBindingRateLimitManagementContract(t *testing.T) {
	h, router, parent, children, executor := newAEManagementFixture(t)
	child := children[0]
	positive := 1.0
	executor.quota.Groups[0].Buckets[0].Fraction = &positive
	h.authManager.ObserveAntigravityEnterpriseQuota(child, executor.quota)
	for _, item := range children {
		registry.GetGlobalRegistry().RegisterClient(item.ID, ae.Provider, []*registry.ModelInfo{{ID: "gemini-4-argon-low"}, {ID: "gemini-3.8-flash-low"}})
		h.authManager.RefreshSchedulerEntry(item.ID)
	}
	deadline := time.Now().Add(time.Hour)
	executor.onExecute = func(auth *coreauth.Auth) error {
		if auth.ID != child.ID {
			return nil
		}
		failure := &ae.Error{HTTP: 429, Code: "upstream_error", Attempted: true, RetryHint: true, RetryAt: deadline}
		if retryAt := h.authManager.ObserveAntigravityEnterpriseRateLimit(auth, strconv.Itoa(executor.calls), failure); !retryAt.IsZero() {
			failure.Code, failure.Scope, failure.RetryAt = "binding_rate_limit", "binding", retryAt
		}
		return failure
	}
	for _, test := range []struct{ model, status, code string }{
		{"gemini-4-argon-low", "failed", "rate_limited"},
		{"gemini-3.8-flash-low", "failed", "binding_rate_limit"},
		{"gemini-4-argon-low", "cooling", "binding_rate_limit"},
	} {
		body, _ := json.Marshal(gin.H{"name": parent.ID, "auth_index": child.Index, "binding_id": ae.String(child.Metadata, "antigravity_enterprise_binding_id"), "expected_revision": 1, "model": test.model})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(string(body))))
		var result struct {
			Status, Code string
			HTTPStatus   int `json:"http_status"`
			RetryAfter   int `json:"retry_after_seconds"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status != test.status || result.Code != test.code || result.HTTPStatus != 429 || result.RetryAfter <= 0 {
			t.Fatalf("incorrect binding test response: %s", response.Body)
		}
	}
	if executor.calls != 2 {
		t.Fatalf("binding preflight issued another generation: %d", executor.calls)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/quota?name="+parent.ID+"&auth_index="+child.Index+"&binding_id="+ae.String(child.Metadata, "antigravity_enterprise_binding_id"), nil))
	var quota struct {
		Cooldowns []coreauth.CooldownView `json:"cooldowns"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &quota) != nil || len(quota.Cooldowns) == 0 || quota.Cooldowns[0].Reason != "binding_rate_limit" || quota.Cooldowns[0].RestrictionScope != "binding" || !quota.Cooldowns[0].RetryAt.Equal(deadline) {
		t.Fatalf("quota refresh lost binding restriction: %s", response.Body)
	}
	sibling := children[1]
	body, _ := json.Marshal(gin.H{"name": parent.ID, "auth_index": sibling.Index, "binding_id": ae.String(sibling.Metadata, "antigravity_enterprise_binding_id"), "expected_revision": 1, "model": "gemini-4-argon-low"})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/test", strings.NewReader(string(body))))
	if !strings.Contains(response.Body.String(), `"status":"ok"`) || executor.calls != 3 {
		t.Fatalf("sibling was blocked: %s", response.Body)
	}
}
func TestAntigravityEnterprisePinnedModelTestContract(t *testing.T) {
	for _, finish := range []string{"stop", "length"} {
		t.Run(finish, func(t *testing.T) {
			_, router, parent, children, ex := newAEManagementFixture(t)
			ex.finish = finish
			child := children[0]
			body, _ := json.Marshal(gin.H{"name": parent.ID, "auth_index": child.Index, "binding_id": ae.String(child.Metadata, "antigravity_enterprise_binding_id"), "expected_revision": 1, "model": "gemini-4-argon-low"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(string(body))))
			if response.Code != 200 || ex.calls != 1 {
				t.Fatalf("probe did not use fixed identity: %d %s calls=%d", response.Code, response.Body, ex.calls)
			}
			var result struct {
				Status    string `json:"status"`
				BindingID string `json:"binding_id"`
				Revision  uint64 `json:"revision"`
			}
			_ = json.Unmarshal(response.Body.Bytes(), &result)
			expected := "ok"
			if finish == "length" {
				expected = "failed"
			}
			if result.Status != expected || result.Revision != 1 || result.BindingID == "" {
				t.Fatalf("invalid probe result %s", response.Body)
			}
		})
	}
}

func TestAntigravityEnterpriseV8CommonEditsProtectBindingScope(t *testing.T) {
	h, router, parent, children, _ := newAEManagementFixture(t)
	for _, item := range []struct {
		path string
		body gin.H
	}{
		{"/status", gin.H{"name": parent.ID, "auth_index": children[0].Index, "disabled": true}},
		{"/fields", gin.H{"name": parent.ID, "prefix": "parent-leak"}},
		{"/fields", gin.H{"name": children[0].ID, "prefix": "child-bypass"}},
	} {
		body, _ := json.Marshal(item.body)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, item.path, strings.NewReader(string(body))))
		if response.Code != 409 || !strings.Contains(response.Body.String(), "binding_required") {
			t.Fatalf("unversioned scope edit accepted: %d %s", response.Code, response.Body)
		}
	}
	response := httptest.NewRecorder()
	body, _ := json.Marshal(gin.H{"name": parent.ID, "auth_index": parent.Index, "disabled": true})
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/status", strings.NewReader(string(body))))
	if response.Code != 200 {
		t.Fatalf("parent switch failed: %d %s", response.Code, response.Body)
	}
	for _, child := range h.authManager.AntigravityEnterpriseBindings(parent.ID) {
		if !child.Disabled {
			t.Fatal("parent switch did not disable children")
		}
	}
	_, graph, _ := h.authManager.AntigravityEnterpriseAccountSnapshot(parent.ID)
	for _, binding := range graph.Bindings {
		if binding.Disabled || binding.Revision != 1 || len(binding.Settings) != 0 {
			t.Fatal("parent switch changed independent binding settings")
		}
	}
}

func TestAntigravityEnterpriseManagementDirectoryUsesRoutingPolicy(t *testing.T) {
	h, router, parent, children, ex := newAEManagementFixture(t)
	if _, err := h.authManager.UpdateAntigravityEnterpriseAccount(t.Context(), children[0], 0, func(account *ae.Account) error {
		account.Bindings[0].Settings = map[string]any{
			"prefix": "team", "allowed_models": []string{"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high", "gemini-3.8-flash-low"},
			"excluded_models": []string{"gemini-4-argon-medium"},
			"model_aliases":   []config.OAuthModelAlias{{Name: "gemini-4-argon-high", Alias: "local-high", DisplayName: "Local high"}},
			"access_token":    "must-not-be-exposed",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.cfg.ForceModelPrefix = true
	h.cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{ae.Provider: {{Name: "gemini-4-argon-low", Alias: "global-low"}}}
	h.cfg.OAuthExcludedModels = map[string][]string{ae.Provider: {"gemini-3.8-*"}}
	h.authManager.SetOAuthModelAlias(h.cfg.OAuthModelAlias)
	child := h.authManager.AntigravityEnterpriseBindings(parent.ID)[0]
	modelInfos := []*registry.ModelInfo{{ID: "team/local-high", Type: ae.Provider}, {ID: "team/global-low", Type: ae.Provider}}
	registry.GetGlobalRegistry().RegisterClient(child.ID, ae.Provider, modelInfos)
	h.authManager.RefreshSchedulerEntry(child.ID)
	target := "/models?name=" + parent.ID + "&auth_index=" + child.Index + "&binding_id=" + ae.String(child.Metadata, "antigravity_enterprise_binding_id") + "&revision=2"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	var result struct {
		Models []struct {
			ID     string `json:"id"`
			Native string `json:"native_model"`
		} `json:"models"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Models) != 2 {
		t.Fatalf("unexpected projected catalog: %d %s", response.Code, response.Body)
	}
	want := map[string]string{"team/local-high": "gemini-4-argon-high", "team/global-low": "gemini-4-argon-low"}
	for _, model := range result.Models {
		if want[model.ID] != model.Native {
			t.Fatalf("native model identity lost: %+v", model)
		}
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/credentials", nil))
	if strings.Contains(response.Body.String(), "must-not-be-exposed") {
		t.Fatal("unknown binding setting leaked through summary")
	}
	body, _ := json.Marshal(gin.H{"name": parent.ID, "auth_index": child.Index, "binding_id": ae.String(child.Metadata, "antigravity_enterprise_binding_id"), "expected_revision": 2, "model": "team/global-low"})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(string(body))))
	if response.Code != 200 || ex.calls != 1 || ex.expectedModel != "gemini-4-argon-low" || ex.lastModel != "gemini-4-argon-low" {
		t.Fatalf("alias test lost native identity: %d %s calls=%d model=%s expected=%v", response.Code, response.Body, ex.calls, ex.lastModel, ex.expectedModel)
	}
}

func TestAntigravityEnterpriseManagementMetadataRejectsStaleBinding(t *testing.T) {
	for _, path := range []string{"/models", "/quota"} {
		t.Run(path, func(t *testing.T) {
			h, router, parent, children, ex := newAEManagementFixture(t)
			child := children[0]
			target := path + "?name=" + parent.ID + "&auth_index=" + child.Index + "&binding_id=" + ae.String(child.Metadata, "antigravity_enterprise_binding_id")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target+"&revision=99", nil))
			if response.Code != 409 {
				t.Fatal("stale requested revision accepted")
			}
			mutate := func(*coreauth.Auth) {
				if _, err := h.authManager.UpdateAntigravityEnterpriseAccount(t.Context(), child, 0, func(account *ae.Account) error {
					account.Bindings[0].Settings = map[string]any{"note": "changed during read"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			ex.onCatalog = mutate
			ex.onQuota = func(auth *coreauth.Auth, _ bool) { mutate(auth) }
			response = httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target+"&revision=1", nil))
			if response.Code != 409 {
				t.Fatalf("stale metadata response accepted: %d %s", response.Code, response.Body)
			}
			if ex.calls != 0 {
				t.Fatal("metadata generated a response")
			}
		})
	}
}

func TestAntigravityEnterpriseBindingFieldsAreValidatedBeforeSave(t *testing.T) {
	for name, fields := range map[string]gin.H{
		"weight": {"weight": 1000001}, "priority": {"priority": 1000001},
		"concurrency": {"max_concurrent_requests": -1}, "fraction": {"request_retry": 1.5},
		"rule-shape":  {"request_scoped_errors": "bad"},
		"rule-regex":  {"request_scoped_errors": []any{gin.H{"status": 400, "action": "stop", "match-regexr": []string{"["}}}},
		"rule-action": {"request_scoped_errors": []any{gin.H{"status": 400, "action": "unknown", "match": []string{"bad"}}}},
		"header":      {"headers": gin.H{"Bad Header": "value"}},
	} {
		t.Run(name, func(t *testing.T) {
			h, router, parent, children, _ := newAEManagementFixture(t)
			child := children[0]
			body, _ := json.Marshal(gin.H{"name": parent.ID, "auth_index": child.Index, "binding_id": ae.String(child.Metadata, "antigravity_enterprise_binding_id"), "revision": 1, "fields": fields})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/bindings", strings.NewReader(string(body))))
			if response.Code != 400 {
				t.Fatalf("invalid setting was not a validation error: %d %s", response.Code, response.Body)
			}
			_, graph, _ := h.authManager.AntigravityEnterpriseAccountSnapshot(parent.ID)
			if graph.Revision != 1 || graph.Bindings[0].Revision != 1 {
				t.Fatal("invalid patch changed the graph")
			}
		})
	}
}

func TestAntigravityEnterpriseQuotaResetSurvivesTokenRotationAndIsVersioned(t *testing.T) {
	h, router, parent, children, ex := newAEManagementFixture(t)
	child := children[0]
	ex.quota.ObservedAt = time.Now().Add(-time.Second)
	if !h.authManager.ObserveAntigravityEnterpriseQuota(child, ex.quota) {
		t.Fatal("failed to observe initial zero quota")
	}
	version := h.authManager.AntigravityEnterpriseQuotaVersion(child)
	poolID := ae.Digest(coreauth.AntigravityEnterpriseLicense(child).PoolKey())
	ex.onQuota = func(_ *coreauth.Auth, force bool) {
		if !force {
			t.Fatal("weekly reset did not force metadata refresh")
		}
		updated, _, _ := h.authManager.AntigravityEnterpriseAccountSnapshot(parent.ID)
		updated.Metadata["access_token"] = "synthetic-rotated"
		if _, err := h.authManager.Update(t.Context(), updated); err != nil {
			t.Fatal(err)
		}
		ex.quota.ObservedAt = time.Now()
	}
	body, _ := json.Marshal(gin.H{"auth_index": child.Index, "reset_scope": "shared_pool", "pool_id": poolID, "pool_version": version})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/reset", strings.NewReader(string(body))))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"phase":"probe_pending"`) || !strings.Contains(response.Body.String(), `"pool_version":`+strconv.FormatUint(version+1, 10)) {
		t.Fatalf("token rotation invalidated quota reset: %d %s", response.Code, response.Body)
	}
	fresh := h.authManager.AntigravityEnterpriseBindings(parent.ID)[0]
	if h.authManager.CheckAntigravityEnterpriseQuota(fresh, time.Now()) == nil {
		t.Fatal("reset locally granted quota after another upstream zero")
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/reset", strings.NewReader(string(body))))
	if response.Code != 409 {
		t.Fatal("old pool version could reset again")
	}
}
