package usage

import (
	"context"
	"sync"
	"sync/atomic"
)

type streamDeliveryKey struct{}

type streamDelivery struct {
	passive   bool
	observed  atomic.Int32
	supported atomic.Int32
	once      sync.Once
	done      chan struct{}
	settleMu  sync.Mutex
	settled   chan struct{}
	err       error
}

// WithObservedHTTPDelivery leaves legacy providers unaware of this tracker.
// Only a producer explicitly opting into observations activates HTTP accounting.
func WithObservedHTTPDelivery(ctx context.Context) context.Context {
	if _, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery); ok {
		return ctx
	}
	return context.WithValue(ctx, streamDeliveryKey{}, &streamDelivery{passive: true, done: make(chan struct{})})
}

// FinishObservedDelivery is a no-op for legacy producers on the same handler.
func FinishObservedDelivery(ctx context.Context, err error) {
	d, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	if !ok || d.observed.Load() == 0 {
		return
	}
	d.once.Do(func() { d.err = err; close(d.done) })
}

func ObservedDeliverySupported(ctx context.Context) bool {
	d, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	return ok && d.observed.Load() > 0
}

func SupportObservedStreamDelivery(ctx context.Context) func() {
	stop := SupportStreamDelivery(ctx)
	d, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	if !ok {
		return stop
	}
	d.settleMu.Lock()
	if d.observed.Add(1) == 1 {
		d.settled = make(chan struct{})
	}
	d.settleMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.settleMu.Lock()
			if d.observed.Add(-1) == 0 {
				close(d.settled)
			}
			d.settleMu.Unlock()
			stop()
		})
	}
}

// WithStreamDelivery lets the HTTP consumer acknowledge delivery independently
// of request cancellation. The consumer must resolve the outcome on every exit.
func WithStreamDelivery(ctx context.Context) (context.Context, func(error)) {
	delivery := &streamDelivery{done: make(chan struct{})}
	return context.WithValue(ctx, streamDeliveryKey{}, delivery), func(err error) {
		delivery.once.Do(func() {
			delivery.err = err
			close(delivery.done)
		})
	}
}

// WithoutStreamDelivery isolates an internal execution from its caller's HTTP
// acknowledgment. Internal consumers retain EOF-based accounting completion;
// cancellation and all unrelated context values remain inherited.
func WithoutStreamDelivery(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, streamDeliveryKey{}, struct{}{})
}

// StreamDeliveryTracked reports whether a consumer will acknowledge delivery.
func StreamDeliveryTracked(ctx context.Context) bool {
	d, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	return ok && (!d.passive || d.observed.Load() > 0)
}

// WaitStreamDelivery waits for the consumer, not the upstream reader, to finish.
// Producers must close their output channel before waiting to avoid deadlocks.
func WaitStreamDelivery(ctx context.Context) (error, bool) {
	delivery, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	if !ok || (delivery.passive && delivery.observed.Load() == 0) {
		return nil, false
	}
	<-delivery.done
	return delivery.err, true
}

// SupportStreamDelivery opts an active producer into early terminal cancellation.
// The producer must reconcile cancellation with WaitStreamDelivery.
func SupportStreamDelivery(ctx context.Context) func() {
	delivery, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	if !ok {
		return func() {}
	}
	delivery.supported.Add(1)
	var once sync.Once
	return func() { once.Do(func() { delivery.supported.Add(-1) }) }
}

// StreamDeliverySupported reports whether an active producer understands delivery.
func StreamDeliverySupported(ctx context.Context) bool {
	delivery, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	return ok && (!delivery.passive || delivery.observed.Load() > 0) && delivery.supported.Load() > 0
}

// WaitObservedSettlement waits for frozen accounting, not transport cleanup.
func WaitObservedSettlement(ctx context.Context) {
	d, ok := ctx.Value(streamDeliveryKey{}).(*streamDelivery)
	if !ok {
		return
	}
	d.settleMu.Lock()
	settled := d.settled
	d.settleMu.Unlock()
	if settled != nil {
		<-settled
	}
}
