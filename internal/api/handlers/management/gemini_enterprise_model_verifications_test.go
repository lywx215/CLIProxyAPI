package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

const configuredModelFixtureID = "gemini-test-neon"

func TestConfiguredEnterpriseModelVerificationUsesOnlyExactApplication(t *testing.T) {
	for _, scenario := range []string{
		"published", "concurrent", "pending", "wrong-model", "unknown-send", "excluded", "wrong-engine", "model-override", "sessionless", "no-store",
		"compatibility", "compatibility-second-rejection", "compatibility-unknown-send", "compatibility-payload-override", "compatibility-field-filter",
		"other-invalid", "skipped", "no-visible-text",
	} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			const name = "configured-test.json"
			binding := (config.GeminiEnterpriseOAuth{ClientID: "fixture-client", ProjectID: "1234", Location: "eu", EngineID: "engine"}).WithDefaults()
			metadata := ge.Metadata(binding, scenario+"-configured@example.test", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})
			graph, err := ge.AccountFromLegacy(metadata)
			if err != nil {
				t.Fatal(err)
			}
			graph.Collections[0].Settings = map[string]any{ge.AllModelsEnabledKey: false}
			if scenario == "excluded" {
				graph.Collections[0].Settings["excluded_models"] = []string{configuredModelFixtureID}
			}
			sibling := graph.Collections[0].ResourceBinding
			sibling.CollectionID, sibling.EngineID = "sibling_collection", "sibling-engine"
			graph.Collections = append(graph.Collections, ge.Collection{ID: ge.CollectionID(sibling), Revision: 1, ResourceBinding: sibling})
			meta, _ := json.Marshal(ge.PutAccount(metadata, graph))
			if errWrite := os.WriteFile(filepath.Join(dir, name), meta, 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			if errLoad := manager.Load(t.Context()); errLoad != nil {
				t.Fatal(errLoad)
			}
			var targetAuth, siblingAuth *coreauth.Auth
			for _, child := range manager.EnterpriseCollections(name) {
				switch coreauth.EnterpriseBindingID(child) {
				case graph.Collections[0].ID:
					targetAuth = child
				case graph.Collections[1].ID:
					siblingAuth = child
				}
				publishConfiguredModelTestLicense(t, manager, child)
				if scenario != "pending" {
					manager.PublishEnterprisePolicyCatalog(child, []ge.Model{})
				}
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
			}
			if targetAuth == nil || siblingAuth == nil {
				t.Fatal("fixture did not project both applications")
			}
			target := config.GeminiEnterpriseModelTarget{AccountID: graph.ID, BindingID: graph.Collections[0].ID, ProjectID: binding.ProjectID,
				Location: binding.Location, CollectionID: binding.CollectionID, EngineID: binding.EngineID, AssistantID: binding.AssistantID, LicenseMode: "paid", RuleRevision: 5}
			definitions := []config.GeminiEnterpriseModelDefinition{{ModelID: configuredModelFixtureID, Targets: []config.GeminiEnterpriseModelTarget{target}}}
			if scenario == "published" {
				manager.PublishEnterprisePolicyCatalog(siblingAuth, []ge.Model{{ID: configuredModelFixtureID}, {ID: ge.CarbonModel}, {ID: ge.HydrogenModel}})
				for _, id := range []string{ge.CarbonModel, ge.HydrogenModel} {
					definitions = append(definitions, config.GeminiEnterpriseModelDefinition{ModelID: id, Targets: []config.GeminiEnterpriseModelTarget{target}})
				}
			}
			cfg := &config.Config{AuthDir: dir, GeminiEnterpriseModels: config.GeminiEnterpriseModels{Revision: 1, Definitions: definitions}}
			if scenario == "model-override" || scenario == "sessionless" {
				params := map[string]any{"isSessionLess": true}
				if scenario == "model-override" {
					params = map[string]any{"generationSpec.modelId": ge.ArgonModel}
				}
				cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: configuredModelFixtureID}}, Params: params}}
			}
			if scenario == "compatibility-payload-override" {
				cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: configuredModelFixtureID}},
					Params: map[string]any{"assistSkippingMode": "REQUEST_ASSIST"}}}
			}
			if scenario == "compatibility-field-filter" {
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: configuredModelFixtureID}},
					Params: []string{"assistSkippingMode"}}}
			}
			manager.SetConfig(cfg)
			manager.MarkEnterpriseModelDefinitionsApplied(1)
			manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
			if scenario != "pending" {
				installConfiguredModelRegistration(manager)
			}

			calls, reads := 0, 0
			query := ""
			entered, release := make(chan struct{}), make(chan struct{})
			session := ge.Engine(binding) + "/sessions/verification-fixture"
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", enterpriseTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "www.googleapis.com" {
					return testConfiguredJSONResponse(map[string]any{}), nil
				}
				calls++
				if req.URL.Path != "/v1alpha/"+ge.Engine(binding)+"/assistants/"+binding.AssistantID+":streamAssist" {
					t.Errorf("verification escaped selected application: %s", req.URL.Path)
				}
				body, _ := io.ReadAll(req.Body)
				currentQuery := gjson.GetBytes(body, "query.text").String()
				const prefix = "Why does water expand when it freezes? Please explain in one short sentence. Verification reference: VERIFY_"
				if !strings.HasPrefix(currentQuery, prefix) || gjson.GetBytes(body, "generationSpec.modelId").String() != configuredModelFixtureID {
					t.Errorf("unexpected fixed generation payload: %s", body)
				}
				if calls == 1 {
					query = currentQuery
				} else if query != currentQuery {
					t.Error("the retry changed the verification question")
				}
				skipField := gjson.GetBytes(body, "assistSkippingMode")
				if calls == 1 && scenario != "compatibility-field-filter" && skipField.String() != "REQUEST_ASSIST" {
					t.Error("the initial attempt did not include Google's assist option")
				}
				if (calls > 1 || scenario == "compatibility-field-filter") && skipField.Exists() {
					t.Error("the compatibility payload sent Google's unsupported field")
				}
				if strings.HasPrefix(scenario, "compatibility") && (calls == 1 || scenario == "compatibility-second-rejection") || scenario == "other-invalid" {
					field := "assistSkippingMode"
					if scenario == "other-invalid" {
						field = "temperature"
					}
					rejection := testConfiguredJSONResponse([]any{map[string]any{"error": map[string]any{
						"code": 400, "status": "INVALID_ARGUMENT",
						"message": "Invalid JSON payload received. Unknown name \"" + field + "\": Cannot find field.",
						"details": []any{map[string]any{"@type": "type.googleapis.com/google.rpc.BadRequest"}},
					}}})
					rejection.StatusCode = http.StatusBadRequest
					return rejection, nil
				}
				if scenario == "skipped" {
					return testConfiguredJSONResponse([]any{map[string]any{"answer": map[string]any{
						"state": "SKIPPED", "assistSkippedReasons": []string{"NON_ASSIST_SEEKING_QUERY_IGNORED", "CUSTOMER_POLICY_VIOLATION"},
					}}}), nil
				}
				if scenario == "unknown-send" || scenario == "compatibility-unknown-send" {
					return nil, errors.New("synthetic transport interruption")
				}
				if scenario == "concurrent" && calls == 1 {
					close(entered)
					<-release
				}
				return testConfiguredJSONResponse([]any{map[string]any{
					"sessionInfo": map[string]string{"session": session},
					"answer": map[string]any{"state": "SUCCEEDED",
						"replies": []any{map[string]any{"content": map[string]any{
							"text": "Freezing creates a crystal structure with more space.", "thought": scenario == "no-visible-text",
						}}}},
				}}), nil
			}))
			previous := newGeminiEnterpriseResourceClient
			t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
			newGeminiEnterpriseResourceClient = func(string) (*http.Client, error) {
				return &http.Client{Transport: carbonSessionRoundTrip(func(req *http.Request) (*http.Response, error) {
					reads++
					if req.Method != http.MethodGet || req.URL.Path != "/v1alpha/"+session || !strings.Contains(req.URL.Query().Get("fields"), "queryConfig") {
						t.Errorf("unexpected session evidence request: %s %s", req.Method, req.URL)
					}
					actual := configuredModelFixtureID
					if scenario == "wrong-model" {
						actual = ge.ArgonModel
					}
					return testConfiguredJSONResponse(map[string]any{"name": session, "turns": []any{map[string]any{
						"query": map[string]string{"text": query}, "queryConfig": map[string]string{"google.discoveryengine.googleapis.com.Assistant.model_id": actual},
					}}}), nil
				})}, nil
			}
			h := &Handler{cfg: cfg, authManager: manager}
			if scenario == "no-store" {
				h.authManager = coreauth.NewManager(nil, nil, nil)
			}
			router := gin.New()
			router.POST("/verify", h.VerifyConfiguredEnterpriseModel)
			router.GET("/verify/:request_id", h.GetConfiguredEnterpriseModelVerification)
			router.GET("/models", func(c *gin.Context) { c.Set(ConfigV8ContextKey, true); h.GetAuthFileModels(c) })
			request := func(method, path string, body any) *httptest.ResponseRecorder {
				encoded, _ := json.Marshal(body)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewReader(encoded)).WithContext(ctx))
				return recorder
			}
			req := enterpriseVerificationRequest{RequestID: "fixture-" + scenario, Name: name, AuthIndex: targetAuth.Index, ModelID: configuredModelFixtureID,
				AccountID: graph.ID, BindingID: target.BindingID, RuleRevision: target.RuleRevision, ExpectedRevision: 1, AuthorizationRevision: 1, Expected: ge.Resources(binding)}
			if scenario == "wrong-engine" {
				req.Expected.EngineID = "sibling-engine"
			}
			if scenario == "no-store" {
				response := request(http.MethodPost, "/verify", req)
				if response.Code != 503 || !strings.Contains(response.Body.String(), "verification_store_unavailable") || calls != 0 {
					t.Fatalf("non-durable verification accepted: %s", response.Body)
				}
				return
			}
			if missing := request(http.MethodGet, "/verify/"+req.RequestID, nil); missing.Code != 404 {
				t.Fatalf("missing receipt: %d %s", missing.Code, missing.Body)
			}
			var response *httptest.ResponseRecorder
			if scenario == "concurrent" {
				first := make(chan *httptest.ResponseRecorder, 1)
				go func() { first <- request(http.MethodPost, "/verify", req) }()
				select {
				case <-entered:
				case early := <-first:
					close(release)
					t.Fatalf("verification stopped before transport: %s", early.Body)
				case <-time.After(5 * time.Second):
					close(release)
					t.Fatal("verification did not reach transport")
				}
				duplicate := request(http.MethodPost, "/verify", req)
				close(release)
				response = <-first
				if duplicate.Code != 200 || gjson.GetBytes(duplicate.Body.Bytes(), "state").String() != "reconcile_required" {
					t.Fatalf("concurrent replay did not see durable reservation: %s", duplicate.Body)
				}
			} else {
				response = request(http.MethodPost, "/verify", req)
			}
			var result enterpriseVerificationResult
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
				t.Fatalf("verification status %d: %s", response.Code, response.Body)
			}
			wantState, wantCalls, wantReads := "failed", 0, 0
			switch scenario {
			case "published", "concurrent":
				wantState, wantCalls, wantReads = "published", 1, 1
			case "pending":
				wantState, wantCalls, wantReads = "runtime_pending", 1, 1
			case "wrong-model":
				wantCalls, wantReads = 1, 1
			case "unknown-send":
				wantState, wantCalls = "reconcile_required", 1
			case "compatibility":
				wantState, wantCalls, wantReads = "published", 2, 1
			case "compatibility-unknown-send":
				wantState, wantCalls = "reconcile_required", 2
			case "compatibility-second-rejection":
				wantCalls = 2
			case "compatibility-payload-override", "compatibility-field-filter", "other-invalid", "skipped":
				wantCalls = 1
			case "no-visible-text":
				wantState = "reconcile_required"
				wantCalls = 1
			}
			if result.AssistSkippingModeRetried != (wantCalls == 2) {
				t.Fatalf("the receipt lost the bounded compatibility retry: %+v", result)
			}
			if scenario == "skipped" &&
				(result.Reason != "upstream_skipped" || !slices.Equal(result.AssistSkippedReasons, []string{"NON_ASSIST_SEEKING_QUERY_IGNORED", "CUSTOMER_POLICY_VIOLATION"})) {
				t.Fatalf("the receipt lost Google's skip reasons: %+v", result)
			}
			if result.State != wantState || calls != wantCalls || reads != wantReads {
				t.Fatalf("unexpected result: %+v, generation %d, evidence read %d", result, calls, reads)
			}
			if scenario == "model-override" && result.Reason != "model_changed" ||
				(scenario == "sessionless" || scenario == "compatibility-payload-override") && result.Reason != "verification_payload_changed" {
				t.Fatalf("configured payload override was not rejected at the final barrier: %+v", result)
			}
			if scenario == "pending" {
				if result.HTTPStatus != 200 || result.ActualModelID != configuredModelFixtureID {
					t.Fatalf("runtime pending lost successful proof: %+v", result)
				}
				installConfiguredModelRegistration(manager)
				wantState = "published"
			}
			if replay := request(http.MethodPost, "/verify", req); replay.Code != 200 || gjson.GetBytes(replay.Body.Bytes(), "state").String() != wantState || calls != wantCalls || reads != wantReads {
				t.Fatalf("replay made another generation or lost receipt: %s", replay.Body)
			}
			if saved := request(http.MethodGet, "/verify/"+req.RequestID, nil); saved.Code != 200 || gjson.GetBytes(saved.Body.Bytes(), "state").String() != wantState {
				t.Fatalf("receipt lookup failed: %s", saved.Body)
			}
			changed := req
			changed.ExpectedRevision++
			if replay := request(http.MethodPost, "/verify", changed); replay.Code != 409 || calls != wantCalls {
				t.Fatalf("request ID admitted a changed scope: %s", replay.Body)
			}
			if wantState != "published" {
				return
			}
			models := request(http.MethodGet, "/models?name="+name+"&auth_index="+targetAuth.Index+"&binding_id="+target.BindingID, nil)
			listing := gjson.GetBytes(models.Body.Bytes(), "models.#(upstream_model==\""+configuredModelFixtureID+"\")")
			if models.Code != 200 || !listing.Exists() || listing.Get("upstream_listed").Bool() || listing.Get("catalog_source").String() != "verified_application" {
				t.Fatalf("model listing misreports verified model provenance: %s", models.Body)
			}
			if scenario != "published" {
				if len(registry.GetGlobalRegistry().GetModelsForClient(siblingAuth.ID)) != 0 {
					t.Fatal("proof registered an absent model on a different application")
				}
				return
			}
			for _, id := range []string{configuredModelFixtureID, ge.CarbonModel, ge.HydrogenModel} {
				if errCatalog := manager.ValidateEnterpriseModelCatalog(siblingAuth, id); errCatalog != nil || !registry.GetGlobalRegistry().ClientSupportsModel(siblingAuth.ID, id) {
					t.Fatalf("rule suppressed a different application's native model %s: %v", id, errCatalog)
				}
			}
			restarted := coreauth.NewManager(store, nil, nil)
			restarted.SetConfig(cfg)
			if errLoad := restarted.Load(t.Context()); errLoad != nil {
				t.Fatal(errLoad)
			}
			current, _ := restarted.GetByID(targetAuth.ID)
			publishConfiguredModelTestLicense(t, restarted, current)
			if !slices.Contains(restarted.EnterpriseModelPolicyView(current).EffectiveModels, configuredModelFixtureID) {
				t.Fatal("new process did not read the saved proof")
			}
			updated := cfg.CloneForRuntime()
			updated.GeminiEnterpriseModels.Definitions[0].Targets[0].RuleRevision++
			restarted.SetConfig(updated)
			if slices.Contains(restarted.EnterpriseModelPolicyView(current).EffectiveModels, configuredModelFixtureID) {
				t.Fatal("changed rule reused the earlier proof")
			}
		})
	}
}

func installConfiguredModelRegistration(manager *coreauth.Manager) {
	manager.SetEnterpriseAccountSyncHook(func(_ context.Context, owner *coreauth.Auth) error {
		for _, child := range manager.EnterpriseCollections(owner.ID) {
			var models []*registry.ModelInfo
			for _, model := range manager.EnterpriseModelPolicyView(child).EffectiveModels {
				models = append(models, &registry.ModelInfo{ID: model, MetadataModelID: model, Type: "gemini"})
			}
			registry.GetGlobalRegistry().RegisterClient(child.ID, ge.Provider, models)
		}
		return nil
	})
}

func publishConfiguredModelTestLicense(t *testing.T, m *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	trial := false
	publishManagementEnterpriseSubscription(t, m, auth, ge.SubscriptionObservation{
		Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now(),
	})
}

func testConfiguredJSONResponse(value any) *http.Response {
	body, _ := json.Marshal(value)
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}
}

type configuredVerificationCASFixture struct {
	*sdkauth.FileTokenStore
	data    []byte
	version int
}

func (*configuredVerificationCASFixture) SupportsEnterpriseModelVerificationJournal() bool {
	return true
}

func (store *configuredVerificationCASFixture) ReadEnterpriseModelVerification(context.Context, string) ([]byte, string, error) {
	if store.version == 0 {
		return nil, "", os.ErrNotExist
	}
	return bytes.Clone(store.data), strconv.Itoa(store.version), nil
}

func (store *configuredVerificationCASFixture) WriteEnterpriseModelVerification(_ context.Context, _ string, data []byte, tag string) error {
	if store.version == 0 && tag != "" || store.version > 0 && tag != strconv.Itoa(store.version) {
		return os.ErrExist
	}
	store.version++
	store.data = bytes.Clone(data)
	return nil
}

func TestEnterpriseModelVerificationManagementUsesSharedConditionalJournal(t *testing.T) {
	store := &configuredVerificationCASFixture{FileTokenStore: sdkauth.NewFileTokenStore()}
	h := &Handler{cfg: &config.Config{}, authManager: coreauth.NewManager(store, nil, nil)}
	// No local auth directory is configured. The fake store requires the ETag
	// returned after the reservation, so a second unconditional write fails.
	req := enterpriseVerificationRequest{RequestID: "shared-receipt-fixture", ModelID: configuredModelFixtureID,
		Name: "fixture.json", AuthIndex: "fixture", AccountID: "fixture-account", BindingID: "fixture-binding",
		RuleRevision: 1, ExpectedRevision: 1, AuthorizationRevision: 1,
		Expected: ge.ResourceBinding{ProjectID: "1234", Location: "eu", CollectionID: "default_collection", EngineID: "engine", AssistantID: "default_assistant"},
	}
	for i := 0; i < 2; i++ {
		reply := enterpriseManagementRequest(t, h.VerifyConfiguredEnterpriseModel, http.MethodPost, req)
		if reply.Code != 200 || gjson.GetBytes(reply.Body.Bytes(), "state").String() != "failed" || store.version != 2 {
			t.Fatalf("shared journal did not persist and replay the rejection: %d %s version=%d", reply.Code, reply.Body, store.version)
		}
	}
}
