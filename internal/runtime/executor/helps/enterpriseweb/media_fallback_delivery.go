package enterpriseweb

import (
	"context"
	"sync"
	"sync/atomic"
)

type mediaFallbackDeliveryKey struct{}
type mediaFallbackValidatorKey struct{}

type mediaFallbackDelivery struct {
	ready      chan struct{}
	onFallback func(MediaFallback)
	once       sync.Once
	payload    []byte
	dispatched atomic.Bool
}

// WithMediaFallbackDelivery observes the route decision before HTTP commits its headers.
func WithMediaFallbackDelivery(ctx context.Context, onFallback func(MediaFallback)) context.Context {
	if mediaDelivery(ctx) != nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, mediaFallbackDeliveryKey{}, &mediaFallbackDelivery{
		ready:      make(chan struct{}),
		onFallback: onFallback,
	})
}

func mediaDelivery(ctx context.Context) *mediaFallbackDelivery {
	if ctx == nil {
		return nil
	}
	delivery, _ := ctx.Value(mediaFallbackDeliveryKey{}).(*mediaFallbackDelivery)
	return delivery
}

func MediaResponseReady(ctx context.Context) <-chan struct{} {
	if delivery := mediaDelivery(ctx); delivery != nil {
		return delivery.ready
	}
	return nil
}

func PublishMediaFallback(ctx context.Context, info MediaFallback) {
	if delivery := mediaDelivery(ctx); delivery != nil {
		delivery.once.Do(func() {
			delivery.payload, _ = Body(ctx)
			if delivery.onFallback != nil {
				delivery.onFallback(info)
			}
			delivery.dispatched.Store(true)
			close(delivery.ready)
		})
	}
}

func PublishWebSessionStarted(ctx context.Context) {
	if delivery := mediaDelivery(ctx); delivery != nil {
		delivery.once.Do(func() { close(delivery.ready) })
	}
}

func MediaFallbackDispatched(ctx context.Context) bool {
	delivery := mediaDelivery(ctx)
	return delivery != nil && delivery.dispatched.Load()
}

func MediaFallbackPayload(ctx context.Context) []byte {
	if delivery := mediaDelivery(ctx); delivery != nil && delivery.dispatched.Load() {
		return delivery.payload
	}
	return nil
}

func WithMediaFallbackValidator(ctx context.Context, validate func([]byte) error) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, mediaFallbackValidatorKey{}, validate)
}

func ValidateMediaFallback(ctx context.Context, payload []byte) error {
	if ctx == nil {
		return nil
	}
	if validate, ok := ctx.Value(mediaFallbackValidatorKey{}).(func([]byte) error); ok && validate != nil {
		return validate(payload)
	}
	return nil
}
