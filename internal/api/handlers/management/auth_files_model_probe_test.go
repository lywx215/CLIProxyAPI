package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"golang.org/x/oauth2"
)

type enterpriseProbeExecutor struct {
	execute func(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error)
}

func (*enterpriseProbeExecutor) Identifier() string { return ge.Provider }
func (e *enterpriseProbeExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
	return e.execute(ctx, auth, req, opts)
}
func (*enterpriseProbeExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (*coreexec.StreamResult, error) {
	return nil, errors.New("unexpected stream execution")
}
func (*enterpriseProbeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, errors.New("unexpected token count")
}
func (*enterpriseProbeExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("unexpected refresh")
}
func (*enterpriseProbeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected generic HTTP request")
}

func TestEnterpriseModelProbeValidationExecutionAndSanitization(t *testing.T) {
	for _, scenario := range []string{"ok", "disabled", "missing", "index", "home", "excluded", "global-excluded", "media", "media-unmarked", "binding", "rebind", "model-changed", "empty", "truncated", "thought-only", "incomplete", "google-disabled", "google-400", "google-403", "google-429", "continue-rule", "cooling", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			modelID := "enterprise-probe-" + scenario
			manager := coreauth.NewManager(nil, nil, nil)
			binding := (config.GeminiEnterpriseOAuth{ClientID: "test-client", ProjectID: "123", Location: "us", EngineID: "probe-engine"}).WithDefaults()
			metadata := ge.Metadata(binding, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})
			auth := &coreauth.Auth{ID: modelID + ".json", FileName: modelID + ".json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: metadata, Attributes: map[string]string{"auth_kind": "oauth"}}
			if scenario == "disabled" {
				auth.Disabled = true
			}
			if scenario == "excluded" {
				auth.Attributes["excluded_models"] = "enterprise-probe-*"
			}
			auth, errRegister := manager.Register(t.Context(), auth)
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			info := &registry.ModelInfo{ID: modelID, Type: "gemini"}
			if scenario == "media" {
				info.EnterpriseMedia = "image"
			}
			if scenario == "media-unmarked" {
				info.MetadataModelID = "gemini-3-pro-image"
			}
			if scenario != "missing" {
				registry.GetGlobalRegistry().RegisterClient(auth.ID, ge.Provider, []*registry.ModelInfo{info})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}
			// A higher-priority alternate credential must never receive this probe.
			other := auth.Clone()
			other.ID, other.FileName = "other-"+auth.ID, "other-"+auth.FileName
			other.Disabled = false
			other.Attributes = map[string]string{"auth_kind": "oauth", "priority": "100"}
			other, errRegister = manager.Register(t.Context(), other)
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			registry.GetGlobalRegistry().RegisterClient(other.ID, ge.Provider, []*registry.ModelInfo{info})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(other.ID) })
			requestCtx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			calls := 0
			manager.RegisterExecutor(&enterpriseProbeExecutor{execute: func(ctx context.Context, selected *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
				calls++
				coreexec.MarkUpstreamAttempt(ctx)
				if selected.ID != auth.ID || opts.Metadata[coreexec.PinnedAuthMetadataKey] != auth.ID {
					t.Fatalf("probe escaped selected credential: %q", selected.ID)
				}
				if req.Model != modelID || opts.SourceFormat.String() != "gemini" || strings.Contains(string(req.Payload), "maxOutputTokens") || !strings.Contains(string(req.Payload), "Reply with only OK.") {
					t.Fatalf("unexpected probe request: %q %s", req.Model, req.Payload)
				}
				usageContext, ok := ctx.Value("gin").(*gin.Context)
				if !ok || usageContext.GetString("userApiKey") != "" {
					t.Fatal("management key leaked into model API usage")
				}
				switch scenario {
				case "rebind":
					current, _ := manager.GetByID(auth.ID)
					current.Metadata["engine_id"] = "different-engine"
					if _, errUpdate := manager.Update(ctx, current); errUpdate != nil {
						t.Fatal(errUpdate)
					}
				case "model-changed":
					return coreexec.Response{}, &coreauth.EnterpriseModelProbeError{Code: "model_changed", Message: "synthetic-access secret-token", HTTPStatus: 409}
				case "google-disabled":
					return coreexec.Response{}, &ge.GenerationError{HTTPStatus: 400, ModelNotEnabled: true, Model: modelID}
				case "google-400", "google-403", "google-429", "continue-rule":
					status := map[string]int{"google-400": 400, "google-403": 403, "google-429": 429, "continue-rule": 400}[scenario]
					return coreexec.Response{}, &coreauth.Error{HTTPStatus: status, Message: "synthetic-access Bearer secret-token access_token=leak-me"}
				case "cooling":
					return coreexec.Response{}, coreauth.NewModelCooldownError(modelID, ge.Provider, 47*time.Second)
				case "cancel":
					cancel()
					return coreexec.Response{}, ctx.Err()
				}
				payload := `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"OK"}]}}]}`
				switch scenario {
				case "empty":
					payload = `{"candidates":[{"finishReason":"STOP","content":{"parts":[]}}]}`
				case "truncated":
					payload = `{"candidates":[`
				case "thought-only":
					payload = `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"thinking","thought":true}]}}]}`
				case "incomplete":
					payload = `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`
				}
				return coreexec.Response{Payload: []byte(payload)}, nil
			}})
			cfg := &config.Config{}
			if scenario == "home" {
				cfg.Home.Enabled = true
			}
			if scenario == "global-excluded" {
				cfg.OAuthExcludedModels = map[string][]string{ge.Provider: {"enterprise-probe-*"}}
			}
			if scenario == "continue-rule" {
				cfg.OAuthRequestScopedErrors = map[string][]config.RequestScopedErrorRule{ge.Provider: {{
					Status: 400, Match: []string{"synthetic-access"}, Action: coreauth.RequestScopedActionContinueAndCooldown,
				}}}
				manager.SetRetryConfig(3, 3, 2)
			}
			manager.SetConfig(cfg)
			handler := &Handler{cfg: cfg, authManager: manager}
			expected := ge.Resources(binding)
			if scenario == "binding" {
				expected.EngineID = "old-engine"
			}
			index := auth.Index
			if scenario == "index" {
				index = "different-index"
			}
			payload, _ := json.Marshal(map[string]any{"name": auth.FileName, "auth_index": index, "model": modelID, "expected": expected})
			router := gin.New()
			router.POST("/probe", func(c *gin.Context) {
				c.Set("userApiKey", "management-secret")
				handler.TestGeminiEnterpriseModel(c)
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(string(payload))).WithContext(requestCtx)
			router.ServeHTTP(response, request)
			for _, secret := range []string{"management-secret", "synthetic-access", "synthetic-refresh", "secret-token", "leak-me"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatalf("response leaked %q: %s", secret, response.Body)
				}
			}
			rejected := map[string]int{"disabled": 409, "missing": 400, "index": 404, "home": 409, "excluded": 409, "global-excluded": 409, "media": 400, "media-unmarked": 400, "binding": 409}
			if expectedHTTP, rejectedBeforeCall := rejected[scenario]; rejectedBeforeCall {
				if response.Code != expectedHTTP || calls != 0 {
					t.Fatalf("validation: HTTP %d, calls %d: %s", response.Code, calls, response.Body)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("one probe performed %d calls: %s", calls, response.Body)
			}
			if scenario == "rebind" || scenario == "model-changed" {
				if response.Code != 409 {
					t.Fatalf("stale test was accepted: %d %s", response.Code, response.Body)
				}
				return
			}
			if scenario == "cancel" {
				if response.Body.Len() != 0 {
					t.Fatal("canceled request emitted a successful result")
				}
				return
			}
			var result enterpriseModelTestResult
			if errDecode := json.Unmarshal(response.Body.Bytes(), &result); response.Code != 200 || errDecode != nil {
				t.Fatalf("result HTTP %d: %s (%v)", response.Code, response.Body, errDecode)
			}
			want := "failed"
			if scenario == "ok" {
				want = "ok"
			}
			if scenario == "cooling" {
				want = "cooling"
				if result.RetryAfterSeconds != 47 {
					t.Fatalf("lost cooldown timer: %+v", result)
				}
			}
			if result.Status != want || result.AuthIndex != auth.Index || result.Model != modelID || result.Resources != ge.Resources(binding) || result.CheckedAt.IsZero() {
				t.Fatalf("incorrect result: %+v", result)
			}
			if scenario == "google-disabled" && result.Code != "MODEL_NOT_ENABLED" {
				t.Fatalf("lost structured reason: %+v", result)
			}
		})
	}
}

func TestEnterpriseModelProbeRejectsInvalidBody(t *testing.T) {
	for index, body := range []string{`{`, `{}`, `{"name":"f","model":"m","expected":{}}`, `{"name":"f","auth_index":"i","model":"m"}`} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			handler := &Handler{}
			router := gin.New()
			router.POST("/probe", handler.TestGeminiEnterpriseModel)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body)
			}
		})
	}
}

type enterpriseProbeUsageFunc func(context.Context, usage.Record)

func (f enterpriseProbeUsageFunc) HandleUsage(ctx context.Context, record usage.Record) {
	f(ctx, record)
}

func TestEnterpriseModelProbeUsesNormalExecutorUsage(t *testing.T) {
	const modelID = "enterprise-probe-usage-integration"
	manager := coreauth.NewManager(nil, nil, nil)
	binding := (config.GeminiEnterpriseOAuth{ClientID: "probe-client", ProjectID: "123", Location: "global", EngineID: "probe-engine"}).WithDefaults()
	metadata := ge.Metadata(binding, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})
	auth, errRegister := manager.Register(t.Context(), &coreauth.Auth{
		ID: modelID + ".json", FileName: modelID + ".json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: metadata,
		EnterpriseQuotaIdentity: &ge.QuotaIdentity{Binding: ge.Binding(binding), CheckedAt: time.Now()},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, ge.Provider, []*registry.ModelInfo{{ID: modelID, Type: "gemini"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(&config.Config{}))
	records := make(chan usage.Record, 8)
	const pluginName = "enterprise-management-model-probe-usage"
	usage.RegisterNamedPlugin(pluginName, enterpriseProbeUsageFunc(func(_ context.Context, record usage.Record) {
		if record.AuthID == auth.ID {
			records <- record
		}
	}))
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(pluginName, enterpriseProbeUsageFunc(func(context.Context, usage.Record) {}))
	})
	calls := 0
	transport := enterpriseTestTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") != "Bearer synthetic-access" || request.URL.Host != "discoveryengine.googleapis.com" || !strings.HasSuffix(request.URL.Path, ":streamAssist") {
			t.Fatal("probe was sent to an unexpected resource")
		}
		if _, deadline := request.Context().Deadline(); deadline {
			t.Fatal("probe added an inference deadline")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"OK"}}]}},{"answer":{"state":"SUCCEEDED"}}]`))}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	handler := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/probe", func(c *gin.Context) {
		c.Set("userApiKey", "management-secret")
		handler.TestGeminiEnterpriseModel(c)
	})
	payload, _ := json.Marshal(map[string]any{"name": auth.FileName, "auth_index": auth.Index, "model": modelID, "expected": ge.Resources(binding)})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(string(payload))).WithContext(ctx))
	var result enterpriseModelTestResult
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || response.Code != 200 || result.Status != "ok" || calls != 1 {
		t.Fatalf("real executor probe failed: %d %s, calls=%d", response.Code, response.Body, calls)
	}
	// Queue a sentinel behind the synchronous executor's usage publication. Seeing
	// it proves all preceding records were delivered without timing-based sleeps.
	usage.PublishRecord(context.Background(), usage.Record{AuthID: auth.ID, Model: "sentinel"})
	count := 0
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-records:
			if record.Model == "sentinel" {
				if count != 1 {
					t.Fatalf("one probe published %d usage records", count)
				}
				return
			}
			count++
			if record.Failed || record.Model != modelID || record.APIKey != "" || record.Detail.InputTokens <= 0 || record.Detail.OutputTokens <= 0 || record.Detail.TotalTokens != record.Detail.InputTokens+record.Detail.OutputTokens {
				t.Fatalf("normal token accounting was lost: %+v", record)
			}
		case <-timer.C:
			t.Fatal("usage records were not delivered")
		}
	}
}
