package test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnterpriseHydrogenPublicHTTPAdmissionAndAliases(t *testing.T) {
	for _, protocol := range []string{"gemini", "openai"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"paid", "verified_closed", "enabled", "manual_no_test", "manual_failed_test", "manual_config_changed", "trial", "trial_closed", "trial_no_proof", "trial_alias", "trial_prefix", "trial_suffix", "unverified", "unassigned", "inactive", "argon_only", "exclude_argon", "whitelist", "excluded", "alias", "prefix", "suffix", "payload_trial_no_proof", "payload_closed", "payload_paid_success", "payload_trial_success"} {
				t.Run(protocol+"/"+scenario+map[bool]string{true: "/stream", false: "/full"}[stream], func(t *testing.T) {
					cfg := &config.Config{}
					requested := ge.HydrogenModel
					if strings.HasPrefix(scenario, "payload_") {
						requested = ge.ArgonModel
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-*", Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": ge.HydrogenModel}}}
					}
					router, transport, manager := enterpriseFixtureRouterWithConfig(t, cfg)
					auth, _ := manager.GetByID("enterprise-wire-fixture")
					auth.Metadata["model_policy"] = "catalog"
					if scenario == "paid" {
						auth.Metadata[ge.HydrogenEnabledKey] = true
					}
					if scenario == "argon_only" || scenario == "exclude_argon" {
						auth.Metadata["model_policy"] = scenario
					}
					if scenario == "whitelist" {
						auth.Metadata["model_policy"], auth.Metadata["allowed_models"] = "custom", []string{ge.ArgonModel}
					}
					if scenario == "excluded" {
						auth.Metadata["excluded_models"] = []string{ge.HydrogenModel}
					}
					variant := strings.TrimPrefix(scenario, "trial_")
					if variant == "alias" || variant == "prefix" {
						requested = "carbon-public"
						cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{ge.Provider: {{Name: ge.HydrogenModel, Alias: requested}}}
						manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
						if variant == "prefix" {
							auth.Prefix, requested = "work", "work/"+requested
						}
					}
					var err error
					auth, err = manager.Update(t.Context(), auth)
					if err != nil {
						t.Fatal(err)
					}
					trial := strings.HasPrefix(scenario, "trial") || strings.HasPrefix(scenario, "payload_trial")
					observation := ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial}
					if scenario == "unverified" {
						observation.Status, observation.Reason, observation.FreeTrial = "unknown", "unknown_subscription_tier", nil
					}
					if scenario == "unassigned" || scenario == "inactive" {
						observation.Status = scenario
					}
					enterprisePublishSubscription(t, manager, auth, observation)
					if strings.HasPrefix(scenario, "manual_") {
						if scenario != "manual_no_test" {
							auth = enterpriseCarbonProofFixture(t, manager, auth, false)
						}
						auth = auth.Clone()
						auth.Metadata[ge.HydrogenEnabledKey] = true
						if scenario == "manual_failed_test" {
							settings, errRead := ge.HydrogenAdmissionModel.ReadSettings(auth.Metadata)
							if errRead != nil || settings.Evidence == nil {
								t.Fatal("missing synthetic diagnostic record")
							}
							evidence := *settings.Evidence
							evidence.Status, evidence.HTTPStatus, evidence.Reason = "failed", 500, "upstream_error"
							auth.Metadata[ge.HydrogenEvidenceKey] = evidence
						}
						auth, err = manager.Update(t.Context(), auth)
						if err != nil {
							t.Fatal(err)
						}
						if scenario == "manual_config_changed" {
							cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: ge.HydrogenModel, Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.thinkingConfig.includeThoughts": true}}}
							manager.SetConfig(cfg)
							if manager.EnterpriseHydrogenEvidenceState(auth).Valid {
								t.Fatal("the diagnostic fixture did not have a changed configuration")
							}
						}
					} else if scenario != "paid" && scenario != "trial_no_proof" && scenario != "payload_trial_no_proof" {
						enabled := scenario != "verified_closed" && scenario != "trial_closed" && scenario != "payload_closed"
						auth = enterpriseCarbonProofFixture(t, manager, auth, enabled)
					}
					manager.PublishEnterprisePolicyCatalog(auth, []ge.Model{{ID: ge.ArgonModel}, {ID: ge.HydrogenModel}})
					upstream := ge.HydrogenModel
					if strings.HasPrefix(scenario, "payload_") {
						upstream = ge.ArgonModel
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, ge.Provider, []*registry.ModelInfo{{ID: requested, MetadataModelID: upstream, Type: "gemini", SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"}}})
					if variant == "suffix" {
						requested += "(high)"
					}
					path, body := enterprisePublicRequest(t, requested, protocol, stream, nil)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
					allowed := strings.HasPrefix(scenario, "manual_") || scenario == "paid" || scenario == "enabled" || scenario == "trial" || scenario == "argon_only" || scenario == "exclude_argon" || variant == "alias" || variant == "prefix" || variant == "suffix" || scenario == "payload_paid_success" || scenario == "payload_trial_success"
					if allowed {
						if response.Code != http.StatusOK || transport.calls.Load() != 1 || gjson.GetBytes(transport.requestBody(), "generationSpec.modelId").String() != ge.HydrogenModel || !strings.Contains(response.Body.String(), "print(7)") {
							t.Fatalf("Carbon request did not reach its exact upstream model once: status=%d calls=%d body=%s", response.Code, transport.calls.Load(), response.Body.String())
						}
					} else if response.Code < 400 || transport.calls.Load() != 0 {
						t.Fatalf("Carbon policy bypassed: status=%d calls=%d body=%s", response.Code, transport.calls.Load(), response.Body.String())
					}
					if scenario == "payload_trial_no_proof" && response.Code != http.StatusServiceUnavailable {
						t.Fatalf("final trial payload bypassed Carbon admission: %s", response.Body.String())
					}
				})
			}
		}
	}
}

// This synthetic fixture represents a server-observed proof. Public HTTP input
// cannot create it, and subscription/user policy is still checked at dispatch.
func enterpriseCarbonProofFixture(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth, enabled bool) *coreauth.Auth {
	t.Helper()
	auth = auth.Clone()
	fingerprint := manager.EnterpriseHydrogenEvidenceFingerprint(auth)
	auth.Metadata[ge.HydrogenEnabledKey] = enabled
	auth.Metadata[ge.HydrogenEvidenceKey] = ge.CarbonTestEvidence{
		CarbonTestSummary: ge.CarbonTestSummary{TestID: "synthetic-carbon-" + auth.ID, Status: "verified", CheckedAt: time.Now().UTC(), UpstreamModel: ge.HydrogenModel, HTTPStatus: http.StatusOK},
		Version:           1, AuthorizationFingerprint: fingerprint.Authorization, BindingFingerprint: fingerprint.Binding, ConfigFingerprint: fingerprint.Configuration,
	}
	updated, err := manager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
