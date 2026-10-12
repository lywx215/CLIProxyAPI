package budget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpectedInitializedEmptyVolumeFreezesAndCannotReset(t *testing.T) {
	clock := newClock()
	path := t.TempDir()
	e := openTest(t, clock, path, true)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	if e.Snapshot().State != StateDisaster {
		t.Fatal("empty initialized volume did not enter disaster state")
	}
	if errorCode(e.Initialize(context.Background())) != "budget_already_initialized" {
		t.Fatal("disaster state could be reset")
	}
	if _, err := e.Reserve(WithRequest(context.Background()), scope); errorCode(err) != "budget_recovery_frozen" {
		t.Fatalf("disaster admission: %v", err)
	}
	if claim, err := e.ClaimBackup(context.Background(), clock.Now()); err != nil || claim {
		t.Fatalf("disaster backup claim: %v %v", claim, err)
	}
	freeze := e.Snapshot().FreezeUntil
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openTest(t, clock, path, true)
	if !e.Snapshot().FreezeUntil.Equal(freeze) {
		t.Fatal("restart extended recovery freeze")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Set(freeze.Add(time.Minute))
	e = openTest(t, clock, path, true)
	if e.Snapshot().State != StateReady {
		t.Fatal("next day did not recover")
	}
	sendTest(t, e, scope)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openTest(t, clock, path, true)
	if e.Snapshot().State != StateReady || usedFor(e, scope.Project, scope.Model) != 1 {
		t.Fatal("recovery metadata caused repeated daily freezes")
	}
}

func TestLiveDisasterRecoveryBecomesSelectableAtPacificMidnight(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), true)
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	freeze := e.Snapshot().FreezeUntil
	clock.Set(freeze.Add(time.Minute))
	before := e.sequence
	if !e.Available(scope) || e.View(scope).State != "ready" || e.Snapshot().State != StateReady {
		t.Fatal("expired disaster freeze blocked selection before Reserve")
	}
	if e.sequence != before {
		t.Fatal("read-only view wrote recovery state")
	}
	sendTest(t, e, scope)
	if e.state.State != StateReady || usedFor(e, "one", scope.Model) != 1 {
		t.Fatal("Reserve did not durably recover and charge next-day usage")
	}
}

func TestMetadataSignatureAndCorruptWALFailClosed(t *testing.T) {
	for _, damage := range []string{"metadata", "secret", "checksum", "missing-wal"} {
		t.Run(damage, func(t *testing.T) {
			clock := newClock()
			path := t.TempDir()
			e := openTest(t, clock, path, false)
			initializeTest(t, e)
			sendTest(t, e, Scope{Project: "one", Model: "gemini-3-flash-preview"})
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "metadata":
				if err := os.WriteFile(filepath.Join(path, "init.meta"), []byte(`{"version":1,"volume_id":"invented","signature":"00"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "secret":
				if err := os.Remove(filepath.Join(path, "init.secret")); err != nil {
					t.Fatal(err)
				}
			case "checksum":
				data, err := os.ReadFile(filepath.Join(path, "ledger.wal"))
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-1] ^= 1
				if err = os.WriteFile(filepath.Join(path, "ledger.wal"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-wal":
				if err := os.Remove(filepath.Join(path, "ledger.wal")); err != nil {
					t.Fatal(err)
				}
			}
			e = openTest(t, clock, path, false)
			if e.Snapshot().State != StateDisaster {
				t.Fatal("damaged initialization was treated as first installation")
			}
			if e.View(Scope{Project: "one", Model: "gemini-3-flash-preview"}).Available {
				t.Fatal("corrupt storage admitted requests")
			}
		})
	}
}

func TestOldBackupRestoreFreezesCurrentDay(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	snapshot := Snapshot{Version: 1, Initialized: true, Day: "2026-10-01", Revision: 10, State: StateReady, Backup: BackupStatus{Day: "2026-10-01", Attempted: true, Success: true, Revision: 9, At: clock.Now().Add(-11 * 24 * time.Hour)}}
	if err := e.Restore(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().State != StateDisaster || e.View(Scope{Project: "one", Model: "gemini-3-flash-preview"}).State != StateDisaster {
		t.Fatal("old snapshot silently reset current-day counts")
	}
	if allowed, err := e.ClaimBackup(context.Background(), clock.Now()); err != nil || allowed {
		t.Fatal("restored stale backup allowed a current-day write")
	}
}

func TestPacificMidnightAndDST(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	clock.Set(time.Date(2026, 10, 13, 6, 59, 59, 0, time.UTC))
	scope := Scope{Project: "one", Model: "gemini-3-flash-preview"}
	ctx := WithRequest(context.Background())
	ticket, err := e.Reserve(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(time.Date(2026, 10, 13, 7, 0, 0, 0, time.UTC))
	if errorCode(ticket.TryStartTransmission(ctx)) != "budget_reservation_expired" {
		t.Fatal("old-day ticket was sent after midnight")
	}
	if err = ticket.Compensate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ticket.Release()
	sendTest(t, e, scope)
	if usedFor(e, scope.Project, scope.Model) != 1 || e.View(scope).Estimated {
		t.Fatal("new Pacific day did not have fresh exact counters")
	}
	for _, test := range []struct {
		month    time.Month
		day      int
		duration time.Duration
	}{{time.March, 8, 23 * time.Hour}, {time.November, 1, 25 * time.Hour}} {
		start := time.Date(2026, test.month, test.day, 0, 0, 0, 0, e.zone)
		if difference := e.nextReset(start).Sub(start); difference != test.duration {
			t.Fatalf("DST reset=%v want=%v", difference, test.duration)
		}
	}
	clock.Set(time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC))
	if _, err = e.Reserve(WithRequest(context.Background()), scope); errorCode(err) != "budget_clock_unsafe" {
		t.Fatalf("clock rollback: %v", err)
	}
}

func TestBackupOncePerDayIncludingFailureAndUploadRace(t *testing.T) {
	clock := newClock()
	path := t.TempDir()
	e := openTest(t, clock, path, false)
	initializeTest(t, e)
	if !e.Snapshot().Dirty {
		t.Fatal("initialization was not backup-dirty")
	}
	claimAt := clock.Now()
	if ok, err := e.ClaimBackup(context.Background(), claimAt); err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	claimedRevision := e.Snapshot().Backup.Revision
	sendTest(t, e, Scope{Project: "one", Model: "gemini-3-flash-preview"})
	if err := e.SetBackupResult(context.Background(), claimAt, true, "saved"); err != nil {
		t.Fatal(err)
	}
	if !e.Snapshot().Dirty || e.Snapshot().Backup.Revision != claimedRevision {
		t.Fatal("upload race marked newer reservations clean")
	}
	if ok, err := e.ClaimBackup(context.Background(), claimAt); err != nil || ok {
		t.Fatal("same day backup repeated")
	}
	clock.Set(e.nextReset(clock.Now()).Add(time.Minute))
	if ok, err := e.ClaimBackup(context.Background(), clock.Now()); err != nil || !ok {
		t.Fatal("next-day backup did not claim")
	}
	failedAt := clock.Now()
	if err := e.SetBackupResult(context.Background(), failedAt, false, "failed"); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ClaimBackup(context.Background(), failedAt); err != nil || ok {
		t.Fatal("failed backup was retried")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openTest(t, clock, path, true)
	if ok, err := e.ClaimBackup(context.Background(), failedAt); err != nil || ok {
		t.Fatal("restart retried failed backup")
	}
	clock.Set(claimAt)
	if ok, err := e.ClaimBackup(context.Background(), claimAt); err != nil || ok {
		t.Fatal("clock rollback repeated an old day backup")
	}
}

func TestSuccessfulBackupWithoutConcurrentChangesBecomesClean(t *testing.T) {
	clock := newClock()
	e := openTest(t, clock, t.TempDir(), false)
	initializeTest(t, e)
	if ok, err := e.ClaimBackup(context.Background(), clock.Now()); err != nil || !ok {
		t.Fatal(err)
	}
	if err := e.SetBackupResult(context.Background(), clock.Now(), true, ""); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Dirty {
		t.Fatal("backup status dirtied itself")
	}
	clock.Set(e.nextReset(clock.Now()).Add(time.Minute))
	if ok, err := e.ClaimBackup(context.Background(), clock.Now()); err != nil || ok {
		t.Fatal("unchanged ledger generated a COS write")
	}
}

func TestWriterLockCancellationAndRelease(t *testing.T) {
	path := t.TempDir()
	e := openTest(t, newClock(), path, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if second, err := Open(ctx, Options{Path: path, Models: testModels()}); !errors.Is(err, context.Canceled) || second != nil {
		t.Fatal("cancelled lock wait did not terminate")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if second, err := Open(ctx, Options{Path: path, Models: testModels()}); err == nil || second != nil {
		t.Fatal("second writer acquired the lock")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	second := openTest(t, newClock(), path, false)
	// An empty journal left by a prior uninitialized instance is not evidence of
	// a completed initialization: only durable secret/metadata or data is.
	if second.Snapshot().State == StateDisaster {
		t.Fatal("uninitialized reopen was treated as data loss")
	}
}
