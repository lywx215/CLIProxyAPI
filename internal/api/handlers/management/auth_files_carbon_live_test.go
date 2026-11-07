package management

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/bcrypt"
)

// This opt-in test exercises the authenticated management flow with an in-memory
// credential. It never enables an application, changes Google configuration, or
// stores OAuth material. Regular test runs cannot initiate a live model call.
func TestCarbonAdmissionLiveManagement(t *testing.T) {
	raw := os.Getenv("CLIPROXY_CARBON_ADMISSION_LIVE_JSON")
	if raw == "" {
		t.Skip("requires explicit live management-test authorization and an in-memory credential")
	}
	var input struct {
		Metadata          map[string]any `json:"metadata"`
		Report            string         `json:"report"`
		CheckGoogleConfig bool           `json:"check_google_config"`
	}
	if json.Unmarshal([]byte(raw), &input) != nil || input.Report == "" {
		t.Fatal("invalid live management-test input")
	}
	binding := ge.FromMetadata(input.Metadata)
	allowed := binding.Location == "global" && binding.EngineID == "gemini-enterprise-17939726_1793972635841" || binding.Location == "au" && binding.EngineID == "gemini-enterprise-17939735_1793973557773"
	if binding.Validate() != nil || !allowed || binding.ProjectID != "1002653018111" || binding.AssistantID != "default_assistant" || binding.CollectionID != "default_collection" {
		t.Fatal("live test requires the explicitly authorized application")
	}
	report := map[string]any{"started_at": time.Now().Format(time.RFC3339), "provider": ge.Provider, "model": ge.CarbonModel, "project_id": binding.ProjectID, "location": binding.Location, "credential_store": "memory_only", "application_enabled": false}
	defer func() {
		report["finished_at"], report["passed"] = time.Now().Format(time.RFC3339), !t.Failed()
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(input.Report, append(data, '\n'), 0600) != nil {
			t.Error("could not save the sanitized management-test report")
		}
	}()
	input.Metadata[ge.CarbonEnabledKey] = false
	delete(input.Metadata, ge.CarbonEvidenceKey)
	delete(input.Metadata, ge.CarbonEvidenceStoreKey)
	account, err := ge.AccountFromLegacy(input.Metadata)
	if err != nil || len(account.Collections) != 1 {
		t.Fatal("could not scope the isolated application")
	}
	metadata := ge.PutAccount(input.Metadata, account)
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
	root, err := manager.Register(t.Context(), &coreauth.Auth{ID: "carbon-admission-live-isolated.json", FileName: "carbon-admission-live-isolated.json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: metadata})
	if err != nil {
		t.Fatal("could not register the in-memory credential")
	}
	children := manager.EnterpriseCollections(root.ID)
	if len(children) != 1 {
		t.Fatal("isolated credential must have exactly one application")
	}
	child := children[0]
	client, err := ge.Client("")
	if err != nil {
		t.Fatal("could not create the Google subscription client")
	}
	defer client.CloseIdleConnections()
	observation := ge.ResolveSubscription(t.Context(), client, binding, ge.String(child.Metadata, "access_token"), time.Now())
	report["subscription_status"], report["subscription_free_trial"] = observation.Status, observation.FreeTrial
	if observation.Status != "ok" || observation.FreeTrial == nil {
		t.Fatal("live assigned subscription could not be verified")
	}
	flight := manager.BeginEnterpriseSubscriptionRefresh(child)
	if flight == nil || !flight.Complete(observation) {
		t.Fatal("live subscription could not be published")
	}
	manager.PublishEnterpriseSubscriptionIdentity(child)
	trace := &carbonAdmissionLiveTransport{transport: http.DefaultTransport}
	manager.SetRoundTripperProvider(trace)
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal("could not create the isolated management key")
	}
	secret := hex.EncodeToString(random[:])
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatal("could not configure management authentication")
	}
	cfg.RemoteManagement.SecretKey = string(hash)
	h := &Handler{authManager: manager, cfg: cfg, localPassword: secret, failedAttempts: make(map[string]*attemptInfo)}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v8/management", h.Middleware(), func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	group.GET("/credentials/models", h.GetAuthFileModels)
	group.POST("/credentials/models/test", h.TestGeminiEnterpriseModel)
	group.POST("/credentials/gemini-enterprise/carbon/configure-google", h.ConfigureGeminiEnterpriseCarbonGoogle)
	public := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router.POST("/v1beta/models/*action", public.GeminiHandler)
	call := func(method, target string, payload []byte, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewReader(payload))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if input.CheckGoogleConfig {
		// This live acceptance may confirm existing settings, but never writes them.
		readTrace := &carbonAdmissionReadOnlyTransport{transport: http.DefaultTransport}
		previous := newGeminiEnterpriseResourceClient
		newGeminiEnterpriseResourceClient = func(proxy string) (*http.Client, error) {
			client, err := previous(proxy)
			if err != nil {
				return nil, err
			}
			if client.Transport != nil {
				readTrace.transport = client.Transport
			}
			client.Transport = readTrace
			return client, nil
		}
		t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
		defer func() {
			readTrace.mu.Lock()
			report["google_config_mutation_attempts"], report["google_resource_reads"] = readTrace.mutations, readTrace.reads
			readTrace.mu.Unlock()
		}()
		configurePayload, _ := json.Marshal(carbonGoogleConfigureRequest{Name: root.FileName, AuthIndex: root.Index, AccountID: account.ID, ExpectedRevision: account.Revision, Applications: []carbonGoogleApplicationRequest{{BindingID: coreauth.EnterpriseBindingID(child), AuthIndex: child.Index, ExpectedRevision: 1, Expected: ge.Resources(binding)}}})
		configured := call(http.MethodPost, "/v8/management/credentials/gemini-enterprise/carbon/configure-google", configurePayload, true)
		status := gjson.GetBytes(configured.Body.Bytes(), "applications.0.status").String()
		report["google_config_http_status"], report["google_config_status"] = configured.Code, status
		report["google_config_reason"] = gjson.GetBytes(configured.Body.Bytes(), "applications.0.reason").String()
		if configured.Code != http.StatusOK || status != "unchanged" || gjson.GetBytes(configured.Body.Bytes(), "applications.#").Int() != 1 || gjson.GetBytes(configured.Body.Bytes(), "gemini_enterprise.carbon_enabled_count").Int() != 0 {
			t.Fatal("existing Google configuration was not confirmed read-only for the isolated application")
		}
	}
	query := url.Values{"name": {root.FileName}, "auth_index": {child.Index}, "binding_id": {coreauth.EnterpriseBindingID(child)}}
	models := call(http.MethodGet, "/v8/management/credentials/models?"+query.Encode(), nil, true)
	report["candidate_visible_before_test"] = gjson.GetBytes(models.Body.Bytes(), "test_candidates.0.id").String() == ge.CarbonModel
	if models.Code != 200 || report["candidate_visible_before_test"] != true || gjson.GetBytes(models.Body.Bytes(), "models.#").Int() != 0 || gjson.GetBytes(models.Body.Bytes(), "carbon_admission.enabled").Bool() {
		t.Fatal("closed Carbon candidate was incorrectly registered or enabled")
	}
	payload, _ := json.Marshal(map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": 1, "expected": ge.Resources(binding), "model": ge.CarbonModel})
	unauthorized := call(http.MethodPost, "/v8/management/credentials/models/test", payload, false)
	report["unauthorized_http_status"] = unauthorized.Code
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("management model test accepted an unauthenticated request")
	}
	started := time.Now()
	result := call(http.MethodPost, "/v8/management/credentials/models/test", payload, true)
	var testResult enterpriseModelTestResult
	if json.Unmarshal(result.Body.Bytes(), &testResult) != nil {
		t.Fatal("management returned an invalid response")
	}
	report["management_http_status"], report["test_status"], report["test_code"] = result.Code, testResult.Status, testResult.Code
	report["elapsed_ms"], report["upstream_model"], report["revision"] = time.Since(started).Milliseconds(), testResult.UpstreamModel, testResult.Revision
	report["expected_revision"], report["carbon_admission"] = testResult.ExpectedRevision, testResult.CarbonAdmission
	if result.Code != 200 || testResult.Status != "ok" || testResult.UpstreamModel != ge.CarbonModel || testResult.CarbonAdmission == nil || testResult.CarbonAdmission.Enabled || testResult.CarbonAdmission.Available || !testResult.CarbonAdmission.CanEnable || testResult.CarbonAdmission.Test == nil || testResult.CarbonAdmission.Test.Status != "verified" || testResult.CarbonAdmission.Test.TestID != testResult.CarbonTestID || testResult.Revision != 1 {
		t.Fatal("live management test did not produce a complete, closed-application verification")
	}
	closed := call(http.MethodPost, "/v1beta/models/"+ge.CarbonModel+":generateContent", []byte(`{"contents":[{"role":"user","parts":[{"text":"this closed application must not generate"}]}]}`), false)
	report["business_http_status_after_test"] = closed.Code
	if closed.Code != http.StatusServiceUnavailable {
		t.Fatal("verification automatically allowed ordinary business requests")
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	report["generation_count"], report["generation_models"] = len(trace.models), append([]string{}, trace.models...)
	if len(trace.models) != 1 || trace.models[0] != ge.CarbonModel {
		t.Fatal("management verification submitted extra or substituted generations")
	}
}

type carbonAdmissionLiveTransport struct {
	mu        sync.Mutex
	transport http.RoundTripper
	models    []string
}

func (c *carbonAdmissionLiveTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return c }
func (c *carbonAdmissionLiveTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, ":streamAssist") {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(body))
		c.mu.Lock()
		c.models = append(c.models, gjson.GetBytes(body, "generationSpec.modelId").String())
		c.mu.Unlock()
	}
	return c.transport.RoundTrip(request)
}

type carbonAdmissionReadOnlyTransport struct {
	mu               sync.Mutex
	transport        http.RoundTripper
	reads, mutations int
}

func (c *carbonAdmissionReadOnlyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.mu.Lock()
	if request.Method != http.MethodGet {
		c.mutations++
		c.mu.Unlock()
		return nil, errors.New("live Google configuration acceptance is read-only")
	}
	c.reads++
	c.mu.Unlock()
	return c.transport.RoundTrip(request)
}
