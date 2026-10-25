package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageBackgroundContextRecordsSuccessWithoutResponseWriter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		background bool
		status     int
		failed     bool
		wantFailed bool
	}{
		{name: "background success", background: true},
		{name: "background explicit failure", background: true, failed: true, wantFailed: true},
		{name: "live 200 success", status: http.StatusOK},
		{name: "live 500 failure", status: http.StatusInternalServerError, wantFailed: true},
		{name: "live 200 explicit failure", status: http.StatusOK, failed: true, wantFailed: true},
		{name: "live 500 explicit failure", status: http.StatusInternalServerError, failed: true, wantFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ginCtx *gin.Context
			if tc.background {
				// Async video jobs retain request metadata without retaining the live writer.
				ginCtx = &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1beta/models/veo-3.1:predictLongRunning", nil)}
			} else {
				ginCtx, _ = gin.CreateTestContext(httptest.NewRecorder())
				ginCtx.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/videos", nil)
				ginCtx.Writer.WriteHeader(tc.status)
			}
			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			requestedAt := time.Date(2026, time.October, 23, 15, 0, 0, 0, time.UTC)
			stats := NewRequestStatistics()
			stats.Record(ctx, coreusage.Record{
				APIKey:      "background-fixture",
				Provider:    "gemini-enterprise",
				Model:       "veo-3.1",
				RequestedAt: requestedAt,
				Failed:      tc.failed,
				Detail:      coreusage.Detail{InputTokens: 7, OutputTokens: 2, TotalTokens: 9},
			})
			snapshot := stats.Snapshot()
			wantSuccess, wantFailure := int64(1), int64(0)
			if tc.wantFailed {
				wantSuccess, wantFailure = 0, 1
			}
			if snapshot.TotalRequests != 1 || snapshot.SuccessCount != wantSuccess || snapshot.FailureCount != wantFailure || snapshot.TotalTokens != 9 {
				t.Fatalf("usage record was lost or misclassified: %+v", snapshot)
			}
			api := snapshot.APIs["background-fixture"]
			model := api.Models["veo-3.1"]
			if api.TotalRequests != 1 || api.TotalTokens != 9 || model.TotalRequests != 1 || model.TotalTokens != 9 || len(model.Details) != 1 {
				t.Fatalf("API/model usage was lost or duplicated: %+v", api)
			}
			detail := model.Details[0]
			if detail.Failed != tc.wantFailed || detail.Tokens.InputTokens != 7 || detail.Tokens.OutputTokens != 2 || detail.Tokens.TotalTokens != 9 {
				t.Fatalf("explicit outcome or text token count changed: %+v", detail)
			}
			if snapshot.RequestsByDay["2026-10-23"] != 1 || snapshot.RequestsByHour["15"] != 1 || snapshot.TokensByDay["2026-10-23"] != 9 || snapshot.TokensByHour["15"] != 9 {
				t.Fatal("time bucket accounting lost or duplicated the record")
			}
			if again := stats.Snapshot(); again.TotalRequests != 1 || again.TotalTokens != 9 {
				t.Fatal("snapshotting changed usage accounting")
			}
		})
	}
}

func TestUsageRequestSnapshotsSurviveGinContextReuse(t *testing.T) {
	for _, status := range []int{0, http.StatusOK, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginCtx.Request = httptest.NewRequest(http.MethodPost, "/original", nil)
			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			ctx = logging.WithEndpoint(ctx, "POST /original")
			ctx = logging.WithResponseStatusHolder(ctx)
			logging.SetResponseStatus(ctx, status)
			ctx = coreauth.WithAntigravityCreditsUsage(ctx)

			// A queued usage record can outlive the handler and its pooled Gin context.
			ginCtx.Request = httptest.NewRequest(http.MethodGet, "/recycled", nil)
			ginCtx.Writer = nil
			changed := make(chan struct{})
			go func() {
				defer close(changed)
				for range 1000 {
					ginCtx.Request = httptest.NewRequest(http.MethodDelete, "/another-request", nil)
					ginCtx.Writer = nil
				}
			}()
			stats := NewRequestStatistics()
			for range 1000 {
				stats.Record(ctx, coreusage.Record{Provider: "fixture", Model: "model"})
			}
			<-changed
			snapshot := stats.Snapshot()
			api := snapshot.APIs["POST /original"]
			if api.TotalRequests != 1000 {
				t.Fatalf("request endpoint changed after Gin reuse: %+v", snapshot.APIs)
			}
			wantFailures := int64(0)
			if status >= http.StatusBadRequest {
				wantFailures = 1000
			}
			if snapshot.FailureCount != wantFailures || snapshot.SuccessCount != 1000-wantFailures {
				t.Fatalf("request outcome changed after Gin reuse: %+v", snapshot)
			}
		})
	}
}
