package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type textPerformanceExecutor struct {
	initialFailureGeminiStreamExecutor
}

func (*textPerformanceExecutor) Execute(context.Context, *auth.Auth, exec.Request, exec.Options) (exec.Response, error) {
	return exec.Response{Payload: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)}, nil
}

// Fixed local transport isolates proxy costs from Google network/model variability.
func BenchmarkEnterpriseWebTextHTTP(b *testing.B) {
	gin.SetMode(gin.TestMode)
	m := auth.NewManager(nil, nil, nil)
	e := &textPerformanceExecutor{}
	m.RegisterExecutor(e)
	a := &auth.Auth{ID: "web-performance-baseline", Provider: e.Identifier(), Status: auth.StatusActive}
	if _, err := m.Register(context.Background(), a); err != nil {
		b.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "web-performance-text"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	h := NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, m))
	r := gin.New()
	r.POST("/v1beta/models/*action", h.GeminiHandler)
	for _, size := range []int{1024, 32768} {
		b.Run(strings.Repeat("k", size/1024), func(b *testing.B) {
			payload := `{"contents":[{"role":"user","parts":[{"text":"` + strings.Repeat("x", size) + `"}]}]}`
			durations := make([]time.Duration, 0, min(b.N, 100000))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1beta/models/web-performance-text:generateContent", strings.NewReader(payload))
				r.ServeHTTP(w, req)
				if w.Code != 200 {
					b.Fatal(w.Code, w.Body.String())
				}
				if len(durations) < cap(durations) {
					durations = append(durations, time.Since(start))
				}
			}
			b.StopTimer()
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			if len(durations) > 0 {
				b.ReportMetric(float64(durations[(len(durations)-1)*95/100].Nanoseconds()), "p95-ns")
			}
		})
	}
}
