package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestEnterpriseIsolatedRotationDoesNotPublishUntilTargetOnlySaveCommits(t *testing.T) {
	store := &enterpriseBlockingStore{started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	m := NewManager(store, nil, nil)
	original := enterpriseAccountFixture(t, 3)
	original.Disabled, original.Status, original.Metadata["disabled"] = true, StatusDisabled, true
	root := registerEnterpriseFixture(t, m, original)

	other := enterpriseAccountFixture(t, 1)
	other.ID, other.FileName, other.Metadata["email"] = "unrelated-rotation.json", "unrelated-rotation.json", "unrelated@example.test"
	_, otherGraph, _ := m.EnterpriseAccountSnapshot(root.ID)
	otherGraph.ID = ge.AccountID(ge.String(other.Metadata, "email"), ge.String(other.Metadata, "client_id"))
	otherGraph.Collections = otherGraph.Collections[:1]
	other.Metadata = ge.PutAccount(other.Metadata, otherGraph)
	other = registerEnterpriseFixture(t, m, other)

	root, graph, ok := m.EnterpriseAccountSnapshot(root.ID)
	if !ok || len(graph.Collections) != 3 || !root.Disabled {
		t.Fatal("missing disabled test account")
	}
	target := graph.Collections[0]
	request := EnterpriseIsolatedRotation{Project: target.ProjectID, Location: target.Location,
		Owner: strings.Repeat("a", 32), Action: "hold",
		Engines: []string{ge.Engine(target.ResourceBinding.Apply(ge.FromMetadata(root.Metadata)))}}
	authorization := EnterpriseOAuthAuthorizationRevision(root)
	held, err := m.UpdateEnterpriseIsolatedRotation(t.Context(), root, graph.Revision, authorization, request)
	if err != nil || !held.Disabled {
		t.Fatalf("could not hold exact disabled target: %v", err)
	}
	held, graph, _ = m.EnterpriseAccountSnapshot(root.ID)
	request.Action = "activate"
	assertDisabled := func() {
		t.Helper()
		parent, current, ok := m.EnterpriseAccountSnapshot(root.ID)
		if !ok || !parent.Disabled || current.Revision != graph.Revision {
			t.Fatal("uncommitted activation changed the parent")
		}
		for _, child := range m.EnterpriseCollections(root.ID) {
			if !child.Disabled || child.Status != StatusDisabled {
				t.Fatal("uncommitted activation opened a region")
			}
			if _, err := child.enterpriseQuota.beginRotationRequest(); err == nil {
				t.Fatal("a disabled child admitted direct generation")
			}
		}
	}

	store.mu.Lock()
	store.failID = root.ID
	store.mu.Unlock()
	store.block.Store(true)
	result := make(chan error, 1)
	go func() {
		_, err := m.UpdateEnterpriseIsolatedRotation(t.Context(), held, graph.Revision, authorization, request)
		result <- err
	}()
	<-store.started
	assertDisabled()
	release()
	if err = <-result; err == nil || !strings.Contains(err.Error(), "synthetic save failure") {
		t.Fatalf("activation did not surface the store failure: %v", err)
	}
	assertDisabled()

	store.mu.Lock()
	store.failID = ""
	store.mu.Unlock()
	store.block.Store(false)
	if _, err = m.UpdateEnterpriseRotation(t.Context(), held, graph.Revision, authorization, func(*ge.Account) error { return nil }); !errors.Is(err, ErrEnterpriseAccountChanged) {
		t.Fatalf("normal rotation accepted the disabled parent: %v", err)
	}
	if _, err = m.UpdateEnterpriseIsolatedRotation(t.Context(), held, graph.Revision, authorization, request); err != nil {
		t.Fatal(err)
	}
	current, activated, ok := m.EnterpriseAccountSnapshot(root.ID)
	if !ok || current.Disabled || current.Status != StatusActive || len(activated.RotationHolds) != 0 {
		t.Fatal("target-only activation did not commit")
	}
	for _, child := range m.EnterpriseCollections(root.ID) {
		wantDisabled := EnterpriseBindingID(child) != target.ID
		if child.Disabled != wantDisabled {
			t.Fatal("activation opened a non-target child or left the target disabled")
		}
	}
	store.mu.Lock()
	saved := store.records[root.ID].Clone()
	store.mu.Unlock()
	projection, err := ProjectEnterpriseAuth(saved)
	if err != nil || len(projection) != 4 || projection[0].Disabled {
		t.Fatalf("bad saved target-only projection: %v", err)
	}
	for _, child := range projection[1:] {
		if child.Disabled != (EnterpriseBindingID(child) != target.ID) {
			t.Fatal("saved parent can reload a non-target application")
		}
	}
	unrelated, _, _ := m.EnterpriseAccountSnapshot(other.ID)
	if unrelated == nil || unrelated.Disabled || unrelated.Generation != other.Generation {
		t.Fatal("activation changed the other account")
	}
}

func TestEnterpriseRotationDrainTracksCancelledUpstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, _ := balanceTestManager(t)
		auth := sharedQuotaAuth(t, m, "rotation-drain", "111", "us", "enterprise_plus", "a")
		m.PublishEnterprisePolicyCatalog(auth, []ge.Model{{ID: ge.ArgonModel}})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := make(chan exec.StreamChunk)
		defer func() {
			if source != nil {
				close(source)
			}
		}()
		e := &enterpriseQuotaExecutor{ProviderExecutor: &sharedQuotaStreamExecutor{chunks: source}}
		stream, err := e.ExecuteStream(ctx, auth, exec.Request{Model: ge.ArgonModel}, exec.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		for range stream.Chunks {
		}
		synctest.Wait()
		if got := m.EnterpriseRotationInFlight(auth.ID); got != 1 {
			t.Fatalf("cancelled producer in flight = %d, want 1", got)
		}
		if view := m.EnterpriseQuotaBalance(auth); view.InFlight != 0 {
			t.Fatalf("the original balancing behavior changed: %+v", view)
		}
		close(source)
		source = nil
		synctest.Wait()
		if got := m.EnterpriseRotationInFlight(auth.ID); got != 0 {
			t.Fatalf("producer finished, still in flight = %d", got)
		}
	})
}
