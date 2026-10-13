package budget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func expectChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
	default:
		t.Fatal("admission change did not notify subscribers")
	}
}

func expectNoChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
		t.Fatal("read-only operation notified admission subscribers")
	default:
	}
}

func TestAdmissionChangesSubscriptionDoesNotWrite(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	before, err := os.ReadFile(filepath.Join(e.path, "ledger.wal"))
	if err != nil {
		t.Fatal(err)
	}
	changes := e.Changes()
	if changes == nil || changes != e.Changes() {
		t.Fatal("subscription was not lazily initialized or stable")
	}
	_ = e.View(Scope{Project: "one", Model: "gemini-3-flash-preview"})
	_ = e.Snapshot()
	expectNoChange(t, changes)
	after, err := os.ReadFile(filepath.Join(e.path, "ledger.wal"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("subscription changed WAL: %v", err)
	}
	initializeTest(t, e)
	expectChange(t, changes)
	changes = e.Changes()
	if claimed, errClaim := e.ClaimBackup(context.Background(), e.now()); errClaim != nil || !claimed {
		t.Fatalf("backup claim: %v, %v", claimed, errClaim)
	}
	if errResult := e.SetBackupResult(context.Background(), e.now(), true, ""); errResult != nil {
		t.Fatal(errResult)
	}
	expectNoChange(t, changes)
}

func TestAdmissionChangesReleasePublishesAvailableState(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	ctx := WithRequest(t.Context())
	ticket, err := e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.TryStartTransmission(ctx); err != nil {
		t.Fatal(err)
	}
	// Subscribe before checking the state, matching the waiter's lost-wakeup barrier.
	changes := e.Changes()
	if view := e.View(scope); view.State != "busy" || view.InFlight != 1 {
		t.Fatalf("reservation did not hold its slot: %+v", view)
	}
	ticket.Release()
	expectChange(t, changes)
	if view := e.View(scope); !view.Available || view.InFlight != 0 || view.Used != 1 {
		t.Fatalf("released notification preceded available state: %+v", view)
	}
}

func TestAdmissionChangesObservationFailurePublishesFailClosedState(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	e.beforePersist = func(operation string) error {
		if operation == "entry" {
			return errors.New("simulated persistent volume failure")
		}
		return nil
	}
	changes := e.Changes()
	if err := e.Observe(t.Context(), scope, Signal{Kind: SignalRPM}); errorCode(err) != "budget_storage_unavailable" {
		t.Fatalf("observation failure was not fail-closed: %v", err)
	}
	expectChange(t, changes)
	if view := e.View(scope); view.Available || view.State != StateStorageError {
		t.Fatalf("failure notification preceded fail-closed state: %+v", view)
	}
}

func TestAdmissionChangesUpdateCloseAndRecovery(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	changes := e.Changes()
	if err := e.UpdateModels(map[string]ModelPolicy{scope.Model: {DailyLimit: 1}}); err != nil {
		t.Fatal(err)
	}
	expectChange(t, changes)
	if view := e.View(scope); view.Limit != 1 {
		t.Fatalf("model update notification was stale: %+v", view)
	}
	changes = e.Changes()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	expectChange(t, changes)
	if view := e.View(scope); view.State != StateClosed {
		t.Fatalf("close notification was stale: %+v", view)
	}

	recovered := openTest(t, clock, t.TempDir(), false)
	changes = recovered.Changes()
	if err := recovered.Restore(t.Context(), Snapshot{Version: 1, Initialized: true, State: StateReady}); err != nil {
		t.Fatal(err)
	}
	expectChange(t, changes)
	if view := recovered.View(scope); view.State != StateDisaster {
		t.Fatalf("restore notification was stale: %+v", view)
	}
	clock.Set(recovered.Snapshot().FreezeUntil.Add(time.Minute))
	changes = recovered.Changes()
	ctx := WithRequest(t.Context())
	ticket, err := recovered.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	expectChange(t, changes)
	if recovered.state.State != StateReady {
		t.Fatal("recovery notification preceded durable resume")
	}
	if err = ticket.Compensate(t.Context()); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
}

func TestAdmissionTurnReleaseIsOnceAndPreservesRequestCounter(t *testing.T) {
	var releases atomic.Int32
	ctx := WithRequest(t.Context())
	ctx = WithAdmissionTurn(ctx, func() { releases.Add(1) })
	scope := Scope{Project: "one", Model: "unlisted"}
	if err := StartUnmeteredTransmission(ctx, scope); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			ReleaseAdmissionTurn(WithRequest(context.WithoutCancel(ctx)))
		}()
	}
	group.Wait()
	if releases.Load() != 1 || Attempts(ctx) != 1 || !HasAttemptedProject(ctx, scope) {
		t.Fatalf("release reset request state: callbacks=%d attempts=%d", releases.Load(), Attempts(ctx))
	}
	ReleaseAdmissionTurn(nil)
	ReleaseAdmissionTurn(WithAdmissionTurn(nil, nil))
}
