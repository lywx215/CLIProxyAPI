package enterpriseweb

import (
	"context"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func ValidateHistoryMode(ctx context.Context, body []byte) error {
	lease := ExistingLease(ctx)
	if lease != nil && lease.HistoryMode() == config.EnterpriseWebSerializedContext {
		return nil
	}
	count := ContentCount(body)
	if count == 0 {
		return InputFailure(400, "invalid_history", "contents", "Media requests require a valid contents array")
	}
	if count != 1 {
		return InputFailure(409, "history_mode_disabled", "contents", "Multi-turn media requires serialized-context history mode; enable it in the Web configuration panel")
	}
	return nil
}

// Only final pre-session rejections are counted. A successful fallback and
// failures after session creation are not preflight refusals.
func (r *Runtime) RecordPreflight(ctx context.Context, err error) {
	lease := ExistingLease(ctx)
	if err == nil || lease == nil || lease.sessionStarted.Load() {
		return
	}
	lease.rejectionOnce.Do(func() {
		reason := "unavailable"
		var failure *Failure
		if errors.As(err, &failure) {
			reason = failure.Reason
			if reason == "" {
				switch failure.Code {
				case 400:
					reason = "invalid_input"
				case 401, 403:
					reason = "authentication_or_policy"
				case 409:
					reason = "credential_changed"
				case 413:
					reason = "input_limit_exceeded"
				case 429:
					reason = "capacity_exhausted"
				case 499:
					reason = "cancelled"
				default:
					reason = "unavailable"
				}
			}
		}
		if errors.Is(err, context.Canceled) {
			reason = "cancelled"
		}
		reason = preflightReason(reason)
		r.mu.Lock()
		r.incrementPreflightLocked(reason)
		r.mu.Unlock()
		observePreflight(ctx, reason, err)
	})
}

func (r *Runtime) incrementPreflightLocked(reason string) {
	if r.preflightRejections == nil {
		r.preflightRejections = map[string]uint64{}
	}
	r.preflightRejections[preflightReason(reason)]++
}

// Only positively identified media may contribute Web rejection counters before
// admission. Unclassified or text-only bodies must not be counted as Web work.
func (r *Runtime) recordPreAdmissionFailure(ctx context.Context, reason string, err error) {
	r.mu.Lock()
	r.incrementPreflightLocked(reason)
	r.mu.Unlock()
	observePreflight(ctx, reason, err)
}

func preflightReason(reason string) string {
	switch reason {
	case "history_mode_disabled", "invalid_history", "unsupported_history_content", "history_limit_exceeded", "invalid_input", "authentication_or_policy", "credential_changed", "input_limit_exceeded", "capacity_exhausted", "channel_disabled", "cancelled":
		return reason
	default:
		return "unavailable"
	}
}
