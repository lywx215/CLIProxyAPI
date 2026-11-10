package enterpriseweb

import (
	"errors"
	"net/http"
)

// PreflightFailure allows another Web credential to be selected before any
// session or upload exists. Its wrapped error retains the pinned response.
type PreflightFailure struct {
	error
	SearchModeMismatch bool
}

func (e *PreflightFailure) Unwrap() error { return e.error }

// IsCookieLoginRequired distinguishes a confirmed login rejection from a
// transport, application authorization or rate limit failure.
func IsCookieLoginRequired(err error) bool {
	var loginErr *loginRequiredError
	return errors.As(err, &loginErr)
}

func AuthenticationPreflightFailure(err error) error {
	var failure *PreflightFailure
	if errors.As(err, &failure) {
		return err
	}
	switch status := failureStatus(err); {
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusTooManyRequests, status >= 500:
		return &PreflightFailure{error: err}
	default:
		return err
	}
}
