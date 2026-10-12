package budget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time    { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Set(now time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = now }
func newClock() *testClock             { return &testClock{now: time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC)} }
func testModels() map[string]ModelPolicy {
	return map[string]ModelPolicy{"gemini-3-flash-preview": {DailyLimit: 20}, "gemini-3.1-flash-lite-preview": {DailyLimit: 500}}
}
func openTest(t *testing.T, clock *testClock, path string, initialized bool) *Engine {
	t.Helper()
	e, err := Open(context.Background(), Options{Path: path, Models: testModels(), Now: clock.Now, ExpectedInitialized: initialized, Jitter: func(time.Duration) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if errClose := e.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	return e
}
func initializeTest(t *testing.T, e *Engine, options ...InitializeOptions) {
	t.Helper()
	if err := e.Initialize(context.Background(), options...); err != nil {
		t.Fatal(err)
	}
}
func sendTest(t *testing.T, e *Engine, scope Scope) {
	t.Helper()
	ctx := WithRequest(context.Background())
	ticket, err := e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.TryStartTransmission(ctx); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
}
func usedFor(e *Engine, project, model string) int {
	for _, entry := range e.Snapshot().Entries {
		if entry.Day == e.Snapshot().Day && entry.Project == project && entry.Model == model {
			return entry.Used
		}
	}
	return 0
}
func errorCode(err error) string {
	var value *Error
	if errors.As(err, &value) {
		return value.Code
	}
	return ""
}

func TestProjectAndRealModelDailyIsolation(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	flash := Scope{Project: "one-project", Model: "gemini-3-flash-preview", AuthIndex: "first"}
	lite := Scope{Project: flash.Project, Model: "gemini-3.1-flash-lite-preview", AuthIndex: "second"}
	initializeTest(t, e, InitializeOptions{Used: []InitialUsage{{Scope: flash, Used: 19}, {Scope: lite, Used: 499}}})
	flash.Model = "models/gemini-3-flash-preview(8192)"
	sendTest(t, e, flash)
	flash.AuthIndex = "different-key-same-project"
	if _, err := e.Reserve(WithRequest(context.Background()), flash); errorCode(err) != "daily_budget_exhausted" {
		t.Fatalf("flash did not share budget: %v", err)
	}
	if !e.Available(lite) {
		t.Fatal("flash exhausted lite")
	}
	sendTest(t, e, lite)
	if _, err := e.Reserve(WithRequest(context.Background()), lite); errorCode(err) != "daily_budget_exhausted" {
		t.Fatalf("lite limit: %v", err)
	}
	sendTest(t, e, Scope{Project: "independent", Model: "gemini-3-flash-preview"})
}

func TestFinalQuotaConcurrentReservation(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	scope := Scope{Project: "shared", Model: "gemini-3-flash-preview"}
	initializeTest(t, e, InitializeOptions{Used: []InitialUsage{{Scope: scope, Used: 19}}})
	var winners atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ctx := WithRequest(context.Background())
			ticket, err := e.Reserve(ctx, scope)
			if err != nil {
				return
			}
			if err = ticket.TryStartTransmission(ctx); err != nil {
				t.Error(err)
				return
			}
			winners.Add(1)
			ticket.Release()
		}()
	}
	wait.Wait()
	if winners.Load() != 1 || usedFor(e, scope.Project, scope.Model) != 20 {
		t.Fatalf("winners=%d used=%d", winners.Load(), usedFor(e, scope.Project, scope.Model))
	}
}

func TestSingleInFlightAndIdempotentCompensation(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "shared", Model: "gemini-3-flash-preview"}
	ctx := WithRequest(context.Background())
	ticket, err := e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if e.InFlight(scope) != 1 || e.Available(scope) {
		t.Fatal("in-flight admission hint is incorrect")
	}
	if _, err = e.Reserve(ctx, Scope{Project: scope.Project, Model: scope.Model, AuthIndex: "other"}); errorCode(err) != "budget_scope_busy" {
		t.Fatalf("second in flight: %v", err)
	}
	if err = ticket.Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = ticket.Compensate(ctx); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
	ticket.Release()
	if usedFor(e, scope.Project, scope.Model) != 0 || e.InFlight(scope) != 0 {
		t.Fatal("compensation duplicated or release retained ownership")
	}
	ticket, err = e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.TryStartTransmission(ctx); err != nil {
		t.Fatal(err)
	}
	if errorCode(ticket.Compensate(ctx)) != "budget_transmission_started" {
		t.Fatal("started request was refunded")
	}
	if errorCode(ticket.TryStartTransmission(ctx)) != "budget_ticket_already_used" || Attempts(ctx) != 1 {
		t.Fatal("ticket could be transmitted twice")
	}
	ticket.Release()
	if usedFor(e, scope.Project, scope.Model) != 1 {
		t.Fatal("release refunded a transmission")
	}
}

func TestThreePhysicalAttemptsSharedAcrossContexts(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	ctx := WithEngine(WithRequest(context.Background()), e)
	derived := WithRequest(context.WithValue(ctx, struct{}{}, true))
	if EngineFromContext(derived) != e {
		t.Fatal("engine context was lost")
	}
	for index := range 3 {
		scope := Scope{Project: fmt.Sprintf("project-%d", index), Model: "gemini-3-flash-preview"}
		ticket, err := e.Reserve(derived, scope)
		if err != nil {
			t.Fatal(err)
		}
		if err = ticket.TryStartTransmission(derived); err != nil {
			t.Fatal(err)
		}
		ticket.Release()
		if !HasAttemptedProject(ctx, scope) {
			t.Fatal("project attempt was not retained")
		}
	}
	if _, err := e.Reserve(derived, Scope{Project: "fourth", Model: "gemini-3-flash-preview"}); errorCode(err) != "request_attempt_limit" {
		t.Fatalf("fourth: %v", err)
	}
	if Attempts(ctx) != 3 {
		t.Fatal("derived context reset the attempt counter")
	}
}

func TestOneProjectPerClientRequest(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	ctx := WithRequest(context.Background())
	scope := Scope{Project: "same", Model: "gemini-3-flash-preview", AuthIndex: "first"}
	ticket, err := e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.TryStartTransmission(ctx); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
	scope.AuthIndex = "second"
	if _, err = e.Reserve(ctx, scope); errorCode(err) != "budget_project_already_attempted" {
		t.Fatalf("duplicate project: %v", err)
	}
	if usedFor(e, scope.Project, scope.Model) != 1 {
		t.Fatal("duplicate project burned budget")
	}
}

func TestRequestCancelledBeforeTransmissionCanBeCompensated(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	ctx, cancel := context.WithCancel(WithRequest(context.Background()))
	ticket, err := e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if !errors.Is(ticket.TryStartTransmission(ctx), context.Canceled) {
		t.Fatal("cancelled request could start")
	}
	if err = ticket.Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
	if usedFor(e, scope.Project, scope.Model) != 0 || Attempts(ctx) != 0 {
		t.Fatal("cancelled pre-transport request was counted")
	}
}

func TestStartCompensationAtomicBoundary(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	for index := range 30 {
		ctx := WithRequest(context.Background())
		scope := Scope{Project: fmt.Sprintf("race-%d", index), Model: "gemini-3-flash-preview"}
		ticket, err := e.Reserve(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		var wait sync.WaitGroup
		wait.Add(2)
		go func() { defer wait.Done(); _ = ticket.TryStartTransmission(ctx) }()
		go func() { defer wait.Done(); _ = ticket.Compensate(context.Background()) }()
		wait.Wait()
		used := usedFor(e, scope.Project, scope.Model)
		if (ticket.Started() && used != 1) || (!ticket.Started() && used != 0) {
			t.Fatalf("started=%v used=%d", ticket.Started(), used)
		}
		ticket.Release()
	}
}

func TestJournalReplayCheckpointAndIncompleteTail(t *testing.T) {
	clock := newClock()
	path := t.TempDir()
	e := openTest(t, clock, path, false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	sendTest(t, e, scope)
	if err := e.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(path, "ledger.wal"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte{0, 0, 0, 12, '{', 'x'}); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	e = openTest(t, clock, path, true)
	if e.Snapshot().State != StateReady || usedFor(e, scope.Project, scope.Model) != 1 {
		t.Fatal("replay lost a durable reservation")
	}
	sendTest(t, e, scope)
	if usedFor(e, scope.Project, scope.Model) != 2 {
		t.Fatal("append after truncated tail failed")
	}
}

func TestBudgetPolicyUpdateDoesNotResetUsed(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	sendTest(t, e, scope)
	if err := e.UpdateModels(map[string]ModelPolicy{scope.Model: {DailyLimit: 1}}); err != nil {
		t.Fatal(err)
	}
	if e.Available(scope) || usedFor(e, scope.Project, scope.Model) != 1 {
		t.Fatal("policy update reset or bypassed budget")
	}
}

func TestPersistFailureCannotAdmitOrCountTransmission(t *testing.T) {
	clock := newClock()
	var fail atomic.Bool
	e, err := Open(context.Background(), Options{Path: t.TempDir(), Models: testModels(), Now: clock.Now, BeforePersist: func(op string) error {
		if fail.Load() && op == "reserve" {
			return errors.New("injected disk failure")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	initializeTest(t, e)
	fail.Store(true)
	ctx := WithRequest(context.Background())
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	if _, err = e.Reserve(ctx, scope); errorCode(err) != "budget_storage_unavailable" {
		t.Fatalf("storage failure: %v", err)
	}
	if Attempts(ctx) != 0 || usedFor(e, scope.Project, scope.Model) != 0 {
		t.Fatal("failed persistence admitted a request")
	}
}
