package handlers

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
)

type throttleObservation struct {
	ctx     context.Context
	started time.Time
	data    diagnostics.ThrottleData
}

// ObserveRequestThrottle attaches to the already-selected per-request values.
// The existing throttler has no config revision, so it reports unversioned.
func ObserveRequestThrottle(ctx context.Context, t *RequestThrottler) func() {
	if !diagnostics.DebugActive(ctx) {
		return func() {}
	}
	o := &throttleObservation{ctx: ctx, started: time.Now(), data: diagnostics.ThrottleData{Revision: "unversioned", Source: "unknown"}}
	if t != nil {
		rate, delay, zero := t.targetRate, float64(t.ttftDelay)/float64(time.Millisecond), float64(0)
		o.data.Enabled = true
		o.data.Rate = &rate
		o.data.FirstDelay = &delay
		o.data.Planned = &zero
		actual := float64(0)
		o.data.Actual = &actual
		t.diagnostic = o
	}
	return func() {
		elapsed := float64(time.Since(o.started)) / float64(time.Millisecond)
		o.data.Elapsed = &elapsed
		diagnostics.ObserveThrottle(ctx, o.data)
	}
}

func (t *RequestThrottler) observeTokens(tokens int, source string, add bool) {
	if t == nil || t.diagnostic == nil || !diagnostics.DebugActive(t.diagnostic.ctx) {
		return
	}
	d := &t.diagnostic.data
	n := int64(tokens)
	if n < 0 {
		return
	}
	if add && d.Tokens != nil {
		n += *d.Tokens
	}
	d.Tokens = &n
	if source != "" {
		d.Source = source
	}
}

func (t *RequestThrottler) observeWait(ctx context.Context, planned time.Duration) func() {
	if t == nil || t.diagnostic == nil || !diagnostics.DebugActive(t.diagnostic.ctx) {
		return func() {}
	}
	started := time.Now()
	d := &t.diagnostic.data
	*d.Planned += float64(planned) / float64(time.Millisecond)
	return func() {
		*d.Actual += float64(time.Since(started)) / float64(time.Millisecond)
		if ctx.Err() != nil {
			d.Cancelled = true
		}
	}
}

// EstimateObservedNonStreamingTokens uses the same calculation as every other
// caller, retaining the selected branch's provenance without re-estimation.
func EstimateObservedNonStreamingTokens(resp []byte, t *RequestThrottler) int {
	n, source := estimateNonStreamingTokensWithSource(resp)
	if t != nil && t.diagnostic != nil && diagnostics.DebugActive(t.diagnostic.ctx) {
		t.diagnostic.data.Source = source
	}
	return n
}
