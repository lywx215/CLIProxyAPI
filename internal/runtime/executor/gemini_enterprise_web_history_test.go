package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func historyTestConfig(serialized bool) *config.Config {
	return historyTestWithConfig(&config.Config{}, serialized)
}
func historyTestWithConfig(cfg *config.Config, serialized bool) *config.Config {
	if serialized {
		cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
	}
	return cfg
}
func historyTestPayload(payload string) string {
	return strings.TrimSuffix(payload, "]}") + `,{"role":"model","parts":[{"text":"HISTORY-MODEL-CODE"},{"functionCall":{"id":"history-call","name":"lookup","args":{"id":7}}}]},{"role":"user","parts":[{"functionResponse":{"id":"history-call","name":"lookup","response":{"value":"HISTORY-TOOL-CODE"}}},{"text":"Answer using the historical attachment"}]}]}`
}
func historyTestRequest(req exec.Request, serialized bool) exec.Request {
	if serialized {
		req.Payload = []byte(historyTestPayload(string(req.Payload)))
	}
	return req
}

func TestEnterpriseWebSerializedRejectsBeforeAuthenticationAndCountsOnce(t *testing.T) {
	m, plain, e, ctx, f := phaseOneManager(t)
	cfg := *e.cfg
	cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
	m.SetConfig(&cfg)
	input := historyTestRequest(phaseOneRequest("text/plain"), true)
	input.Payload = bytes.Replace(input.Payload, []byte(`"functionResponse"`), []byte(`"unknownContent"`), 1)
	_, err := m.Execute(ctx, []string{"gemini-enterprise", web.Provider}, input, phaseOneOptions())
	if err == nil || len(f.calls) != 0 || plain.calls.Load() != 0 {
		t.Fatal("invalid history reached upstream", err, f.calls)
	}
	if e.runtime.Status()["preflight_rejections"].(map[string]uint64)["unsupported_history_content"] != 1 || e.runtime.Status()["active_requests"] != 0 {
		t.Fatal(e.runtime.Status())
	}
	errBody := handlers.BuildErrorResponseBodyWithError(400, err.Error(), err)
	if gjson.GetBytes(errBody, "error.code").String() != "unsupported_history_content" || gjson.GetBytes(errBody, "error.param").String() == "" {
		t.Fatal("safe error classification lost", string(errBody))
	}
}

// Each node uses a separate manager, Cookie and loopback upstream. The second
// node has no access to the first node's session or cached conversation.
func TestEnterpriseWebSerializedAcrossTwoHTTPNodes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var previous string
			for node := 0; node < 2; node++ {
				e, a, ctx, f := phaseOneExecutor(t, historyTestConfig(true))
				a.ID = fmt.Sprintf("web-history-node-%d", node)
				credential := phaseOneCredential(t)
				credential.Cookies = fmt.Sprintf("SID=synthetic-node-%d; SAPISID=synthetic-signing", node)
				credential.SetCatalog(credential.AvailableModels, credential.CatalogUpdatedAt)
				a.Metadata["enterprise_web"] = credential.MetadataValue()
				m := auth.NewManager(nil, nil, nil)
				e.runtime = m.WebRuntime()
				m.SetConfig(e.cfg)
				m.RegisterExecutor(e)
				if _, err := m.Register(ctx, a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, web.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
				uploads := make(map[string]string)
				mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					origin, _ := url.Parse(r.Header.Get("X-Synthetic-Origin"))
					r.Header.Del("X-Synthetic-Origin")
					r.URL.Scheme, r.URL.Host = origin.Scheme, origin.Host
					if strings.HasSuffix(r.URL.Path, ":uploadFile") && r.Header.Get("X-Goog-Upload-Command") != "start" {
						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						uploads[r.Header.Get("X-Goog-Upload-File-Name")] = string(data)
						r.Body = io.NopCloser(bytes.NewReader(data))
					}
					res, err := f.roundTrip(r)
					if err != nil {
						t.Error(err)
						w.WriteHeader(502)
						return
					}
					defer func() { _ = res.Body.Close() }()
					for k, v := range res.Header {
						w.Header()[k] = v
					}
					w.WriteHeader(res.StatusCode)
					_, _ = io.Copy(w, res.Body)
				}))
				endpoint, _ := url.Parse(mock.URL)
				transport := &http.Transport{}
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Host != "vertexaisearch.cloud.google.com" && r.URL.Host != "ca-discoveryengine.clients6.google.com" {
						return nil, fmt.Errorf("unexpected synthetic destination")
					}
					clone := r.Clone(r.Context())
					u := *r.URL
					clone.URL = &u
					clone.Header.Set("X-Synthetic-Origin", r.URL.Scheme+"://"+r.URL.Host)
					clone.URL.Scheme, clone.URL.Host = endpoint.Scheme, endpoint.Host
					return transport.RoundTrip(clone)
				}))
				// The latest input is text. The second node has media only in
				// model history, including after an unclassified body prefix.
				payload := historyTestRequest(phaseOneRequest(), true)
				media := `,{"inlineData":{"mimeType":"image/png","data":"cG5nLWhpc3Rvcnk="}},{"text":"between attachments"},{"inlineData":{"mimeType":"application/pdf","data":"cGRmLWhpc3Rvcnk="}},{"inline_data":{"mime_type":"text/plain","data":"dHh0LWhpc3Rvcnk="}}`
				payload.Payload = bytes.Replace(payload.Payload, []byte(`{"text":"HISTORY-MODEL-CODE"}`), []byte(`{"text":"HISTORY-MODEL-CODE"}`+media), 1)
				if node == 0 {
					payload.Payload = bytes.Replace(payload.Payload, []byte(`{"text":"read these attachments"}`),
						[]byte(`{"text":"read these attachments"},{"inlineData":{"mimeType":"image/png","data":"dXNlci1wbmc="}}`), 1)
				}
				if previous != "" {
					payload.Payload = bytes.Replace(payload.Payload, []byte("HISTORY-MODEL-CODE"), []byte(previous), 1)
				}
				historyPrefix := ""
				if node == 1 {
					historyPrefix = strings.Repeat(`{"role":"model","parts":[{"text":"short"}]},`, 5000)
					payload.Payload = bytes.Replace(payload.Payload, []byte(`"contents":[`), []byte(`"contents":[`+historyPrefix), 1)
				}
				router := gin.New()
				router.Use(middleware.EnterpriseWebBodyAdmission(m))
				h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&e.cfg.SDKConfig, m))
				router.POST("/v1beta/models/*action", h.GeminiHandler)
				action := "generateContent"
				if stream {
					action = "streamGenerateContent?alt=sse"
				}
				response := httptest.NewRecorder()
				request := httptest.NewRequest("POST", "/v1beta/models/gemini-3.8-flash:"+action, bytes.NewReader(payload.Payload)).WithContext(ctx)
				if node == 1 {
					request.ContentLength = -1
				}
				router.ServeHTTP(response, request)
				transport.CloseIdleConnections()
				mock.Close()
				registry.GetGlobalRegistry().UnregisterClient(a.ID)
				wantUploads := 3
				if node == 0 {
					wantUploads++
				}
				if response.Code != 200 || f.calls["widgetCreateSession"] != 1 || f.calls["uploadFile"] != wantUploads || f.calls["widgetStreamAssist"] != 1 {
					t.Fatal(response.Code, response.Body.String(), f.calls)
				}
				query := gjson.GetBytes(f.final, "streamAssistRequest.query.parts.0.text").String()
				if !strings.Contains(query, "HISTORY-TOOL-CODE") || (previous != "" && !strings.Contains(query, previous)) {
					t.Fatal("cross-node context lost")
				}
				index := "0001"
				if node == 0 && uploads["c0000-p0001.png"] != "user-png" {
					t.Fatal("user and model attachments collided", uploads)
				}
				if node == 1 {
					index = "5001"
				}
				if len(uploads) != wantUploads || uploads["c"+index+"-p0001.png"] != "png-history" ||
					uploads["c"+index+"-p0003.pdf"] != "pdf-history" || uploads["c"+index+"-p0004.txt"] != "txt-history" ||
					!strings.Contains(query, `"role":"model","parts":[{"kind":"text"`) {
					t.Fatal("history attachments not reuploaded", uploads)
				}
				if e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
					t.Fatal("request resources retained")
				}
				if !stream {
					previous = gjson.GetBytes(response.Body.Bytes(), "candidates.0.content.parts.0.text").String()
				} else {
					previous = "hello world"
				}
				if previous == "" {
					t.Fatal("missing prior answer")
				}
			}
		})
	}
}

func TestEnterpriseWebSerializedPreservesPureTextRoute(t *testing.T) {
	m, plain, e, ctx, f := phaseOneManager(t)
	cfg := *e.cfg
	cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
	m.SetConfig(&cfg)
	contents := []map[string]any{{"role": "user", "parts": []any{map[string]string{"text": "first"}}}, {"role": "model", "parts": []any{map[string]string{"text": "answer"}}}, {"role": "user", "parts": []any{map[string]string{"text": "last"}}}}
	body, _ := json.Marshal(map[string]any{"contents": contents})
	_, err := m.Execute(ctx, []string{"gemini-enterprise", web.Provider}, exec.Request{Model: "gemini-3.8-flash", Payload: body}, phaseOneOptions())
	if err != nil || plain.calls.Load() != 1 || len(f.calls) != 0 || e.runtime.Status()["active_requests"] != 0 {
		t.Fatal("plain history route changed", err)
	}
}
