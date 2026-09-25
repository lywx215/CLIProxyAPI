package diagnostics

import (
	"context"
	"time"
)

type attemptKey struct{}
type attemptIdentity struct {
	id, scope string
	number    uint64
	started   time.Time
}

// ExecutorAttempt is invoked by the conductor at each actual model executor
// dispatch, including its explicit retries. HTTP sends never allocate attempts.
// Only the instrumented Gemini family participates in this semantic scope.
func ExecutorAttempt(ctx context.Context, provider string) context.Context {
	if provider != "gemini" && provider != "antigravity" {
		return ctx
	}
	s := ServerFromContext(ctx)
	if s == nil {
		return ctx
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return ctx
	}
	s.attempts++
	return context.WithValue(ctx, attemptKey{}, attemptIdentity{randomHex(8), "conductor_executor", s.attempts, time.Now()})
}

func applyAttempt(ctx context.Context, r *Record) {
	if a, ok := ctx.Value(attemptKey{}).(attemptIdentity); ok {
		r.AttemptID = &a.id
		r.AttemptNo = &a.number
		r.RetryScope = &a.scope
	}
}
