package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

type enterprisePolicyAPICallTransport struct {
	t       *testing.T
	mu      sync.Mutex
	calls   int
	body    string
	headers http.Header
	status  int
}

func (f *enterprisePolicyAPICallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !ge.OfficialResource(req) {
		f.t.Errorf("unexpected upstream endpoint %s://%s", req.URL.Scheme, req.URL.Host)
	}
	if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > defaultAPICallTimeout {
		f.t.Error("management API call did not retain its bounded context deadline")
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.calls++
	f.body, f.headers = string(body), req.Header.Clone()
	f.mu.Unlock()
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}, "Location": {"https://untrusted.example.invalid/redirect"}},
		Body:       io.NopCloser(strings.NewReader(`{"fixture":"ok"}`)),
	}, nil
}

func (f *enterprisePolicyAPICallTransport) snapshot() (int, string, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.body, f.headers.Clone()
}

func enterprisePolicyAPICallRouter(h *Handler) *gin.Engine {
	router := gin.New()
	v8 := router.Group("/v8/management")
	v8.Use(func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	v8.POST("/requests/api-call", h.APICall)
	router.POST("/v0/management/api-call", h.APICall)
	return router
}

func enterprisePolicyAPICallRequest(t *testing.T, router *gin.Engine, path string, payload map[string]any, transport http.RoundTripper) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer synthetic-management-secret")
	if transport != nil {
		request = request.WithContext(context.WithValue(request.Context(), "cliproxy.roundtripper", transport))
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestEnterprisePolicyV8APICallUsesCheckedExecutor(t *testing.T) {
	const validBody = `{"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-4-argon"}}`
	for _, scenario := range []struct {
		name, mode, body, suffix, project, endpoint, code, region string
		headers                                                   map[string]string
		disabled, account                                         bool
		paid, unknown, imageDisabled                              bool
		wantStatus, wantCalls, upstreamStatus                     int
	}{
		{name: "allowed argon", body: validBody, wantStatus: 200, wantCalls: 1},
		{name: "ca allowed argon", region: "ca", body: validBody, wantStatus: 200, wantCalls: 1},
		{name: "au allowed argon", region: "au", body: validBody, wantStatus: 200, wantCalls: 1},
		{name: "in allowed argon", region: "in", body: validBody, wantStatus: 200, wantCalls: 1},
		{name: "ca rejects global host", region: "ca", endpoint: ge.Origin, body: validBody, wantStatus: 502},
		{name: "au rejects global host", region: "au", endpoint: ge.Origin, body: validBody, wantStatus: 502},
		{name: "in rejects global host", region: "in", endpoint: ge.Origin, body: validBody, wantStatus: 502},
		{name: "forbidden flash", body: `{"generationSpec":{"modelId":"gemini-3.8-flash"}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "forbidden image with allowed text", body: `{"generationSpec":{"modelId":"gemini-4-argon"},"toolsSpec":{"imageGenerationSpec":{}}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "forbidden video", body: `{"toolsSpec":{"videoGenerationSpec":{}}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "unverified subscription", mode: "auto", body: validBody, code: "subscription_unverified", wantStatus: 503},
		{name: "unresolved auto", body: `{"generationSpec":{"modelId":"auto"}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "unsupported generation operation", body: validBody, suffix: "/assistants/default_assistant:answer", code: "model_policy_operation_unsupported", wantStatus: 400},
		{name: "wrong assistant", body: validBody, suffix: "/assistants/another_assistant:streamAssist", wantStatus: 502},
		{name: "wrong project", body: validBody, project: "67890", wantStatus: 502},
		{name: "untrusted endpoint", body: validBody, endpoint: "https://untrusted.example.invalid", wantStatus: 502},
		{name: "host override", body: validBody, headers: map[string]string{"Host": "untrusted.example.invalid"}, code: "invalid_host", wantStatus: 400},
		{name: "token in body", body: `{"query":{"text":"$TOKEN$"},"generationSpec":{"modelId":"gemini-4-argon"}}`, code: "invalid_token_placement", wantStatus: 400},
		{name: "token in custom header", body: validBody, headers: map[string]string{"X-Custom": "$TOKEN$"}, code: "invalid_token_placement", wantStatus: 400},
		{name: "disabled collection", body: validBody, disabled: true, code: "credential_disabled", wantStatus: 409},
		{name: "account requires collection", body: validBody, account: true, code: "collection_required", wantStatus: 409},
		{name: "redirect is returned without following", body: validBody, upstreamStatus: 302, wantStatus: 200, wantCalls: 1},
		{name: "manual media allowed", mode: "exclude_argon", body: `{"query":{"text":"image"},"toolsSpec":{"imageGenerationSpec":{}}}`, wantStatus: 200, wantCalls: 1},
		{name: "paid auto cannot widen", mode: "auto", paid: true, body: `{"generationSpec":{"modelId":"gemini-3.8-flash"}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "paid catalog cannot widen", mode: "catalog", paid: true, body: `{"generationSpec":{"modelId":"gemini-3.8-flash"}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "paid argon-only cannot widen", mode: "argon_only", paid: true, body: `{"generationSpec":{"modelId":"gemini-3.8-flash"}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "paid exclude-argon cannot widen", mode: "exclude_argon", paid: true, body: `{"generationSpec":{"modelId":"gemini-3.8-flash"}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "paid custom cannot widen", mode: "custom", paid: true, body: `{"generationSpec":{"modelId":"gemini-3.8-flash"}}`, code: "model_not_allowed", wantStatus: 400},
		{name: "paid argon allowed", mode: "catalog", paid: true, body: validBody, wantStatus: 200, wantCalls: 1},
		{name: "paid image allowed", mode: "catalog", paid: true, body: `{"query":{"text":"image"},"toolsSpec":{"imageGenerationSpec":{}}}`, wantStatus: 200, wantCalls: 1},
		{name: "paid video allowed", mode: "catalog", paid: true, body: `{"query":{"text":"video"},"toolsSpec":{"videoGenerationSpec":{}}}`, wantStatus: 200, wantCalls: 1},
		{name: "paid image feature disabled", mode: "catalog", paid: true, imageDisabled: true, body: `{"query":{"text":"image"},"toolsSpec":{"imageGenerationSpec":{}}}`, wantStatus: 502},
		{name: "unknown manual catalog", mode: "catalog", unknown: true, body: validBody, code: "subscription_unverified", wantStatus: 503},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cfg := &config.Config{}
			if scenario.imageDisabled {
				cfg.DisableImageGeneration = config.DisableImageGenerationAll
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
			binding := (config.GeminiEnterpriseOAuth{ClientID: "synthetic-client", ProjectID: "12345", Location: scenario.region, EngineID: "synthetic-app"}).WithDefaults()
			metadata := ge.Metadata(binding, "synthetic@example.invalid", &oauth2.Token{
				AccessToken: "synthetic-enterprise-access", RefreshToken: "synthetic-enterprise-refresh", Expiry: time.Now().Add(time.Hour),
			})
			metadata["model_policy"] = "argon_only"
			if scenario.mode != "" {
				metadata["model_policy"] = scenario.mode
			}
			if scenario.mode == "custom" {
				metadata["allowed_models"] = []string{ge.ArgonModel, "gemini-3.8-flash", "gemini-3-pro-image", "veo-3.1"}
			}
			if scenario.account {
				account, err := ge.AccountFromLegacy(metadata)
				if err != nil {
					t.Fatal(err)
				}
				metadata = ge.PutAccount(metadata, account)
			}
			auth, err := manager.Register(t.Context(), &coreauth.Auth{
				ID: "api-call-policy-" + scenario.name, Provider: ge.Provider, Status: coreauth.StatusActive,
				Metadata: metadata, Disabled: scenario.disabled,
			})
			if err != nil {
				t.Fatal(err)
			}
			manager.PublishEnterprisePolicyCatalog(auth, []ge.Model{
				{ID: ge.ArgonModel}, {ID: "gemini-3.8-flash"}, {ID: "gemini-3-pro-image", MediaKind: "image"}, {ID: "veo-3.1", MediaKind: "video"},
			})
			if scenario.paid {
				trial := false
				publishManagementEnterpriseSubscription(t, manager, auth, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()})
			} else if !scenario.account && scenario.mode != "auto" && !scenario.unknown {
				publishManagementEnterpriseTrial(t, manager, auth)
			}
			project, endpoint, suffix := scenario.project, scenario.endpoint, scenario.suffix
			if project == "" {
				project = "12345"
			}
			if endpoint == "" {
				endpoint = ge.OriginForLocation(binding.Location)
			}
			if suffix == "" {
				suffix = "/assistants/default_assistant:streamAssist"
			}
			headers := map[string]string{"Authorization": "Bearer $TOKEN$", "X-Goog-User-Project": "caller-project", "Content-Type": "application/json"}
			for key, value := range scenario.headers {
				headers[key] = value
			}
			payload := map[string]any{
				"auth_index": auth.Index, "method": http.MethodPost, "header": headers, "data": scenario.body,
				"url": endpoint + "/v1alpha/projects/" + project + "/locations/" + binding.Location + "/collections/default_collection/engines/synthetic-app" + suffix,
			}
			transport := &enterprisePolicyAPICallTransport{t: t, status: scenario.upstreamStatus}
			// Fence the legacy direct HTTP path too, so a regression cannot reach Google.
			var bypassCalls atomic.Int32
			bypassProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				bypassCalls.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer bypassProxy.Close()
			managementCfg := &config.Config{}
			managementCfg.ProxyURL = bypassProxy.URL
			router := enterprisePolicyAPICallRouter(&Handler{cfg: managementCfg, authManager: manager})
			response := enterprisePolicyAPICallRequest(t, router, "/v8/management/requests/api-call", payload, transport)
			if bypassCalls.Load() != 0 {
				t.Fatal("Enterprise API call bypassed the policy-aware executor")
			}
			calls, actualBody, actualHeaders := transport.snapshot()
			if response.Code != scenario.wantStatus || calls != scenario.wantCalls {
				t.Fatalf("status=%d calls=%d; want status=%d calls=%d; body=%s", response.Code, calls, scenario.wantStatus, scenario.wantCalls, response.Body.String())
			}
			if scenario.code != "" && gjson.GetBytes(response.Body.Bytes(), "code").String() != scenario.code {
				t.Fatalf("local policy rejection was lost: %s", response.Body.String())
			}
			if scenario.paid && scenario.wantCalls == 0 && scenario.code == "model_not_allowed" && !strings.Contains(response.Body.String(), "Paid Enterprise subscriptions permit only") {
				t.Fatalf("paid restriction was not explained: %s", response.Body.String())
			}
			for _, private := range []string{"synthetic-enterprise-access", "synthetic-enterprise-refresh", "synthetic-management-secret"} {
				if strings.Contains(response.Body.String(), private) {
					t.Fatal("API call response exposed a credential")
				}
			}
			if calls > 0 {
				if actualBody != scenario.body {
					t.Fatalf("raw payload changed during policy validation: %s", actualBody)
				}
				if actualHeaders.Get("Authorization") != "Bearer synthetic-enterprise-access" || actualHeaders.Get("X-Goog-User-Project") != "12345" {
					t.Fatal("executor did not use the selected collection's authorization and billing project")
				}
				if scenario.upstreamStatus != 0 && gjson.GetBytes(response.Body.Bytes(), "status_code").Int() != int64(scenario.upstreamStatus) {
					t.Fatalf("upstream status was lost: %s", response.Body.String())
				}
			}
		})
	}
}

func TestEnterprisePolicyAPICallKeepsOtherChannelsAndV0(t *testing.T) {
	for _, scenario := range []struct{ name, provider, path string }{
		{"v8 other provider", "codex", "/v8/management/requests/api-call"},
		{"v0 enterprise compatibility", ge.Provider, "/v0/management/api-call"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer synthetic-generic-access" || r.Header.Get("X-Custom") != "synthetic-generic-access" {
					t.Error("generic token substitution changed")
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("generic fixture"))
			}))
			defer upstream.Close()
			manager := coreauth.NewManager(nil, nil, nil)
			auth, err := manager.Register(t.Context(), &coreauth.Auth{
				ID: "generic-api-call", Provider: scenario.provider, Status: coreauth.StatusActive,
				Metadata: map[string]any{"access_token": "synthetic-generic-access", "model_policy": "argon_only"},
			})
			if err != nil {
				t.Fatal(err)
			}
			router := enterprisePolicyAPICallRouter(&Handler{cfg: &config.Config{}, authManager: manager})
			response := enterprisePolicyAPICallRequest(t, router, scenario.path, map[string]any{
				"auth_index": auth.Index, "method": http.MethodGet, "url": upstream.URL,
				"header": map[string]string{"Authorization": "Bearer $TOKEN$", "X-Custom": "$TOKEN$"},
			}, nil)
			if response.Code != http.StatusOK || calls != 1 || gjson.GetBytes(response.Body.Bytes(), "status_code").Int() != http.StatusCreated {
				t.Fatalf("generic API call changed: status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
			}
		})
	}
}
