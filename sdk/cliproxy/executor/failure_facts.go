package executor

import (
	"errors"
	"net"
	"time"
)

// FailureFacts is a bounded diagnostic projection, independent of retry policy.
// Causes and upstream bodies deliberately do not belong to this wire type.
type FailureFacts struct {
	Code       string   `json:"code"`
	Stage      string   `json:"stage"`
	Scope      string   `json:"scope"`
	HTTPStatus int      `json:"http_status"`
	SendState  string   `json:"send_state"`
	Generation string   `json:"generation_state"`
	Replay     string   `json:"replay_safety"`
	RetryMS    *float64 `json:"retry_after_ms"`
}

func (f *FailureFacts) Clone() *FailureFacts {
	if f == nil {
		return nil
	}
	c := *f
	if f.RetryMS != nil {
		v := *f.RetryMS
		c.RetryMS = &v
	}
	return &c
}

// ReplayForbidden is a hard constraint, including uncertain upstream acceptance.
func ReplayForbidden(err error) bool {
	var replay interface{ ReplayAllowed() bool }
	return errors.As(err, &replay) && (!replay.ReplayAllowed() || GenerationHasStarted(err))
}

type observedFailure struct {
	cause error
	facts FailureFacts
}

func (e *observedFailure) Error() string               { return e.cause.Error() }
func (e *observedFailure) Unwrap() error               { return e.cause }
func (e *observedFailure) ReplayAllowed() bool         { return e.facts.Replay == "safe" }
func (e *observedFailure) GenerationStarted() bool     { return e.facts.Generation == "started" }
func (e *observedFailure) StatusCode() int             { return e.facts.HTTPStatus }
func (e *observedFailure) FailureFacts() *FailureFacts { return e.facts.Clone() }
func (e *observedFailure) RetryAfter() *time.Duration {
	var r interface{ RetryAfter() *time.Duration }
	if errors.As(e.cause, &r) {
		return r.RetryAfter()
	}
	return nil
}

func WithFailureFacts(err error, facts FailureFacts) error {
	if err == nil {
		return nil
	}
	if facts.HTTPStatus == 0 {
		var s StatusError
		if errors.As(err, &s) {
			facts.HTTPStatus = s.StatusCode()
		} else {
			facts.HTTPStatus = 502
		}
	}
	if facts.RetryMS == nil {
		var r interface{ RetryAfter() *time.Duration }
		if errors.As(err, &r) {
			if d := r.RetryAfter(); d != nil && *d >= 0 {
				ms := float64(*d) / float64(time.Millisecond)
				facts.RetryMS = &ms
			}
		}
	}
	return &observedFailure{cause: err, facts: *facts.Clone()}
}

func FactsFromError(err error) *FailureFacts {
	var f interface{ FailureFacts() *FailureFacts }
	if errors.As(err, &f) {
		return f.FailureFacts().Clone()
	}
	return nil
}

// TransportFailure only declares a send safe when a concrete dial/DNS failure
// proves that the model request could not have been written.
func TransportFailure(err error) error {
	if err == nil {
		return nil
	}
	f := FailureFacts{Code: "upstream_transport", Stage: "transport", Scope: "request", SendState: "possibly_sent", Generation: "unknown", Replay: "unsafe"}
	var dns *net.DNSError
	var op *net.OpError
	if errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial") {
		f.SendState, f.Generation, f.Replay = "not_sent", "not_started", "safe"
	}
	return WithFailureFacts(err, f)
}
