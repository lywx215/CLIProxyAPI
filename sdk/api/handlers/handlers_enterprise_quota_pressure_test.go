package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/oauth2"
)

type enterpriseQuotaBootstrapExecutor struct {
	coreauth.ProviderExecutor
	calls     int
	successAt int
}

func (*enterpriseQuotaBootstrapExecutor) Identifier() string { return ge.Provider }

func (e *enterpriseQuotaBootstrapExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, _ coreexec.Options) (*coreexec.StreamResult, error) {
	e.calls++
	coreexec.MarkUpstreamAttempt(ctx)
	if e.calls == e.successAt {
		chunks := make(chan coreexec.StreamChunk, 1)
		chunks <- coreexec.StreamChunk{Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")}
		close(chunks)
		return &coreexec.StreamResult{Chunks: chunks}, nil
	}
	rejection := ge.ParseGenerationError([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`),
		http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, ge.FromMetadata(auth.Metadata),
		auth.EnterpriseQuotaIdentity, time.Now(), false)
	rejection.Model = req.Model
	chunks := make(chan coreexec.StreamChunk, 1)
	chunks <- coreexec.StreamChunk{Err: rejection}
	close(chunks)
	return &coreexec.StreamResult{Chunks: chunks}, nil
}

func quotaBootstrapFixture(t *testing.T, models []ge.Model, executor *enterpriseQuotaBootstrapExecutor) (*coreauth.Manager, *BaseAPIHandler, []*coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	// Each Manager call can try one credential. Only the handler can redispatch.
	manager.SetRetryConfig(0, 0, 1)
	manager.RegisterExecutor(executor)
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1},
	}, manager)

	registered := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		registered = append(registered, &registry.ModelInfo{ID: model.ID})
	}
	const project = "123451"
	const edition = "enterprise_plus"
	paid := false
	var credentials []*coreauth.Auth
	for _, id := range []string{"a", "b"} {
		name := "enterprise-quota-bootstrap-" + id
		binding := (sdkconfig.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: project, Location: "us", EngineID: "fixture-app"}).WithDefaults()
		auth, errRegister := manager.Register(t.Context(), &coreauth.Auth{
			ID: name, Provider: ge.Provider, Status: coreauth.StatusActive,
			Metadata: ge.Metadata(binding, name+"@example.test", &oauth2.Token{
				AccessToken: "fixture-access-" + id, RefreshToken: "fixture-refresh-" + id, Expiry: time.Now().Add(time.Hour),
			}),
		})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		flight := manager.BeginEnterpriseSubscriptionRefresh(auth)
		if flight == nil || !flight.Complete(ge.SubscriptionObservation{
			Status: "ok", State: "ACTIVE", Edition: edition, FreeTrial: &paid, ProjectNumber: project,
			PrincipalHash: strings.Repeat(id, 64), CheckedAt: time.Now().UTC(),
		}) {
			t.Fatal("publish paid fixture subscription")
		}
		manager.PublishEnterpriseSubscriptionIdentity(auth)
		auth, _ = manager.GetByID(name)
		manager.PublishEnterprisePolicyCatalog(auth, models)
		registry.GetGlobalRegistry().RegisterClient(name, ge.Provider, registered)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(name) })
		manager.RefreshSchedulerEntry(name)
		credentials = append(credentials, auth)
	}
	return manager, handler, credentials
}

func TestEnterpriseQuotaPressureDeduplicatesHTTPStreamBootstrap(t *testing.T) {
	models := []ge.Model{{ID: "gemini-quota-bootstrap-one"}, {ID: "gemini-quota-bootstrap-two"}, {ID: "gemini-quota-bootstrap-three"}}
	executor := &enterpriseQuotaBootstrapExecutor{}
	manager, handler, credentials := quotaBootstrapFixture(t, models, executor)
	project, edition := "123451", "enterprise_plus"
	rejectRequest := func(model string) int {
		t.Helper()
		before := executor.calls
		data, _, errs := handler.executeStreamWithAuthManagerFormats(t.Context(), "openai", "openai", model,
			[]byte(`{"messages":[{"role":"user","content":"fixture"}]}`), "", false,
			modelExecutionOptions{Operation: "chat_completions"})
		if data != nil {
			for range data {
				t.Fatal("a bootstrap rejection produced payload")
			}
		}
		failed := false
		for result := range errs {
			if result != nil {
				failed = true
				if result.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("bootstrap error status %d, want 429: %v", result.StatusCode, result.Error)
				}
			}
		}
		if !failed {
			t.Fatal("bootstrap rejection did not reach the handler")
		}
		return executor.calls - before
	}
	sharedPause := func(auth *coreauth.Auth) string {
		t.Helper()
		current, found := manager.GetByID(auth.ID)
		if !found {
			t.Fatalf("missing credential %s", auth.ID)
		}
		for _, view := range coreauth.CooldownSnapshotForAuth(current, time.Now()) {
			if view.Origin == "shared_pool" && view.TimeSource == "local_quota_pressure" {
				return view.PoolID
			}
		}
		return ""
	}

	if calls := rejectRequest(models[0].ID); calls != 2 {
		t.Fatalf("first request issued %d attempts, want Manager dispatch plus HTTP bootstrap retry", calls)
	}
	if !manager.EnterpriseQuotaRefreshNeeded(credentials[0]) {
		t.Fatal("bootstrap 429 did not qualify for quota verification")
	}

	now := time.Now().UTC()
	la, errLocation := time.LoadLocation("America/Los_Angeles")
	if errLocation != nil {
		t.Fatal(errLocation)
	}
	year, month, day := now.In(la).Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, la).UTC()
	reset := ge.NextPacificMidnight(now)
	limit, used, remaining := int64(10000), int64(9900), int64(100)
	if !manager.ObserveEnterpriseQuota(credentials[0], ge.QuotaSnapshot{
		Status: "ok", ProjectID: project, Location: "us", ObservedAt: now, PeriodStart: &start, ResetAt: &reset,
		Pools: []ge.QuotaPool{{Tier: edition, Status: "ok", Limit: &limit, Used: &used, Remaining: &remaining, UsageUpdatedAt: &now}},
	}) {
		t.Fatal("near-limit quota observation was not accepted")
	}
	if pool := sharedPause(credentials[0]); pool != "" {
		t.Fatalf("one request paused the pool %s", pool)
	}
	if calls := rejectRequest(models[1].ID); calls != 2 {
		t.Fatalf("second request issued %d attempts, want two", calls)
	}
	if pool := sharedPause(credentials[0]); pool != "" {
		t.Fatalf("two independent requests paused the pool %s", pool)
	}
	if calls := rejectRequest(models[2].ID); calls != 1 {
		t.Fatalf("third request issued %d attempts, want the triggering attempt only", calls)
	}
	a, b := sharedPause(credentials[0]), sharedPause(credentials[1])
	if a == "" || a != b {
		t.Fatalf("three independent requests did not pause the shared pool: %q, %q", a, b)
	}
}

func TestEnterpriseQuotaTenthStreamBootstrapSuccessBreaksStreak(t *testing.T) {
	var models []ge.Model
	for i := range 20 {
		models = append(models, ge.Model{ID: fmt.Sprintf("gemini-streak-test-%d", i)})
	}
	executor := &enterpriseQuotaBootstrapExecutor{successAt: 20}
	manager, handler, credentials := quotaBootstrapFixture(t, models, executor)
	for i, model := range models {
		before := executor.calls
		data, _, errs := handler.executeStreamWithAuthManagerFormats(t.Context(), "openai", "openai", model.ID,
			[]byte(`{"messages":[{"role":"user","content":"test"}]}`), "", false, modelExecutionOptions{Operation: "chat_completions"})
		var payload bool
		if data != nil {
			for chunk := range data {
				payload = payload || len(chunk) > 0
			}
		}
		var status int
		if errs != nil {
			for failure := range errs {
				if failure != nil {
					status = failure.StatusCode
				}
			}
		}
		if executor.calls-before != 2 || payload != (i == 9) || (status == http.StatusTooManyRequests) != (i != 9) {
			t.Fatalf("request %d: attempts=%d payload=%v status=%d", i+1, executor.calls-before, payload, status)
		}
		view := manager.EnterpriseQuotaBalance(credentials[0])
		if (view.ProtectionStatus == "rate_limited") != (i == 19) {
			t.Fatalf("request %d: %+v", i+1, view)
		}
	}
}
