package geminienterprise

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestSubscriptionResolutionPagesRegionsAndFailures(t *testing.T) {
	for _, region := range []string{"global", "us", "eu"} {
		for _, scenario := range []string{"plus", "business", "paid", "unassigned", "expired", "conflict", "permission", "loop", "foreign", "unknown-tier"} {
			t.Run(region+"/"+scenario, func(t *testing.T) {
				pages, configs := 0, 0
				client := &http.Client{Transport: identityTransport(func(r *http.Request) (*http.Response, error) {
					body, status := "", http.StatusOK
					switch {
					case r.URL.Host == "www.googleapis.com":
						body = `{"email":"current@example.test","verified_email":true}`
					case r.URL.Host == "cloudresourcemanager.googleapis.com":
						t.Fatal("numeric projects must not require Resource Manager")
					case strings.HasSuffix(r.URL.Path, "/userLicenses"):
						if r.URL.Host != strings.TrimPrefix(OriginForLocation(region), "https://") || r.URL.Query().Get("filter") != "" {
							t.Fatal("wrong regional host or unsupported user filter")
						}
						pages++
						body = `{"userLicenses":[{"userPrincipal":"other@example.test","licenseAssignmentState":"ASSIGNED","licenseConfig":"projects/123/locations/` + region + `/licenseConfigs/other"}],"nextPageToken":"second"}`
						if scenario == "permission" {
							status = 403
						}
						if pages > 1 {
							body = `{"userLicenses":[{"userPrincipal":"current@example.test","licenseAssignmentState":"ASSIGNED","licenseConfig":"projects/123/locations/` + region + `/licenseConfigs/own"}]}`
							if scenario == "unassigned" {
								body = `{"userLicenses":[]}`
							}
							if scenario == "loop" {
								body = `{"nextPageToken":"second"}`
							}
							if scenario == "foreign" {
								body = strings.ReplaceAll(body, "projects/123/", "projects/999/")
							}
							if scenario == "conflict" {
								body = strings.Replace(body, `]}`, `,{"userPrincipal":"current@example.test","licenseAssignmentState":"ASSIGNED","licenseConfig":"projects/123/locations/`+region+`/licenseConfigs/second"}]}`, 1)
							}
						}
					case strings.Contains(r.URL.Path, "/licenseConfigs/"):
						configs++
						tier, state, trial := "SUBSCRIPTION_TIER_SEARCH_AND_ASSISTANT", "ACTIVE", true
						if scenario == "business" || strings.HasSuffix(r.URL.Path, "/second") {
							tier = "SUBSCRIPTION_TIER_AGENTSPACE_BUSINESS"
						}
						if scenario == "paid" {
							trial = false
						}
						if scenario == "expired" {
							state = "EXPIRED"
						}
						if scenario == "unknown-tier" {
							tier = "FUTURE_TIER"
						}
						body = fmt.Sprintf(`{"name":%q,"subscriptionTier":%q,"state":%q,"freeTrial":%v}`, strings.TrimPrefix(r.URL.Path, "/v1/"), tier, state, trial)
					default:
						t.Fatalf("unexpected request %s", r.URL.Path)
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				observation := ResolveSubscription(t.Context(), client, config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", Location: region, EngineID: "app"}, "synthetic", time.Unix(1000, 0))
				want := map[string]string{"unassigned": "unassigned", "expired": "inactive", "conflict": "conflict", "permission": "unknown", "loop": "unknown", "foreign": "unknown", "unknown-tier": "unknown"}[scenario]
				if want == "" {
					want = "ok"
				}
				if observation.Status != want {
					t.Fatalf("observation=%+v want=%s", observation, want)
				}
				if want == "ok" {
					policy := ModelPolicyFor(nil, observation)
					if policy.Allows(ArgonModel) != (scenario != "plus") || policy.Allows("gemini-3.8-flash") != (scenario != "business") {
						t.Fatalf("wrong trial policy: %+v", policy)
					}
				}
				if scenario == "permission" && configs != 0 {
					t.Fatal("permission failure consulted unrelated licenses")
				}
				if scenario != "permission" && pages != 2 {
					t.Fatalf("pagination calls=%d", pages)
				}
			})
		}
	}
}

func TestModelPolicyManualOverridesAndExactWhitelist(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		model    string
		allowed  bool
	}{
		{"unknown-auto", nil, ArgonModel, false},
		{"manual-catalog", map[string]any{"model_policy": "catalog"}, "future-model", true},
		{"manual-argon", map[string]any{"model_policy": "argon_only"}, ArgonModel, true},
		{"manual-other", map[string]any{"model_policy": "argon_only"}, "gemini-3.8-flash", false},
		{"empty-whitelist", map[string]any{"model_policy": "custom", "allowed_models": []string{}}, ArgonModel, false},
		{"missing-whitelist", map[string]any{"model_policy": "custom"}, ArgonModel, true},
		{"null-whitelist", map[string]any{"model_policy": "custom", "allowed_models": nil}, ArgonModel, true},
		{"typed-null-whitelist", map[string]any{"model_policy": "custom", "allowed_models": []string(nil)}, ArgonModel, true},
		{"exact-whitelist", map[string]any{"model_policy": "custom", "allowed_models": []string{ArgonModel}}, ArgonModel + "-future", false},
		{"exclusion", map[string]any{"model_policy": "argon_only", "excluded_models": []string{"gemini-4-*"}}, ArgonModel, false},
		{"unknown-auto-id", map[string]any{"model_policy": "exclude_argon"}, "future-auto", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ModelPolicyFor(tc.metadata, SubscriptionObservation{})
			if got := p.Allows(tc.model); got != tc.allowed {
				t.Fatalf("policy=%+v allowed=%v", p, got)
			}
		})
	}
	for _, raw := range []any{[]string{"gemini-*"}, []string{" "}, []any{1}, "gemini-4-argon", []string{"models/gemini-4-argon"}, []string{"gemini\t4-argon"}, []string{"gemini\u00a04-argon"}, []string{"gemini\x1f4-argon"}} {
		if _, _, err := ModelPolicySettings(map[string]any{"allowed_models": raw}); err == nil {
			t.Fatalf("accepted invalid whitelist %v", raw)
		}
	}
}

func TestSubscriptionAuthoritativeUnknownBinding(t *testing.T) {
	for _, scenario := range []string{"foreign-project", "foreign-license", "unknown-tier", "permission"} {
		t.Run(scenario, func(t *testing.T) {
			client := &http.Client{Transport: identityTransport(func(r *http.Request) (*http.Response, error) {
				body, status := "", http.StatusOK
				switch {
				case r.URL.Host == "www.googleapis.com":
					body = `{"email":"current@example.test","verified_email":true}`
				case r.URL.Host == "cloudresourcemanager.googleapis.com":
					body = `{"name":"projects/123","projectId":"legacy-project"}`
					if scenario == "foreign-project" {
						body = strings.ReplaceAll(body, "legacy-project", "foreign-project")
					}
				case strings.HasSuffix(r.URL.Path, "/userLicenses"):
					body = `{"userLicenses":[{"userPrincipal":"current@example.test","licenseAssignmentState":"ASSIGNED","licenseConfig":"projects/123/locations/global/licenseConfigs/own"}]}`
				case strings.HasSuffix(r.URL.Path, "/licenseConfigs/own"):
					body = `{"name":"projects/123/locations/global/licenseConfigs/own","subscriptionTier":"SEARCH_AND_ASSISTANT","state":"ACTIVE"}`
					switch scenario {
					case "foreign-license":
						body = strings.ReplaceAll(body, "/own", "/other")
					case "unknown-tier":
						body = strings.ReplaceAll(body, "SEARCH_AND_ASSISTANT", "FUTURE_TIER")
					case "permission":
						status = http.StatusForbidden
					}
				default:
					t.Fatalf("unexpected request %s", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			binding := config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "legacy-project", EngineID: "app"}
			observation := ResolveSubscription(t.Context(), client, binding, "synthetic", time.Unix(1000, 0))
			if observation.Status != "unknown" || observation.Authoritative() != (scenario != "permission") {
				t.Fatalf("unknown subscription trust=%+v", observation)
			}
			identity := ResolveQuotaIdentity(t.Context(), client, binding, "synthetic", time.Unix(1000, 0))
			if identity.Verified || identity.Invalidated != (scenario != "permission") {
				t.Fatalf("unknown subscription quota identity=%+v", identity)
			}
		})
	}
}
