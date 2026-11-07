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
