package enterpriseweb

import (
	"context"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	webgemini "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/gemini"
	log "github.com/sirupsen/logrus"
)

type FallbackReason string

const (
	FallbackWebDisabled  FallbackReason = "web_disabled"
	FallbackNoCredential FallbackReason = "no_web_credential"
	MediaFallbackHeader                 = "X-CPA-Media-Fallback"

	channelDisabledMessage = "Media input channel is disabled"
)

type channelDisabledError struct{ *Failure }

func (e *channelDisabledError) Unwrap() error { return e.Failure }

func newChannelDisabledError() error {
	return &channelDisabledError{&Failure{Code: 503, Message: channelDisabledMessage, clientMessage: channelDisabledMessage}}
}

func IsChannelDisabled(err error) bool {
	var failure *channelDisabledError
	return errors.As(err, &failure)
}

// MediaFallback describes the projection without retaining discarded content.
type MediaFallback struct {
	Reason             FallbackReason
	Source             string
	RemovedAttachments int
	RequestBytes       int64
}

func (f MediaFallback) HeaderValue() string { return string(f.Reason) + "/" + f.Source }

type mediaFallbackKey struct{}

func MediaFallbackFrom(ctx context.Context) (MediaFallback, bool) {
	if ctx == nil {
		return MediaFallback{}, false
	}
	fallback, ok := ctx.Value(mediaFallbackKey{}).(MediaFallback)
	return fallback, ok
}

// ProjectMediaFallback is shared by direct SDK requests and late admission
// changes. An already allocated body is included in the projection budget.
func (r *Runtime) ProjectMediaFallback(ctx context.Context, body []byte, reason FallbackReason) (context.Context, []byte, error) {
	settings := r.admissionConfig(ctx)
	rawBytes := int64(len(body))
	if rawBytes > settings.InputLimits.MaxRequestBytes {
		return ctx, nil, InputFailureFrom(contract.LimitFailure("body", "Media request exceeds its configured byte limit", contract.InputLimit{
			Name: contract.LimitRequestBytes, Observed: rawBytes, Maximum: settings.InputLimits.MaxRequestBytes,
		}))
	}
	lease := ExistingLease(ctx)
	if lease.SessionStarted() {
		return ctx, nil, Fail(503, "Media fallback is unavailable after Web session creation")
	}
	reserve := r.reserveProjection(ctx)
	if lease == nil {
		release, err := reserve(rawBytes)
		if err != nil {
			return ctx, nil, err
		}
		defer release()
	} else {
		// The prior HTTP/SDK scope still holds the original buffer. Reserve
		// projection scratch from that lease and leave its owner in charge.
		rawBytes = int64(cap(body))
		if original, ok := Body(ctx); ok && len(original) > 0 {
			if len(body) > 0 && &original[0] == &body[0] {
				rawBytes = max(rawBytes, int64(cap(original)))
			} else {
				rawBytes += int64(cap(original))
			}
		}
		used := rawBytes
		reserve = func(size int64) (func(), error) {
			if err := lease.EnsureBudget(ctx, used+size); err != nil {
				return nil, err
			}
			used += size
			return func() { used -= size }, nil
		}
	}
	projected, clean, err := r.projectMediaFallback(ctx, body, reason, reserve)
	if err == nil && lease != nil {
		lease.releaseGenerationSlot(rawBytes)
	}
	return projected, clean, err
}

func (r *Runtime) projectMediaFallback(ctx context.Context, body []byte, reason FallbackReason, reserve func(int64) (func(), error)) (context.Context, []byte, error) {
	limits := r.admissionConfig(ctx).InputLimits
	clean, count, history, err := webgemini.ProjectText(ctx, body, min(limits.MaxRequestBytes, MaxBodyBytes), limits.MaxJSONObjectFields, reserve)
	if err != nil {
		var failure *contract.Failure
		if errors.As(err, &failure) {
			err = InputFailureFrom(failure)
		}
		return ctx, nil, err
	}
	if count == 0 {
		return ctx, nil, InputFailure(400, "invalid_history", "contents", "Inline media must belong to valid content parts")
	}
	source := "current_input"
	if history {
		source = "history_text"
	}
	info := MediaFallback{Reason: reason, Source: source, RemovedAttachments: count, RequestBytes: int64(len(body))}
	if ExistingLease(ctx) != nil {
		ctx = context.WithValue(ctx, leaseKey{}, (*Lease)(nil))
	}
	return context.WithValue(WithBody(ctx, clean), mediaFallbackKey{}, info), clean, nil
}

// reserveProjection charges the shared budget without acquiring a generation
// slot. Each reservation is scoped to the synchronous projection.
func (r *Runtime) reserveProjection(ctx context.Context) func(int64) (func(), error) {
	var ceiling int64
	return func(bytes int64) (func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		if ceiling == 0 {
			ceiling = r.settings.MaxInflightBytes
		}
		if bytes > ceiling-r.used {
			r.rejectedMemory++
			r.mu.Unlock()
			return nil, Fail(429, "Media buffer budget is exhausted")
		}
		r.used += bytes
		r.mu.Unlock()
		return func() {
			r.mu.Lock()
			r.used -= bytes
			r.mu.Unlock()
		}, nil
	}
}

// Release the Web slot while keeping original bytes charged until the admission
// owner releases the lease. Older contexts can still reference the raw body.
func (l *Lease) releaseGenerationSlot(rawBytes int64) {
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.released || !l.generation {
		return
	}
	l.generation = false
	r.active--
	if rawBytes < l.reserved {
		r.used -= l.reserved - rawBytes
		l.reserved = rawBytes
	}
}

// RecordMediaFallback is called once at ordinary Enterprise dispatch.
func (r *Runtime) RecordMediaFallback(ctx context.Context) {
	info, ok := MediaFallbackFrom(ctx)
	if !ok {
		return
	}
	r.mu.Lock()
	switch info.Reason {
	case FallbackWebDisabled:
		r.fallbackDisabled++
	case FallbackNoCredential:
		r.fallbackNoCredential++
	}
	r.mu.Unlock()
	PublishMediaFallback(ctx, info)
	log.WithFields(log.Fields{
		"request_id": logging.GetRequestID(ctx), "provider": "gemini-enterprise",
		"reason": info.Reason, "source": info.Source, "removed_attachments": info.RemovedAttachments,
		"request_bytes": info.RequestBytes,
	}).Info("Web media request fell back to Enterprise text")
}
