package budget

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRPDSignalStopsOnlyOneProjectRealModel(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalRPD}); err != nil {
		t.Fatal(err)
	}
	if e.Available(scope) || e.View(scope).State != "daily_exhausted" {
		t.Fatal("RPD did not stop daily model pool")
	}
	if !e.Available(Scope{Project: "one", Model: "gemini-3.1-flash-lite-preview"}) || !e.Available(Scope{Project: "two", Model: scope.Model}) {
		t.Fatal("RPD propagated to unrelated model/project")
	}
	clock.Set(e.nextReset(clock.Now()).Add(time.Second))
	if !e.Available(scope) {
		t.Fatal("daily block did not reset")
	}
}

func TestUnknown429BackoffDoesNotBecomeDailyExhaustion(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	for _, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute} {
		now := clock.Now()
		if err := e.Observe(context.Background(), scope, Signal{Kind: SignalUnknown429}); err != nil {
			t.Fatal(err)
		}
		view := e.View(scope)
		if view.State != "cooldown" || view.RetryAt.Sub(now) != delay {
			t.Fatalf("view=%+v want delay=%v", view, delay)
		}
		if err := e.Observe(context.Background(), scope, Signal{Kind: SignalUnknown429}); err != nil {
			t.Fatal(err)
		}
		if !e.View(scope).RetryAt.Equal(view.RetryAt) {
			t.Fatal("concurrent duplicate failure advanced backoff")
		}
		clock.Set(view.RetryAt.Add(time.Second))
	}
	if !e.Available(scope) {
		t.Fatal("unknown 429 blocked until midnight")
	}
}

func TestTemporaryAuthFailureRecoversWithoutQuarantine(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview", CredentialID: "same-key"}
	if err := e.Observe(t.Context(), scope, Signal{Kind: SignalTemporaryAuth}); err != nil {
		t.Fatal(err)
	}
	view := e.View(scope)
	if view.State != "cooldown" || view.RetryAt.Sub(clock.Now()) != time.Minute {
		t.Fatalf("unexpected temporary auth state: %+v", view)
	}
	clock.Set(view.RetryAt.Add(time.Second))
	if !e.Available(scope) {
		t.Fatal("unclassified authorization failure permanently quarantined key")
	}
}

func Test503BreakerRequiresDistinctProjectsAndFencesHalfOpenSuccess(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	model := "gemini-3-flash-preview"
	for range 3 {
		if err := e.Observe(context.Background(), Scope{Project: "one", Model: model}, Signal{Kind: Signal503}); err != nil {
			t.Fatal(err)
		}
	}
	if e.View(Scope{Project: "one", Model: model}).State != "cooldown" || !e.Available(Scope{Project: "independent", Model: model}) {
		t.Fatal("one project opened a global breaker")
	}
	for _, project := range []string{"two", "three"} {
		if err := e.Observe(context.Background(), Scope{Project: project, Model: model}, Signal{Kind: Signal503}); err != nil {
			t.Fatal(err)
		}
	}
	if e.View(Scope{Project: "independent", Model: model}).State != "circuit_open" {
		t.Fatal("three projects did not open global breaker")
	}
	if !e.Available(Scope{Project: "independent", Model: "gemini-3.1-flash-lite-preview"}) {
		t.Fatal("flash breaker blocked lite")
	}
	clock.Set(clock.Now().Add(61 * time.Second))
	scope := Scope{Project: "probe", Model: model}
	ctx := WithRequest(context.Background())
	ticket, err := e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Reserve(WithRequest(context.Background()), Scope{Project: "other-probe", Model: model}); errorCode(err) != "circuit_open" {
		t.Fatalf("duplicate half open: %v", err)
	}
	if err = ticket.TryStartTransmission(ctx); err != nil {
		t.Fatal(err)
	}
	if err = e.Observe(context.Background(), scope, Signal{Kind: SignalSuccess, AttemptID: "obsolete"}); err != nil {
		t.Fatal(err)
	}
	if e.View(Scope{Project: "other-probe", Model: model}).State != "circuit_open" {
		t.Fatal("obsolete success closed the active probe")
	}
	if err = e.Observe(context.Background(), scope, Signal{Kind: SignalSuccess, AttemptID: ticket.ID()}); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
	if !e.Available(Scope{Project: "other-probe", Model: model}) {
		t.Fatal("completed probe did not reopen routing")
	}
}

func TestHalfOpenCrashReopensWithoutStartupProbe(t *testing.T) {
	clock := newClock()
	path := t.TempDir()
	e := openTest(t, clock, path, false)
	initializeTest(t, e)
	for index := range 3 {
		if err := e.Observe(context.Background(), Scope{Project: fmt.Sprintf("p%d", index), Model: "gemini-3-flash-preview"}, Signal{Kind: Signal503}); err != nil {
			t.Fatal(err)
		}
	}
	clock.Set(clock.Now().Add(61 * time.Second))
	ticket, err := e.Reserve(WithRequest(context.Background()), Scope{Project: "probe", Model: "gemini-3-flash-preview"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openTest(t, clock, path, true)
	if e.View(ticket.Scope()).State != "circuit_open" {
		t.Fatal("lost in-flight probe permitted immediate retry")
	}
	clock.Set(clock.Now().Add(61 * time.Second))
	if !e.Available(ticket.Scope()) {
		t.Fatal("lost probe permanently stuck half-open")
	}
}

func TestCredentialAndModelRestrictionsVisibleWithoutMutations(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview", AuthIndex: "bad-key-index"}
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalModelUnsupported}); err != nil {
		t.Fatal(err)
	}
	before := e.Snapshot().Revision
	if e.View(scope).State != "model_restricted" || e.Available(scope) {
		t.Fatal("model restriction not reflected in view")
	}
	other := scope
	other.Model = "gemini-3.1-flash-lite-preview"
	if !e.Available(other) {
		t.Fatal("one model restriction disabled entire key")
	}
	if e.Snapshot().Revision != before {
		t.Fatal("read-only view wrote the journal")
	}
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalCredentialInvalid}); err != nil {
		t.Fatal(err)
	}
	if e.View(other).State != "quarantined" {
		t.Fatal("credential quarantine not visible")
	}
}

func TestCredentialQuarantineCannotBeBypassedByRegrouping(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview", AuthIndex: "old-index", CredentialID: "same-key-fingerprint"}
	if err := e.Observe(t.Context(), scope, Signal{Kind: SignalCredentialInvalid}); err != nil {
		t.Fatal(err)
	}
	scope.AuthIndex = "new-index"
	if e.View(scope).State != "quarantined" || e.Available(scope) {
		t.Fatal("regrouping revived a quarantined credential")
	}
	other := scope
	other.CredentialID = "another-key-fingerprint"
	if !e.Available(other) {
		t.Fatal("one key quarantine affected its healthy project sibling")
	}
}

func TestAdmissionErrorsNeverExposePrivateRoutingIdentity(t *testing.T) {
	e := openTest(t, newClock(), t.TempDir(), false)
	scope := Scope{Project: "private-project", Model: "gemini-3-flash-preview", AuthIndex: "private-auth"}
	initializeTest(t, e, InitializeOptions{Used: []InitialUsage{{Scope: scope, Used: 20}}})
	_, err := e.Reserve(WithRequest(context.Background()), scope)
	if err == nil {
		t.Fatal("expected quota rejection")
	}
	for _, private := range []string{scope.Project, scope.Model, scope.AuthIndex} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("admission error exposed routing identity")
		}
	}
}

func TestOfficialRPDHintSurvivesMidnightAndOnlyExtends(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	hinted := e.nextReset(clock.Now()).Add(time.Minute)
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalRPD, RetryAfter: hinted.Sub(clock.Now())}); err != nil {
		t.Fatal(err)
	}
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalRPD}); err != nil {
		t.Fatal(err)
	}
	clock.Set(e.nextReset(clock.Now()).Add(time.Second))
	view := e.View(scope)
	if view.State != "cooldown" || !view.RetryAt.Equal(hinted) {
		t.Fatalf("official hint shortened: %+v", view)
	}
	clock.Set(hinted.Add(time.Second))
	if !e.Available(scope) {
		t.Fatal("official hint did not recover")
	}
}

func TestSuccessClearsOnlyShortCooldownWithoutRefund(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview", AuthIndex: "one"}
	sendTest(t, e, scope)
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalUnknown429}); err != nil {
		t.Fatal(err)
	}
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalSuccess}); err != nil {
		t.Fatal(err)
	}
	if !e.Available(scope) || usedFor(e, scope.Project, scope.Model) != 1 {
		t.Fatal("success did not clear only short cooldown")
	}
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalRPD}); err != nil {
		t.Fatal(err)
	}
	if err := e.Observe(context.Background(), scope, Signal{Kind: SignalSuccess}); err != nil {
		t.Fatal(err)
	}
	if e.Available(scope) || e.View(scope).State != "daily_exhausted" {
		t.Fatal("success cleared daily exhaustion")
	}
}
