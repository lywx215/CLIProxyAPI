package enterpriseweb

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type admissionConfigKey struct{}

func (r *Runtime) admissionConfig(ctx context.Context) config.GeminiEnterpriseWebConfig {
	if lease := ExistingLease(ctx); lease != nil {
		return lease.config
	}
	if snapshot, ok := ctx.Value(admissionConfigKey{}).(config.GeminiEnterpriseWebConfig); ok {
		return snapshot
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

func (l *Lease) InputLimits() config.GeminiEnterpriseWebInputLimits {
	return l.config.InputLimits
}

// EnsureBudget reserves additional validated history headroom from the same
// provider-wide budget. It never revokes an existing request's reservation.
func (l *Lease) EnsureBudget(ctx context.Context, required int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := l.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.released {
		return Fail(503, "Media resource lease is closed")
	}
	if required <= l.reserved {
		return nil
	}
	additional := required - l.reserved
	// Accepted requests keep their admission ceiling across hot-reload. Lower
	// limits constrain new leases, without revoking an older history reservation.
	// Growth is still charged against all live requests under the same lock.
	ceiling := l.config.MaxInflightBytes
	if additional > ceiling-r.used {
		r.rejectedMemory++
		return Fail(429, "Media buffer budget is exhausted")
	}
	r.used += additional
	l.reserved = required
	return nil
}
