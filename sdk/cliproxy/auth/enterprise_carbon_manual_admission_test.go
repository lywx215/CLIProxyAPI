package auth

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCarbonManualAdmissionDoesNotRequireDiagnosticEvidence(t *testing.T) {
	for _, trial := range []bool{false, true} {
		for _, status := range []string{"missing", "verified", "failed", "incomplete", "needs_review", "malformed"} {
			t.Run(map[bool]string{false: "paid/", true: "trial/"}[trial]+status, func(t *testing.T) {
				m, _, target, _ := carbonEvidenceFixture(t, false)
				publishSubscription(t, m, target, "enterprise_plus", trial)
				if status == "malformed" {
					copy := target.Clone()
					copy.Metadata[ge.HydrogenEvidenceKey] = map[string]any{"invalid_historical_record": true}
					var err error
					target, err = m.Update(t.Context(), copy)
					if err != nil {
						t.Fatal(err)
					}
				} else if status != "missing" {
					pending, err := m.BeginEnterpriseHydrogenTest(target, 1)
					if err != nil {
						t.Fatal(err)
					}
					evidence := &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: status, UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}}
					if status != "verified" {
						evidence.HTTPStatus, evidence.Reason = 400, "model_not_enabled"
					}
					if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, evidence); err != nil {
						t.Fatal(err)
					}
				}
				current, _ := m.GetByID(target.ID)
				defaultEnabled := false
				if view := m.EnterpriseHydrogenAdmissionView(current); !view.CanEnable || view.Enabled != defaultEnabled || view.Available != defaultEnabled || view.VerificationRequired {
					t.Fatalf("diagnostic state became an enablement prerequisite: %+v", view)
				}
				if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 1, true, ""); err != nil {
					t.Fatal(err)
				}
				current, _ = m.GetByID(target.ID)
				assertAvailable := func() {
					t.Helper()
					view := m.EnterpriseHydrogenAdmissionView(current)
					if !view.Enabled || !view.Available || !view.CanEnable || view.VerificationRequired || view.Reason != "" {
						t.Fatalf("manual admission depended on diagnostics: %+v", view)
					}
					if err := ValidateEnterpriseModelRequest(t.Context(), current, ge.HydrogenModel+"(high)"); err != nil {
						t.Fatal(err)
					}
					policy := m.EnterpriseModelPolicyView(current)
					if !slices.Contains(policy.EffectiveModels, ge.HydrogenModel) {
						t.Fatal("manual Carbon was absent from the effective model list")
					}
				}
				assertAvailable()
				m.SetConfig(&config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: ge.HydrogenModel, Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.thinkingConfig.includeThoughts": true}}}}})
				assertAvailable()
				if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 2, false, ""); err != nil {
					t.Fatal(err)
				}
				current, _ = m.GetByID(target.ID)
				if m.EnterpriseHydrogenAdmissionView(current).Available {
					t.Fatal("manual close was ignored")
				}
				if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 3, true, "legacy-ignored-test-id"); err != nil {
					t.Fatal(err)
				}
				current, _ = m.GetByID(target.ID)
				assertAvailable()
			})
		}
	}
}

func TestCarbonManualBusinessWithoutModelTestsRetainsSharedQuota(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			m, _, target, _ := carbonEvidenceFixture(t, false)
			now := time.Date(2026, time.November, 7, 10, 0, 0, 0, time.UTC)
			m.enterpriseQuota.nowFunc = func() time.Time { return now }
			if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), target, 1, true, ""); err != nil {
				t.Fatal(err)
			}
			current, _ := m.GetByID(target.ID)
			registry.GetGlobalRegistry().RegisterClient(current.ID, ge.Provider, []*registry.ModelInfo{{ID: ge.HydrogenModel, Type: "gemini"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(current.ID) })
			rejectSharedQuota(t, current, now, "credential")
			calls := 0
			m.RegisterExecutor(&customStreamMockExecutor{identifier: ge.Provider,
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(context.Context, *Auth, exec.Request, exec.Options) (exec.Response, error) {
					calls++
					return exec.Response{}, nil
				}},
				streamFn: func(context.Context, *Auth, exec.Request, exec.Options) (*exec.StreamResult, error) {
					calls++
					return nil, nil
				},
			})
			req := exec.Request{Model: ge.HydrogenModel, Payload: []byte(`{"contents":[{"parts":[{"text":"synthetic"}]}]}`)}
			opts := exec.Options{SourceFormat: translator.FormatGemini, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content", exec.PinnedAuthMetadataKey: current.ID}}
			var err error
			if stream {
				_, err = m.ExecuteStream(t.Context(), []string{ge.Provider}, req, opts)
			} else {
				_, err = m.Execute(t.Context(), []string{ge.Provider}, req, opts)
			}
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != 429 || calls != 0 {
				t.Fatalf("manual Carbon bypassed shared quota: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestCarbonManualAdmissionUsesSubscriptionEligibilityIndependentlyFromModelTests(t *testing.T) {
	m, _, target, _ := carbonEvidenceFixture(t, false)
	if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), target, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetByID(target.ID)
	m.InvalidateEnterpriseSubscription(current)
	flight := m.BeginEnterpriseSubscriptionRefresh(current)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unassigned", Edition: "enterprise_plus"}) {
		t.Fatal("could not publish the current unassigned subscription")
	}
	if view := m.EnterpriseHydrogenAdmissionView(current); !view.Enabled || view.Available || view.CanEnable || view.Reason != "subscription_unverified" {
		t.Fatalf("manual opening bypassed unknown subscription: %+v", view)
	}
	publishSubscription(t, m, current, "enterprise_plus", false)
	if view := m.EnterpriseHydrogenAdmissionView(current); !view.Available || view.Test != nil {
		t.Fatalf("confirmed subscription required a model test: %+v", view)
	}
}

func TestCarbonManualAdmissionDoesNotRequireARefreshTokenOnlyUsedByDiagnostics(t *testing.T) {
	m, root, target, _ := carbonEvidenceFixture(t, false)
	copy := root.Clone()
	delete(copy.Metadata, "refresh_token")
	_, err := m.Update(t.Context(), copy)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetByID(target.ID)
	publishSubscription(t, m, current, "enterprise_plus", false)
	if _, err = m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(current.ID)
	if view := m.EnterpriseHydrogenAdmissionView(current); !view.Available || !view.CanEnable || view.CanTest {
		t.Fatalf("diagnostic identity requirements leaked into ordinary generation: %+v", view)
	}
}
