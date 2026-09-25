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

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type cancellationTransportProvider struct{ http.RoundTripper }

func (p cancellationTransportProvider) RoundTripperFor(*cliproxyauth.Auth) http.RoundTripper {
	return p.RoundTripper
}

// The read ends on cancellation, but the existing executor Body.Close is held
// behind a channel. This forces Gin's server terminal before Exchange.Finish.
type cancellationCleanupBody struct {
	ctx                       context.Context
	first                     []byte
	reading, closing, release chan struct{}
}

func (b *cancellationCleanupBody) Read(p []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	close(b.reading)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancellationCleanupBody) Close() error {
	close(b.closing)
	<-b.release
	return errors.New("synthetic delayed cleanup failure")
}

func TestDIAG05GinCancellationBeforeExchangeCleanup(t *testing.T) {
	// Keep the process-wide usage dispatcher outside the synctest bubble.
	coreusage.StartDefault(context.Background())
	var artifact bytes.Buffer
	for _, throttle := range []bool{false, true} {
		name := "waiting-upstream"
		if throttle {
			name = "waiting-throttle"
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
				body := &cancellationCleanupBody{reading: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{})}
				if throttle {
					body.first = []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"fixture output\"}]}}]}\n\n")
				}
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(NewGeminiExecutor(&config.Config{}))
				manager.SetRoundTripperProvider(cancellationTransportProvider{semanticRT(func(r *http.Request) (*http.Response, error) {
					body.ctx = r.Context()
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
				})})
				authID, model := "diag05-cancel-auth", "gemini-2.5-flash"
				if _, err := manager.Register(context.Background(), &cliproxyauth.Auth{ID: authID, Provider: "gemini", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": "http://fixture.invalid"}}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(authID, "gemini", []*registry.ModelInfo{{ID: model}})
				defer registry.GetGlobalRegistry().UnregisterClient(authID)
				cfg := &config.SDKConfig{SpeedThrottle: config.SpeedThrottleConfig{Enabled: throttle, MinTokensPerSecond: 1, MaxTokensPerSecond: 1, MinFirstTokenDelayMs: 1000, MaxFirstTokenDelayMs: 1000}}
				router := gin.New()
				router.Use(engine.GinMiddleware(func(*gin.Context) string { return "cancel-fixture" }))
				router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager)).GeminiHandler)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req := httptest.NewRequest("POST", "/v1beta/models/"+model+":streamGenerateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)).WithContext(ctx)
				writer := httptest.NewRecorder()
				handlerDone := make(chan struct{})
				go func() { defer close(handlerDone); router.ServeHTTP(writer, req) }()
				<-body.reading
				synctest.Wait()
				cancel()
				<-body.closing
				<-handlerDone // No stream drain and no cleanup release are needed to return.
				mu.Lock()
				serverCount, attempts, converted := 0, 0, 0
				for _, r := range records {
					switch r.Event {
					case "upstream.attempt_finished":
						attempts++
					case "response.converted":
						converted++
					case "diag.server":
						serverCount++
						d := r.Data.(map[string]any)
						coverage := d["coverage"].(map[string]any)
						if coverage["debugCapture"] != "interrupted" {
							t.Errorf("pending exchange silently omitted: coverage=%v", coverage)
						}
						if d["endReason"] != "client_cancel" || coverage["expectedLastLogSeq"] != float64(r.LogSeq) {
							t.Errorf("server terminal changed: %v", d)
						}
					}
				}
				mu.Unlock()
				if serverCount != 1 || attempts != 0 || converted != 0 || strings.Contains(writer.Body.String(), "fixture output") {
					t.Errorf("unexpected pre-cleanup result: server=%d attempt=%d converted=%d body=%d", serverCount, attempts, converted, writer.Body.Len())
				}
				close(body.release)
				synctest.Wait()
				mu.Lock()
				defer mu.Unlock()
				for _, r := range records {
					if r.Event == "upstream.attempt_finished" || r.Event == "response.converted" {
						t.Error("late observation appended after sealed server")
					}
				}
			})
		})
	}
	if path := os.Getenv("DIAG05_R1_TEST_RECORDS"); path != "" {
		if err := os.WriteFile(path, artifact.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
