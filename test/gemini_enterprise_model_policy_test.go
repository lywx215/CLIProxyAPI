package test

import (
	"fmt"
	"io"
	"net/http"
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
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

type enterprisePolicyWireRequest struct {
	path, model, userProject, authorization string
}

type enterprisePolicyWireTransport struct {
	t          *testing.T
	mu         sync.Mutex
	requests   []enterprisePolicyWireRequest
	unexpected atomic.Int32
}

func (f *enterprisePolicyWireTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return f
}

func (f *enterprisePolicyWireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "discoveryengine.googleapis.com" || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, ":streamAssist") {
		f.unexpected.Add(1)
		f.t.Errorf("generation attempted an unexpected HTTP operation: %s %s://%s%s", req.Method, req.URL.Scheme, req.URL.Host, req.URL.Path)
		return nil, fmt.Errorf("offline policy fixture rejects non-generation HTTP operations")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, enterprisePolicyWireRequest{
		path:          req.URL.Path,
		model:         gjson.GetBytes(body, "generationSpec.modelId").String(),
		userProject:   req.Header.Get("X-Goog-User-Project"),
		authorization: req.Header.Get("Authorization"),
	})
	f.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`[
			{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"offline policy response"}}]}},
			{"answer":{"state":"SUCCEEDED"}}
		]`)),
	}, nil
}

func (f *enterprisePolicyWireTransport) snapshot() []enterprisePolicyWireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]enterprisePolicyWireRequest(nil), f.requests...)
}

type enterprisePolicyWireAuth struct {
	id, project, edition, prefix, mode string
	trial, unknown                     bool
	aliases                            []config.OAuthModelAlias
	allowedModels                      []string
}

type enterprisePolicyWireFixture struct {
	router    *gin.Engine
	base      *handlers.BaseAPIHandler
	manager   *coreauth.Manager
	transport *enterprisePolicyWireTransport
	authIDs   map[string]string
}

func enterprisePolicyTrialAuths() []enterprisePolicyWireAuth {
	return []enterprisePolicyWireAuth{
		{id: "business", project: "12345", edition: "business_standard", trial: true},
		{id: "plus", project: "67890", edition: "enterprise_plus", trial: true},
	}
}

func newEnterprisePolicyWireFixture(t *testing.T, cfg *config.Config, auths []enterprisePolicyWireAuth) *enterprisePolicyWireFixture {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 0)
	transport := &enterprisePolicyWireTransport{t: t}
	manager.SetRoundTripperProvider(transport)
	manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
	fixture := &enterprisePolicyWireFixture{manager: manager, transport: transport, authIDs: map[string]string{}}
	catalog := []ge.Model{
		{ID: ge.ArgonModel},
		{ID: "gemini-3.8-flash"},
		{ID: "gemini-3.1-pro-preview"},
		{ID: "auto", DisplayName: "Auto"},
		{ID: "gemini-auto", DisplayName: "Auto"},
	}
	for _, spec := range auths {
		id := "policy-wire-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + spec.id
		metadata := ge.Metadata(config.GeminiEnterpriseOAuth{
			ClientID: "synthetic-policy-client", ProjectID: spec.project, Location: "global", EngineID: "synthetic-policy-engine",
		}, "synthetic-policy@example.invalid", &oauth2.Token{
			AccessToken: "synthetic-policy-access-" + spec.project, RefreshToken: "synthetic-policy-refresh-" + spec.project, Expiry: time.Now().Add(time.Hour),
		})
		if spec.mode != "" {
			metadata["model_policy"] = spec.mode
		}
		if spec.allowedModels != nil {
			metadata["allowed_models"] = spec.allowedModels
		}
		auth := &coreauth.Auth{ID: id, Provider: ge.Provider, Status: coreauth.StatusActive, Prefix: spec.prefix, Metadata: metadata}
		coreauth.SetOAuthModelAliasesAttribute(auth, spec.aliases)
		registered, err := manager.Register(t.Context(), auth)
		if err != nil {
			t.Fatal(err)
		}
		fixture.authIDs[spec.id] = registered.ID
		flight := manager.BeginEnterpriseSubscriptionRefresh(registered)
		if flight == nil {
			t.Fatal("could not start synthetic subscription observation")
		}
		observation := ge.SubscriptionObservation{
			Status: "ok", Edition: spec.edition, FreeTrial: &spec.trial, State: "ACTIVE", ProjectNumber: spec.project,
			PrincipalHash: strings.Repeat("a", 64), CheckedAt: time.Now().UTC(),
		}
		if spec.unknown {
			observation = ge.SubscriptionObservation{Status: "unknown", Reason: "permission_denied", CheckedAt: time.Now().UTC()}
		}
		if !flight.Complete(observation) {
			t.Fatal("synthetic subscription observation was rejected")
		}
		manager.PublishEnterpriseSubscriptionIdentity(registered)
		manager.PublishEnterprisePolicyCatalog(registered, catalog)

		// Keep a deliberately stale, unfiltered registration for both collections.
		// Admission must use the live policy even before model reconciliation runs.
		models := make([]*registry.ModelInfo, 0, len(catalog)+len(spec.aliases))
		addModel := func(model, upstream string) {
			if spec.prefix != "" {
				model = spec.prefix + "/" + model
			}
			models = append(models, &registry.ModelInfo{
				ID: model, Name: "models/" + model, MetadataModelID: upstream, Type: "gemini", OwnedBy: ge.Provider,
				SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
			})
		}
		for _, model := range catalog {
			addModel(model.ID, model.ID)
		}
		for _, alias := range spec.aliases {
			addModel(alias.Alias, alias.Name)
		}
		registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	fixture.base = handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	chat := openaihandlers.NewOpenAIAPIHandler(fixture.base)
	gemini := geminihandlers.NewGeminiAPIHandler(fixture.base)
	fixture.router = gin.New()
	fixture.router.POST("/v1/chat/completions", chat.ChatCompletions)
	fixture.router.POST("/v1beta/models/*action", gemini.GeminiHandler)
	t.Cleanup(func() {
		if got := transport.unexpected.Load(); got != 0 {
			t.Errorf("generation performed %d unexpected HTTP operations", got)
		}
	})
	return fixture
}

func enterprisePolicyAssertWireUsage(t *testing.T, body []byte, protocol string, stream bool) {
	t.Helper()
	packets := []string{string(body)}
	if stream {
		packets = nil
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "data:") {
				packets = append(packets, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
	usagePackets := 0
	for _, packet := range packets {
		root := gjson.Parse(packet)
		node := root.Get("usage")
		input, output, total := "prompt_tokens", "completion_tokens", "total_tokens"
		if protocol == "gemini" {
			node = root.Get("usageMetadata")
			input, output, total = "promptTokenCount", "candidatesTokenCount", "totalTokenCount"
		}
		if !node.Exists() {
			continue
		}
		usagePackets++
		if node.Get(input).Int() <= 0 || node.Get(output).Int() <= 0 || node.Get(total).Int() != node.Get(input).Int()+node.Get(output).Int() {
			t.Fatalf("invalid final usage: %s", packet)
		}
		for _, field := range []string{"thoughtsTokenCount", "cachedContentTokenCount", "prompt_tokens_details", "completion_tokens_details"} {
			if node.Get(field).Exists() {
				t.Fatalf("unknown thinking or cache usage was filled: %s", packet)
			}
		}
		if stream && protocol == "openai" && root.Get("choices.#").Int() != 0 {
			t.Fatalf("stream usage packet contains choices: %s", packet)
		}
	}
	if usagePackets != 1 {
		t.Fatalf("final usage packet count = %d, want 1: %s", usagePackets, body)
	}
}

func enterprisePolicyAssertPublicSuccess(t *testing.T, f *enterprisePolicyWireFixture, model, upstream, project, protocol string, stream bool) {
	t.Helper()
	before := len(f.transport.snapshot())
	fields := map[string]any(nil)
	if protocol == "openai" && stream {
		fields = map[string]any{"stream_options": map[string]any{"include_usage": true}}
	}
	path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
	response := enterprisePublicCall(f.router, path, body)
	if response.Code != http.StatusOK {
		t.Fatalf("generation failed: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := enterprisePublicText(response.Body.Bytes(), protocol, stream); got != "offline policy response" {
		t.Fatalf("generation text = %q", got)
	}
	requests := f.transport.snapshot()
	if len(requests) != before+1 {
		t.Fatalf("generation HTTP calls = %d, want 1", len(requests)-before)
	}
	enterprisePolicyAssertWireRequest(t, requests[before], upstream, project)
	enterprisePolicyAssertWireUsage(t, response.Body.Bytes(), protocol, stream)
}

func enterprisePolicyAssertWireRequest(t *testing.T, actual enterprisePolicyWireRequest, upstream, project string) {
	t.Helper()
	wantPath := "/v1alpha/projects/" + project + "/locations/global/collections/default_collection/engines/synthetic-policy-engine/assistants/default_assistant:streamAssist"
	if actual.path != wantPath || actual.model != upstream || actual.userProject != project || actual.authorization != "Bearer synthetic-policy-access-"+project {
		t.Fatalf("wrong collection or upstream model: path=%q model=%q billing_project=%q", actual.path, actual.model, actual.userProject)
	}
}

func enterprisePolicyAssertNotCooled(t *testing.T, fixture *enterprisePolicyWireFixture) {
	t.Helper()
	for _, id := range fixture.authIDs {
		auth, ok := fixture.manager.GetByID(id)
		if !ok || auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.Unavailable || auth.Quota.Exceeded {
			t.Fatalf("policy denial disabled or cooled collection %q", id)
		}
	}
}

func TestEnterprisePolicyPublicTrialRouting(t *testing.T) {
	fixture := newEnterprisePolicyWireFixture(t, nil, enterprisePolicyTrialAuths())
	for _, model := range []struct{ name, project string }{
		{ge.ArgonModel, "12345"},
		{"gemini-3.8-flash", "67890"},
		{"gemini-3.1-pro-preview", "67890"},
	} {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model.name, protocol, stream), func(t *testing.T) {
					// Repeat to catch a disallowed collection entering round-robin rotation.
					for attempt := 0; attempt < 3; attempt++ {
						enterprisePolicyAssertPublicSuccess(t, fixture, model.name, model.name, model.project, protocol, stream)
					}
				})
			}
		}
	}
}

func TestEnterprisePolicyPublicResolvedAutoRouting(t *testing.T) {
	for _, model := range []struct{ upstream, project string }{
		{ge.ArgonModel, "12345"},
		{"gemini-3.8-flash", "67890"},
		{"gemini-3.1-pro-preview", "67890"},
	} {
		t.Run(model.upstream, func(t *testing.T) {
			fixture := newEnterprisePolicyWireFixture(t, nil, enterprisePolicyTrialAuths())
			// Public auto resolves to the newest concrete registration before admission.
			for _, id := range fixture.authIDs {
				models := registry.GetGlobalRegistry().GetModelsForClient(id)
				for _, info := range models {
					info.Created = 1
					if info.ID == model.upstream {
						info.Created = 2
					}
				}
				registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, models)
			}
			for _, protocol := range []string{"openai", "gemini"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
						enterprisePolicyAssertPublicSuccess(t, fixture, "auto", model.upstream, model.project, protocol, stream)
					})
				}
			}
		})
	}
}

func TestEnterprisePolicyPublicAliasPrefixSuffixRouting(t *testing.T) {
	auths := enterprisePolicyTrialAuths()
	for i := range auths {
		auths[i].prefix = "policy-team"
		auths[i].aliases = []config.OAuthModelAlias{
			{Name: ge.ArgonModel, Alias: "policy-argon", Fork: true},
			{Name: "gemini-3.8-flash", Alias: "policy-flash", Fork: true},
			{Name: "gemini-3.1-pro-preview", Alias: "policy-pro", Fork: true},
		}
	}
	fixture := newEnterprisePolicyWireFixture(t, nil, auths)
	for _, model := range []struct{ route, upstream, project string }{
		{"policy-team/policy-argon(high)", ge.ArgonModel, "12345"},
		{"policy-team/policy-flash(high)", "gemini-3.8-flash", "67890"},
		{"policy-team/policy-pro(low)", "gemini-3.1-pro-preview", "67890"},
	} {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model.route, protocol, stream), func(t *testing.T) {
					enterprisePolicyAssertPublicSuccess(t, fixture, model.route, model.upstream, model.project, protocol, stream)
				})
			}
		}
	}
}

func TestEnterprisePolicyPublicDeniedCallsNeverReachGoogle(t *testing.T) {
	for _, scenario := range []struct {
		name, collection string
		models           []string
		mutate           func([]enterprisePolicyWireAuth)
		cfg              *config.Config
	}{
		{name: "unresolved upstream auto", models: []string{"gemini-auto"}},
		{name: "unverified subscription", models: []string{ge.ArgonModel, "gemini-3.8-flash", "gemini-3.1-pro-preview"}, mutate: func(auths []enterprisePolicyWireAuth) {
			for i := range auths {
				auths[i].unknown = true
			}
		}},
		{name: "unverified subscription with payload rules", models: []string{"gemini-3.8-flash"}, mutate: func(auths []enterprisePolicyWireAuth) {
			for i := range auths {
				auths[i].unknown = true
			}
		}, cfg: &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "gemini-3.8-flash", Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": ge.ArgonModel},
		}}}}},
		{name: "payload argon to flash", collection: "business", models: []string{ge.ArgonModel}, cfg: &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: ge.ArgonModel, Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": "gemini-3.8-flash"},
		}}}}},
		{name: "payload flash to argon", collection: "plus", models: []string{"gemini-3.8-flash"}, cfg: &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "gemini-3.8-flash", Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": ge.ArgonModel},
		}}}}},
		{name: "payload argon to auto", models: []string{ge.ArgonModel}, cfg: &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: ge.ArgonModel, Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": "auto"},
		}}}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			auths := enterprisePolicyTrialAuths()
			if scenario.collection != "" {
				for _, auth := range auths {
					if auth.id == scenario.collection {
						auths = []enterprisePolicyWireAuth{auth}
						break
					}
				}
			}
			if scenario.mutate != nil {
				scenario.mutate(auths)
			}
			fixture := newEnterprisePolicyWireFixture(t, scenario.cfg, auths)
			for _, model := range scenario.models {
				for _, protocol := range []string{"openai", "gemini"} {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
							before := len(fixture.transport.snapshot())
							path, body := enterprisePublicRequest(t, model, protocol, stream, nil)
							response := enterprisePublicCall(fixture.router, path, body)
							if got := len(fixture.transport.snapshot()) - before; got != 0 {
								t.Fatalf("policy denial sent %d generation requests: status=%d body=%s", got, response.Code, response.Body.String())
							}
							if response.Code < http.StatusBadRequest {
								t.Fatalf("policy denial returned status %d: %s", response.Code, response.Body.String())
							}
							enterprisePolicyAssertNotCooled(t, fixture)
						})
					}
				}
			}
		})
	}
}

func TestEnterprisePolicyPublicAllowedFinalPayloadModel(t *testing.T) {
	for _, scenario := range []struct {
		name, collection, requested, upstream string
		wholeSpec                             bool
	}{
		{name: "business flash to argon", collection: "business", requested: "gemini-3.8-flash", upstream: ge.ArgonModel},
		{name: "business pro to argon spec", collection: "business", requested: "gemini-3.1-pro-preview", upstream: ge.ArgonModel, wholeSpec: true},
		{name: "plus argon to flash", collection: "plus", requested: ge.ArgonModel, upstream: "gemini-3.8-flash"},
		{name: "plus argon to pro spec", collection: "plus", requested: ge.ArgonModel, upstream: "gemini-3.1-pro-preview", wholeSpec: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			params := map[string]any{"generationSpec.modelId": scenario.upstream}
			if scenario.wholeSpec {
				params = map[string]any{"generationSpec": map[string]any{"modelId": scenario.upstream}}
			}
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: scenario.requested, Protocol: ge.Provider}}, Params: params,
			}}}}
			var selected enterprisePolicyWireAuth
			for _, auth := range enterprisePolicyTrialAuths() {
				if auth.id == scenario.collection {
					selected = auth
				}
			}
			// The other collection is absent, so success requires final-model admission.
			fixture := newEnterprisePolicyWireFixture(t, cfg, []enterprisePolicyWireAuth{selected})
			for _, protocol := range []string{"openai", "gemini"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
						enterprisePolicyAssertPublicSuccess(t, fixture, scenario.requested, scenario.upstream, selected.project, protocol, stream)
					})
				}
			}
		})
	}
}

func TestEnterprisePolicyPinnedCollectionAdmission(t *testing.T) {
	fixture := newEnterprisePolicyWireFixture(t, nil, enterprisePolicyTrialAuths())
	for _, scenario := range []struct{ collection, model, project string }{
		{"business", ge.ArgonModel, "12345"},
		{"plus", "gemini-3.8-flash", "67890"},
		{"plus", "gemini-3.1-pro-preview", "67890"},
		{"business", "gemini-3.8-flash", ""},
		{"business", "gemini-3.1-pro-preview", ""},
		{"plus", ge.ArgonModel, ""},
	} {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%s/stream=%t", scenario.collection, scenario.model, protocol, stream), func(t *testing.T) {
					before := len(fixture.transport.snapshot())
					_, body := enterprisePublicRequest(t, scenario.model, protocol, stream, nil)
					operation := "generate_content"
					if protocol == "openai" {
						operation = "chat_completions"
					} else if stream {
						operation = "stream_generate_content"
					}
					req := handlers.ModelExecutionRequest{
						Operation: operation, EntryProtocol: protocol, ExitProtocol: protocol,
						Model: scenario.model, Body: body, AuthID: fixture.authIDs[scenario.collection], Stream: stream,
					}
					denied := false
					if stream {
						result, errMsg := fixture.base.ExecuteModelStream(t.Context(), req)
						denied = errMsg != nil
						if errMsg == nil {
							for chunk := range result.Chunks {
								denied = denied || chunk.Err != nil
							}
						}
					} else {
						_, errMsg := fixture.base.ExecuteModel(t.Context(), req)
						denied = errMsg != nil
					}
					if scenario.project != "" {
						requests := fixture.transport.snapshot()
						if denied || len(requests) != before+1 {
							t.Fatalf("valid pin was denied: denied=%t generation_calls=%d", denied, len(requests)-before)
						}
						enterprisePolicyAssertWireRequest(t, requests[before], scenario.model, scenario.project)
						return
					}
					if !denied {
						t.Fatal("pinned request bypassed the collection model policy")
					}
					if got := len(fixture.transport.snapshot()) - before; got != 0 {
						t.Fatalf("pinned policy denial sent %d generation requests", got)
					}
					enterprisePolicyAssertNotCooled(t, fixture)
				})
			}
		}
	}
}

func TestEnterprisePolicyPublicPaidAndManualCatalog(t *testing.T) {
	for _, scenario := range []struct {
		name string
		auth enterprisePolicyWireAuth
	}{
		{name: "paid business", auth: enterprisePolicyWireAuth{id: "paid-business", project: "12345", edition: "business_standard"}},
		{name: "paid plus", auth: enterprisePolicyWireAuth{id: "paid-plus", project: "67890", edition: "enterprise_plus"}},
		{name: "manual catalog with unverified subscription", auth: enterprisePolicyWireAuth{id: "manual-catalog", project: "12345", unknown: true, mode: "catalog"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newEnterprisePolicyWireFixture(t, nil, []enterprisePolicyWireAuth{scenario.auth})
			for _, model := range []string{ge.ArgonModel, "gemini-3.8-flash", "gemini-3.1-pro-preview"} {
				for _, protocol := range []string{"openai", "gemini"} {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
							enterprisePolicyAssertPublicSuccess(t, fixture, model, model, scenario.auth.project, protocol, stream)
						})
					}
				}
			}
		})
	}
}

func TestEnterprisePolicyPublicManualRestrictions(t *testing.T) {
	for _, scenario := range []struct {
		mode    string
		allowed []string
		models  map[string]bool
	}{
		{mode: "argon_only", models: map[string]bool{ge.ArgonModel: true}},
		{mode: "exclude_argon", models: map[string]bool{"gemini-3.8-flash": true, "gemini-3.1-pro-preview": true}},
		{mode: "custom", allowed: []string{"gemini-3.8-flash"}, models: map[string]bool{"gemini-3.8-flash": true}},
		{mode: "custom", allowed: []string{}, models: map[string]bool{}},
	} {
		t.Run(fmt.Sprintf("%s/allowed=%v", scenario.mode, scenario.allowed), func(t *testing.T) {
			fixture := newEnterprisePolicyWireFixture(t, nil, []enterprisePolicyWireAuth{{
				id: "manual", project: "12345", unknown: true, mode: scenario.mode, allowedModels: scenario.allowed,
			}})
			for _, model := range []string{ge.ArgonModel, "gemini-3.8-flash", "gemini-3.1-pro-preview"} {
				for _, protocol := range []string{"openai", "gemini"} {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
							if scenario.models[model] {
								enterprisePolicyAssertPublicSuccess(t, fixture, model, model, "12345", protocol, stream)
								return
							}
							before := len(fixture.transport.snapshot())
							path, body := enterprisePublicRequest(t, model, protocol, stream, nil)
							response := enterprisePublicCall(fixture.router, path, body)
							if got := len(fixture.transport.snapshot()) - before; got != 0 || response.Code < http.StatusBadRequest {
								t.Fatalf("manual restriction: generation_calls=%d status=%d body=%s", got, response.Code, response.Body.String())
							}
							enterprisePolicyAssertNotCooled(t, fixture)
						})
					}
				}
			}
		})
	}
}
