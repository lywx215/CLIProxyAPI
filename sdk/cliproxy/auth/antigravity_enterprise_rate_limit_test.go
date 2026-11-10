package auth

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func aeRateFixture(t *testing.T) (*Manager, *Auth, []*Auth, *time.Time) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	parent := aeRegister(t, m, aeAccountFixture("rate-limit", aeLicense("project-a", "global"), aeLicense("project-a", "us"), aeLicense("project-b", "global")))
	now := time.Now().UTC().Truncate(time.Second)
	m.antigravityEnterprise.now = func() time.Time { return now }
	children := m.AntigravityEnterpriseBindings(parent.ID)
	for _, child := range children {
		m.ObserveAntigravityEnterpriseQuota(child, aeQuota(now, 1, nil))
	}
	return m, parent, children, &now
}

func aeRateFailure(deadline time.Time) *ae.Error {
	return &ae.Error{HTTP: 429, Attempted: true, RetryHint: true, RetryAt: deadline, Code: "upstream_error"}
}

func TestAntigravityEnterpriseRateLimitMatching(t *testing.T) {
	for _, difference := range []time.Duration{0, -5 * time.Second, 5 * time.Second, 5*time.Second + time.Nanosecond, -5*time.Second - time.Nanosecond} {
		t.Run(difference.String(), func(t *testing.T) {
			m, _, children, now := aeRateFixture(t)
			first := now.Add(time.Hour)
			second := first.Add(difference)
			if got := m.ObserveAntigravityEnterpriseRateLimit(children[0], "first", aeRateFailure(first)); !got.IsZero() {
				t.Fatal("one response restricted the binding")
			}
			got := m.ObserveAntigravityEnterpriseRateLimit(children[0], "second", aeRateFailure(second))
			matched := difference >= -5*time.Second && difference <= 5*time.Second
			if matched == got.IsZero() {
				t.Fatalf("incorrect matching result %v", got)
			}
			if matched && (!got.Equal(first) && !got.Equal(second) || got.Before(first) || got.Before(second)) {
				t.Fatal("did not use the later deadline")
			}
		})
	}
}

func TestAntigravityEnterpriseRateLimitRejectsUnverifiedEvidence(t *testing.T) {
	m, _, children, now := aeRateFixture(t)
	deadline := now.Add(time.Hour)
	for _, change := range []func(*ae.Error){
		func(e *ae.Error) { e.Attempted = false },
		func(e *ae.Error) { e.RetryHint = false },
		func(e *ae.Error) { e.HTTP = 503 },
		func(e *ae.Error) { e.RetryAt = *now },
		func(e *ae.Error) { e.RetryAt = time.Time{} },
	} {
		failure := aeRateFailure(deadline)
		change(failure)
		for _, id := range []string{"one", "two"} {
			if !m.ObserveAntigravityEnterpriseRateLimit(children[0], id, failure).IsZero() {
				t.Fatal("unverified evidence restricted a binding")
			}
		}
	}
	if len(m.antigravityEnterpriseQuotaSnapshot().RateLimits) != 0 {
		t.Fatal("unverified evidence was persisted")
	}
}

func TestAntigravityEnterpriseRateLimitConcurrentDeduplicationAndWindows(t *testing.T) {
	m, _, children, now := aeRateFixture(t)
	deadline := now.Add(time.Hour)
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Go(func() { m.ObserveAntigravityEnterpriseRateLimit(children[0], "same-attempt", aeRateFailure(deadline)) })
	}
	group.Wait()
	if blocked, _ := children[0].antigravityEnterpriseBlocked(*now); blocked {
		t.Fatal("duplicate callbacks counted as independent attempts")
	}
	matched := deadline.Add(4 * time.Second)
	if got := m.ObserveAntigravityEnterpriseRateLimit(children[0], "second", aeRateFailure(matched)); !got.Equal(matched) {
		t.Fatal("independent attempt did not trigger binding cooldown")
	}
	if got := m.ObserveAntigravityEnterpriseRateLimit(children[0], "third", aeRateFailure(deadline.Add(8*time.Second))); !got.Equal(matched) {
		t.Fatal("a consumed observation extended the window transitively")
	}
	for i := 0; i < 80; i++ {
		m.ObserveAntigravityEnterpriseRateLimit(children[1], fmt.Sprint(i), aeRateFailure(deadline.Add(time.Duration(i)*10*time.Second)))
	}
	if len(m.antigravityEnterprise.rateLimits[children[1].ID].Observations) != 64 {
		t.Fatal("history is not capped at 64")
	}
}

func TestAntigravityEnterpriseRateLimitIsolationAndRecovery(t *testing.T) {
	m, parent, children, now := aeRateFixture(t)
	deadline := now.Add(time.Hour)
	for _, id := range []string{"first", "second"} {
		m.ObserveAntigravityEnterpriseRateLimit(children[0], id, aeRateFailure(deadline))
	}
	for _, child := range children[1:] {
		if err := m.CheckAntigravityEnterpriseQuota(child, *now); err != nil {
			t.Fatalf("restriction leaked to sibling: %v", err)
		}
	}
	if len(CooldownSnapshotForAuth(parent, *now)) != 0 {
		t.Fatal("restriction leaked to the account")
	}
	m.MarkResult(t.Context(), Result{AuthID: children[0].ID, Provider: ae.Provider, Model: "late-success", Success: true})
	*now = now.Add(time.Second)
	m.ObserveAntigravityEnterpriseQuota(children[0], aeQuota(*now, 1, nil))
	var failure *ae.Error
	if err := m.CheckAntigravityEnterpriseQuota(children[0], *now); !errors.As(err, &failure) || failure.Code != "binding_rate_limit" {
		t.Fatalf("success or positive quota erased restriction: %v", err)
	}
	view := CooldownSnapshotForAuth(children[0], *now)
	if len(view) != 1 || view[0].Scope != "credential" || view[0].RestrictionScope != "binding" || view[0].Reason != "binding_rate_limit" || view[0].HTTPStatus != 429 || !view[0].RetryAt.Equal(deadline) {
		t.Fatalf("incorrect cooldown projection: %+v", view)
	}
	m.RegisterExecutor(schedulerTestExecutor{provider: ae.Provider})
	for _, child := range children {
		registry.GetGlobalRegistry().RegisterClient(child.ID, ae.Provider, []*registry.ModelInfo{{ID: "rate-test-one"}, {ID: "rate-test-two"}})
		m.RefreshSchedulerEntry(child.ID)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(child.ID) })
	}
	for _, model := range []string{"rate-test-one", "rate-test-two"} {
		for _, fast := range []bool{false, true} {
			var picked *Auth
			var err error
			if fast {
				picked, err = m.scheduler.pickSingle(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
			} else {
				picked, _, err = m.pickNextLegacy(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
			}
			if err != nil || picked.ID == children[0].ID {
				t.Fatalf("scheduler fast=%v selected restricted binding: %v", fast, err)
			}
		}
	}
	*now = deadline
	if blocked, _ := children[0].antigravityEnterpriseBlocked(*now); blocked {
		t.Fatal("expired binding rate limit still blocked routing")
	}
	if len(m.antigravityEnterpriseQuotaSnapshot().RateLimits) != 0 {
		t.Fatal("expired evidence retained")
	}
}

func TestAntigravityEnterpriseRateLimitPersistenceAndReset(t *testing.T) {
	m, _, children, now := aeRateFixture(t)
	store := NewFileCooldownStateStore(t.TempDir())
	m.SetCooldownStateStore(store)
	deadline := now.Add(time.Hour)
	m.ObserveAntigravityEnterpriseRateLimit(children[0], "first", aeRateFailure(deadline))
	restarted, _, fresh, clock := aeRateFixture(t)
	*clock = *now
	restarted.SetCooldownStateStore(store)
	if err := restarted.RestoreCooldownStates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !restarted.ObserveAntigravityEnterpriseRateLimit(fresh[0], "duplicate-first", aeRateFailure(deadline)).Equal(deadline) {
		t.Fatal("pending evidence did not survive restart")
	}
	loaded, _, next, _ := aeRateFixture(t)
	loaded.SetCooldownStateStore(store)
	if err := loaded.RestoreCooldownStates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := next[0].antigravityEnterpriseBlocked(*now); !blocked {
		t.Fatal("confirmed restriction did not survive restart")
	}
	if _, _, err := loaded.ResetQuota(t.Context(), next[0].ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadAntigravityEnterpriseQuota(t.Context())
	if err != nil || len(snapshot.RateLimits) != 0 {
		t.Fatalf("reset was not persisted: %v", err)
	}
}

func TestAntigravityEnterpriseRateLimitRevisionAndRemoval(t *testing.T) {
	for _, mode := range []string{"binding-revision", "reauthorize", "remove-binding", "remove"} {
		t.Run(mode, func(t *testing.T) {
			m, parent, children, now := aeRateFixture(t)
			store := NewFileCooldownStateStore(t.TempDir())
			m.SetCooldownStateStore(store)
			for _, id := range []string{"first", "second"} {
				m.ObserveAntigravityEnterpriseRateLimit(children[0], id, aeRateFailure(now.Add(time.Hour)))
			}
			switch mode {
			case "binding-revision":
				_, graph, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
				if _, err := m.UpdateAntigravityEnterpriseAccount(t.Context(), parent, graph.Revision, func(a *ae.Account) error { a.Bindings[0].Settings = map[string]any{"note": "changed"}; return nil }); err != nil {
					t.Fatal(err)
				}
			case "reauthorize":
				if _, err := m.ReauthorizeAntigravityEnterpriseAccount(t.Context(), parent); err != nil {
					t.Fatal(err)
				}
			case "remove-binding":
				_, graph, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
				if _, err := m.UpdateAntigravityEnterpriseAccount(t.Context(), parent, graph.Revision, func(a *ae.Account) error { a.Bindings = a.Bindings[1:]; return nil }); err != nil {
					t.Fatal(err)
				}
			case "remove":
				m.Remove(t.Context(), parent.ID)
			}
			if len(m.antigravityEnterpriseQuotaSnapshot().RateLimits) != 0 {
				t.Fatal("old binding evidence survived identity change")
			}
			persisted, err := store.LoadAntigravityEnterpriseQuota(t.Context())
			if err != nil || len(persisted.RateLimits) != 0 {
				t.Fatalf("old binding evidence remained on disk: %v", err)
			}
			if !m.ObserveAntigravityEnterpriseRateLimit(children[0], "late", aeRateFailure(now.Add(time.Hour))).IsZero() {
				t.Fatal("stale in-flight failure recreated restriction")
			}
		})
	}
}

func TestAntigravityEnterpriseRateLimitPreservesOtherRestrictionsAndLegacyState(t *testing.T) {
	m, _, children, now := aeRateFixture(t)
	child := children[0]
	for _, model := range []string{"old-one", "old-two"} {
		wait := time.Hour
		m.MarkResult(t.Context(), Result{AuthID: child.ID, Provider: ae.Provider, Model: model,
			Error: &Error{Code: "antigravity_enterprise_rate_limit_hint", HTTPStatus: 429}, RetryAfter: &wait})
	}
	if len(m.antigravityEnterpriseQuotaSnapshot().RateLimits) != 0 {
		t.Fatal("legacy model records became binding-wide evidence")
	}
	for _, id := range []string{"one", "two"} {
		m.ObserveAntigravityEnterpriseRateLimit(child, id, aeRateFailure(now.Add(time.Minute)))
	}
	m.restrictAntigravityEnterpriseBinding(Result{AuthID: child.ID, Provider: ae.Provider})
	weeklyDeadline := now.Add(24 * time.Hour)
	m.RestrictAntigravityEnterpriseQuota(child, &ae.Error{HTTP: 429, Code: "weekly_quota", RetryAt: weeklyDeadline})
	*now = now.Add(2 * time.Minute)
	var failure *ae.Error
	if err := m.CheckAntigravityEnterpriseQuota(child, *now); !errors.As(err, &failure) || failure.HTTP != 403 {
		t.Fatalf("rate-limit expiry erased permission restriction: %v", err)
	}
	*now = now.Add(time.Hour)
	if err := m.CheckAntigravityEnterpriseQuota(child, *now); !errors.As(err, &failure) || failure.Code != "weekly_quota" {
		t.Fatalf("expiry erased weekly restriction: %v", err)
	}
	snapshot := m.antigravityEnterpriseQuotaSnapshot()
	if err := validateAEQuotaSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.RateLimits = []AntigravityEnterpriseRateLimitState{{AuthID: child.ID, Fingerprint: aeBindingFingerprint(child), Observations: []AntigravityEnterpriseRateLimitObservation{
		{AttemptID: "duplicate", RetryAt: weeklyDeadline}, {AttemptID: "duplicate", RetryAt: weeklyDeadline},
	}}}
	if validateAEQuotaSnapshot(snapshot) == nil {
		t.Fatal("persisted duplicate attempts were accepted")
	}
}
