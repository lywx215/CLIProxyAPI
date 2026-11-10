package gemini

import (
	"context"

	appconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
)

type inputLimitsKey struct{}
type historyReservationKey struct{}

// WithInputLimits binds the Web admission snapshot to parsing and attachment decoding.
func WithInputLimits(ctx context.Context, limits appconfig.GeminiEnterpriseWebInputLimits) context.Context {
	return context.WithValue(ctx, inputLimitsKey{}, limits.WithDefaults())
}

func usesWebInputLimits(ctx context.Context) bool { return ctx.Value(inputLimitsKey{}) != nil }

// WithHistoryReservation lets the host reserve validated serialization headroom
// before allocation without reserving the configured text maximum for every file.
func WithHistoryReservation(ctx context.Context, reserve func(int64) error) context.Context {
	return context.WithValue(ctx, historyReservationKey{}, reserve)
}

func reserveHistory(ctx context.Context, size int64) error {
	if reserve, ok := ctx.Value(historyReservationKey{}).(func(int64) error); ok {
		if err := reserve(size); err != nil {
			return err
		}
	}
	_, err := resources.Reserve(ctx, size)
	return err
}

func inputLimits(ctx context.Context) appconfig.GeminiEnterpriseWebInputLimits {
	if limits, ok := ctx.Value(inputLimitsKey{}).(appconfig.GeminiEnterpriseWebInputLimits); ok {
		return limits
	}
	// Other embedders of the Business parser retain their original limits.
	limits := (appconfig.GeminiEnterpriseWebInputLimits{}).WithDefaults()
	limits.MaxTextBytes = maxTextBytes
	limits.MaxAttachments = maxAttachments
	return limits
}
