package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

type enterpriseFixtureTransport struct{ calls atomic.Int32 }

func (f *enterpriseFixtureTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return f }
func (f *enterpriseFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	if req.URL.Host != "discoveryengine.googleapis.com" {
		return nil, io.ErrUnexpectedEOF
	}
	body, _ := io.ReadAll(req.Body)
	text := "offline 前🙂引用"
	if strings.Contains(gjson.GetBytes(body, "query.text").String(), "ONLY valid JSON") {
		text = `{"ok":true}`
	}
	runes := []rune(text)
	split := len(runes) / 2
	content, _ := json.Marshal(map[string]any{"answer": map[string]any{"state": "IN_PROGRESS", "replies": []map[string]any{
		{"content": map[string]any{"executableCode": map[string]any{"language": "PYTHON", "code": "print(7)"}}},
		{"content": map[string]any{"codeExecutionResult": map[string]any{"outcome": "OUTCOME_OK", "output": "7\n"}}},
		{"content": map[string]any{"inlineData": map[string]string{"mimeType": "application/json+suggestions", "data": "e30="}}},
		{"content": map[string]string{"text": string(runes[:split])}},
		{"content": map[string]string{"text": string(runes[split:])}},
	}}})
	terminal, _ := json.Marshal(map[string]any{"answer": map[string]any{"state": "SUCCEEDED", "replies": []map[string]any{{"groundedContent": map[string]any{
		"textGroundingMetadata": map[string]any{"references": []map[string]any{{"documentMetadata": map[string]string{"uri": "https://example.com/sdk-fixture", "title": "Offline fixture"}}}, "segments": []map[string]any{{"startIndex": 15, "endIndex": 21, "text": "引用", "referenceIndices": []int{0}}}},
	}}}}})
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("[" + string(content) + "," + string(terminal) + "]"))}, nil
}

func enterpriseFixtureRouter(t *testing.T) (*gin.Engine, *enterpriseFixtureTransport, *coreauth.Manager) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	transport := &enterpriseFixtureTransport{}
	manager.SetRoundTripperProvider(transport)
	cfg := &config.Config{}
	manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
	id := "enterprise-wire-fixture"
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "fixture-client", ProjectID: "fixture-project", EngineID: "fixture-engine"}, "fixture@example.com", &oauth2.Token{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour)})})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, []*registry.ModelInfo{{ID: "gemini-4-argon", Name: "models/gemini-4-argon", Type: "gemini", OwnedBy: ge.Provider, SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"}}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	chat := openaihandlers.NewOpenAIAPIHandler(base)
	gemini := geminihandlers.NewGeminiAPIHandler(base)
	router := gin.New()
	router.POST("/v1/chat/completions", chat.ChatCompletions)
	router.POST("/v1/completions", chat.Completions)
	router.GET("/v1/models", chat.OpenAIModels)
	router.GET("/v1beta/models", gemini.GeminiModels)
	router.POST("/v1beta/models/*action", gemini.GeminiHandler)
	return router, transport, manager
}

func TestEnterprisePublicRoutesBothProtocolsAndOperations(t *testing.T) {
	router, transport, _ := enterpriseFixtureRouter(t)
	for _, test := range []struct {
		path, body string
		status     int
		visible    string
	}{
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`, 200, "offline"},
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"stream":true}`, 200, "data: [DONE]"},
		{"/v1beta/models/gemini-4-argon:generateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`, 200, "executableCode"},
		{"/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hi"}]}]}`, 200, "groundingMetadata"},
		{"/v1/completions", `{"model":"gemini-4-argon","prompt":"hi"}`, 400, "operation_not_supported"},
		{"/v1/chat/completions", `{"model":"gemini-4-argon","input":"hi"}`, 400, "operation_not_supported"},
		{"/v1beta/models/gemini-4-argon:countTokens", `{"contents":[{"parts":[{"text":"hi"}]}]}`, 400, "operation_not_supported"},
	} {
		before := transport.calls.Load()
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.visible) {
			t.Fatalf("%s: status=%d body=%s", test.path, response.Code, response.Body.String())
		}
		if test.status == 400 && before != transport.calls.Load() {
			t.Fatal("unsupported endpoint called upstream")
		}
	}
	for _, path := range []string{"/v1/models", "/v1beta/models"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), "gemini-4-argon") || strings.Contains(response.Body.String(), "countTokens") {
			t.Fatalf("invalid public catalog: %s", response.Body.String())
		}
	}
}

// TestEnterpriseSDKHarness is opt-in and serves synthetic fixtures on loopback only.
// The Python acceptance script uses real SDKs without Google credentials or model calls.
func TestEnterpriseSDKHarness(t *testing.T) {
	ready := os.Getenv("CLIPROXY_ENTERPRISE_SDK_READY_FILE")
	if ready == "" {
		t.Skip("optional local SDK harness")
	}
	router, _, _ := enterpriseFixtureRouter(t)
	stop := make(chan struct{}, 1)
	router.POST("/__stop", func(c *gin.Context) {
		select {
		case stop <- struct{}{}:
		default:
		}
		c.Status(204)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	if err := os.WriteFile(ready, []byte(server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stop:
	case <-t.Context().Done():
	}
}

// A same-name route remains a mixed host pool for Chat. Legacy operations remove
// Enterprise before selecting credentials and continue on the companion provider.
type enterpriseCompanionExecutor struct {
	*runtimeexec.GeminiEnterpriseExecutor
	calls atomic.Int32
}

func (*enterpriseCompanionExecutor) Identifier() string { return "enterprise-companion" }
func (e *enterpriseCompanionExecutor) Execute(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	e.calls.Add(1)
	return coreexec.Response{Payload: []byte(`{"id":"companion","object":"chat.completion","model":"gemini-4-argon","choices":[{"index":0,"message":{"role":"assistant","content":"companion"},"finish_reason":"stop"}]}`)}, nil
}

func TestEnterpriseSameNameMixedPoolAndOtherOperationFallback(t *testing.T) {
	router, transport, manager := enterpriseFixtureRouter(t)
	companion := &enterpriseCompanionExecutor{GeminiEnterpriseExecutor: runtimeexec.NewGeminiEnterpriseExecutor(&config.Config{})}
	manager.RegisterExecutor(companion)
	id := "enterprise-companion-auth"
	_, err := manager.Register(t.Context(), &coreauth.Auth{ID: id, Provider: companion.Identifier(), Status: coreauth.StatusActive, Metadata: map[string]any{"email": "companion@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, companion.Identifier(), []*registry.ModelInfo{{ID: "gemini-4-argon", Type: "openai"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	for range 8 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`))
		router.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("mixed Chat failed: %s", response.Body.String())
		}
	}
	if transport.calls.Load() == 0 || companion.calls.Load() == 0 {
		t.Fatal("same-name providers did not share normal host scheduling")
	}
	before := transport.calls.Load()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/v1/completions", strings.NewReader(`{"model":"gemini-4-argon","prompt":"hi"}`)))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "companion") || transport.calls.Load() != before {
		t.Fatalf("legacy operation did not stay on companion: status=%d body=%s", response.Code, response.Body.String())
	}
}
