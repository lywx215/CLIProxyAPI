package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"golang.org/x/oauth2"
)

// This hook is absent when this benchmark is copied to the unmodified baseline.
var enterpriseWebPerformanceSetup func(*auth.Manager, *config.Config, bool) gin.HandlerFunc

type enterpriseTextPerformanceExecutor struct {
	initialFailureGeminiStreamExecutor
}

func (*enterpriseTextPerformanceExecutor) Identifier() string { return "gemini-enterprise" }
func (*enterpriseTextPerformanceExecutor) Execute(context.Context, *auth.Auth, exec.Request, exec.Options) (exec.Response, error) {
	return exec.Response{Payload: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)}, nil
}

// Exercise the real native HTTP, credential selection and Enterprise policy paths.
// Model execution is an identical immediate local mock in every revision and mode.
func BenchmarkEnterpriseWebEnterpriseTextHTTP(b *testing.B) { benchmarkEnterpriseWebTextHTTP(b, false) }

func BenchmarkEnterpriseWebEscapedTextHTTP(b *testing.B) { benchmarkEnterpriseWebTextHTTP(b, true) }

func benchmarkEnterpriseWebTextHTTP(b *testing.B, escaped bool) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("web=%t", enabled), func(b *testing.B) {
			cfg := &config.Config{}
			m := auth.NewManager(nil, nil, nil)
			m.SetConfig(cfg)
			m.RegisterExecutor(&enterpriseTextPerformanceExecutor{})
			a := &auth.Auth{ID: fmt.Sprintf("enterprise-web-text-performance-%t", enabled), Provider: "gemini-enterprise", Status: auth.StatusActive, Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "fixture-client", ProjectID: "123", EngineID: "fixture-engine"}, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic", Expiry: time.Now().Add(time.Hour)})}
			a.Metadata["model_policy"] = "catalog"
			if _, err := m.Register(context.Background(), a); err != nil {
				b.Fatal(err)
			}
			for _, account := range m.List() {
				registry.GetGlobalRegistry().RegisterClient(account.ID, account.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
				defer registry.GetGlobalRegistry().UnregisterClient(account.ID)
				if account.Provider == "gemini-enterprise" {
					trial := true
					lease := m.BeginEnterpriseSubscriptionRefresh(account)
					if lease == nil || !lease.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()}) {
						b.Fatal("subscription fixture failed")
					}
					m.PublishEnterprisePolicyCatalog(account, []ge.Model{{ID: "gemini-3.8-flash"}})
				}
			}
			h := NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m))
			r := gin.New()
			if enterpriseWebPerformanceSetup != nil {
				r.Use(enterpriseWebPerformanceSetup(m, cfg, enabled))
			}
			r.POST("/v1beta/models/*action", h.GeminiHandler)
			for _, size := range []int{1024, 32768} {
				b.Run(fmt.Sprintf("%dKiB", size/1024), func(b *testing.B) {
					text := strings.Repeat("x", size)
					if escaped {
						text = strings.Repeat(`\u4f60`, size/6) + strings.Repeat("x", size%6)
					}
					payload := `{"contents":[{"role":"user","parts":[{"text":"` + text + `"}]}]}`
					durations := make([]time.Duration, 0, min(b.N, 100000))
					call := func() {
						start := performanceNow()
						w := httptest.NewRecorder()
						req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:generateContent", strings.NewReader(payload))
						r.ServeHTTP(w, req)
						if w.Code != 200 {
							b.Fatal(w.Code, w.Body.String())
						}
						if len(durations) < cap(durations) {
							durations = append(durations, time.Duration(performanceNow()-start))
						}
					}
					call()
					durations = durations[:0]
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						call()
					}
					b.StopTimer()
					sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
					b.ReportMetric(float64(durations[(len(durations)-1)*95/100].Nanoseconds()), "p95-ns")
					b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "requests/s")
				})
			}
		})
	}
}
