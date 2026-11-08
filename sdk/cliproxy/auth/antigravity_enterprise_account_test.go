package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func aeAccountFixture(subject string, licenses ...ae.License) *Auth {
	metadata := map[string]any{"type": ae.Provider, "email": subject + "@example.test", "google_user_id": subject, "client_id": "fixture-client", "access_token": "fixture-access-secret", "refresh_token": "fixture-refresh-secret", "expired": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}
	account := ae.Account{Version: 1, ID: ae.AccountID(subject, "fixture-client"), Revision: 1, Bindings: []ae.Binding{}}
	for _, l := range licenses {
		account.Bindings = append(account.Bindings, ae.Binding{ID: ae.BindingID(account.ID, l), Revision: 1, License: l, Settings: map[string]any{}})
	}
	id := ae.Provider + "-account-" + account.ID[:32] + ".json"
	return &Auth{ID: id, FileName: id, Provider: ae.Provider, Status: StatusActive, Metadata: ae.PutAccount(metadata, account)}
}
func aeLicense(project, region string) ae.License {
	return ae.License{ProjectID: project, Location: region, UserTier: "gcp-ge-plus-tier", TierDisplayName: "Gemini Enterprise Plus"}
}
func aeRegister(t *testing.T, m *Manager, a *Auth) *Auth {
	t.Helper()
	saved, err := m.Register(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
func TestAntigravityEnterpriseGraphProjectionAndRevision(t *testing.T) {
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	parent := aeRegister(t, m, aeAccountFixture("identity", aeLicense("project-a", "global"), aeLicense("project-a", "us"), aeLicense("project-b", "global"), aeLicense("project-c", "global")))
	children := m.AntigravityEnterpriseBindings(parent.ID)
	if len(children) != 4 || len(m.List()) != 5 {
		t.Fatalf("wrong graph: children=%d runtime=%d", len(children), len(m.List()))
	}
	if _, _, schedulable := isSchedulableAuth(parent); schedulable {
		t.Fatal("parent is schedulable")
	}
	seen := map[string]bool{parent.Index: true}
	for _, a := range children {
		if seen[a.Index] || !IsAntigravityEnterpriseBindingAuth(a) || a.Storage != nil {
			t.Fatal("invalid projected child")
		}
		seen[a.Index] = true
	}
	first := children[0]
	m.MarkResult(t.Context(), Result{AuthID: first.ID, Provider: ae.Provider, Model: "gemini-4-argon-low", Success: true, RegistrationEpoch: first.RegistrationEpoch, CredentialVersion: first.CredentialVersion})
	_, graph, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
	updated, err := m.UpdateAntigravityEnterpriseAccount(t.Context(), parent, graph.Revision, func(a *ae.Account) error {
		a.Bindings[0].Disabled = true
		a.Bindings[0].Settings = map[string]any{"prefix": "one", "weight": 3}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := m.AntigravityEnterpriseBindings(updated.ID)
	if !after[0].Disabled || after[0].Prefix != "one" || after[0].Success != 1 || after[1].Disabled || after[1].Prefix != "" {
		t.Fatal("settings/counters leaked or were lost")
	}
	if _, err = m.UpdateAntigravityEnterpriseAccount(t.Context(), parent, graph.Revision, func(*ae.Account) error { return nil }); !errors.Is(err, ErrAntigravityEnterpriseAccountChanged) {
		t.Fatal("stale revision accepted")
	}
	updated.Disabled, updated.Status = true, StatusDisabled
	if _, err = m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := m.GetByID(parent.ID); !snapshot.Disabled {
		t.Fatal("parent disabled flag was lost")
	}
	for _, a := range m.AntigravityEnterpriseBindings(parent.ID) {
		if !a.Disabled {
			t.Fatal("parent switch did not disable child")
		}
	}
	updated, _ = m.GetByID(parent.ID)
	updated.Disabled, updated.Status = false, StatusActive
	if _, err = m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	after = m.AntigravityEnterpriseBindings(parent.ID)
	if !after[0].Disabled || after[1].Disabled {
		t.Fatal("parent switch overwrote child switches")
	}
	for _, id := range store.saves {
		if strings.Contains(id, "-binding:") {
			t.Fatal("runtime child was persisted")
		}
	}
	loaded := NewManager(store, nil, nil)
	if err = loaded.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(loaded.AntigravityEnterpriseBindings(parent.ID)) != 4 {
		t.Fatal("load did not project bindings")
	}
	m.Remove(t.Context(), parent.ID)
	if len(m.List()) != 0 {
		t.Fatal("parent removal left children")
	}
}

func TestAntigravityEnterpriseBindingMutationRejectsStaleEpoch(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parent := aeRegister(t, m, aeAccountFixture("epoch-fence", aeLicense("project-a", "global")))
	child := m.AntigravityEnterpriseBindings(parent.ID)[0]
	if _, err := m.UpdateAntigravityEnterpriseAccount(t.Context(), child, 0, func(account *ae.Account) error {
		account.Bindings[0].Settings = map[string]any{"note": "first"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := m.UpdateAntigravityEnterpriseAccount(t.Context(), child, 0, func(*ae.Account) error {
		called = true
		return nil
	}); !errors.Is(err, ErrAntigravityEnterpriseAccountChanged) || called {
		t.Fatalf("stale child mutation accepted: err=%v called=%v", err, called)
	}
}

func TestAntigravityEnterpriseQuotaObservationIsIdempotentAndRechecksReset(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parent := aeRegister(t, m, aeAccountFixture("observation-fence", aeLicense("project-a", "global")))
	child := m.AntigravityEnterpriseBindings(parent.ID)[0]
	now := time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)
	m.antigravityEnterprise.now = func() time.Time { return now }
	reset := now.Add(time.Minute)
	quota := aeQuota(now, 1, &reset)
	if !m.ObserveAntigravityEnterpriseQuota(child, quota) {
		t.Fatal("fresh observation rejected")
	}
	version := m.AntigravityEnterpriseQuotaVersion(child)
	if !m.ObserveAntigravityEnterpriseQuota(child, quota) || m.AntigravityEnterpriseQuotaVersion(child) != version {
		t.Fatal("duplicate observation changed the pool version")
	}
	if m.ObserveAntigravityEnterpriseQuota(child, aeQuota(now, 0, &reset)) {
		t.Fatal("conflicting observation at the same timestamp replaced the pool")
	}
	now = reset
	if blocked, _ := child.antigravityEnterpriseBlocked(now); blocked {
		t.Fatal("selector mislabeled expired metadata as a quota cooldown")
	}
	var failure *ae.Error
	if err := m.CheckAntigravityEnterpriseQuota(child, now); !errors.As(err, &failure) || failure.Code != "metadata_pending" {
		t.Fatalf("reset should require metadata, got %v", err)
	}
	for _, view := range CooldownSnapshotForAuth(child, now) {
		if view.Reason == "weekly_quota" {
			t.Fatal("unknown quota after reset was mislabeled as an observed weekly zero")
		}
	}
}

type aeRefreshExecutor struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (*aeRefreshExecutor) Identifier() string { return ae.Provider }
func (*aeRefreshExecutor) Execute(context.Context, *Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, errors.New("generation forbidden in refresh test")
}
func (*aeRefreshExecutor) ExecuteStream(context.Context, *Auth, coreexec.Request, coreexec.Options) (*coreexec.StreamResult, error) {
	return nil, errors.New("generation forbidden in refresh test")
}
func (*aeRefreshExecutor) CountTokens(context.Context, *Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, errors.New("count forbidden")
}
func (*aeRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("network forbidden")
}
func (e *aeRefreshExecutor) Refresh(ctx context.Context, a *Auth) (*Auth, error) {
	if e.calls.Add(1) == 1 {
		close(e.started)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.release:
	}
	next := a.Clone()
	next.Metadata["access_token"] = "refreshed-fixture-secret"
	next.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	return next, nil
}

func TestAntigravityEnterpriseRefreshSharedAndReauthorizationPreservesGraph(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := aeAccountFixture("refresh", aeLicense("project-a", "global"), aeLicense("project-b", "global"))
	a.Metadata["expired"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	parent := aeRegister(t, m, a)
	oldGraph := parent.Clone()
	_, graph, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
	if _, err := m.UpdateAntigravityEnterpriseAccount(t.Context(), parent, graph.Revision, func(a *ae.Account) error {
		a.Bindings[0].Disabled = true
		a.Bindings[1].Settings = map[string]any{"prefix": "latest"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	executor := &aeRefreshExecutor{started: make(chan struct{}), release: make(chan struct{})}
	m.RegisterExecutor(executor)
	children := m.AntigravityEnterpriseBindings(parent.ID)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := m.EnsureAntigravityEnterpriseAccountToken(t.Context(), children[i%2].ID)
			errs <- err
		}(i)
	}
	<-executor.started
	close(executor.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("refresh count %d", executor.calls.Load())
	}
	for _, a := range m.AntigravityEnterpriseBindings(parent.ID) {
		if a.Metadata["access_token"] != "refreshed-fixture-secret" {
			t.Fatal("token not propagated")
		}
	}
	oldGraph.Metadata["access_token"] = "reauthorized-fixture-secret"
	oldGraph.Metadata["refresh_token"] = ""
	if _, err := m.ReauthorizeAntigravityEnterpriseAccount(t.Context(), oldGraph); err != nil {
		t.Fatal(err)
	}
	after := m.AntigravityEnterpriseBindings(parent.ID)
	if !after[0].Disabled || after[1].Prefix != "latest" {
		t.Fatal("reauthorization replayed stale graph")
	}
	fresh, _, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
	if ae.String(fresh.Metadata, "refresh_token") != "fixture-refresh-secret" {
		t.Fatal("refresh grant was discarded")
	}
}

func TestAntigravityEnterpriseReauthorizationClearsTerminalAuthButPreservesOperatorDisable(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parent := aeRegister(t, m, aeAccountFixture("invalid-grant", aeLicense("project-a", "global")))
	broken := parent.Clone()
	broken.Disabled, broken.Status = true, StatusDisabled
	broken.LastError = &Error{Code: "invalid_grant", Message: "invalid_grant", HTTPStatus: 401}
	broken.StatusMessage = "invalid_grant"
	if _, err := m.Update(t.Context(), broken); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReauthorizeAntigravityEnterpriseAccount(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	fresh, _, _ := m.AntigravityEnterpriseAccountSnapshot(parent.ID)
	if fresh.Disabled || fresh.LastError != nil || fresh.Status != StatusActive {
		t.Fatal("terminal authorization was not cleared")
	}
	child := m.AntigravityEnterpriseBindings(parent.ID)[0]
	if child.Disabled || child.LastError != nil || child.Unavailable {
		t.Fatal("terminal child error survived authorization")
	}
	fresh.Disabled, fresh.Status = true, StatusDisabled
	if _, err := m.Update(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReauthorizeAntigravityEnterpriseAccount(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	fresh, _, _ = m.AntigravityEnterpriseAccountSnapshot(parent.ID)
	if !fresh.Disabled || fresh.Status != StatusDisabled {
		t.Fatal("reauthorization overrode operator disable")
	}
}

func aeQuota(now time.Time, fraction float64, reset *time.Time) ae.Quota {
	q := ae.Quota{ObservedAt: now, Overage: &struct {
		Enabled bool `json:"enabled"`
	}{}}
	q.Groups = append(q.Groups, struct {
		DisplayName string      `json:"displayName"`
		Buckets     []ae.Bucket `json:"buckets"`
	}{Buckets: []ae.Bucket{{ID: ae.WeeklyBucket, Window: "weekly", Fraction: &fraction, ResetTime: reset}}})
	return q
}
func TestAntigravityEnterpriseWeeklyPoolSharedFencedAndPersistent(t *testing.T) {
	m := NewManager(nil, nil, nil)
	p1 := aeRegister(t, m, aeAccountFixture("one", aeLicense("project-a", "global"), aeLicense("project-a", "us")))
	p2 := aeRegister(t, m, aeAccountFixture("two", aeLicense("project-a", "global")))
	a := m.AntigravityEnterpriseBindings(p1.ID)[0]
	other := m.AntigravityEnterpriseBindings(p2.ID)[0]
	us := m.AntigravityEnterpriseBindings(p1.ID)[1]
	now := time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)
	m.antigravityEnterprise.now = func() time.Time { return now }
	reset := now.Add(7 * 24 * time.Hour)
	if !m.ObserveAntigravityEnterpriseQuota(a, aeQuota(now, 0, &reset)) {
		t.Fatal("observation rejected")
	}
	if blocked, _ := other.antigravityEnterpriseBlocked(now); !blocked {
		t.Fatal("same project pool did not propagate")
	}
	if blocked, _ := us.antigravityEnterpriseBlocked(now); blocked {
		t.Fatal("quota crossed region")
	}
	if err := m.CheckAntigravityEnterpriseQuota(other, now); err == nil {
		t.Fatal("weekly zero passed final gate")
	}
	other.Metadata["disable_cooling"] = true
	if err := m.CheckAntigravityEnterpriseQuota(other, now); err == nil {
		t.Fatal("disable cooling bypassed weekly quota")
	}
	m.RestrictAntigravityEnterpriseQuota(a, &ae.Error{HTTP: 429, Code: "weekly_quota", RetryAt: reset})
	now = now.Add(time.Minute)
	if !m.ObserveAntigravityEnterpriseQuota(other, aeQuota(now, 1, &reset)) {
		t.Fatal("new positive observation rejected")
	}
	if err := m.CheckAntigravityEnterpriseQuota(other, now); err == nil {
		t.Fatal("early positive quota erased upstream restriction")
	}
	store := &FileCooldownStateStore{dir: t.TempDir()}
	snapshot := m.antigravityEnterpriseQuotaSnapshot()
	if err := store.SaveAntigravityEnterpriseQuota(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.dir, "antigravity-enterprise-quota.sqs"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fixture-access-secret") || strings.Contains(string(data), "fixture-refresh-secret") {
		t.Fatal("quota state contains credential")
	}
	restarted := NewManager(nil, nil, nil)
	rp := aeRegister(t, restarted, aeAccountFixture("two", aeLicense("project-a", "global")))
	if err = restarted.restoreAntigravityEnterpriseQuota(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	ra := restarted.AntigravityEnterpriseBindings(rp.ID)[0]
	if blocked, _ := ra.antigravityEnterpriseBlocked(now); !blocked {
		t.Fatal("restart lost weekly restriction")
	}
	now = reset.Add(time.Second)
	if !m.ObserveAntigravityEnterpriseQuota(other, aeQuota(now, 1, nil)) {
		t.Fatal("reset observation rejected")
	}
	if err = m.CheckAntigravityEnterpriseQuota(other, now); err != nil {
		t.Fatalf("pool did not recover after reset: %v", err)
	}
	_, graph, _ := m.AntigravityEnterpriseAccountSnapshot(p1.ID)
	if _, err = m.UpdateAntigravityEnterpriseAccount(t.Context(), p1, graph.Revision, func(a *ae.Account) error { a.Bindings[0].Settings = map[string]any{"note": "new revision"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if m.ObserveAntigravityEnterpriseQuota(a, aeQuota(now, 0, nil)) {
		t.Fatal("stale binding observation accepted")
	}
	fresh := m.AntigravityEnterpriseBindings(p1.ID)[0]
	m.MarkResult(t.Context(), Result{AuthID: a.ID, Provider: ae.Provider, Model: "gemini-4-argon-low", Success: true, RegistrationEpoch: a.RegistrationEpoch, CredentialVersion: a.CredentialVersion})
	m.MarkResult(t.Context(), Result{AuthID: a.ID, Provider: ae.Provider, Model: "gemini-4-argon-low", Error: AntigravityEnterpriseResultError(&ae.Error{HTTP: 403, Code: "permission_denied", Scope: "binding", Attempted: true}), RegistrationEpoch: a.RegistrationEpoch, CredentialVersion: a.CredentialVersion})
	completed, _ := m.GetByID(fresh.ID)
	if completed.Success != fresh.Success+1 || completed.Failed != fresh.Failed+1 {
		t.Fatal("historical binding counters were discarded")
	}
	if failure, ok := m.antigravityEnterprise.cooldowns[fresh.ID]; ok && failure.Fingerprint == aeBindingFingerprint(fresh) {
		t.Fatal("historical403 changed replacement binding health")
	}
}

func TestAntigravityEnterpriseLocalGatesAndDiagnosticErrorsDoNotCoolBindings(t *testing.T) {
	m := NewManager(nil, nil, nil)
	p := aeRegister(t, m, aeAccountFixture("errors", aeLicense("project-a", "global"), aeLicense("project-b", "global")))
	children := m.AntigravityEnterpriseBindings(p.ID)
	source := children[0]
	for _, e := range []*ae.Error{{HTTP: 403, Code: "permission_denied"}, {HTTP: 429, Code: "weekly_quota"}, {HTTP: 409, Code: "subscription_only_required"}} {
		m.MarkResult(t.Context(), Result{AuthID: source.ID, Provider: ae.Provider, Model: "gemini-4-argon-low", Error: AntigravityEnterpriseResultError(e), RegistrationEpoch: source.RegistrationEpoch, CredentialVersion: source.CredentialVersion})
	}
	after, _ := m.GetByID(source.ID)
	if after.Failed != 0 || len(after.ModelStates) != 0 {
		t.Fatal("metadata failures counted as generations")
	}
	m.MarkResult(t.Context(), Result{AuthID: source.ID, Provider: ae.Provider, Model: ae.CarbonModel, Error: &Error{Code: "antigravity_enterprise_diagnostic", HTTPStatus: 403, Message: "unlisted model denied"}, RegistrationEpoch: source.RegistrationEpoch, CredentialVersion: source.CredentialVersion})
	after, _ = m.GetByID(source.ID)
	if after.Failed != 1 || len(after.ModelStates) != 0 {
		t.Fatal("diagnostic error poisoned normal models")
	}
	if blocked, _ := after.antigravityEnterpriseBlocked(time.Now()); blocked {
		t.Fatal("diagnostic error cooled binding")
	}
	permission := &ae.Error{HTTP: 403, Code: "permission_denied", Scope: "binding", Attempted: true}
	m.MarkResult(t.Context(), Result{AuthID: source.ID, Provider: ae.Provider, Model: "gemini-4-argon-low", Error: AntigravityEnterpriseResultError(permission), RegistrationEpoch: source.RegistrationEpoch, CredentialVersion: source.CredentialVersion})
	after, _ = m.GetByID(source.ID)
	if after.Failed != 2 {
		t.Fatal("generation permission failure not counted")
	}
	if blocked, _ := after.antigravityEnterpriseBlocked(time.Now()); !blocked {
		t.Fatal("403 did not cool all binding models")
	}
	if blocked, _ := children[1].antigravityEnterpriseBlocked(time.Now()); blocked {
		t.Fatal("403 crossed subscription bindings")
	}
	if _, _, err := m.ResetQuota(t.Context(), source.ID); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := after.antigravityEnterpriseBlocked(time.Now()); blocked {
		t.Fatal("manual transient reset did not clear binding403")
	}
}

func TestAntigravityEnterpriseUnknown429BackoffAndExplicitHints(t *testing.T) {
	m := NewManager(nil, nil, nil)
	p := aeRegister(t, m, aeAccountFixture("429", aeLicense("project-a", "global")))
	source := m.AntigravityEnterpriseBindings(p.ID)[0]
	for _, seconds := range []int{10, 20, 40, 60, 60} {
		e := &ae.Error{HTTP: 429, Code: "upstream_error", Attempted: true}
		m.MarkResult(t.Context(), Result{AuthID: source.ID, Provider: ae.Provider, Model: "gemini-4-argon-low", Error: AntigravityEnterpriseResultError(e), RegistrationEpoch: source.RegistrationEpoch, CredentialVersion: source.CredentialVersion})
		after, _ := m.GetByID(source.ID)
		wait := after.ModelStates["gemini-4-argon-low"].NextRetryAfter.Sub(time.Now())
		if wait < time.Duration(seconds-1)*time.Second || wait > time.Duration(seconds+1)*time.Second {
			t.Fatalf("backoff=%v want%ds", wait, seconds)
		}
	}
	hint := 2 * time.Second
	e := &ae.Error{HTTP: 429, Code: "upstream_error", RetryHint: true, Attempted: true}
	m.MarkResult(t.Context(), Result{AuthID: source.ID, Provider: ae.Provider, Model: "gemini-3.8-flash-low", Error: AntigravityEnterpriseResultError(e), RetryAfter: &hint, RegistrationEpoch: source.RegistrationEpoch, CredentialVersion: source.CredentialVersion})
	after, _ := m.GetByID(source.ID)
	wait := after.ModelStates["gemini-3.8-flash-low"].NextRetryAfter.Sub(time.Now())
	if wait < time.Second || wait > 3*time.Second {
		t.Fatalf("explicit short server hint not preserved: %v", wait)
	}
}

func TestAntigravityEnterpriseAliasAndBothSchedulersRespectBindingWeightAndWeeklyGate(t *testing.T) {
	if OAuthModelAliasChannel(ae.Provider, "oauth") != ae.Provider {
		t.Fatal("provider alias channel missing")
	}
	m := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	fixture := aeAccountFixture("routing", aeLicense("route-a", "global"), aeLicense("route-b", "global"))
	graph, _ := ae.ReadAccount(fixture.Metadata)
	graph.Bindings[0].Settings = map[string]any{"weight": 3, "model_aliases": []any{map[string]any{"name": "gemini-4-argon-low", "alias": "local-model"}}}
	graph.Bindings[1].Settings = map[string]any{"weight": 1}
	fixture.Metadata = ae.PutAccount(fixture.Metadata, *graph)
	parent := aeRegister(t, m, fixture)
	children := m.AntigravityEnterpriseBindings(parent.ID)
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{ae.Provider: {{Name: "gemini-3.8-flash-low", Alias: "global-model"}}})
	if got := m.applyOAuthModelAlias(children[0], "local-model"); got != "gemini-4-argon-low" {
		t.Fatalf("scoped alias=%s", got)
	}
	if got := m.applyOAuthModelAlias(children[1], "local-model"); got != "local-model" {
		t.Fatal("alias leaked between bindings")
	}
	if got := m.applyOAuthModelAlias(children[1], "global-model"); got != "gemini-3.8-flash-low" {
		t.Fatal("global alias missing")
	}
	m.RegisterExecutor(schedulerTestExecutor{provider: ae.Provider})
	model := "ae-routing-fixture-model"
	r := registry.GetGlobalRegistry()
	for _, a := range append(children, parent) {
		r.RegisterClient(a.ID, ae.Provider, []*registry.ModelInfo{{ID: model, Object: "model", OwnedBy: ae.Provider}})
		m.RefreshSchedulerEntry(a.ID)
		t.Cleanup(func() { r.UnregisterClient(a.ID) })
	}
	if _, _, scheduled := isSchedulableAuth(parent); scheduled {
		t.Fatal("parent became schedulable")
	}
	for _, fast := range []bool{false, true} {
		m.SetSelector(&WeightedRoundRobinSelector{})
		counts := map[string]int{}
		for i := 0; i < 40; i++ {
			var chosen *Auth
			var err error
			if fast {
				chosen, err = m.scheduler.pickSingle(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
			} else {
				chosen, _, err = m.pickNextLegacy(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			counts[chosen.ID]++
		}
		if counts[children[0].ID] != 30 || counts[children[1].ID] != 10 || counts[parent.ID] != 0 {
			t.Fatalf("weighted selection fast=%v counts=%v", fast, counts)
		}
	}
	now := time.Now()
	reset := now.Add(7 * 24 * time.Hour)
	m.ObserveAntigravityEnterpriseQuota(children[0], aeQuota(now, 0, &reset))
	m.ObserveAntigravityEnterpriseQuota(children[1], aeQuota(now, 1, &reset))
	for _, fast := range []bool{false, true} {
		var chosen *Auth
		var err error
		if fast {
			chosen, err = m.scheduler.pickSingle(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
		} else {
			chosen, _, err = m.pickNextLegacy(t.Context(), ae.Provider, model, coreexec.Options{}, nil)
		}
		if err != nil || chosen == nil || chosen.ID != children[1].ID {
			t.Fatalf("weekly pool bypass fast=%v chosen=%v err=%v", fast, chosen, err)
		}
	}
}
