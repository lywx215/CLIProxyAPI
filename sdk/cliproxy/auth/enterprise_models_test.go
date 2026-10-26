package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type enterpriseModelTestSelector struct{}

func (*enterpriseModelTestSelector) Pick(_ context.Context, _, _ string, _ exec.Options, auths []*Auth) (*Auth, error) {
	sorted := append([]*Auth(nil), auths...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	if len(sorted) == 0 {
		return nil, errors.New("no test candidates")
	}
	return sorted[0], nil
}

func registerEnterpriseModelTestAuth(t *testing.T, m *Manager, id, region, engine, model string) *Auth {
	t.Helper()
	a := &Auth{
		ID: id, Provider: ge.Provider, Status: StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata: map[string]any{
			"model_policy": "catalog", "client_id": "fixture-client", "project_id": "12345", "location": region,
			"engine_id": engine, "refresh_token": "synthetic-" + id,
		},
	}
	saved, err := m.Register(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: model, Type: "gemini"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	return saved
}

func enterpriseModelTestRejection(model string, started bool) error {
	return &ge.GenerationError{HTTPStatus: http.StatusBadRequest, Started: started, ModelNotEnabled: true, Model: model}
}

func TestEnterpriseModelNotEnabledRetryPreservesRegionAndPin(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"same_region", "cross_region_only", "pinned", "ordinary400"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, mode), func(t *testing.T) {
				model := "enterprise-availability-" + strings.NewReplacer("/", "-", "=", "-").Replace(t.Name())
				m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
				m.SetRetryConfig(3, 0, 3)
				a := registerEnterpriseModelTestAuth(t, m, "a-"+model, "global", "engine-a", model)
				registerEnterpriseModelTestAuth(t, m, "b-"+model, "us", "engine-us", model)
				if mode != "cross_region_only" {
					registerEnterpriseModelTestAuth(t, m, "c-"+model, "global", "engine-c", model)
				}
				var calls []string
				var mu sync.Mutex
				run := func(ctx context.Context, auth *Auth) error {
					exec.MarkUpstreamAttempt(ctx)
					mu.Lock()
					calls = append(calls, auth.ID)
					mu.Unlock()
					if auth.ID == a.ID {
						if mode == "ordinary400" {
							return &Error{HTTPStatus: 400, Message: `{"error":{"code":"invalid_request","message":"bad field"}}`}
						}
						return enterpriseModelTestRejection(model, false)
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
							ch := make(chan exec.StreamChunk, 1)
							ch <- exec.StreamChunk{Err: err}
							close(ch)
							return &exec.StreamResult{Chunks: ch}, nil
						}
						ch := make(chan exec.StreamChunk, 1)
						ch <- exec.StreamChunk{Payload: []byte(`{"text":"OK"}`)}
						close(ch)
						return &exec.StreamResult{Chunks: ch}, nil
					},
				})
				opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}}
				if mode == "pinned" {
					opts.Metadata[exec.PinnedAuthMetadataKey] = a.ID
				}
				req := exec.Request{Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"OK"}]}]}`)}
				var resultErr error
				if stream {
					var result *exec.StreamResult
					result, resultErr = m.ExecuteStream(t.Context(), []string{ge.Provider}, req, opts)
					if resultErr == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								resultErr = chunk.Err
								break
							}
						}
					}
				} else {
					_, resultErr = m.Execute(t.Context(), []string{ge.Provider}, req, opts)
				}
				mu.Lock()
				defer mu.Unlock()
				if mode == "same_region" {
					if resultErr != nil || len(calls) != 2 || calls[0] != a.ID || calls[1] != "c-"+model {
						t.Fatalf("same-region fallback: calls=%v error=%v", calls, resultErr)
					}
				} else if resultErr == nil || len(calls) != 1 || calls[0] != a.ID {
					t.Fatalf("constraint or ordinary400 was ignored: calls=%v error=%v", calls, resultErr)
				}
			})
		}
	}
}

func TestEnterpriseModelNotEnabledStartedCannotBeReplayed(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			model := "enterprise-started-availability-" + fmt.Sprint(stream)
			m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
			m.SetConfig(&config.Config{OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{
				ge.Provider: {{Status: 400, Match: []string{"Google generation"}, Action: RequestScopedActionContinueAndCooldown}},
			}})
			m.SetRetryConfig(3, 0, 3)
			registerEnterpriseModelTestAuth(t, m, "a-"+model, "global", "engine-a", model)
			registerEnterpriseModelTestAuth(t, m, "b-"+model, "global", "engine-b", model)
			var calls int
			var mu sync.Mutex
			fail := func(ctx context.Context) error {
				exec.MarkUpstreamAttempt(ctx)
				mu.Lock()
				calls++
				mu.Unlock()
				return enterpriseModelTestRejection(model, true)
			}
			m.RegisterExecutor(&customStreamMockExecutor{
				identifier: ge.Provider,
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
					return exec.Response{}, fail(ctx)
				}},
				streamFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
					ch := make(chan exec.StreamChunk, 2)
					ch <- exec.StreamChunk{Payload: []byte(`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`)}
					ch <- exec.StreamChunk{Err: fail(ctx)}
					close(ch)
					return &exec.StreamResult{Chunks: ch}, nil
				},
			})
			req := exec.Request{Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}
			opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}}
			var resultErr error
			if stream {
				var result *exec.StreamResult
				result, resultErr = m.ExecuteStream(t.Context(), []string{ge.Provider}, req, opts)
				if resultErr == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							resultErr = chunk.Err
						}
					}
				}
			} else {
				_, resultErr = m.Execute(t.Context(), []string{ge.Provider}, req, opts)
			}
			mu.Lock()
			defer mu.Unlock()
			if resultErr == nil || calls != 1 {
				t.Fatalf("started generation was replayed: calls=%d error=%v", calls, resultErr)
			}
		})
	}
}

func TestEnterpriseModelAvailabilityBindingIsolationAndExpiry(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC)
	m.enterpriseModels.nowFunc = func() time.Time { return now }
	a := registerEnterpriseModelTestAuth(t, m, "availability-primary", "global", "engine-a", "model-a")
	b := registerEnterpriseModelTestAuth(t, m, "availability-same-binding", "global", "engine-a", "model-a")
	otherRegion := registerEnterpriseModelTestAuth(t, m, "availability-other-region", "us", "engine-a", "model-a")
	otherEngine := registerEnterpriseModelTestAuth(t, m, "availability-other-engine", "global", "engine-b", "model-a")
	ctx := withEnterpriseModelAttempts(context.Background())
	m.observeEnterpriseModelError(ctx, a, "model-a", enterpriseModelTestRejection("model-a", false))
	wantUntil := now.Add(time.Minute)
	for _, auth := range []*Auth{a, b} {
		if got := m.enterpriseModels.retryAt(auth, "model-a"); !got.Equal(wantUntil) {
			t.Fatalf("credential %s did not share application rejection: %v", auth.ID, got)
		}
		if got := m.enterpriseModels.retryAt(auth, "model-b"); !got.IsZero() {
			t.Fatal("one model rejection affected a sibling model")
		}
		if view := auth.enterpriseQuota.modelAvailabilityViews(now); len(view) != 1 || view[0].ModelKey != "model-a" || view[0].RemainingSeconds != 60 || view[0].Reason != "model_not_enabled" {
			t.Fatalf("invalid projected cooldown: %+v", view)
		}
	}
	for _, auth := range []*Auth{otherRegion, otherEngine} {
		if !m.enterpriseModels.retryAt(auth, "model-a").IsZero() {
			t.Fatal("application rejection escaped its binding")
		}
	}
	if enterpriseRetryCandidateAllowed(ctx, otherRegion) || !enterpriseRetryCandidateAllowed(ctx, otherEngine) {
		t.Fatal("fallback did not preserve the first rejected application's region")
	}
	now = now.Add(time.Minute - time.Nanosecond)
	if m.enterpriseModels.retryAt(b, "model-a").IsZero() {
		t.Fatal("model cooldown expired before its 60 second boundary")
	}
	now = now.Add(time.Nanosecond)
	if !m.enterpriseModels.retryAt(b, "model-a").IsZero() || len(b.enterpriseQuota.modelAvailabilityViews(now)) != 0 {
		t.Fatal("model cooldown remained active at the 60 second boundary")
	}
}

func TestEnterpriseModelAvailabilityRefreshAndSuccessDoNotClearRejection(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	m.enterpriseModels.nowFunc = func() time.Time { return now }
	a := registerEnterpriseModelTestAuth(t, m, "availability-refresh", "global", "engine-a", "model-a")
	m.observeEnterpriseModelError(context.Background(), a, "model-a", enterpriseModelTestRejection("model-a", false))
	want := now.Add(time.Minute)
	updated := a.Clone()
	updated.Metadata["access_token"] = "new-synthetic-token"
	current, err := m.Update(context.Background(), updated)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.enterpriseModels.retryAt(current, "model-a"); !got.Equal(want) {
		t.Fatalf("token refresh cleared a resource-level rejection: %v", got)
	}
	m.MarkResult(context.Background(), Result{AuthID: current.ID, Provider: ge.Provider, Model: "model-a", Success: true, CredentialVersion: current.CredentialVersion, RegistrationEpoch: current.RegistrationEpoch})
	if got := m.enterpriseModels.retryAt(current, "model-a"); !got.Equal(want) {
		t.Fatal("a concurrent request success cleared an active rejection")
	}
	selected := m.enterpriseModelSelectionAuth(current, "model-a")
	state := selected.ModelStates["model-a"]
	if state == nil || !state.Unavailable || !state.NextRetryAfter.Equal(want) {
		t.Fatal("selector snapshot omitted the shared model gate")
	}
	if current.ModelStates["model-a"] != nil && current.ModelStates["model-a"].Unavailable {
		t.Fatal("availability projection mutated the saved credential snapshot")
	}
}

func TestEnterpriseModelAvailabilityFencesChangedResourcesAndRegistration(t *testing.T) {
	for _, change := range []string{"resources", "registration"} {
		t.Run(change, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			old := registerEnterpriseModelTestAuth(t, m, "availability-stale-"+change, "global", "engine-a", "model-a")
			updated := old.Clone()
			var err error
			if change == "resources" {
				binding := ge.Resources(ge.FromMetadata(old.Metadata))
				binding.EngineID = "engine-b"
				updated, err = m.UpdateEnterpriseResources(context.Background(), old, binding)
			} else {
				updated, err = m.Register(context.Background(), updated)
			}
			if err != nil {
				t.Fatal(err)
			}
			m.observeEnterpriseModelError(context.Background(), old, "model-a", enterpriseModelTestRejection("model-a", false))
			if !m.enterpriseModels.retryAt(updated, "model-a").IsZero() || !m.enterpriseModels.retryAt(old, "model-a").IsZero() {
				t.Fatal("stale request created a model gate after credential mutation")
			}
			if err := ValidateEnterpriseModelRequest(context.Background(), old, "model-a"); err == nil {
				t.Fatal("stale dispatch was not fenced")
			}
		})
	}
}

func TestEnterpriseModelAvailabilityHonorsCooldownSwitches(t *testing.T) {
	for _, scope := range []string{"credential", "global"} {
		t.Run(scope, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			a := registerEnterpriseModelTestAuth(t, m, "availability-switch-"+scope, "global", "engine-a", "model-a")
			m.observeEnterpriseModelError(context.Background(), a, "model-a", enterpriseModelTestRejection("model-a", false))
			if err := ValidateEnterpriseModelRequest(context.Background(), a, "model-a"); err == nil {
				t.Fatal("enabled cooldown failed to prevent dispatch")
			}
			if scope == "global" {
				m.SetConfig(&config.Config{DisableCooling: true})
			} else {
				updated := a.Clone()
				updated.Metadata["disable_cooling"] = true
				var err error
				a, err = m.Update(context.Background(), updated)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateEnterpriseModelRequest(context.Background(), a, "model-a"); err != nil {
				t.Fatalf("disabled cooldown was still enforced: %v", err)
			}
			if len(a.enterpriseQuota.modelAvailabilityViews(time.Now())) != 0 {
				t.Fatal("disabled cooldown was still projected")
			}
		})
	}
}

func TestEnterpriseModelProbeRejectsBindingAndPayloadModelChanges(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := registerEnterpriseModelTestAuth(t, m, "availability-probe", "global", "engine-a", "model-a")
	expected := ge.Resources(ge.FromMetadata(a.Metadata))
	ctx := WithEnterpriseModelProbe(context.Background(), a.ID, expected, "model-a")
	if err := ValidateEnterpriseModelRequest(ctx, a, "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnterpriseModelRequest(ctx, a, "model-b"); err == nil {
		t.Fatal("payload model override escaped fixed model probe")
	}
	other := registerEnterpriseModelTestAuth(t, m, "availability-probe-other", "global", "engine-a", "model-a")
	if err := ValidateEnterpriseModelRequest(ctx, other, "model-a"); err == nil || enterpriseRetryCandidateAllowed(ctx, other) {
		t.Fatal("probe escaped its exact credential")
	}
	expected.EngineID = "another-engine"
	ctx = WithEnterpriseModelProbe(context.Background(), a.ID, expected, "model-a")
	if err := ValidateEnterpriseModelRequest(ctx, a, "model-a"); err == nil {
		t.Fatal("probe escaped its expected application binding")
	}
}

func TestEnterpriseModelAvailabilityUsesFinalUpstreamModel(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := registerEnterpriseModelTestAuth(t, m, "availability-final-model", "global", "engine-a", "route-alias")
	ctx := withEnterpriseModelAttempts(context.Background())
	m.observeEnterpriseModelError(ctx, a, "route-alias", enterpriseModelTestRejection("actual-upstream-model", false))
	if m.enterpriseModels.retryAt(a, "actual-upstream-model").IsZero() || !m.enterpriseModels.retryAt(a, "route-alias").IsZero() {
		t.Fatal("availability was assigned to a logical alias instead of the final model")
	}
	other := registerEnterpriseModelTestAuth(t, m, "availability-final-other", "global", "engine-b", "route-alias")
	m.PublishEnterprisePolicyCatalog(other, []ge.Model{{ID: "actual-upstream-model"}})
	if err := ValidateEnterpriseModelRequest(ctx, other, "actual-upstream-model"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnterpriseModelRequest(ctx, other, "another-upstream-model"); err == nil {
		t.Fatal("fallback silently changed the actual upstream model")
	}
}

func TestEnterpriseModelAvailabilityConcurrentObservations(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	m.enterpriseModels.nowFunc = func() time.Time { return now }
	a := registerEnterpriseModelTestAuth(t, m, "availability-concurrent", "global", "engine-a", "model-a")
	b := registerEnterpriseModelTestAuth(t, m, "availability-concurrent-sibling", "global", "engine-a", "model-a")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				model := fmt.Sprintf("model-%d", i%3)
				m.observeEnterpriseModelError(context.Background(), a, model, enterpriseModelTestRejection(model, false))
				m.enterpriseModels.retryAt(b, model)
				m.enterpriseModelSelectionAuth(b, model)
				b.enterpriseQuota.modelAvailabilityViews(now)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		if got := m.enterpriseModels.retryAt(b, fmt.Sprintf("model-%d", i)); !got.Equal(now.Add(time.Minute)) {
			t.Fatalf("concurrent application rejection lost: %v", got)
		}
	}
}

func TestEnterpriseModelAvailabilityAliasFallbackCannotChangeModel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			route := "availability-alias-" + fmt.Sprint(stream)
			const target = "availability-actual-a"
			m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
			m.SetRetryConfig(2, 0, 3)
			for i, upstream := range []string{target, "availability-actual-b", target} {
				a := registerEnterpriseModelTestAuth(t, m, fmt.Sprintf("%d-%s", i, route), "global", fmt.Sprintf("engine-%d", i), route)
				updated := a.Clone()
				SetOAuthModelAliasesAttribute(updated, []config.OAuthModelAlias{{Name: upstream, Alias: route, Fork: true}})
				if _, err := m.Update(context.Background(), updated); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			var calls []string
			run := func(ctx context.Context, a *Auth, req exec.Request) error {
				exec.MarkUpstreamAttempt(ctx)
				mu.Lock()
				calls = append(calls, a.ID+"/"+req.Model)
				mu.Unlock()
				if req.Model != target {
					t.Errorf("fallback contacted a different underlying model: %q", req.Model)
				}
				if strings.HasPrefix(a.ID, "0-") {
					return enterpriseModelTestRejection(req.Model, false)
				}
				return nil
			}
			m.RegisterExecutor(&customStreamMockExecutor{
				identifier: ge.Provider,
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, a *Auth, req exec.Request, _ exec.Options) (exec.Response, error) {
					return exec.Response{Payload: []byte("OK")}, run(ctx, a, req)
				}},
				streamFn: func(ctx context.Context, a *Auth, req exec.Request, _ exec.Options) (*exec.StreamResult, error) {
					if err := run(ctx, a, req); err != nil {
						return nil, err
					}
					ch := make(chan exec.StreamChunk, 1)
					ch <- exec.StreamChunk{Payload: []byte("OK")}
					close(ch)
					return &exec.StreamResult{Chunks: ch}, nil
				},
			})
			req := exec.Request{Model: route}
			opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}}
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
			mu.Lock()
			defer mu.Unlock()
			if err != nil || len(calls) != 2 || !strings.HasPrefix(calls[0], "0-") || !strings.HasPrefix(calls[1], "2-") {
				t.Fatalf("same-actual-model alias fallback: calls=%v error=%v", calls, err)
			}
		})
	}
}

func TestEnterpriseModelProbeIgnoresConfiguredContinueRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			model := "availability-single-probe-" + fmt.Sprint(stream)
			m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
			m.SetConfig(&config.Config{OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{
				ge.Provider: {{Status: 500, Match: []string{"probe failure"}, Action: RequestScopedActionContinue}},
			}})
			m.SetRetryConfig(4, 0, 4)
			a := registerEnterpriseModelTestAuth(t, m, "single-probe-"+fmt.Sprint(stream), "global", "engine-a", model)
			ctx := WithEnterpriseModelProbe(t.Context(), a.ID, ge.Resources(ge.FromMetadata(a.Metadata)), model)
			var mu sync.Mutex
			calls := 0
			fail := func(ctx context.Context) error {
				exec.MarkUpstreamAttempt(ctx)
				mu.Lock()
				calls++
				mu.Unlock()
				return &Error{HTTPStatus: 500, Message: "probe failure"}
			}
			m.RegisterExecutor(&customStreamMockExecutor{
				identifier: ge.Provider,
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
					return exec.Response{}, fail(ctx)
				}},
				streamFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
					ch := make(chan exec.StreamChunk, 1)
					ch <- exec.StreamChunk{Err: fail(ctx)}
					close(ch)
					return &exec.StreamResult{Chunks: ch}, nil
				},
			})
			opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content", exec.PinnedAuthMetadataKey: a.ID}}
			var err error
			if stream {
				_, err = m.ExecuteStream(ctx, []string{ge.Provider}, exec.Request{Model: model}, opts)
			} else {
				_, err = m.Execute(ctx, []string{ge.Provider}, exec.Request{Model: model}, opts)
			}
			mu.Lock()
			defer mu.Unlock()
			if err == nil || calls != 1 {
				t.Fatalf("single test request retried: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestEnterpriseModelStaleFailureStillConstrainsRequestRetry(t *testing.T) {
	for _, change := range []string{"token", "resources", "registration"} {
		t.Run(change, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			old := registerEnterpriseModelTestAuth(t, m, "stale-retry-"+change, "global", "engine-a", "model-a")
			foreign := registerEnterpriseModelTestAuth(t, m, "stale-retry-foreign-"+change, "us", "engine-b", "model-a")
			updated := old.Clone()
			var err error
			switch change {
			case "token":
				updated.Metadata["access_token"] = "new-token"
				_, err = m.Update(context.Background(), updated)
			case "resources":
				binding := ge.Resources(ge.FromMetadata(old.Metadata))
				binding.Location = "eu"
				_, err = m.UpdateEnterpriseResources(context.Background(), old, binding)
			case "registration":
				_, err = m.Register(context.Background(), updated)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := withEnterpriseModelAttempts(context.Background())
			m.observeEnterpriseModelError(ctx, old, "model-a", enterpriseModelTestRejection("model-a", false))
			if enterpriseRetryCandidateAllowed(ctx, foreign) {
				t.Fatal("stale failure publication fencing erased the same-region request retry constraint")
			}
		})
	}
}

type enterpriseProbeRefreshExecutor struct {
	*customStreamMockExecutor
	refreshes atomic.Int32
}

func (e *enterpriseProbeRefreshExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	e.refreshes.Add(1)
	updated := a.Clone()
	updated.Metadata["access_token"] = "new-synthetic-token"
	updated.Metadata["expiry"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	return updated, nil
}

func TestEnterpriseModelProbeRetainsOneUnauthorizedRefresh(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			model := "availability-probe-refresh-" + fmt.Sprint(stream)
			m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
			m.SetRetryConfig(4, 0, 4)
			a := registerEnterpriseModelTestAuth(t, m, "probe-refresh-"+fmt.Sprint(stream), "global", "engine-a", model)
			updated := a.Clone()
			updated.Metadata["access_token"] = "old-synthetic-token"
			var err error
			a, err = m.Update(context.Background(), updated)
			if err != nil {
				t.Fatal(err)
			}
			var attempts atomic.Int32
			run := func(ctx context.Context, a *Auth) error {
				exec.MarkUpstreamAttempt(ctx)
				attempts.Add(1)
				if ge.String(a.Metadata, "access_token") != "new-synthetic-token" {
					return &ge.GenerationError{HTTPStatus: http.StatusUnauthorized}
				}
				return nil
			}
			provider := &enterpriseProbeRefreshExecutor{customStreamMockExecutor: &customStreamMockExecutor{
				identifier: ge.Provider,
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, a *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
					return exec.Response{Payload: []byte("OK")}, run(ctx, a)
				}},
				streamFn: func(ctx context.Context, a *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
					if err := run(ctx, a); err != nil {
						return nil, err
					}
					ch := make(chan exec.StreamChunk, 1)
					ch <- exec.StreamChunk{Payload: []byte("OK")}
					close(ch)
					return &exec.StreamResult{Chunks: ch}, nil
				},
			}}
			m.RegisterExecutor(provider)
			ctx := WithEnterpriseModelProbe(t.Context(), a.ID, ge.Resources(ge.FromMetadata(a.Metadata)), model)
			opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content", exec.PinnedAuthMetadataKey: a.ID}}
			if stream {
				var result *exec.StreamResult
				result, err = m.ExecuteStream(ctx, []string{ge.Provider}, exec.Request{Model: model}, opts)
				if err == nil {
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = m.Execute(ctx, []string{ge.Provider}, exec.Request{Model: model}, opts)
			}
			if err != nil || provider.refreshes.Load() != 1 || attempts.Load() != 2 {
				t.Fatalf("probe lost ordinary refresh: calls=%d refreshes=%d error=%v", attempts.Load(), provider.refreshes.Load(), err)
			}
		})
	}
}

func TestEnterpriseModelAvailabilityNestedExecutionHasIndependentRetryScope(t *testing.T) {
	m := NewManager(nil, nil, nil)
	global := registerEnterpriseModelTestAuth(t, m, "availability-nested-global", "global", "engine-global", "model-a")
	us := registerEnterpriseModelTestAuth(t, m, "availability-nested-us", "us", "engine-us", "model-b")
	parent := withEnterpriseModelAttempts(context.Background())
	m.observeEnterpriseModelError(parent, global, "model-a", enterpriseModelTestRejection("model-a", false))
	child := withEnterpriseModelAttempts(parent)
	if !enterpriseRetryCandidateAllowed(child, us) {
		t.Fatal("nested execution inherited its caller's model retry constraint")
	}
	m.observeEnterpriseModelError(child, us, "model-b", enterpriseModelTestRejection("model-b", false))
	if !enterpriseRetryCandidateAllowed(parent, global) || enterpriseRetryCandidateAllowed(parent, us) {
		t.Fatal("nested execution overwrote its caller's retry constraint")
	}
	probe := WithEnterpriseModelProbe(parent, global.ID, ge.Resources(ge.FromMetadata(global.Metadata)), "model-a")
	nestedProbe := withEnterpriseModelAttempts(probe)
	if enterpriseRetryCandidateAllowed(nestedProbe, us) || !enterpriseRetryCandidateAllowed(nestedProbe, global) {
		t.Fatal("initializing an execution discarded the explicit administrative probe pin")
	}
}

func TestEnterpriseModelAvailabilityNeverWaitsAfterSecondaryTransientFailure(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 2*time.Minute, 3)
	a := registerEnterpriseModelTestAuth(t, m, "availability-round-original", "global", "engine-a", "model-a")
	b := registerEnterpriseModelTestAuth(t, m, "availability-round-secondary", "global", "engine-b", "model-a")
	ctx := withEnterpriseModelAttempts(context.Background())
	m.observeEnterpriseModelError(ctx, a, "model-a", enterpriseModelTestRejection("model-a", false))
	secondary := &Error{HTTPStatus: 500, Message: "secondary transient failure"}
	m.MarkResult(context.Background(), Result{AuthID: b.ID, Provider: ge.Provider, Model: "model-a", Error: secondary, CredentialVersion: b.CredentialVersion, RegistrationEpoch: b.RegistrationEpoch})
	wait, shouldRetry := m.shouldRetryAfterErrorWithAttempted(ctx, exec.Options{}, secondary, 0, []string{ge.Provider}, "model-a", 2*time.Minute, -1, 3, nil)
	if shouldRetry || wait != 0 {
		t.Fatalf("model availability request entered another cooldown retry round: wait=%v retry=%v", wait, shouldRetry)
	}
}
