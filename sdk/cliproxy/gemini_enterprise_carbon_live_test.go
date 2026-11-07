package cliproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// This opt-in live test accepts short-lived credentials only through process memory.
// It uses no credential store, browser, administrative mutations or deployed service.
func TestEnterpriseCarbonLiveSystem(t *testing.T) {
	t.Skip("Carbon applications now require the authenticated fixed-application management test flow; the legacy harness never enables applications")
	raw := os.Getenv("CLIPROXY_ENTERPRISE_CARBON_LIVE_JSON")
	if raw == "" {
		t.Skip("requires explicit live Carbon authorization and an in-memory credential")
	}
	var input struct {
		Metadata map[string]any `json:"metadata"`
		Report   string         `json:"report"`
	}
	if json.Unmarshal([]byte(raw), &input) != nil {
		t.Fatal("invalid live test input")
	}
	application := ge.FromMetadata(input.Metadata)
	if application.Validate() != nil || application.ProjectID != "1002653018111" || application.Location != "global" {
		t.Fatal("live test requires the explicitly authorized collection")
	}
	var report = map[string]any{"started_at": time.Now().Format(time.RFC3339), "provider": ge.Provider, "model": ge.CarbonModel, "project_id": application.ProjectID, "location": application.Location, "credential_store": "memory_only"}
	defer func() {
		report["finished_at"], report["passed"] = time.Now().Format(time.RFC3339), !t.Failed()
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(input.Report, append(data, '\n'), 0600) != nil {
			t.Error("could not save the sanitized live test report")
		}
	}()
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: "carbon-live-isolated", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: input.Metadata})
	if err != nil {
		t.Fatal("could not register the isolated credential")
	}
	svc := &Service{cfg: cfg, coreManager: manager, antigravityContext: t.Context()}
	t.Cleanup(func() {
		svc.cancelEnterpriseProbes("")
		svc.enterpriseState().wg.Wait()
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	// Exercise normal subscription verification, model discovery and registration.
	svc.registerEnterpriseModels(t.Context(), auth)
	svc.enterpriseState().wg.Wait()
	policy := manager.EnterpriseModelPolicyView(auth)
	report["subscription_status"], report["subscription_edition"] = policy.Subscription.Status, policy.Subscription.Edition
	report["subscription_free_trial"], report["effective_models"] = policy.Subscription.FreeTrial, policy.EffectiveModels
	if policy.Subscription.Status != "ok" || policy.Subscription.FreeTrial == nil || *policy.Subscription.FreeTrial || !slices.Contains(policy.EffectiveModels, ge.CarbonModel) {
		t.Fatal("live subscription or Carbon model registration was not verified")
	}
	trace := &carbonLiveTransport{transport: http.DefaultTransport}
	manager.SetRoundTripperProvider(trace)
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	gemini := geminihandlers.NewGeminiAPIHandler(base)
	router := gin.New()
	router.GET("/v1beta/models", gemini.GeminiModels)
	router.POST("/v1beta/models/*action", gemini.GeminiHandler)
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	response, err := client.Get(server.URL + "/v1beta/models")
	if err != nil {
		t.Fatal("local system model list failed")
	}
	list, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(list, []byte("models/"+ge.CarbonModel)) {
		t.Fatal("Carbon missing from the local public model list")
	}
	report["public_model_list_contains_carbon"] = true
	results := []map[string]any{}
	report["requests"] = results
	for _, stream := range []bool{false, true} {
		started := time.Now()
		nonce := "CARBON_SYSTEM_" + strings.ReplaceAll(started.Format("150405.000000000"), ".", "")
		action := "generateContent"
		if stream {
			action = "streamGenerateContent?alt=sse"
		}
		body, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "Return exactly this verification code and nothing else: " + nonce}}}}})
		response, err = client.Post(server.URL+"/v1beta/models/"+ge.CarbonModel+":"+action, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal("live system request failed before its response")
		}
		content, errRead := io.ReadAll(response.Body)
		_ = response.Body.Close()
		reply, terminal, hasError := carbonLiveReply(content, stream)
		result := map[string]any{"stream": stream, "started_at": started.Format(time.RFC3339), "finished_at": time.Now().Format(time.RFC3339), "elapsed_ms": time.Since(started).Milliseconds(), "http_status": response.StatusCode, "nonce": nonce, "nonce_matched": strings.TrimSpace(reply) == nonce, "read_complete": errRead == nil, "terminal_stop": terminal, "error_present": hasError, "content_type": response.Header.Get("Content-Type")}
		results = append(results, result)
		report["requests"] = results
		t.Logf("stream=%v status=%d elapsed=%dms nonce=%v terminal=%v", stream, response.StatusCode, result["elapsed_ms"], result["nonce_matched"], result["terminal_stop"])
		if errRead != nil || response.StatusCode != http.StatusOK || result["nonce_matched"] != true || result["terminal_stop"] != true || result["error_present"] != false {
			t.Fatal("live Carbon response did not pass the functional checks")
		}
	}
	trace.mu.Lock()
	report["upstream_generation_models"] = append([]string{}, trace.models...)
	report["upstream_endpoints"] = append([]string{}, trace.paths...)
	valid := slices.Equal(trace.models, []string{ge.CarbonModel, ge.CarbonModel})
	trace.mu.Unlock()
	if !valid {
		t.Fatal("system changed the upstream model or submitted additional generations")
	}
}

func carbonLiveReply(content []byte, stream bool) (string, bool, bool) {
	frames := [][]byte{content}
	if stream {
		frames = nil
		for _, line := range bytes.Split(content, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				frames = append(frames, bytes.TrimSpace(line[len("data:"):]))
			}
		}
	}
	var reply strings.Builder
	terminal, hasError := false, false
	for _, frame := range frames {
		data := gjson.ParseBytes(frame)
		hasError = hasError || data.Get("error").Exists()
		data.Get("candidates").ForEach(func(_, candidate gjson.Result) bool {
			terminal = terminal || candidate.Get("finishReason").String() == "STOP"
			candidate.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
				if !part.Get("thought").Bool() {
					reply.WriteString(part.Get("text").String())
				}
				return true
			})
			return true
		})
	}
	return reply.String(), terminal, hasError
}

type carbonLiveTransport struct {
	mu        sync.Mutex
	transport http.RoundTripper
	models    []string
	paths     []string
}

func (c *carbonLiveTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return c }
func (c *carbonLiveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, ":streamAssist") {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		c.mu.Lock()
		c.models = append(c.models, gjson.GetBytes(body, "generationSpec.modelId").String())
		c.paths = append(c.paths, req.URL.Scheme+"://"+req.URL.Host+req.URL.Path)
		c.mu.Unlock()
	}
	return c.transport.RoundTrip(req)
}
