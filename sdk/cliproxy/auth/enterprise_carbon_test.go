package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
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
	fingerprint := m.EnterpriseCarbonEvidenceFingerprint(auth)
	auth.Metadata[ge.CarbonEnabledKey] = enabled
	auth.Metadata[ge.CarbonEvidenceKey] = ge.CarbonTestEvidence{
		CarbonTestSummary: ge.CarbonTestSummary{TestID: "verified-" + auth.ID, Status: "verified", CheckedAt: m.enterprisePolicies.now(), UpstreamModel: ge.CarbonModel, HTTPStatus: 200},
		Version:           1, AuthorizationFingerprint: fingerprint.Authorization, BindingFingerprint: fingerprint.Binding, ConfigFingerprint: fingerprint.Configuration,
	}
	auth, err = m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestEnterpriseCarbonAdmissionDefaultsPaidAndHonorsManualSwitch(t *testing.T) {
	m, auth, now := policyFixture(t)
	publishSubscription(t, m, auth, "enterprise_plus", false)
	original := m.EnterpriseModelPolicyView(auth)
	if !original.CarbonAdmission.Enabled || !original.CarbonAdmission.Available || !original.CarbonAdmission.CanEnable || original.CarbonAdmission.VerificationRequired || !original.CarbonAdmission.CanTest || !slices.Contains(original.EffectiveModels, ge.CarbonModel) {
		t.Fatalf("default Carbon admission: %+v", original.CarbonAdmission)
	}
	if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.CarbonModel); err != nil {
		t.Fatalf("paid Carbon required manual admission: %v", err)
	}
	auth = carbonVerifiedFixture(t, m, auth, false)
	ctx := WithEnterpriseCarbonModelTest(t.Context(), auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)))
	if err := ValidateEnterpriseModelRequest(ctx, auth, ge.CarbonModel); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.CarbonModel); err == nil {
		t.Fatal("a closed candidate was callable")
	}
	if view := m.EnterpriseCarbonAdmissionView(auth); !view.CanEnable || view.Enabled || view.Available {
		t.Fatalf("a proof enabled business routing: %+v", view)
	}
	for _, mode := range []string{"auto", "catalog", "argon_only", "exclude_argon"} {
		next := auth.Clone()
		next.Metadata["model_policy"] = mode
		next.Metadata[ge.CarbonEnabledKey] = true
		var err error
		auth, err = m.Update(t.Context(), next)
		if err != nil {
			t.Fatal(err)
		}
		view := m.EnterpriseModelPolicyView(auth)
		if !view.CarbonAdmission.Available || !slices.Contains(view.EffectiveModels, ge.CarbonModel) {
			t.Fatalf("Argon template %q controlled Carbon: %+v", mode, view)
		}
		if mode == "argon_only" && !view.Allows(ge.ArgonModel) || mode == "exclude_argon" && view.Allows(ge.ArgonModel) {
			t.Fatal("Carbon changed Argon policy")
		}
		if err = ValidateEnterpriseModelRequest(t.Context(), auth, ge.CarbonModel+"(high)"); err != nil {
			t.Fatal(err)
		}
	}
	key := enterprisePolicyCatalogKey(auth)
	if slices.ContainsFunc(m.enterprisePolicies.catalogs[key], func(model ge.Model) bool { return model.ID == ge.CarbonModel }) {
		t.Fatal("admission overwrote the raw observed catalog")
	}
	m.PublishEnterprisePolicyCatalog(auth, []ge.Model{})
	if view := m.EnterpriseModelPolicyView(auth); !slices.Equal(view.EffectiveModels, []string{ge.CarbonModel}) {
		t.Fatalf("verified Carbon depended on the raw catalog: %+v", view)
	}
	*now = now.Add(31 * time.Minute)
	if view := m.EnterpriseCarbonAdmissionView(auth); view.Available || view.CanEnable || view.CanTest || view.Reason != "subscription_unverified" {
		t.Fatalf("expired entitlement: %+v", view)
	}
}

func TestEnterpriseCarbonProbeCannotBypassVerifiedSubscriptionUserPolicyOrBinding(t *testing.T) {
	for _, scenario := range []string{"unverified", "disabled", "whitelist", "excluded", "binding", "authorization"} {
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
				next.Metadata["excluded_models"] = []string{"gemini-4-c*"}
			case "binding":
				expected.EngineID = "another-app"
			case "authorization":
				delete(next.Metadata, "refresh_token")
			}
			var err error
			auth, err = m.Update(t.Context(), next)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithEnterpriseCarbonModelTest(t.Context(), auth.ID, expected)
			if err = ValidateEnterpriseModelRequest(ctx, auth, ge.CarbonModel); err == nil {
				t.Fatal("management test bypassed " + scenario)
			}
		})
	}
}

func TestEnterpriseCarbonTrialManualApplicationAdmissionAndOptionalDiagnostics(t *testing.T) {
	m, _, target, _ := carbonEvidenceFixture(t, false)
	publishSubscription(t, m, target, "enterprise_plus", true)
	m.PublishEnterprisePolicyCatalog(target, []ge.Model{{ID: ge.ArgonModel}, {ID: ge.CarbonModel}})
	view := m.EnterpriseModelPolicyView(target)
	if !view.CarbonAdmission.CanTest || view.CarbonAdmission.Enabled || view.CarbonAdmission.Available || !view.CarbonAdmission.CanEnable {
		t.Fatalf("trial Carbon was automatically opened or refused testing: %+v", view.CarbonAdmission)
	}
	ctx := WithEnterpriseCarbonModelTest(t.Context(), target.ID, ge.Resources(ge.FromMetadata(target.Metadata)))
	if err := ValidateEnterpriseModelRequest(ctx, target, ge.CarbonModel); err != nil {
		t.Fatalf("verified assigned trial could not use the fixed management test: %v", err)
	}
	pending, err := m.BeginEnterpriseCarbonTest(target, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "incomplete", HTTPStatus: 502, Reason: "model_verification_incomplete"}}); err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetByID(target.ID)
	if view := m.EnterpriseCarbonAdmissionView(current); !view.CanEnable || view.Enabled {
		t.Fatal("an incomplete diagnostic blocked manual admission or opened it automatically")
	}
	updated, _ := recordCarbonVerified(t, m, current)
	current, _ = m.GetByID(target.ID)
	view = m.EnterpriseModelPolicyView(current)
	if !view.CarbonAdmission.CanEnable || view.CarbonAdmission.Enabled || view.CarbonAdmission.Available {
		t.Fatalf("a successful trial test did not remain manually closed: %+v", view.CarbonAdmission)
	}
	if err = ValidateEnterpriseModelRequest(t.Context(), current, ge.CarbonModel); err == nil {
		t.Fatal("a successful trial test bypassed the manual switch")
	}
	revision, _ := carbonCollectionRevision(updated, EnterpriseBindingID(current))
	if _, err = m.UpdateEnterpriseCarbonEnabled(t.Context(), current, revision, true, ""); err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(target.ID)
	view = m.EnterpriseModelPolicyView(current)
	if !view.CarbonAdmission.Available || !view.Allows(ge.CarbonModel) || slices.Contains(view.EnforcedExcludedModels, ge.CarbonModel) {
		t.Fatalf("manually verified trial Carbon remained unavailable: %+v", view)
	}
	if err = ValidateEnterpriseModelRequest(t.Context(), current, ge.CarbonModel+"(high)"); err != nil {
		t.Fatal(err)
	}
	if err = CommitEnterpriseCarbonSubmission(withEnterpriseModelAttempts(t.Context()), current, ge.CarbonModel); err != nil {
		t.Fatal(err)
	}
	for _, model := range view.CatalogModels {
		if model.ID == ge.CarbonModel && (!model.Allowed || model.Reason != "") {
			t.Fatalf("the trial catalog still described a paid-only hard block: %+v", model)
		}
	}
	// Temporary verification failures retain the existing bounded stale-license
	// behavior; this change does not create a second license-refresh path.
	m.InvalidateEnterpriseSubscription(current)
	flight := m.BeginEnterpriseSubscriptionRefresh(current)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable"}) {
		t.Fatal("temporary subscription failure was not published")
	}
	if state := m.EnterpriseModelPolicyView(current); state.Subscription.Status != "stale" || !state.CarbonAdmission.Available {
		t.Fatalf("a bounded stale trial license lost existing cache semantics: %+v", state.CarbonAdmission)
	}
	owner, _, _ := m.EnterpriseAccountSnapshot(current.ID)
	for _, scenario := range []string{"unknown", "unassigned", "inactive", "conflict", "missing_trial_classification"} {
		t.Run(scenario, func(t *testing.T) {
			restarted := NewManager(nil, nil, nil)
			restarted.SetConfig(&config.Config{})
			if _, err := restarted.Register(t.Context(), owner); err != nil {
				t.Fatal(err)
			}
			credential, _ := restarted.GetByID(current.ID)
			trial := true
			observation := ge.SubscriptionObservation{Status: scenario, FreeTrial: &trial, State: "ACTIVE"}
			if scenario == "missing_trial_classification" {
				observation.Status, observation.FreeTrial = "ok", nil
			}
			if scenario == "inactive" {
				observation.State = "SUSPENDED"
			}
			flight := restarted.BeginEnterpriseSubscriptionRefresh(credential)
			if flight == nil || !flight.Complete(observation) {
				t.Fatal("invalid subscription fixture was not published")
			}
			view := restarted.EnterpriseCarbonAdmissionView(credential)
			if !view.Enabled || view.Available || view.CanTest || view.CanEnable || view.Reason != "subscription_unverified" {
				t.Fatalf("%s subscription retained Carbon admission: %+v", scenario, view)
			}
			ctx := WithEnterpriseCarbonModelTest(t.Context(), credential.ID, ge.Resources(ge.FromMetadata(credential.Metadata)))
			if err := ValidateEnterpriseModelRequest(ctx, credential, ge.CarbonModel); err == nil {
				t.Fatal("a fixed test bypassed unavailable trial subscription state")
			}
		})
	}
}

func TestEnterpriseCarbonClosedManagementCandidateUsesNormalExecutionWithoutRegistration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			m, auth, _ := policyFixture(t)
			auth = carbonVerifiedFixture(t, m, auth, false)
			calls := 0
			run := func(_ context.Context, a *Auth, r exec.Request) error {
				calls++
				if a.ID != auth.ID || r.Model != ge.CarbonModel {
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
			req := exec.Request{Model: ge.CarbonModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}
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
				t.Fatalf("a pinned ordinary request bypassed the switch: %v", err)
			}
			ordinaryProbe := WithEnterpriseModelProbe(t.Context(), auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)), ge.CarbonModel)
			if err := invoke(ordinaryProbe); err == nil || calls != 0 {
				t.Fatalf("a generic probe bypassed the switch: %v", err)
			}
			dedicated := WithEnterpriseCarbonModelTest(t.Context(), auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)))
			if err := invoke(dedicated); err != nil || calls != 1 {
				t.Fatalf("fixed closed Carbon test failed: calls=%d err=%v", calls, err)
			}
			if len(registry.GetGlobalRegistry().GetModelsForClient(auth.ID)) != 0 {
				t.Fatal("the probe registered a public Carbon model")
			}
			if m.EnterpriseCarbonAdmissionView(auth).Enabled {
				t.Fatal("a test enabled its application")
			}
		})
	}
}

func TestEnterpriseCarbonSubmissionOrdersCloseAndReopenWithoutResettingRequests(t *testing.T) {
	m, auth, _ := policyFixture(t)
	auth = carbonVerifiedFixture(t, m, auth, true)
	old := auth.Clone()
	ctx := withEnterpriseModelAttempts(t.Context())
	committed, complete := make(chan error, 1), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := CommitEnterpriseCarbonSubmission(ctx, old, ge.CarbonModel)
		committed <- err
		if err == nil {
			<-complete
		}
	}()
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	next := auth.Clone()
	next.Metadata[ge.CarbonEnabledKey] = false
	var err error
	auth, err = m.Update(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err = CommitEnterpriseCarbonSubmission(withEnterpriseModelAttempts(t.Context()), old, ge.CarbonModel); err == nil {
		t.Fatal("close preceded a successful new commit")
	}
	next = auth.Clone()
	next.Metadata[ge.CarbonEnabledKey] = true
	auth, err = m.Update(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if auth.RegistrationEpoch != old.RegistrationEpoch || auth.enterpriseCarbonWindow == old.enterpriseCarbonWindow {
		t.Fatal("a Carbon close/open cycle changed Argon registration or reused an old admission window")
	}
	if err = CommitEnterpriseCarbonSubmission(withEnterpriseModelAttempts(t.Context()), old, ge.CarbonModel); err == nil {
		t.Fatal("a pre-close snapshot submitted after a rapid reopen")
	}
	if err = CommitEnterpriseCarbonSubmission(withEnterpriseModelAttempts(t.Context()), auth, ge.CarbonModel); err != nil {
		t.Fatal(err)
	}
	current := auth.Clone()
	current.Metadata["note"] = "An unrelated edit must retain the Carbon admission window"
	if _, err = m.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err = CommitEnterpriseCarbonSubmission(withEnterpriseModelAttempts(t.Context()), auth, ge.CarbonModel); err != nil {
		t.Fatalf("an unrelated edit invalidated the Carbon window: %v", err)
	}
	close(complete)
	wg.Wait()
	// An old proof remains available after an off/on cycle; no account or quota counters are reset.
	if !m.EnterpriseCarbonAdmissionView(auth).Available {
		t.Fatal("reopening discarded valid evidence")
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
	m.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{ge.Provider: {{Name: ge.CarbonModel, Alias: "carbon-friendly"}}})
	for _, model := range []string{ge.CarbonModel, "carbon-friendly", "carbon-friendly(high)", "team/carbon-friendly(8192)", "team/" + ge.CarbonModel} {
		if !m.EnterpriseCarbonRoute(model) {
			t.Errorf("Carbon route not recognized: %s", model)
		}
	}
	for _, model := range []string{ge.ArgonModel, "gemini-3.8-flash", "other/carbon-friendly"} {
		if m.EnterpriseCarbonRoute(model) {
			t.Errorf("unrelated route captured: %s", model)
		}
	}
}

func registerCarbonSelectionFixture(t *testing.T, m *Manager, id, mode string, priority int, enabled bool) *Auth {
	t.Helper()
	auth := registerEnterpriseModelTestAuth(t, m, id, "global", "engine-"+id, ge.CarbonModel)
	auth.Metadata["model_policy"] = mode
	auth.Attributes["priority"] = fmt.Sprint(priority)
	return carbonVerifiedFixture(t, m, auth, enabled)
}

func TestEnterpriseCarbonSelectorsIgnoreClosedHigherPriorityApplications(t *testing.T) {
	for _, selector := range []Selector{&RoundRobinSelector{}, &FillFirstSelector{}, &WeightedRoundRobinSelector{}} {
		t.Run(fmt.Sprintf("%T", selector), func(t *testing.T) {
			m := NewManager(nil, selector, nil)
			registerCarbonSelectionFixture(t, m, "carbon-priority-closed", "argon_only", 100, false)
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
			excluded.Metadata["excluded_models"] = []string{ge.CarbonModel}
			if _, err := m.Update(t.Context(), excluded); err != nil {
				t.Fatal(err)
			}
			trial := registerCarbonSelectionFixture(t, m, "carbon-priority-a", "argon_only", 1, true)
			publishSubscription(t, m, trial, "enterprise_plus", true)
			registerCarbonSelectionFixture(t, m, "carbon-priority-b", "exclude_argon", 1, true)
			var seen []string
			m.RegisterExecutor(&customStreamMockExecutor{identifier: ge.Provider, mockCustomErrorExecutor: mockCustomErrorExecutor{
				executeFn: func(_ context.Context, auth *Auth, req exec.Request, _ exec.Options) (exec.Response, error) {
					if req.Model != ge.CarbonModel {
						t.Errorf("changed selected model to %s", req.Model)
					}
					seen = append(seen, auth.ID)
					return exec.Response{Payload: []byte(`{"text":"OK"}`)}, nil
				},
			}})
			for range 4 {
				_, err := m.Execute(t.Context(), []string{ge.Provider}, exec.Request{Model: ge.CarbonModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}, exec.Options{SourceFormat: sdktranslator.FormatGemini, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}})
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
				registerCarbonSelectionFixture(t, m, "carbon-retry-b", "catalog", 100, false)
				last := registerCarbonSelectionFixture(t, m, "carbon-retry-c", "exclude_argon", 1, true)
				var seen []string
				run := func(ctx context.Context, auth *Auth) error {
					exec.MarkUpstreamAttempt(ctx)
					seen = append(seen, auth.ID)
					if auth.ID == first.ID {
						return enterpriseModelTestRejection(ge.CarbonModel, false)
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
				req := exec.Request{Model: ge.CarbonModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}
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
	ctx := WithEnterpriseCarbonModelTest(t.Context(), auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)))
	_, err := m.Execute(ctx, []string{ge.Provider}, exec.Request{Model: ge.CarbonModel, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}, exec.Options{SourceFormat: sdktranslator.FormatGemini, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content", exec.PinnedAuthMetadataKey: auth.ID}})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 429 || calls != 0 {
		t.Fatalf("fixed management test bypassed shared quota: calls=%d err=%v", calls, err)
	}
}
