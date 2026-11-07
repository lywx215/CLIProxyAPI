// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
// Package resources accounts for retained Business buffers and bounded
// allocation headroom. It is an admission budget, not a measurement of Go RSS.
package resources

import (
	"context"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

const DefaultLimitBytes int64 = 256 << 20
const GenerationReserveBytes int64 = 32 << 20
const AttachmentUploadReserveBytes int64 = 1 << 20

var ErrExhausted = &contract.Failure{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "Business memory budget is exhausted"}
var ErrLeaseClosed = &contract.Failure{Code: 503, Status: "UNAVAILABLE", Message: "Business resource lease is closed"}

type Snapshot struct {
	LimitBytes     int64 `json:"limit_bytes"`
	InUseBytes     int64 `json:"in_use_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

type Budget struct {
	mu          sync.Mutex
	limit, used int64
}
type Lease struct {
	budget *Budget
	ctx    context.Context
	used   int64
	closed bool
}
type leaseKey struct{}

func New(limitBytes int64) *Budget { return &Budget{limit: max(0, limitBytes)} }

// SetLimit never cancels or revokes an existing reservation. If a lower limit
// is below current usage, new reservations fail until usage falls below it.
func (b *Budget) SetLimit(limitBytes int64) { b.mu.Lock(); b.limit = max(0, limitBytes); b.mu.Unlock() }
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{LimitBytes: b.limit, InUseBytes: b.used, AvailableBytes: max(0, b.limit-b.used)}
}

func (b *Budget) Begin(ctx context.Context) (context.Context, *Lease) {
	lease := &Lease{budget: b, ctx: ctx}
	return context.WithValue(ctx, leaseKey{}, lease), lease
}
func FromContext(ctx context.Context) *Lease {
	lease, _ := ctx.Value(leaseKey{}).(*Lease)
	return lease
}

// Reserve obtains permission before allocation. Its release is idempotent,
// including when called after Close. Cancellation forbids new reservations;
// it does not free bytes while the cancelled request can still hold buffers.
func (l *Lease) Reserve(bytes int64) (func(), error) {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := l.ctx.Err(); err != nil {
		return nil, ContextError(err)
	}
	if l.closed {
		return nil, ErrLeaseClosed
	}
	if bytes < 0 {
		return nil, contract.Fail(500, "INTERNAL", "Invalid Business resource reservation")
	}
	if bytes == 0 {
		return func() {}, nil
	}
	if bytes > b.limit-b.used {
		return nil, ErrExhausted
	}
	b.used += bytes
	l.used += bytes
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if !l.closed {
				b.used -= bytes
				l.used -= bytes
			}
		})
	}, nil
}

func (l *Lease) Close() {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if !l.closed {
		b.used -= l.used
		l.used = 0
		l.closed = true
	}
}

// Reserve leaves standalone low-level callers compatible. Every production
// Business HTTP operation receives a lease from Host; tests may inject one.
func Reserve(ctx context.Context, bytes int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, ContextError(err)
	}
	if lease := FromContext(ctx); lease != nil {
		return lease.Reserve(bytes)
	}
	return func() {}, nil
}

func ContextError(err error) error {
	if err == context.DeadlineExceeded {
		return contract.Fail(504, "DEADLINE_EXCEEDED", "Business resource operation exceeded its deadline")
	}
	if err == context.Canceled {
		return contract.Fail(499, "CANCELLED", "Business resource operation was cancelled")
	}
	return err
}

type attachmentAllowanceKey struct{}

func WithAttachmentAllowance(ctx context.Context, bytes int64) context.Context {
	return context.WithValue(ctx, attachmentAllowanceKey{}, bytes)
}
func AttachmentAllowance(ctx context.Context) (int64, bool) {
	bytes, ok := ctx.Value(attachmentAllowanceKey{}).(int64)
	return bytes, ok
}
