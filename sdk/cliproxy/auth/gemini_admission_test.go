package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const admissionFlash = "gemini-2.5-flash"
const admissionLite = "gemini-3.1-flash-lite-preview"

type geminiAdmissionTestClock struct {
	base   time.Time
	offset atomic.Int64
}

func (c *geminiAdmissionTestClock) Now() time.Time {
	return c.base.Add(time.Duration(c.offset.Load()))
}

func (c *geminiAdmissionTestClock) Advance(delay time.Duration) { c.offset.Add(int64(delay)) }

func newGeminiAdmissionTestManager(t *testing.T, count int) (*Manager, *budget.Engine, []*Auth, *geminiAdmissionTestClock) {
	t.Helper()
	clock := &geminiAdmissionTestClock{base: time.Now()}
	m := NewManager(nil, nil, nil)
	m.geminiAdmission.nowFunc = clock.Now
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "gemini"})
	path := t.TempDir()
	cfg := &config.Config{GeminiFreeTierPolicy: config.GeminiFreeTierPolicyConfig{StoragePath: path, ManagedModels: map[string]int{admissionFlash: 20, admissionLite: 500}}}
	var auths []*Auth
	for i := range count {
		key := fmt.Sprintf("synthetic-admission-key-%d", i)
		auths = append(auths, &Auth{ID: fmt.Sprintf("%s-key-%d", t.Name(), i), Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"api_key": key, "config_index": fmt.Sprint(i)}})
		cfg.GeminiKey = append(cfg.GeminiKey, config.GeminiKey{APIKey: key, Models: []config.GeminiModel{{Name: admissionFlash, Alias: "public-flash"}, {Name: admissionLite, Alias: "public-lite"}}})
	}
	m.SetConfig(cfg)
	engine, err := budget.Open(t.Context(), budget.Options{Path: path, Models: map[string]budget.ModelPolicy{admissionFlash: {DailyLimit: 20}, admissionLite: {DailyLimit: 500}}, Now: clock.Now, Jitter: func(time.Duration) time.Duration { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseGeminiBudget() })
	if err = engine.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.geminiBudget = engine
	m.geminiBudgetPath = path
	cfg.GeminiFreeTierPolicy.Enabled = true
	m.SetConfig(cfg)
	for _, auth := range auths {
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(auth.ID, "gemini", []*registry.ModelInfo{{ID: admissionFlash}, {ID: admissionLite}, {ID: "public-flash"}, {ID: "public-lite"}})
		t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
		if _, err = m.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	return m, engine, auths, clock
}

func holdGeminiAdmissionScope(t *testing.T, engine *budget.Engine, auth *Auth, model string) *budget.Ticket {
	t.Helper()
	ctx := budget.WithRequest(t.Context())
	ticket, err := engine.Reserve(ctx, GeminiBudgetScope(auth, model))
	if err != nil {
		t.Fatal(err)
	}
	if err = ticket.TryStartTransmission(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ticket.Release)
	return ticket
}

type geminiAdmissionTestResult struct {
	ctx context.Context
	err error
}

func startGeminiAdmissionWait(m *Manager, ctx context.Context, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) <-chan geminiAdmissionTestResult {
	done := make(chan geminiAdmissionTestResult, 1)
	go func() {
		selected, err := m.awaitGeminiAdmission(ctx, []string{"gemini"}, model, opts, tried)
		done <- geminiAdmissionTestResult{ctx: selected, err: err}
	}()
	return done
}

func assertGeminiAdmissionPending(t *testing.T, m *Manager, pending int) {
	t.Helper()
	if view := m.GeminiAdmissionQueueStatus(); view.Pending != pending {
		t.Fatalf("pending=%d, want %d: %+v", view.Pending, pending, view)
	}
}

func readGeminiAdmissionWAL(t *testing.T, m *Manager) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(m.GeminiBudgetPolicy().StoragePath, "ledger.wal"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertGeminiAdmissionResult(t *testing.T, done <-chan geminiAdmissionTestResult) context.Context {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("admission failed: %v", result.err)
		}
		if _, hasDeadline := result.ctx.Deadline(); hasDeadline {
			t.Fatal("admission added an execution deadline")
		}
		return result.ctx
	default:
		t.Fatal("eligible FIFO head did not dispatch")
		return nil
	}
}

func assertGeminiAdmissionStillWaiting(t *testing.T, done <-chan geminiAdmissionTestResult) {
	t.Helper()
	select {
	case result := <-done:
		budget.ReleaseAdmissionTurn(result.ctx)
		t.Fatalf("request bypassed queued admission: %v", result.err)
	default:
	}
}

func TestGeminiAdmissionFIFOAndAliasesShareQueueWithoutPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, _ := newGeminiAdmissionTestManager(t, 1)
		hold := holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
		before := readGeminiAdmissionWAL(t, m)
		requestCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var rows []<-chan geminiAdmissionTestResult
		for i, model := range []string{"public-flash", NormalizeGeminiBudgetThinkingModel("public-flash:thinking"), NormalizeGeminiBudgetThinkingModel("public-flash:low")} {
			rows = append(rows, startGeminiAdmissionWait(m, budget.WithRequest(requestCtx), model, cliproxyexecutor.Options{}, nil))
			synctest.Wait()
			assertGeminiAdmissionPending(t, m, i+1)
		}
		m.geminiAdmission.mu.Lock()
		queueCount := len(m.geminiAdmission.queues)
		queueSize := len(m.geminiAdmission.queues[admissionFlash])
		m.geminiAdmission.mu.Unlock()
		if queueCount != 1 || queueSize != 3 {
			t.Fatalf("aliases split actual-model queue: queues=%d size=%d", queueCount, queueSize)
		}
		hold.Release()
		for i, row := range rows {
			synctest.Wait()
			ctx := assertGeminiAdmissionResult(t, row)
			for _, later := range rows[i+1:] {
				assertGeminiAdmissionStillWaiting(t, later)
			}
			budget.ReleaseAdmissionTurn(ctx)
		}
		synctest.Wait()
		assertGeminiAdmissionPending(t, m, 0)
		if view := m.GeminiAdmissionQueueStatus(); view.Dispatched != 3 || view.Selecting {
			t.Fatalf("turn ownership leaked: %+v", view)
		}
		if !bytes.Equal(before, readGeminiAdmissionWAL(t, m)) || engine.Snapshot().Backup.Attempted {
			t.Fatal("queue activity persisted state or claimed a COS backup")
		}
	})
}

func TestGeminiAdmissionCancellingHeadAllowsNextWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, _ := newGeminiAdmissionTestManager(t, 1)
		hold := holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first := startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		secondCtx, cancelSecond := context.WithCancel(t.Context())
		defer cancelSecond()
		second := startGeminiAdmissionWait(m, secondCtx, admissionFlash, cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		assertGeminiAdmissionPending(t, m, 2)
		cancel()
		synctest.Wait()
		if result := <-first; !errors.Is(result.err, context.Canceled) {
			t.Fatalf("cancelled head: %v", result.err)
		}
		assertGeminiAdmissionPending(t, m, 1)
		hold.Release()
		synctest.Wait()
		budget.ReleaseAdmissionTurn(assertGeminiAdmissionResult(t, second))
		if view := m.GeminiAdmissionQueueStatus(); view.Cancelled != 1 || view.Pending != 0 {
			t.Fatalf("cancelled queue retained ownership: %+v", view)
		}
	})
}

func TestGeminiAdmissionQueueBoundsAreAtomic(t *testing.T) {
	for _, bound := range []string{"model", "total"} {
		t.Run(bound, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, engine, auths, _ := newGeminiAdmissionTestManager(t, 1)
				holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
				holdGeminiAdmissionScope(t, engine, auths[0], admissionLite)
				cfg := m.runtimeConfigSnapshot().CloneForRuntime()
				cfg.GeminiFreeTierPolicy.AdmissionQueue = &config.GeminiAdmissionQueueConfig{MaxPendingPerModel: 1, MaxPendingTotal: 8}
				if bound == "total" {
					cfg.GeminiFreeTierPolicy.AdmissionQueue = &config.GeminiAdmissionQueueConfig{MaxPendingPerModel: 8, MaxPendingTotal: 1}
				}
				m.SetConfig(cfg)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				first := startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
				synctest.Wait()
				model := admissionFlash
				if bound == "total" {
					model = admissionLite
				}
				_, err := m.awaitGeminiAdmission(t.Context(), []string{"gemini"}, model, cliproxyexecutor.Options{}, nil)
				var coded *Error
				if !errors.As(err, &coded) || coded.Code != "gemini_queue_full" || statusCodeFromError(err) != http.StatusTooManyRequests {
					t.Fatalf("overflow did not reject atomically: %v", err)
				}
				assertGeminiAdmissionPending(t, m, 1)
				if m.GeminiAdmissionQueueStatus().Rejected != 1 {
					t.Fatal("overflow did not increment rejection count")
				}
				cancel()
				synctest.Wait()
				<-first
				assertGeminiAdmissionPending(t, m, 0)
			})
		})
	}
}

func TestGeminiAdmissionFiltersDoNotBorrowOtherCredentials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, _ := newGeminiAdmissionTestManager(t, 2)
		if err := engine.Observe(t.Context(), GeminiBudgetScope(auths[0], admissionFlash), budget.Signal{Kind: budget.SignalRPD}); err != nil {
			t.Fatal(err)
		}
		pin := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auths[0].ID}}
		_, err := m.awaitGeminiAdmission(t.Context(), []string{"gemini"}, admissionFlash, pin, nil)
		if statusCodeFromError(err) != http.StatusTooManyRequests {
			t.Fatalf("pin borrowed ready credential: %v", err)
		}
		cases := []struct {
			name      string
			ctx       context.Context
			opts      cliproxyexecutor.Options
			tried     map[string]struct{}
			providers []string
		}{
			{"eligibility", withRequiredAuthKind(t.Context(), AuthKindOAuth), cliproxyexecutor.Options{}, nil, []string{"gemini"}},
			{"tried", t.Context(), cliproxyexecutor.Options{}, map[string]struct{}{auths[0].ID: {}, auths[1].ID: {}}, []string{"gemini"}},
			{"provider", t.Context(), cliproxyexecutor.Options{}, nil, []string{"claude"}},
		}
		for _, test := range cases {
			before := m.GeminiAdmissionQueueStatus().Queued
			ready := m.geminiAdmissionReadiness(test.ctx, test.providers, admissionFlash, test.opts, test.tried)
			if ready.managed || ready.ready || ready.waitable {
				t.Fatalf("%s borrowed forbidden credentials: %+v", test.name, ready)
			}
			_, _, _, errPick := m.pickNextMixed(test.ctx, test.providers, admissionFlash, test.opts, test.tried)
			if errPick == nil {
				t.Fatalf("%s bypassed selection restrictions", test.name)
			}
			if m.GeminiAdmissionQueueStatus().Queued != before {
				t.Fatalf("%s entered a forbidden queue", test.name)
			}
		}
		attemptCtx := budget.WithRequest(t.Context())
		if err = budget.StartUnmeteredTransmission(attemptCtx, GeminiBudgetScope(auths[1], admissionFlash)); err != nil {
			t.Fatal(err)
		}
		attemptPin := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auths[1].ID}}
		if ready := m.geminiAdmissionReadiness(attemptCtx, []string{"gemini"}, admissionFlash, attemptPin, nil); ready.managed || ready.ready || ready.waitable {
			t.Fatal("already attempted project re-entered admission")
		}
		assertGeminiAdmissionPending(t, m, 0)
	})
}

func TestGeminiAdmissionBusyPoolWaitsDespiteUnusableSiblings(t *testing.T) {
	for _, sibling := range []string{"daily", "long-cooldown"} {
		t.Run(sibling, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, engine, auths, _ := newGeminiAdmissionTestManager(t, 2)
				hold := holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
				signal := budget.Signal{Kind: budget.SignalRPD}
				if sibling == "long-cooldown" {
					signal = budget.Signal{Kind: budget.SignalRPM, RetryAfter: 20 * time.Minute}
				}
				if err := engine.Observe(t.Context(), GeminiBudgetScope(auths[1], admissionFlash), signal); err != nil {
					t.Fatal(err)
				}
				before := readGeminiAdmissionWAL(t, m)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
				synctest.Wait()
				assertGeminiAdmissionStillWaiting(t, done)
				assertGeminiAdmissionPending(t, m, 1)
				hold.Release()
				synctest.Wait()
				budget.ReleaseAdmissionTurn(assertGeminiAdmissionResult(t, done))
				if !bytes.Equal(before, readGeminiAdmissionWAL(t, m)) {
					t.Fatal("busy waiting modified durable quota")
				}
			})
		})
	}
}

func TestGeminiAdmissionPolicyDisableWakesWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, _ := newGeminiAdmissionTestManager(t, 1)
		holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		assertGeminiAdmissionPending(t, m, 1)
		cfg := m.runtimeConfigSnapshot().CloneForRuntime()
		cfg.GeminiFreeTierPolicy.Enabled = false
		m.SetConfig(cfg)
		synctest.Wait()
		budget.ReleaseAdmissionTurn(assertGeminiAdmissionResult(t, done))
		assertGeminiAdmissionPending(t, m, 0)
		if m.GeminiAdmissionQueueStatus().Enabled {
			t.Fatal("disabled queue remained enabled")
		}
	})
}

func TestGeminiAdmissionPathFailureWakesAndFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, _ := newGeminiAdmissionTestManager(t, 1)
		holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		cfg := m.runtimeConfigSnapshot().CloneForRuntime()
		cfg.GeminiFreeTierPolicy.StoragePath = t.TempDir()
		m.SetConfig(cfg)
		// Let the config notification be consumed before the ledger becomes unavailable.
		synctest.Wait()
		if _, err := m.EnsureGeminiBudget(t.Context(), false); err == nil {
			t.Fatal("unsafe storage path was accepted")
		}
		synctest.Wait()
		select {
		case result := <-done:
			if statusCodeFromError(result.err) != http.StatusServiceUnavailable {
				t.Fatalf("path failure did not fail closed: %v", result.err)
			}
		default:
			t.Fatal("path failure did not notify waiting requests")
		}
		assertGeminiAdmissionPending(t, m, 0)
	})
}

func TestGeminiAdmissionWaitBudgetCountsOnlyCredentialAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, clock := newGeminiAdmissionTestManager(t, 2)
		hold := holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
		other := holdGeminiAdmissionScope(t, engine, auths[1], admissionFlash)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		original := budget.WithRequest(withGeminiAdmissionRequest(ctx))
		done := startGeminiAdmissionWait(m, original, admissionFlash, cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		clock.Advance(10 * time.Second)
		hold.Release()
		synctest.Wait()
		selected := assertGeminiAdmissionResult(t, done)
		if err := budget.StartUnmeteredTransmission(selected, GeminiBudgetScope(auths[0], admissionFlash)); err != nil {
			t.Fatal(err)
		}
		clock.Advance(10 * time.Minute)
		budget.ReleaseAdmissionTurn(selected)
		other.Release()
		selected, err := m.awaitGeminiAdmission(selected, []string{"gemini"}, admissionFlash, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("upstream execution time consumed acquisition wait budget: %v", err)
		}
		budget.ReleaseAdmissionTurn(selected)
		state := selected.Value(geminiAdmissionRequestKey{}).(*geminiAdmissionRequest)
		state.mu.Lock()
		spent := state.spent
		state.mu.Unlock()
		if spent != 10*time.Second || budget.Attempts(selected) != 1 || !budget.HasAttemptedProject(selected, GeminiBudgetScope(auths[0], admissionFlash)) {
			t.Fatalf("acquisition budget or send counter changed: spent=%s attempts=%d", spent, budget.Attempts(selected))
		}
	})
}

func TestGeminiAdmissionFlashBlockDoesNotHoldLiteQueue(t *testing.T) {
	for _, state := range []string{"busy", "daily-exhausted"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, engine, auths, _ := newGeminiAdmissionTestManager(t, 1)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var waiting <-chan geminiAdmissionTestResult
				if state == "busy" {
					holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
					waiting = startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
					synctest.Wait()
					assertGeminiAdmissionPending(t, m, 1)
				} else {
					if err := engine.Observe(t.Context(), GeminiBudgetScope(auths[0], admissionFlash), budget.Signal{Kind: budget.SignalRPD}); err != nil {
						t.Fatal(err)
					}
					if _, err := m.awaitGeminiAdmission(ctx, []string{"gemini"}, admissionFlash, cliproxyexecutor.Options{}, nil); statusCodeFromError(err) != http.StatusTooManyRequests {
						t.Fatalf("exhausted Flash did not reject immediately: %v", err)
					}
					assertGeminiAdmissionPending(t, m, 0)
				}
				lite := startGeminiAdmissionWait(m, ctx, "public-lite", cliproxyexecutor.Options{}, nil)
				synctest.Wait()
				budget.ReleaseAdmissionTurn(assertGeminiAdmissionResult(t, lite))
				if waiting != nil {
					assertGeminiAdmissionStillWaiting(t, waiting)
					cancel()
					synctest.Wait()
					if result := <-waiting; !errors.Is(result.err, context.Canceled) {
						t.Fatalf("blocked Flash retained ownership: %v", result.err)
					}
				}
			})
		})
	}
}

func TestGeminiAdmissionTimeoutDoesNotSpendQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, clock := newGeminiAdmissionTestManager(t, 1)
		holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
		before := readGeminiAdmissionWAL(t, m)
		requestCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx := budget.WithRequest(requestCtx)
		done := startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		clock.Advance(76 * time.Second)
		m.geminiAdmission.notify()
		synctest.Wait()
		result := <-done
		var coded *Error
		if !errors.As(result.err, &coded) || coded.Code != "gemini_queue_timeout" {
			t.Fatalf("wait budget was not enforced: %v", result.err)
		}
		if budget.Attempts(ctx) != 0 || !bytes.Equal(before, readGeminiAdmissionWAL(t, m)) || engine.Snapshot().Backup.Attempted {
			t.Fatal("timed out queue spent quota or claimed backup")
		}
		if view := m.GeminiAdmissionQueueStatus(); view.Pending != 0 || view.TimedOut != 1 {
			t.Fatalf("timeout leaked queue state: %+v", view)
		}
	})
}

func configureGeminiAdmissionMixedRoute(t *testing.T, m *Manager, auths []*Auth) {
	t.Helper()
	cfg := m.runtimeConfigSnapshot().CloneForRuntime()
	for i, model := range []string{admissionFlash, admissionLite} {
		cfg.GeminiKey[i].Models = append(cfg.GeminiKey[i].Models, config.GeminiModel{Name: model, Alias: "public-mixed"})
		registry.GetGlobalRegistry().RegisterClient(auths[i].ID, "gemini", []*registry.ModelInfo{{ID: admissionFlash}, {ID: admissionLite}, {ID: "public-flash"}, {ID: "public-lite"}, {ID: "public-mixed"}})
	}
	m.SetConfig(cfg)
}

func TestGeminiAdmissionMixedRouteUsesReadyLiteWithoutFlashHeadBlocking(t *testing.T) {
	for _, state := range []string{"busy", "daily-exhausted"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, engine, auths, _ := newGeminiAdmissionTestManager(t, 2)
				configureGeminiAdmissionMixedRoute(t, m, auths)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var blocked <-chan geminiAdmissionTestResult
				if state == "busy" {
					holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
					pin := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auths[0].ID}}
					blocked = startGeminiAdmissionWait(m, ctx, "public-flash", pin, nil)
					synctest.Wait()
					assertGeminiAdmissionPending(t, m, 1)
				} else if err := engine.Observe(t.Context(), GeminiBudgetScope(auths[0], admissionFlash), budget.Signal{Kind: budget.SignalRPD}); err != nil {
					t.Fatal(err)
				}
				ready := m.geminiAdmissionReadiness(ctx, []string{"gemini"}, "public-mixed", cliproxyexecutor.Options{}, nil)
				if !ready.ready || ready.key != admissionLite {
					t.Fatalf("mixed alias retained an unavailable Flash queue key: %+v", ready)
				}
				mixed := startGeminiAdmissionWait(m, ctx, "public-mixed", cliproxyexecutor.Options{}, nil)
				synctest.Wait()
				budget.ReleaseAdmissionTurn(assertGeminiAdmissionResult(t, mixed))
				if blocked != nil {
					assertGeminiAdmissionStillWaiting(t, blocked)
					cancel()
					synctest.Wait()
					<-blocked
				}
			})
		})
	}
}

func TestGeminiAdmissionAdministratorModelDisableCannotSupplyReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, _ := newGeminiAdmissionTestManager(t, 2)
		configureGeminiAdmissionMixedRoute(t, m, auths)
		m.mu.Lock()
		m.auths[auths[0].ID].ModelStates = map[string]*ModelState{admissionFlash: {Status: StatusDisabled}}
		m.mu.Unlock()
		hold := holdGeminiAdmissionScope(t, engine, auths[1], admissionLite)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ready := m.geminiAdmissionReadiness(ctx, []string{"gemini"}, "public-mixed", cliproxyexecutor.Options{}, nil)
		if ready.ready || !ready.waitable || ready.key != admissionLite {
			t.Fatalf("disabled actual model supplied readiness through alias: %+v", ready)
		}
		done := startGeminiAdmissionWait(m, ctx, "public-mixed", cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		assertGeminiAdmissionStillWaiting(t, done)
		hold.Release()
		synctest.Wait()
		selectedCtx := assertGeminiAdmissionResult(t, done)
		selected, _, _, err := m.pickNextMixed(selectedCtx, []string{"gemini"}, "public-mixed", cliproxyexecutor.Options{}, nil)
		budget.ReleaseAdmissionTurn(selectedCtx)
		if err != nil || selected == nil || selected.ID != auths[1].ID {
			t.Fatalf("alias bypassed administrator disable: auth=%v err=%v", selected, err)
		}
	})
}

func TestGeminiAdmissionSelectionTurnRetainsFIFOHeadUntilHandoff(t *testing.T) {
	var coordinator geminiAdmissionCoordinator
	settings := config.GeminiFreeTierPolicyConfig{}.AdmissionSettings()
	head, err := coordinator.enqueue(admissionFlash, settings)
	if err != nil {
		t.Fatal(err)
	}
	_, staleTurn, err := coordinator.turnFor(head, admissionFlash, settings, true)
	if err != nil || staleTurn == nil {
		t.Fatalf("head did not acquire selection turn: %v", err)
	}
	follower, err := coordinator.enqueue(admissionFlash, settings)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	retained := coordinator.pending == 2 && coordinator.queues[admissionFlash][0] == head
	coordinator.mu.Unlock()
	if !retained {
		t.Fatal("selection removed FIFO head before reservation handoff")
	}
	if _, release, errTurn := coordinator.turnFor(follower, admissionFlash, settings, true); errTurn != nil || release != nil {
		t.Fatalf("follower bypassed active selection turn: %v", errTurn)
	}
	// A stale availability read yields only the turn; it retains the old head.
	coordinator.mu.Lock()
	coordinator.turn = false
	coordinator.notifyLocked()
	coordinator.mu.Unlock()
	if _, release, errTurn := coordinator.turnFor(follower, admissionFlash, settings, true); errTurn != nil || release != nil {
		t.Fatalf("follower bypassed head during stale-read recovery: %v", errTurn)
	}
	_, releaseHead, err := coordinator.turnFor(head, admissionFlash, settings, true)
	if err != nil || releaseHead == nil {
		t.Fatalf("retained head could not reacquire turn: %v", err)
	}
	releaseHead()
	_, releaseFollower, err := coordinator.turnFor(follower, admissionFlash, settings, true)
	if err != nil || releaseFollower == nil {
		t.Fatalf("handoff did not unblock follower: %v", err)
	}
	releaseFollower()
	releaseFollower()
	if coordinator.pending != 0 || coordinator.turn {
		t.Fatal("selection handoff leaked queue ownership")
	}
}

func TestGeminiAdmissionCustomFallbackBypassesBlockedOfficialModelQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, _ := newGeminiAdmissionTestManager(t, 2)
		m.mu.Lock()
		m.auths[auths[1].ID].Attributes["base_url"] = "https://custom.example.invalid"
		m.mu.Unlock()
		holdGeminiAdmissionScope(t, engine, auths[0], admissionFlash)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pin := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: auths[0].ID}}
		blocked := startGeminiAdmissionWait(m, ctx, "public-flash", pin, nil)
		synctest.Wait()
		assertGeminiAdmissionPending(t, m, 1)
		before := readGeminiAdmissionWAL(t, m)
		fallback := startGeminiAdmissionWait(m, ctx, "public-flash", cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		selectedCtx := assertGeminiAdmissionResult(t, fallback)
		selected, _, _, err := m.pickNextMixed(selectedCtx, []string{"gemini"}, "public-flash", cliproxyexecutor.Options{}, nil)
		budget.ReleaseAdmissionTurn(selectedCtx)
		if err != nil || selected == nil || selected.ID != auths[1].ID {
			t.Fatalf("ready custom upstream was not preserved: auth=%v err=%v", selected, err)
		}
		assertGeminiAdmissionStillWaiting(t, blocked)
		if !bytes.Equal(before, readGeminiAdmissionWAL(t, m)) {
			t.Fatal("custom fallback changed official quota")
		}
		cancel()
		synctest.Wait()
		<-blocked
	})
}

func TestGeminiAdmissionBackoffRequiresPhysicalSendAndDeduplicatesAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, auths, clock := newGeminiAdmissionTestManager(t, 3)
		ctx := budget.WithRequest(withGeminiAdmissionRequest(t.Context()))
		state := ctx.Value(geminiAdmissionRequestKey{}).(*geminiAdmissionRequest)
		failure := &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "synthetic failure"}
		m.markGeminiAdmissionBackoff(ctx, failure)
		if !state.retryAt.IsZero() || state.lastFailureAttempt != 0 || m.GeminiAdmissionQueueStatus().RetryBackoffs != 0 {
			t.Fatal("local unsent failure triggered upstream backoff")
		}
		if err := budget.StartUnmeteredTransmission(ctx, GeminiBudgetScope(auths[0], admissionFlash)); err != nil {
			t.Fatal(err)
		}
		m.markGeminiAdmissionBackoff(ctx, failure)
		first := state.retryAt
		if delay := first.Sub(clock.Now()); delay < 500*time.Millisecond || delay > 600*time.Millisecond {
			t.Fatalf("first backoff=%s, want 500-600ms", delay)
		}
		clock.Advance(time.Minute)
		m.markGeminiAdmissionBackoff(ctx, &Error{HTTPStatus: http.StatusTooManyRequests})
		if !state.retryAt.Equal(first) || state.lastFailureAttempt != 1 || m.GeminiAdmissionQueueStatus().RetryBackoffs != 1 {
			t.Fatal("re-observing one physical attempt extended or duplicated backoff")
		}
		if err := budget.StartUnmeteredTransmission(ctx, GeminiBudgetScope(auths[1], admissionFlash)); err != nil {
			t.Fatal(err)
		}
		m.markGeminiAdmissionBackoff(ctx, failure)
		if delay := state.retryAt.Sub(clock.Now()); delay < time.Second || delay > 1100*time.Millisecond {
			t.Fatalf("second backoff=%s, want 1-1.1s", delay)
		}
		if state.lastFailureAttempt != 2 || m.GeminiAdmissionQueueStatus().RetryBackoffs != 2 {
			t.Fatal("second physical failure was not counted once")
		}
		second := state.retryAt
		if err := budget.StartUnmeteredTransmission(ctx, GeminiBudgetScope(auths[2], admissionFlash)); err != nil {
			t.Fatal(err)
		}
		m.markGeminiAdmissionBackoff(ctx, failure)
		if !state.retryAt.Equal(second) || state.lastFailureAttempt != 2 || m.GeminiAdmissionQueueStatus().RetryBackoffs != 2 {
			t.Fatal("attempt limit triggered a fourth-send backoff")
		}
	})
}

func TestGeminiAdmissionBackoffIgnoresOtherStatusesAndDisabledPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, auths, _ := newGeminiAdmissionTestManager(t, 1)
		ctx := budget.WithRequest(withGeminiAdmissionRequest(t.Context()))
		if err := budget.StartUnmeteredTransmission(ctx, GeminiBudgetScope(auths[0], admissionFlash)); err != nil {
			t.Fatal(err)
		}
		state := ctx.Value(geminiAdmissionRequestKey{}).(*geminiAdmissionRequest)
		for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway} {
			m.markGeminiAdmissionBackoff(ctx, &Error{HTTPStatus: status})
			if !state.retryAt.IsZero() || state.lastFailureAttempt != 0 {
				t.Fatalf("status %d scheduled retry backoff", status)
			}
		}
		cfg := m.runtimeConfigSnapshot().CloneForRuntime()
		cfg.GeminiFreeTierPolicy.Enabled = false
		m.SetConfig(cfg)
		m.markGeminiAdmissionBackoff(ctx, &Error{HTTPStatus: http.StatusTooManyRequests})
		if !state.retryAt.IsZero() {
			t.Fatal("disabled policy scheduled retry backoff")
		}
		cfg.GeminiFreeTierPolicy.Enabled = true
		disabled := false
		cfg.GeminiFreeTierPolicy.AdmissionQueue = &config.GeminiAdmissionQueueConfig{Enabled: &disabled}
		m.SetConfig(cfg)
		m.markGeminiAdmissionBackoff(ctx, &Error{HTTPStatus: http.StatusServiceUnavailable})
		if !state.retryAt.IsZero() || m.GeminiAdmissionQueueStatus().RetryBackoffs != 0 {
			t.Fatal("disabled admission queue scheduled retry backoff")
		}
	})
}

func TestGeminiAdmissionBackoffWaitPreservesAttemptsAndDoesNotPersist(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, engine, auths, clock := newGeminiAdmissionTestManager(t, 2)
		requestCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx := budget.WithRequest(withGeminiAdmissionRequest(requestCtx))
		if err := budget.StartUnmeteredTransmission(ctx, GeminiBudgetScope(auths[0], admissionFlash)); err != nil {
			t.Fatal(err)
		}
		before := readGeminiAdmissionWAL(t, m)
		m.markGeminiAdmissionBackoff(ctx, &Error{HTTPStatus: http.StatusTooManyRequests})
		done := startGeminiAdmissionWait(m, ctx, admissionFlash, cliproxyexecutor.Options{}, nil)
		synctest.Wait()
		assertGeminiAdmissionStillWaiting(t, done)
		assertGeminiAdmissionPending(t, m, 1)
		clock.Advance(400 * time.Millisecond)
		m.geminiAdmission.notify()
		synctest.Wait()
		assertGeminiAdmissionStillWaiting(t, done)
		clock.Advance(250 * time.Millisecond)
		m.geminiAdmission.notify()
		synctest.Wait()
		selected := assertGeminiAdmissionResult(t, done)
		if budget.Attempts(selected) != 1 || !budget.HasAttemptedProject(selected, GeminiBudgetScope(auths[0], admissionFlash)) {
			t.Fatal("waiting reset physical send history")
		}
		budget.ReleaseAdmissionTurn(selected)
		state := selected.Value(geminiAdmissionRequestKey{}).(*geminiAdmissionRequest)
		state.mu.Lock()
		spent := state.spent
		state.mu.Unlock()
		if spent != 650*time.Millisecond {
			t.Fatalf("retry delay escaped total acquisition budget: %s", spent)
		}
		if !bytes.Equal(before, readGeminiAdmissionWAL(t, m)) || engine.Snapshot().Backup.Attempted {
			t.Fatal("retry waiting spent quota or claimed backup")
		}
	})
}
