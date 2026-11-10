package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestEnterpriseWebMediaFallbackThroughNativeHTTP(t *testing.T) {
	const currentInput = `{"systemInstruction":{"parts":[{"text":"system"}]},"contents":[{"parts":[{"text":"current","inlineData":{"mimeType":"image/png","data":"aW5wdXQ="}}]}],"tools":[{"googleSearch":{}}],"generationConfig":{"temperature":0.2}}`
	const historyInput = `{"contents":[{"role":"user","parts":[{"text":"earlier"}]},{"role":"model","parts":[{"inline_data":{"mime_type":"image/png","data":"aW5wdXQ="}},{"text":"earlier reply"}]},{"role":"user","parts":[{"fileData":{"fileUri":"files/private-attachment"}}]}]}`
	const attachmentsOnly = `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aW5wdXQ="}}]}]}`
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, source, text string
			status                   int
			closed, disabled, stale  bool
		}{
			{name: "disabled", body: currentInput, source: "current_input", text: "current", status: 200, closed: true},
			{name: "no eligible Cookie", body: historyInput, source: "history_text", text: "earlier", status: 200, disabled: true},
			{name: "confirmed relogin before session", body: string(phaseOneRequest("image/png").Payload), source: "current_input", text: "read these attachments", status: 200, stale: true},
			{name: "no usable text", body: attachmentsOnly, status: 400, closed: true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				m, plain, e, ctx, upstream := phaseOneManager(t)
				cfg := *e.cfg
				cfg.GeminiEnterpriseWeb.Enabled = !tc.closed
				m.SetConfig(&cfg)
				if tc.disabled {
					cookie, ok := m.GetByID("web-phase-one-cookie")
					if !ok {
						t.Fatal("missing synthetic Cookie")
					}
					cookie.Disabled = true
					if _, err := m.Update(ctx, cookie); err != nil {
						t.Fatal(err)
					}
				}
				authCalls := 0
				if tc.stale {
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(request *http.Request) (*http.Response, error) {
						authCalls++
						if request.Method != http.MethodGet {
							t.Error("the expired Cookie reached a Web session or upload")
						}
						return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
					}))
				}
				var submitted exec.Request
				var submittedOptions exec.Options
				plain.capture = func(req exec.Request, opts exec.Options) { submitted, submittedOptions = req, opts }
				logs := t.TempDir()
				router := gin.New()
				router.Use(middleware.EnterpriseWebBodyAdmission(m), middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(true, logs, "", 10)))
				h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m))
				router.POST("/v1beta/models/*action", h.GeminiHandler)
				action := "generateContent"
				if stream {
					action = "streamGenerateContent?alt=sse"
				}
				req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:"+action, strings.NewReader(tc.body)).WithContext(ctx)
				if tc.disabled {
					req.ContentLength = -1
				}
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != tc.status || len(upstream.calls) != 0 || (authCalls != 0) != tc.stale {
					t.Fatal(res.Code, res.Body.String(), upstream.calls, authCalls)
				}
				state := m.WebRuntime().Status()
				if state["active_requests"] != 0 || state["inflight_bytes"] != int64(0) {
					t.Fatal("the request retained a Web lease", state)
				}
				result := res.Result()
				defer result.Body.Close()
				header := result.Header.Get(web.MediaFallbackHeader)
				if tc.status != http.StatusOK {
					if plain.calls.Load() != 0 || header != "" || gjson.Get(res.Body.String(), "error.code").String() != "media_fallback_no_text" {
						t.Fatal("attachment-only input reached generation or lost its error code", res.Body.String(), header)
					}
					return
				}
				reason := "no_web_credential"
				if tc.closed {
					reason = "web_disabled"
				}
				if header != reason+"/"+tc.source || plain.calls.Load() != 1 || !strings.Contains(res.Body.String(), `"finishReason":"STOP"`) {
					t.Fatal("fallback did not commit its header before generation delivery", header, res.Body.String(), plain.calls.Load())
				}
				if submitted.Model != "gemini-3.8-flash" || !bytes.Equal(submitted.Payload, submittedOptions.OriginalRequest) || web.HasMedia(submitted.Payload) || !bytes.Contains(submitted.Payload, []byte(tc.text)) {
					t.Fatal("Enterprise did not receive the cleaned request and model", submitted.Model, string(submitted.Payload))
				}
				if tc.source == "history_text" && gjson.GetBytes(submitted.Payload, "contents.2.parts.0.text").String() != tc.text {
					t.Fatal("the historical user text was not made current", string(submitted.Payload))
				}
				if tc.body == currentInput && (!gjson.GetBytes(submitted.Payload, "tools.0.googleSearch").Exists() || gjson.GetBytes(submitted.Payload, "generationConfig.temperature").Float() != 0.2 || gjson.GetBytes(submitted.Payload, "systemInstruction.parts.0.text").String() != "system") {
					t.Fatal("fallback lost ordinary request settings", string(submitted.Payload))
				}
				counts, ok := state["media_fallback_requests"].(map[string]uint64)
				if !ok || len(counts) != 2 || counts[reason] != 1 || state["preflight_rejections"].(map[string]uint64)["channel_disabled"] != 0 {
					t.Fatal("fallback was not counted exactly once or was counted as a Web refusal", state)
				}
				files, err := os.ReadDir(logs)
				if err != nil || len(files) == 0 {
					t.Fatal("request log missing", err)
				}
				for _, file := range files {
					raw, err := os.ReadFile(filepath.Join(logs, file.Name()))
					if err != nil || bytes.Contains(raw, []byte("aW5wdXQ=")) || bytes.Contains(raw, []byte("files/private-attachment")) {
						t.Fatal("attachment content reached request logs", err)
					}
				}
			})
		}
	}
}

type fallbackBootstrapExecutor struct{ *phaseOnePlainExecutor }

func (e fallbackBootstrapExecutor) ExecuteStream(ctx context.Context, credential *auth.Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	_, _ = e.Execute(ctx, credential, req, opts)
	chunks := make(chan exec.StreamChunk, 1)
	chunks <- exec.StreamChunk{Err: fmt.Errorf("synthetic upstream failed before first chunk")}
	close(chunks)
	return &exec.StreamResult{Chunks: chunks}, nil
}

func TestEnterpriseWebMediaFallbackDoesNotReplayNativeStreamBootstrap(t *testing.T) {
	m, plain, e, ctx, upstream := phaseOneManager(t)
	config := *e.cfg
	config.GeminiEnterpriseWeb.Enabled = false
	config.Streaming.BootstrapRetries = 2
	m.SetConfig(&config)
	m.RegisterExecutor(fallbackBootstrapExecutor{plain})
	router := gin.New()
	router.Use(middleware.EnterpriseWebBodyAdmission(m))
	h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig, m))
	router.POST("/v1beta/models/*action", h.GeminiHandler)
	payload := phaseOneRequest("image/png").Payload
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse", bytes.NewReader(payload)).WithContext(ctx)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	counts := m.WebRuntime().Status()["media_fallback_requests"].(map[string]uint64)
	if response.Code < 400 || counts["web_disabled"] != 1 || plain.calls.Load() != 1 || len(upstream.calls) != 0 {
		t.Fatal("failed fallback bootstrap was replayed", response.Code, counts, plain.calls.Load(), upstream.calls)
	}
}
