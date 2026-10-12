package budget

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

type reservation struct {
	ID          string `json:"id"`
	Scope       Scope  `json:"scope"`
	Day         string `json:"day"`
	Limit       int    `json:"limit"`
	Compensated bool   `json:"compensated,omitempty"`
}

type compensation struct {
	TargetAttemptID string `json:"target_attempt_id"`
}

type diskState struct {
	VolumeID         string                  `json:"volume_id"`
	Initialized      bool                    `json:"initialized"`
	State            string                  `json:"state"`
	FreezeUntil      time.Time               `json:"freeze_until,omitempty"`
	LastDay          string                  `json:"last_day"`
	Revision         uint64                  `json:"revision"`
	Entries          map[string]Entry        `json:"entries"`
	Reservations     map[string]reservation  `json:"reservations"`
	Backups          map[string]BackupStatus `json:"backups"`
	Circuits         map[string]CircuitView  `json:"circuits"`
	CredentialBlocks map[string]bool         `json:"credential_blocks"`
}

type initMetadata struct {
	Version       int       `json:"version"`
	VolumeID      string    `json:"volume_id"`
	CreatedAt     time.Time `json:"created_at"`
	RecoveryUntil time.Time `json:"recovery_until,omitempty"`
	RecoveredAt   time.Time `json:"recovered_at,omitempty"`
	EstimatedDay  string    `json:"estimated_day,omitempty"`
	Signature     string    `json:"signature"`
}

type Engine struct {
	mu                sync.Mutex
	path              string
	models            map[string]ModelPolicy
	now               func() time.Time
	zone              *time.Location
	lock              *os.File
	wal               *os.File
	sequence          uint64
	state             diskState
	meta              initMetadata
	secret            []byte
	closed            bool
	unhealthy         bool
	inflight          map[string]int
	beforePersist     func(string) error
	circuitThreshold  int
	circuitWindow     time.Duration
	circuitOpen       time.Duration
	maxInFlight       int
	jitter            func(time.Duration) time.Duration
	checkpointRecords uint64
	lastCheckpoint    uint64
}

func emptyState() diskState {
	return diskState{State: StateUninitialized, Entries: map[string]Entry{}, Reservations: map[string]reservation{}, Backups: map[string]BackupStatus{}, Circuits: map[string]CircuitView{}, CredentialBlocks: map[string]bool{}}
}

func Open(ctx context.Context, options Options) (*Engine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.Path) == "" {
		return nil, errors.New("budget storage path is required")
	}
	path, err := filepath.Abs(options.Path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create budget storage: %w", err)
	}
	lock, err := acquireLock(ctx, filepath.Join(path, "writer.lock"), options.LockTimeout)
	if err != nil {
		return nil, err
	}
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		_ = unlockFile(lock)
		_ = lock.Close()
		return nil, fmt.Errorf("load budget reset timezone: %w", err)
	}
	e := &Engine{path: path, models: map[string]ModelPolicy{}, now: options.Now, zone: zone, lock: lock, state: emptyState(), inflight: map[string]int{}, beforePersist: options.BeforePersist, circuitThreshold: options.CircuitThreshold, circuitWindow: options.CircuitWindow, circuitOpen: options.CircuitOpen, maxInFlight: options.MaxInFlight}
	e.jitter = options.Jitter
	e.checkpointRecords = 1024
	if options.CheckpointRecords > 0 {
		e.checkpointRecords = uint64(options.CheckpointRecords)
	}
	if e.jitter == nil {
		e.jitter = func(base time.Duration) time.Duration {
			value, err := rand.Int(rand.Reader, big.NewInt(int64(base/10)+1))
			if err != nil {
				return 0
			}
			return time.Duration(value.Int64())
		}
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.circuitThreshold <= 0 {
		e.circuitThreshold = 3
	}
	if e.circuitWindow <= 0 {
		e.circuitWindow = 30 * time.Second
	}
	if e.circuitOpen <= 0 {
		e.circuitOpen = 60 * time.Second
	}
	if e.maxInFlight <= 0 {
		e.maxInFlight = 1
	}
	for model, policy := range options.Models {
		if canonicalModel(model) == "" || policy.DailyLimit <= 0 {
			_ = e.Close()
			return nil, errors.New("managed model daily limit must be positive")
		}
		e.models[canonicalModel(model)] = policy
	}
	walPath := filepath.Join(path, "ledger.wal")
	_, metaErr := os.Stat(filepath.Join(path, "init.meta"))
	_, secretErr := os.Stat(filepath.Join(path, "init.secret"))
	walInfo, walErr := os.Stat(walPath)
	anyExisting := !errors.Is(metaErr, os.ErrNotExist) || !errors.Is(secretErr, os.ErrNotExist) || (walErr == nil && walInfo.Size() > 0) || (walErr != nil && !errors.Is(walErr, os.ErrNotExist))
	e.wal, err = os.OpenFile(walPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	valid := false
	if metaErr == nil && secretErr == nil && walErr == nil && walInfo.Size() > 0 {
		if err = e.readMetadata(); err == nil {
			e.sequence, err = replayWAL(e.wal, e.applyRecord)
			valid = err == nil && e.sequence > 0 && e.state.Initialized && e.state.VolumeID == e.meta.VolumeID
			if valid && !e.meta.RecoveryUntil.IsZero() && e.meta.RecoveredAt.IsZero() {
				e.state.State, e.state.FreezeUntil = StateDisaster, e.meta.RecoveryUntil
			}
		}
	}
	if !valid && (anyExisting || options.ExpectedInitialized) {
		if err = e.enterRecoveryLocked(ctx, e.now()); err != nil {
			_ = e.Close()
			return nil, err
		}
	} else if valid && e.state.State == StateDisaster && !e.now().Before(e.state.FreezeUntil) {
		if err = e.ensureReadyLocked(ctx, e.now()); err != nil {
			_ = e.Close()
			return nil, err
		}
	}
	if valid {
		for _, circuit := range e.state.Circuits {
			if circuit.HalfOpen {
				circuit.HalfOpen, circuit.ProbeID = false, ""
				circuit.OpenUntil = e.now().Add(e.circuitOpen)
				if err = e.appendLocked(ctx, "circuit", circuit, true); err != nil {
					_ = e.Close()
					return nil, err
				}
			}
		}
		// Replayed tickets belong to an earlier process and cannot be refunded.
		e.state.Reservations = map[string]reservation{}
		e.lastCheckpoint = e.sequence
	}
	return e, nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (e *Engine) day(now time.Time) string { return now.In(e.zone).Format("2006-01-02") }
func (e *Engine) nextReset(now time.Time) time.Time {
	local := now.In(e.zone)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, e.zone)
}

func canonicalModel(model string) string {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if index := strings.IndexByte(model, '('); index >= 0 {
		model = model[:index]
	}
	return strings.ToLower(strings.TrimSpace(model))
}
func normalizedScope(scope Scope) Scope {
	scope.Project = strings.TrimSpace(scope.Project)
	scope.Model = canonicalModel(scope.Model)
	scope.AuthIndex = strings.TrimSpace(scope.AuthIndex)
	scope.CredentialID = strings.TrimSpace(scope.CredentialID)
	scope.Origin = strings.TrimRight(strings.ToLower(strings.TrimSpace(scope.Origin)), "/")
	if scope.Origin == "" {
		scope.Origin = "https://generativelanguage.googleapis.com"
	}
	return scope
}

func credentialKey(scope Scope) string {
	if scope.CredentialID != "" {
		return scope.CredentialID
	}
	return scope.AuthIndex
}
func entryKey(scope Scope, day string) string {
	value, _ := json.Marshal([]string{scope.Origin, scope.Project, scope.Model, day})
	return string(value)
}
func circuitKey(scope Scope) string {
	value, _ := json.Marshal([]string{scope.Origin, scope.Model})
	return string(value)
}

func (e *Engine) Initialize(ctx context.Context, arguments ...InitializeOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.closed || e.unhealthy {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	if e.state.State != StateUninitialized || e.state.Initialized {
		return admissionError("budget_already_initialized", http.StatusConflict, 0)
	}
	if len(arguments) > 1 {
		return errors.New("one initialization options value is permitted")
	}
	now := e.now()
	state := emptyState()
	state.Initialized, state.State, state.LastDay = true, StateReady, e.day(now)
	if len(arguments) == 1 {
		for _, initial := range arguments[0].Used {
			scope := normalizedScope(initial.Scope)
			policy, ok := e.models[scope.Model]
			if !ok || scope.Project == "" || initial.Used < 0 {
				return errors.New("invalid first-day budget estimate")
			}
			key := entryKey(scope, state.LastDay)
			if _, exists := state.Entries[key]; exists {
				return errors.New("duplicate first-day budget estimate")
			}
			state.Entries[key] = Entry{Origin: scope.Origin, Project: scope.Project, Model: scope.Model, Day: state.LastDay, Used: initial.Used, Limit: policy.DailyLimit, DailyExhausted: initial.Used >= policy.DailyLimit}
		}
	}
	if err := e.newMetadata(now, time.Time{}); err != nil {
		e.unhealthy = true
		return err
	}
	state.VolumeID = e.meta.VolumeID
	return e.appendLocked(ctx, "initialize", state, true)
}

func (e *Engine) readMetadata() error {
	secret, err := os.ReadFile(filepath.Join(e.path, "init.secret"))
	if err != nil || len(secret) != 32 {
		return errors.New("budget initialization secret unavailable")
	}
	data, err := os.ReadFile(filepath.Join(e.path, "init.meta"))
	if err != nil {
		return err
	}
	var meta initMetadata
	if err = json.Unmarshal(data, &meta); err != nil || meta.Version != 1 || meta.VolumeID == "" {
		return errors.New("invalid budget initialization metadata")
	}
	signature, err := hex.DecodeString(meta.Signature)
	if err != nil {
		return errors.New("invalid budget metadata signature")
	}
	meta.Signature = ""
	payload, _ := json.Marshal(meta)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return errors.New("budget initialization signature mismatch")
	}
	meta.Signature = hex.EncodeToString(signature)
	e.secret, e.meta = secret, meta
	return nil
}

func (e *Engine) newMetadata(now, freeze time.Time) error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(e.path, "init.secret"), secret); err != nil {
		return err
	}
	e.secret, e.meta = secret, initMetadata{Version: 1, VolumeID: id, CreatedAt: now, RecoveryUntil: freeze}
	if freeze.IsZero() {
		e.meta.EstimatedDay = e.day(now)
	}
	return e.writeMetadata()
}

func (e *Engine) writeMetadata() error {
	if e.beforePersist != nil {
		if err := e.beforePersist("metadata"); err != nil {
			return err
		}
	}
	meta := e.meta
	meta.Signature = ""
	payload, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, e.secret)
	_, _ = mac.Write(payload)
	meta.Signature = hex.EncodeToString(mac.Sum(nil))
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(e.path, "init.meta"), data); err != nil {
		return err
	}
	e.meta = meta
	return nil
}

func (e *Engine) enterRecoveryLocked(ctx context.Context, now time.Time) error {
	// Replace an untrustworthy journal with an explicit recovery checkpoint. A
	// signed marker survives subsequent restarts and does not extend every day.
	state := emptyState()
	state.Initialized, state.State, state.LastDay = true, StateDisaster, e.day(now)
	state.FreezeUntil = e.nextReset(now)
	state.Backups = e.state.Backups
	state.Revision = e.state.Revision
	for _, backup := range state.Backups {
		state.Revision = max(state.Revision, backup.Revision)
	}
	if err := e.newMetadata(now, state.FreezeUntil); err != nil {
		return err
	}
	state.VolumeID = e.meta.VolumeID
	if err := e.wal.Truncate(0); err != nil {
		return err
	}
	if _, err := e.wal.Seek(0, 0); err != nil {
		return err
	}
	e.sequence = 0
	e.state = emptyState()
	return e.appendLocked(ctx, "recover", state, true)
}

func (e *Engine) ensureReadyLocked(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.closed || e.unhealthy {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	if e.state.State == StateUninitialized {
		return admissionError("budget_not_initialized", 503, 0)
	}
	day := e.day(now)
	if day < e.state.LastDay {
		return admissionError("budget_clock_unsafe", 503, 0)
	}
	if e.state.State == StateDisaster {
		if now.Before(e.state.FreezeUntil) {
			return admissionError("budget_recovery_frozen", 503, e.state.FreezeUntil.Sub(now))
		}
		e.meta.RecoveredAt = now
		if err := e.writeMetadata(); err != nil {
			e.unhealthy = true
			return admissionError("budget_storage_unavailable", 503, 0)
		}
		if err := e.appendLocked(ctx, "resume", struct {
			Day string `json:"day"`
		}{day}, true); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) appendLocked(ctx context.Context, operation string, value any, business bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.beforePersist != nil {
		if err := e.beforePersist(operation); err != nil {
			e.unhealthy = true
			return admissionError("budget_storage_unavailable", 503, 0)
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	revision := e.state.Revision
	if state, ok := value.(diskState); ok {
		revision = max(revision, state.Revision)
	}
	if business {
		revision++
	}
	record := walRecord{Version: 1, Sequence: e.sequence + 1, Revision: revision, Operation: operation, Data: data}
	if err = appendWAL(e.wal, record); err != nil {
		e.unhealthy = true
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	if err = e.applyRecord(record); err != nil {
		e.unhealthy = true
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	e.sequence = record.Sequence
	return nil
}

func (e *Engine) applyRecord(record walRecord) error {
	switch record.Operation {
	case "initialize", "checkpoint", "recover", "restore":
		var state diskState
		if err := json.Unmarshal(record.Data, &state); err != nil {
			return err
		}
		if !state.Initialized || state.Entries == nil || state.Reservations == nil || state.Backups == nil || state.Circuits == nil || state.CredentialBlocks == nil {
			return errors.New("invalid budget checkpoint")
		}
		e.state = state
	case "resume":
		var value struct {
			Day string `json:"day"`
		}
		if err := json.Unmarshal(record.Data, &value); err != nil {
			return err
		}
		e.state.State, e.state.FreezeUntil, e.state.LastDay = StateReady, time.Time{}, value.Day
	case "reserve":
		var value reservation
		if err := json.Unmarshal(record.Data, &value); err != nil {
			return err
		}
		if value.ID == "" || value.Scope.Project == "" || value.Limit <= 0 {
			return errors.New("invalid budget reservation")
		}
		if _, exists := e.state.Reservations[value.ID]; exists {
			return errors.New("duplicate budget reservation")
		}
		key := entryKey(value.Scope, value.Day)
		entry := e.state.Entries[key]
		entry.Origin, entry.Project, entry.Model, entry.Day, entry.Limit = value.Scope.Origin, value.Scope.Project, value.Scope.Model, value.Day, value.Limit
		entry.Used++
		e.state.Entries[key] = entry
		e.state.Reservations[value.ID] = value
		if value.Day > e.state.LastDay {
			e.state.LastDay = value.Day
		}
	case "compensate":
		var valueData compensation
		if err := json.Unmarshal(record.Data, &valueData); err != nil {
			return err
		}
		id := valueData.TargetAttemptID
		value, ok := e.state.Reservations[id]
		if !ok {
			return errors.New("unknown budget reservation")
		}
		if !value.Compensated {
			key := entryKey(value.Scope, value.Day)
			entry := e.state.Entries[key]
			if entry.Used <= 0 {
				return errors.New("invalid compensation balance")
			}
			entry.Used--
			e.state.Entries[key] = entry
			value.Compensated = true
			e.state.Reservations[id] = value
		}
	case "entry":
		var entry Entry
		if err := json.Unmarshal(record.Data, &entry); err != nil {
			return err
		}
		scope := normalizedScope(Scope{Origin: entry.Origin, Project: entry.Project, Model: entry.Model})
		e.state.Entries[entryKey(scope, entry.Day)] = entry
		if entry.Day > e.state.LastDay {
			e.state.LastDay = entry.Day
		}
	case "circuit":
		var circuit CircuitView
		if err := json.Unmarshal(record.Data, &circuit); err != nil {
			return err
		}
		e.state.Circuits[circuitKey(normalizedScope(Scope{Origin: circuit.Origin, Model: circuit.Model}))] = circuit
	case "credential":
		var index string
		if err := json.Unmarshal(record.Data, &index); err != nil {
			return err
		}
		e.state.CredentialBlocks[index] = true
	case "backup":
		var backup BackupStatus
		if err := json.Unmarshal(record.Data, &backup); err != nil {
			return err
		}
		e.state.Backups[backup.Day] = backup
	default:
		return errors.New("unsupported budget journal operation")
	}
	e.state.Revision = record.Revision
	return nil
}

// Available is a non-mutating scheduling hint. Reserve remains authoritative.
func (e *Engine) Available(scope Scope) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.viewLocked(normalizedScope(scope), e.now()).Available
}

// View is read-only and shares the scheduler's admission hint. It must not
// perform recovery writes, reserve a half-open probe, or claim a COS backup.
func (e *Engine) View(scope Scope) EntryView {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.viewLocked(normalizedScope(scope), e.now())
}
func (e *Engine) viewLocked(scope Scope, now time.Time) EntryView {
	entry := e.state.Entries[entryKey(scope, e.day(now))]
	policy, managed := e.models[scope.Model]
	result := EntryView{State: "ready", Used: entry.Used, Limit: policy.DailyLimit, InFlight: e.inFlightLocked(scope), Estimated: e.meta.EstimatedDay == e.day(now)}
	if entry.ObservedLimit > 0 && entry.ObservedLimit < result.Limit {
		result.Limit = entry.ObservedLimit
	}
	switch {
	case e.closed:
		result.State = StateClosed
	case e.unhealthy:
		result.State = StateStorageError
	case !managed:
		result.State = "unmanaged"
		result.Available = true
	case scope.Project == "":
		result.State = "project_unknown"
	case !e.state.Initialized:
		result.State = StateUninitialized
	case e.day(now) < e.state.LastDay:
		result.State = "clock_unsafe"
	case e.state.State == StateDisaster && now.Before(e.state.FreezeUntil):
		result.State = StateDisaster
		result.RetryAt = e.state.FreezeUntil
	case credentialKey(scope) != "" && e.state.CredentialBlocks[credentialKey(scope)]:
		result.State = "quarantined"
	case credentialKey(scope) != "" && e.state.CredentialBlocks[credentialKey(scope)+"\x00"+scope.Model]:
		result.State = "model_restricted"
	case entry.DailyExhausted || entry.Used >= result.Limit:
		result.State = "daily_exhausted"
		result.RetryAt = e.nextReset(now)
	case now.Before(e.cooldownUntilLocked(scope)):
		result.State = "cooldown"
		result.RetryAt = e.cooldownUntilLocked(scope)
	default:
		circuit := e.state.Circuits[circuitKey(scope)]
		if now.Before(circuit.OpenUntil) || circuit.HalfOpen {
			result.State = "circuit_open"
			result.RetryAt = circuit.OpenUntil
		} else if result.InFlight >= e.maxInFlight {
			result.State = "busy"
		} else {
			result.Available = true
		}
	}
	return result
}

func (e *Engine) Observe(ctx context.Context, scope Scope, signal Signal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	scope = normalizedScope(scope)
	now := e.now()
	if _, managed := e.models[scope.Model]; !managed {
		return nil
	}
	if err := e.ensureReadyLocked(ctx, now); err != nil {
		return err
	}
	if signal.Kind == SignalSuccess {
		for _, entry := range e.state.Entries {
			if entry.Origin == scope.Origin && entry.Project == scope.Project && entry.Model == scope.Model && !entry.DailyExhausted && (!entry.CooldownUntil.IsZero() || entry.BackoffLevel > 0) {
				entry.CooldownUntil, entry.BackoffLevel = time.Time{}, 0
				if err := e.appendLocked(ctx, "entry", entry, true); err != nil {
					return err
				}
			}
		}
	}
	if signal.Kind == Signal503 || signal.Kind == SignalSuccess {
		key := circuitKey(scope)
		circuit := e.state.Circuits[key]
		circuit.Projects = cloneProjectTimes(circuit.Projects)
		circuit.Origin, circuit.Model = scope.Origin, scope.Model
		if signal.Kind == SignalSuccess {
			if !circuit.OpenUntil.IsZero() && !circuit.HalfOpen {
				return nil
			}
			if circuit.HalfOpen && signal.AttemptID != circuit.ProbeID {
				return nil
			}
			if circuit.Failures == 0 && circuit.OpenUntil.IsZero() && !circuit.HalfOpen {
				return nil
			}
			circuit.Failures, circuit.OpenUntil, circuit.HalfOpen, circuit.WindowStart, circuit.ProbeID = 0, time.Time{}, false, time.Time{}, ""
			circuit.Projects = map[string]time.Time{}
		} else {
			for project, at := range circuit.Projects {
				if now.Sub(at) > e.circuitWindow {
					delete(circuit.Projects, project)
				}
			}
			circuit.Projects[scope.Project] = now
			if circuit.WindowStart.IsZero() || now.Sub(circuit.WindowStart) > e.circuitWindow {
				circuit.WindowStart = now
				circuit.Failures = 0
			}
			circuit.Failures++
			if len(circuit.Projects) >= e.circuitThreshold || circuit.HalfOpen {
				circuit.OpenUntil = now.Add(e.circuitOpen)
				circuit.HalfOpen = false
				circuit.ProbeID = ""
			}
			day := e.day(now)
			entry := e.state.Entries[entryKey(scope, day)]
			entry.Origin, entry.Project, entry.Model, entry.Day, entry.Limit = scope.Origin, scope.Project, scope.Model, day, e.models[scope.Model].DailyLimit
			delay := max(60*time.Second, signal.RetryAfter)
			deadline := now.Add(delay)
			if deadline.After(entry.CooldownUntil) {
				entry.CooldownUntil = deadline
			}
			if err := e.appendLocked(ctx, "entry", entry, true); err != nil {
				return err
			}
		}
		return e.appendLocked(ctx, "circuit", circuit, true)
	}
	if signal.Kind == SignalCredentialInvalid || signal.Kind == SignalModelUnsupported {
		if credentialKey(scope) == "" {
			return errors.New("credential signal requires a credential identity")
		}
		index := credentialKey(scope)
		if signal.Kind == SignalModelUnsupported {
			index += "\x00" + scope.Model
		}
		return e.appendLocked(ctx, "credential", index, true)
	}
	day := e.day(now)
	key := entryKey(scope, day)
	entry := e.state.Entries[key]
	entry.Origin, entry.Project, entry.Model, entry.Day, entry.Limit = scope.Origin, scope.Project, scope.Model, day, e.models[scope.Model].DailyLimit
	switch signal.Kind {
	case SignalRPD:
		entry.DailyExhausted = true
		deadline := e.nextReset(now)
		if hinted := now.Add(signal.RetryAfter); hinted.After(deadline) {
			deadline = hinted
		}
		if deadline.After(entry.CooldownUntil) {
			entry.CooldownUntil = deadline
		}
		if signal.Limit > 0 {
			entry.ObservedLimit = signal.Limit
		}
	case SignalRPM, SignalTPM, SignalUnknown429, SignalTemporaryAuth:
		if signal.RetryAfter <= 0 && now.Before(entry.CooldownUntil) {
			return nil
		}
		base := min(time.Minute*time.Duration(1<<min(entry.BackoffLevel, 4)), 15*time.Minute)
		delay := max(base, signal.RetryAfter)
		delay += min(max(e.jitter(delay), time.Duration(0)), delay/10)
		if !now.Before(entry.CooldownUntil) {
			entry.BackoffLevel = min(entry.BackoffLevel+1, 4)
		}
		deadline := now.Add(delay)
		if deadline.After(entry.CooldownUntil) {
			entry.CooldownUntil = deadline
		}
	default:
		return errors.New("unsupported budget signal")
	}
	return e.appendLocked(ctx, "entry", entry, true)
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}
func (e *Engine) ExportSnapshot(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if e.closed || e.unhealthy {
		return Snapshot{}, admissionError("budget_storage_unavailable", 503, 0)
	}
	return e.snapshotLocked(), nil
}
func (e *Engine) snapshotLocked() Snapshot {
	state := e.state.State
	if state == StateDisaster && !e.now().Before(e.state.FreezeUntil) && e.day(e.now()) >= e.state.LastDay {
		// Read-only projections allow Pick to reach the next durable Reserve.
		state = StateReady
	}
	if e.unhealthy {
		state = StateStorageError
	}
	if e.closed {
		state = StateClosed
	}
	result := Snapshot{Version: 1, Revision: e.state.Revision, Day: e.day(e.now()), Initialized: e.state.Initialized, State: state, FreezeUntil: e.state.FreezeUntil, Entries: []Entry{}, Backups: []BackupStatus{}, Circuits: []CircuitView{}}
	for _, entry := range e.state.Entries {
		if policy, ok := e.models[entry.Model]; ok {
			entry.Limit = policy.DailyLimit
			if entry.ObservedLimit > 0 && entry.ObservedLimit < entry.Limit {
				entry.Limit = entry.ObservedLimit
			}
		}
		result.Entries = append(result.Entries, entry)
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		a, b := result.Entries[i], result.Entries[j]
		return a.Day+"|"+a.Origin+"|"+a.Project+"|"+a.Model < b.Day+"|"+b.Origin+"|"+b.Project+"|"+b.Model
	})
	var backedRevision uint64
	for _, backup := range e.state.Backups {
		result.Backups = append(result.Backups, backup)
		if backup.Day > result.Backup.Day {
			result.Backup = backup
		}
		if backup.Success && backup.Revision > backedRevision {
			backedRevision = backup.Revision
		}
	}
	sort.Slice(result.Backups, func(i, j int) bool { return result.Backups[i].Day < result.Backups[j].Day })
	for _, circuit := range e.state.Circuits {
		circuit.Projects = cloneProjectTimes(circuit.Projects)
		result.Circuits = append(result.Circuits, circuit)
	}
	sort.Slice(result.Circuits, func(i, j int) bool {
		return result.Circuits[i].Origin+result.Circuits[i].Model < result.Circuits[j].Origin+result.Circuits[j].Model
	})
	result.Dirty = e.state.Revision > backedRevision
	return result
}

func cloneProjectTimes(source map[string]time.Time) map[string]time.Time {
	copy := make(map[string]time.Time, len(source))
	for project, at := range source {
		copy[project] = at
	}
	return copy
}

func (e *Engine) ClaimBackup(ctx context.Context, now time.Time) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if e.closed || e.unhealthy {
		return false, admissionError("budget_storage_unavailable", 503, 0)
	}
	if !e.state.Initialized || (e.state.State == StateDisaster && now.Before(e.state.FreezeUntil)) {
		return false, nil
	}
	if err := e.ensureReadyLocked(ctx, now); err != nil {
		return false, err
	}
	day := e.day(now)
	if e.state.Backups[day].Attempted || !e.snapshotLocked().Dirty {
		return false, nil
	}
	status := BackupStatus{Day: day, Attempted: true, Revision: e.state.Revision, At: now}
	if err := e.appendLocked(ctx, "backup", status, false); err != nil {
		return false, err
	}
	return true, nil
}
func (e *Engine) SetBackupResult(ctx context.Context, now time.Time, success bool, message string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.unhealthy {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	day := e.day(now)
	status, ok := e.state.Backups[day]
	if !ok || !status.Attempted {
		return errors.New("backup was not claimed")
	}
	if status.Success {
		return nil
	}
	status.Success = success
	// Only bounded, caller-sanitized diagnostics belong in the local ledger.
	if len(message) > 300 {
		message = message[:300]
	}
	status.Message = message
	return e.appendLocked(ctx, "backup", status, false)
}

// Restore never treats a remote snapshot as an exact current-day counter. Local
// state loss freezes this Pacific day, including all backup writes, regardless
// of the backup's age. Next-day admission creates durable recovery metadata.
func (e *Engine) Restore(ctx context.Context, snapshot Snapshot) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.unhealthy {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	if e.state.State == StateReady {
		return admissionError("budget_already_initialized", 409, 0)
	}
	if snapshot.Version != 1 || !snapshot.Initialized {
		return errors.New("invalid budget backup snapshot")
	}
	if e.state.State == StateUninitialized {
		if err := e.enterRecoveryLocked(ctx, e.now()); err != nil {
			return err
		}
	}
	state := e.state
	state.Backups = make(map[string]BackupStatus, len(snapshot.Backups)+len(e.state.Backups))
	for day, status := range e.state.Backups {
		state.Backups[day] = status
	}
	for _, status := range snapshot.Backups {
		if status.Attempted {
			state.Backups[status.Day] = status
		}
	}
	if snapshot.Backup.Attempted {
		state.Backups[snapshot.Backup.Day] = snapshot.Backup
	}
	state.Revision = max(state.Revision, snapshot.Revision)
	for _, backup := range state.Backups {
		state.Revision = max(state.Revision, backup.Revision)
	}
	return e.appendLocked(ctx, "restore", state, true)
}

func (e *Engine) Checkpoint(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.checkpointLocked(ctx)
}

func (e *Engine) checkpointLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.closed || e.unhealthy || !e.state.Initialized {
		return admissionError("budget_storage_unavailable", 503, 0)
	}
	if e.beforePersist != nil {
		if err := e.beforePersist("checkpoint"); err != nil {
			e.unhealthy = true
			return admissionError("budget_storage_unavailable", 503, 0)
		}
	}
	state := e.state
	state.Entries = make(map[string]Entry, len(e.state.Entries))
	state.Reservations = make(map[string]reservation, len(e.state.Reservations))
	now := e.now()
	local := now.In(e.zone)
	cutoff := time.Date(local.Year(), local.Month(), local.Day()-1, 0, 0, 0, 0, e.zone).Format("2006-01-02")
	for key, entry := range e.state.Entries {
		if entry.Day >= cutoff || now.Before(entry.CooldownUntil) || e.inflight[key] > 0 {
			state.Entries[key] = entry
		}
	}
	for id, reservation := range e.state.Reservations {
		if reservation.Day >= cutoff || e.inflight[entryKey(reservation.Scope, reservation.Day)] > 0 {
			state.Reservations[id] = reservation
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	record := walRecord{Version: 1, Sequence: e.sequence + 1, Revision: e.state.Revision, Operation: "checkpoint", Data: data}
	frame, err := encodeRecord(record)
	if err != nil {
		return err
	}
	frame = append(append([]byte(nil), walMagic...), frame...)
	// Close before replacement for Windows. Failure remains fail-closed even if
	// a new process could subsequently recover the old durable journal.
	if err = e.wal.Close(); err != nil {
		e.unhealthy = true
		return err
	}
	if err = atomicWrite(filepath.Join(e.path, "ledger.wal"), frame); err != nil {
		e.unhealthy = true
		return err
	}
	e.wal, err = os.OpenFile(filepath.Join(e.path, "ledger.wal"), os.O_RDWR, 0o600)
	if err != nil {
		e.unhealthy = true
		return err
	}
	e.sequence = record.Sequence
	e.state = state
	e.lastCheckpoint = e.sequence
	return nil
}

func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	var err error
	if e.wal != nil {
		err = errors.Join(e.wal.Sync(), e.wal.Close())
	}
	if e.lock != nil {
		err = errors.Join(err, unlockFile(e.lock), e.lock.Close())
	}
	return err
}
