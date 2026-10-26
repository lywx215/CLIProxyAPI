package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type enterpriseAccountStore struct {
	mu      sync.Mutex
	records map[string]*Auth
	saves   []string
	failID  string
}

func (s *enterpriseAccountStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []*Auth
	for _, auth := range s.records {
		records = append(records, auth.Clone())
	}
	return records, nil
}

func (s *enterpriseAccountStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, auth.ID)
	if auth.ID == s.failID {
		return "", fmt.Errorf("synthetic save failure")
	}
	if IsEnterpriseCollectionAuth(auth) {
		return "", errors.New("child credential was persisted")
	}
	if s.records == nil {
		s.records = make(map[string]*Auth)
	}
	s.records[auth.ID] = auth.Clone()
	return auth.ID, nil
}

func (*enterpriseAccountStore) Delete(context.Context, string) error {
	return errors.New("source deletion is forbidden")
}

func enterpriseAccountFixture(t *testing.T, collections int) *Auth {
	t.Helper()
	parent := regionalTestAuth()
	account, err := ge.AccountFromLegacy(parent.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	first := account.Collections[0]
	for i := 1; i < collections; i++ {
		collection := first
		collection.ProjectID = fmt.Sprintf("%d", 123+i)
		collection.ID = ge.CollectionID(collection.ResourceBinding)
		account.Collections = append(account.Collections, collection)
	}
	for _, key := range []string{"project_id", "location", "collection_id", "engine_id", "assistant_id"} {
		delete(parent.Metadata, key)
	}
	parent.Metadata = ge.PutAccount(parent.Metadata, account)
	return parent
}

func registerEnterpriseFixture(t *testing.T, m *Manager, parent *Auth) *Auth {
	t.Helper()
	saved, err := m.Register(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, auth := range m.List() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		}
	})
	return saved
}

func TestEnterpriseAccountProjectsAllChildrenAndIsolatesSettings(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 3))
	if !IsEnterpriseAccountAuth(parent) || parent.Index == "" {
		t.Fatal("missing parent identity")
	}
	if _, scheduled := m.scheduler.authProviders[parent.ID]; scheduled {
		t.Fatal("parent was inserted into the inference scheduler")
	}
	children := m.EnterpriseCollections(parent.Index)
	if len(children) != 3 || len(m.List()) != 4 {
		t.Fatalf("projection count=%d runtime=%d", len(children), len(m.List()))
	}
	indices := map[string]bool{parent.Index: true}
	for _, child := range children {
		if !IsEnterpriseCollectionAuth(child) || EnterpriseOwnerID(child) != parent.ID || indices[child.Index] || IsPluginVirtualAuth(child) {
			t.Fatal("child identity reused a parent or plugin identity")
		}
		indices[child.Index] = true
		if _, _, schedulable := isSchedulableAuth(parent); schedulable {
			t.Fatal("parent was schedulable")
		}
	}
	first := children[0]
	m.MarkResult(t.Context(), Result{AuthID: first.ID, Provider: ge.Provider, Model: "model", Success: true, RegistrationEpoch: first.RegistrationEpoch, CredentialVersion: first.CredentialVersion})
	_, graph, _ := m.EnterpriseAccountSnapshot(parent.ID)
	updated, err := m.UpdateEnterpriseAccount(t.Context(), parent, graph.Revision, func(account *ge.Account) error {
		account.Collections[0].Disabled = true
		account.Collections[0].Settings = map[string]any{"prefix": "first", "proxy_url": "http://proxy.invalid", "model_aliases": []any{map[string]any{"name": "upstream", "alias": "custom"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := m.EnterpriseCollections(updated.ID)
	if !after[0].Disabled || after[0].Prefix != "first" || after[0].ProxyURL != "http://proxy.invalid" || after[0].Success != 1 || len(OAuthModelAliasesFromAttributes(after[0].Attributes)) != 1 {
		t.Fatal("child settings or counters were lost")
	}
	if after[1].Disabled || after[1].Prefix != "" || after[1].Success != 0 {
		t.Fatal("sibling settings or counters were changed")
	}
	updated.Disabled, updated.Status = true, StatusDisabled
	if _, err = m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	for _, child := range m.EnterpriseCollections(parent.ID) {
		if !child.Disabled {
			t.Fatal("parent disable did not reach every child")
		}
	}
	updated, _ = m.GetByID(parent.ID)
	updated.Disabled, updated.Status = false, StatusActive
	if _, err = m.Update(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	after = m.EnterpriseCollections(parent.ID)
	if !after[0].Disabled || after[1].Disabled || after[0].Success != 1 {
		t.Fatal("parent off/on reset an independent collection preference")
	}
}

type enterpriseSharedRefreshExecutor struct {
	*mockOAuthErrorExecutor
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	badRoot atomic.Bool
}

func (e *enterpriseSharedRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if !IsEnterpriseAccountAuth(auth) || IsEnterpriseCollectionAuth(auth) {
		e.badRoot.Store(true)
	}
	if e.calls.Add(1) == 1 {
		close(e.started)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.release:
	}
	auth.Metadata["access_token"], auth.Metadata["refresh_token"] = "synthetic-new", "synthetic-rotated"
	auth.Metadata["expired"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	return auth, nil
}

func TestEnterpriseAccountSharedRefreshPersistsOnceAndDetachesCancellation(t *testing.T) {
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	fixture := enterpriseAccountFixture(t, 3)
	fixture.Disabled, fixture.Status = true, StatusDisabled
	fixture.Metadata["expired"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	parent := registerEnterpriseFixture(t, m, fixture)
	children := m.EnterpriseCollections(parent.ID)
	executor := &enterpriseSharedRefreshExecutor{mockOAuthErrorExecutor: &mockOAuthErrorExecutor{id: ge.Provider}, started: make(chan struct{}), release: make(chan struct{})}
	m.RegisterExecutor(executor)
	cancelCtx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() { _, err := m.EnsureEnterpriseAccountToken(cancelCtx, children[0].ID); firstDone <- err }()
	<-executor.started
	const waiters = 6
	var wg sync.WaitGroup
	errorsOut := make(chan error, waiters)
	for i := range waiters {
		wg.Go(func() {
			current, err := m.EnsureEnterpriseAccountToken(t.Context(), children[i%len(children)].Index)
			if err == nil && (current == nil || !current.Disabled || ge.String(current.Metadata, "access_token") != "synthetic-new") {
				err = errors.New("refresh did not return the requested disabled child")
			}
			errorsOut <- err
		})
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error=%v", err)
	}
	close(executor.release)
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if executor.calls.Load() != 1 || executor.badRoot.Load() {
		t.Fatalf("refresh calls=%d bad root=%v", executor.calls.Load(), executor.badRoot.Load())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saves) != 2 || len(store.records) != 1 || ge.String(store.records[parent.ID].Metadata, "refresh_token") != "synthetic-rotated" {
		t.Fatalf("refresh did not save the parent exactly once: saves=%d records=%d", len(store.saves), len(store.records))
	}
}

func TestEnterpriseAccountMutationPreservesLatestTokensAndFencesOldBinding(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 2))
	stale := parent.Clone()
	rotated := parent.Clone()
	rotated.Metadata["access_token"], rotated.Metadata["refresh_token"] = "synthetic-rotated-access", "synthetic-rotated-refresh"
	if _, err := m.UpdateRefreshedAuth(t.Context(), parent, rotated); err != nil {
		t.Fatal(err)
	}
	before := m.EnterpriseCollections(parent.ID)[0]
	m.MarkResult(t.Context(), Result{AuthID: before.ID, Provider: ge.Provider, Success: true, RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion})
	updated, err := m.UpdateEnterpriseAccount(t.Context(), stale, 0, func(account *ge.Account) error {
		account.Collections[0].EngineID = "replacement"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, graph, _ := m.EnterpriseAccountSnapshot(updated.ID)
	after := m.EnterpriseCollections(parent.ID)[0]
	if ge.String(updated.Metadata, "access_token") != "synthetic-rotated-access" || after.Success != 1 || graph.Revision != 2 || graph.Collections[0].Revision != 2 || after.RegistrationEpoch <= before.RegistrationEpoch {
		t.Fatal("graph mutation lost tokens, counters, revisions, or its binding fence")
	}
	m.MarkResult(t.Context(), Result{AuthID: before.ID, Provider: ge.Provider, Model: "model", RegistrationEpoch: before.RegistrationEpoch, CredentialVersion: before.CredentialVersion, Error: &Error{HTTPStatus: 429, Message: "old binding"}})
	after, _ = m.GetByID(after.ID)
	if after.Failed != 0 || after.Unavailable || len(after.ModelStates) != 0 {
		t.Fatal("old binding completion mutated its replacement")
	}
	if _, err = m.UpdateEnterpriseAccount(t.Context(), stale, 1, func(*ge.Account) error { return nil }); !errors.Is(err, ErrEnterpriseAccountChanged) {
		t.Fatalf("stale graph revision accepted: %v", err)
	}
}

func TestEnterpriseAccountMergeSaveFirstRetireRecoverablyAndRetry(t *testing.T) {
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 1))
	source := regionalTestAuth()
	source.ID, source.FileName = "legacy-other.json", "legacy-other.json"
	source.Metadata["project_id"], source.Metadata["prefix"] = "456", "old-prefix"
	source, err := m.Register(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	oldIndex := source.Index
	m.MarkResult(t.Context(), Result{AuthID: source.ID, Provider: ge.Provider, Success: true, RegistrationEpoch: source.RegistrationEpoch, CredentialVersion: source.CredentialVersion})
	store.failID = source.ID
	updated, merged, err := m.MergeEnterpriseAccounts(t.Context(), parent.ID, []string{source.ID})
	if err == nil || updated == nil || len(merged) != 1 {
		t.Fatal("partial retirement did not preserve/report the saved target")
	}
	children := m.EnterpriseCollections(parent.ID)
	if len(children) != 2 || children[1].Prefix != "old-prefix" || children[1].Success != 1 {
		t.Fatal("merge lost the source binding, configuration or counters")
	}
	if got := m.ResolveEnterpriseInferenceID(oldIndex); got != children[1].ID {
		t.Fatalf("legacy index routed to %s", got)
	}
	retired, _ := m.GetByID(source.ID)
	if !retired.Disabled || ge.String(retired.Metadata, enterpriseRetiredKey) != parent.ID {
		t.Fatal("failed retirement remained schedulable")
	}
	store.failID = ""
	if _, _, err = m.MergeEnterpriseAccounts(t.Context(), parent.ID, []string{source.ID}); err != nil {
		t.Fatal(err)
	}
	children = m.EnterpriseCollections(parent.ID)
	if children[1].Success != 1 {
		t.Fatal("merge retry counted legacy traffic twice")
	}
	m.MarkResult(t.Context(), Result{AuthID: source.ID, Provider: ge.Provider, Success: true, RegistrationEpoch: source.RegistrationEpoch, CredentialVersion: source.CredentialVersion})
	children = m.EnterpriseCollections(parent.ID)
	if children[1].Success != 2 {
		t.Fatal("in-flight legacy traffic lost its collection attribution")
	}
	store.mu.Lock()
	rootFirst := store.saves[2] == parent.ID && store.saves[3] == source.ID
	store.mu.Unlock()
	if !rootFirst {
		t.Fatal("source retired before the target was durably saved")
	}
	restored := NewManager(store, nil, nil)
	if err = restored.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(restored.EnterpriseCollections(parent.ID)) != 2 {
		t.Fatal("load lost merged children")
	}
	m.Remove(t.Context(), parent.ID)
	for _, auth := range m.List() {
		if IsEnterpriseCollectionAuth(auth) && EnterpriseOwnerID(auth) == parent.ID {
			t.Fatal("account removal left a child in runtime")
		}
	}
}

func TestEnterpriseAccountMergeTargetFailureAndIdentityMismatchKeepSources(t *testing.T) {
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 1))
	source := regionalTestAuth()
	source.ID, source.FileName = "source.json", "source.json"
	source.Metadata["project_id"] = "456"
	source, _ = m.Register(t.Context(), source)
	store.failID = parent.ID
	if _, _, err := m.MergeEnterpriseAccounts(t.Context(), parent.ID, []string{source.ID}); err == nil {
		t.Fatal("target save failure accepted")
	}
	current, _ := m.GetByID(source.ID)
	if current.Disabled || len(m.EnterpriseCollections(parent.ID)) != 1 {
		t.Fatal("failed merge retired a source or changed the target")
	}
	store.failID = ""
	current.Metadata["email"] = "other@example.test"
	if _, err := m.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MergeEnterpriseAccounts(t.Context(), parent.ID, []string{source.ID}); !errors.Is(err, ErrEnterpriseAccountChanged) {
		t.Fatalf("different verified principal accepted: %v", err)
	}
}

func TestEnterpriseAccountCloneAndCanonicalProjectMigration(t *testing.T) {
	m := NewManager(nil, nil, nil)
	fixture := enterpriseAccountFixture(t, 1)
	account, _ := ge.ReadAccount(fixture.Metadata)
	account.Collections[0].ProjectID = "legacy-slug"
	account.Collections[0].ID = ge.CollectionID(account.Collections[0].ResourceBinding)
	fixture.Metadata = ge.PutAccount(fixture.Metadata, *account)
	parent := registerEnterpriseFixture(t, m, fixture)
	old := m.EnterpriseCollections(parent.ID)[0]
	m.MarkResult(t.Context(), Result{AuthID: old.ID, Provider: ge.Provider, Success: true, RegistrationEpoch: old.RegistrationEpoch, CredentialVersion: old.CredentialVersion})
	updated, err := m.UpdateEnterpriseAccount(t.Context(), parent, 0, func(account *ge.Account) error {
		account.Collections[0].LegacyAliases = append(account.Collections[0].LegacyAliases, account.Collections[0].ID)
		account.Collections[0].ProjectID = "123"
		account.Collections[0].ID = ge.CollectionID(account.Collections[0].ResourceBinding)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	child := m.EnterpriseCollections(updated.ID)[0]
	if child.ID == old.ID || child.Success != 1 || m.ResolveEnterpriseInferenceID(old.ID) != child.ID || m.ResolveEnterpriseInferenceID(old.Index) != child.ID {
		t.Fatal("canonical project migration lost legacy identity or counters")
	}
	copyAuth := updated.Clone()
	graph := copyAuth.Metadata["enterprise_account"].(ge.Account)
	graph.Collections[0].LegacyAliases[0] = "changed"
	copyAuth.Metadata["enterprise_account"] = graph
	_, currentGraph, _ := m.EnterpriseAccountSnapshot(parent.ID)
	if currentGraph.Collections[0].LegacyAliases[0] == "changed" {
		t.Fatal("nested graph clone shared mutable aliases")
	}
}

func TestEnterpriseAccountLegacySettingsStayOnOriginalCollection(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{OAuthExcludedModels: map[string][]string{ge.Provider: {"global-excluded"}}})
	legacy := regionalTestAuth()
	legacy.Prefix, legacy.ProxyURL = "original", "http://original.invalid"
	legacy.Metadata["prefix"], legacy.Metadata["proxy_url"], legacy.Metadata["weight"] = legacy.Prefix, legacy.ProxyURL, 7
	legacy.Metadata["model_aliases"] = []any{map[string]any{"name": "upstream", "alias": "original-model"}}
	legacy.Metadata["excluded_models"] = []any{"original-excluded"}
	legacy = registerEnterpriseFixture(t, m, legacy)
	updated, err := m.UpdateEnterpriseAccount(t.Context(), legacy, 0, func(account *ge.Account) error {
		other := account.Collections[0]
		other.ProjectID = "456"
		other.ID = ge.CollectionID(other.ResourceBinding)
		other.Settings, other.LegacyAliases = nil, nil
		account.Collections = append(account.Collections, other)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	children := m.EnterpriseCollections(updated.ID)
	if children[0].Prefix != "original" || children[0].ProxyURL != "http://original.invalid" || children[0].Attributes["weight"] != "7" || len(OAuthModelAliasesFromAttributes(children[0].Attributes)) != 1 {
		t.Fatal("legacy collection settings were lost")
	}
	if children[1].Prefix != "" || children[1].ProxyURL != "" || children[1].Attributes["weight"] != "" || len(OAuthModelAliasesFromAttributes(children[1].Attributes)) != 0 || children[1].Attributes["excluded_models"] != "global-excluded" {
		t.Fatal("legacy settings leaked into a newly discovered collection")
	}
	if m.ResolveEnterpriseInferenceID(legacy.Index) != children[0].ID || m.ResolveEnterpriseInferenceID(updated.Index) != updated.Index {
		t.Fatal("legacy inference and account administration indices were conflated")
	}
	m.RegisterExecutor(&mockOAuthErrorExecutor{id: ge.Provider})
	selected, _, err := m.pickNextLegacy(t.Context(), ge.Provider, "", coreexec.Options{Metadata: map[string]any{coreexec.PinnedAuthMetadataKey: legacy.Index}}, nil)
	if err != nil || selected == nil || selected.ID != children[0].ID {
		t.Fatal("legacy index did not pin the original collection", err)
	}
}

func TestEnterpriseAccountMergeReparentsExistingChildFromDuplicateAccount(t *testing.T) {
	m := NewManager(nil, nil, nil)
	source := enterpriseAccountFixture(t, 2)
	source.ID, source.FileName = "first-account.json", "first-account.json"
	source = registerEnterpriseFixture(t, m, source)
	firstChild := m.EnterpriseCollections(source.ID)[0]
	m.MarkResult(t.Context(), Result{AuthID: firstChild.ID, Provider: ge.Provider, Success: true, RegistrationEpoch: firstChild.RegistrationEpoch, CredentialVersion: firstChild.CredentialVersion})
	target := enterpriseAccountFixture(t, 1)
	target.ID, target.FileName = "selected-account.json", "selected-account.json"
	target = registerEnterpriseFixture(t, m, target)
	updated, _, err := m.MergeEnterpriseAccounts(t.Context(), target.ID, []string{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	children := m.EnterpriseCollections(updated.ID)
	if len(children) != 2 || children[0].Success != 1 || EnterpriseOwnerID(children[0]) != target.ID {
		t.Fatal("existing child state was not reparented to the selected account")
	}
}

func TestEnterpriseAccountCooldownSurvivesDisableAndRejectsReboundRestore(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 1))
	child := m.EnterpriseCollections(parent.ID)[0]
	m.MarkResult(t.Context(), Result{AuthID: child.ID, Provider: ge.Provider, Model: "model", RegistrationEpoch: child.RegistrationEpoch, CredentialVersion: child.CredentialVersion, Error: &Error{HTTPStatus: 429, Message: "synthetic rate limit"}})
	parent.Disabled, parent.Status = true, StatusDisabled
	if _, err := m.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	records := m.cooldownStateRecordsSnapshot()
	if len(records) == 0 || records[0].EnterpriseBinding == "" || records[0].AuthID != child.ID {
		t.Fatal("disabled parent discarded child cooldown persistence")
	}
	parent, _ = m.GetByID(parent.ID)
	parent.Disabled, parent.Status = false, StatusActive
	if _, err := m.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateEnterpriseAccount(t.Context(), parent, 0, func(account *ge.Account) error {
		account.Collections[0].EngineID = "new-app"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	for _, record := range records {
		if m.restoreCooldownRecordLocked(record, time.Now()) {
			m.mu.Unlock()
			t.Fatal("old binding cooldown restored onto a new engine")
		}
	}
	m.mu.Unlock()
}

type enterpriseBlockingStore struct {
	enterpriseAccountStore
	block   atomic.Bool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *enterpriseBlockingStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if s.block.Load() {
		s.once.Do(func() { close(s.started) })
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-s.release:
		}
	}
	return s.enterpriseAccountStore.Save(ctx, auth)
}

func TestEnterpriseAccountReauthorizationRebasesAndSerializesConcurrentPatch(t *testing.T) {
	store := &enterpriseBlockingStore{started: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(store, nil, nil)
	staleConstructor := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 1))
	if _, err := m.UpdateEnterpriseAccount(t.Context(), staleConstructor, 0, func(account *ge.Account) error {
		account.Collections[0].Disabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	staleConstructor.Metadata["access_token"] = "freshly-authorized-token"
	store.block.Store(true)
	reauthorized := make(chan error, 1)
	go func() { _, err := m.ReauthorizeEnterpriseAccount(t.Context(), staleConstructor); reauthorized <- err }()
	<-store.started
	patchStarted, patched := make(chan struct{}), make(chan error, 1)
	go func() {
		close(patchStarted)
		_, err := m.UpdateEnterpriseAccount(t.Context(), staleConstructor, 0, func(account *ge.Account) error {
			account.Collections[0].Settings = map[string]any{"prefix": "concurrent-choice"}
			return nil
		})
		patched <- err
	}()
	<-patchStarted
	select {
	case err := <-patched:
		t.Fatal("child patch crossed the parent save transaction", err)
	default:
	}
	close(store.release)
	if err := <-reauthorized; err != nil {
		t.Fatal(err)
	}
	if err := <-patched; err != nil {
		t.Fatal(err)
	}
	parent, graph, ok := m.EnterpriseAccountSnapshot(staleConstructor.ID)
	if !ok || ge.String(parent.Metadata, "access_token") != "freshly-authorized-token" || !graph.Collections[0].Disabled || graph.Collections[0].Settings["prefix"] != "concurrent-choice" || graph.Revision != 3 {
		t.Fatal("reauthorization replayed an old graph or lost the concurrent patch")
	}
	store.mu.Lock()
	saved, _ := ge.ReadAccount(store.records[parent.ID].Metadata)
	store.mu.Unlock()
	if saved.Collections[0].Settings["prefix"] != "concurrent-choice" || !saved.Collections[0].Disabled {
		t.Fatal("live reauthorization state was not durably saved")
	}
}

func TestEnterpriseAccountRotatedTokenSaveFailureRetriesPersistenceOnly(t *testing.T) {
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	fixture := enterpriseAccountFixture(t, 1)
	fixture.Metadata["expired"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	parent := registerEnterpriseFixture(t, m, fixture)
	executor := &enterpriseSharedRefreshExecutor{mockOAuthErrorExecutor: &mockOAuthErrorExecutor{id: ge.Provider}, started: make(chan struct{}), release: make(chan struct{})}
	close(executor.release)
	m.RegisterExecutor(executor)
	store.failID = parent.ID
	if _, err := m.EnsureEnterpriseAccountToken(t.Context(), parent.ID); err == nil {
		t.Fatal("failed token persistence was reported as success")
	}
	current, _ := m.GetByID(parent.ID)
	if ge.String(current.Metadata, "refresh_token") != "synthetic-rotated" {
		t.Fatal("a failed save discarded the already rotated refresh token")
	}
	store.failID = ""
	child := m.EnterpriseCollections(parent.ID)[0]
	if _, err := m.EnsureEnterpriseAccountToken(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	saved := store.records[parent.ID].Clone()
	store.mu.Unlock()
	if executor.calls.Load() != 1 || ge.String(saved.Metadata, "refresh_token") != "synthetic-rotated" {
		t.Fatal("persistence retry repeated OAuth acquisition or lost its token")
	}
}
