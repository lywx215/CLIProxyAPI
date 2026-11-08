package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func balanceTestManager(t *testing.T) (*Manager, *QuotaBalancedSelector, *time.Time) {
	t.Helper()
	s := &QuotaBalancedSelector{Fallback: &RoundRobinSelector{}}
	m := NewManager(nil, s, nil)
	m.SetConfig(&config.Config{Routing: config.RoutingConfig{GeminiEnterpriseQuotaBalancing: true}})
	now := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	return m, s, &now
}

func balanceObservation(a *Auth, now time.Time, remaining int64) ge.QuotaSnapshot {
	b := ge.FromMetadata(a.Metadata)
	start, reset := enterpriseQuotaDayStart(now), ge.NextPacificMidnight(now)
	limit, used := int64(10000), 10000-remaining
	return ge.QuotaSnapshot{Status: "ok", ProjectID: b.ProjectID, Location: b.Location, ObservedAt: now, PeriodStart: &start, ResetAt: &reset,
		Pools: []ge.QuotaPool{{Tier: "enterprise_plus", Status: "ok", Limit: &limit, Used: &used, Remaining: &remaining, UsageUpdatedAt: &now}}}
}

func observeBalance(t *testing.T, m *Manager, a *Auth, now time.Time, left int64) {
	t.Helper()
	if !m.ObserveEnterpriseQuota(a, balanceObservation(a, now, left)) {
		t.Fatal("observation rejected")
	}
}

func balancedPick(t *testing.T, s *QuotaBalancedSelector, auths ...*Auth) *Auth {
	t.Helper()
	// Manager eligibility is tested separately; these fixtures have no catalogs.
	ctx := context.WithValue(context.Background(), prevalidatedAuthCandidatesKey{}, true)
	a, err := s.Pick(ctx, ge.Provider, "gemini-3.1-pro-preview", exec.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestEnterpriseBalanceSmoothQuotaDistributionAndInFlight(t *testing.T) {
	m, s, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "balance-a", "111", "us", "enterprise_plus", "a")
	b := sharedQuotaAuth(t, m, "balance-b", "222", "us", "enterprise_plus", "b")
	observeBalance(t, m, a, *now, 7000)
	observeBalance(t, m, b, *now, 3000)
	counts := map[string]int{}
	previous, run, longest := "", 0, 0
	for i := 0; i < 100; i++ {
		id := balancedPick(t, s, a, b).ID
		counts[id]++
		if id == previous {
			run++
		} else {
			previous, run = id, 1
		}
		longest = max(longest, run)
	}
	if counts[a.ID] != 70 || counts[b.ID] != 30 || longest > 3 {
		t.Fatalf("distribution=%v longest run=%d; want 70:30 without concentration", counts, longest)
	}
	var tickets []*enterpriseQuotaTicket
	for i := 0; i < 20; i++ {
		ticket, err := a.enterpriseQuota.begin()
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, ticket)
	}
	counts = map[string]int{}
	for i := 0; i < 100; i++ {
		counts[balancedPick(t, s, a, b).ID]++
	}
	if counts[a.ID] < 18 || counts[a.ID] > 22 {
		t.Fatalf("busy pool retained too much traffic: %v", counts)
	}
	for _, ticket := range tickets {
		ticket.finish(nil)
	}
}

func TestEnterpriseBalanceExtremeQuotaAndCredentialWeightsStillSpread(t *testing.T) {
	m, s, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "dominant-a", "111", "us", "enterprise_plus", "a")
	b := sharedQuotaAuth(t, m, "dominant-b", "222", "us", "enterprise_plus", "b")
	observeBalance(t, m, a, *now, 9000)
	observeBalance(t, m, b, *now, 100)
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[balancedPick(t, s, a, b).ID]++
	}
	if counts[a.ID] != 80 || counts[b.ID] != 20 {
		t.Fatalf("extreme pool ratios monopolized requests: %v", counts)
	}
	c := sharedQuotaAuth(t, m, "dominant-c", "111", "us", "enterprise_plus", "c")
	a.Attributes = map[string]string{"weight": "100"}
	counts = map[string]int{}
	for i := 0; i < 100; i++ {
		counts[balancedPick(t, s, a, c).ID]++
	}
	if counts[a.ID] != 80 || counts[c.ID] != 20 {
		t.Fatalf("serial weighted credentials monopolized one application: %v", counts)
	}
}

func TestEnterpriseBalanceDuplicateCredentialsDoNotMultiplyPool(t *testing.T) {
	m, s, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "duplicate-a", "111", "us", "enterprise_plus", "a")
	b := sharedQuotaAuth(t, m, "duplicate-b", "111", "us", "enterprise_plus", "b")
	c := sharedQuotaAuth(t, m, "duplicate-c", "222", "us", "enterprise_plus", "c")
	observeBalance(t, m, a, *now, 5000)
	observeBalance(t, m, c, *now, 5000)
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[balancedPick(t, s, a, b, c).ID]++
	}
	if counts[a.ID] != 25 || counts[b.ID] != 25 || counts[c.ID] != 50 {
		t.Fatalf("duplicate pool biased routing: %v", counts)
	}
}

func TestEnterpriseBalanceLocalDebitsWatermarksAndDailyReset(t *testing.T) {
	m, _, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "debits", "111", "us", "enterprise_plus", "a")
	observeBalance(t, m, a, *now, 2)
	first, _ := a.enterpriseQuota.begin()
	second, _ := a.enterpriseQuota.begin()
	if _, err := a.enterpriseQuota.begin(); err == nil {
		t.Fatal("concurrent requests oversubscribed observed quota")
	}
	first.finish(nil)
	second.finish(errors.New("pre-generation failure"))
	first.finish(nil)
	pool := m.enterpriseQuota.balances[a.enterpriseQuota.balanceKey()]
	if left, _ := pool.remaining(*now, 0); left != 1 || pool.inFlight != 0 {
		t.Fatalf("completion/refund not exactly once: left=%d inFlight=%d", left, pool.inFlight)
	}
	// A newer fetch with the same usage watermark must retain local consumption.
	old := balanceObservation(a, *now, 2)
	*now = now.Add(30 * time.Second)
	old.ObservedAt = *now
	if !m.ObserveEnterpriseQuota(a, old) {
		t.Fatal("same usage watermark should refresh observation freshness")
	}
	if left, _ := pool.remaining(*now, 0); left != 1 {
		t.Fatalf("repeated Monitoring sample refunded local consumption: %d", left)
	}
	*now = now.Add(time.Minute)
	observeBalance(t, m, a, *now, 1)
	if left, _ := pool.remaining(*now, 0); left != 1 || len(pool.debits) != 0 {
		t.Fatal("Monitoring catch-up double charged local consumption")
	}
	observeBalance(t, m, a, *now, 0)
	if blocked, _ := a.enterpriseQuota.blocked(*now); !blocked {
		t.Fatal("zero pool stayed eligible")
	}
	*now = now.Add(EnterpriseQuotaObservationTTL + time.Second)
	if _, err := a.enterpriseQuota.begin(); err == nil {
		t.Fatal("observation expiry replenished a verified exhausted pool")
	}
	*now = ge.NextPacificMidnight(*now)
	if _, known := pool.remaining(*now, 0); known {
		t.Fatal("yesterday's exhausted balance survived Pacific midnight")
	}
	observeBalance(t, m, a, *now, 10000)
	if blocked, _ := a.enterpriseQuota.blocked(*now); blocked {
		t.Fatal("fresh daily pool did not recover")
	}
}

func TestEnterpriseBalanceObservationFencesAndUnknownFallback(t *testing.T) {
	m, s, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "known", "111", "us", "enterprise_plus", "a")
	b := sharedQuotaAuth(t, m, "unknown", "222", "us", "enterprise_plus", "b")
	observeBalance(t, m, a, *now, 1000)
	for _, mutate := range []func(*ge.QuotaSnapshot){
		func(q *ge.QuotaSnapshot) { q.ProjectID = "foreign" },
		func(q *ge.QuotaSnapshot) { q.Location = "eu" },
		func(q *ge.QuotaSnapshot) { q.Pools[0].Tier = "business_standard" },
		func(q *ge.QuotaSnapshot) { q.Pools[0].Status = "stale" },
		func(q *ge.QuotaSnapshot) { q.ObservedAt = q.ObservedAt.Add(-time.Second) },
		func(q *ge.QuotaSnapshot) { q.ObservedAt = q.ObservedAt.Add(time.Second) },
	} {
		q := balanceObservation(a, *now, 9000)
		mutate(&q)
		if m.ObserveEnterpriseQuota(a, q) {
			t.Fatalf("invalid observation accepted: %+v", q)
		}
	}
	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		counts[balancedPick(t, s, a, b).ID]++
	}
	if counts[a.ID] != 10 || counts[b.ID] != 10 {
		t.Fatalf("unknown balance monopolized or starved: %v", counts)
	}
	*now = now.Add(EnterpriseQuotaObservationTTL + time.Second)
	if _, known := m.enterpriseQuota.balances[a.enterpriseQuota.balanceKey()].remaining(*now, 0); known {
		t.Fatal("stale quota stayed authoritative")
	}
	updated := a.Clone()
	updated.Metadata["refresh_token"] = "replacement"
	if _, err := m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	if m.ObserveEnterpriseQuota(a, balanceObservation(a, *now, 9000)) {
		t.Fatal("old credential refreshed current pool")
	}
}

func TestEnterpriseBalanceReleaseAfterMetadataChangeAndCancel(t *testing.T) {
	m, _, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "release-stale", "111", "us", "enterprise_plus", "a")
	observeBalance(t, m, a, *now, 1000)
	ticket, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	updated := a.Clone()
	updated.Metadata["note"] = "metadata refresh"
	if _, err := m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	ticket.finish(context.Canceled)
	ticket.finish(nil)
	pool := ticket.balance
	if pool.inFlight != 0 || len(pool.byAuth) != 0 {
		t.Fatal("credential refresh leaked live occupancy")
	}
	if left, _ := pool.remaining(*now, 0); left != 999 {
		t.Fatal("cancellation failed to retain conservative consumption")
	}
}

func TestEnterpriseBalanceIdentityDiscoveryMigratesLiveReservations(t *testing.T) {
	m, _, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "late-identity", "111", "us", "", "a")
	b := sharedQuotaAuth(t, m, "known-identity", "111", "us", "enterprise_plus", "b")
	observeBalance(t, m, b, *now, 100)
	ticket, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	updated := a.Clone()
	identity := *b.EnterpriseQuotaIdentity
	identity.PrincipalHash = quotaDigest("a")
	updated.EnterpriseQuotaIdentity = &identity
	if _, err := m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	if view := m.EnterpriseQuotaBalance(b); view.InFlight != 1 || view.EffectiveRemaining == nil || *view.EffectiveRemaining != 99 {
		t.Fatalf("identity discovery lost reservation: %+v", view)
	}
	ticket.finish(nil)
	if view := m.EnterpriseQuotaBalance(b); view.InFlight != 0 || view.LocalUnreported != 1 {
		t.Fatalf("old ticket failed to release shared reservation: %+v", view)
	}
}

func TestEnterpriseBalanceConcurrentSelectionTurns(t *testing.T) {
	m, s, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "parallel-a", "111", "us", "enterprise_plus", "a")
	b := sharedQuotaAuth(t, m, "parallel-b", "222", "us", "enterprise_plus", "b")
	observeBalance(t, m, a, *now, 1000)
	observeBalance(t, m, b, *now, 1000)
	const n = 40
	var wg sync.WaitGroup
	results := make(chan *enterpriseQuotaTicket, n)
	errorsCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, err := m.awaitEnterpriseBalanceTurn(context.Background(), []string{ge.Provider})
			if err != nil {
				errorsCh <- err
				return
			}
			defer budget.ReleaseAdmissionTurn(ctx)
			ctx = context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true)
			picked, err := s.Pick(ctx, ge.Provider, "model", exec.Options{}, []*Auth{a, b})
			if err != nil {
				errorsCh <- err
				return
			}
			ticket, err := picked.enterpriseQuota.begin()
			if err != nil {
				errorsCh <- err
				return
			}
			results <- ticket
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for ticket := range results {
		counts[ticket.gate.authID]++
		ticket.finish(nil)
	}
	if counts[a.ID] != n/2 || counts[b.ID] != n/2 {
		t.Fatalf("concurrent selection concentrated: %v", counts)
	}
	ctx, err := m.awaitEnterpriseBalanceTurn(context.Background(), []string{ge.Provider})
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.awaitEnterpriseBalanceTurn(cancelCtx, []string{ge.Provider}); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting selection ignored cancellation: %v", err)
	}
	budget.ReleaseAdmissionTurn(ctx)
}

func TestEnterpriseBalanceSharedRestrictionsDoNotBecomeModelFailures(t *testing.T) {
	for _, kind := range []string{"daily", "minute"} {
		t.Run(kind, func(t *testing.T) {
			m, _, now := balanceTestManager(t)
			a := sharedQuotaAuth(t, m, "scope-a-"+kind, "111", "us", "enterprise_plus", "a")
			b := sharedQuotaAuth(t, m, "scope-b-"+kind, "111", "us", "enterprise_plus", "b")
			other := sharedQuotaAuth(t, m, "scope-other-"+kind, "111", "eu", "enterprise_plus", "a")
			ticket, _ := a.enterpriseQuota.begin()
			retry := now.Add(time.Minute)
			if kind == "daily" {
				retry = ge.NextPacificMidnight(*now)
			}
			ticket.finish(&ge.GenerationError{HTTPStatus: 429, Restriction: &ge.Restriction{Kind: kind, Scope: "edition", Edition: "enterprise_plus", Feature: "assistant_search", RetryAt: retry}})
			observeBalance(t, m, a, *now, 9000)
			for _, candidate := range []*Auth{a, b} {
				for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
					if blocked, _, _ := isAuthBlockedForModel(candidate, model, *now); !blocked {
						t.Fatal("shared restriction did not cover sibling model/credential")
					}
				}
			}
			if blocked, _ := other.enterpriseQuota.blocked(*now); blocked {
				t.Fatal("restriction crossed a regional pool")
			}
			*now = retry
			probe, err := b.enterpriseQuota.begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.enterpriseQuota.begin(); err == nil {
				t.Fatal("recovery allowed more than one probe")
			}
			probe.finish(nil)
			if blocked, _ := a.enterpriseQuota.blocked(*now); blocked {
				t.Fatal("successful shared probe failed to recover")
			}
		})
	}
}

func TestEnterpriseBalanceReserveAndPriority(t *testing.T) {
	m, s, now := balanceTestManager(t)
	m.SetConfig(&config.Config{Routing: config.RoutingConfig{GeminiEnterpriseQuotaBalancing: true, GeminiEnterpriseQuotaReserve: 10}})
	a := sharedQuotaAuth(t, m, "reserve-a", "111", "us", "enterprise_plus", "a")
	b := sharedQuotaAuth(t, m, "reserve-b", "222", "us", "enterprise_plus", "b")
	observeBalance(t, m, a, *now, 10)
	observeBalance(t, m, b, *now, 1000)
	if _, err := a.enterpriseQuota.begin(); err == nil {
		t.Fatal("reserve did not protect the last quota units")
	}
	if picked := balancedPick(t, s, a, b); picked.ID != b.ID {
		t.Fatal("exhausted pool remained selectable")
	}
	observeBalance(t, m, a, *now, 100)
	a.Attributes = map[string]string{"priority": "5"}
	for i := 0; i < 5; i++ {
		if picked := balancedPick(t, s, a, b); picked.ID != a.ID {
			t.Fatal("quota score overrode explicit priority")
		}
	}
	a.Attributes["weight"] = "0"
	if picked := balancedPick(t, s, a, b); picked.ID != b.ID {
		t.Fatal("zero-weight high priority credential hid an eligible lower priority")
	}
	available, err := m.availableAuthsForRouteModelForRequest(t.Context(), []*Auth{a, b}, ge.Provider, "", *now, false)
	if err != nil || len(available) != 1 || available[0].ID != b.ID {
		t.Fatalf("manager priority filtering retained zero weight: %v, %v", available, err)
	}
}

func TestEnterpriseQuotaResetBeijingClock(t *testing.T) {
	beijing := time.FixedZone("CST", 8*60*60)
	for _, tc := range []struct {
		date string
		hour int
	}{
		{"2026-10-31T12:00:00Z", 15},
		{"2026-11-01T12:00:00Z", 16},
		{"2026-11-08T06:00:00Z", 16},
		{"2026-03-08T12:00:00Z", 15},
	} {
		now, _ := time.Parse(time.RFC3339, tc.date)
		reset := ge.NextPacificMidnight(now)
		if reset.In(beijing).Hour() != tc.hour {
			t.Fatal(fmt.Sprintf("reset for %s = %s; want Beijing %d:00", tc.date, reset.In(beijing), tc.hour))
		}
		if start := enterpriseQuotaDayStart(now); start.After(now) || !start.Before(reset) {
			t.Fatalf("invalid Pacific day boundary: %s .. %s", start, reset)
		}
	}
}

func TestEnterpriseBalanceStreamLifetimeAndAdmissionHandoff(t *testing.T) {
	for _, ending := range []string{"success", "cancel", "error"} {
		t.Run(ending, func(t *testing.T) {
			m, _, now := balanceTestManager(t)
			a := sharedQuotaAuth(t, m, "stream-balance-"+ending, "111", "us", "enterprise_plus", "a")
			observeBalance(t, m, a, *now, 100)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, err := m.awaitEnterpriseBalanceTurn(ctx, []string{ge.Provider})
			if err != nil {
				t.Fatal(err)
			}
			source := make(chan exec.StreamChunk)
			e := &enterpriseQuotaExecutor{ProviderExecutor: &sharedQuotaStreamExecutor{chunks: source}}
			stream, err := e.ExecuteStream(ctx, a, exec.Request{}, exec.Options{})
			if err != nil {
				t.Fatal(err)
			}
			// This must succeed while the stream remains open: selection is released
			// after reservation, rather than held for the entire generation.
			nextCtx, err := m.awaitEnterpriseBalanceTurn(context.Background(), []string{ge.Provider})
			if err != nil {
				t.Fatal(err)
			}
			budget.ReleaseAdmissionTurn(nextCtx)
			if view := m.EnterpriseQuotaBalance(a); view.InFlight != 1 || view.CredentialInFlight != 1 {
				t.Fatalf("stream was not counted: %+v", view)
			}
			if ending == "error" {
				go func() { source <- exec.StreamChunk{Err: &ge.GenerationError{HTTPStatus: 429}} }()
				<-stream.Chunks
				if view := m.EnterpriseQuotaBalance(a); view.InFlight != 1 {
					t.Fatal("stream error released occupancy before source completion")
				}
			}
			if ending == "cancel" {
				cancel()
			} else {
				close(source)
			}
			for range stream.Chunks {
			}
			if ending == "cancel" {
				close(source)
			}
			view := m.EnterpriseQuotaBalance(a)
			if view.InFlight != 0 || view.CredentialInFlight != 0 {
				t.Fatalf("stream leaked occupancy: %+v", view)
			}
			want := int64(99)
			if ending == "error" {
				want = 100
			}
			if view.EffectiveRemaining == nil || *view.EffectiveRemaining != want {
				t.Fatalf("stream accounting = %+v, want remaining %d", view, want)
			}
		})
	}
}

type balanceBlockingExecutor struct {
	ProviderExecutor
	started chan string
	release chan struct{}
}

func (*balanceBlockingExecutor) Identifier() string { return ge.Provider }
func (e *balanceBlockingExecutor) Execute(ctx context.Context, a *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
	e.started <- a.ID
	select {
	case <-e.release:
		return exec.Response{Payload: []byte(`{"ok":true}`)}, nil
	case <-ctx.Done():
		return exec.Response{}, ctx.Err()
	}
}

func TestEnterpriseBalanceManagerConcurrentExecution(t *testing.T) {
	m, _, now := balanceTestManager(t)
	a := sharedQuotaAuth(t, m, "execute-a", "111", "us", "enterprise_plus", "a")
	b := sharedQuotaAuth(t, m, "execute-b", "222", "us", "enterprise_plus", "b")
	observeBalance(t, m, a, *now, 1000)
	observeBalance(t, m, b, *now, 1000)
	model := "gemini-3.8-flash"
	registerSchedulerModels(t, ge.Provider, model, a.ID, b.ID)
	const n = 20
	e := &balanceBlockingExecutor{started: make(chan string, n), release: make(chan struct{})}
	m.RegisterExecutor(e)
	var once sync.Once
	unblock := func() { once.Do(func() { close(e.release) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := m.Execute(ctx, []string{ge.Provider}, exec.Request{Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"test"}]}]}`)}, exec.Options{SourceFormat: "gemini", Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}})
			results <- err
		}()
	}
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		select {
		case id := <-e.started:
			counts[id]++
		case err := <-results:
			t.Fatalf("request ended before release: %v", err)
		}
	}
	if counts[a.ID] != n/2 || counts[b.ID] != n/2 {
		t.Fatalf("manager execution concentrated traffic: %v", counts)
	}
	for _, auth := range []*Auth{a, b} {
		if view := m.EnterpriseQuotaBalance(auth); view.InFlight != n/2 {
			t.Fatalf("manager failed to retain active requests: %+v", view)
		}
	}
	unblock()
	for i := 0; i < n; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, auth := range []*Auth{a, b} {
		if view := m.EnterpriseQuotaBalance(auth); view.InFlight != 0 || view.LocalUnreported != n/2 {
			t.Fatalf("manager failed to finish accounting: %+v", view)
		}
	}
}
