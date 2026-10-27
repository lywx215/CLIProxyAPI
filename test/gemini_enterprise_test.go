package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	stats "github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

var enterpriseFixtureModels = []string{
	"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash",
	"gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro",
}

type enterpriseFixtureTransport struct {
	calls          atomic.Int32
	mu             sync.Mutex
	lastBody       []byte
	upstreamStatus int
	upstreamBody   string
}

func (f *enterpriseFixtureTransport) requestBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.lastBody...)
}

func (f *enterpriseFixtureTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return f }
func (f *enterpriseFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "discoveryengine.googleapis.com" {
		return nil, io.ErrUnexpectedEOF
	}
	f.calls.Add(1)
	body, _ := io.ReadAll(req.Body)
	f.mu.Lock()
	f.lastBody = append([]byte(nil), body...)
	f.mu.Unlock()
	if f.upstreamStatus != 0 {
		return &http.Response{StatusCode: f.upstreamStatus, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(f.upstreamBody))}, nil
	}
	text := "offline 前🙂引用"
	if strings.Contains(gjson.GetBytes(body, "query.text").String(), "Return a JSON object") {
		text = `{"ok":true}`
	}
	runes := []rune(text)
	split := len(runes) / 2
	content, _ := json.Marshal(map[string]any{"answer": map[string]any{"state": "IN_PROGRESS", "replies": []map[string]any{
		{"content": map[string]any{"executableCode": map[string]any{"language": "PYTHON", "code": "print(7)"}}},
		{"content": map[string]any{"codeExecutionResult": map[string]any{"outcome": "OUTCOME_OK", "output": "7\n"}}},
		{"content": map[string]any{"inlineData": map[string]string{"mimeType": "application/json+suggestions", "data": "e30="}}},
		{"content": map[string]string{"text": string(runes[:split])}},
		{"content": map[string]string{"text": string(runes[split:])}},
	}}})
	terminal, _ := json.Marshal(map[string]any{"answer": map[string]any{"state": "SUCCEEDED", "replies": []map[string]any{{"groundedContent": map[string]any{
		"textGroundingMetadata": map[string]any{"references": []map[string]any{{"documentMetadata": map[string]string{"uri": "https://example.com/sdk-fixture", "title": "Offline fixture"}}}, "segments": []map[string]any{{"startIndex": 15, "endIndex": 21, "text": "引用", "referenceIndices": []int{0}}}},
	}}}}})
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("[" + string(content) + "," + string(terminal) + "]"))}, nil
}

func enterpriseFixtureRouter(t *testing.T) (*gin.Engine, *enterpriseFixtureTransport, *coreauth.Manager) {
	t.Helper()
	return enterpriseFixtureRouterWithConfig(t, &config.Config{})
}

func enterpriseFixtureRouterWithConfig(t *testing.T, cfg *config.Config) (*gin.Engine, *enterpriseFixtureTransport, *coreauth.Manager) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	transport := &enterpriseFixtureTransport{}
	manager.SetRoundTripperProvider(transport)
	manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
	id := "enterprise-wire-fixture"
	metadata := ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "fixture-client", ProjectID: "fixture-project", EngineID: "fixture-engine"}, "fixture@example.com", &oauth2.Token{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour)})
	metadata["model_policy"] = "catalog"
	registered, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	models := make([]*registry.ModelInfo, 0, len(enterpriseFixtureModels))
	catalog := make([]ge.Model, 0, len(enterpriseFixtureModels))
	for _, model := range enterpriseFixtureModels {
		models = append(models, &registry.ModelInfo{ID: model, Name: "models/" + model, Type: "gemini", OwnedBy: ge.Provider, SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"}})
		catalog = append(catalog, ge.Model{ID: model})
	}
	trial := true
	enterprisePublishSubscription(t, manager, registered, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()})
	manager.PublishEnterprisePolicyCatalog(registered, catalog)
	registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	chat := openaihandlers.NewOpenAIAPIHandler(base)
	gemini := geminihandlers.NewGeminiAPIHandler(base)
	router := gin.New()
	router.POST("/v1/chat/completions", chat.ChatCompletions)
	router.POST("/v1/completions", chat.Completions)
	router.GET("/v1/models", chat.OpenAIModels)
	router.GET("/v1beta/models", gemini.GeminiModels)
	router.POST("/v1beta/models/*action", gemini.GeminiHandler)
	return router, transport, manager
}

func enterprisePublishSubscription(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth, observation ge.SubscriptionObservation) {
	t.Helper()
	manager.InvalidateEnterpriseSubscription(auth)
	flight := manager.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil || !flight.Complete(observation) {
		t.Fatal("could not publish the synthetic runtime subscription")
	}
}

func enterprisePublicRequest(t *testing.T, model, protocol string, stream bool, fields map[string]any) (string, []byte) {
	t.Helper()
	path := "/v1/chat/completions"
	request := map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": stream}
	if protocol == "gemini" {
		action := "generateContent"
		if stream {
			action = "streamGenerateContent?alt=sse"
		}
		path = "/v1beta/models/" + model + ":" + action
		request = map[string]any{"contents": []map[string]any{{"parts": []map[string]any{{"text": "hi"}}}}}
	}
	for key, value := range fields {
		request[key] = value
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return path, body
}

func enterprisePublicCall(router *gin.Engine, path string, body []byte) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	return response
}

func enterpriseAssertPublicErrorChannelNeutral(t *testing.T, body []byte) {
	t.Helper()
	lower := strings.ToLower(string(body))
	for _, private := range []string{"gemini enterprise", "gemini-enterprise", "gemini_enterprise", "geminienterprise", "streamassist", "discoveryengine"} {
		if strings.Contains(lower, private) {
			t.Fatalf("public error exposed internal channel detail %q: %s", private, body)
		}
	}
}

func enterprisePublicText(body []byte, protocol string, stream bool) string {
	packets := []string{string(body)}
	if stream {
		packets = nil
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "data:") {
				packets = append(packets, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
	var text strings.Builder
	for _, packet := range packets {
		root := gjson.Parse(packet)
		if protocol == "gemini" {
			for _, part := range root.Get("candidates.0.content.parts").Array() {
				text.WriteString(part.Get("text").String())
			}
		} else if stream {
			text.WriteString(root.Get("choices.0.delta.content").String())
		} else {
			text.WriteString(root.Get("choices.0.message.content").String())
		}
	}
	return text.String()
}

func TestEnterprisePublicPayloadValidationAllModels(t *testing.T) {
	const privateValue = "synthetic-private-payload-value"
	modelRules := []config.PayloadModelRule{{Name: "gemini-*", Protocol: ge.Provider}}
	for _, scenario := range []struct {
		name, field, reason string
		params              map[string]any
		filter              []string
	}{
		{"native generation config", "generationConfig", "is not supported", map[string]any{"generationConfig.thinkingConfig.includeThoughts": true}, nil},
		{"native system instruction", "systemInstruction", "is not supported", map[string]any{"systemInstruction.parts": []map[string]string{{"text": privateValue}}}, nil},
		{"unknown nested field", "generationSpec.temperature", "is not supported", map[string]any{"generationSpec.temperature": privateValue}, nil},
		{"wrong object type", "toolsSpec.webGroundingSpec", "must be an object", map[string]any{"toolsSpec.webGroundingSpec": []string{privateValue}}, nil},
		{"null text", "languageCode", "must be text", map[string]any{"languageCode": nil}, nil},
		{"filtered required field", "query.text", "is required", nil, []string{"query.text"}},
		{"internal channel field", "body", "is not supported", map[string]any{"gemini_enterprise_private": privateValue}, nil},
		{"internal endpoint field", "body", "is not supported", map[string]any{"streamAssist": privateValue}, nil},
		{"internal host field", "body", "is not supported", map[string]any{"discoveryengine": privateValue}, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{}}
			if scenario.params != nil {
				cfg.Payload.Override = []config.PayloadRule{{Models: modelRules, Params: scenario.params}}
			}
			if scenario.filter != nil {
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: modelRules, Params: scenario.filter}}
			}
			router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
			for _, model := range enterpriseFixtureModels {
				for _, protocol := range []string{"openai", "gemini"} {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
							path, body := enterprisePublicRequest(t, model, protocol, stream, nil)
							response := enterprisePublicCall(router, path, body)
							wire := response.Body.Bytes()
							if response.Code != http.StatusBadRequest || gjson.GetBytes(wire, "error.code").String() != "invalid_field" || gjson.GetBytes(wire, "error.param").String() != scenario.field || gjson.GetBytes(wire, "error.type").String() != "invalid_request_error" {
								t.Fatalf("invalid field classification lost: status=%d body=%s", response.Code, wire)
							}
							message := gjson.GetBytes(wire, "error.message").String()
							for _, expected := range []string{scenario.field, scenario.reason, "Check the server request payload configuration."} {
								if !strings.Contains(message, expected) {
									t.Fatalf("missing diagnostic %q: %s", expected, wire)
								}
							}
							for _, forbidden := range []string{privateValue, "fixture-access", "fixture-refresh"} {
								if strings.Contains(string(wire), forbidden) {
									t.Fatalf("field error exposed %q: %s", forbidden, wire)
								}
							}
							if transport.calls.Load() != 0 {
								t.Fatal("invalid final payload reached upstream")
							}
							enterpriseAssertPublicErrorChannelNeutral(t, wire)
						})
					}
				}
			}
		})
	}
}

func TestEnterprisePublicUpstreamErrorsAreChannelNeutralAllModels(t *testing.T) {
	const privateValue = "synthetic-private-upstream-value"
	for _, scenario := range []struct {
		name, raw, code, param string
		status                 int
	}{
		{"bad field", `{"error":{"message":"Gemini Enterprise: synthetic-private-upstream-value","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"generationSpec.modelId"}]}]}}`, "upstream_invalid_request", "generationSpec.modelId", http.StatusBadRequest},
		{"model permission", `{"error":{"message":"Model synthetic-private-upstream-value is not allowed"}}`, "model_access_denied", "model", http.StatusBadRequest},
		{"permission", `{"error":{"message":"Gemini Enterprise: synthetic-private-upstream-value"}}`, "permission_denied", "", http.StatusForbidden},
		{"missing resource", `{"error":{"message":"discoveryengine.googleapis.com synthetic-private-upstream-value"}}`, "resource_not_found", "", http.StatusNotFound},
		{"invalid request", `{"error":{"message":"streamAssist synthetic-private-upstream-value"}}`, "upstream_invalid_request", "", http.StatusUnprocessableEntity},
		{"server failure", `{"error":{"message":"gemini_enterprise_upstream_error synthetic-private-upstream-value"}}`, "upstream_error", "", http.StatusServiceUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for _, model := range enterpriseFixtureModels {
				for _, protocol := range []string{"openai", "gemini"} {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
							router, transport, _ := enterpriseFixtureRouter(t)
							transport.upstreamStatus, transport.upstreamBody = scenario.status, scenario.raw
							path, body := enterprisePublicRequest(t, model, protocol, stream, nil)
							response := enterprisePublicCall(router, path, body)
							wire := response.Body.Bytes()
							if response.Code != scenario.status || gjson.GetBytes(wire, "error.code").String() != scenario.code || gjson.GetBytes(wire, "error.param").String() != scenario.param {
								t.Fatalf("upstream classification lost: status=%d body=%s", response.Code, wire)
							}
							if transport.calls.Load() == 0 || strings.Contains(string(wire), privateValue) {
								t.Fatalf("upstream fixture was not called or a private value leaked: calls=%d body=%s", transport.calls.Load(), wire)
							}
							enterpriseAssertPublicErrorChannelNeutral(t, wire)
						})
					}
				}
			}
		})
	}
}

func TestEnterprisePublicOptionalParametersAllModels(t *testing.T) {
	const privateValue = "synthetic-private-optional-value"
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gemini-*", Protocol: "gemini"}},
		Params: map[string]any{"generationConfig.thinkingConfig.includeThoughts": true, "systemInstruction.parts": []map[string]string{{"text": privateValue}}},
	}}}}
	cfg.PassthroughHeaders = true
	router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
	for _, model := range enterpriseFixtureModels {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
					fields := map[string]any{"temperature": 0.5, "top_p": 0.8, "max_tokens": 128, "metadata": map[string]string{"fixture": privateValue}}
					wantIgnored := "temperature"
					if protocol == "gemini" {
						fields = map[string]any{"generationConfig": map[string]any{"temperature": 0.5, "topP": 0.8, "maxOutputTokens": 128, "thinkingConfig": map[string]any{"includeThoughts": true}}, "safetySettings": []map[string]string{{"category": privateValue}}}
						wantIgnored = "generationConfig.temperature"
					}
					path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
					before := transport.calls.Load()
					response := enterprisePublicCall(router, path, body)
					if response.Code != http.StatusOK || !strings.Contains(enterprisePublicText(response.Body.Bytes(), protocol, stream), "offline 前🙂引用") || transport.calls.Load() != before+1 {
						t.Fatalf("optional parameters broke text request: status=%d calls=%d body=%s", response.Code, transport.calls.Load()-before, response.Body.String())
					}
					terminal := `"finish_reason":"stop"`
					if protocol == "gemini" {
						terminal = `"finishReason":"STOP"`
					}
					if !strings.Contains(response.Body.String(), terminal) {
						t.Fatalf("successful completion has no terminal status: %s", response.Body.String())
					}
					upstream := transport.requestBody()
					if err := enterprise.ValidateFinal(upstream); err != nil {
						t.Fatalf("invalid final payload: %v", err)
					}
					if got := gjson.GetBytes(upstream, "generationSpec.modelId").String(); got != model {
						t.Fatalf("wrong upstream model: got %q want %q", got, model)
					}
					ignored := response.Header().Get("X-CLIProxyAPI-Ignored-Parameters")
					if !strings.Contains(ignored, wantIgnored) {
						t.Fatalf("ignored options are not reported: %q", ignored)
					}
					for _, wire := range []string{string(upstream), response.Body.String(), fmt.Sprint(response.Header())} {
						if strings.Contains(wire, privateValue) {
							t.Fatalf("ignored option value leaked: %s", wire)
						}
					}
				})
			}
		}
	}
}

func TestEnterprisePublicRejectsUnsupportedContent(t *testing.T) {
	const privateValue = "synthetic-private-media-value"
	for _, scenario := range []struct {
		name, protocol, field, reason string
		fields                        map[string]any
	}{
		{"chat image", "openai", "messages[0].content[0]", "text parts only", map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "image_url", "image_url": map[string]string{"url": privateValue}}}}}}},
		{"chat malformed tool history", "openai", "messages[0].tool_calls[0].type", "must be function", map[string]any{"messages": []map[string]any{{"role": "assistant", "content": "hi", "tool_calls": []map[string]string{{"id": privateValue}}}, {"role": "user", "content": "hi"}}}},
		{"gemini inline data", "gemini", "contents[0].parts[0].inlineData", "is not supported", map[string]any{"contents": []map[string]any{{"parts": []map[string]any{{"inlineData": map[string]string{"mimeType": "image/png", "data": privateValue}}}}}}},
		{"gemini malformed function declaration", "gemini", "tools[0].functionDeclarations", "must be an array", map[string]any{"tools": []map[string]any{{"functionDeclarations": privateValue}}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			router, transport, _ := enterpriseFixtureRouter(t)
			for _, model := range enterpriseFixtureModels {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
						path, body := enterprisePublicRequest(t, model, scenario.protocol, stream, scenario.fields)
						response := enterprisePublicCall(router, path, body)
						wire := response.Body.Bytes()
						if response.Code != http.StatusBadRequest || gjson.GetBytes(wire, "error.code").String() != "invalid_field" || gjson.GetBytes(wire, "error.param").String() != scenario.field || !strings.Contains(gjson.GetBytes(wire, "error.message").String(), scenario.reason) {
							t.Fatalf("unsupported content diagnostic lost: status=%d body=%s", response.Code, wire)
						}
						if strings.Contains(string(wire), privateValue) || transport.calls.Load() != 0 {
							t.Fatalf("unsupported content leaked a value or reached upstream: calls=%d body=%s", transport.calls.Load(), wire)
						}
						enterpriseAssertPublicErrorChannelNeutral(t, wire)
					})
				}
			}
		})
	}
}

func TestEnterprisePublicRoutesBothProtocolsAndOperations(t *testing.T) {
	router, transport, _ := enterpriseFixtureRouter(t)
	for _, test := range []struct {
		path, body string
		status     int
		visible    string
	}{
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`, 200, "offline"},
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"stream":true}`, 200, "data: [DONE]"},
		{"/v1beta/models/gemini-4-argon:generateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`, 200, "executableCode"},
		{"/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hi"}]}]}`, 200, "groundingMetadata"},
		{"/v1/completions", `{"model":"gemini-4-argon","prompt":"hi"}`, 400, "operation_not_supported"},
		{"/v1/chat/completions", `{"model":"gemini-4-argon","input":"hi"}`, 400, "operation_not_supported"},
		{"/v1beta/models/gemini-4-argon:countTokens", `{"contents":[{"parts":[{"text":"hi"}]}]}`, 400, "operation_not_supported"},
	} {
		before := transport.calls.Load()
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.visible) {
			t.Fatalf("%s: status=%d body=%s", test.path, response.Code, response.Body.String())
		}
		if test.status == 400 && before != transport.calls.Load() {
			t.Fatal("unsupported endpoint called upstream")
		}
		if test.status == 400 {
			enterpriseAssertPublicErrorChannelNeutral(t, response.Body.Bytes())
		}
	}
	for _, path := range []string{"/v1/models", "/v1beta/models"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), "gemini-4-argon") || strings.Contains(response.Body.String(), "countTokens") {
			t.Fatalf("invalid public catalog: %s", response.Body.String())
		}
	}
}

func TestEnterprisePublicTokenUsage(t *testing.T) {
	router, _, _ := enterpriseFixtureRouter(t)
	for _, scenario := range []struct {
		path, body              string
		stream, include, gemini bool
	}{
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`, false, true, false},
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"stream":true}`, true, false, false},
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`, true, true, false},
		{"/v1beta/models/gemini-4-argon:generateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`, false, true, true},
		{"/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hi"}]}]}`, true, true, true},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", scenario.path, strings.NewReader(scenario.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("%s: %s", scenario.path, response.Body.String())
		}
		packets := []string{response.Body.String()}
		if scenario.stream {
			packets = nil
			for _, line := range strings.Split(response.Body.String(), "\n") {
				if strings.HasPrefix(line, "data:") {
					packets = append(packets, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				}
			}
		}
		count := 0
		for _, packet := range packets {
			root := gjson.Parse(packet)
			node := root.Get("usage")
			input, output, total := "prompt_tokens", "completion_tokens", "total_tokens"
			if scenario.gemini {
				node = root.Get("usageMetadata")
				input, output, total = "promptTokenCount", "candidatesTokenCount", "totalTokenCount"
			}
			if !node.Exists() {
				continue
			}
			count++
			if node.Get(input).Int() <= 0 || node.Get(output).Int() <= 0 || node.Get(total).Int() != node.Get(input).Int()+node.Get(output).Int() {
				t.Fatalf("bad wire usage: %s", packet)
			}
			for _, field := range []string{"thoughtsTokenCount", "cachedContentTokenCount", "prompt_tokens_details", "completion_tokens_details"} {
				if node.Get(field).Exists() {
					t.Fatalf("unknown field %s was filled", field)
				}
			}
			if scenario.stream && !scenario.gemini && root.Get("choices.#").Int() != 0 {
				t.Fatal("usage tail contains choices")
			}
		}
		want := 0
		if scenario.include {
			want = 1
		}
		if count != want {
			t.Fatalf("%s: usage packets=%d want=%d", scenario.path, count, want)
		}
	}
}

// TestEnterpriseSDKHarness is opt-in and serves synthetic fixtures on loopback only.
// The Python acceptance script uses real SDKs without Google credentials or model calls.
func TestEnterpriseSDKHarness(t *testing.T) {
	ready := os.Getenv("CLIPROXY_ENTERPRISE_SDK_READY_FILE")
	if ready == "" {
		t.Skip("optional local SDK harness")
	}
	router, _, _ := enterpriseFixtureRouter(t)
	stop := make(chan struct{}, 1)
	router.POST("/__stop", func(c *gin.Context) {
		select {
		case stop <- struct{}{}:
		default:
		}
		c.Status(204)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	if err := os.WriteFile(ready, []byte(server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stop:
	case <-t.Context().Done():
	}
}

// A same-name route remains a mixed host pool for Chat. Legacy operations remove
// Enterprise before selecting credentials and continue on the companion provider.
type enterpriseCompanionExecutor struct {
	*runtimeexec.GeminiEnterpriseExecutor
	calls atomic.Int32
}

func (*enterpriseCompanionExecutor) Identifier() string { return "enterprise-companion" }
func (e *enterpriseCompanionExecutor) Execute(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	e.calls.Add(1)
	return coreexec.Response{Payload: []byte(`{"id":"companion","object":"chat.completion","model":"gemini-4-argon","choices":[{"index":0,"message":{"role":"assistant","content":"companion"},"finish_reason":"stop"}]}`)}, nil
}

func TestEnterpriseSameNameMixedPoolAndOtherOperationFallback(t *testing.T) {
	router, transport, manager := enterpriseFixtureRouter(t)
	companion := &enterpriseCompanionExecutor{GeminiEnterpriseExecutor: runtimeexec.NewGeminiEnterpriseExecutor(&config.Config{})}
	manager.RegisterExecutor(companion)
	id := "enterprise-companion-auth"
	_, err := manager.Register(t.Context(), &coreauth.Auth{ID: id, Provider: companion.Identifier(), Status: coreauth.StatusActive, Metadata: map[string]any{"email": "companion@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, companion.Identifier(), []*registry.ModelInfo{{ID: "gemini-4-argon", Type: "openai"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	for range 8 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`))
		router.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("mixed Chat failed: %s", response.Body.String())
		}
	}
	if transport.calls.Load() == 0 || companion.calls.Load() == 0 {
		t.Fatal("same-name providers did not share normal host scheduling")
	}
	before := transport.calls.Load()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/v1/completions", strings.NewReader(`{"model":"gemini-4-argon","prompt":"hi"}`)))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "companion") || transport.calls.Load() != before {
		t.Fatalf("legacy operation did not stay on companion: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEnterpriseStatisticsUnknownRoundTrip(t *testing.T) {
	source := stats.NewRequestStatistics()
	source.Record(context.Background(), coreusage.Record{RequestedAt: time.Now(), APIKey: "fixture", Model: "enterprise", Detail: coreusage.Detail{InputTokens: 12, OutputTokens: 3, TotalTokens: 15, TokenPresence: &coreusage.TokenPresence{InputTokens: true, OutputTokens: true, TotalTokens: true}}})
	data, err := json.Marshal(source.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot stats.StatisticsSnapshot
	if err = json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	destination := stats.NewRequestStatistics()
	if result := destination.MergeSnapshot(snapshot); result.Added != 1 {
		t.Fatal(result)
	}
	if result := destination.MergeSnapshot(snapshot); result.Added != 0 || result.Skipped != 1 {
		t.Fatal(result)
	}
	data, err = json.Marshal(destination.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	fields := gjson.GetBytes(data, "apis.fixture.models.enterprise.details.0.tokens")
	if fields.Get("reasoning_tokens").Type != gjson.Null || fields.Get("cached_tokens").Type != gjson.Null || fields.Get("input_tokens").Int() != 12 || destination.Snapshot().TotalTokens != 15 {
		t.Fatalf("bad round trip: %s", data)
	}
}
