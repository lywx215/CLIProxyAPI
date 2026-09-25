package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type diagnosticReadErrorBody struct {
	*strings.Reader
	closing, release chan struct{}
}

func (b *diagnosticReadErrorBody) Read(p []byte) (int, error) {
	if b.Reader.Len() > 0 {
		return b.Reader.Read(p)
	}
	return 0, errors.New("synthetic stream read failure")
}

func (b *diagnosticReadErrorBody) Close() error {
	close(b.closing)
	<-b.release
	return nil
}

func TestDIAG05GinReadErrorBeforeExchangeCleanup(t *testing.T) {
	coreusage.StartDefault(context.Background())
	cache.CacheSignature("diag05-fixture", "diagnostics fixture", strings.Repeat("x", cache.MinValidSignatureLen))
	defer cache.ClearSignatureCache("diag05-fixture")
	var artifact bytes.Buffer
	for _, provider := range []string{"gemini", "antigravity"} {
		for _, protocol := range []string{"gemini", "openai", "responses"} {
			for _, initial := range []string{"", `{"candidates":[{"content":{"parts":[{"text":"fixture output"}]}}]}`} {
				name := provider + "/" + protocol + "/bootstrap"
				if initial != "" {
					name = provider + "/" + protocol + "/partial"
				}
				t.Run(name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						gin.SetMode(gin.TestMode)
						var mu sync.Mutex
						var records []diagnostics.Record
						engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
							var r diagnostics.Record
							if err := json.Unmarshal(b[6:], &r); err != nil {
								return err
							}
							mu.Lock()
							records = append(records, r)
							artifact.Write(b)
							mu.Unlock()
							return nil
						})
						frame := initial
						if frame != "" {
							if provider == "antigravity" {
								frame = `{"response":` + frame + `}`
							}
							frame = "data: " + frame + "\n\n"
						}
						body := &diagnosticReadErrorBody{strings.NewReader(frame), make(chan struct{}), make(chan struct{})}
						manager := cliproxyauth.NewManager(nil, nil, nil)
						if provider == "gemini" {
							manager.RegisterExecutor(NewGeminiExecutor(&config.Config{}))
						} else {
							manager.RegisterExecutor(NewAntigravityExecutor(&config.Config{}))
						}
						manager.SetRoundTripperProvider(cancellationTransportProvider{semanticRT(func(*http.Request) (*http.Response, error) {
							return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
						})})
						authID, model := "diag05-read-auth", "gemini-2.5-flash"
						_, err := manager.Register(context.Background(), &cliproxyauth.Auth{ID: authID, Provider: provider, Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": "http://fixture.invalid", "api_key": "fixture-key"}, Metadata: map[string]any{"access_token": "fixture-token", "expired": time.Now().Add(24 * time.Hour).Format(time.RFC3339), "project_id": "fixture-project", "disable_cooling": true}})
						if err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
						defer registry.GetGlobalRegistry().UnregisterClient(authID)
						base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager)
						router := gin.New()
						router.Use(engine.GinMiddleware(func(*gin.Context) string { return "read-error-fixture" }))
						path, request := "/v1beta/models/"+model+":streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`
						if protocol == "gemini" {
							router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(base).GeminiHandler)
						} else if protocol == "responses" {
							path, request = "/v1/responses", `{"model":"`+model+`","stream":true,"input":"fixture"}`
							router.POST(path, openai.NewOpenAIResponsesAPIHandler(base).Responses)
						} else {
							path, request = "/v1/chat/completions", `{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"fixture"}]}`
							router.POST(path, openai.NewOpenAIAPIHandler(base).ChatCompletions)
						}
						writer := httptest.NewRecorder()
						handlerDone := make(chan struct{})
						go func() {
							defer close(handlerDone)
							router.ServeHTTP(writer, httptest.NewRequest("POST", path, strings.NewReader(request)))
						}()
						<-body.closing
						<-handlerDone // Real handlers return on Err without draining or releasing Close.
						mu.Lock()
						attempts, servers := 0, 0
						for _, r := range records {
							d, _ := r.Data.(map[string]any)
							if r.Event == "upstream.attempt_finished" {
								attempts++
								if servers != 0 || d["resultClass"] != "error" || d["failureStage"] != "read" || d["eofSeen"] != false {
									t.Errorf("read evidence lost: %v", d)
								}
							}
							if r.Event == "diag.server" {
								servers++
								if d["coverage"].(map[string]any)["debugCapture"] != "enabled_throughout" {
									t.Errorf("error delivery preceded settlement: %v", d)
								}
							}
						}
						before := len(records)
						mu.Unlock()
						completedTail := provider == "gemini" && protocol == "responses" && initial != ""
						if attempts != 1 || servers != 1 || (!completedTail && !strings.Contains(writer.Body.String(), `"error"`)) {
							t.Errorf("attempt=%d server=%d business error=%s", attempts, servers, writer.Body.String())
						}
						// Gemini's Responses converter emits a completed tail before Err; the forwarder
						// treats it as payload and still receives the error afterwards.
						if provider == "gemini" && protocol == "responses" && initial != "" && !strings.Contains(writer.Body.String(), "response.completed") {
							t.Error("existing Gemini DONE tail changed")
						}
						if provider == "antigravity" && (strings.Contains(writer.Body.String(), "[DONE]") || strings.Contains(writer.Body.String(), "response.completed")) {
							t.Error("Antigravity read error synthesized a clean tail")
						}
						close(body.release)
						synctest.Wait()
						mu.Lock()
						if len(records) != before {
							t.Error("late duplicate terminal")
						}
						mu.Unlock()
					})
				})
			}
		}
	}
	if path := os.Getenv("DIAG05_R2_TEST_RECORDS"); path != "" {
		if err := os.WriteFile(path, artifact.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
