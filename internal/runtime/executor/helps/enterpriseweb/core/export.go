package core

import (
	"errors"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

type upstreamError struct{ status int }

func (*upstreamError) Error() string { return "Media upstream request failed" }
func Status(err error) int {
	var f *contract.Failure
	if errors.As(err, &f) {
		return f.Code
	}
	var r *responseError
	if errors.As(err, &r) && r.status > 0 {
		return r.status
	}
	return 502
}

// ParseStream preserves incremental parts and requires a complete successful terminal.
func ParseStream(r io.Reader, emit func(contract.Event) error) error { return decodeStream(r, emit) }

// ParseObservedStream exposes recognized answer states separately from the
// final event, which is emitted only after the complete tail is verified.
func ParseObservedStream(r io.Reader, emit func(contract.Event) error, state func(string)) error {
	return decodeStreamWithState(r, emit, state)
}

type StreamFailureDetails struct {
	Code, GoogleStatus, GoogleReason string
	Rejected                         bool
}

// DescribeStreamFailure projects parser errors without exposing raw response
// bytes, underlying transport errors, or decoder messages.
func DescribeStreamFailure(err error) StreamFailureDetails {
	var rpc *responseError
	var rejected *rejectedStateError
	var problem *streamProblem
	var transport *transportError
	var failure *contract.Failure
	switch {
	case errors.As(err, &rpc):
		return StreamFailureDetails{Code: "upstream_rpc_error", GoogleStatus: knownStatus(rpc.statusEnum), GoogleReason: knownReason(rpc.reason), Rejected: true}
	case errors.As(err, &rejected):
		return StreamFailureDetails{Code: "upstream_answer_rejected", Rejected: true}
	case errors.As(err, &problem):
		return StreamFailureDetails{Code: string(problem.reason)}
	case errors.Is(err, io.ErrUnexpectedEOF):
		return StreamFailureDetails{Code: string(streamTruncatedJSON)}
	case errors.As(err, &transport):
		return StreamFailureDetails{Code: "upstream_stream_read_failed"}
	case errors.As(err, &failure) && failure.Status == "RESOURCE_EXHAUSTED":
		return StreamFailureDetails{Code: "stream_response_limit"}
	default:
		return StreamFailureDetails{Code: "stream_invalid_response"}
	}
}
