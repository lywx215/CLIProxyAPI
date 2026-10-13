package auth

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type geminiAdmissionRequestKey struct{}
type geminiAdmissionRequest struct {
	mu                 sync.Mutex
	spent              time.Duration
	limit              time.Duration
	retryAt            time.Time
	lastFailureAttempt int
}

// WithGeminiAdmissionRequest shares acquisition accounting across handler bootstrap retries.
func WithGeminiAdmissionRequest(ctx context.Context) context.Context {
	return withGeminiAdmissionRequest(ctx)
}

func withGeminiAdmissionRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(geminiAdmissionRequestKey{}).(*geminiAdmissionRequest); ok {
		return ctx
	}
	return context.WithValue(ctx, geminiAdmissionRequestKey{}, &geminiAdmissionRequest{})
}

func (r *geminiAdmissionRequest) remaining(limit time.Duration) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.limit == 0 {
		r.limit = limit
	}
	return min(r.limit, limit) - r.spent
}

func (r *geminiAdmissionRequest) consume(elapsed time.Duration) {
	r.mu.Lock()
	r.spent += max(elapsed, 0)
	r.mu.Unlock()
}

func (r *geminiAdmissionRequest) retryDelay(now time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return max(r.retryAt.Sub(now), 0)
}

type geminiAdmissionNode struct {
	key   string
	order uint64
}
type geminiAdmissionCoordinator struct {
	mu            sync.Mutex
	changed       chan struct{}
	queues        map[string][]*geminiAdmissionNode
	pending       int
	turn          bool
	dispatched    uint64
	queued        uint64
	rejected      uint64
	timedOut      uint64
	cancelled     uint64
	waitMillis    uint64
	retryBackoffs uint64
	sequence      uint64
	nowFunc       func() time.Time
}

func (m *Manager) markGeminiAdmissionBackoff(ctx context.Context, previous error) {
	if previous == nil || !m.geminiBudgetEnabled() || !m.GeminiBudgetPolicy().AdmissionSettings().IsEnabled() {
		return
	}
	status := statusCodeFromError(previous)
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return
	}
	attempts := budget.Attempts(ctx)
	if attempts == 0 || attempts >= budget.MaxRequestAttempts {
		return
	}
	state, _ := ctx.Value(geminiAdmissionRequestKey{}).(*geminiAdmissionRequest)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if attempts <= state.lastFailureAttempt {
		return
	}
	// Only acquisition of the next credential waits; no upstream I/O deadline changes.
	delay := 500 * time.Millisecond * time.Duration(1<<(attempts-1))
	delay += time.Duration(rand.IntN(101)) * time.Millisecond
	state.lastFailureAttempt = attempts
	state.retryAt = m.geminiAdmission.now().Add(delay)
	m.geminiAdmission.mu.Lock()
	m.geminiAdmission.retryBackoffs++
	m.geminiAdmission.mu.Unlock()
}

func (q *geminiAdmissionCoordinator) now() time.Time {
	if q.nowFunc != nil {
		return q.nowFunc()
	}
	return time.Now()
}

func (q *geminiAdmissionCoordinator) initLocked() {
	if q.changed == nil {
		q.changed = make(chan struct{})
	}
	if q.queues == nil {
		q.queues = make(map[string][]*geminiAdmissionNode)
	}
}

func (q *geminiAdmissionCoordinator) notifyLocked() {
	q.initLocked()
	close(q.changed)
	q.changed = make(chan struct{})
}

func (q *geminiAdmissionCoordinator) notify() {
	q.mu.Lock()
	q.notifyLocked()
	q.mu.Unlock()
}

func (q *geminiAdmissionCoordinator) removeLocked(node *geminiAdmissionNode) {
	rows := q.queues[node.key]
	for i, value := range rows {
		if value == node {
			rows = append(rows[:i], rows[i+1:]...)
			q.pending--
			if len(rows) == 0 {
				delete(q.queues, node.key)
			} else {
				q.queues[node.key] = rows
			}
			return
		}
	}
}

func admissionModelsOverlap(first, second string) bool {
	for _, model := range strings.Split(first, "\x00") {
		for _, other := range strings.Split(second, "\x00") {
			if model == other {
				return true
			}
		}
	}
	return false
}

func (q *geminiAdmissionCoordinator) modelQueueFullLocked(key string, limit int, exclude *geminiAdmissionNode) bool {
	for _, model := range strings.Split(key, "\x00") {
		pending := 0
		for bucket, rows := range q.queues {
			if admissionModelsOverlap(model, bucket) {
				for _, row := range rows {
					if row != exclude {
						pending++
					}
				}
			}
		}
		if pending >= limit {
			return true
		}
	}
	return false
}

func (q *geminiAdmissionCoordinator) enqueue(key string, settings config.GeminiAdmissionQueueConfig) (*geminiAdmissionNode, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.initLocked()
	if q.pending >= settings.MaxPendingTotal || q.modelQueueFullLocked(key, settings.MaxPendingPerModel, nil) {
		q.rejected++
		return nil, geminiAdmissionError("gemini_queue_full", http.StatusTooManyRequests, time.Second)
	}
	q.sequence++
	node := &geminiAdmissionNode{key: key, order: q.sequence}
	q.queues[key] = append(q.queues[key], node)
	q.pending++
	q.queued++
	q.notifyLocked()
	return node, nil
}

func (q *geminiAdmissionCoordinator) turnFor(node *geminiAdmissionNode, key string, settings config.GeminiAdmissionQueueConfig, ready bool) (<-chan struct{}, func(), error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if key != node.key {
		if q.modelQueueFullLocked(key, settings.MaxPendingPerModel, node) {
			return nil, nil, geminiAdmissionError("gemini_queue_full", 429, time.Second)
		}
		q.removeLocked(node)
		node.key = key
		q.queues[key] = append(q.queues[key], node)
		sort.Slice(q.queues[key], func(i, j int) bool { return q.queues[key][i].order < q.queues[key][j].order })
		q.pending++
		q.notifyLocked()
	}
	rows := q.queues[node.key]
	first := len(rows) > 0 && rows[0] == node
	if first {
		for otherKey, values := range q.queues {
			if len(values) > 0 && values[0].order < node.order && admissionModelsOverlap(node.key, otherKey) {
				first = false
				break
			}
		}
	}
	if ready && first && !q.turn {
		q.turn = true
		q.dispatched++
		q.notifyLocked()
		var once sync.Once
		return q.changed, func() {
			once.Do(func() { q.mu.Lock(); q.removeLocked(node); q.turn = false; q.notifyLocked(); q.mu.Unlock() })
		}, nil
	}
	return q.changed, nil, nil
}

type geminiAdmissionReadiness struct {
	key          string
	managed      bool
	ready        bool
	waitable     bool
	retryAt      time.Time
	dailyRetryAt time.Time
	eventWait    bool
	quota        bool
	terminal     error
}

// Candidate filtering mirrors the local mixed Pick before ledger availability.
// Disabled credentials, route permissions, pins and attempted projects never wait.
func (m *Manager) geminiAdmissionReadiness(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) geminiAdmissionReadiness {
	result := geminiAdmissionReadiness{}
	policy := m.GeminiBudgetPolicy()
	if !policy.Enabled || !policy.AdmissionSettings().IsEnabled() || m.HomeEnabled() {
		return result
	}
	engine := m.GeminiBudgetEngine()
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	providerSet := make(map[string]bool, len(providers))
	for _, provider := range providers {
		providerSet[canonicalSchedulingProvider(provider)] = true
	}
	now := m.geminiAdmission.now()
	actualModels := make(map[string]bool)
	availableModels := make(map[string]bool)
	unmanagedReady := false
	seenScopes := make(map[string]bool)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || (pinned != "" && auth.ID != pinned) || !eligibility.allows(auth) {
			continue
		}
		if _, used := tried[auth.ID]; used {
			continue
		}
		provider := executorKeyFromAuth(auth)
		if !providerSet[provider] {
			continue
		}
		if _, ok := m.executorLocked(provider); !ok {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registry.GetGlobalRegistry(), auth, model) {
			continue
		}
		projected := m.geminiBudgetSelectionAuth(auth, model)
		blocked, _, _ := isAuthBlockedForModel(projected, m.selectionModelForAuth(projected, model), now)
		if blocked {
			continue
		}
		if !IsOfficialGeminiAuth(auth) {
			result.ready = true
			unmanagedReady = true
			continue
		}
		for _, candidate := range m.executionModelCandidates(auth, model) {
			real := CanonicalBudgetModel(candidate)
			if _, managed := policy.ManagedModels[real]; !managed {
				result.ready = true
				unmanagedReady = true
				continue
			}
			if state := existingModelState(projected, canonicalModelKey(real)); state != nil && state.Status == StatusDisabled {
				continue
			}
			scope := GeminiBudgetScope(auth, real)
			if budget.HasAttemptedProject(ctx, scope) {
				continue
			}
			result.managed = true
			actualModels[real] = true
			identity := scope.Origin + "\x00" + scope.Project + "\x00" + real
			if seenScopes[identity] {
				continue
			}
			seenScopes[identity] = true
			if engine == nil {
				result.terminal = geminiAdmissionError("gemini_budget_unavailable", 503, 0)
				continue
			}
			view := engine.View(scope)
			if view.Available {
				result.ready = true
				availableModels[real] = true
				continue
			}
			switch view.State {
			case "busy", "cooldown", "circuit_open":
				result.waitable = true
				if view.State == "busy" || !view.RetryAt.After(now) {
					result.eventWait = true
				}
				if view.State == "cooldown" {
					result.quota = true
				}
				if view.RetryAt.After(now) && (result.retryAt.IsZero() || view.RetryAt.Before(result.retryAt)) {
					result.retryAt = view.RetryAt
				}
			case "daily_exhausted":
				result.quota = true
				if view.RetryAt.After(now) && (result.dailyRetryAt.IsZero() || view.RetryAt.Before(result.dailyRetryAt)) {
					result.dailyRetryAt = view.RetryAt
				}
			case "quarantined", "model_restricted":
			default:
				result.terminal = geminiAdmissionError("gemini_budget_unavailable", 503, 0)
			}
		}
	}
	if unmanagedReady && len(availableModels) == 0 {
		result.managed = false
		return result
	}
	if len(availableModels) > 0 {
		actualModels = availableModels
	}
	models := make([]string, 0, len(actualModels))
	for model := range actualModels {
		models = append(models, model)
	}
	sort.Strings(models)
	result.key = strings.Join(models, "\x00")
	return result
}

func geminiAdmissionError(code string, status int, delay time.Duration) error {
	return wrapRequestStopError(&geminiBudgetAdmissionError{base: &Error{Code: code, Message: "Request capacity is temporarily unavailable.", HTTPStatus: status, Retryable: true}, delay: max(delay, 0)})
}

func (r geminiAdmissionReadiness) rejection(now time.Time) error {
	if r.terminal != nil {
		return r.terminal
	}
	status := http.StatusServiceUnavailable
	if r.quota {
		status = http.StatusTooManyRequests
	}
	retryAt := r.retryAt
	if retryAt.IsZero() && !r.waitable {
		retryAt = r.dailyRetryAt
	}
	return geminiAdmissionError("gemini_budget_unavailable", status, retryAt.Sub(now))
}

// Waiting has its own timer; the execution context never gains a deadline.
func (m *Manager) awaitGeminiAdmission(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (context.Context, error) {
	ctx = withGeminiAdmissionRequest(ctx)
	q := &m.geminiAdmission
	start := q.now()
	settings := m.GeminiBudgetPolicy().AdmissionSettings()
	state := ctx.Value(geminiAdmissionRequestKey{}).(*geminiAdmissionRequest)
	remaining := state.remaining(time.Duration(settings.MaxWaitSeconds) * time.Second)
	initial := m.geminiAdmissionReadiness(ctx, providers, model, opts, tried)
	if !initial.managed {
		return ctx, nil
	}
	if !initial.ready && (!initial.waitable || initial.terminal != nil) {
		return ctx, initial.rejection(start)
	}
	if remaining <= 0 {
		return ctx, geminiAdmissionError("gemini_queue_timeout", 503, time.Second)
	}
	node, err := q.enqueue(initial.key, settings)
	if err != nil {
		return ctx, err
	}
	dispatched := false
	defer func() {
		elapsed := q.now().Sub(start)
		state.consume(elapsed)
		q.mu.Lock()
		q.waitMillis += uint64(max(elapsed.Milliseconds(), 0))
		if !dispatched {
			q.removeLocked(node)
			q.notifyLocked()
		}
		q.mu.Unlock()
	}()
	for {
		if errCtx := ctx.Err(); errCtx != nil {
			q.mu.Lock()
			q.cancelled++
			q.mu.Unlock()
			return ctx, errCtx
		}
		// Subscribe before reading state so a release between read and wait is retained.
		q.mu.Lock()
		changed := q.changed
		q.mu.Unlock()
		var ledgerChanged <-chan struct{}
		if engine := m.GeminiBudgetEngine(); engine != nil {
			ledgerChanged = engine.Changes()
		}
		current := m.geminiAdmissionReadiness(ctx, providers, model, opts, tried)
		if !current.managed {
			return ctx, nil
		}
		if !current.ready && (!current.waitable || current.terminal != nil) {
			return ctx, current.rejection(q.now())
		}
		left := remaining - q.now().Sub(start)
		if left <= 0 {
			q.mu.Lock()
			q.timedOut++
			q.mu.Unlock()
			return ctx, geminiAdmissionError("gemini_queue_timeout", 503, max(current.retryAt.Sub(q.now()), time.Second))
		}
		if !current.ready && !current.eventWait && current.retryAt.After(q.now()) && current.retryAt.Sub(q.now()) > left {
			return ctx, current.rejection(q.now())
		}
		retryDelay := state.retryDelay(q.now())
		_, release, errTurn := q.turnFor(node, current.key, settings, current.ready && retryDelay <= 0)
		if errTurn != nil {
			return ctx, errTurn
		}
		if release != nil {
			// Availability may have changed between the read and acquiring the turn.
			fresh := m.geminiAdmissionReadiness(ctx, providers, model, opts, tried)
			if !fresh.managed {
				release()
				dispatched = true
				return ctx, nil
			}
			if !fresh.ready || fresh.key != current.key {
				q.mu.Lock()
				q.turn = false
				q.notifyLocked()
				q.mu.Unlock()
				continue
			}
			dispatched = true
			return budget.WithAdmissionTurn(ctx, release), nil
		}
		wait := left
		if retryDelay > 0 {
			wait = min(wait, retryDelay)
		}
		if current.retryAt.After(q.now()) {
			wait = min(wait, current.retryAt.Sub(q.now()))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-ledgerChanged:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (m *Manager) pickNextMixedWithAdmission(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (context.Context, *Auth, ProviderExecutor, string, error) {
	for {
		pickCtx, errWait := m.awaitGeminiAdmission(ctx, providers, model, opts, tried)
		if errWait != nil {
			return pickCtx, nil, nil, "", errWait
		}
		auth, executor, provider, errPick := m.pickNextMixed(pickCtx, providers, model, opts, tried)
		if errPick == nil {
			return pickCtx, auth, executor, provider, nil
		}
		budget.ReleaseAdmissionTurn(pickCtx)
		var admissionError *geminiBudgetAdmissionError
		if !errors.As(errPick, &admissionError) {
			return pickCtx, nil, nil, "", errPick
		}
		current := m.geminiAdmissionReadiness(ctx, providers, model, opts, tried)
		if !current.managed || (!current.ready && !current.waitable) || current.terminal != nil {
			return pickCtx, nil, nil, "", errPick
		}
	}
}

type GeminiAdmissionQueueView struct {
	Enabled            bool   `json:"enabled"`
	Pending            int    `json:"pending"`
	Selecting          bool   `json:"selecting"`
	MaxPendingPerModel int    `json:"max_pending_per_model"`
	MaxPendingTotal    int    `json:"max_pending_total"`
	MaxWaitSeconds     int    `json:"max_wait_seconds"`
	Queued             uint64 `json:"queued"`
	Dispatched         uint64 `json:"dispatched"`
	Rejected           uint64 `json:"rejected"`
	TimedOut           uint64 `json:"timed_out"`
	Cancelled          uint64 `json:"cancelled"`
	WaitMillis         uint64 `json:"wait_millis"`
	RetryBackoffs      uint64 `json:"retry_backoffs"`
}

func (m *Manager) GeminiAdmissionQueueStatus() GeminiAdmissionQueueView {
	settings := m.GeminiBudgetPolicy().AdmissionSettings()
	q := &m.geminiAdmission
	q.mu.Lock()
	defer q.mu.Unlock()
	return GeminiAdmissionQueueView{Enabled: m.geminiBudgetEnabled() && settings.IsEnabled(), Pending: q.pending, Selecting: q.turn, MaxPendingPerModel: settings.MaxPendingPerModel, MaxPendingTotal: settings.MaxPendingTotal, MaxWaitSeconds: settings.MaxWaitSeconds, Queued: q.queued, Dispatched: q.dispatched, Rejected: q.rejected, TimedOut: q.timedOut, Cancelled: q.cancelled, WaitMillis: q.waitMillis, RetryBackoffs: q.retryBackoffs}
}
