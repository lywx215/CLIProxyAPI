package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func enterpriseProbeGoogleRateLimited(upstream *ge.GenerationError) bool {
	return upstream != nil && upstream.HTTPStatus == http.StatusTooManyRequests &&
		(upstream.UpstreamHTTPStatus == http.StatusTooManyRequests ||
			upstream.RPCStatus == "RESOURCE_EXHAUSTED" || upstream.UpstreamCode == http.StatusTooManyRequests)
}

// A recovery request already tests a restricted pool. Even an ambiguous
// upstream 429 prevents another immediate probe, without contributing to the
// evidence that pauses an otherwise healthy pool.
func enterpriseProbeFailureCooldown(err error, upstream *ge.GenerationError, now time.Time) time.Time {
	if err == nil {
		return time.Time{}
	}
	retryAt := now.Add(enterpriseQuotaPressureWindow).UTC()
	if enterpriseProbeGoogleRateLimited(upstream) {
		// A classified restriction owns its own deadline, which may apply to
		// a user or project instead of the edition being verified.
		if upstream.Restriction == nil && upstream.RetryAt.After(retryAt) && enterpriseQuotaTextModel(upstream.Model) {
			return upstream.RetryAt.UTC()
		}
		return retryAt
	}
	if errors.Is(err, context.Canceled) {
		return time.Time{}
	}
	var transport *ge.TransportError
	if errors.Is(err, io.ErrUnexpectedEOF) ||
		upstream != nil && upstream.HTTPStatus >= http.StatusInternalServerError ||
		errors.As(err, &transport) {
		return retryAt
	}
	return time.Time{}
}

// A late probe cannot replace the newer restriction. It may defer recovery
// using a real upstream 429 and hints attributed to the same quota pool.
func enterpriseSupersededProbeRetryAt(upstream *ge.GenerationError, scopedToPool bool, retryAt time.Time) time.Time {
	if !enterpriseProbeGoogleRateLimited(upstream) {
		return time.Time{}
	}
	restriction := upstream.Restriction
	if scopedToPool && restriction != nil && restriction.TimeSource == "google" {
		if upstream.RetryAt.After(retryAt) {
			retryAt = upstream.RetryAt.UTC()
		}
		if restriction.RetryAt.After(retryAt) {
			retryAt = restriction.RetryAt.UTC()
		}
	}
	return retryAt
}
