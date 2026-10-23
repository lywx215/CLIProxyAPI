package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	management "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type quotaWireTransport struct {
	calls atomic.Int32
	mode  string
}

func (f *quotaWireTransport) RoundTripperFor(*auth.Auth) http.RoundTripper { return f }
func (f *quotaWireTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "discoveryengine.googleapis.com" || !strings.HasSuffix(r.URL.Path, ":streamAssist") {
		return nil, io.ErrUnexpectedEOF
	}
	f.calls.Add(1)
	quota := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"fixture-sensitive-assistToken","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","metadata":{"service":"discoveryengine.googleapis.com","consumer":"projects/12345","location":"us","quota_metric":"discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional","quota_limit":"TextAnswersPerDay"}}]}}`
	status := 200
	body := quota
	switch f.mode {
	case "http":
		status = 429
	case "ndjson-thought":
		body = `{"answer":{"state":"IN_PROGRESS"}}` + "\n" + quota
	case "sse-tail":
		body = "data: {\"answer\":{\"state\":\"IN_PROGRESS\",\"replies\":[{\"content\":{\"text\":\"hello\"}}]}}\n\ndata: " + quota + "\n\n"
	case "array":
		body = "[" + quota + "]"
	case "success":
		body = `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"normal response"}}]}},{"answer":{"state":"SUCCEEDED"}}]`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(&byteReader{body: []byte(body)})}, nil
}

type byteReader struct{ body []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.body) == 0 {
		return 0, io.EOF
	}
	p[0] = r.body[0]
	r.body = r.body[1:]
	return 1, nil
}

func TestEnterpriseSharedQuotaPublicProtocolsSynthetic(t *testing.T) {
	for _, mode := range []string{"http", "array", "ndjson-thought", "sse-tail"} {
		for _, route := range []struct{ path, body string }{
			{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`},
			{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"stream":true}`},
			{"/v1beta/models/gemini-4-argon:generateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`},
			{"/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hi"}]}]}`},
		} {
			t.Run(mode+route.path+route.body[len(route.body)-2:], func(t *testing.T) {
				router, _, manager := enterpriseFixtureRouter(t)
				binding := config.GeminiEnterpriseOAuth{ClientID: "fixture-client", ProjectID: "12345", Location: "us", EngineID: "fixture-engine"}.WithDefaults()
				for _, id := range []string{"enterprise-wire-fixture", "enterprise-wire-sibling"} {
					metadata := map[string]any{"client_id": binding.ClientID, "project_id": binding.ProjectID, "location": binding.Location, "engine_id": binding.EngineID, "access_token": "offline-only", "refresh_token": "offline-only"}
					a := &auth.Auth{ID: id, Provider: ge.Provider, Status: auth.StatusActive, Metadata: metadata, EnterpriseQuotaIdentity: &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(binding), ProjectNumber: "12345", Location: "us", Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: time.Now()}}
					if _, err := manager.Register(context.Background(), a); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, []*registry.ModelInfo{{ID: "gemini-4-argon", Type: "gemini", OwnedBy: ge.Provider, SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"}}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}
				upstream := &quotaWireTransport{mode: mode}
				manager.SetRoundTripperProvider(upstream)
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(response, request)
				if upstream.calls.Load() != 1 {
					t.Fatalf("unsafe generation replay: calls=%d", upstream.calls.Load())
				}
				if strings.Contains(response.Body.String(), "sensitive") {
					t.Fatal("raw generation error leaked")
				}
				var pool auth.CooldownView
				for _, id := range []string{"enterprise-wire-fixture", "enterprise-wire-sibling"} {
					a, _ := manager.GetByID(id)
					views := auth.CooldownSnapshotForAuth(a, time.Now())
					if len(views) != 1 || views[0].Origin != "shared_pool" {
						data, _ := json.Marshal(views)
						t.Fatalf("missing shared projection for %s: %s; HTTP=%d body=%s", id, data, response.Code, response.Body.String())
					}
					pool = views[0]
				}
				h := management.NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
				router.POST("/v8/management/routing/cooldown/reset", h.ResetQuotaV8)
				sibling, _ := manager.GetByID("enterprise-wire-sibling")
				resetBody, _ := json.Marshal(map[string]any{"auth_index": sibling.Index, "reset_scope": "shared_pool", "pool_id": pool.PoolID, "pool_version": pool.PoolVersion})
				for _, want := range []int{200, 409} {
					resetResponse := httptest.NewRecorder()
					resetRequest := httptest.NewRequest(http.MethodPost, "/v8/management/routing/cooldown/reset", strings.NewReader(string(resetBody)))
					resetRequest.Header.Set("Content-Type", "application/json")
					router.ServeHTTP(resetResponse, resetRequest)
					if resetResponse.Code != want {
						t.Fatalf("versioned reset status=%d want=%d body=%s", resetResponse.Code, want, resetResponse.Body.String())
					}
					if want == 200 && !strings.Contains(resetResponse.Body.String(), "affected_auth_indexes") {
						t.Fatal("reset omitted affected credentials")
					}
				}
				upstream.mode = "success"
				recovered := httptest.NewRecorder()
				request = httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(recovered, request)
				if recovered.Code != 200 || upstream.calls.Load() != 2 {
					t.Fatalf("normal-request recovery failed: status=%d calls=%d body=%s", recovered.Code, upstream.calls.Load(), recovered.Body.String())
				}
				for _, id := range []string{"enterprise-wire-fixture", "enterprise-wire-sibling"} {
					a, _ := manager.GetByID(id)
					if len(auth.CooldownSnapshotForAuth(a, time.Now())) != 0 {
						t.Fatal("successful verification left a restriction")
					}
				}
			})
		}
	}
}

// An optional output directory exports only synthetic fixtures for browser QA.
func TestEnterpriseSharedQuotaBrowserFixture(t *testing.T) {
	dir := os.Getenv("ENTERPRISE_QUOTA_FIXTURE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m := auth.NewManager(nil, nil, nil)
	m.SetCooldownStateStore(auth.NewFileCooldownStateStore(dir))
	m.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(&config.Config{}))
	m.SetRoundTripperProvider(&quotaWireTransport{mode: "http"})
	b := config.GeminiEnterpriseOAuth{ClientID: "offline-browser-client", ProjectID: "12345", Location: "us", EngineID: "offline-engine"}.WithDefaults()
	for _, id := range []string{"shared-a.json", "shared-b.json"} {
		metadata := map[string]any{"type": ge.Provider, "email": id + "@example.test", "client_id": b.ClientID, "project_id": b.ProjectID, "location": b.Location, "collection_id": b.CollectionID, "engine_id": b.EngineID, "assistant_id": b.AssistantID, "access_token": "offline-synthetic-only", "refresh_token": "offline-synthetic-only", "expiry": "2090-01-01T00:00:00Z"}
		a := &auth.Auth{ID: id, FileName: id, Provider: ge.Provider, Status: auth.StatusActive, Metadata: metadata, EnterpriseQuotaIdentity: &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(b), ProjectNumber: b.ProjectID, Location: b.Location, Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: time.Now()}}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(metadata)
		if err := os.WriteFile(filepath.Join(dir, id), data, 0600); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, []*registry.ModelInfo{{ID: "gemini-4-argon", Type: "gemini", OwnedBy: ge.Provider}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	_, err := m.Execute(context.Background(), []string{ge.Provider}, exec.Request{Model: "gemini-4-argon", Payload: []byte(`{"messages":[{"role":"user","content":"offline"}]}`)}, exec.Options{SourceFormat: translator.FormatOpenAI, Metadata: map[string]any{exec.OperationMetadataKey: "chat_completions"}})
	if err == nil {
		t.Fatal("synthetic rejection expected")
	}
	for _, id := range []string{"shared-a.json", "shared-b.json"} {
		a, _ := m.GetByID(id)
		if len(auth.CooldownSnapshotForAuth(a, time.Now())) != 1 {
			t.Fatal("fixture lost shared restriction")
		}
	}
}
