// Package budget provides durable, local admission control for upstream requests.
// It deliberately does not depend on the application's configuration or auth packages.
package budget

import (
	"context"
	"net/http"
	"time"
)

const MaxRequestAttempts = 3

type ModelPolicy struct {
	DailyLimit int `json:"daily_limit" yaml:"daily-limit"`
	RPM        int `json:"rpm,omitempty" yaml:"rpm,omitempty"`
	TPM        int `json:"tpm,omitempty" yaml:"tpm,omitempty"`
}

type BackupPolicy struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Prefix  string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	At      string `json:"at,omitempty" yaml:"at,omitempty"`
}

type Policy struct {
	Enabled       bool                   `json:"enabled" yaml:"enabled"`
	StoragePath   string                 `json:"storage_path" yaml:"storage-path"`
	COSBackup     BackupPolicy           `json:"cos_backup" yaml:"cos-backup"`
	ManagedModels map[string]ModelPolicy `json:"managed_models" yaml:"managed-models"`
}

type Options struct {
	Path                string
	Models              map[string]ModelPolicy
	Now                 func() time.Time
	LockTimeout         time.Duration
	ExpectedInitialized bool
	CircuitThreshold    int
	CircuitWindow       time.Duration
	CircuitOpen         time.Duration
	MaxInFlight         int
	// Jitter returns an additive delay; values are clamped to [0, base/10].
	Jitter            func(base time.Duration) time.Duration
	CheckpointRecords int
	// BeforePersist is a test seam. A returned error prevents the write and admission.
	BeforePersist func(operation string) error
}

// Scope identifies the actual upstream quota pool. AuthIndex is diagnostic only:
// credentials belonging to one project share the same project/model budget.
type Scope struct {
	Project      string `json:"project"`
	Model        string `json:"model"`
	AuthIndex    string `json:"auth_index,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Origin       string `json:"origin,omitempty"`
}

type InitializeOptions struct {
	// Used is an explicitly supplied first-day estimate, keyed by project/model.
	Used []InitialUsage
}

type InitialUsage struct {
	Scope Scope `json:"scope"`
	Used  int   `json:"used"`
}

type SignalKind string

const (
	SignalSuccess           SignalKind = "success"
	SignalRPM               SignalKind = "rpm"
	SignalTPM               SignalKind = "tpm"
	SignalRPD               SignalKind = "rpd"
	Signal503               SignalKind = "503"
	SignalCredentialInvalid SignalKind = "credential_invalid"
	SignalModelUnsupported  SignalKind = "model_unsupported"
	SignalUnknown429        SignalKind = "unknown_429"
	SignalTemporaryAuth     SignalKind = "temporary_auth"
)

type Signal struct {
	Kind       SignalKind
	RetryAfter time.Duration
	AttemptID  string
	// Limit is an optional authoritative daily limit observed from the upstream.
	Limit int
}

type Entry struct {
	Origin         string    `json:"origin"`
	Project        string    `json:"project"`
	Model          string    `json:"model"`
	Day            string    `json:"day"`
	Used           int       `json:"used"`
	Limit          int       `json:"limit"`
	CooldownUntil  time.Time `json:"cooldown_until,omitempty"`
	DailyExhausted bool      `json:"daily_exhausted,omitempty"`
	BackoffLevel   int       `json:"backoff_level,omitempty"`
	ObservedLimit  int       `json:"observed_limit,omitempty"`
}

type EntryView struct {
	State     string    `json:"state"`
	RetryAt   time.Time `json:"retry_at,omitempty"`
	Used      int       `json:"used"`
	Limit     int       `json:"limit"`
	InFlight  int       `json:"in_flight"`
	Estimated bool      `json:"estimated"`
	Available bool      `json:"available"`
}

type BackupStatus struct {
	Day       string    `json:"day"`
	Attempted bool      `json:"attempted"`
	Success   bool      `json:"success"`
	Revision  uint64    `json:"revision"`
	At        time.Time `json:"at"`
	Message   string    `json:"message,omitempty"`
}

type CircuitView struct {
	Origin      string               `json:"origin"`
	Model       string               `json:"model"`
	Failures    int                  `json:"failures"`
	OpenUntil   time.Time            `json:"open_until,omitempty"`
	HalfOpen    bool                 `json:"half_open"`
	WindowStart time.Time            `json:"window_start,omitempty"`
	ProbeID     string               `json:"probe_id,omitempty"`
	Projects    map[string]time.Time `json:"projects,omitempty"`
}

type Snapshot struct {
	Version     int            `json:"version"`
	Revision    uint64         `json:"revision"`
	Day         string         `json:"day"`
	Initialized bool           `json:"initialized"`
	State       string         `json:"state"`
	FreezeUntil time.Time      `json:"freeze_until,omitempty"`
	Dirty       bool           `json:"dirty"`
	Entries     []Entry        `json:"entries"`
	Backup      BackupStatus   `json:"backup"`
	Backups     []BackupStatus `json:"backups"`
	Circuits    []CircuitView  `json:"circuits"`
}

const (
	StateUninitialized = "uninitialized"
	StateReady         = "ready"
	StateDisaster      = "disaster"
	StateClosed        = "closed"
	StateStorageError  = "storage_error"
)

// Error intentionally contains no upstream model or project identity. The client
// response may expose its code and delay without exposing private routing names.
type Error struct {
	Code   string
	Delay  time.Duration
	status int
}

func (e *Error) Error() string {
	if e == nil {
		return "request admission unavailable"
	}
	switch e.Code {
	case "daily_budget_exhausted", "quota_cooldown":
		return "Request quota is temporarily unavailable."
	case "request_attempt_limit":
		return "Request attempt limit reached."
	case "circuit_open":
		return "The upstream service is temporarily unavailable."
	default:
		return "Request admission is temporarily unavailable."
	}
}

func (e *Error) StatusCode() int {
	if e == nil || e.status == 0 {
		return http.StatusServiceUnavailable
	}
	return e.status
}

func (e *Error) RetryAfter() *time.Duration {
	if e == nil || e.Delay <= 0 {
		return nil
	}
	d := e.Delay
	return &d
}

func admissionError(code string, status int, delay time.Duration) error {
	return &Error{Code: code, status: status, Delay: delay}
}

// Ledger is the narrow interface used by the optional backup worker.
type Ledger interface {
	ClaimBackup(context.Context, time.Time) (bool, error)
	ExportSnapshot(context.Context) (Snapshot, error)
	SetBackupResult(context.Context, time.Time, bool, string) error
}
