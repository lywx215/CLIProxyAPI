package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestUsageExportImportRetainsCreditsAndDeduplicates(t *testing.T) {
	h := &Handler{usageStats: usage.NewRequestStatistics()}
	router := gin.New()
	router.POST("/observability/usage/import", h.ImportUsageStatistics)
	router.GET("/observability/usage/export", h.ExportUsageStatistics)
	router.GET("/observability/usage", h.GetUsageStatistics)
	payload := usageImportPayload{Version: 1, Usage: usage.StatisticsSnapshot{APIs: map[string]usage.APISnapshot{
		"synthetic-client": {Models: map[string]usage.ModelSnapshot{
			"claude-test": {Details: []usage.RequestDetail{{Timestamp: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), Source: "synthetic-auth", AuthIndex: "synthetic-index", CreditsUsed: true, Tokens: usage.TokenStats{InputTokens: 3, OutputTokens: 7, TotalTokens: 10}}}},
		}},
	}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/observability/usage/import", strings.NewReader(string(encoded))))
		if response.Code != http.StatusOK {
			t.Fatalf("import status=%d body=%s", response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/observability/usage/export", nil))
	var exported usageExportPayload
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &exported) != nil {
		t.Fatalf("export status=%d body=%s", response.Code, response.Body.String())
	}
	details := exported.Usage.APIs["synthetic-client"].Models["claude-test"].Details
	if exported.Version != 1 || exported.Usage.TotalRequests != 1 || exported.Usage.TotalTokens != 10 || len(details) != 1 || !details[0].CreditsUsed || details[0].AuthIndex != "synthetic-index" {
		t.Fatalf("import/export changed fork usage detail: %#v", exported)
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/observability/usage", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"credits_used":true`) {
		t.Fatalf("snapshot lost credits flag: %s", response.Body.String())
	}
}

func TestUsageQueuePreservesBatchDefaultAndInvalidRecords(t *testing.T) {
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte(`{"id":1}`))
		redisqueue.Enqueue([]byte("plugin-invalid-json"))
		router := gin.New()
		h := &Handler{}
		router.GET("/queue", h.GetUsageQueue)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/queue", nil))
		var records []json.RawMessage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &records) != nil || len(records) != 2 || string(records[1]) != `"plugin-invalid-json"` {
			t.Fatalf("queue default or invalid-record contract changed: status=%d body=%s", response.Code, response.Body.String())
		}
		if remaining := redisqueue.PopOldest(100); len(remaining) != 0 {
			t.Fatalf("default batch did not consume its records: %q", remaining)
		}
	})
}
