package management

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGeminiBudgetExplicitInitRevisionAndNoReset(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{GeminiFreeTierPolicy: config.GeminiFreeTierPolicyConfig{StoragePath: filepath.Join(dir, "ledger"), ManagedModels: map[string]int{"gemini-2.5-flash": 20}}}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	t.Cleanup(func() { _ = m.CloseGeminiBudget() })
	h := NewHandler(cfg, path, m)
	r := gin.New()
	r.GET("/budget", h.GetGeminiBudget)
	r.POST("/init", h.InitializeGeminiBudget)
	request := func(method, route, revision string) *httptest.ResponseRecorder {
		var body []byte
		if method == "POST" {
			body, _ = json.Marshal(map[string]any{"revision": revision, "acknowledge_unknown_usage": true})
		}
		req := httptest.NewRequest(method, route, bytes.NewReader(body))
		if revision != "" {
			req.Header.Set("If-Match", `"`+revision+`"`)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	get := request("GET", "/budget", "")
	if get.Code != 200 {
		t.Fatalf("GET: %d %s", get.Code, get.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	revision := state["revision"].(string)
	if _, err := os.Stat(cfg.GeminiFreeTierPolicy.StoragePath); !os.IsNotExist(err) {
		t.Fatal("GET created ledger")
	}
	if w := request("POST", "/init", "stale"); w.Code != 409 {
		t.Fatalf("stale init: %d", w.Code)
	}
	if w := request("POST", "/init", revision); w.Code != 200 {
		t.Fatalf("init: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.GeminiFreeTierPolicy.StoragePath, "init.meta")); err != nil {
		t.Fatal(err)
	}
	if w := request("POST", "/init", revision); w.Code != 409 {
		t.Fatalf("reset allowed: %d", w.Code)
	}
	if m.GeminiBudgetPolicy().Enabled {
		t.Fatal("initialization silently enabled policy")
	}
}

func TestGeminiBudgetProjectViewKeepsHealthySiblingKeyAvailable(t *testing.T) {
	cfg := &config.Config{GeminiFreeTierPolicy: config.GeminiFreeTierPolicyConfig{StoragePath: t.TempDir(), ManagedModels: map[string]int{"gemini-2.5-flash": 20}}}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	engine, err := m.EnsureGeminiBudget(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseGeminiBudget() })
	cfg.GeminiFreeTierPolicy.Enabled = true
	m.SetConfig(cfg)
	var keys []*coreauth.Auth
	for _, id := range []string{"budget-view-first", "budget-view-last"} {
		a, errRegister := m.Register(t.Context(), &coreauth.Auth{ID: id, Provider: "gemini", Attributes: map[string]string{"api_key": id, "quota_project": "shared-local-project"}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		keys = append(keys, a)
		registry.GetGlobalRegistry().RegisterClient(id, "gemini", []*registry.ModelInfo{{ID: "gemini-2.5-flash"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	if err = engine.Observe(t.Context(), coreauth.GeminiBudgetScope(keys[1], "gemini-2.5-flash"), budget.Signal{Kind: budget.SignalCredentialInvalid}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, "", m)
	view := h.geminiBudgetView("local-revision", cfg)
	projects := view["projects"].([]geminiBudgetProjectView)
	if len(projects) != 1 || projects[0].KeyCount != 2 || projects[0].Models[0].State != "available" {
		t.Fatalf("healthy sibling hidden: %+v", projects)
	}
	if err = engine.Observe(t.Context(), coreauth.GeminiBudgetScope(keys[0], "gemini-2.5-flash"), budget.Signal{Kind: budget.SignalCredentialInvalid}); err != nil {
		t.Fatal(err)
	}
	projects = h.geminiBudgetView("local-revision", cfg)["projects"].([]geminiBudgetProjectView)
	if projects[0].Models[0].State != "quarantined" {
		t.Fatalf("all quarantined keys advertised available: %+v", projects)
	}
}
