package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	"github.com/tidwall/gjson"
)

func TestEnterpriseWebCompatibleHistoryThroughNativeHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			m, plain, e, ctx, f := phaseOneManager(t)
			cfg := *e.cfg
			cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
			m.SetConfig(&cfg)
			e.cfg = &cfg
			body := []byte(`{"contents":[{"role":"model","parts":[{"text":"TRUNCATED-HISTORY"}]},{"parts":[{"inlineData":{"mimeType":"text/plain","data":"aW5wdXQ="}},{"text":"CURRENT-QUESTION"}]},{"role":"model","parts":[{"text":"ANSWER-PREFIX"}]}]}`)
			router := gin.New()
			router.Use(middleware.EnterpriseWebBodyAdmission(m))
			h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&e.cfg.SDKConfig, m))
			router.POST("/v1beta/models/*action", h.GeminiHandler)
			action := "generateContent"
			if stream {
				action = "streamGenerateContent?alt=sse"
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/v1beta/models/gemini-3.8-flash:"+action, bytes.NewReader(body)).WithContext(ctx)
			router.ServeHTTP(response, request)
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"finishReason":"STOP"`) || f.calls["widgetCreateSession"] != 1 || f.calls["uploadFile"] != 1 || f.calls["widgetStreamAssist"] != 1 || plain.calls.Load() != 0 {
				t.Fatal("compatible history failed or fell back to text", response.Code, response.Body.String(), f.calls)
			}
			query := gjson.GetBytes(f.final, "streamAssistRequest.query.parts.0.text").String()
			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(query[strings.Index(query, "\n")+1:]), &doc); err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(doc["history"], "0.role").String() != "model" || gjson.GetBytes(doc["current_input"], "parts.1.text").String() != "CURRENT-QUESTION" || gjson.GetBytes(doc["model_continuation"], "0.parts.0.text").String() != "ANSWER-PREFIX" {
				t.Fatal("history roles or current input/prefix changed", query)
			}
			if e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
				t.Fatal("history resources leaked")
			}
		})
	}
}

func TestEnterpriseWebLimitDetailSurvivesManagerBeforeUpstream(t *testing.T) {
	m, plain, e, ctx, f := phaseOneManager(t)
	cfg := *e.cfg
	cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
	m.SetConfig(&cfg)
	input := phaseOneRequest("text/plain")
	input.Payload = bytes.Replace(input.Payload, []byte("read these attachments"), []byte(strings.Repeat("x", int(cfg.GeminiEnterpriseWeb.InputLimits.WithDefaults().MaxTextBytes)+1)), 1)
	_, err := m.Execute(ctx, []string{"gemini-enterprise", web.Provider}, input, phaseOneOptions())
	var failure *web.Failure
	if !errors.As(err, &failure) || failure.Code != 413 || failure.Limit == nil || failure.Limit.Name != contract.LimitHistoryTextBytes || failure.Param != "contents[0].parts[0].text" {
		t.Fatalf("limit evidence lost through executor/manager: %#v (%v)", failure, err)
	}
	if len(f.calls) != 0 || plain.calls.Load() != 0 || e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["preflight_rejections"].(map[string]uint64)["history_limit_exceeded"] != 1 {
		t.Fatal("limit failure reached upstream, leaked admission, or was counted incorrectly", f.calls, e.runtime.Status())
	}
}

func TestEnterpriseWebConfiguredHistoryGrowsOnlyWithinGlobalBudget(t *testing.T) {
	for _, budgetBytes := range []int64{64 << 20, 256 << 20} {
		t.Run(fmt.Sprintf("budget=%d", budgetBytes), func(t *testing.T) {
			cfg := historyTestConfig(true)
			cfg.GeminiEnterpriseWeb.MaxInflightBytes = budgetBytes
			e, a, ctx, f := phaseOneExecutor(t, cfg)
			input := phaseOneRequest("text/plain")
			input.Payload = bytes.Replace(input.Payload, []byte("read these attachments"), []byte(strings.Repeat("x", 2<<20)), 1)
			n := int64(len(input.Payload))
			initial := 2*n + n*3/4 + (32 << 20)
			var reserved int64
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(request *http.Request) (*http.Response, error) {
				reserved = e.runtime.Status()["inflight_bytes"].(int64)
				return f.roundTrip(request)
			}))
			_, err := e.Execute(ctx, a, input, phaseOneOptions())
			if budgetBytes == 64<<20 {
				var failure *web.Failure
				if !errors.As(err, &failure) || failure.Code != 429 || len(f.calls) != 0 {
					t.Fatal("insufficient memory reached authentication or was misclassified", err, f.calls)
				}
			} else if err != nil || reserved <= initial || reserved > budgetBytes || f.calls["widgetStreamAssist"] != 1 {
				t.Fatal("configured text failed to grow within the provider budget", err, reserved, initial, f.calls)
			}
			if e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
				t.Fatal("history growth reservation leaked", e.runtime.Status())
			}
		})
	}
}
