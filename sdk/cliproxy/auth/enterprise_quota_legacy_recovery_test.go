package auth

import (
	"errors"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func restoreLegacyDailyQuota(t *testing.T, manager *Manager, auth *Auth, state SharedQuotaState) string {
	t.Helper()
	state.Key = auth.enterpriseQuota.keys()["edition"]
	state.PoolID = quotaDigest(state.Key)
	state.Version = 1
	state.Restriction.Kind = "daily"
	state.Restriction.Scope = "edition"
	state.Restriction.Feature = "assistant_search"
	state.Restriction.Edition = auth.EnterpriseQuotaIdentity.Edition
	if state.Phase == "" {
		state.Phase = "cooling"
	}
	store := NewFileCooldownStateStore(t.TempDir())
	if errSave := store.SaveSharedQuota(t.Context(), SharedQuotaSnapshot{
		SchemaVersion: 1, Version: 1, Pools: []SharedQuotaState{state},
	}); errSave != nil {
		t.Fatal(errSave)
	}
	if errRestore := manager.restoreSharedQuota(t.Context(), store); errRestore != nil {
		t.Fatal(errRestore)
	}
	return state.PoolID
}

func legacyDailyQuotaState(manager *Manager, poolID string) SharedQuotaState {
	manager.enterpriseQuota.mu.Lock()
	defer manager.enterpriseQuota.mu.Unlock()
	return *manager.enterpriseQuota.pools[poolID]
}

func assertLegacyDailyQuotaWaiting(t *testing.T, auth *Auth) {
	t.Helper()
	_, err := auth.enterpriseQuota.begin()
	var gate *enterpriseLocalGateError
	if !errors.As(err, &gate) {
		t.Fatalf("restored quota dispatched before the recovery window: %v", err)
	}
}

func TestLegacyPaidDailyQuotaRestoreAlignsBeforeBeijingRecovery(t *testing.T) {
	for _, test := range []struct {
		name                   string
		now, anchor, retryAt   time.Time
		lastRejected, explicit time.Time
		source, phase          string
	}{
		{
			name:    "winter estimated Pacific midnight",
			now:     time.Date(2026, 11, 17, 8, 1, 0, 0, time.UTC),
			anchor:  time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC),
			retryAt: time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC),
			source:  "estimated_pacific_midnight",
		},
		{
			name:         "summer short Google deadline in stored quota day",
			now:          time.Date(2026, 7, 17, 8, 1, 0, 0, time.UTC),
			anchor:       time.Date(2026, 7, 18, 7, 0, 0, 0, time.UTC),
			retryAt:      time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC),
			lastRejected: time.Date(2026, 7, 17, 7, 59, 0, 0, time.UTC),
			source:       "google",
		},
		{
			name:         "summer automatic pending from older restore",
			now:          time.Date(2026, 7, 17, 8, 1, 0, 0, time.UTC),
			anchor:       time.Date(2026, 7, 17, 7, 0, 0, 0, time.UTC),
			retryAt:      time.Date(2026, 7, 17, 7, 0, 0, 0, time.UTC),
			lastRejected: time.Date(2026, 7, 17, 6, 59, 0, 0, time.UTC),
			source:       "estimated_pacific_midnight", phase: "probe_pending",
		},
		{
			name:         "explicit reset precedes rejection day fallback",
			now:          time.Date(2026, 11, 17, 8, 1, 0, 0, time.UTC),
			anchor:       time.Date(2026, 11, 17, 8, 0, 30, 0, time.UTC),
			retryAt:      time.Date(2026, 11, 17, 8, 0, 45, 0, time.UTC),
			lastRejected: time.Date(2026, 11, 17, 8, 0, 15, 0, time.UTC),
			explicit:     time.Date(2026, 11, 17, 8, 0, 30, 0, time.UTC),
			source:       "google",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, now := quotaPressureTestManager()
			*now = test.now
			auth := quotaPressureAuth(t, manager, "legacy-recovery", "12345", "us", true)
			id := restoreLegacyDailyQuota(t, manager, auth, SharedQuotaState{
				Phase: test.phase, LastRejectedAt: test.lastRejected,
				Restriction: ge.Restriction{RetryAt: test.retryAt, ResetAt: test.explicit, TimeSource: test.source},
			})
			readyAt := enterprisePaidDailyRecoveryAt(test.anchor, id)
			state := legacyDailyQuotaState(manager, id)
			if state.Version != 2 || state.Phase != "cooling" ||
				!state.Restriction.RetryAt.Equal(readyAt) || !state.Restriction.ResetAt.Equal(test.anchor) ||
				state.Restriction.TimeSource != test.source {
				t.Fatalf("legacy cooldown was not aligned: %+v", state)
			}
			assertLegacyDailyQuotaWaiting(t, auth)
			*now = readyAt
			ticket, err := auth.enterpriseQuota.begin()
			if err != nil || len(ticket.probes) != 1 {
				t.Fatalf("could not acquire the recovery probe at %s: %v", readyAt, err)
			}
			ticket.finish(nil)
		})
	}
}

func TestLegacyPaidDailyQuotaRestorePreservesElapsedAndManualCooldowns(t *testing.T) {
	for _, test := range []struct {
		name        string
		retryOffset time.Duration
		nowOffset   time.Duration
		phase       string
		resetEpoch  uint64
		wantPhase   string
	}{
		{name: "elapsed window", nowOffset: 6 * time.Minute, wantPhase: "probe_pending"},
		{name: "manual early probe", nowOffset: time.Minute, phase: "probe_pending", resetEpoch: 1, wantPhase: "probe_pending"},
		{name: "later Google deadline", retryOffset: 10 * time.Minute, nowOffset: time.Minute, wantPhase: "cooling"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, now := quotaPressureTestManager()
			reset := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
			*now = reset.Add(test.nowOffset)
			auth := quotaPressureAuth(t, manager, "legacy-recovery", "12345", "us", true)
			retryAt := reset.Add(test.retryOffset)
			id := restoreLegacyDailyQuota(t, manager, auth, SharedQuotaState{
				Phase: test.phase, ResetEpoch: test.resetEpoch,
				Restriction: ge.Restriction{RetryAt: retryAt, TimeSource: "google"},
			})
			state := legacyDailyQuotaState(manager, id)
			if state.Version != 1 || state.ResetEpoch != test.resetEpoch ||
				!state.Restriction.RetryAt.Equal(retryAt) || quotaPhase(&state, *now) != test.wantPhase {
				t.Fatalf("valid legacy cooldown changed on restore: %+v", state)
			}
			if test.wantPhase != "probe_pending" {
				assertLegacyDailyQuotaWaiting(t, auth)
				return
			}
			ticket, err := auth.enterpriseQuota.begin()
			if err != nil || len(ticket.probes) != 1 {
				t.Fatalf("the explicit or elapsed cooldown did not allow a probe: %v", err)
			}
			if test.name == "elapsed window" {
				// The in-flight probe keeps paid recovery semantics even after
				// the original license verification expires.
				*now = now.Add(enterpriseSubscriptionMaxStale)
				rejection := quotaRejection(*now, "edition", auth.EnterpriseQuotaIdentity.Edition)
				upstream := rejection.(*ge.GenerationError)
				upstream.Restriction.TimeSource = "estimated_pacific_midnight"
				upstream.Restriction.RetryAt = ge.NextPacificMidnight(*now)
				ticket.finish(upstream)
				state = legacyDailyQuotaState(manager, id)
				if !state.Restriction.ResetAt.Equal(reset) || !state.Restriction.RetryAt.Equal(now.Add(enterpriseQuotaPressureWindow)) {
					t.Fatalf("a late old-day probe waited for another quota day: %+v", state)
				}
			} else {
				ticket.finish(nil)
			}
		})
	}
}

func TestLegacyDailyQuotaWaitsForAnEnabledConfirmedPaidMember(t *testing.T) {
	manager, now := quotaPressureTestManager()
	reset := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	*now = reset.Add(time.Minute)
	auth := quotaPressureAuth(t, manager, "legacy-late-paid", "12345", "us", false)
	id := restoreLegacyDailyQuota(t, manager, auth, SharedQuotaState{
		Restriction: ge.Restriction{RetryAt: reset, TimeSource: "estimated_pacific_midnight"},
	})
	if state := legacyDailyQuotaState(manager, id); state.Version != 1 || quotaPhase(&state, *now) != "probe_pending" {
		t.Fatalf("unconfirmed paid pool was migrated: %+v", state)
	}

	disabled := auth.Clone()
	disabled.Disabled = true
	auth, errUpdate := manager.Update(t.Context(), disabled)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	manager.InvalidateEnterpriseSubscription(auth)
	paid := false
	flight := manager.BeginEnterpriseSubscriptionRefresh(auth)
	identity := auth.EnterpriseQuotaIdentity
	if flight == nil || identity == nil || !flight.Complete(ge.SubscriptionObservation{
		Status: "ok", State: "ACTIVE", Edition: identity.Edition, FreeTrial: &paid,
		ProjectNumber: identity.ProjectNumber, PrincipalHash: identity.PrincipalHash, CheckedAt: *now,
	}) {
		t.Fatal("could not publish verified paid subscription")
	}
	manager.PublishEnterpriseSubscriptionIdentity(auth)
	current, _ := manager.GetByID(auth.ID)
	if current.EnterpriseQuotaIdentity == nil || !current.EnterpriseQuotaIdentity.Paid {
		t.Fatal("paid fixture was not verified")
	}
	if state := legacyDailyQuotaState(manager, id); state.Version != 1 {
		t.Fatalf("disabled credential migrated the pool: %+v", state)
	}
	current.Disabled, current.Status = false, StatusActive
	active, errActivate := manager.Update(t.Context(), current)
	if errActivate != nil {
		t.Fatal(errActivate)
	}
	if state := legacyDailyQuotaState(manager, id); state.Version != 2 || !state.Restriction.RetryAt.After(*now) {
		t.Fatalf("paid verification did not align the restored cooldown: %+v", state)
	}
	assertLegacyDailyQuotaWaiting(t, active)
}

func TestLegacyDailyQuotaGuardsAdmissionDuringLicensePublication(t *testing.T) {
	manager, now := quotaPressureTestManager()
	reset := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	*now = reset.Add(time.Minute)
	// The old persisted binding has identity attribution with no transient
	// funding proof. Publish the policy cache before updating that binding.
	auth := sharedQuotaAuth(t, manager, "legacy-license-publication", "12345", "us", "enterprise_plus", "member")
	id := restoreLegacyDailyQuota(t, manager, auth, SharedQuotaState{
		Restriction: ge.Restriction{RetryAt: reset, TimeSource: "estimated_pacific_midnight"},
	})
	manager.InvalidateEnterpriseSubscription(auth)
	trial := false
	flight := manager.BeginEnterpriseSubscriptionRefresh(auth)
	identity := auth.EnterpriseQuotaIdentity
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{
		Status: "ok", State: "ACTIVE", Edition: identity.Edition, FreeTrial: &trial,
		ProjectNumber: identity.ProjectNumber, PrincipalHash: identity.PrincipalHash, CheckedAt: *now,
	}) {
		t.Fatal("could not verify paid subscription")
	}
	if !manager.EnterpriseSubscriptionSnapshot(auth).ConfirmedPaid() || auth.EnterpriseQuotaIdentity.Paid {
		t.Fatal("fixture did not stage paid verification ahead of attribution")
	}
	assertLegacyDailyQuotaWaiting(t, auth)
	manager.PublishEnterpriseSubscriptionIdentity(auth)
	current, _ := manager.GetByID(auth.ID)
	assertLegacyDailyQuotaWaiting(t, current)
	*now = enterprisePaidDailyRecoveryAt(reset, id)
	ticket, err := current.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err != nil || !ticket.paidRecovery {
		t.Fatalf("verified paid credential could not probe in the scheduled window: %v", err)
	}
	ticket.finish(nil)
}

func TestLegacyDailyQuotaPreservesVerifiedTrialTimer(t *testing.T) {
	manager, now := quotaPressureTestManager()
	reset := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	*now = reset.Add(time.Minute)
	auth := quotaPressureAuth(t, manager, "legacy-confirmed-trial", "12345", "us", false)
	quotaPressureAuth(t, manager, "legacy-confirmed-trial-peer", "named-project", "us", false)
	sharedQuotaAuth(t, manager, "legacy-unrelated-unverified", "12345", "eu", "enterprise_plus", "other-region")
	restoreLegacyDailyQuota(t, manager, auth, SharedQuotaState{
		Restriction: ge.Restriction{RetryAt: reset, TimeSource: "estimated_pacific_midnight"},
	})
	ticket, err := auth.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err != nil || len(ticket.probes) != 1 || ticket.paidRecovery {
		t.Fatalf("paid recovery schedule altered the confirmed trial's saved timer: %v", err)
	}
	ticket.finish(nil)
}

func TestLegacyDailyQuotaTrialCannotClearSharedPoolBeforePaidPublication(t *testing.T) {
	for _, situation := range []string{"unverified sibling", "disabled paid sibling", "stale trial sibling"} {
		t.Run(situation, func(t *testing.T) {
			manager, now := quotaPressureTestManager()
			reset := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
			*now = reset.Add(time.Minute)
			trial := quotaPressureAuth(t, manager, "legacy-trial-member", "12345", "us", false)

			switch situation {
			case "unverified sibling":
				// A restored edition binding can exist before the live license
				// observation is published to it. It proves pool membership only.
				sharedQuotaAuth(t, manager, "legacy-unknown-member", "named-project", "us", "enterprise_plus", "unknown")
			case "disabled paid sibling":
				paid := quotaPressureAuth(t, manager, "legacy-disabled-paid", "named-project", "us", true)
				paid.Disabled = true
				if _, errUpdate := manager.Update(t.Context(), paid); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			case "stale trial sibling":
				current := *now
				*now = now.Add(-enterpriseSubscriptionMaxStale)
				quotaPressureAuth(t, manager, "legacy-stale-trial", "named-project", "us", false)
				*now = current
			}

			id := restoreLegacyDailyQuota(t, manager, trial, SharedQuotaState{
				Restriction: ge.Restriction{RetryAt: reset, TimeSource: "estimated_pacific_midnight"},
			})
			readyAt := enterprisePaidDailyRecoveryAt(reset, id)
			saved := legacyDailyQuotaState(manager, id)
			if !saved.legacyReadyAt.Equal(readyAt) || !saved.Restriction.RetryAt.Equal(reset) {
				t.Fatalf("fixture did not exercise the old, unaligned cooldown: %+v", saved)
			}
			if blocked, retryAt := trial.enterpriseQuota.blocked(*now); !blocked || !retryAt.Equal(readyAt) {
				t.Fatalf("scheduler did not protect the shared recovery window: %v, %s", blocked, retryAt)
			}
			assertLegacyDailyQuotaWaiting(t, trial)

			// An explicit reset still permits a single probe before the window.
			if _, errReset := manager.ResetSharedQuota(t.Context(), trial.ID, id, saved.Version); errReset != nil {
				t.Fatal(errReset)
			}
			ticket, errBegin := trial.enterpriseQuota.beginForModel(quotaPressureTestModel)
			if errBegin != nil || len(ticket.probes) != 1 {
				t.Fatalf("manual early probe was blocked: %v", errBegin)
			}
			ticket.finish(nil)
		})
	}
}

func TestLegacyDailyQuotaTrialProbeCannotClearLatePaidMemberWait(t *testing.T) {
	manager, now := quotaPressureTestManager()
	reset := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	*now = reset.Add(time.Minute)
	trial := quotaPressureAuth(t, manager, "legacy-probing-trial", "12345", "us", false)
	id := restoreLegacyDailyQuota(t, manager, trial, SharedQuotaState{
		Restriction: ge.Restriction{RetryAt: reset, TimeSource: "estimated_pacific_midnight"},
	})
	readyAt := enterprisePaidDailyRecoveryAt(reset, id)
	probe, errBegin := trial.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if errBegin != nil || len(probe.probes) != 1 || probe.paidRecovery {
		t.Fatalf("pure trial pool did not acquire its old-date probe: %v", errBegin)
	}

	paid := quotaPressureAuth(t, manager, "legacy-late-paid-member", "named-project", "us", true)
	assertLegacyDailyQuotaWaiting(t, paid)
	probe.finish(nil)
	if blocked, retryAt := paid.enterpriseQuota.blocked(*now); !blocked || !retryAt.Equal(readyAt) {
		t.Fatalf("successful trial probe cleared a newly confirmed paid member's wait: %v, retry at %s", blocked, retryAt)
	}
	assertLegacyDailyQuotaWaiting(t, paid)

	*now = readyAt
	recovery, errRecovery := paid.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if errRecovery != nil || len(recovery.probes) != 1 || !recovery.paidRecovery {
		t.Fatalf("paid credential could not claim its scheduled recovery probe: %v", errRecovery)
	}
	recovery.finish(nil)
	if state := legacyDailyQuotaState(manager, id); state.Phase != "normal" {
		t.Fatalf("successful scheduled probe did not clear the shared restriction: %+v", state)
	}
}

func TestLegacyPaidDailyQuotaPreservesProbePolicyAcrossEditionMinuteLimit(t *testing.T) {
	manager, now := quotaPressureTestManager()
	reset := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	*now = reset.Add(time.Minute)
	auth := quotaPressureAuth(t, manager, "legacy-paid-minute-transition", "12345", "us", true)
	id := restoreLegacyDailyQuota(t, manager, auth, SharedQuotaState{
		Restriction: ge.Restriction{RetryAt: reset, TimeSource: "estimated_pacific_midnight"},
	})
	state := legacyDailyQuotaState(manager, id)
	if !state.PaidRecovery {
		t.Fatalf("confirmed paid legacy restriction was not marked: %+v", state)
	}

	*now = state.Restriction.RetryAt
	ticket, errBegin := auth.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if errBegin != nil || !ticket.paidRecovery {
		t.Fatalf("could not claim paid probe: %v", errBegin)
	}
	rejection := quotaRejection(*now, "edition", auth.EnterpriseQuotaIdentity.Edition).(*ge.GenerationError)
	rejection.Restriction.Kind = "minute"
	rejection.Restriction.RetryAt = now.Add(time.Minute)
	ticket.finish(rejection)
	state = legacyDailyQuotaState(manager, id)
	if state.Restriction.Kind != "minute" || !state.PaidRecovery {
		t.Fatalf("same-pool minute limit lost paid recovery: %+v", state)
	}

	*now = now.Add(enterpriseSubscriptionMaxStale)
	if auth.enterpriseQuota.paidQuotaKey(*now) != "" {
		t.Fatal("paid verification did not expire during the fixture")
	}
	ticket, errBegin = auth.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if errBegin != nil || !ticket.paidRecovery {
		t.Fatalf("minute recovery lost the paid policy when verification expired: %v", errBegin)
	}
	ticket.finish(nil)
}
