package auth

import (
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

const (
	enterpriseQuotaStreakCount     = 10
	enterprisePaidHighUsagePercent = 95
	enterpriseQuotaProbeSpacing    = 30 * time.Second
	enterpriseRateLimitedKind      = "rate_limited"
	enterpriseHighUsageKind        = "high_usage"
	enterpriseExhaustedKind        = "quota_exhausted"
)

func enterpriseLocalProtectionKind(kind string) bool {
	return kind == enterpriseRateLimitedKind || kind == enterpriseHighUsageKind || kind == enterpriseExhaustedKind
}

func enterpriseObservedPause(kind string) bool {
	return kind == enterpriseHighUsageKind || kind == enterpriseExhaustedKind
}

func (s *SharedQuotaState) durableProtection() bool {
	return enterpriseLocalProtectionKind(s.Restriction.Kind) || s.Restriction.Kind == enterpriseNearLimitKind ||
		s.PaidRecovery && s.Restriction.Kind == "daily" && s.Restriction.Scope == "edition"
}

func (g *enterpriseQuotaGate) matchesProtectedPool(s *SharedQuotaState) bool {
	return g.matchesPaidQuotaKey(s.Key)
}

func (g *enterpriseQuotaGate) matchesPaidQuotaKey(key string) bool {
	return g.identity != nil && !g.identity.Trial && (g.identity.Paid || g.restoredProtectionPaid) &&
		g.keys()["edition"] == key
}

func (c *enterpriseQuotaCoordinator) recordRateLimit(s *SharedQuotaState, now, retryAt time.Time) bool {
	day := enterpriseQuotaDayStart(now)
	streak := c.streaks[s.PoolID]
	if !streak.Day.Equal(day) {
		streak = enterpriseQuotaStreak{Day: day}
	}
	streak.Count++
	streak.RetryAt = maxTime(streak.RetryAt, retryAt).UTC()
	if streak.Count < enterpriseQuotaStreakCount {
		if c.streaks == nil {
			c.streaks = make(map[string]enterpriseQuotaStreak)
		}
		c.streaks[s.PoolID] = streak
		return false
	}
	retryAt = maxTime(s.Restriction.RetryAt, maxTime(now.Add(enterpriseQuotaProbeSpacing), streak.RetryAt))
	s.Phase, s.PaidRecovery = "cooling", true
	s.Restriction = ge.Restriction{
		Kind: enterpriseRateLimitedKind, Scope: "edition", Feature: "assistant_search",
		RetryAt: retryAt.UTC(), TimeSource: "local_quota_pressure",
	}
	s.LastRejectedAt, s.ProtectionStartedAt = now.UTC(), now.UTC()
	s.Version++
	delete(c.streaks, s.PoolID)
	s.rejections = nil
	return true
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (p *enterpriseBalancePool) currentPaidObservation(now time.Time) bool {
	return p != nil && p.paid.pool != nil && now.Before(*p.paid.snapshot.ResetAt) &&
		!p.paid.snapshot.ObservedAt.After(now) && now.Sub(p.paid.snapshot.ObservedAt) < EnterpriseQuotaObservationTTL
}

func (p *enterpriseBalancePool) usedAtLeast(now time.Time, percent int64) bool {
	if !p.currentPaidObservation(now) {
		return false
	}
	limit, used := *p.paid.pool.Limit, *p.paid.pool.Used
	// ceil(percent * limit / 100), without multiplying the full limit.
	threshold := (limit/100)*percent + (limit%100*percent+99)/100
	return limit > 0 && used >= threshold
}

func (g *enterpriseQuotaGate) protectObservedQuotaLocked(s *SharedQuotaState, p *enterpriseBalancePool, now time.Time) bool {
	if !p.currentPaidObservation(now) {
		return false
	}
	reading := p.paid
	if !s.ProtectionUsageAt.IsZero() && reading.pool.UsageUpdatedAt.Before(s.ProtectionUsageAt) {
		return false
	}
	if enterpriseObservedPause(s.Restriction.Kind) && *reading.pool.Remaining > 0 && !p.usedAtLeast(now, enterprisePaidHighUsagePercent) &&
		reading.snapshot.ObservedAt.After(s.ProtectionObservedAt) {
		// Release a verified observation-based pause through the same single
		// lease, retaining any outstanding upstream Retry-After.
		readyAt := maxTime(now, s.ProtectionRetryAt)
		if !now.Before(s.Restriction.ResetAt) {
			readyAt = maxTime(readyAt, enterprisePaidDailyRecoveryAt(s.Restriction.ResetAt, s.PoolID))
		}
		s.Restriction.Kind = enterpriseRateLimitedKind
		s.Restriction.ResetAt = time.Time{}
		s.Restriction.RetryAt = readyAt.UTC()
		s.Restriction.TimeSource = "local_quota_pressure"
		s.ProtectionStartedAt = reading.snapshot.ObservedAt.UTC()
		s.Phase = "cooling"
		s.Version++
		return true
	}
	kind := ""
	switch {
	case *reading.pool.Remaining == 0:
		kind = enterpriseExhaustedKind
	case s.Restriction.Kind == enterpriseRateLimitedKind && p.usedAtLeast(now, enterprisePaidHighUsagePercent):
		kind = enterpriseHighUsageKind
	default:
		return false
	}
	if s.Phase != "normal" && !enterpriseLocalProtectionKind(s.Restriction.Kind) && s.Restriction.Kind != enterpriseNearLimitKind {
		// Explicit upstream restrictions retain their scope and recovery rules.
		return false
	}
	reset := reading.snapshot.ResetAt.UTC()
	if s.Restriction.Kind == kind && s.Restriction.ResetAt.Equal(reset) {
		return false
	}
	if !enterpriseObservedPause(s.Restriction.Kind) {
		s.ProtectionRetryAt = s.Restriction.RetryAt
	}
	s.Restriction = ge.Restriction{
		Kind: kind, Scope: "edition", Feature: "assistant_search", Edition: g.identity.Edition,
		RetryAt: maxTime(enterprisePaidDailyRecoveryAt(reset, s.PoolID), s.ProtectionRetryAt).UTC(),
		ResetAt: reset, TimeSource: "quota_observation",
	}
	s.Phase, s.PaidRecovery = "cooling", true
	s.ProtectionObservedAt, s.ProtectionUsageAt = reading.snapshot.ObservedAt.UTC(), reading.pool.UsageUpdatedAt.UTC()
	s.Version++
	delete(g.manager.enterpriseQuota.streaks, s.PoolID)
	s.rejections = nil
	return true
}

func (g *enterpriseQuotaGate) rateLimitedPoolReadyLocked(s *SharedQuotaState, now time.Time) bool {
	p := g.manager.enterpriseQuota.balances[s.Key]
	return (p.currentPaidObservation(now) && !p.paid.snapshot.ObservedAt.Before(s.ProtectionStartedAt) &&
		*p.paid.pool.Limit > 0 && !p.usedAtLeast(now, enterprisePaidHighUsagePercent)) ||
		(!s.ProtectionStartedAt.IsZero() && s.ProtectionStartedAt.Before(enterpriseQuotaDayStart(now)))
}

func (t *enterpriseQuotaTicket) retainProtectionAfterProbeLocked(s *SharedQuotaState, err error, upstream *ge.GenerationError, now time.Time, ownerCurrent bool) bool {
	if !enterpriseLocalProtectionKind(s.Restriction.Kind) {
		return false
	}
	if err == nil && ownerCurrent {
		return false
	}
	delay := maxTime(now, t.admittedAt.Add(enterpriseQuotaProbeSpacing))
	if err != nil && upstream != nil && upstream.Restriction == nil && enterpriseProbeGoogleRateLimited(upstream) {
		delay = maxTime(now.Add(enterpriseQuotaProbeSpacing), upstream.RetryAt)
		s.LastRejectedAt = now.UTC()
		t.sharedRateLimit = enterpriseUnscopedRateLimit(upstream)
	}
	if enterpriseObservedPause(s.Restriction.Kind) {
		// After the documented daily reset, allow a delayed observation or a
		// single probe to settle the new quota day without opening the pool.
		delay = maxTime(delay, now.Add(enterpriseQuotaProbeSpacing))
	}
	s.Phase = "cooling"
	s.Restriction.RetryAt = maxTime(s.Restriction.RetryAt, delay).UTC()
	return true
}

func (g *enterpriseQuotaGate) deferRateLimited(now time.Time) bool {
	if g == nil || !g.enabled() {
		return false
	}
	c := &g.manager.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	g = c.bindings[g.authID]
	if g == nil || !g.enabled() {
		return false
	}
	s := c.pools[quotaDigest(g.keys()["edition"])]
	return s != nil && s.Restriction.Kind == enterpriseRateLimitedKind && g.matchesProtectedPool(s) && !g.rateLimitedPoolReadyLocked(s, now)
}

func preferHealthyEnterprisePools(available map[int][]*Auth, now time.Time) map[int][]*Auth {
	var limited, healthy int
	for _, candidates := range available {
		for _, candidate := range candidates {
			if candidate.enterpriseQuota.deferRateLimited(now) {
				limited++
			} else {
				healthy++
			}
		}
	}
	if limited == 0 || healthy == 0 {
		return available
	}
	filtered := make(map[int][]*Auth, len(available))
	for priority, candidates := range available {
		for _, candidate := range candidates {
			if !candidate.enterpriseQuota.deferRateLimited(now) {
				filtered[priority] = append(filtered[priority], candidate)
			}
		}
	}
	if len(filtered) == 0 {
		// A concurrent transition made all candidates rate-limited.
		return available
	}
	return filtered
}
