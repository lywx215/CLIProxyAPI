package management

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type webManagementTransport func(*http.Request) (*http.Response, error)

func (f webManagementTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func webManagementFixture(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\nserver: {port: 8317}\nupstream:\n  gemini-enterprise-web: {enabled: false, max-concurrent: 2, max-inflight-bytes: 268435456}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = filepath.Join(dir, "auths")
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(cfg.AuthDir)
	manager := auth.NewManager(store, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewGeminiEnterpriseWebExecutor(cfg, manager.WebRuntime()))
	h := &Handler{cfg: cfg, configFilePath: path, authManager: manager, tokenStore: store}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	r.POST("/v8/management/credentials/gemini-enterprise-web", h.SaveEnterpriseWebCredential)
	r.PATCH("/v8/management/credentials/gemini-enterprise-web", h.SaveEnterpriseWebCredential)
	r.GET("/v8/management/credentials/gemini-enterprise-web/status", h.GetEnterpriseWebStatus)
	r.POST("/v8/management/credentials/gemini-enterprise-web/models/test", h.TestEnterpriseWebModel)
	r.GET("/v8/management/credentials/models", h.GetAuthFileModels)
	r.GET("/v8/management/model-definitions/:channel", h.GetStaticModelDefinitions)
	r.GET("/v8/management/credentials", h.ListAuthFiles)
	r.PATCH("/v8/management/credentials/status", h.PatchAuthFileStatus)
	r.PATCH("/v8/management/credentials/fields", h.PatchAuthFileFields)
	r.DELETE("/v8/management/credentials", h.DeleteAuthFile)
	r.POST("/v8/management/credentials/refresh", h.RefreshAuthFiles)
	r.PATCH("/v8/management/config/*path", h.ConfigV8)
	r.GET("/v8/management/config.yaml", h.ConfigV8)
	return h, r
}
func webManagementRequest(t *testing.T, r http.Handler, method, path string, body any, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	switch value := body.(type) {
	case string:
		b = []byte(value)
	default:
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func addWebManagementCredential(t *testing.T, r http.Handler) string {
	t.Helper()
	w := webManagementRequest(t, r, "POST", "/v8/management/credentials/gemini-enterprise-web", map[string]any{"model_policy": web.PolicyExcludeArgon, "label": "Synthetic Web", "cookies": "SID=synthetic-cookie-one; SAPISID=synthetic-signing", "page_url": "https://vertexaisearch.cloud.google.com/ca/home/cid/config"}, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "synthetic-cookie-one") {
		t.Fatal("credential response leaked cookies")
	}
	return gjson.Get(w.Body.String(), "credential.name").String()
}

func TestEnterpriseWebCredentialCRUDRevisionAndValidation(t *testing.T) {
	h, r := webManagementFixture(t)
	name := addWebManagementCredential(t, r)
	for _, field := range []string{"models", "available_models"} {
		forbidden := webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{"name": name, "expected_revision": 1, field: []any{}}, nil)
		if forbidden.Code != 400 {
			t.Fatal("credential editor accepted model configuration", field, forbidden.Code)
		}
	}
	if h.authManager.WebRuntime().Enabled() {
		t.Fatal("saving a Cookie enabled the channel")
	}
	list := webManagementRequest(t, r, "GET", "/v8/management/credentials", nil, nil)
	if strings.Contains(list.Body.String(), "synthetic-cookie") || !gjson.Get(list.Body.String(), "files.0.gemini_enterprise_web.cookie_present").Bool() {
		t.Fatal("unsafe or absent summary")
	}
	patch := map[string]any{"name": name, "expected_revision": 1, "label": "Renamed"}
	w := webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", patch, nil)
	if w.Code != 200 || gjson.Get(w.Body.String(), "credential.gemini_enterprise_web.revision").Int() != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, name))
	if err != nil || !bytes.Contains(saved, []byte("synthetic-cookie-one")) {
		t.Fatal("omitted cookie was not preserved", err)
	}
	w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", patch, nil)
	if w.Code != 409 {
		t.Fatal("stale edit accepted", w.Code)
	}
	patch["expected_revision"] = 2
	patch["cookies"] = "SID=synthetic-cookie-two; SAPISID=synthetic-signing"
	w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", patch, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	current, _ := h.authManager.GetByID(name)
	if current.CredentialVersion < 3 {
		t.Fatal("replacement did not advance credential version")
	}
	// A source-file edit must be detected even before the watcher has observed it.
	raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	_ = json.Unmarshal(raw, &metadata)
	credential, _ := web.CredentialFrom(metadata)
	credential.Revision = 100
	metadata["enterprise_web"] = credential.MetadataValue()
	raw, _ = json.Marshal(metadata)
	if err = os.WriteFile(filepath.Join(h.cfg.AuthDir, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
	patch["expected_revision"] = 3
	delete(patch, "cookies")
	w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", patch, nil)
	if w.Code != 409 {
		t.Fatal("persisted revision not checked", w.Code, w.Body.String())
	}
	w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/fields", map[string]any{"name": name, "enterprise_web.cookies": "bypass"}, nil)
	if w.Code != 400 {
		t.Fatal("generic editor can replace authentication material")
	}
}

func TestEnterpriseWebCookieVerificationUsesNoGenerationAndNoJWTOnDisk(t *testing.T) {
	h, r := webManagementFixture(t)
	name := addWebManagementCredential(t, r)
	a, _ := h.authManager.GetByID(name)
	credential, _ := web.CredentialFrom(a.Metadata)
	credential.ModelPolicy = ""
	credential.UserAgent = "Synthetic Enterprise Web Agent/1.0"
	credential.Revision++
	a.Metadata["enterprise_web"] = credential.MetadataValue()
	if _, err := h.authManager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", webManagementTransport(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		reply := `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`
		if req.Method != http.MethodGet {
			if req.URL.Host != "ca-discoveryengine.clients6.google.com" || !strings.HasSuffix(req.URL.Path, "lookupWidgetConfig") {
				t.Fatal("verification generated content")
			}
			reply = `{"engineResourceDetails":{"projectNumber":"123","location":"ca","collectionId":"default_collection","engineId":"synthetic"},"anonymousWidgetConfig":{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"observed-id"}]}}}}`
		} else if req.URL.Host != "vertexaisearch.cloud.google.com" {
			t.Fatal("wrong bootstrap origin")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
	}))
	w := webManagementRequest(t, r, "POST", "/v8/management/credentials/refresh", map[string]string{"name": name}, ctx)
	if w.Code != 200 || calls.Load() != 2 || !gjson.Get(w.Body.String(), "credential.auth_verified").Bool() || strings.Contains(w.Body.String(), "synthetic-cookie") {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
	if gjson.Get(w.Body.String(), "credential.model_policy").String() != web.PolicyUnclassified {
		t.Fatal("verification inferred a classification")
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, name))
	if err != nil || bytes.Contains(raw, []byte("eyJ")) || bytes.Contains(raw, []byte("access_token")) {
		t.Fatal("JWT persisted", err)
	}
	status := webManagementRequest(t, r, "GET", "/v8/management/credentials/gemini-enterprise-web/status", nil, ctx)
	if status.Code != 200 || calls.Load() != 2 {
		t.Fatal("status invoked upstream")
	}
	// The dedicated editor round-trips safe fields from the summary. Omitting
	// the user agent there would replace it with an empty value on a label edit.
	list := webManagementRequest(t, r, "GET", "/v8/management/credentials", nil, nil)
	summary := gjson.Get(list.Body.String(), "files.0.gemini_enterprise_web")
	if summary.Get("user_agent").String() != credential.UserAgent || strings.Contains(list.Body.String(), "synthetic-cookie") {
		t.Fatal("safe edit summary lost the user agent or disclosed cookies")
	}
	w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{
		"name": name, "expected_revision": credential.Revision, "label": "Renamed verified Web",
		"user_agent": summary.Get("user_agent").String(),
	}, nil)
	if w.Code != 200 || !gjson.Get(w.Body.String(), "credential.gemini_enterprise_web.auth_verified").Bool() || gjson.Get(w.Body.String(), "credential.gemini_enterprise_web.available_models.#").Int() != 1 || calls.Load() != 2 {
		t.Fatal("safe metadata edit invalidated authentication or catalog", w.Code, w.Body.String(), calls.Load())
	}
	credential.Revision++
	// Replacing cookies invalidates authentication proof and its observed model catalog.
	w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{"name": name, "expected_revision": credential.Revision, "cookies": "SID=synthetic-replacement; SAPISID=synthetic-signing"}, nil)
	if w.Code != 200 || gjson.Get(w.Body.String(), "credential.gemini_enterprise_web.auth_verified").Bool() || gjson.Get(w.Body.String(), "credential.gemini_enterprise_web.available_models.#").Int() != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, disabled := range []bool{true, false} {
		w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/status", map[string]any{"name": name, "disabled": disabled}, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = webManagementRequest(t, r, "DELETE", "/v8/management/credentials?name="+url.QueryEscape(name), nil, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = os.Stat(filepath.Join(h.cfg.AuthDir, name)); !os.IsNotExist(err) {
		t.Fatal("credential source retained", err)
	}
}

func TestEnterpriseWebHotConfigDrainAndLimitsValidation(t *testing.T) {
	h, r := webManagementFixture(t)
	patch := func(body string, want int) {
		t.Helper()
		w := webManagementRequest(t, r, "PATCH", "/v8/management/config/upstream/gemini-enterprise-web", body, nil)
		if w.Code != want {
			t.Fatalf("config %d: %s", w.Code, w.Body.String())
		}
	}
	patch(`{"enabled":true}`, 200)
	_, lease, err := h.authManager.WebRuntime().Acquire(context.Background(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	patch(`{"enabled":false,"max-concurrent":1,"max-inflight-bytes":67108864}`, 200)
	status := webManagementRequest(t, r, "GET", "/v8/management/credentials/gemini-enterprise-web/status", nil, nil)
	if !gjson.Get(status.Body.String(), "draining").Bool() || gjson.Get(status.Body.String(), "active_requests").Int() != 1 {
		t.Fatal(status.Body.String())
	}
	patch(`{"enabled":true}`, 200)
	if _, _, err = h.authManager.WebRuntime().Acquire(context.Background(), 1); err == nil {
		t.Fatal("reopen erased old concurrency")
	}
	lease.Release()
	patch(`{"max-concurrent":0}`, 422)
	patch(`{"max-inflight-bytes":123}`, 422)
	patch(`{"enabled":false}`, 200)
	saved := webManagementRequest(t, r, "GET", "/v8/management/config.yaml", nil, nil)
	if !strings.Contains(saved.Body.String(), "enabled: false") {
		t.Fatal("explicit false was lost")
	}
}
