package cliproxy

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func quotaRefreshTestService(t *testing.T) (*Service, *coreauth.Auth) {
	t.Helper()
	s, a := enterpriseTestService(t, "quota-background")
	s.cfg.Routing.GeminiEnterpriseQuotaBalancing = true
	s.coreManager.SetConfig(s.cfg)
	binding := ge.FromMetadata(a.Metadata)
	a.EnterpriseQuotaIdentity = &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(binding), ProjectNumber: "123", Location: binding.Location, Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64)}
	var err error
	a, err = s.coreManager.Update(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	return s, a
}

func serviceQuotaObservation(a *coreauth.Auth) ge.QuotaSnapshot {
	now := time.Now().UTC()
	zone, _ := time.LoadLocation("America/Los_Angeles")
	local := now.In(zone)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).UTC()
	reset := ge.NextPacificMidnight(now)
	binding := ge.FromMetadata(a.Metadata)
	limit, used, remaining := int64(10000), int64(3000), int64(7000)
	return ge.QuotaSnapshot{Status: "ok", ProjectID: binding.ProjectID, Location: binding.Location, ObservedAt: now, PeriodStart: &start, ResetAt: &reset,
		Pools: []ge.QuotaPool{{Tier: "enterprise_plus", Status: "ok", Limit: &limit, Used: &used, Remaining: &remaining, UsageUpdatedAt: &now}}}
}

func TestEnterpriseQuotaBackgroundRefreshDeduplicatesAndExpires(t *testing.T) {
	s, a := quotaRefreshTestService(t)
	b := a.Clone()
	b.ID = "quota-background-sibling"
	var err error
	b, err = s.coreManager.Register(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	d := s.enterpriseState()
	now := time.Now()
	d.now = func() time.Time { return now }
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	d.fetchQuota = func(ctx context.Context, auth *coreauth.Auth, _ string) ge.QuotaSnapshot {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return serviceQuotaObservation(auth)
	}
	s.queueEnterpriseQuota(t.Context(), a)
	<-entered
	s.queueEnterpriseQuota(t.Context(), b)
	close(release)
	d.wg.Wait()
	view := s.coreManager.EnterpriseQuotaBalance(b)
	if calls.Load() != 1 || view.Status != "ok" || view.Remaining == nil || *view.Remaining != 7000 {
		t.Fatalf("shared background observation missing: calls=%d view=%+v", calls.Load(), view)
	}
	s.queueEnterpriseQuota(t.Context(), b)
	d.wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("fresh pool read more than once")
	}
	now = now.Add(enterpriseQuotaRefreshInterval + time.Second)
	s.queueEnterpriseQuota(t.Context(), b)
	d.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("expired observation was not refreshed")
	}
	s.coreManager.SetConfig(&config.Config{})
	now = now.Add(enterpriseQuotaRefreshInterval + time.Second)
	s.queueEnterpriseQuota(t.Context(), a)
	d.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("disabled balancing still polled Google")
	}
}

func TestEnterpriseQuotaBackgroundRefreshCancellationAndCredentialFence(t *testing.T) {
	for _, change := range []string{"cancel", "credential", "home", "disable"} {
		t.Run(change, func(t *testing.T) {
			s, a := quotaRefreshTestService(t)
			d := s.enterpriseState()
			entered, release := make(chan struct{}), make(chan struct{})
			d.fetchQuota = func(ctx context.Context, auth *coreauth.Auth, _ string) ge.QuotaSnapshot {
				close(entered)
				<-release
				return serviceQuotaObservation(auth)
			}
			s.queueEnterpriseQuota(t.Context(), a)
			<-entered
			switch change {
			case "cancel":
				s.cancelEnterpriseProbes(a.ID)
			case "credential":
				updated := a.Clone()
				updated.Metadata["refresh_token"] = "changed"
				if _, err := s.coreManager.Update(t.Context(), updated); err != nil {
					t.Fatal(err)
				}
			case "home":
				s.coreManager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
			case "disable":
				s.coreManager.SetConfig(&config.Config{})
			}
			close(release)
			d.wg.Wait()
			if view := s.coreManager.EnterpriseQuotaBalance(a); view.Status != "unknown" {
				t.Fatalf("stale worker published after %s: %+v", change, view)
			}
		})
	}
}

func TestEnterpriseQuotaRoutingTogglePreservesFallback(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{Strategy: "weighted-round-robin", GeminiEnterpriseQuotaBalancing: true}}
	s := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	if !s.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: 1}) {
		t.Fatal("could not enable balancing")
	}
	selector, ok := s.coreManager.Selector().(*coreauth.QuotaBalancedSelector)
	if !ok {
		t.Fatalf("enabled selector = %T", s.coreManager.Selector())
	}
	if _, ok := selector.Fallback.(*coreauth.WeightedRoundRobinSelector); !ok {
		t.Fatal("configured fallback was lost")
	}
	disabled := cfg.CloneForRuntime()
	disabled.Routing.GeminiEnterpriseQuotaBalancing = false
	if !s.applyManagerConfig(context.Background(), configCommit{cfg: disabled, sequence: 2}) {
		t.Fatal("could not disable balancing")
	}
	if _, ok := s.coreManager.Selector().(*coreauth.WeightedRoundRobinSelector); !ok {
		t.Fatal("disabled balancing did not restore weighted round robin")
	}
}
