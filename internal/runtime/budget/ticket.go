package budget

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type requestContextKey struct{}
type engineContextKey struct{}
type engineBinding struct{ engine *Engine }
type attemptCounter struct {
	mu       sync.Mutex
	count    int
	projects map[string]bool
}

// WithRequest is idempotent. All retries and derived contexts for an inbound
// request must retain this same counter rather than create one per attempt.
func WithRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(requestContextKey{}).(*attemptCounter); ok {
		return ctx
	}
	return context.WithValue(ctx, requestContextKey{}, &attemptCounter{projects: map[string]bool{}})
}
func Attempts(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	counter, _ := ctx.Value(requestContextKey{}).(*attemptCounter)
	if counter == nil {
		return 0
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	return counter.count
}
func WithEngine(ctx context.Context, engine *Engine) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, engineContextKey{}, engineBinding{engine: engine})
}
func EngineFromContext(ctx context.Context) *Engine {
	if ctx == nil {
		return nil
	}
	binding, _ := ctx.Value(engineContextKey{}).(engineBinding)
	return binding.engine
}

// HasEngineBinding distinguishes an explicit unavailable ledger from an absent binding.
func HasEngineBinding(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(engineContextKey{}).(engineBinding)
	return ok
}

type Ticket struct {
	mu          sync.Mutex
	engine      *Engine
	id          string
	scope       Scope
	day         string
	managed     bool
	started     atomic.Bool
	compensated bool
	released    bool
	halfOpen    bool
}

func (t *Ticket) ID() string {
	if t == nil {
		return ""
	}
	return t.id
}
func (t *Ticket) Started() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started.Load()
}

func (t *Ticket) Scope() Scope {
	if t == nil {
		return Scope{}
	}
	return t.scope
}

func HasAttemptedProject(ctx context.Context, scope Scope) bool {
	if ctx == nil {
		return false
	}
	counter, _ := ctx.Value(requestContextKey{}).(*attemptCounter)
	if counter == nil {
		return false
	}
	scope = normalizedScope(scope)
	counter.mu.Lock()
	defer counter.mu.Unlock()
	return counter.projects[scope.Origin+"|"+scope.Project]
}

// StartUnmeteredTransmission enforces the channel-wide physical send limit
// for models without a configured daily budget. It creates no ledger event.
func StartUnmeteredTransmission(ctx context.Context, scope Scope) error {
	if ctx == nil {
		return admissionError("budget_request_context_required", 503, 0)
	}
	counter, _ := ctx.Value(requestContextKey{}).(*attemptCounter)
	if counter == nil {
		return admissionError("budget_request_context_required", 503, 0)
	}
	scope = normalizedScope(scope)
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if counter.count >= MaxRequestAttempts {
		return admissionError("request_attempt_limit", 503, 0)
	}
	project := scope.Origin + "|" + scope.Project
	if scope.Project != "" && counter.projects[project] {
		return admissionError("budget_project_already_attempted", 503, 0)
	}
	counter.count++
	if scope.Project != "" {
		counter.projects[project] = true
	}
	return nil
}

func (e *Engine) Reserve(ctx context.Context, scope Scope) (*Ticket, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.closed || e.unhealthy {
		return nil, admissionError("budget_storage_unavailable", 503, 0)
	}
	if Attempts(ctx) >= MaxRequestAttempts {
		return nil, admissionError("request_attempt_limit", 503, 0)
	}
	scope = normalizedScope(scope)
	if scope.Project != "" && HasAttemptedProject(ctx, scope) {
		return nil, admissionError("budget_project_already_attempted", 503, 0)
	}
	now := e.now()
	day := e.day(now)
	policy, managed := e.models[scope.Model]
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	ticket := &Ticket{engine: e, id: id, scope: scope, day: day, managed: managed}
	if !managed {
		return ticket, nil
	}
	if scope.Project == "" {
		return nil, admissionError("budget_project_unknown", 503, 0)
	}
	if err = e.ensureReadyLocked(ctx, now); err != nil {
		return nil, err
	}
	if e.state.CredentialBlocks[credentialKey(scope)] || e.state.CredentialBlocks[credentialKey(scope)+"\x00"+scope.Model] {
		return nil, admissionError("budget_credential_unavailable", 503, 0)
	}
	if e.inFlightLocked(scope) >= e.maxInFlight {
		return nil, admissionError("budget_scope_busy", 503, time.Second)
	}
	entry := e.state.Entries[entryKey(scope, day)]
	limit := policy.DailyLimit
	if entry.ObservedLimit > 0 && entry.ObservedLimit < limit {
		limit = entry.ObservedLimit
	}
	if entry.DailyExhausted || entry.Used >= limit {
		return nil, admissionError("daily_budget_exhausted", http.StatusTooManyRequests, e.nextReset(now).Sub(now))
	}
	if cooldown := e.cooldownUntilLocked(scope); now.Before(cooldown) {
		return nil, admissionError("quota_cooldown", 429, cooldown.Sub(now))
	}
	circuit := e.state.Circuits[circuitKey(scope)]
	if now.Before(circuit.OpenUntil) || circuit.HalfOpen {
		delay := circuit.OpenUntil.Sub(now)
		if delay <= 0 {
			delay = time.Second
		}
		return nil, admissionError("circuit_open", 503, delay)
	}
	if err = e.appendLocked(ctx, "reserve", reservation{ID: id, Scope: scope, Day: day, Limit: limit}, true); err != nil {
		return nil, err
	}
	if !circuit.OpenUntil.IsZero() {
		circuit.Origin, circuit.Model, circuit.HalfOpen, circuit.ProbeID = scope.Origin, scope.Model, true, id
		if err = e.appendLocked(ctx, "circuit", circuit, true); err != nil {
			// Never send if the half-open claim could not be persisted. The unused
			// reservation remains conservatively charged if compensation fails.
			_ = e.appendLocked(context.Background(), "compensate", compensation{TargetAttemptID: id}, true)
			return nil, err
		}
		ticket.halfOpen = true
	}
	e.inflight[entryKey(scope, day)]++
	return ticket, nil
}

func (e *Engine) cooldownUntilLocked(scope Scope) time.Time {
	var deadline time.Time
	for _, entry := range e.state.Entries {
		if entry.Origin == scope.Origin && entry.Project == scope.Project && entry.Model == scope.Model && entry.CooldownUntil.After(deadline) {
			deadline = entry.CooldownUntil
		}
	}
	return deadline
}

// TryStartTransmission is the final atomic boundary immediately before the
// underlying transport is called. Once started, compensation is forbidden,
// including on timeout/cancellation: the upstream may already have counted it.
func (t *Ticket) TryStartTransmission(ctx context.Context) error {
	if t == nil {
		return errors.New("budget ticket is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started.Load() || t.compensated || t.released {
		return admissionError("budget_ticket_already_used", 409, 0)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	counter, _ := ctx.Value(requestContextKey{}).(*attemptCounter)
	if counter == nil {
		return admissionError("budget_request_context_required", 503, 0)
	}
	e := t.engine
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.unhealthy {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	if t.managed {
		now := e.now()
		if err := e.ensureReadyLocked(ctx, now); err != nil {
			return err
		}
		if e.day(now) != t.day {
			return admissionError("budget_reservation_expired", 503, 0)
		}
		if e.state.CredentialBlocks[credentialKey(t.scope)] || e.state.CredentialBlocks[credentialKey(t.scope)+"\x00"+t.scope.Model] {
			return admissionError("budget_credential_unavailable", 503, 0)
		}
		entry := e.state.Entries[entryKey(t.scope, t.day)]
		policy, exists := e.models[t.scope.Model]
		if !exists {
			return admissionError("budget_policy_changed", 503, 0)
		}
		limit := policy.DailyLimit
		if entry.ObservedLimit > 0 && entry.ObservedLimit < limit {
			limit = entry.ObservedLimit
		}
		if entry.Used > limit {
			return admissionError("daily_budget_exhausted", 429, e.nextReset(now).Sub(now))
		}
		if entry.DailyExhausted {
			return admissionError("daily_budget_exhausted", 429, e.nextReset(now).Sub(now))
		}
		if deadline := e.cooldownUntilLocked(t.scope); now.Before(deadline) {
			return admissionError("quota_cooldown", 429, deadline.Sub(now))
		}
		circuit := e.state.Circuits[circuitKey(t.scope)]
		if (circuit.HalfOpen && circuit.ProbeID != t.id) || (!t.halfOpen && now.Before(circuit.OpenUntil)) {
			return admissionError("circuit_open", 503, e.circuitOpen)
		}
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if counter.count >= MaxRequestAttempts {
		return admissionError("request_attempt_limit", 503, 0)
	}
	project := t.scope.Origin + "|" + t.scope.Project
	if t.scope.Project != "" && counter.projects[project] {
		return admissionError("budget_project_already_attempted", 503, 0)
	}
	// Set this before entering RoundTrip. No concurrent compensation can win
	// after the context count is consumed.
	t.started.Store(true)
	counter.count++
	if t.scope.Project != "" {
		counter.projects[project] = true
	}
	return nil
}

func (t *Ticket) Compensate(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.compensated {
		return nil
	}
	if t.started.Load() {
		return admissionError("budget_transmission_started", 409, 0)
	}
	if !t.managed {
		t.compensated = true
		return nil
	}
	e := t.engine
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.unhealthy {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	if err := e.appendLocked(ctx, "compensate", compensation{TargetAttemptID: t.id}, true); err != nil {
		return err
	}
	t.compensated = true
	return nil
}

// Release ends in-flight ownership. It never refunds a started transmission.
// Callers must separately Compensate an unused reservation before releasing it.
func (t *Ticket) Release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	if !t.managed {
		return
	}
	e := t.engine
	e.mu.Lock()
	defer e.mu.Unlock()
	key := entryKey(t.scope, t.day)
	if e.inflight[key] > 1 {
		e.inflight[key]--
	} else {
		delete(e.inflight, key)
	}
	if t.started.Load() || t.compensated {
		delete(e.state.Reservations, t.id)
	}
	if t.halfOpen && !e.closed && !e.unhealthy {
		circuit := e.state.Circuits[circuitKey(t.scope)]
		if circuit.HalfOpen && circuit.ProbeID == t.id {
			circuit.HalfOpen, circuit.ProbeID = false, ""
			if t.started.Load() {
				circuit.OpenUntil = e.now().Add(e.circuitOpen)
			}
			if err := e.appendLocked(context.Background(), "circuit", circuit, true); err != nil {
				e.unhealthy = true
			}
		}
	}
	if !e.closed && !e.unhealthy && e.sequence-e.lastCheckpoint >= e.checkpointRecords {
		if err := e.checkpointLocked(context.Background()); err != nil {
			e.unhealthy = true
		}
	}
}

func (e *Engine) InFlight(scope Scope) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	scope = normalizedScope(scope)
	return e.inFlightLocked(scope)
}

func (e *Engine) inFlightLocked(scope Scope) int {
	count := 0
	for key, active := range e.inflight {
		for _, entry := range e.state.Entries {
			if entryKey(scope, entry.Day) == key {
				count += active
				break
			}
		}
	}
	return count
}

func (e *Engine) UpdateModels(models map[string]ModelPolicy) error {
	updated := make(map[string]ModelPolicy, len(models))
	for model, policy := range models {
		model = canonicalModel(model)
		if model == "" || policy.DailyLimit <= 0 {
			return errors.New("invalid managed model policy")
		}
		updated[model] = policy
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	e.models = updated
	return nil
}
