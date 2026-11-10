package usage

import (
	"context"
	"sync/atomic"
)

type modelAttemptSequenceKey struct{}

func WithModelAttemptSequence(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(modelAttemptSequenceKey{}).(*atomic.Uint64); ok {
		return ctx
	}
	return context.WithValue(ctx, modelAttemptSequenceKey{}, &atomic.Uint64{})
}

func NextModelAttempt(ctx context.Context) uint64 {
	if counter, ok := ctx.Value(modelAttemptSequenceKey{}).(*atomic.Uint64); ok {
		return counter.Add(1)
	}
	return 1
}
