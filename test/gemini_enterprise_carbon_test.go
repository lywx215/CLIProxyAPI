package test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestEnterpriseHydrogenPublicHTTPAdmissionAndAliases(t *testing.T) {
	for _, protocol := range []string{"gemini", "openai"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []struct {
				name                                                             string
				legacy                                                           *bool
				trial, inactive, missing, empty, disabled, force, rewrite, allow bool
				mode, variant                                                    string
			}{
				{name: "paid", allow: true},
				{name: "legacy-off", legacy: new(false), allow: true},
				{name: "legacy-on", legacy: new(true), allow: true},
				{name: "trial-legacy-on", trial: true, legacy: new(true), force: true},
				{name: "trial-with-all-models", trial: true, mode: "all"},
				{name: "inactive", inactive: true, legacy: new(true), force: true},
				{name: "missing-in-catalog", missing: true, legacy: new(true)},
				{name: "empty-catalog", empty: true, legacy: new(true)},
				{name: "disabled", disabled: true},
				{name: "only-group", mode: "argon_only", allow: true},
				{name: "exclude-group", mode: "exclude_argon", force: true},
				{name: "custom-list", mode: "custom"},
				{name: "alias-with-prefix", variant: "alias", allow: true},
				{name: "alias-on-trial", variant: "alias", trial: true},
				{name: "thinking-suffix", variant: "suffix", allow: true},
				{name: "rewritten-paid", rewrite: true, allow: true},
				{name: "rewritten-trial", rewrite: true, trial: true},
				{name: "rewritten-missing", rewrite: true, missing: true},
				{name: "upstream-auto-on-trial", variant: "upstream-auto", trial: true, mode: "all"},
			} {
				t.Run(protocol+"/"+scenario.name+map[bool]string{true: "/stream", false: "/full"}[stream], func(t *testing.T) {
					cfg := &config.Config{}
					requested, upstream := ge.HydrogenModel, ge.HydrogenModel
					if scenario.rewrite {
						requested, upstream = ge.ArgonModel, ge.ArgonModel
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-*", Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": ge.HydrogenModel}}}
					}
					if scenario.variant == "alias" {
						cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{ge.Provider: {{Name: ge.HydrogenModel, Alias: "hydrogen-public"}}}
						requested = "work/hydrogen-public"
					}
					if scenario.variant == "upstream-auto" {
						requested, upstream = "gemini-auto", "gemini-auto"
					}
					router, transport, manager := enterpriseFixtureRouterWithConfig(t, cfg)
					auth, _ := manager.GetByID("enterprise-wire-fixture")
					manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
					if scenario.variant == "alias" {
						auth.Prefix = "work"
					}
					if scenario.legacy != nil {
						auth.Metadata[ge.HydrogenEnabledKey] = *scenario.legacy
					}
					if scenario.mode == "all" {
						auth.Metadata[ge.AllModelsEnabledKey] = true
					} else if scenario.mode != "" {
						auth.Metadata["model_policy"] = scenario.mode
					}
					if scenario.mode == "custom" {
						auth.Metadata["allowed_models"] = []string{ge.ArgonModel}
					}
					if scenario.force {
						auth.Metadata[ge.ArgonForceEnabledKey] = true
					}
					auth.Disabled = scenario.disabled
					auth, err := manager.Update(t.Context(), auth)
					if err != nil {
						t.Fatal(err)
					}
					observation := ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &scenario.trial}
					if scenario.inactive {
						observation.Status = "inactive"
					}
					enterprisePublishSubscription(t, manager, auth, observation)
					var catalog []ge.Model
					if !scenario.empty {
						catalog = append(catalog, ge.Model{ID: ge.ArgonModel})
						if scenario.variant == "upstream-auto" {
							catalog = append(catalog, ge.Model{ID: upstream, DisplayName: "Auto"})
						}
						if !scenario.missing {
							catalog = append(catalog, ge.Model{ID: ge.HydrogenModel})
						}
					}
					manager.PublishEnterprisePolicyCatalog(auth, catalog)
					// Intentionally leave a registry candidate even when the live policy denies it.
					registry.GetGlobalRegistry().RegisterClient(auth.ID, ge.Provider, []*registry.ModelInfo{{ID: requested, MetadataModelID: upstream, Type: "gemini", SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"}}})
					if scenario.variant == "suffix" {
						requested += "(high)"
					}
					path, body := enterprisePublicRequest(t, requested, protocol, stream, nil)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
					if scenario.allow {
						if response.Code != http.StatusOK || transport.calls.Load() != 1 || gjson.GetBytes(transport.requestBody(), "generationSpec.modelId").String() != ge.HydrogenModel || !strings.Contains(response.Body.String(), "print(7)") {
							t.Fatalf("Hydrogen did not reach the exact upstream once: status=%d calls=%d body=%s", response.Code, transport.calls.Load(), response.Body.String())
						}
					} else if response.Code < 400 || transport.calls.Load() != 0 {
						t.Fatalf("Hydrogen policy bypassed: status=%d calls=%d body=%s", response.Code, transport.calls.Load(), response.Body.String())
					}
				})
			}
		}
	}
}

func TestEnterprisePublicCredentialExhaustionUses429(t *testing.T) {
	for _, route := range []struct {
		protocol string
		stream   bool
	}{{"openai", false}, {"openai", true}, {"gemini", false}, {"gemini", true}} {
		for _, scenario := range []struct {
			name, model string
			wantStatus  int
		}{
			{name: "no credential", model: ge.HydrogenModel, wantStatus: http.StatusTooManyRequests},
			{name: "configured alias", model: "paid-hydrogen-alias", wantStatus: http.StatusTooManyRequests},
			{name: "disabled registered route", model: ge.ArgonModel, wantStatus: http.StatusTooManyRequests},
			{name: "all credentials expired", model: ge.ArgonModel, wantStatus: http.StatusTooManyRequests},
			{name: "catalog model no longer registered", model: "workspace/gemini-3.8-flash", wantStatus: http.StatusTooManyRequests},
			{name: "local catalog rejection", model: ge.HydrogenModel, wantStatus: http.StatusTooManyRequests},
			{name: "unknown model", model: "gemini-unknown-credential-test", wantStatus: http.StatusBadRequest},
			{name: "bad payload", model: ge.ArgonModel, wantStatus: http.StatusBadRequest},
		} {
			t.Run(route.protocol+"/"+scenario.name+map[bool]string{true: "/stream", false: "/full"}[route.stream], func(t *testing.T) {
				cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
					ge.Provider: {{Name: ge.HydrogenModel, Alias: "paid-hydrogen-alias"}},
				}}
				var auths []enterprisePolicyWireAuth
				switch scenario.name {
				case "disabled registered route", "all credentials expired", "local catalog rejection", "bad payload":
					auths = []enterprisePolicyWireAuth{{id: "paid", project: "12345", edition: "enterprise_plus", mode: "catalog"}}
				case "catalog model no longer registered":
					auths = []enterprisePolicyWireAuth{{id: "paid", project: "12345", edition: "enterprise_plus", mode: "catalog", prefix: "workspace"}}
				}
				fixture := newEnterprisePolicyWireFixture(t, cfg, auths)
				fixture.manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				if id := fixture.authIDs["paid"]; id != "" {
					a, _ := fixture.manager.GetByID(id)
					switch scenario.name {
					case "disabled registered route", "catalog model no longer registered":
						a.Disabled, a.Status = true, coreauth.StatusDisabled
						if _, err := fixture.manager.Update(t.Context(), a); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().UnregisterClient(id)
						if scenario.name == "disabled registered route" {
							// A stale registration should produce the same status.
							registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, []*registry.ModelInfo{{ID: ge.ArgonModel}})
						}
					case "local catalog rejection":
						registry.GetGlobalRegistry().RegisterClient(id, ge.Provider, []*registry.ModelInfo{{ID: ge.HydrogenModel}})
					case "all credentials expired":
						a.Status, a.Unavailable, a.StatusMessage = coreauth.StatusError, true, "unauthorized"
						a.LastError = &coreauth.Error{Code: "unauthorized", Message: "authorization expired", HTTPStatus: http.StatusUnauthorized}
						if _, err := fixture.manager.Update(t.Context(), a); err != nil {
							t.Fatal(err)
						}
					}
				}
				requestFields := map[string]any{}
				if scenario.name == "bad payload" {
					field := "messages"
					if route.protocol == "gemini" {
						field = "contents"
					}
					requestFields[field] = "invalid"
				}
				path, body := enterprisePublicRequest(t, scenario.model, route.protocol, route.stream, requestFields)
				response := enterprisePublicCall(fixture.router, path, body)
				if response.Code != scenario.wantStatus {
					t.Fatalf("HTTP %d, want %d: %s", response.Code, scenario.wantStatus, response.Body.String())
				}
				wantType := "invalid_request_error"
				if scenario.wantStatus == http.StatusTooManyRequests {
					wantType = "rate_limit_error"
					if scenario.name == "all credentials expired" {
						wantType = "authentication_error"
					}
				}
				if got := gjson.GetBytes(response.Body.Bytes(), "error.type").String(); got != wantType {
					t.Fatalf("error type = %q, want %q: %s", got, wantType, response.Body.String())
				}
				if got := len(fixture.transport.snapshot()); got != 0 {
					t.Fatalf("request reached upstream %d times", got)
				}
			})
		}
	}
}

func TestEnterprisePublicAvailabilityDistinguishesUpstream400(t *testing.T) {
	const mismatch = `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"MODEL_NOT_ENABLED"}]}}`
	const invalid = `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"invalid request"}}`
	for _, stream := range []bool{false, true} {
		for _, scenario := range []struct {
			name, body string
			wantStatus int
		}{
			{"model unavailable", mismatch, http.StatusTooManyRequests},
			{"invalid argument", invalid, http.StatusBadRequest},
		} {
			t.Run(scenario.name+map[bool]string{true: "/stream", false: "/full"}[stream], func(t *testing.T) {
				router, transport, _ := enterpriseFixtureRouter(t)
				transport.upstreamStatus, transport.upstreamBody = http.StatusBadRequest, scenario.body
				path, body := enterprisePublicRequest(t, ge.ArgonModel, "openai", stream, nil)
				response := enterprisePublicCall(router, path, body)
				if response.Code != scenario.wantStatus || transport.calls.Load() != 1 {
					t.Fatalf("HTTP %d, want %d, upstream calls %d: %s", response.Code, scenario.wantStatus, transport.calls.Load(), response.Body.String())
				}
			})
		}
	}
}

func TestEnterpriseHydrogenFinalPolicyUsesAnEligibleApplication(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, rewrite := range []bool{false, true} {
			name := map[bool]string{true: "rewritten", false: "explicit"}[rewrite]
			t.Run(name+map[bool]string{true: "/stream", false: "/full"}[stream], func(t *testing.T) {
				cfg := &config.Config{}
				requested := ge.HydrogenModel
				if rewrite {
					requested = ge.ArgonModel
					cfg.Payload.Override = []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: ge.ArgonModel, Protocol: ge.Provider}},
						Params: map[string]any{"generationSpec.modelId": ge.HydrogenModel},
					}}
				}
				fixture := newEnterprisePolicyWireFixture(t, cfg, []enterprisePolicyWireAuth{
					{id: "first", project: "12345", edition: "enterprise_plus", mode: "catalog", trial: rewrite, priority: "100"},
					{id: "paid", project: "67890", edition: "enterprise_plus", mode: "catalog"},
				})
				for _, name := range []string{"first", "paid"} {
					auth, _ := fixture.manager.GetByID(fixture.authIDs[name])
					catalog := []ge.Model{{ID: ge.ArgonModel}}
					if name == "paid" || rewrite {
						catalog = append(catalog, ge.Model{ID: ge.HydrogenModel})
					}
					fixture.manager.PublishEnterprisePolicyCatalog(auth, catalog)
					// Preserve a stale catalog registration to exercise the final check.
					registry.GetGlobalRegistry().RegisterClient(auth.ID, ge.Provider, []*registry.ModelInfo{
						{ID: requested, Type: "gemini", OwnedBy: ge.Provider},
					})
				}
				records := make(chan coreusage.Record, 8)
				coreusage.RegisterNamedPlugin(t.Name(), enterpriseHydrogenUsageCapture{
					first: fixture.authIDs["first"], paid: fixture.authIDs["paid"], records: records,
				})
				t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), enterpriseReasoningContractNoop{}) })
				enterprisePolicyAssertPublicSuccess(t, fixture, requested, ge.HydrogenModel, "67890", "openai", stream)
				enterprisePolicyAssertNotCooled(t, fixture)
				first, _ := fixture.manager.GetByID(fixture.authIDs["first"])
				if first.Failed != 0 || first.Success != 0 || len(first.ModelStates) != 0 {
					t.Fatalf("local policy rejection counted as an upstream request: success=%d failed=%d", first.Success, first.Failed)
				}
				select {
				case record := <-records:
					// Usage events are ordered. A local rejection would be delivered before
					// the request completed by the paid application.
					if record.AuthID != fixture.authIDs["paid"] || record.Failed {
						t.Fatalf("local policy rejection was published as an upstream usage record")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("no usage record for the paid application")
				}
			})
		}
	}
}

type enterpriseHydrogenUsageCapture struct {
	first, paid string
	records     chan coreusage.Record
}

func (c enterpriseHydrogenUsageCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	if record.AuthID == c.first || record.AuthID == c.paid {
		c.records <- record
	}
}
