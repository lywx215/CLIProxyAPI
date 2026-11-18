package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnterpriseDefinitionsV8PersistCASAndReportRuntimeRevision(t *testing.T) {
	const route = "/config/" + enterpriseDefinitionsV8Path
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandler(&config.Config{}, path, manager)
	reload := make(chan *config.Config, 2)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reload <- cfg })
	router := gin.New()
	router.GET("/config/*path", h.ConfigV8)
	router.PUT("/config/*path", h.ConfigV8)
	router.PATCH("/config/*path", h.ConfigV8)
	request := func(method, path, tag string, body any) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		req.Header.Set("If-Match", tag)
		router.ServeHTTP(recorder, req)
		return recorder
	}
	before := request(http.MethodGet, route, "", nil)
	if before.Code != 200 || before.Header().Get("ETag") == "" || !gjson.GetBytes(before.Body.Bytes(), "definitions").IsArray() ||
		gjson.GetBytes(before.Body.Bytes(), "revision").Uint() != 0 || gjson.GetBytes(before.Body.Bytes(), "applied_revision").Uint() != 0 {
		t.Fatalf("unexpected initial state: %s", before.Body)
	}
	value := config.GeminiEnterpriseModels{Revision: 1, Definitions: []config.GeminiEnterpriseModelDefinition{{
		ModelID: "gemini-config-fixture", Targets: []config.GeminiEnterpriseModelTarget{{
			AccountID: strings.Repeat("1", 64), BindingID: strings.Repeat("2", 64), ProjectID: "1234", Location: "eu",
			CollectionID: "default_collection", EngineID: "engine", AssistantID: "default_assistant", LicenseMode: "paid", RuleRevision: 1,
		}},
	}}}
	for _, bad := range []string{"", "*", "W/" + before.Header().Get("ETag")} {
		if refused := request(http.MethodPut, route, bad, value); refused.Code != 409 {
			t.Fatalf("dedicated config write did not require the strong ETag: %s", refused.Body)
		}
	}
	result := request(http.MethodPut, route, before.Header().Get("ETag"), value)
	if result.Code != 200 || gjson.GetBytes(result.Body.Bytes(), "revision").Uint() != 1 ||
		gjson.GetBytes(result.Body.Bytes(), "applied_revision").Uint() != 0 || result.Header().Get("ETag") == before.Header().Get("ETag") {
		t.Fatalf("revision advanced without independent runtime confirmation: %s", result.Body)
	}
	select {
	case next := <-reload:
		manager.SetConfig(next)
		manager.MarkEnterpriseModelDefinitionsApplied(next.GeminiEnterpriseModels.Revision)
	case <-time.After(5 * time.Second):
		t.Fatal("save did not notify runtime")
	}
	if current := request(http.MethodGet, route, "", nil); gjson.GetBytes(current.Body.Bytes(), "applied_revision").Uint() != 1 {
		t.Fatalf("runtime revision was not visible: %s", current.Body)
	}
	if bypass := request(http.MethodPatch, "/config/upstream/gemini-enterprise", "", map[string]any{"model-definitions": map[string]any{"revision": 2}}); bypass.Code != 400 {
		t.Fatalf("ancestor patch bypassed revision checks: %s", bypass.Body)
	}
	if stale := request(http.MethodPut, route, before.Header().Get("ETag"), value); stale.Code != 409 {
		t.Fatalf("stale ETag was accepted: %s", stale.Body)
	}
	persisted, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(persisted, []byte("model-definitions:")) || !bytes.Contains(persisted, []byte("port: 8317")) {
		t.Fatalf("configuration lost the canonical subtree or unrelated values: %v", err)
	}
	parsed, errParse := config.ParseConfigBytes(persisted)
	if errParse != nil || len(parsed.GeminiEnterpriseModels.Definitions) != 1 || parsed.GeminiEnterpriseModels.Definitions[0].Targets[0] != value.Definitions[0].Targets[0] {
		t.Fatalf("persisted application identity did not round trip: %v", errParse)
	}
}
