package auth

import (
	"context"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	enterpriseQuotaPressureWindow = time.Minute
	enterpriseQuotaRejectionCount = 3
	enterpriseNearLimitKind       = "near_limit"
)

type enterpriseQuotaRequestKey struct{}
type enterpriseQuotaRequest struct {
	pools sync.Map
}

// WithEnterpriseQuotaRequest identifies one logical request across dispatch and
// stream bootstrap retries. It never uses a client-supplied request ID.
func WithEnterpriseQuotaRequest(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if enterpriseQuotaRequestFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, enterpriseQuotaRequestKey{}, &enterpriseQuotaRequest{})
}

func enterpriseQuotaRequestFromContext(ctx context.Context) *enterpriseQuotaRequest {
	if ctx == nil {
		return nil
	}
	request, _ := ctx.Value(enterpriseQuotaRequestKey{}).(*enterpriseQuotaRequest)
	return request
}

type enterpriseQuotaRejection struct {
	at, retryAt time.Time
}

func (s *SharedQuotaState) pruneRejections(now time.Time) {
	cutoff := now.Add(-enterpriseQuotaPressureWindow)
	if start := enterpriseQuotaDayStart(now); start.After(cutoff) {
		cutoff = start
	}
	for len(s.rejections) > 0 && s.rejections[0].at.Before(cutoff) {
		s.rejections = s.rejections[1:]
	}
}

func (g *enterpriseQuotaGate) paidQuotaKey(now time.Time) string {
	i := g.identity
	if i == nil || !i.Paid || i.CheckedAt.IsZero() || i.CheckedAt.After(now) || now.Sub(i.CheckedAt) >= enterpriseSubscriptionMaxStale {
		return ""
	}
	return g.keys()["edition"]
}

func (g *enterpriseQuotaGate) appliesToQuota(s *SharedQuotaState, now time.Time) bool {
	return s.Restriction.Kind != enterpriseNearLimitKind || g.paidQuotaKey(now) == s.Key
}

func (p *enterpriseBalancePool) nearDailyLimit(now time.Time) bool {
	if p == nil || !p.paidObservation || p.pool == nil || p.snapshot.ResetAt == nil || !now.Before(*p.snapshot.ResetAt) ||
		p.snapshot.ObservedAt.After(now) || now.Sub(p.snapshot.ObservedAt) >= EnterpriseQuotaObservationTTL {
		return false
	}
	limit, used := *p.pool.Limit, *p.pool.Used
	// ceil(0.99 * limit), calculated without multiplying potentially large values.
	return limit > 0 && used >= limit-limit/100
}

func (g *enterpriseQuotaGate) quotaPressureStateLocked(now time.Time) *SharedQuotaState {
	if key := g.paidQuotaKey(now); key != "" && g.enabled() {
		return g.manager.enterpriseQuota.pools[quotaDigest(key)]
	}
	return nil
}

func enterpriseUnscopedRateLimit(upstream *ge.GenerationError) bool {
	return upstream != nil && upstream.UnscopedRateLimit && enterpriseQuotaTextModel(upstream.Model)
}

func enterpriseQuotaTextModel(model string) bool {
	return model != "" && model != "gemini-3-pro-image" && model != "veo-3.1" && registry.EnterpriseMediaKind(model) == ""
}

func (t *enterpriseQuotaTicket) recordQuotaPressureLocked(g *enterpriseQuotaGate, upstream *ge.GenerationError, now time.Time) bool {
	if t.request == nil {
		return false
	}
	s := g.quotaPressureStateLocked(now)
	if s == nil {
		return false
	}
	captured, ok := t.versions[s.PoolID]
	if !ok || s.ResetEpoch != captured.reset {
		return false
	}
	if s.Phase == "cooling" && s.Restriction.Kind == enterpriseNearLimitKind {
		t.sharedRateLimit = true
	}
	if s.Phase != "normal" || s.Version != captured.version {
		return false
	}
	if _, seen := t.request.pools.LoadOrStore(s.PoolID, struct{}{}); seen {
		return false
	}
	s.pruneRejections(now)
	if len(s.rejections) == enterpriseQuotaRejectionCount {
		s.rejections = s.rejections[1:]
	}
	s.rejections = append(s.rejections, enterpriseQuotaRejection{at: now, retryAt: upstream.RetryAt})
	paused := g.pauseNearQuotaLimitLocked(s, now)
	t.sharedRateLimit = paused
	return paused
}

func (g *enterpriseQuotaGate) pauseNearQuotaLimitLocked(s *SharedQuotaState, now time.Time) bool {
	s.pruneRejections(now)
	pool := g.manager.enterpriseQuota.balances[s.Key]
	if s.Phase != "normal" || len(s.rejections) < enterpriseQuotaRejectionCount || !pool.nearDailyLimit(now) {
		return false
	}
	retryAt := now.Add(enterpriseQuotaPressureWindow)
	for _, rejection := range s.rejections {
		if rejection.retryAt.After(retryAt) {
			retryAt = rejection.retryAt
		}
	}
	s.Version++
	s.Phase = "cooling"
	s.PaidRecovery = true
	s.Restriction = ge.Restriction{Kind: enterpriseNearLimitKind, Scope: "edition", Feature: "assistant_search", Edition: g.identity.Edition, RetryAt: retryAt.UTC(), TimeSource: "local_quota_pressure"}
	s.LastRejectedAt = s.rejections[len(s.rejections)-1].at.UTC()
	s.rejections = nil
	return true
}

func (g *enterpriseQuotaGate) needsQuotaObservationLocked(now time.Time) bool {
	s := g.quotaPressureStateLocked(now)
	if s == nil || s.Phase != "normal" {
		return false
	}
	s.pruneRejections(now)
	return len(s.rejections) > 0 && !g.manager.enterpriseQuota.balances[s.Key].nearDailyLimit(now)
}
