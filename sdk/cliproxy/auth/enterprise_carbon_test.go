package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func carbonVerifiedFixture(t *testing.T, m *Manager, auth *Auth, enabled bool) *Auth {
	t.Helper()
	auth = auth.Clone()
	if ge.String(auth.Metadata, "email") == "" {
		auth.Metadata["email"] = "carbon-fixture@example.test"
	}
	var err error
	auth, err = m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	publishSubscription(t, m, auth, "enterprise_plus", false)
	m.PublishEnterprisePolicyCatalog(auth, []ge.Model{{ID: ge.ArgonModel}, {ID: ge.CarbonModel}, {ID: ge.HydrogenModel}})
	fingerprint := m.EnterpriseHydrogenEvidenceFingerprint(auth)
	auth.Metadata[ge.HydrogenEnabledKey] = enabled
	auth.Metadata[ge.HydrogenEvidenceKey] = ge.CarbonTestEvidence{
		CarbonTestSummary: ge.CarbonTestSummary{TestID: "verified-" + auth.ID, Status: "verified", CheckedAt: m.enterprisePolicies.now(), UpstreamModel: ge.HydrogenModel, HTTPStatus: 200},
		Version:           1, AuthorizationFingerprint: fingerprint.Authorization, BindingFingerprint: fingerprint.Binding, ConfigFingerprint: fingerprint.Configuration,
	}
	auth, err = m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestEnterpriseHydrogenUsesPaidCatalogRegardlessOfLegacySwitch(t *testing.T) {
	m, auth, now := policyFixture(t)
	publishSubscription(t, m, auth, "enterprise_plus", false)
	if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.HydrogenModel); err == nil {
		t.Fatal("Hydrogen was absent from the successful catalog")
	}
	m.PublishEnterprisePolicyCatalog(auth, []ge.Model{{ID: ge.ArgonModel}, {ID: ge.CarbonModel}, {ID: ge.HydrogenModel}})
	for _, legacy := range []any{nil, false, true} {
		next := auth.Clone()
		if legacy == nil {
			delete(next.Metadata, ge.HydrogenEnabledKey)
		} else {
			next.Metadata[ge.HydrogenEnabledKey] = legacy
		}
		var err error
		auth, err = m.Update(t.Context(), next)
		if err != nil {
			t.Fatal(err)
		}
		if view := m.EnterpriseModelPolicyView(auth); !slices.Contains(view.EffectiveModels, ge.HydrogenModel) {
			t.Fatalf("legacy flag %v changed the model list: %+v", legacy, view)
		}
		if err = ValidateEnterpriseModelRequest(t.Context(), auth, ge.HydrogenModel+"(high)"); err != nil {
			t.Fatalf("legacy flag %v blocked the listed model: %v", legacy, err)
		}
	}
	*now = now.Add(6 * time.Minute)
	if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.HydrogenModel); err != nil {
		t.Fatalf("bounded stale paid subscription was rejected: %v", err)
	}
	*now = now.Add(25 * time.Minute)
	if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.HydrogenModel); err == nil {
		t.Fatal("expired subscription cache admitted Hydrogen")
	}
	publishSubscription(t, m, auth, "enterprise_plus", false)
	m.PublishEnterprisePolicyCatalog(auth, []ge.Model{})
	if view := m.EnterpriseModelPolicyView(auth); !view.CatalogKnown || len(view.EffectiveModels) != 0 {
		t.Fatalf("successful empty catalog was supplemented: %+v", view)
	}
}

func TestEnterpriseCarbonProbeCannotBypassVerifiedSubscriptionUserPolicyOrBinding(t *testing.T) {
	for _, scenario := range []string{"unverified", "disabled", "whitelist", "excluded", "binding", "authorization", "trial", "missing_catalog"} {
		t.Run(scenario, func(t *testing.T) {
			m, auth, _ := policyFixture(t)
			auth = carbonVerifiedFixture(t, m, auth, false)
			expected := ge.Resources(ge.FromMetadata(auth.Metadata))
			next := auth.Clone()
			switch scenario {
			case "unverified":
				m.InvalidateEnterpriseSubscription(auth)
				next.Metadata["refresh_token"] = "different-authorization"
			case "disabled":
				next.Disabled = true
			case "whitelist":
				next.Metadata["model_policy"] = "custom"
				next.Metadata["allowed_models"] = []string{ge.ArgonModel}
			case "excluded":
				next.Metadata["excluded_models"] = []string{"gemini-4-h*"}
			case "binding":
				expected.EngineID = "another-app"
			case "authorization":
				delete(next.Metadata, "refresh_token")
			case "trial":
				publishSubscription(t, m, auth, "enterprise_plus", true)
			case "missing_catalog":
				m.PublishEnterprisePolicyCatalog(auth, []ge.Model{{ID: ge.ArgonModel}})
			}
			var err error
			auth, err = m.Update(t.Context(), next)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithEnterpriseHydrogenModelTest(t.Context(), auth.ID, expected)
			if err = ValidateEnterpriseModelRequest(ctx, auth, ge.HydrogenModel); err == nil {
				t.Fatal("management test bypassed " + scenario)
			}
		})
	}
}

func TestEnterpriseHydrogenDowngradeAppliesToOldRegistrationsAndDiagnostics(t *testing.T) {
	m, _, target, _ := carbonEvidenceFixture(t, false)
	seedLegacyHydrogenSwitch(t, m, target, 1, true)
	target, _ = m.GetByID(target.ID)
	registry.GetGlobalRegistry().RegisterClient(target.ID, ge.Provider, []*registry.ModelInfo{{ID: ge.HydrogenModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(target.ID) })
	if err := ValidateEnterpriseModelRequest(t.Context(), target, ge.HydrogenModel); err != nil {
		t.Fatal(err)
	}
	publishSubscription(t, m, target, "enterprise_plus", true)
	ctx := WithEnterpriseHydrogenModelTest(t.Context(), target.ID, ge.Resources(ge.FromMetadata(target.Metadata)))
	if err := ValidateEnterpriseModelRequest(ctx, target, ge.HydrogenModel); err == nil {
		t.Fatal("legacy diagnostics bypassed the trial restriction")
	}
	if err := CommitEnterpriseHydrogenSubmission(t.Context(), target, ge.HydrogenModel); err == nil {
		t.Fatal("a previously selected application submitted after downgrading")
	}
	state := m.EnterpriseModelPolicyView(target)
	if slices.Contains(state.EffectiveModels, ge.HydrogenModel) || !slices.Contains(state.EnforcedExcludedModels, ge.HydrogenModel) {
		t.Fatalf("trial model list differs from the execution gate: %+v", state)
	}
}

func TestEnterpriseHydrogenLegacyDiagnosticsUseAnEligibleCatalogWithoutPublicRegistration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			m, auth, _ := policyFixture(t)
			auth = carbonVerifiedFixture(t, m, auth, false)
			calls := 0
			run := func(_ context.Context, a *Auth, r exec.Request) error {
				calls++
				if a.ID != auth.ID || r.Model != ge.HydrogenModel {
					t.Errorf("management test changed target")
				}
				return nil
			}
			m.RegisterExecutor(&customStreamMockExecutor{
				identifier: ge.Provider,
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, a *Auth, r exec.Request, _ exec.Options) (exec.Response, error) {
					return exec.Response{Payload: []byte(`{"text":"OK"}`)}, run(ctx, a, r)
				}},
				streamFn: func(ctx context.Context, a *Auth, r exec.Request, _ exec.Options) (*exec.StreamResult, error) {
					if err := run(ctx, a, r); err != nil {
						return nil, err
					}
					chunks := make(chan exec.StreamChunk, 1)
					chunks <- exec.StreamChunk{Payload: []byte(`{"text":"OK"}`)}
					close(chunks)
					return &exec.StreamResult{Chunks: chunks}, nil
				},
			})
			opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content", exec.PinnedAuthMetadataKey: auth.ID}}
			req := exec.Request{Model: ge.HydrogenModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}
			invoke := func(ctx context.Context) error {
				if stream {
					result, err := m.ExecuteStream(ctx, []string{ge.Provider}, req, opts)
					if err != nil {
						return err
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}
				_, err := m.Execute(ctx, []string{ge.Provider}, req, opts)
				return err
			}
			if err := invoke(t.Context()); err == nil || calls != 0 {
				t.Fatalf("a pinned ordinary request bypassed registration: %v", err)
			}
			ordinaryProbe := WithEnterpriseModelProbe(t.Context(), auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)), ge.HydrogenModel)
			if err := invoke(ordinaryProbe); err == nil || calls != 0 {
				t.Fatalf("a generic probe bypassed registration: %v", err)
			}
			dedicated := WithEnterpriseHydrogenModelTest(t.Context(), auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)))
			if err := invoke(dedicated); err != nil || calls != 1 {
				t.Fatalf("eligible legacy Hydrogen test failed: calls=%d err=%v", calls, err)
			}
			if len(registry.GetGlobalRegistry().GetModelsForClient(auth.ID)) != 0 {
				t.Fatal("the probe registered a public Hydrogen model")
			}
		})
	}
}

func TestEnterpriseHydrogenRechecksLivePolicyBeforeSubmission(t *testing.T) {
	m, auth, _ := policyFixture(t)
	auth = carbonVerifiedFixture(t, m, auth, false)
	selected := auth.Clone()
	next := auth.Clone()
	next.Metadata[ge.HydrogenEnabledKey] = true
	current, err := m.Update(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err = CommitEnterpriseHydrogenSubmission(t.Context(), selected, ge.HydrogenModel); err != nil {
		t.Fatalf("legacy switch changed submission eligibility: %v", err)
	}
	next = current.Clone()
	next.Metadata["model_policy"] = "exclude_argon"
	next.Metadata[ge.ArgonForceEnabledKey] = true
	if _, err = m.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if err = CommitEnterpriseHydrogenSubmission(t.Context(), selected, ge.HydrogenModel); err == nil {
		t.Fatal("an in-flight choice bypassed the current policy")
	}
	if err = CommitEnterpriseModelSubmission(t.Context(), selected, ge.ArgonModel); err != nil {
		t.Fatalf("legacy Argon force was lost: %v", err)
	}
}

func TestEnterpriseCarbonAliasAndPrefixRouteChecksUseActualModel(t *testing.T) {
	m, auth, _ := policyFixture(t)
	auth = carbonVerifiedFixture(t, m, auth, true)
	auth.Prefix = "team"
	var err error
	auth, err = m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	m.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{ge.Provider: {{Name: ge.HydrogenModel, Alias: "carbon-friendly"}}})
	for _, model := range []string{ge.HydrogenModel, "carbon-friendly", "carbon-friendly(high)", "team/carbon-friendly(8192)", "team/" + ge.HydrogenModel} {
		if !m.EnterpriseHydrogenRoute(model) {
			t.Errorf("Carbon route not recognized: %s", model)
		}
	}
	for _, model := range []string{ge.ArgonModel, "gemini-3.8-flash", "other/carbon-friendly"} {
		if m.EnterpriseHydrogenRoute(model) {
			t.Errorf("unrelated route captured: %s", model)
		}
	}
}

func registerCarbonSelectionFixture(t *testing.T, m *Manager, id, mode string, priority int, enabled bool) *Auth {
	t.Helper()
	auth := registerEnterpriseModelTestAuth(t, m, id, "global", "engine-"+id, ge.HydrogenModel)
	auth.Metadata["model_policy"] = mode
	auth.Attributes["priority"] = fmt.Sprint(priority)
	return carbonVerifiedFixture(t, m, auth, enabled)
}

func TestEnterpriseHydrogenSelectorsIgnoreIneligibleHigherPriorityApplications(t *testing.T) {
	for _, selector := range []Selector{&RoundRobinSelector{}, &FillFirstSelector{}, &WeightedRoundRobinSelector{}} {
		t.Run(fmt.Sprintf("%T", selector), func(t *testing.T) {
			m := NewManager(nil, selector, nil)
			registerCarbonSelectionFixture(t, m, "carbon-priority-closed", "exclude_argon", 100, true)
			unassigned := registerCarbonSelectionFixture(t, m, "carbon-priority-unassigned", "catalog", 100, true)
			m.InvalidateEnterpriseSubscription(unassigned)
			flight := m.BeginEnterpriseSubscriptionRefresh(unassigned)
			if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unassigned", Reason: "license_unassigned"}) {
				t.Fatal("unassigned subscription was not published")
			}
			whitelist := registerCarbonSelectionFixture(t, m, "carbon-priority-whitelist", "custom", 100, true)
			whitelist.Metadata["allowed_models"] = []string{ge.ArgonModel}
			if _, err := m.Update(t.Context(), whitelist); err != nil {
				t.Fatal(err)
			}
			excluded := registerCarbonSelectionFixture(t, m, "carbon-priority-excluded", "catalog", 100, true)
			excluded.Metadata["excluded_models"] = []string{ge.HydrogenModel}
			if _, err := m.Update(t.Context(), excluded); err != nil {
				t.Fatal(err)
			}
			trial := registerCarbonSelectionFixture(t, m, "carbon-priority-trial", "catalog", 100, true)
			publishSubscription(t, m, trial, "enterprise_plus", true)
			registerCarbonSelectionFixture(t, m, "carbon-priority-a", "argon_only", 1, false)
			registerCarbonSelectionFixture(t, m, "carbon-priority-b", "catalog", 1, true)
			var seen []string
			m.RegisterExecutor(&customStreamMockExecutor{identifier: ge.Provider, mockCustomErrorExecutor: mockCustomErrorExecutor{
				executeFn: func(_ context.Context, auth *Auth, req exec.Request, _ exec.Options) (exec.Response, error) {
					if req.Model != ge.HydrogenModel {
						t.Errorf("changed selected model to %s", req.Model)
					}
					seen = append(seen, auth.ID)
					return exec.Response{Payload: []byte(`{"text":"OK"}`)}, nil
				},
			}})
			for range 4 {
				_, err := m.Execute(t.Context(), []string{ge.Provider}, exec.Request{Model: ge.HydrogenModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}, exec.Options{SourceFormat: sdktranslator.FormatGemini, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range seen {
				if id != "carbon-priority-a" && id != "carbon-priority-b" {
					t.Fatalf("Carbon selected an ineligible higher priority application: %v", seen)
				}
			}
			if _, ok := selector.(*RoundRobinSelector); ok && seen[0] == seen[1] {
				t.Fatalf("round robin did not rotate eligible independent Argon templates: %v", seen)
			}
		})
	}
}

func TestEnterpriseCarbonRetryAndPinnedSelectionCannotBypassAdmission(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/pinned=%v", stream, pinned), func(t *testing.T) {
				m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
				m.SetRetryConfig(3, 0, 3)
				first := registerCarbonSelectionFixture(t, m, "carbon-retry-a", "argon_only", 1, true)
				registerCarbonSelectionFixture(t, m, "carbon-retry-b", "exclude_argon", 100, true)
				last := registerCarbonSelectionFixture(t, m, "carbon-retry-c", "catalog", 1, true)
				var seen []string
				run := func(ctx context.Context, auth *Auth) error {
					exec.MarkUpstreamAttempt(ctx)
					seen = append(seen, auth.ID)
					if auth.ID == first.ID {
						return enterpriseModelTestRejection(ge.HydrogenModel, false)
					}
					return nil
				}
				m.RegisterExecutor(&customStreamMockExecutor{
					identifier: ge.Provider,
					mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, auth *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
						return exec.Response{Payload: []byte(`{"text":"OK"}`)}, run(ctx, auth)
					}},
					streamFn: func(ctx context.Context, auth *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
						if err := run(ctx, auth); err != nil {
							return nil, err
						}
						chunks := make(chan exec.StreamChunk, 1)
						chunks <- exec.StreamChunk{Payload: []byte(`{"text":"OK"}`)}
						close(chunks)
						return &exec.StreamResult{Chunks: chunks}, nil
					},
				})
				opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}}
				if pinned {
					opts.Metadata[exec.PinnedAuthMetadataKey] = first.ID
				}
				req := exec.Request{Model: ge.HydrogenModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}
				var err error
				if stream {
					var result *exec.StreamResult
					result, err = m.ExecuteStream(t.Context(), []string{ge.Provider}, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = m.Execute(t.Context(), []string{ge.Provider}, req, opts)
				}
				if pinned {
					if err == nil || !slices.Equal(seen, []string{first.ID}) {
						t.Fatalf("pinned Carbon request switched applications: calls=%v err=%v", seen, err)
					}
				} else if err != nil || !slices.Equal(seen, []string{first.ID, last.ID}) {
					t.Fatalf("retry selected a closed application or failed: calls=%v err=%v", seen, err)
				}
			})
		}
	}
}

func TestEnterpriseCarbonClosedManagementTestRetainsQuotaRestrictions(t *testing.T) {
	m, auth, now := policyFixture(t)
	m.enterpriseQuota.nowFunc = func() time.Time { return *now }
	auth = carbonVerifiedFixture(t, m, auth, false)
	rejectSharedQuota(t, auth, *now, "credential")
	calls := 0
	m.RegisterExecutor(&customStreamMockExecutor{identifier: ge.Provider, mockCustomErrorExecutor: mockCustomErrorExecutor{
		executeFn: func(context.Context, *Auth, exec.Request, exec.Options) (exec.Response, error) {
			calls++
			return exec.Response{}, nil
		},
	}})
	ctx := WithEnterpriseHydrogenModelTest(t.Context(), auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)))
	_, err := m.Execute(ctx, []string{ge.Provider}, exec.Request{Model: ge.HydrogenModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}, exec.Options{SourceFormat: sdktranslator.FormatGemini, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content", exec.PinnedAuthMetadataKey: auth.ID}})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 429 || calls != 0 {
		t.Fatalf("fixed management test bypassed shared quota: calls=%d err=%v", calls, err)
	}
}
