package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const quotaPressureTestModel = "gemini-3.8-flash"

func quotaPressureTestManager() (*Manager, *time.Time) {
	m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
	now := time.Date(2026, 11, 17, 7, 50, 0, 0, time.UTC)
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	m.enterprisePolicies.nowFunc = func() time.Time { return now }
	return m, &now
}

func quotaPressureAuth(t *testing.T, m *Manager, id, project, location string, paid bool) *Auth {
	t.Helper()
	a := sharedQuotaAuth(t, m, id, project, location, "enterprise_plus", id)
	trial := !paid
	m.InvalidateEnterpriseSubscription(a)
	flight := m.BeginEnterpriseSubscriptionRefresh(a)
	i := a.EnterpriseQuotaIdentity
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{
		Status: "ok", State: "ACTIVE", FreeTrial: &trial, Edition: i.Edition,
		ProjectNumber: i.ProjectNumber, PrincipalHash: i.PrincipalHash, CheckedAt: m.enterpriseQuota.now(),
	}) {
		t.Fatal("could not publish verified subscription")
	}
	m.PublishEnterpriseSubscriptionIdentity(a)
	a, _ = m.GetByID(id)
	m.PublishEnterprisePolicyCatalog(a, []ge.Model{{ID: quotaPressureTestModel}})
	return a
}

func plainGoogleRateLimit(a *Auth, now time.Time, model, retryAfter string) *ge.GenerationError {
	err := ge.ParseGenerationError(nil, http.StatusTooManyRequests, http.Header{"Retry-After": {retryAfter}},
		ge.FromMetadata(a.Metadata), a.EnterpriseQuotaIdentity, now, false)
	err.Model = model
	return err
}

func quotaPressureAttempt(t *testing.T, ctx context.Context, a *Auth, err error) {
	t.Helper()
	ticket, errBegin := a.enterpriseQuota.begin()
	if errBegin != nil {
		t.Fatalf("could not dispatch request: %v", errBegin)
	}
	ticket.request = enterpriseQuotaRequestFromContext(ctx)
	ticket.finish(err)
}

func TestEnterprisePaidQuotaPressureDeduplicatesManagerRetries(t *testing.T) {
	m, now := quotaPressureTestManager()
	m.SetRetryConfig(0, 0, 3)
	a := quotaPressureAuth(t, m, "pressure-retry-a", "named-project", "us", true)
	b := quotaPressureAuth(t, m, "pressure-retry-b", "12345", "us", true)
	c := quotaPressureAuth(t, m, "pressure-retry-c", "12345", "us", true)
	for _, account := range []*Auth{a, b, c} {
		registry.GetGlobalRegistry().RegisterClient(account.ID, ge.Provider, []*registry.ModelInfo{{ID: quotaPressureTestModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(account.ID) })
		m.scheduler.upsertAuth(account)
	}
	attempts := 0
	m.RegisterExecutor(&customStreamMockExecutor{
		identifier: ge.Provider,
		mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, a *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
			exec.MarkUpstreamAttempt(ctx)
			attempts++
			return exec.Response{}, plainGoogleRateLimit(a, *now, quotaPressureTestModel, "")
		}},
	})
	_, err := m.Execute(t.Context(), []string{ge.Provider}, exec.Request{Model: quotaPressureTestModel},
		exec.Options{SourceFormat: translator.FormatGemini, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}})
	if statusCodeFromError(err) != 429 || attempts != 3 || !m.EnterpriseQuotaRefreshNeeded(a) {
		t.Fatalf("logical request produced %d upstream calls, error %v", attempts, err)
	}
	observeBalance(t, m, a, *now, 100)
	if view := b.enterpriseQuota.views(*now); len(view) != 0 {
		t.Fatalf("three retries were counted as three requests: %+v", view)
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), b, plainGoogleRateLimit(b, *now, quotaPressureTestModel, "120"))
	if len(b.enterpriseQuota.views(*now)) != 0 {
		t.Fatal("two independent requests paused the edition")
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), c, plainGoogleRateLimit(c, *now, quotaPressureTestModel, ""))
	view := b.enterpriseQuota.views(*now)
	if len(view) != 1 || view[0].Phase != "cooling" || view[0].RestrictionKind != "" ||
		view[0].TimeSource != "local_quota_pressure" || view[0].RelatedAuthCount != 3 || !view[0].RetryAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("expected a backward-compatible shared pause honoring Retry-After: %+v", view)
	}
	if m.EnterpriseQuotaBalancingEnabled() || m.EnterpriseQuotaRefreshNeeded(a) {
		t.Fatal("quota protection changed balancing or kept reading a paused pool")
	}
}

func TestEnterprisePaidQuotaPressureRequiresCurrentUsageAndIndependent429s(t *testing.T) {
	for _, scenario := range []string{"below 99%", "stale quota", "old 429", "previous day", "trial", "trial observation", "local errors", "media"} {
		t.Run(scenario, func(t *testing.T) {
			m, now := quotaPressureTestManager()
			m.SetConfig(&config.Config{Routing: config.RoutingConfig{GeminiEnterpriseQuotaBalancing: true}})
			if scenario == "previous day" {
				*now = ge.NextPacificMidnight(*now).Add(-10 * time.Second)
			}
			a := quotaPressureAuth(t, m, "pressure-miss", "12345", "us", scenario != "trial")
			left := int64(100)
			if scenario == "below 99%" {
				left++
			}
			observer := a
			if scenario == "trial observation" {
				observer = quotaPressureAuth(t, m, "pressure-other-observer", "12345", "us", false)
			}
			observeBalance(t, m, observer, *now, left)
			if scenario == "stale quota" {
				*now = now.Add(EnterpriseQuotaObservationTTL + time.Second)
			}
			for i := range 3 {
				if i == 1 {
					switch scenario {
					case "old 429":
						*now = now.Add(enterpriseQuotaPressureWindow + time.Nanosecond)
					case "previous day":
						*now = ge.NextPacificMidnight(*now)
						observeBalance(t, m, a, *now, left)
					}
				}
				var rejection error = plainGoogleRateLimit(a, *now, quotaPressureTestModel, "")
				switch scenario {
				case "local errors":
					rejection = &enterpriseLocalGateError{newModelCooldownError("", ge.Provider, time.Second)}
				case "media":
					rejection = plainGoogleRateLimit(a, *now, "gemini-3-pro-image", "")
				}
				quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, rejection)
			}
			if view := a.enterpriseQuota.views(*now); len(view) != 0 {
				t.Fatalf("insufficient evidence paused an edition: %+v", view)
			}
		})
	}
}

func TestEnterprisePaidQuotaPressureSingleProbeAndLateObservation(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "pressure-probe-a", "named-project", "us", true)
	b := quotaPressureAuth(t, m, "pressure-probe-b", "12345", "us", true)
	trial := quotaPressureAuth(t, m, "pressure-probe-trial", "12345", "us", false)
	other := quotaPressureAuth(t, m, "pressure-probe-other", "12345", "eu", true)
	oldSuccess, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, plainGoogleRateLimit(a, *now, quotaPressureTestModel, ""))
	}
	if len(a.enterpriseQuota.views(*now)) != 0 {
		t.Fatal("no observation yet")
	}
	observeBalance(t, m, b, *now, 100)
	oldSuccess.finish(nil)
	if len(a.enterpriseQuota.views(*now)) != 1 {
		t.Fatal("late observation did not use the recent 429s, or an older success reopened the pool")
	}
	for _, member := range []*Auth{trial, other} {
		if blocked, _ := member.enterpriseQuota.blocked(*now); blocked {
			t.Fatalf("unrelated credential %s was paused", member.ID)
		}
	}
	if snapshot := m.sharedQuotaSnapshot(); snapshot.Validate() != nil {
		t.Fatal("runtime-only near-limit cooldown corrupted the existing persistence format")
	}
	*now = now.Add(time.Minute)
	leases := make(chan *enterpriseQuotaTicket, 32)
	var wg sync.WaitGroup
	for i := range 32 {
		member := a
		if i%2 == 1 {
			member = b
		}
		wg.Go(func() {
			if ticket, err := member.enterpriseQuota.begin(); err == nil {
				leases <- ticket
			} else {
				var blocked *enterpriseLocalGateError
				if !errors.As(err, &blocked) {
					t.Errorf("unexpected gate error: %v", err)
				}
			}
		})
	}
	wg.Wait()
	if len(leases) != 1 {
		t.Fatalf("admitted %d simultaneous recovery requests", len(leases))
	}
	probe := <-leases
	owner, _ := m.GetByID(probe.gate.authID)
	owner.Metadata["note"] = "safe concurrent edit"
	if _, err := m.Update(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	if ticket, err := a.enterpriseQuota.begin(); err == nil {
		ticket.finish(context.Canceled)
		t.Fatal("metadata publication released an occupied recovery lease")
	}
	probe.finish(plainGoogleRateLimit(owner, *now, quotaPressureTestModel, "90"))
	if views := a.enterpriseQuota.views(*now); len(views) != 1 || views[0].Phase != "cooling" || !views[0].RetryAt.Equal(now.Add(90*time.Second)) {
		t.Fatalf("failed probe did not pause again: %+v", views)
	}
	*now = now.Add(90 * time.Second)
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), b, nil)
	if views := a.enterpriseQuota.views(*now); len(views) != 0 {
		t.Fatalf("verified recovery did not reopen the pool: %+v", views)
	}
	current, _ := m.GetByID(owner.ID)
	if current.Metadata["note"] != "safe concurrent edit" || current.Disabled {
		t.Fatal("runtime recovery changed saved credential settings")
	}
}

func TestEnterprisePaidDailyQuotaRecoveryAfterBeijingReset(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "pressure-daily-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "pressure-daily-b", "12345", "us", true)
	disabled := quotaPressureAuth(t, m, "pressure-daily-disabled", "12345", "us", true)
	disabled.Disabled = true
	var err error
	disabled, err = m.Update(t.Context(), disabled)
	if err != nil {
		t.Fatal(err)
	}
	rejectDaily := func(retryAfter string) error {
		binding := ge.FromMetadata(a.Metadata)
		raw := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","metadata":{"service":"discoveryengine.googleapis.com","consumer":"projects/12345","location":"us","quota_metric":"discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional","quota_limit":"TextAnswersPerDay"}}]}}`)
		return ge.ParseGenerationError(raw, 429, http.Header{"Retry-After": {retryAfter}}, binding, a.EnterpriseQuotaIdentity, *now, false)
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, rejectDaily("15"))
	views := b.enterpriseQuota.views(*now)
	reset := time.Date(2026, 11, 17, 16, 0, 0, 0, enterpriseRecoveryLocation)
	if len(views) != 1 || views[0].Phase != "cooling" || views[0].RestrictionKind != "daily" ||
		views[0].RetryAt.Before(reset.Add(2*time.Minute)) || views[0].RetryAt.After(reset.Add(5*time.Minute)) {
		t.Fatalf("daily rejection did not pause until the scheduled window: %+v", views)
	}
	*now = reset.Add(time.Minute)
	if blocked, _ := b.enterpriseQuota.blocked(*now); !blocked {
		t.Fatal("dispatched during the reset propagation delay")
	}
	*now = views[0].RetryAt
	recoveryCtx := WithEnterpriseQuotaRequest(t.Context())
	quotaPressureAttempt(t, recoveryCtx, b, rejectDaily(""))
	if views = b.enterpriseQuota.views(*now); len(views) != 1 || !views[0].RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("post-reset quota error delayed the next probe by a full day: %+v", views)
	}
	*now = now.Add(time.Minute)
	quotaPressureAttempt(t, recoveryCtx, a, nil)
	if _, err = disabled.enterpriseQuota.begin(); err == nil || len(b.enterpriseQuota.views(*now)) != 0 {
		t.Fatal("automatic quota recovery did not preserve manual disablement")
	}
}

func TestEnterprisePaidQuotaProbeWithZeroBalanceAndCredentialRotation(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "pressure-balance-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "pressure-balance-b", "12345", "us", true)
	for range 3 {
		quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, plainGoogleRateLimit(a, *now, quotaPressureTestModel, ""))
	}
	m.SetConfig(&config.Config{Routing: config.RoutingConfig{GeminiEnterpriseQuotaBalancing: true}})
	observeBalance(t, m, a, *now, 0)
	*now = now.Add(time.Minute)
	if _, err := a.enterpriseQuota.beginForModel("gemini-3-pro-image"); err == nil {
		t.Fatal("image request was allowed to verify the text quota")
	}
	s := &QuotaBalancedSelector{Fallback: &RoundRobinSelector{}}
	picked := balancedPick(t, s, a, b)
	probe, err := picked.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err != nil {
		t.Fatalf("balance estimate prevented the recovery probe: %v", err)
	}
	updated := picked.Clone()
	updated.Metadata["refresh_token"] = "synthetic-rotated"
	if _, err := m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	sibling := a
	if picked.ID == a.ID {
		sibling = b
	}
	if ticket, err := sibling.enterpriseQuota.begin(); err == nil {
		ticket.finish(context.Canceled)
		t.Fatal("token rotation allowed a second in-flight probe")
	}
	probe.finish(nil)
	if views := sibling.enterpriseQuota.views(*now); len(views) != 1 || views[0].Phase != "probe_pending" {
		t.Fatalf("a superseded credential recovered the current quota: %+v", views)
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), sibling, nil)
	if views := sibling.enterpriseQuota.views(*now); len(views) != 0 {
		t.Fatalf("current verified probe failed to recover: %+v", views)
	}
}

func TestEnterprisePaidQuotaEmptyStreamDoesNotRecover(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "pressure-empty-stream", "12345", "us", true)
	for range 3 {
		quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, plainGoogleRateLimit(a, *now, quotaPressureTestModel, ""))
	}
	observeBalance(t, m, a, *now, 100)
	*now = now.Add(time.Minute)
	source := make(chan exec.StreamChunk)
	close(source)
	e := &enterpriseQuotaExecutor{ProviderExecutor: &sharedQuotaStreamExecutor{chunks: source}}
	stream, err := e.ExecuteStream(t.Context(), a, exec.Request{Model: quotaPressureTestModel}, exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	chunk, ok := <-stream.Chunks
	if !ok || !errors.Is(chunk.Err, io.ErrUnexpectedEOF) || statusCodeFromError(chunk.Err) != 502 {
		t.Fatalf("incomplete probe did not surface an upstream failure: %+v", chunk)
	}
	for range stream.Chunks {
	}
	if views := a.enterpriseQuota.views(*now); len(views) != 1 || views[0].Phase != "cooling" || !views[0].RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("empty stream recovered the quota or kept probing immediately: %+v", views)
	}
}

func TestEnterprisePaidQuotaNewRestrictionKeepsTheRecoverySlotOccupied(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "pressure-prior-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "pressure-prior-b", "12345", "us", true)
	prior, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, plainGoogleRateLimit(a, *now, quotaPressureTestModel, ""))
	}
	observeBalance(t, m, a, *now, 100)
	*now = now.Add(time.Minute)
	probe, err := b.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	prior.finish(&ge.GenerationError{HTTPStatus: 429, Restriction: &ge.Restriction{
		Kind: "minute", Scope: "edition", Edition: "enterprise_plus", Feature: "assistant_search",
		RetryAt: now.Add(time.Minute), TimeSource: "google",
	}})
	*now = now.Add(time.Minute)
	if ticket, err := a.enterpriseQuota.begin(); err == nil {
		ticket.finish(context.Canceled)
		t.Fatal("a new restriction let a second recovery request start while the first was in flight")
	}
	probe.finish(nil)
	if views := a.enterpriseQuota.views(*now); len(views) != 1 || views[0].Phase != "probe_pending" {
		t.Fatalf("superseded probe reopened the pool: %+v", views)
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, nil)
	if len(a.enterpriseQuota.views(*now)) != 0 {
		t.Fatal("new recovery request did not reopen the pool")
	}
}
