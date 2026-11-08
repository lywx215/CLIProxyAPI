package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestEnterpriseProvidersMixedBalancingPreservesFallbackAndRebuild(t *testing.T) {
	m, _, now := balanceTestManager(t)
	*now = time.Now().UTC()
	m.antigravityEnterprise.now = func() time.Time { return *now }
	first := sharedQuotaAuth(t, m, "mixed-daily-a", "111", "us", "enterprise_plus", "first")
	second := sharedQuotaAuth(t, m, "mixed-daily-b", "222", "us", "enterprise_plus", "second")
	observeBalance(t, m, first, *now, 7000)
	observeBalance(t, m, second, *now, 3000)
	parent := aeRegister(t, m, aeAccountFixture("mixed-weekly", aeLicense("project-a", "us"), aeLicense("project-b", "us")))
	bindings := m.AntigravityEnterpriseBindings(parent.ID)
	model := "gemini-3.8-flash"
	registerSchedulerModels(t, ge.Provider, model, first.ID, second.ID)
	// Even an accidentally registered parent must never become a route candidate.
	registerSchedulerModels(t, ae.Provider, model, parent.ID, bindings[0].ID, bindings[1].ID)
	m.RegisterExecutor(schedulerTestExecutor{provider: ge.Provider})
	m.RegisterExecutor(schedulerTestExecutor{provider: ae.Provider})
	for _, binding := range bindings {
		if !m.ObserveAntigravityEnterpriseQuota(binding, aeQuota(*now, 1, nil)) {
			t.Fatal("weekly quota observation rejected")
		}
	}
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		chosen, _, _, err := m.pickNextMixed(t.Context(), []string{ge.Provider, ae.Provider}, model, exec.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[chosen.ID]++
		if i%5 == 0 {
			m.scheduler.rebuild(m.snapshotAuths())
		}
	}
	if counts[first.ID] != 70 || counts[second.ID] != 30 || counts[bindings[0].ID] != 50 || counts[bindings[1].ID] != 50 || counts[parent.ID] != 0 {
		t.Fatalf("mixed providers lost daily weighting or weekly round robin: %v", counts)
	}
}

func TestEnterpriseProvidersDailyAndWeeklyQuotaRemainIsolated(t *testing.T) {
	m, _, now := balanceTestManager(t)
	*now = time.Now().UTC()
	m.antigravityEnterprise.now = func() time.Time { return *now }
	daily := sharedQuotaAuth(t, m, "isolated-daily", "project-a", "us", "enterprise_plus", "daily")
	parent := aeRegister(t, m, aeAccountFixture("isolated-weekly", aeLicense("project-a", "us")))
	weekly := m.AntigravityEnterpriseBindings(parent.ID)[0]
	model := "gemini-3.8-flash"
	registerSchedulerModels(t, ge.Provider, model, daily.ID)
	registerSchedulerModels(t, ae.Provider, model, weekly.ID)
	m.RegisterExecutor(schedulerTestExecutor{provider: ge.Provider})
	m.RegisterExecutor(schedulerTestExecutor{provider: ae.Provider})
	pick := func(want string) {
		t.Helper()
		chosen, _, _, err := m.pickNextMixed(t.Context(), []string{ge.Provider, ae.Provider}, model, exec.Options{}, nil)
		if err != nil || chosen == nil || chosen.ID != want {
			t.Fatalf("wanted %s, got %v, error %v", want, chosen, err)
		}
	}
	observeBalance(t, m, daily, *now, 0)
	if !m.ObserveAntigravityEnterpriseQuota(weekly, aeQuota(*now, 1, nil)) {
		t.Fatal("weekly quota observation rejected")
	}
	pick(weekly.ID)
	*now = now.Add(time.Nanosecond)
	observeBalance(t, m, daily, *now, 100)
	if !m.ObserveAntigravityEnterpriseQuota(weekly, aeQuota(*now, 0, nil)) {
		t.Fatal("exhausted weekly observation rejected")
	}
	pick(daily.ID)
	if err := m.CheckAntigravityEnterpriseQuota(weekly, *now); err == nil {
		t.Fatal("daily replenishment reopened the weekly quota")
	}
	ticket, err := daily.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	m.Remove(t.Context(), parent.ID)
	ticket.finish(nil)
	view := m.EnterpriseQuotaBalance(daily)
	if view.InFlight != 0 || view.LocalUnreported != 1 || view.EffectiveRemaining == nil || *view.EffectiveRemaining != 99 {
		t.Fatalf("removing a weekly account corrupted daily accounting: %+v", view)
	}
	pick(daily.ID)
}

type enterpriseIntegrationBlockingExecutor struct {
	*balanceBlockingExecutor
	provider string
}

func (e *enterpriseIntegrationBlockingExecutor) Identifier() string { return e.provider }

func TestEnterpriseProvidersWeeklyRequestReleasesDailyAdmission(t *testing.T) {
	m, _, now := balanceTestManager(t)
	*now = time.Now().UTC()
	daily := sharedQuotaAuth(t, m, "admission-daily", "111", "us", "enterprise_plus", "daily")
	observeBalance(t, m, daily, *now, 100)
	parent := aeRegister(t, m, aeAccountFixture("admission-weekly", aeLicense("project-a", "us")))
	weekly := m.AntigravityEnterpriseBindings(parent.ID)[0]
	model := "gemini-3.8-flash"
	registerSchedulerModels(t, ge.Provider, model, daily.ID)
	registerSchedulerModels(t, ae.Provider, model, weekly.ID)
	started := make(chan string, 2)
	release := make(chan struct{})
	defer close(release)
	for _, provider := range []string{ge.Provider, ae.Provider} {
		m.RegisterExecutor(&enterpriseIntegrationBlockingExecutor{
			balanceBlockingExecutor: &balanceBlockingExecutor{started: started, release: release},
			provider:                provider,
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 2)
	for _, auth := range []*Auth{weekly, daily} {
		go func() {
			_, err := m.Execute(ctx, []string{ge.Provider, ae.Provider}, exec.Request{Model: model}, exec.Options{
				SourceFormat: "gemini",
				Metadata: map[string]any{
					exec.PinnedAuthMetadataKey: auth.ID,
					exec.OperationMetadataKey:  "generate_content",
				},
			})
			results <- err
		}()
		select {
		case id := <-started:
			if id != auth.ID {
				t.Fatalf("pinned request selected %s instead of %s", id, auth.ID)
			}
		case err := <-results:
			t.Fatalf("%s request ended before its release: %v", auth.Provider, err)
		case <-time.After(5 * time.Second):
			t.Fatal("weekly request retained the daily admission turn")
		}
	}
	if view := m.EnterpriseQuotaBalance(daily); view.InFlight != 1 {
		t.Fatalf("daily accounting included the weekly request: %+v", view)
	}
	cancel()
	for i := 0; i < 2; i++ {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("expected request cancellation, got %v", err)
		}
	}
	if view := m.EnterpriseQuotaBalance(daily); view.InFlight != 0 {
		t.Fatalf("cancellation leaked the daily reservation: %+v", view)
	}
}
