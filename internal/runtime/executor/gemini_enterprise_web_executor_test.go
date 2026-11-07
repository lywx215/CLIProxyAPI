package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

type phaseOneTransport func(*http.Request) (*http.Response, error)

const phaseOnePlusConfig = `{"engineResourceDetails":{"projectNumber":"123","location":"ca","collectionId":"default_collection","engineId":"synthetic"},"anonymousWidgetConfig":{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"gemini-3.8-flash"}]}}}}`
const phaseOneGoogleSearchSettings = `{"webGroundingType":"WEB_GROUNDING_TYPE_GOOGLE_SEARCH","defaultWebGroundingToggleOff":true}`

func (f phaseOneTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type phaseOneFixture struct {
	t               *testing.T
	mu              sync.Mutex
	calls           map[string]int
	session, token  string
	final           []byte
	lookupConfig    string
	uploadFailure   bool
	generateFailure bool
	response        string
	block           chan struct{}
	entered         chan struct{}
}

func (f *phaseOneFixture) roundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	action := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
	f.calls[action]++
	reply := func(status int, s string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(s)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	}
	if req.Method == http.MethodGet {
		if req.URL.Host != "vertexaisearch.cloud.google.com" || req.Header.Get("Cookie") == "" || req.Header.Get("Authorization") != "" {
			f.t.Fatal("wrong authentication boundary")
		}
		return reply(200, `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`)
	}
	if req.URL.Host != "ca-discoveryengine.clients6.google.com" || req.Header.Get("Cookie") == "" || !strings.HasPrefix(req.Header.Get("Authorization"), "SAPISIDHASH ") {
		f.t.Fatal("wrong generation boundary")
	}
	if f.token == "" {
		f.token = req.Header.Get("Cookie")
	}
	if f.token != req.Header.Get("Cookie") {
		f.t.Fatal("request changed credentials")
	}
	if action == "lookupWidgetConfig" {
		if f.lookupConfig != "" {
			return reply(200, f.lookupConfig)
		}
		return reply(200, phaseOnePlusConfig)
	}
	if strings.HasSuffix(action, ":uploadFile") {
		if !strings.Contains(req.URL.Path, "/projects/123/locations/ca/collections/default_collection/engines/synthetic/sessions/") || !strings.HasSuffix(req.URL.Path, "/"+strings.TrimPrefix(f.session, "collections/default_collection/engines/synthetic/sessions/")+":uploadFile") {
			f.t.Fatal("upload left the new session")
		}
		if req.Header.Get("X-Goog-Upload-Command") == "start" {
			response, _ := reply(200, `{}`)
			response.Header.Set("X-Goog-Upload-URL", req.URL.String()+"?upload_id=synthetic")
			return response, nil
		}
		f.calls["uploadFile"]++
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		if f.uploadFailure {
			return reply(500, `{"error":"synthetic upload error"}`)
		}
		return reply(200, fmt.Sprintf(`{"fileId":"file-%d"}`, f.calls["uploadFile"]))
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		f.t.Fatal("invalid upstream framing")
	}
	switch action {
	case "widgetCreateSession":
		f.session = "collections/default_collection/engines/synthetic/sessions/generated"
		return reply(200, fmt.Sprintf(`{"session":{"name":%q}}`, f.session))
	case "widgetStreamAssist":
		if gjson.GetBytes(body, "streamAssistRequest.session").String() != f.session {
			f.t.Fatal("generation left the upload session")
		}
		f.final = bytes.Clone(body)
		if f.entered != nil {
			close(f.entered)
		}
		if f.block != nil {
			f.mu.Unlock()
			select {
			case <-f.block:
			case <-req.Context().Done():
				f.mu.Lock()
				return nil, req.Context().Err()
			}
			f.mu.Lock()
		}
		if f.generateFailure {
			return nil, errors.New("synthetic submission outcome unknown")
		}
		response := f.response
		if response == "" {
			response = `[{"streamAssistResponse":{"answer":{"state":"IN_PROGRESS","replies":[{"groundedContent":{"content":{"text":"hello"}}}]}}},{"streamAssistResponse":{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"content":{"text":"hello world"}}}]}}}]`
		}
		return reply(200, response)
	}
	return reply(500, `{}`)
}
func phaseOneCredential(t *testing.T) web.Credential {
	t.Helper()
	c := web.Credential{ModelPolicy: web.PolicyExcludeArgon, Revision: 1, AvailableModels: []web.AvailableModel{{DisplayName: "gemini-3.8-flash", ID: "gemini-3.8-flash"}}}
	if err := c.SetCookies("SID=synthetic-session; SAPISID=synthetic-signing"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/config"); err != nil {
		t.Fatal(err)
	}
	c.SetCatalog(c.AvailableModels, time.Now())
	return c
}
func phaseOneExecutor(t *testing.T, cfg *config.Config) (*GeminiEnterpriseWebExecutor, *auth.Auth, context.Context, *phaseOneFixture) {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.GeminiEnterpriseWeb.Enabled = true
	r := web.NewRuntime()
	r.Configure(cfg.GeminiEnterpriseWeb)
	e := NewGeminiEnterpriseWebExecutor(cfg, r)
	c := phaseOneCredential(t)
	a := &auth.Auth{ID: "web-phase-one-cookie", Provider: web.Provider, Status: auth.StatusActive, Metadata: map[string]any{"type": web.Provider, "enterprise_web": c.MetadataValue()}}
	f := &phaseOneFixture{t: t, calls: map[string]int{}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", phaseOneTransport(f.roundTrip))
	return e, a, ctx, f
}
func phaseOneRequest(mimes ...string) exec.Request {
	parts := []any{map[string]string{"text": "read these attachments"}}
	for _, mime := range mimes {
		parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": "aW5wdXQ="}})
	}
	body, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}})
	return exec.Request{Model: "gemini-3.8-flash", Payload: body}
}
func phaseOneOptions() exec.Options {
	return exec.Options{SourceFormat: translator.FromString("gemini"), ResponseFormat: translator.FromString("gemini"), Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}}
}

func phaseOneWithTools(req exec.Request, fields string) exec.Request {
	req.Payload = []byte(string(req.Payload[:len(req.Payload)-1]) + "," + fields + "}")
	return req
}

func phaseOneWithSearchSettings(settings string) string {
	return strings.Replace(phaseOnePlusConfig, `"uiSettings":`, `"assistantSettings":`+settings+`,"uiSettings":`, 1)
}

func TestEnterpriseWebSingleSessionMultipleAttachments(t *testing.T) {
	e, a, ctx, f := phaseOneExecutor(t, nil)
	response, err := e.Execute(ctx, a, phaseOneRequest("text/plain", "application/pdf", "image/png"), phaseOneOptions())
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, part := range gjson.GetBytes(response.Payload, "candidates.0.content.parts").Array() {
		text += part.Get("text").String()
	}
	if text != "hello world" || !strings.Contains(string(response.Payload), `"finishReason":"STOP"`) || !gjson.GetBytes(response.Payload, "usageMetadata.candidatesTokenCount").Exists() || gjson.GetBytes(response.Payload, "usageMetadata.promptTokenCount").Exists() || gjson.GetBytes(response.Payload, "usageMetadata.totalTokenCount").Exists() {
		t.Fatal(string(response.Payload))
	}
	if f.calls["widgetCreateSession"] != 1 || f.calls["uploadFile"] != 3 || f.calls["widgetStreamAssist"] != 1 {
		t.Fatal(f.calls)
	}
	if f.calls["lookupWidgetConfig"] != 1 || gjson.GetBytes(f.final, "streamAssistRequest.toolsSpec.webGroundingSpec").Exists() {
		t.Fatal("ordinary media requested search", f.calls, string(f.final))
	}
	if gjson.GetBytes(f.final, "streamAssistRequest.assistGenerationConfig.modelId").String() != "gemini-3.8-flash" || len(gjson.GetBytes(f.final, "streamAssistRequest.fileIds").Array()) != 3 {
		t.Fatal(string(f.final))
	}
	if e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
		t.Fatal(e.runtime.Status())
	}
}

func TestEnterpriseWebGoogleSearchChecksCurrentIndex(t *testing.T) {
	e, a, ctx, f := phaseOneExecutor(t, nil)
	f.lookupConfig = phaseOneWithSearchSettings(phaseOneGoogleSearchSettings)
	req := phaseOneWithTools(phaseOneRequest("text/plain"), `"tools":[{"google_search":{}}],"tool_config":{}`)
	if _, err := e.Execute(ctx, a, req, phaseOneOptions()); err != nil {
		t.Fatal(err)
	}
	if f.calls["lookupWidgetConfig"] != 2 || f.calls["widgetCreateSession"] != 1 || f.calls["uploadFile"] != 1 || f.calls["widgetStreamAssist"] != 1 {
		t.Fatal(f.calls)
	}
	for _, tc := range []struct {
		name, lookup string
		status       int
	}{
		{"enterprise", phaseOneWithSearchSettings(`{"webGroundingType":"WEB_GROUNDING_TYPE_ENTERPRISE_WEB_SEARCH"}`), 400},
		{"disabled", phaseOneWithSearchSettings(`{"webGroundingType":"WEB_GROUNDING_TYPE_DISABLED"}`), 400},
		{"missing", phaseOnePlusConfig, 400},
		{"legacy-toggle", phaseOneWithSearchSettings(`{"googleSearchGroundingEnabled":true}`), 400},
		{"duplicate", phaseOneWithSearchSettings(`{"webGroundingType":"WEB_GROUNDING_TYPE_DISABLED","webGroundingType":"WEB_GROUNDING_TYPE_GOOGLE_SEARCH"}`), 502},
		{"wrong-type", phaseOneWithSearchSettings(`{"webGroundingType":1}`), 502},
		{"rpc-error-with-config", `{"error":{"code":403,"message":"hidden upstream details"},` + phaseOneWithSearchSettings(phaseOneGoogleSearchSettings)[1:], 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.lookupConfig = tc.lookup
			before := f.calls["lookupWidgetConfig"]
			_, err := e.Execute(ctx, a, req, phaseOneOptions())
			var failure *web.Failure
			if !errors.As(err, &failure) || failure.StatusCode() != tc.status || !strings.Contains(failure.Error(), "Google Search") {
				t.Fatalf("expected safe Google Search error %d, got %v", tc.status, err)
			}
			if f.calls["lookupWidgetConfig"] != before+1 || f.calls["widgetCreateSession"] != 1 || f.calls["uploadFile"] != 1 || f.calls["widgetStreamAssist"] != 1 {
				t.Fatal("stale capability caused another session, upload or generation", f.calls)
			}
		})
	}
}

func TestEnterpriseWebGoogleSearchRejectsUnsupportedOptions(t *testing.T) {
	e, a, ctx, f := phaseOneExecutor(t, nil)
	for _, fields := range []string{
		`"tools":[{"googleSearch":{"timeRangeFilter":{"startTime":"2026-11-01T00:00:00Z","endTime":"2026-11-02T00:00:00Z"}}}]`,
		`"tools":[{"googleSearch":{"searchTypes":{"imageSearch":{}}}}]`,
		`"tools":[{"googleSearch":{}},{"urlContext":{}}]`,
		`"tools":[{"googleSearch":{},"functionDeclarations":[]}]`,
		`"tools":[{"googleSearch":{},"google_search":{}}]`,
		`"tools":[{"googleSearch":{}},{"google_search":{}}]`,
		`"tools":[{"googleSearch":{}}],"toolConfig":{"retrievalConfig":{"languageCode":"zh-CN"}}`,
	} {
		req := phaseOneWithTools(phaseOneRequest("image/png"), fields)
		_, err := e.Execute(ctx, a, req, phaseOneOptions())
		var failure *web.Failure
		if !errors.As(err, &failure) || failure.StatusCode() != 400 {
			t.Fatalf("%s: expected 400, got %v", fields, err)
		}
		if len(f.calls) != 0 {
			t.Fatal("unsupported search reached upstream", f.calls)
		}
	}
}

func TestEnterpriseWebGoogleSearchRespectsFinalPayloadFilter(t *testing.T) {
	const path = "streamAssistRequest.toolsSpec.webGroundingSpec"
	cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{
		{Models: []config.PayloadModelRule{{Name: "*", Protocol: web.Provider}}, Params: []string{path}},
	}}}
	e, a, ctx, f := phaseOneExecutor(t, cfg)
	f.lookupConfig = phaseOneWithSearchSettings(phaseOneGoogleSearchSettings)
	req := phaseOneWithTools(phaseOneRequest("image/png"), `"tools":[{"googleSearch":{}}]`)
	_, err := e.Execute(ctx, a, req, phaseOneOptions())
	var failure *web.Failure
	if !errors.As(err, &failure) || failure.StatusCode() != 400 || !strings.Contains(failure.Error(), "Payload rules") || f.calls["widgetStreamAssist"] != 0 {
		t.Fatal("payload filter was ignored or search silently dropped", err, f.calls)
	}
}

func TestEnterpriseWebFailureDoesNotForgeSuccessOrReplay(t *testing.T) {
	testEnterpriseWebFailureDoesNotForgeSuccessOrReplay(t, false)
}
func TestEnterpriseWebSerializedFailureDoesNotForgeSuccessOrReplay(t *testing.T) {
	testEnterpriseWebFailureDoesNotForgeSuccessOrReplay(t, true)
}

func testEnterpriseWebFailureDoesNotForgeSuccessOrReplay(t *testing.T, serialized bool) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"upload", "submit", "truncated", "failed"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				e, a, ctx, f := phaseOneExecutor(t, historyTestConfig(serialized))
				switch mode {
				case "upload":
					f.uploadFailure = true
				case "submit":
					f.generateFailure = true
				case "truncated":
					f.response = `[{"streamAssistResponse":{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"content":{"text":"partial"}}}]}}}`
				case "failed":
					f.response = `[{"streamAssistResponse":{"answer":{"state":"FAILED"}}}]`
				}
				var err error
				if stream {
					var result *exec.StreamResult
					result, err = e.ExecuteStream(ctx, a, historyTestRequest(phaseOneRequest("image/png"), serialized), phaseOneOptions())
					if err == nil {
						for chunk := range result.Chunks {
							if strings.Contains(string(chunk.Payload), `"finishReason":"STOP"`) {
								t.Fatal("forged terminal success")
							}
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = e.Execute(ctx, a, historyTestRequest(phaseOneRequest("image/png"), serialized), phaseOneOptions())
				}
				if err == nil {
					t.Fatal("failed request succeeded")
				}
				if mode == "upload" {
					if f.calls["widgetStreamAssist"] != 0 {
						t.Fatal("generated after failed upload")
					}
				} else if !exec.GenerationHasStarted(err) {
					t.Fatal("uncertain generation can be replayed")
				}
				if e.runtime.Status()["active_requests"] != 0 {
					t.Fatal(e.runtime.Status())
				}
			})
		}
	}
}

func TestEnterpriseWebFinalPayloadRulesAndBindings(t *testing.T) {
	testEnterpriseWebFinalPayloadRulesAndBindings(t, false)
}
func TestEnterpriseWebSerializedFinalPayloadRulesAndBindings(t *testing.T) {
	testEnterpriseWebFinalPayloadRulesAndBindings(t, true)
}

func testEnterpriseWebFinalPayloadRulesAndBindings(t *testing.T, serialized bool) {
	rules := []config.PayloadModelRule{{Name: "*", Protocol: web.Provider}}
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: rules, Params: map[string]any{"streamAssistRequest.query.parts.0.text": "configured-final-query", "streamAssistRequest.answerGenerationMode": "CONFIGURED_MODE"}}}}}
	e, a, ctx, f := phaseOneExecutor(t, historyTestWithConfig(cfg, serialized))
	if _, err := e.Execute(ctx, a, historyTestRequest(phaseOneRequest("text/plain"), serialized), phaseOneOptions()); err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(f.final, "streamAssistRequest.query.parts.0.text").String() != "configured-final-query" || gjson.GetBytes(f.final, "streamAssistRequest.answerGenerationMode").String() != "CONFIGURED_MODE" {
		t.Fatal(string(f.final))
	}
	cfg.Payload.Override[0].Params = map[string]any{"streamAssistRequest.fileIds": []string{"foreign-session-file"}}
	f.calls["widgetStreamAssist"] = 0
	if _, err := e.Execute(ctx, a, historyTestRequest(phaseOneRequest("text/plain"), serialized), phaseOneOptions()); err == nil || f.calls["widgetStreamAssist"] != 0 {
		t.Fatal("payload escaped pinned uploads")
	}
}

func TestEnterpriseWebCancellationReleasesResources(t *testing.T) {
	testEnterpriseWebCancellationReleasesResources(t, false)
}
func TestEnterpriseWebSerializedCancellationReleasesResources(t *testing.T) {
	testEnterpriseWebCancellationReleasesResources(t, true)
}

func testEnterpriseWebCancellationReleasesResources(t *testing.T, serialized bool) {
	e, a, ctx, f := phaseOneExecutor(t, historyTestConfig(serialized))
	f.block = make(chan struct{})
	f.entered = make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(ctx, a, historyTestRequest(phaseOneRequest("image/png"), serialized), phaseOneOptions())
		done <- err
	}()
	<-f.entered
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request succeeded")
	}
	if e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
		t.Fatal(e.runtime.Status())
	}
}

type phaseOnePlainExecutor struct {
	*GeminiEnterpriseWebExecutor
	calls atomic.Int32
}

func (*phaseOnePlainExecutor) Identifier() string { return "gemini-enterprise" }
func (e *phaseOnePlainExecutor) Execute(context.Context, *auth.Auth, exec.Request, exec.Options) (exec.Response, error) {
	e.calls.Add(1)
	return exec.Response{Payload: []byte(`{"text":"plain"}`)}, nil
}

func phaseOneManager(t *testing.T) (*auth.Manager, *phaseOnePlainExecutor, *GeminiEnterpriseWebExecutor, context.Context, *phaseOneFixture) {
	t.Helper()
	e, a, ctx, f := phaseOneExecutor(t, nil)
	m := auth.NewManager(nil, nil, nil)
	e.runtime = m.WebRuntime()
	m.SetConfig(e.cfg)
	m.RegisterExecutor(e)
	plain := &phaseOnePlainExecutor{GeminiEnterpriseWebExecutor: e}
	m.RegisterExecutor(plain)
	for _, account := range []*auth.Auth{a, {ID: "web-phase-one-plain", Provider: "gemini-enterprise", Status: auth.StatusActive, Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "fixture-client", ProjectID: "123", EngineID: "fixture-engine"}, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic", Expiry: time.Now().Add(time.Hour)})}} {
		if account.Provider == "gemini-enterprise" {
			account.Metadata["model_policy"] = "catalog"
		}
		if _, err := m.Register(context.Background(), account); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(account.ID, account.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(account.ID) })
	}
	for _, projected := range m.List() {
		if !auth.IsEnterpriseAccountAuth(projected) {
			registry.GetGlobalRegistry().RegisterClient(projected.ID, projected.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(projected.ID) })
		}
	}
	for _, account := range m.List() {
		if account.Provider == "gemini-enterprise" {
			trial := true
			lease := m.BeginEnterpriseSubscriptionRefresh(account)
			if lease == nil || !lease.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()}) {
				t.Fatal("mock subscription not published")
			}
			m.PublishEnterprisePolicyCatalog(account, []ge.Model{{ID: "gemini-3.8-flash"}})
		}
	}
	return m, plain, e, ctx, f
}

func TestEnterpriseWebManagerRoutesAndNoCrossChannelFallback(t *testing.T) {
	m, plain, e, ctx, f := phaseOneManager(t)
	providers := []string{"gemini-enterprise", web.Provider}
	text := exec.Request{Model: "gemini-3.8-flash", Payload: []byte(`{"contents":[{"parts":[{"text":"ordinary"}]}]}`)}
	if _, err := m.Execute(ctx, providers, phaseOneWithTools(text, `"tools":[{"googleSearch":{}}]`), phaseOneOptions()); err != nil || plain.calls.Load() != 1 || len(f.calls) != 0 {
		t.Fatal("text search left Enterprise", err)
	}
	if _, err := m.Execute(ctx, providers, phaseOneRequest("image/png"), phaseOneOptions()); err != nil || plain.calls.Load() != 1 || f.calls["widgetStreamAssist"] != 1 {
		t.Fatal("mixed message not routed exclusively to Web", err)
	}
	cfg := *e.cfg
	cfg.GeminiEnterpriseWeb.Enabled = false
	m.SetConfig(&cfg)
	if _, err := m.Execute(ctx, providers, phaseOneRequest("image/png"), phaseOneOptions()); err == nil || plain.calls.Load() != 1 {
		t.Fatal("disabled media fell back to text")
	}
	if _, err := m.Execute(ctx, providers, text, phaseOneOptions()); err != nil || plain.calls.Load() != 2 {
		t.Fatal("disabled Web broke plain text", err)
	}
	cfg.GeminiEnterpriseWeb.Enabled = true
	m.SetConfig(&cfg)
	payload := phaseOneRequest("image/png")
	payload.Payload = []byte(`{"contents":[{"role":"user","parts":[{"text":"old"}]},{"role":"model","parts":[{"text":"reply"}]},` + string(payload.Payload[len(`{"contents":[`):]))
	if _, err := m.Execute(ctx, providers, payload, phaseOneOptions()); err == nil || plain.calls.Load() != 2 {
		t.Fatal("media history not rejected")
	}
	req := phaseOneRequest("image/png")
	req.Model = "unmapped"
	if _, err := m.Execute(ctx, providers, req, phaseOneOptions()); err == nil {
		t.Fatal("guessed media mapping")
	}
	if _, err := m.ExecuteCount(ctx, providers, phaseOneRequest("image/png"), phaseOneOptions()); err == nil {
		t.Fatal("base64 used for token count")
	}
}

func TestEnterpriseWebManagerDoesNotReplayUncertainSubmission(t *testing.T) {
	m, _, _, ctx, f := phaseOneManager(t)
	c := phaseOneCredential(t)
	c.Cookies = "SID=synthetic-replay-second; SAPISID=synthetic-signing"
	a := &auth.Auth{ID: "web-replay-second", Provider: web.Provider, Status: auth.StatusActive, Metadata: map[string]any{"enterprise_web": c.MetadataValue()}}
	if _, err := m.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	m.SetRetryConfig(2, 0, 0)
	f.generateFailure = true
	if _, err := m.Execute(ctx, []string{"gemini-enterprise", web.Provider}, phaseOneRequest("image/png"), phaseOneOptions()); err == nil {
		t.Fatal("uncertain submission reported success")
	}
	if f.calls["widgetStreamAssist"] != 1 || f.calls["widgetCreateSession"] != 1 || f.calls["uploadFile"] != 1 {
		t.Fatal("uncertain outcome was replayed", f.calls)
	}
}

func TestEnterpriseWebManagerSwitchesOnlyBeforeSession(t *testing.T) {
	testEnterpriseWebManagerSwitchesOnlyBeforeSession(t, false)
}
func TestEnterpriseWebSerializedManagerSwitchesOnlyBeforeSession(t *testing.T) {
	testEnterpriseWebManagerSwitchesOnlyBeforeSession(t, true)
}

func testEnterpriseWebManagerSwitchesOnlyBeforeSession(t *testing.T, serialized bool) {
	for _, stream := range []bool{false, true} {
		for _, scenario := range []string{"rejected-cookie", "search-index", "pinned", "wrong-class", "retry-limit", "both-search-mismatch", "uncertain-submit"} {
			t.Run(fmt.Sprintf("%s/stream=%t", scenario, stream), func(t *testing.T) {
				m, plain, e, ctx, f := phaseOneManager(t)
				if serialized {
					cfg := *e.cfg
					cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
					m.SetConfig(&cfg)
				}
				credential := phaseOneCredential(t)
				if err := credential.SetCookies("SID=synthetic-backup; SAPISID=synthetic-signing"); err != nil {
					t.Fatal(err)
				}
				if scenario == "wrong-class" {
					credential.ModelPolicy = web.PolicyArgonOnly
				}
				credential.SetCatalog(phaseOneCredential(t).AvailableModels, time.Now())
				backup := &auth.Auth{ID: "web-phase-one-backup", Provider: web.Provider, Status: auth.StatusActive, Metadata: map[string]any{
					"enterprise_web": credential.MetadataValue(), "priority": float64(-10),
				}}
				auth.ApplyAuthPriorityMetadata(backup, backup.Metadata)
				if _, err := m.Register(ctx, backup); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(backup.ID, backup.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
				defer registry.GetGlobalRegistry().UnregisterClient(backup.ID)
				if scenario == "retry-limit" {
					m.SetRetryConfig(2, 0, 1)
				}
				req, opts := historyTestRequest(phaseOneRequest("image/png"), serialized), phaseOneOptions()
				search := scenario == "search-index" || scenario == "both-search-mismatch"
				if search {
					req = phaseOneWithTools(req, `"tools":[{"googleSearch":{}}]`)
					if scenario == "search-index" {
						f.lookupConfig = phaseOneWithSearchSettings(phaseOneGoogleSearchSettings)
					}
				}
				if scenario == "pinned" {
					opts.Metadata[exec.PinnedAuthMetadataKey] = "web-phase-one-cookie"
				}
				var primary, backupCalls int
				var generatedWith string
				reply := func(status int, body string) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(r *http.Request) (*http.Response, error) {
					cookie := r.Header.Get("Cookie")
					if strings.Contains(cookie, "SID=synthetic-session") && scenario != "uncertain-submit" {
						primary++
						switch {
						case !search:
							return reply(401, "{}")
						case r.Method == http.MethodGet:
							return reply(200, `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`)
						case strings.HasSuffix(r.URL.Path, "lookupWidgetConfig"):
							return reply(200, phaseOnePlusConfig)
						default:
							t.Error("primary created a session for an incompatible search")
							return reply(500, "{}")
						}
					}
					if strings.Contains(cookie, "SID=synthetic-backup") {
						backupCalls++
					}
					if strings.HasSuffix(r.URL.Path, "widgetStreamAssist") {
						generatedWith = cookie
					}
					return f.roundTrip(r)
				}))
				f.generateFailure = scenario == "uncertain-submit"
				execute := func() error {
					providers := []string{"gemini-enterprise", web.Provider}
					if stream {
						opts.Metadata[exec.OperationMetadataKey] = "stream_generate_content"
						result, err := m.ExecuteStream(ctx, providers, req, opts)
						if err != nil {
							return err
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
						return err
					}
					_, err := m.Execute(ctx, providers, req, opts)
					return err
				}
				err := execute()
				switch scenario {
				case "rejected-cookie", "search-index":
					if err != nil || primary == 0 || backupCalls == 0 || !strings.Contains(generatedWith, "SID=synthetic-backup") ||
						f.calls["widgetCreateSession"] != 1 || f.calls["uploadFile"] != 1 || f.calls["widgetStreamAssist"] != 1 {
						t.Fatal(err, primary, backupCalls, f.calls)
					}
					if scenario == "rejected-cookie" {
						if err = execute(); err != nil || primary != 1 || f.calls["widgetStreamAssist"] != 2 {
							t.Fatal("confirmed rejected credential was selected again", err, primary, f.calls)
						}
					}
				case "uncertain-submit":
					if err == nil || backupCalls != 0 || f.calls["widgetStreamAssist"] != 1 {
						t.Fatal("uncertain generation replayed", err, backupCalls, f.calls)
					}
				default:
					want := 503
					if scenario == "pinned" {
						want = 401
					} else if scenario == "both-search-mismatch" {
						want = 400
					}
					var failure interface{ StatusCode() int }
					if !errors.As(err, &failure) || failure.StatusCode() != want || f.calls["widgetCreateSession"] != 0 ||
						(scenario != "both-search-mismatch" && backupCalls != 0) {
						t.Fatal("unexpected rejection", err, backupCalls, f.calls)
					}
				}
				if plain.calls.Load() != 0 || e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
					t.Fatal("request left the Web channel or leaked resources", e.runtime.Status())
				}
			})
		}
	}
}

func TestEnterpriseWebNativeHTTPAndBoundedLogs(t *testing.T) {
	testEnterpriseWebNativeHTTPAndBoundedLogs(t, false)
}
func TestEnterpriseWebSerializedNativeHTTPAndBoundedLogs(t *testing.T) {
	testEnterpriseWebNativeHTTPAndBoundedLogs(t, true)
}

func testEnterpriseWebNativeHTTPAndBoundedLogs(t *testing.T, serialized bool) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			e, a, ctx, f := phaseOneExecutor(t, historyTestConfig(serialized))
			a.ID = t.Name()
			capture := webCaptureUsage(t, a.ID)
			f.lookupConfig = phaseOneWithSearchSettings(phaseOneGoogleSearchSettings)
			f.response = `[{"streamAssistResponse":{"answer":{"state":"IN_PROGRESS","replies":[{"groundedContent":{"content":{"text":"hello"}}}]}}},{"streamAssistResponse":{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"content":{"text":"hello world"},"textGroundingMetadata":{"references":[{"documentMetadata":{"uri":"https://example.test/source","title":"Example"}}],"segments":[{"startIndex":0,"endIndex":5,"referenceIndices":[0],"text":"hello"}]}}}]}}}]`
			m := auth.NewManager(nil, nil, nil)
			e.runtime = m.WebRuntime()
			m.SetConfig(e.cfg)
			m.RegisterExecutor(e)
			if _, err := m.Register(ctx, a); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(a.ID, web.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
			defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
			logs := t.TempDir()
			r := gin.New()
			r.Use(middleware.EnterpriseWebBodyAdmission(m), middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(true, logs, "", 10)))
			h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&e.cfg.SDKConfig, m))
			r.POST("/v1beta/models/*action", h.GeminiHandler)
			action := "generateContent"
			if stream {
				action = "streamGenerateContent?alt=sse"
			}
			payload := phaseOneWithTools(historyTestRequest(phaseOneRequest("image/png", "text/plain"), serialized), `"tools":[{"googleSearch":{}}]`).Payload
			req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:"+action, bytes.NewReader(payload)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"finishReason":"STOP"`) || f.calls["widgetStreamAssist"] != 1 {
				t.Fatal(w.Code, w.Body.String(), f.calls)
			}
			tools := gjson.GetBytes(f.final, "streamAssistRequest.toolsSpec")
			if tools.Get("webGroundingSpec").Raw != "{}" || tools.Get("toolRegistry").String() != "default_tool_registry" || f.calls["uploadFile"] != 2 {
				t.Fatal("upstream did not receive the requested search and attachments", f.calls, tools.Raw)
			}
			body := w.Body.String()
			record := capture.next(t)
			if strings.Count(body, `"usageEstimate"`) != 1 || strings.Count(body, `"candidatesTokenCount"`) != 1 ||
				strings.Contains(body, `"promptTokenCount"`) || strings.Contains(body, `"totalTokenCount"`) ||
				record.Detail.Estimate == nil || record.Detail.Estimate.InputTextTokens == nil || record.Detail.OutputTokens <= 0 || record.Detail.TokenPresence.TotalTokens {
				t.Fatalf("HTTP or manager lost partial token provenance: %s / %+v", body, record.Detail)
			}
			for _, expected := range []string{`"groundingMetadata"`, `"retrievedContext":{"uri":"https://example.test/source","title":"Example"}`, `"groundingSupports"`} {
				if strings.Count(body, expected) != 1 {
					t.Fatalf("expected one %s in %s", expected, body)
				}
			}
			if strings.Contains(body, `"webSearchQueries"`) || strings.Contains(body, `"searchEntryPoint"`) || strings.Contains(body, `"web":`) {
				t.Fatal("ambiguous grounding was relabeled as verified Google Search metadata", body)
			}
			if stream {
				frames := 0
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if strings.HasPrefix(line, "data:") {
						frames++
						if !json.Valid([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))) {
							t.Fatal("invalid native SSE JSON framing", line)
						}
					}
				}
				if frames == 0 || strings.Count(w.Body.String(), `"finishReason":"STOP"`) != 1 {
					t.Fatal("missing or duplicate native SSE completion")
				}
			}
			if m.WebRuntime().Status()["active_requests"] != 0 || m.WebRuntime().Status()["inflight_bytes"] != int64(0) {
				t.Fatal("HTTP lease did not drain", m.WebRuntime().Status())
			}
			files, err := os.ReadDir(logs)
			if err != nil || len(files) == 0 {
				t.Fatal("request log missing", err)
			}
			for _, file := range files {
				if file.IsDir() {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(logs, file.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(raw, []byte("aW5wdXQ=")) || bytes.Contains(raw, []byte("synthetic-session")) {
					t.Fatal("media or cookies leaked to log")
				}
				if len(raw) > 65536 {
					t.Fatal("unbounded media request log")
				}
			}
		})
	}
}

func TestEnterpriseWebInputValidationBeforeUpstream(t *testing.T) {
	for _, kind := range []string{"invalid-base64", "unsupported-mime", "too-many", "image-output"} {
		t.Run(kind, func(t *testing.T) {
			e, a, ctx, f := phaseOneExecutor(t, nil)
			req := phaseOneRequest("image/png")
			switch kind {
			case "invalid-base64":
				req.Payload = bytes.Replace(req.Payload, []byte("aW5wdXQ="), []byte("invalid!"), 1)
			case "unsupported-mime":
				req = phaseOneRequest("audio/mpeg")
			case "too-many":
				types := make([]string, 33)
				for i := range types {
					types[i] = "text/plain"
				}
				req = phaseOneRequest(types...)
			case "image-output":
				req.Payload = append([]byte(`{"generationConfig":{"responseModalities":["IMAGE"]},`), req.Payload[1:]...)
			}
			if _, err := e.Execute(ctx, a, req, phaseOneOptions()); err == nil {
				t.Fatal("invalid media was accepted")
			}
			if len(f.calls) != 0 || e.runtime.Status()["active_requests"] != 0 {
				t.Fatal("invalid input touched upstream or leaked resources")
			}
		})
	}
}

// The transport consumes upload framing incrementally, like a remote upstream.
// Test clients retain the input before sampling, so heap deltas describe server work.
func TestEnterpriseWebAttachmentSizesAndMemory(t *testing.T) {
	testEnterpriseWebAttachmentSizesAndMemory(t, false)
}
func TestEnterpriseWebSerializedAttachmentSizesAndMemory(t *testing.T) {
	testEnterpriseWebAttachmentSizesAndMemory(t, true)
}

func testEnterpriseWebAttachmentSizesAndMemory(t *testing.T, serialized bool) {
	for _, sizeMiB := range []int{1, 10, 50} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dMiB/unknown=%t", sizeMiB, unknown), func(t *testing.T) {
				e, a, _, _ := phaseOneExecutor(t, historyTestConfig(serialized))
				m := auth.NewManager(nil, nil, nil)
				e.runtime = m.WebRuntime()
				m.SetConfig(e.cfg)
				m.RegisterExecutor(e)
				if _, err := m.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, web.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
				defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
				raw := bytes.Repeat([]byte{'x'}, sizeMiB<<20)
				payload := `{"contents":[{"role":"user","parts":[{"text":"read this file"},{"inlineData":{"mimeType":"application/pdf","data":"` + base64.StdEncoding.EncodeToString(raw) + `"}}]}]}`
				if serialized {
					payload = historyTestPayload(payload)
				}
				raw = nil
				runtime.GC()
				var before, after, snapshot runtime.MemStats
				runtime.ReadMemStats(&before)
				peak := before.HeapAlloc
				goroutines := runtime.NumGoroutine()
				generated, uploaded := 0, 0
				var reserved int64
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", phaseOneTransport(func(req *http.Request) (*http.Response, error) {
					reply := `{}`
					action := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
					if strings.HasSuffix(action, ":uploadFile") {
						if req.Header.Get("X-Goog-Upload-Command") == "start" {
							action = "uploadStart"
						} else {
							action = "uploadFile"
						}
					}
					if strings.HasSuffix(action, ":uploadFile") {
						if req.Header.Get("X-Goog-Upload-Command") == "start" {
							action = "uploadStart"
						} else {
							action = "uploadFile"
						}
					}
					switch action {
					case "config":
						reply = `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`
					case "lookupWidgetConfig":
						reply = phaseOnePlusConfig
					case "widgetCreateSession":
						reply = `{"session":{"name":"collections/default_collection/engines/synthetic/sessions/synthetic"}}`
					case "uploadStart":
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{"X-Goog-Upload-Url": []string{req.URL.String() + "?upload_id=synthetic"}}}, nil
					case "uploadFile":
						n, err := io.Copy(io.Discard, req.Body)
						if err != nil || n != int64(sizeMiB<<20) {
							t.Fatal("incomplete upload", n, err)
						}
						uploaded++
						reply = `{"fileId":"input-file"}`
					case "widgetStreamAssist":
						generated++
						reply = `[{"streamAssistResponse":{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"content":{"text":"ok"}}}]}}}]`
					default:
						t.Fatal("unexpected operation", action)
					}
					runtime.ReadMemStats(&snapshot)
					peak = max(peak, snapshot.HeapAlloc)
					reserved = max(reserved, m.WebRuntime().Status()["inflight_bytes"].(int64))
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
				}))
				logs := t.TempDir()
				r := gin.New()
				r.Use(middleware.EnterpriseWebBodyAdmission(m), middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(true, logs, "", 10)))
				h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&e.cfg.SDKConfig, m))
				r.POST("/v1beta/models/*action", h.GeminiHandler)
				req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:generateContent", strings.NewReader(payload)).WithContext(ctx)
				if unknown {
					req.ContentLength = -1
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != 200 || uploaded != 1 || generated != 1 {
					t.Fatal(w.Code, w.Body.String(), uploaded, generated)
				}
				if m.WebRuntime().Status()["active_requests"] != 0 || m.WebRuntime().Status()["inflight_bytes"] != int64(0) {
					t.Fatal("resource lease leaked", m.WebRuntime().Status())
				}
				files, err := os.ReadDir(logs)
				if err != nil || len(files) == 0 {
					t.Fatal("logs missing", err)
				}
				for _, f := range files {
					if !f.IsDir() {
						info, _ := f.Info()
						if info.Size() > 65536 {
							t.Fatal("unbounded attachment log", info.Size())
						}
					}
				}
				runtime.ReadMemStats(&after)
				peak = max(peak, after.HeapAlloc)
				if peak-before.HeapAlloc > uint64(reserved) {
					t.Fatalf("sampled server heap exceeded the reserved envelope: %d > %d", peak-before.HeapAlloc, reserved)
				}
				if runtime.NumGoroutine() > goroutines+1 {
					t.Fatal("request goroutines did not return to their baseline")
				}
				t.Logf("wire_bytes=%d reservation_bytes=%d sampled_heap_delta_bytes=%d total_alloc_bytes=%d goroutines_before=%d goroutines_after=%d", len(payload), reserved, peak-before.HeapAlloc, after.TotalAlloc-before.TotalAlloc, goroutines, runtime.NumGoroutine())
			})
		}
	}
}

func TestEnterpriseWebMixedLoadIsolation(t *testing.T) { testEnterpriseWebMixedLoadIsolation(t, false) }
func TestEnterpriseWebSerializedMixedLoadIsolation(t *testing.T) {
	testEnterpriseWebMixedLoadIsolation(t, true)
}

func testEnterpriseWebMixedLoadIsolation(t *testing.T, serialized bool) {
	m, plain, e, _, _ := phaseOneManager(t)
	if serialized {
		cfg := *e.cfg
		cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
		m.SetConfig(&cfg)
	}
	c := phaseOneCredential(t)
	c.Cookies = "SID=synthetic-second-cookie; SAPISID=synthetic-signing"
	a := &auth.Auth{ID: "web-mixed-second", Provider: web.Provider, Status: auth.StatusActive, Metadata: map[string]any{"enterprise_web": c.MetadataValue()}}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	var generations atomic.Int32
	transport := phaseOneTransport(func(req *http.Request) (*http.Response, error) {
		action := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		reply := `{}`
		if strings.HasSuffix(action, ":uploadFile") {
			if req.Header.Get("X-Goog-Upload-Command") == "start" {
				action = "uploadStart"
			} else {
				action = "uploadFile"
			}
		}
		switch action {
		case "config":
			reply = `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`
		case "lookupWidgetConfig":
			reply = phaseOnePlusConfig
		case "widgetCreateSession":
			var data struct {
				Request struct {
					Session struct {
						Name string `json:"name"`
					} `json:"session"`
				} `json:"createSessionRequest"`
			}
			if json.NewDecoder(req.Body).Decode(&data) != nil {
				t.Error("invalid session")
			}
			reply = `{"session":{"name":"collections/default_collection/engines/synthetic/sessions/generated"}}`
		case "uploadStart":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{"X-Goog-Upload-Url": []string{req.URL.String() + "?upload_id=synthetic"}}}, nil
		case "uploadFile":
			if _, err := io.Copy(io.Discard, req.Body); err != nil {
				return nil, err
			}
			reply = `{"fileId":"input"}`
		case "widgetStreamAssist":
			if generations.Add(1) == 2 {
				close(entered)
			}
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			reply = `[{"streamAssistResponse":{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"content":{"text":"ok"}}}]}}}]`
		default:
			t.Error("unexpected operation", action)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
	})
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := context.WithValue(requestCtx, "cliproxy.roundtripper", transport)
	r := gin.New()
	r.Use(middleware.EnterpriseWebBodyAdmission(m), middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(true, t.TempDir(), "", 10)))
	h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&e.cfg.SDKConfig, m))
	r.POST("/v1beta/models/*action", h.GeminiHandler)
	call := func(payload string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:generateContent", strings.NewReader(payload)).WithContext(ctx))
		return w
	}
	text := `{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 32768) + `"}]}]}`
	measure := func() int64 {
		samples := make([]int64, 100)
		for i := range samples {
			start := mixedPerformanceNow()
			w := call(text)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			samples[i] = mixedPerformanceNow() - start
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[94]
	}
	idleP95 := measure()
	media := `{"contents":[{"parts":[{"text":"read this"},{"inlineData":{"mimeType":"application/pdf","data":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, 10<<20)) + `"}}]}]}`
	runtime.GC()
	var before, peak runtime.MemStats
	runtime.ReadMemStats(&before)
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- call(media) }()
	}
	select {
	case <-entered:
	case early := <-results:
		t.Fatalf("media request finished before overlap: %d %s", early.Code, early.Body.String())
	}
	runtime.ReadMemStats(&peak)
	if m.WebRuntime().Status()["active_requests"] != 2 {
		t.Fatal("Web requests did not overlap")
	}
	if w := call(media); w.Code != 429 {
		t.Fatal("concurrency limit queued the request", w.Code)
	}
	mixedP95 := measure()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	peak.HeapAlloc = max(peak.HeapAlloc, after.HeapAlloc)
	close(release)
	for i := 0; i < 2; i++ {
		w := <-results
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"finishReason":"STOP"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if plain.calls.Load() != 200 || generations.Load() != 2 || m.WebRuntime().Status()["active_requests"] != 0 || m.WebRuntime().Status()["inflight_bytes"] != int64(0) {
		t.Fatal("mixed load leaked work or rerouted text", m.WebRuntime().Status())
	}
	t.Logf("web_requests=2 decoded_bytes_each=%d text_requests=200 text_bytes=32768 idle_text_p95_ns=%d mixed_text_p95_ns=%d mixed_sampled_heap_delta_bytes=%d", 10<<20, idleP95, mixedP95, peak.HeapAlloc-before.HeapAlloc)
}
