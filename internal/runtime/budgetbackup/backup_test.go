package budgetbackup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
)

type testLedger struct {
	mu      sync.Mutex
	dirty   bool
	frozen  bool
	markers map[string]bool
	results []bool
	put     *atomic.Int32
}

func (l *testLedger) ClaimBackup(_ context.Context, now time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.put != nil && l.put.Load() != 0 {
		return false, errors.New("networking started before the durable marker")
	}
	day := now.In(pacific).Format("2006-01-02")
	if l.frozen || !l.dirty || l.markers[day] {
		return false, nil
	}
	if l.markers == nil {
		l.markers = make(map[string]bool)
	}
	l.markers[day] = true
	return true, nil
}

func (l *testLedger) ExportSnapshot(context.Context) (budget.Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return budget.Snapshot{Dirty: l.dirty, Revision: 1}, nil
}

func (l *testLedger) SetBackupResult(_ context.Context, _ time.Time, success bool, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results = append(l.results, success)
	if success {
		l.dirty = false
	}
	return nil
}

func optionsForServer(server *httptest.Server) Options {
	return Options{
		Endpoint: server.URL, Bucket: "test-bucket", AccessKey: "test-access", SecretKey: "test-secret",
		Region: "ap-guangzhou", Prefix: "private", PathStyle: true,
	}
}

func TestCOSFailureUsesOnePhysicalPUTAndConsumesDay(t *testing.T) {
	var puts atomic.Int32
	ledger := &testLedger{dirty: true, put: &puts}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.URL.Path != "/test-bucket/private/quota/gemini-budget/latest.json" {
			t.Errorf("unexpected key: %s", r.URL.Path)
		}
		ledger.mu.Lock()
		marked := len(ledger.markers) == 1
		ledger.mu.Unlock()
		if !marked {
			t.Error("request was transmitted before claiming the backup day")
		}
		puts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<Error><Code>InternalError</Code><Message>secret upstream message</Message></Error>")
	}))
	defer server.Close()
	service, err := New(context.Background(), optionsForServer(server), ledger)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 7, 10, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	claimed, err := service.backupAt(context.Background(), now)
	if !claimed || !errors.Is(err, ErrUploadFailed) || strings.Contains(err.Error(), "secret upstream") {
		t.Fatalf("first failed backup: claimed=%v, err=%v", claimed, err)
	}
	// A new service models a process restart; the marker belongs to the ledger.
	ledger.put = nil
	restarted, err := New(context.Background(), optionsForServer(server), ledger)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	claimed, err = restarted.backupAt(context.Background(), now.Add(time.Hour))
	if claimed || err != nil || puts.Load() != 1 {
		t.Fatalf("same-day backup retried: claimed=%v err=%v PUTs=%d", claimed, err, puts.Load())
	}
	if len(ledger.results) != 1 || ledger.results[0] {
		t.Fatalf("failed result was not recorded: %v", ledger.results)
	}
}

func TestCOSCancellationDoesNotRetry(t *testing.T) {
	var puts atomic.Int32
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		puts.Add(1)
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	ledger := &testLedger{dirty: true}
	service, err := New(context.Background(), optionsForServer(server), ledger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.now = func() time.Time { return time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC) }
	result := make(chan error, 1)
	go func() {
		_, errBackup := service.backupAt(ctx, time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC))
		result <- errBackup
	}()
	<-entered
	cancel()
	if err = <-result; !errors.Is(err, ErrUploadFailed) {
		t.Fatalf("cancelled PUT: %v", err)
	}
	if puts.Load() != 1 || len(ledger.results) != 1 || ledger.results[0] {
		t.Fatalf("cancelled PUT retried or lost result: PUTs=%d results=%v", puts.Load(), ledger.results)
	}
	if claimed, errRetry := service.backupAt(context.Background(), time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)); claimed || errRetry != nil {
		t.Fatalf("cancelled attempt did not consume day: claimed=%v err=%v", claimed, errRetry)
	}
}

func TestCOSRedirectCannotCreateSecondPUT(t *testing.T) {
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		w.Header().Set("Location", "/redirected-object")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	service, err := New(context.Background(), optionsForServer(server), &testLedger{dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Backup(context.Background())
	if !errors.Is(err, ErrUploadFailed) || puts.Load() != 1 {
		t.Fatalf("redirect followed: err=%v PUTs=%d", err, puts.Load())
	}
}

func TestUnchangedFrozenOrAlreadyClaimedNeverPUT(t *testing.T) {
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { puts.Add(1) }))
	defer server.Close()
	now := time.Date(2026, 10, 12, 7, 10, 0, 0, time.UTC)
	for _, ledger := range []*testLedger{
		{},
		{dirty: true, frozen: true},
		{dirty: true, markers: map[string]bool{"2026-10-12": true}},
	} {
		service, err := New(context.Background(), optionsForServer(server), ledger)
		if err != nil {
			t.Fatal(err)
		}
		if claimed, errBackup := service.backupAt(context.Background(), now); claimed || errBackup != nil {
			t.Fatalf("unexpected claim: %v %v", claimed, errBackup)
		}
	}
	if puts.Load() != 0 {
		t.Fatalf("blocked backups caused %d PUTs", puts.Load())
	}
}

func TestNextPacificScheduleHandlesDSTAndDoesNotCatchUp(t *testing.T) {
	for _, tc := range []struct{ now, next string }{
		{"2026-10-12T07:00:00Z", "2026-10-12T07:10:00Z"},
		{"2026-10-12T07:10:00Z", "2026-10-13T07:10:00Z"},
		{"2026-10-12T07:11:00Z", "2026-10-13T07:10:00Z"},
		{"2026-03-08T08:11:00Z", "2026-03-09T07:10:00Z"},
		{"2026-11-01T07:11:00Z", "2026-11-02T08:10:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		expect, _ := time.Parse(time.RFC3339, tc.next)
		if next := nextScheduled(now, 0, 10); !next.Equal(expect) {
			t.Errorf("next after %s = %s; want %s", now, next, expect)
		}
	}
}

func TestSchedulerStartupAndShutdownDoNotUpload(t *testing.T) {
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { puts.Add(1) }))
	defer server.Close()
	service, err := New(context.Background(), optionsForServer(server), &testLedger{dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.Run(ctx)
	if puts.Load() != 0 {
		t.Fatalf("startup/shutdown caused %d PUTs", puts.Load())
	}
}

func TestCOSPUTTransportRejectsSDKOrClientSecondAttempt(t *testing.T) {
	var requests atomic.Int32
	transport := onePUTTransport{inner: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("simulated uncertain result")
	})}
	ctx := context.WithValue(context.Background(), putPermitKey{}, &atomic.Bool{})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "https://example.invalid/backup", nil)
	_, _ = transport.RoundTrip(req)
	if _, err := transport.RoundTrip(req); !errors.Is(err, ErrRepeatedPUT) || requests.Load() != 1 {
		t.Fatalf("second transmission allowed: err=%v requests=%d", err, requests.Load())
	}
}

func TestPhysicalWriteCrossingMidnightCannotUseAlreadyClaimedDay(t *testing.T) {
	oldDay := time.Date(2026, 10, 12, 23, 59, 59, 0, pacific)
	newDay := oldDay.Add(2 * time.Second)
	ledger := &testLedger{dirty: true, markers: map[string]bool{newDay.Format("2006-01-02"): true}}
	current := oldDay
	var puts int
	writer := testWriter(func(ctx context.Context, body []byte) error {
		current = newDay // Models a slow connection crossing midnight before its first byte.
		gate := ctx.Value(backupDayGateKey{}).(func() error)
		if err := gate(); err != nil {
			return err
		}
		puts++
		return nil
	})
	service, err := NewWithWriter(Options{}, ledger, writer)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return current }
	claimed, err := service.Backup(context.Background())
	if !claimed || !errors.Is(err, ErrUploadFailed) || puts != 0 {
		t.Fatalf("PUT crossed into already claimed day: claimed=%v err=%v puts=%d", claimed, err, puts)
	}
}

func TestPhysicalWriteCrossingMidnightConservativelyClaimsBothDays(t *testing.T) {
	oldDay := time.Date(2026, 10, 12, 23, 59, 59, 0, pacific)
	newDay := oldDay.Add(2 * time.Second)
	ledger := &testLedger{dirty: true}
	current := oldDay
	var puts int
	writer := testWriter(func(ctx context.Context, body []byte) error {
		current = newDay
		if err := ctx.Value(backupDayGateKey{}).(func() error)(); err != nil {
			return err
		}
		puts++
		return nil
	})
	service, err := NewWithWriter(Options{}, ledger, writer)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return current }
	if claimed, err := service.Backup(context.Background()); !claimed || err != nil || puts != 1 {
		t.Fatalf("cross-day upload: claimed=%v err=%v puts=%d", claimed, err, puts)
	}
	if !ledger.markers[oldDay.Format("2006-01-02")] || !ledger.markers[newDay.Format("2006-01-02")] {
		t.Fatalf("cross-day markers missing: %v", ledger.markers)
	}
}

func TestCOSFailedAttemptSurvivesRealLedgerRestart(t *testing.T) {
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { puts.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	now := time.Date(2026, 10, 12, 0, 10, 0, 0, pacific)
	options := budget.Options{Path: t.TempDir(), Now: func() time.Time { return now }, Models: map[string]budget.ModelPolicy{"gemini-2.5-flash": {DailyLimit: 20}}}
	engine, err := budget.Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Initialize(context.Background()); err != nil {
		_ = engine.Close()
		t.Fatal(err)
	}
	service, err := New(context.Background(), optionsForServer(server), engine)
	if err != nil {
		_ = engine.Close()
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	if claimed, errBackup := service.Backup(context.Background()); !claimed || !errors.Is(errBackup, ErrUploadFailed) {
		t.Fatalf("first backup: %v %v", claimed, errBackup)
	}
	if err = engine.Close(); err != nil {
		t.Fatal(err)
	}
	options.ExpectedInitialized = true
	restarted, err := budget.Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	service, err = New(context.Background(), optionsForServer(server), restarted)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	if claimed, errBackup := service.Backup(context.Background()); claimed || errBackup != nil || puts.Load() != 1 {
		t.Fatalf("restart retried PUT: claimed=%v err=%v puts=%d", claimed, errBackup, puts.Load())
	}
}

type testWriter func(context.Context, []byte) error

func (f testWriter) Put(ctx context.Context, body []byte) error { return f(ctx, body) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
