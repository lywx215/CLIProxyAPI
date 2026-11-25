package auth

import (
	"context"
	"net/http"
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
	pools    sync.Map // One fast-path rejection per logical request and pool.
	mu       sync.Mutex
	holds    int
	outcomes map[string]enterpriseQuotaOutcome
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
	model       string
}

func (s *SharedQuotaState) pruneRejections(now time.Time) {
	start := enterpriseQuotaDayStart(now)
	s.rejections = pruneQuotaRejections(s.rejections, maxTime(now.Add(-enterpriseQuotaPressureWindow), start))
	s.completedRejections = pruneQuotaRejections(s.completedRejections, maxTime(now.Add(-EnterpriseQuotaObservationTTL), start))
}

func pruneQuotaRejections(rejections []enterpriseQuotaRejection, cutoff time.Time) []enterpriseQuotaRejection {
	for len(rejections) > 0 && rejections[0].at.Before(cutoff) {
		rejections = rejections[1:]
	}
	return rejections
}

func appendQuotaRejection(rejections []enterpriseQuotaRejection, rejection enterpriseQuotaRejection) []enterpriseQuotaRejection {
	if len(rejections) == enterpriseQuotaRejectionCount {
		rejections = rejections[1:]
	}
	return append(rejections, rejection)
}

func (s *SharedQuotaState) recordCompletedRejection(rejection enterpriseQuotaRejection) {
	recent := s.completedRejections
	if len(recent) == enterpriseQuotaRejectionCount {
		drop := 0
		if recent[0].model != rejection.model {
			// Retain the only other model until its evidence expires.
			drop = 1
			for _, previous := range recent[1:] {
				if previous.model != rejection.model {
					drop = 0
					break
				}
			}
		}
		copy(recent[drop:], recent[drop+1:])
		recent = recent[:len(recent)-1]
	}
	s.completedRejections = append(recent, rejection)
}

func (s *SharedQuotaState) clearQuotaRejections() {
	s.rejections, s.completedRejections = nil, nil
}

func (s *SharedQuotaState) corroboratedQuotaPressure() bool {
	if len(s.completedRejections) < enterpriseQuotaRejectionCount {
		return false
	}
	// Model cooldowns can spread failures over several minutes. Rejections
	// across models corroborate a fresh paid quota reading over that window.
	for _, rejection := range s.completedRejections[1:] {
		if rejection.model != s.completedRejections[0].model {
			return true
		}
	}
	return false
}

func (g *enterpriseQuotaGate) paidQuotaKey(now time.Time) string {
	i := g.identity
	if i == nil || !i.Paid || i.CheckedAt.IsZero() || i.CheckedAt.After(now) || now.Sub(i.CheckedAt) >= enterpriseSubscriptionMaxStale {
		return ""
	}
	return g.keys()["edition"]
}

func (g *enterpriseQuotaGate) enforcesQuota(s *SharedQuotaState, now time.Time) bool {
	return (g.enabled() || s.UsageCutoff) && g.appliesToQuota(s, now)
}

func (g *enterpriseQuotaGate) appliesToQuota(s *SharedQuotaState, now time.Time) bool {
	if enterpriseLocalProtectionKind(s.Restriction.Kind) {
		return g.matchesProtectedPool(s)
	}
	return s.Restriction.Kind != enterpriseNearLimitKind || g.paidQuotaKey(now) == s.Key ||
		g.restoredProtectionPaid && g.matchesProtectedPool(s)
}

func (p *enterpriseBalancePool) nearDailyLimit(now time.Time) bool {
	return p.usedAtLeast(now, enterprisePaidHighUsagePercent)
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

func enterpriseQuotaPressureHint(upstream *ge.GenerationError) bool {
	return upstream != nil && upstream.QuotaPressureHint && enterpriseQuotaTextModel(upstream.Model)
}

func enterpriseTextQuotaRejection(upstream *ge.GenerationError) bool {
	return enterpriseUnscopedRateLimit(upstream) || upstream != nil && upstream.HTTPStatus == http.StatusTooManyRequests &&
		upstream.RPCStatus == "RESOURCE_EXHAUSTED" && enterpriseQuotaTextModel(upstream.Model)
}

func enterpriseQuotaTextModel(model string) bool {
	return model != "" && model != "gemini-3-pro-image" && model != "veo-3.1" && registry.EnterpriseMediaKind(model) == ""
}

func (t *enterpriseQuotaTicket) recordQuotaPressureLocked(g *enterpriseQuotaGate, upstream *ge.GenerationError, now time.Time) (changed, persist bool) {
	if t.request == nil || t.admittedAt.Before(enterpriseQuotaDayStart(now)) {
		return false, false
	}
	s := g.quotaPressureStateLocked(now)
	if s == nil {
		return false, false
	}
	captured, ok := t.versions[s.PoolID]
	if !ok || s.ResetEpoch != captured.reset {
		return false, false
	}
	unscoped := enterpriseUnscopedRateLimit(upstream)
	pool := g.manager.enterpriseQuota.balances[s.Key]
	// An actual Google exhaustion response warrants a quota read even when
	// its details are insufficient to contribute to shared protection.
	if !unscoped && s.Phase == "normal" && s.Version == captured.version && !pool.currentPaidObservation(now) && !now.Before(s.nextPressureRead) {
		t.refreshQuota = true
		s.nextPressureRead = now.Add(enterpriseQuotaProbeSpacing)
	}
	if !unscoped && !enterpriseQuotaPressureHint(upstream) {
		return false, false
	}
	if s.Restriction.Kind == enterpriseNearLimitKind {
		t.sharedRateLimit = true
		if t.probes[s.PoolID] == 0 && upstream.RetryAt.After(s.Restriction.RetryAt) {
			s.Restriction.RetryAt = upstream.RetryAt.UTC()
			s.Phase = "cooling"
			s.Version++
			return true, true
		}
	}
	if enterpriseLocalProtectionKind(s.Restriction.Kind) {
		t.sharedRateLimit = true
		if t.probes[s.PoolID] == 0 && upstream.RetryAt.After(s.ProtectionRetryAt) {
			// Requests admitted just before the limiting decision may still
			// return a longer upstream delay after it takes effect.
			s.ProtectionRetryAt = upstream.RetryAt.UTC()
			if s.ProtectionRetryAt.After(s.Restriction.RetryAt) {
				s.Restriction.RetryAt = s.ProtectionRetryAt
				s.Phase = "cooling"
				s.Version++
			}
			return true, true
		}
	}
	// An opaque rejection becomes pressure evidence only after retries settle.
	if !unscoped || s.Phase != "normal" || s.Version != captured.version {
		return false, false
	}
	if streak := g.manager.enterpriseQuota.streaks[s.PoolID]; streak.Count == enterpriseQuotaStreakCount-1 && streak.Day.Equal(enterpriseQuotaDayStart(now)) {
		// Retries of the prospective tenth failure must not add a separate
		// model cooldown. Only logical settlement can close the pool.
		t.sharedRateLimit = true
	}
	if _, seen := t.request.pools.LoadOrStore(s.PoolID, struct{}{}); seen {
		return false, false
	}
	s.pruneRejections(now)
	t.refreshQuota = len(s.rejections) == 0 || !pool.currentPaidObservation(now) && !now.Before(s.nextPressureRead)
	if t.refreshQuota {
		s.nextPressureRead = now.Add(enterpriseQuotaProbeSpacing)
	}
	s.rejections = appendQuotaRejection(s.rejections, enterpriseQuotaRejection{at: now, retryAt: upstream.RetryAt})
	if g.pauseNearQuotaLimitLocked(s, now) {
		t.sharedRateLimit = true
		return true, true
	}
	return false, false
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
	s.clearQuotaRejections()
	return true
}

// SetEnterpriseQuotaRefreshHook schedules a metadata read after the first
// rejection in a pressure window. The callback runs outside manager locks.
func (m *Manager) SetEnterpriseQuotaRefreshHook(hook func(*Auth)) {
	c := &m.enterpriseQuota
	c.mu.Lock()
	c.refreshQuota = hook
	c.mu.Unlock()
}
