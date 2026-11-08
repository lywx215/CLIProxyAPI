package auth

import (
	"errors"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestAntigravityEnterpriseMetadataRefreshDoesNotBecomeModelCooldown(t *testing.T) {
	for _, kind := range []string{"stale", "reset", "unknown", "overage", "missing"} {
		t.Run(kind, func(t *testing.T) {
			m := NewManager(nil, &RoundRobinSelector{}, nil)
			parent := aeRegister(t, m, aeAccountFixture("metadata-"+kind, aeLicense("metadata-project", "global")))
			child := m.AntigravityEnterpriseBindings(parent.ID)[0]
			now := time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)
			m.antigravityEnterprise.now = func() time.Time { return now }
			quota := aeQuota(now, 1, nil)
			wantCode := "metadata_pending"
			switch kind {
			case "reset":
				reset := now.Add(time.Minute)
				quota = aeQuota(now, 1, &reset)
			case "unknown":
				quota.Groups = nil
				wantCode = "quota_unknown"
			case "overage":
				quota.Overage.Enabled = true
				wantCode = "subscription_only_required"
			}
			if kind != "missing" && !m.ObserveAntigravityEnterpriseQuota(child, quota) {
				t.Fatal("initial observation rejected")
			}
			if kind == "stale" {
				now = now.Add(ae.QuotaTTL + time.Nanosecond)
			} else if kind == "reset" {
				now = now.Add(time.Minute)
			}
			if blocked, _, _ := isAuthBlockedForModel(child, "metadata-model", now); blocked {
				t.Fatal("metadata preflight was replaced by a model cooldown")
			}
			if blocked := isCredentialBlocked(child, 1, now); blocked {
				t.Fatal("scheduler treated metadata preflight as a credential cooldown")
			}
			var failure *ae.Error
			if err := m.CheckAntigravityEnterpriseQuota(child, now); !errors.As(err, &failure) || failure.Code != wantCode {
				t.Fatalf("expected %s preflight error, got %v", wantCode, err)
			}
			if views := CooldownSnapshotForAuth(child, now); len(views) != 0 {
				t.Fatalf("metadata preflight created cooldown views: %+v", views)
			}

			model := "metadata-model-" + kind
			registerSchedulerModels(t, ae.Provider, model, child.ID)
			m.RegisterExecutor(schedulerTestExecutor{provider: ae.Provider})
			m.RefreshSchedulerEntry(child.ID)
			selected, _, err := m.pickNextLegacy(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
			if err != nil || selected == nil || selected.ID != child.ID {
				t.Fatalf("legacy selection could not reach metadata preflight: selected=%v err=%v", selected, err)
			}
			selected, err = m.scheduler.pickSingle(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
			if err != nil || selected == nil || selected.ID != child.ID {
				t.Fatalf("scheduler selection could not reach metadata preflight: selected=%v err=%v", selected, err)
			}
		})
	}
}

func TestAntigravityEnterpriseExhaustionRemainsAuthoritativeUntilFreshObservation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parent := aeRegister(t, m, aeAccountFixture("exhaustion-refresh", aeLicense("metadata-project", "global")))
	child := m.AntigravityEnterpriseBindings(parent.ID)[0]
	now := time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)
	m.antigravityEnterprise.now = func() time.Time { return now }
	reset := now.Add(time.Hour)
	if !m.ObserveAntigravityEnterpriseQuota(child, aeQuota(now, 0, &reset)) {
		t.Fatal("exhausted observation rejected")
	}
	now = now.Add(ae.QuotaTTL + time.Nanosecond)
	if blocked, _, _ := isAuthBlockedForModel(child, "model", now); !blocked {
		t.Fatal("an observation expiring replenished an exhausted weekly pool")
	}
	if err := m.CheckAntigravityEnterpriseQuota(child, now); err == nil {
		t.Fatal("expired exhausted observation admitted generation")
	}
	now = reset
	if blocked, _, _ := isAuthBlockedForModel(child, "model", now); blocked {
		t.Fatal("reset boundary prevented a fresh metadata preflight")
	}
	if err := m.CheckAntigravityEnterpriseQuota(child, now); err == nil {
		t.Fatal("reset boundary invented quota before a fresh observation")
	}
	m.RestrictAntigravityEnterpriseQuota(child, &ae.Error{HTTP: 429, Code: "weekly_quota", RetryAt: reset})
	if blocked, _, _ := isAuthBlockedForModel(child, "model", now); !blocked {
		t.Fatal("an explicit upstream weekly restriction was bypassed")
	}
	now = now.Add(time.Nanosecond)
	if !m.ObserveAntigravityEnterpriseQuota(child, aeQuota(now, 1, nil)) {
		t.Fatal("fresh replenished observation rejected")
	}
	if err := m.CheckAntigravityEnterpriseQuota(child, now); err != nil {
		t.Fatalf("fresh metadata did not restore dispatch: %v", err)
	}
}
