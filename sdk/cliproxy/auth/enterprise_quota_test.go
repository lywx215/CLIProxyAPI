package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func sharedQuotaAuth(t *testing.T, m *Manager, id, project, location, edition, user string) *Auth {
	t.Helper()
	b := config.GeminiEnterpriseOAuth{ClientID: "synthetic-client", ProjectID: project, Location: location, EngineID: "engine"}.WithDefaults()
	a := &Auth{ID: id, Provider: ge.Provider, Status: StatusActive, Metadata: map[string]any{"client_id": b.ClientID, "project_id": project, "location": b.Location, "engine_id": b.EngineID, "refresh_token": "synthetic-" + id}}
	if edition != "" {
		number := "12345"
		if _, err := strconv.ParseUint(project, 10, 64); err == nil {
			number = project
		}
		a.EnterpriseQuotaIdentity = &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(b), ProjectNumber: number, Location: b.Location, Edition: edition, PrincipalHash: quotaDigest(user), CheckedAt: time.Now()}
	}
	saved, err := m.Register(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
func quotaRejection(now time.Time, scope, edition string) error {
	return &ge.GenerationError{HTTPStatus: 429, Restriction: &ge.Restriction{Kind: "daily", Scope: scope, Edition: edition, Feature: "assistant_search", RetryAt: now.Add(time.Hour), TimeSource: "google"}}
}
func rejectSharedQuota(t *testing.T, a *Auth, now time.Time, scope string) {
	t.Helper()
	ticket, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	ticket.finish(quotaRejection(now, scope, "enterprise_plus"))
}

func TestEnterpriseSharedQuotaScopeAndNoFalseFailures(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "source", "my-project", "us", "enterprise_plus", "alice")
	b := sharedQuotaAuth(t, m, "sibling", "12345", "us", "enterprise_plus", "bob")
	otherRegion := sharedQuotaAuth(t, m, "region", "12345", "eu", "enterprise_plus", "alice")
	otherEdition := sharedQuotaAuth(t, m, "edition", "12345", "us", "enterprise_standard", "alice")
	otherProject := sharedQuotaAuth(t, m, "project", "99999", "us", "enterprise_plus", "alice")
	unknown := sharedQuotaAuth(t, m, "unknown", "12345", "us", "", "alice")
	rejectSharedQuota(t, a, now, "edition")
	for _, model := range []string{"flash", "pro", "argon", "new-model", "alias(high)", ""} {
		if blocked, _, _ := isAuthBlockedForModel(b, model, now); !blocked {
			t.Fatalf("sibling bypassed shared gate: %s", model)
		}
	}
	for _, other := range []*Auth{otherRegion, otherEdition, otherProject, unknown} {
		if blocked, _, _ := isAuthBlockedForModel(other, "pro", now); blocked {
			t.Fatalf("unrelated auth blocked: %s", other.ID)
		}
	}
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: ge.Provider, Model: "flash", Error: resultErrorFromError(quotaRejection(now, "edition", "enterprise_plus"))})
	current, _ := m.GetByID(b.ID)
	if current.Failed != 0 || len(current.ModelStates) != 0 || current.Unavailable {
		t.Fatal("shared projection manufactured sibling health failures")
	}
	views := CooldownSnapshotForAuth(current, now)
	if len(views) != 1 || views[0].Scope != "credential" || views[0].RelatedAuthCount != 2 {
		t.Fatalf("invalid backward-compatible projection: %+v", views)
	}
	newMember := sharedQuotaAuth(t, m, "new", "12345", "us", "enterprise_plus", "charlie")
	if blocked, _, _ := isAuthBlockedForModel(newMember, "future-text", now); !blocked {
		t.Fatal("new same-pool member escaped restriction")
	}
}

func TestEnterpriseUserAndUnknownIdentityIsolation(t *testing.T) {
	for _, scope := range []string{"user", "edition"} {
		t.Run(scope, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			now := time.Now()
			edition := "enterprise_plus"
			if scope == "edition" {
				edition = ""
			}
			a := sharedQuotaAuth(t, m, "a", "project", "us", edition, "alice")
			b := sharedQuotaAuth(t, m, "b", "12345", "us", "enterprise_standard", "alice")
			c := sharedQuotaAuth(t, m, "c", "12345", "us", "enterprise_plus", "bob")
			rejectSharedQuota(t, a, now, scope)
			blockedB, _, _ := isAuthBlockedForModel(b, "argon", now)
			blockedC, _, _ := isAuthBlockedForModel(c, "flash", now)
			if blockedC || blockedB != (scope == "user") {
				t.Fatal("restriction escaped its verified identity scope")
			}
		})
	}
}

func TestEnterpriseConcurrentBatchAndSingleRecoveryRequest(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "a", "12345", "us", "enterprise_plus", "alice")
	var tickets []*enterpriseQuotaTicket
	for range 50 {
		ticket, err := a.enterpriseQuota.begin()
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, ticket)
	}
	var wg sync.WaitGroup
	for _, ticket := range tickets {
		wg.Go(func() { ticket.finish(quotaRejection(now, "edition", "enterprise_plus")) })
	}
	wg.Wait()
	views := a.enterpriseQuota.views(now)
	if len(views) != 1 || views[0].PoolVersion != 2 {
		t.Fatalf("batch escalated versions/backoff: %+v", views)
	}
	now = now.Add(2 * time.Hour)
	if views = a.enterpriseQuota.views(now); views[0].Phase != "probe_pending" || views[0].RemainingSeconds != 0 {
		t.Fatal("expiry claimed recovery")
	}
	var admitted atomic.Int32
	leases := make(chan *enterpriseQuotaTicket, 50)
	for range 50 {
		wg.Go(func() {
			ticket, err := a.enterpriseQuota.begin()
			if err == nil {
				admitted.Add(1)
				leases <- ticket
			}
		})
	}
	wg.Wait()
	close(leases)
	if admitted.Load() != 1 {
		t.Fatalf("recovery requests=%d, want 1", admitted.Load())
	}
	for ticket := range leases {
		ticket.finish(nil)
	}
	if len(a.enterpriseQuota.views(now)) != 0 {
		t.Fatal("terminal successful verification did not release pool")
	}
}

func TestEnterpriseFencingResetRebindAndCancellation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "a", "12345", "us", "enterprise_plus", "alice")
	oldSuccess, _ := a.enterpriseQuota.begin()
	oldFailure, _ := a.enterpriseQuota.begin()
	rejectSharedQuota(t, a, now, "edition")
	oldSuccess.finish(nil)
	view := a.enterpriseQuota.views(now)[0]
	if _, err := m.ResetSharedQuota(context.Background(), a.ID, view.PoolID, view.PoolVersion+1); err == nil {
		t.Fatal("stale reset accepted")
	}
	reset, err := m.ResetSharedQuota(context.Background(), a.ID, view.PoolID, view.PoolVersion)
	if err != nil {
		t.Fatal(err)
	}
	oldFailure.finish(quotaRejection(now, "edition", "enterprise_plus"))
	if v := a.enterpriseQuota.views(now)[0]; v.Phase != "probe_pending" || v.PoolVersion != reset.PoolVersion {
		t.Fatal("pre-reset rejection overwrote explicit retry")
	}
	probe, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	probe.finish(context.Canceled)
	if v := a.enterpriseQuota.views(now)[0]; v.Phase != "probe_pending" {
		t.Fatal("cancel stranded verification lease")
	}
	probe, _ = a.enterpriseQuota.begin()
	m.Remove(context.Background(), a.ID)
	rebound := sharedQuotaAuth(t, m, "a", "12345", "eu", "enterprise_plus", "alice")
	probe.finish(quotaRejection(now, "edition", "enterprise_plus"))
	if len(rebound.enterpriseQuota.views(now)) != 0 {
		t.Fatal("old binding response contaminated new resource")
	}
}

func TestEnterprisePersistenceRestoreAndOptOut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := NewFileCooldownStateStore(dir)
	m := NewManager(nil, nil, nil)
	m.SetCooldownStateStore(store)
	now := time.Now()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "a", "12345", "us", "enterprise_plus", "alice")
	b := sharedQuotaAuth(t, m, "b", "12345", "us", "enterprise_plus", "bob")
	rejectSharedQuota(t, a, now, "edition")
	if _, err := os.Stat(filepath.Join(dir, "shared-quota-state.sqs")); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(nil, nil, nil)
	restored.SetCooldownStateStore(store)
	r := sharedQuotaAuth(t, restored, "b", "12345", "us", "", "bob")
	if err := restored.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = restored.GetByID(r.ID)
	if blocked, _, _ := isAuthBlockedForModel(r, "new-model", now); !blocked {
		t.Fatal("restored shared restriction lost verified membership")
	}
	opt := b.Clone()
	opt.Metadata["disable_cooling"] = true
	updated, err := m.Update(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, "flash", now); blocked {
		t.Fatal("credential opt-out ignored")
	}
	if blocked, _, _ := isAuthBlockedForModel(a, "flash", now); !blocked {
		t.Fatal("opt-out cleared sibling restriction")
	}
	snapshot := m.sharedQuotaSnapshot()
	stale := snapshot
	stale.Version--
	if err := store.SaveSharedQuota(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSharedQuota(ctx, stale); err == nil {
		t.Fatal("stale persistence accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "shared-quota-state.sqs"), []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreCooldownStates(ctx); err == nil {
		t.Fatal("corrupt shared state accepted")
	}
	if blocked, _, _ := isAuthBlockedForModel(r, "flash", now); !blocked {
		t.Fatal("store failure cleared memory restriction")
	}
}

type sharedBoundaryExecutor struct {
	ProviderExecutor
	calls atomic.Int32
}

func (*sharedBoundaryExecutor) Identifier() string { return ge.Provider }
func (e *sharedBoundaryExecutor) Execute(context.Context, *Auth, exec.Request, exec.Options) (exec.Response, error) {
	e.calls.Add(1)
	return exec.Response{}, nil
}

func TestEnterpriseSelectorsSchedulerAndFinalBoundary(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	a := sharedQuotaAuth(t, m, "a", "12345", "us", "enterprise_plus", "alice")
	b := sharedQuotaAuth(t, m, "b", "12345", "us", "enterprise_plus", "bob")
	e := &sharedBoundaryExecutor{}
	m.RegisterExecutor(e)
	for _, auth := range []*Auth{a, b} {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, ge.Provider, []*registry.ModelInfo{{ID: "argon"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		m.scheduler.upsertAuth(auth)
	}
	stale := b.Clone()
	rejectSharedQuota(t, a, now, "edition")
	for _, selector := range []Selector{&RoundRobinSelector{}, &WeightedRoundRobinSelector{}, &FillFirstSelector{}} {
		if _, err := selector.Pick(context.Background(), ge.Provider, "argon", exec.Options{}, []*Auth{b}); err == nil {
			t.Fatalf("selector bypass: %T", selector)
		}
	}
	for _, pick := range []func() error{func() error {
		_, _, e := m.pickNext(context.Background(), ge.Provider, "argon", exec.Options{}, nil)
		return e
	}, func() error {
		_, _, e := m.pickNextLegacy(context.Background(), ge.Provider, "argon", exec.Options{}, nil)
		return e
	}} {
		if err := pick(); err == nil {
			t.Fatal("dispatch selector bypassed pool")
		}
	}
	if _, err := executorForAuth(e, stale).Execute(context.Background(), stale, exec.Request{}, exec.Options{}); err == nil || e.calls.Load() != 0 {
		t.Fatal("final boundary dispatched stale/plugin selection")
	}
	if p := m.clientModelProjectionForAuth(stale, "new-model", now); !p.Suspended {
		t.Fatal(fmt.Sprintf("registry projection escaped gate: %+v", p))
	}
}

func TestEnterpriseTechnicalMinuteConfigAndOptOutSuccess(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	a := sharedQuotaAuth(t, m, "minute-source", "12345", "us", "enterprise_plus", "alice")
	b := sharedQuotaAuth(t, m, "minute-sibling", "12345", "us", "enterprise_standard", "bob")
	ticket, _ := a.enterpriseQuota.begin()
	ticket.finish(&ge.GenerationError{HTTPStatus: 429, Restriction: &ge.Restriction{Kind: "minute", Scope: "project", Feature: "technical_requests", RetryAt: now.Add(time.Minute), TimeSource: "google"}})
	if blocked, _, _ := isAuthBlockedForModel(b, "new-model", now); !blocked {
		t.Fatal("project minute quota was separated by edition")
	}
	opt := b.Clone()
	opt.Metadata["disable_cooling"] = true
	b, _ = m.Update(context.Background(), opt)
	ticket, err := b.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	ticket.finish(nil)
	if blocked, _, _ := isAuthBlockedForModel(a, "new-model", now); !blocked {
		t.Fatal("opt-out success cleared shared pool")
	}
	m.SetConfig(&config.Config{DisableCooling: true})
	if blocked, _, _ := isAuthBlockedForModel(a, "new-model", now); blocked {
		t.Fatal("global hot opt-out ignored")
	}
	m.SetConfig(&config.Config{})
	if blocked, _, _ := isAuthBlockedForModel(a, "new-model", now); !blocked {
		t.Fatal("hot opt-out erased authoritative state")
	}
}

func TestEnterpriseProbeSuccessCannotClearNewRejectionAndStoreSwap(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "fence-a", "12345", "us", "enterprise_plus", "alice")
	b := sharedQuotaAuth(t, m, "fence-b", "12345", "us", "enterprise_plus", "bob")
	oldRejection, _ := b.enterpriseQuota.begin()
	rejectSharedQuota(t, a, now, "edition")
	now = now.Add(2 * time.Hour)
	probe, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	oldRejection.finish(quotaRejection(now, "edition", "enterprise_plus"))
	probe.finish(nil)
	if views := a.enterpriseQuota.views(now); len(views) != 1 || views[0].Phase != "cooling" {
		t.Fatal("old probe success cleared newer rejection")
	}
	store := NewFileCooldownStateStore(t.TempDir())
	if !m.SwapCooldownStateStore(context.Background(), store, true) {
		t.Fatal("store transition failed")
	}
	m.PersistCooldownStates(context.Background())
	m.Remove(context.Background(), a.ID)
	if len(b.enterpriseQuota.views(now)) != 1 {
		t.Fatal("removing one member cleared other member restriction")
	}
	restored := NewManager(nil, nil, nil)
	restored.SetCooldownStateStore(store)
	sharedQuotaAuth(t, restored, "fence-b", "12345", "us", "", "bob")
	if err := restored.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := restored.GetByID(b.ID)
	if len(current.enterpriseQuota.views(now)) != 1 {
		t.Fatal("store transition lost shared state")
	}
}

func TestEnterpriseDeadlineExtensionVersionsWithoutBatchEscalation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "deadline", "12345", "us", "enterprise_plus", "alice")
	first, _ := a.enterpriseQuota.begin()
	later, _ := a.enterpriseQuota.begin()
	shorter, _ := a.enterpriseQuota.begin()
	first.finish(quotaRejection(now, "edition", "enterprise_plus"))
	before := a.enterpriseQuota.views(now)[0]
	later.finish(quotaRejection(now.Add(time.Hour), "edition", "enterprise_plus"))
	after := a.enterpriseQuota.views(now)[0]
	if after.PoolVersion != before.PoolVersion+1 || !after.RetryAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("deadline extension lacked a new version: before=%+v, after=%+v", before, after)
	}
	if _, err := m.ResetSharedQuota(context.Background(), a.ID, before.PoolID, before.PoolVersion); err == nil {
		t.Fatal("reset using a pre-extension version was accepted")
	}
	shorter.finish(quotaRejection(now, "edition", "enterprise_plus"))
	merged := a.enterpriseQuota.views(now)[0]
	if merged.PoolVersion != after.PoolVersion || !merged.RetryAt.Equal(after.RetryAt) {
		t.Fatal("same-window rejection escalated or shortened the shared restriction")
	}
}

func TestEnterpriseHomeTransitionFencesInFlightLocalRequests(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "home-fence", "12345", "us", "enterprise_plus", "alice")
	oldFailure, _ := a.enterpriseQuota.begin()
	rejectSharedQuota(t, a, now, "edition")
	now = now.Add(2 * time.Hour)
	probe, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	m.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	if _, err := a.enterpriseQuota.begin(); err == nil {
		t.Fatal("stale local executor started a request in Home mode")
	}
	if views := a.enterpriseQuota.views(now); len(views) != 0 {
		t.Fatal("Home mode exposed local authoritative restrictions")
	}
	m.SetConfig(&config.Config{})
	before := a.enterpriseQuota.views(now)[0]
	if before.Phase != "probe_pending" {
		t.Fatal("Home transition stranded a local verification lease")
	}
	probe.finish(nil)
	oldFailure.finish(quotaRejection(now, "edition", "enterprise_plus"))
	after := a.enterpriseQuota.views(now)[0]
	if after.Phase != "probe_pending" || after.PoolVersion != before.PoolVersion {
		t.Fatal("pre-Home request mutated the resumed local authority")
	}
	current, err := a.enterpriseQuota.begin()
	if err != nil {
		t.Fatal(err)
	}
	current.finish(nil)
	if len(a.enterpriseQuota.views(now)) != 0 {
		t.Fatal("new local request could not verify after returning from Home mode")
	}
}

func TestEnterpriseStoreSwapKeepsCurrentAuthorityAndIdentity(t *testing.T) {
	for _, phase := range []string{"normal", "cooling", "probe_pending"} {
		t.Run(phase, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			now := time.Now().UTC()
			m.enterpriseQuota.nowFunc = func() time.Time { return now }
			a := sharedQuotaAuth(t, m, "store-authority", "12345", "us", "enterprise_plus", "alice")
			rejectSharedQuota(t, a, now, "edition")
			before := a.enterpriseQuota.views(now)[0]
			foreign := m.sharedQuotaSnapshot()
			foreign.Version += 100
			for i := range foreign.Pools {
				s := &foreign.Pools[i]
				s.Version += 100
				if s.Phase != "normal" {
					s.Phase = phase
					s.Restriction.RetryAt = now.Add(time.Minute)
					if phase == "normal" {
						s.Restriction = ge.Restriction{}
					}
				}
			}
			foreign.Bindings[0].Identity.Edition = "enterprise_standard"
			store := NewFileCooldownStateStore(t.TempDir())
			if err := store.SaveSharedQuota(context.Background(), foreign); err != nil {
				t.Fatal(err)
			}
			if !m.SwapCooldownStateStore(context.Background(), store, true) {
				t.Fatal("store transition failed")
			}
			current, _ := m.GetByID(a.ID)
			after := current.enterpriseQuota.views(now)[0]
			if after.Phase != before.Phase || after.PoolVersion != before.PoolVersion || !after.RetryAt.Equal(before.RetryAt) {
				t.Fatal("unrelated store versions overwrote the in-memory restriction")
			}
			if current.EnterpriseQuotaIdentity.Edition != "enterprise_plus" {
				t.Fatal("stored identity overwrote the currently verified subscription")
			}
			m.PersistCooldownStates(context.Background())
			saved, err := store.LoadSharedQuota(context.Background())
			if err != nil || saved.Version <= foreign.Version {
				t.Fatalf("store version was not adopted before the next save: %+v, %v", saved, err)
			}
		})
	}
}

func TestEnterpriseCorruptStoreTransitionKeepsMemoryAndOriginalStore(t *testing.T) {
	for _, contents := range []string{`{broken`, `{}`, `{"schema_version":1,"version":0}`} {
		t.Run(contents, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			source := NewFileCooldownStateStore(t.TempDir())
			m.SetCooldownStateStore(source)
			now := time.Now().UTC()
			a := sharedQuotaAuth(t, m, "bad-store", "12345", "us", "enterprise_plus", "alice")
			rejectSharedQuota(t, a, now, "edition")
			before := a.enterpriseQuota.views(now)[0]
			target := NewFileCooldownStateStore(t.TempDir())
			if err := os.WriteFile(filepath.Join(target.dir, "shared-quota-state.sqs"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if m.SwapCooldownStateStore(context.Background(), target, true) {
				t.Fatal("corrupt store transition was reported as successful")
			}
			m.mu.RLock()
			retained := m.cooldownStore == source
			m.mu.RUnlock()
			if !retained {
				t.Fatal("failed restore replaced the active store")
			}
			after := a.enterpriseQuota.views(now)[0]
			if before.PoolVersion != after.PoolVersion || !before.RetryAt.Equal(after.RetryAt) {
				t.Fatal("failed restore cleared or changed the in-memory restriction")
			}
		})
	}
}

func TestEnterpriseSharedSnapshotRejectsMismatchedKeyAndDuplicateRecords(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	a := sharedQuotaAuth(t, m, "bad-snapshot", "12345", "us", "enterprise_plus", "alice")
	rejectSharedQuota(t, a, now, "edition")
	encoded, err := json.Marshal(m.sharedQuotaSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*SharedQuotaSnapshot){
		func(s *SharedQuotaSnapshot) { s.Pools = append(s.Pools, s.Pools[0]) },
		func(s *SharedQuotaSnapshot) { s.Bindings = append(s.Bindings, s.Bindings[0]) },
		func(s *SharedQuotaSnapshot) {
			for i := range s.Pools {
				if s.Pools[i].Phase != "normal" {
					s.Pools[i].Restriction.Scope = "project"
				}
			}
		},
	} {
		var snapshot SharedQuotaSnapshot
		if err := json.Unmarshal(encoded, &snapshot); err != nil {
			t.Fatal(err)
		}
		mutation(&snapshot)
		if err := snapshot.Validate(); err == nil {
			t.Fatal("corrupt shared quota snapshot was accepted")
		}
	}
}
