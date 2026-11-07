// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package contract

import (
	"context"
	"sync"
)

type deliveryKey struct{}
type delivery struct {
	mu        sync.Mutex
	closed    bool
	callbacks []func(bool)
}

// WithDelivery postpones continuation admission until the complete native HTTP
// response has been written. A parser/transport success alone is insufficient.
func WithDelivery(ctx context.Context) (context.Context, func(bool)) {
	d := &delivery{}
	return context.WithValue(ctx, deliveryKey{}, d), func(ok bool) {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return
		}
		d.closed = true
		callbacks := d.callbacks
		d.callbacks = nil
		d.mu.Unlock()
		for _, callback := range callbacks {
			callback(ok && ctx.Err() == nil)
		}
	}
}

// OnDelivery registers one bounded metadata commit. Standalone generators
// without the HTTP delivery contract cannot create resumable conversations.
func OnDelivery(ctx context.Context, callback func(bool)) bool {
	d, ok := ctx.Value(deliveryKey{}).(*delivery)
	if !ok {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	d.callbacks = append(d.callbacks, callback)
	return true
}
