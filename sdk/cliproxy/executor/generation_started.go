package executor

import "errors"

type generationStartedError struct{ error }

func (e generationStartedError) Unwrap() error         { return e.error }
func (generationStartedError) GenerationStarted() bool { return true }
func (generationStartedError) IsRequestScoped() bool   { return true }
func (e generationStartedError) StatusCode() int {
	var status interface{ StatusCode() int }
	if errors.As(e.error, &status) {
		return status.StatusCode()
	}
	return 502
}

// MarkGenerationStarted prevents configurable credential fallback from replaying a side effect.
func MarkGenerationStarted(err error) error {
	if err == nil || GenerationHasStarted(err) {
		return err
	}
	return generationStartedError{err}
}

func GenerationHasStarted(err error) bool {
	var started interface{ GenerationStarted() bool }
	return errors.As(err, &started) && started.GenerationStarted()
}
