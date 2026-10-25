package geminienterprise

import (
	"encoding/json"
	"net/http"
	"strings"
)

const unspecifiedSkipReason = "ASSIST_SKIPPED_REASON_UNSPECIFIED"

// SkippedError describes an upstream request rejection, never a credential fault.
// Only public enum values survive; policy details and arbitrary strings do not.
type SkippedError struct {
	reasons []string
	started bool
}

// NewSkippedError preserves recognized reasons from an AssistAnswer in SKIPPED state.
func NewSkippedError(raw json.RawMessage, started bool) *SkippedError {
	e := &SkippedError{started: started}
	var values []json.RawMessage
	if len(raw) <= 4096 {
		_ = json.Unmarshal(raw, &values)
	}
	seen := map[string]bool{}
	for _, value := range values {
		reason := ""
		_ = json.Unmarshal(value, &reason)
		switch reason {
		case "NON_ASSIST_SEEKING_QUERY_IGNORED", "CUSTOMER_POLICY_VIOLATION", unspecifiedSkipReason:
		default:
			reason = unspecifiedSkipReason
		}
		if !seen[reason] {
			e.reasons = append(e.reasons, reason)
			seen[reason] = true
		}
	}
	if len(e.reasons) == 0 {
		e.reasons = []string{unspecifiedSkipReason}
	}
	return e
}

func (e *SkippedError) Error() string {
	data, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": "upstream_skipped", "type": "invalid_request_error",
		"message": e.SkipMessage(), "skip_reasons": e.SkipReasons(),
		"retryable": false,
	}})
	return string(data)
}

func (*SkippedError) StatusCode() int           { return http.StatusBadRequest }
func (*SkippedError) IsRequestScoped() bool     { return true }
func (*SkippedError) IsRequestStop() bool       { return true }
func (e *SkippedError) GenerationStarted() bool { return e.started }

// SkipReasons returns a copy of the sanitized Google enum values.
func (e *SkippedError) SkipReasons() []string { return append([]string(nil), e.reasons...) }

// SkipMessage is safe for model APIs, management probes, and ordinary error logs.
func (e *SkippedError) SkipMessage() string {
	details := make([]string, 0, len(e.reasons))
	for _, reason := range e.reasons {
		description := "Google did not provide a recognized skip reason"
		switch reason {
		case "NON_ASSIST_SEEKING_QUERY_IGNORED":
			description = "the query was not classified as seeking assistance"
		case "CUSTOMER_POLICY_VIOLATION":
			description = "the request was rejected by the application's content policy"
		}
		details = append(details, reason+" ("+description+")")
	}
	return "Google skipped generation: " + strings.Join(details, "; ")
}
