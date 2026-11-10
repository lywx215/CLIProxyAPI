package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type webFailingBody struct{}

func (webFailingBody) Read([]byte) (int, error) {
	return 0, errors.New("private synthetic request reader detail")
}
func (webFailingBody) Close() error { return nil }

func TestEnterpriseWebAdmissionErrorsPreserveSafeClassifications(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, wantType, wantCode, wantParam string
		status                              int
	}{
		{"input-limit", "invalid_request_error", "input_limit_exceeded", "body", 413},
		{"capacity", "rate_limit_error", "media_input_error", "", 429},
		{"closed", "server_error", "media_input_error", "", 503},
		{"invalid-length", "invalid_request_error", "media_input_error", "", 400},
		{"private-reader-error", "server_error", "internal_server_error", "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{GeminiEnterpriseWeb: config.GeminiEnterpriseWebConfig{Enabled: tc.name != "closed", MaxConcurrent: 1}}
			if tc.name == "input-limit" {
				cfg.GeminiEnterpriseWeb.InputLimits.MaxRequestBytes = 64
			}
			manager := auth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			const model = "synthetic-admission-model"
			registry.GetGlobalRegistry().RegisterClient(t.Name(), "gemini-enterprise", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(t.Name())
			if tc.name == "capacity" {
				_, lease, err := manager.WebRuntime().Acquire(context.Background(), 1024)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
			}
			router := gin.New()
			router.Use(EnterpriseWebBodyAdmission(manager))
			router.POST("/v1beta/models/*action", func(c *gin.Context) { t.Error("rejected body reached execution") })
			body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"text/plain","data":"eA=="}}]}]}`
			action := "generateContent"
			if tc.name == "capacity" || tc.name == "closed" {
				action = "countTokens"
			}
			request := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":"+action, strings.NewReader(body))
			if tc.name == "invalid-length" {
				request.ContentLength++
			}
			if tc.name == "private-reader-error" {
				request.Body = webFailingBody{}
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			raw := response.Body.String()
			if response.Code != tc.status || gjson.Get(raw, "error.type").String() != tc.wantType || gjson.Get(raw, "error.code").String() != tc.wantCode || gjson.Get(raw, "error.param").String() != tc.wantParam {
				t.Fatal("admission error disagrees with execution error contract", response.Code, raw)
			}
			if strings.Contains(raw, "private synthetic") || strings.Contains(raw, "UNAVAILABLE") || strings.Contains(raw, "eA==") {
				t.Fatal("error disclosed request content or retained an incorrect classification", raw)
			}
			if tc.name == "input-limit" && manager.WebRuntime().Status()["preflight_rejections"].(map[string]uint64)["input_limit_exceeded"] != 1 {
				t.Fatal("read-time media rejection was not counted exactly once")
			}
			if tc.name != "capacity" && (manager.WebRuntime().Status()["active_requests"] != 0 || manager.WebRuntime().Status()["inflight_bytes"] != int64(0)) {
				t.Fatal("rejected body leaked admission")
			}
		})
	}
}
