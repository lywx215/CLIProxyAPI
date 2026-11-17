package auth

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

var enterpriseRecoveryLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("UTC+8", 8*60*60)
	}
	return location
}()

// enterprisePaidDailyRecoveryAt spreads recovery across 16:02:00–16:05:00
// on the Beijing date of the quota reset. A later upstream reset takes
// precedence, even when it lands after the window.
func enterprisePaidDailyRecoveryAt(resetAt time.Time, poolID string) time.Time {
	if resetAt.IsZero() {
		return resetAt
	}
	year, month, day := resetAt.In(enterpriseRecoveryLocation).Date()
	start := time.Date(year, month, day, 16, 2, 0, 0, enterpriseRecoveryLocation)
	const windowSeconds = 3 * 60
	hash := sha256.Sum256([]byte(poolID))
	offset := binary.BigEndian.Uint64(hash[:8]) % (windowSeconds + 1)
	readyAt := start.Add(time.Duration(offset) * time.Second)
	if resetAt.After(readyAt) {
		return resetAt
	}
	return readyAt
}

func (s *SharedQuotaState) dailyResetAnchor() time.Time {
	if !s.Restriction.ResetAt.IsZero() {
		return s.Restriction.ResetAt
	}
	if !s.LastRejectedAt.IsZero() && s.LastRejectedAt.Before(s.Restriction.RetryAt) {
		return ge.NextPacificMidnight(s.LastRejectedAt)
	}
	return s.Restriction.RetryAt
}

func (s *SharedQuotaState) canAlignLegacyDailyRecovery() bool {
	r := s.Restriction
	return s.probe == 0 &&
		(s.Phase == "cooling" || s.Phase == "probe_pending" && s.ResetEpoch == 0) &&
		r.Kind == "daily" && r.Scope == "edition" && r.Feature == "assistant_search" &&
		(r.TimeSource == "google" || r.TimeSource == "estimated_pacific_midnight")
}

// legacyProbeReadyAt runs with the coordinator lock held. A trial credential
// can use the old deadline only when the entire shared pool is verified trial.
func (g *enterpriseQuotaGate) legacyProbeReadyAt(s *SharedQuotaState, now time.Time) time.Time {
	if !s.legacyReadyAt.After(now) {
		return time.Time{}
	}
	if s.PaidRecovery {
		return s.legacyReadyAt
	}
	hasMembers := false
	for _, member := range g.manager.enterpriseQuota.bindings {
		if member.keys()["edition"] != s.Key {
			continue
		}
		// A disabled credential still belongs to its quota pool.
		hasMembers = true
		i := member.identity
		if !i.Trial || i.Paid || i.CheckedAt.IsZero() || i.CheckedAt.After(now) ||
			now.Sub(i.CheckedAt) >= enterpriseSubscriptionMaxStale {
			return s.legacyReadyAt
		}
	}
	if !hasMembers {
		return s.legacyReadyAt
	}
	return time.Time{}
}

// alignLegacyPaidDailyRecoveryLocked runs with the coordinator lock held. The
// saved rejection belongs to its original quota day, even after a late restore.
func (c *enterpriseQuotaCoordinator) alignLegacyPaidDailyRecoveryLocked(g *enterpriseQuotaGate, now time.Time) bool {
	if g == nil || g.policy == nil || g.policy.Disabled || g.policy.Status == StatusDisabled || !g.enabled() {
		return false
	}
	key := g.paidQuotaKey(now)
	if key == "" {
		return false
	}
	s := c.pools[quotaDigest(key)]
	if s == nil || !s.canAlignLegacyDailyRecovery() {
		return false
	}
	resetAt := s.dailyResetAnchor()
	readyAt := enterprisePaidDailyRecoveryAt(resetAt, s.PoolID)
	if !readyAt.After(now) || !readyAt.After(s.Restriction.RetryAt) {
		return false
	}
	s.Restriction.ResetAt = resetAt.UTC()
	s.Restriction.RetryAt = readyAt.UTC()
	s.Phase = "cooling"
	s.PaidRecovery = true
	s.legacyReadyAt = time.Time{}
	s.Version++
	return true
}

func (g *enterpriseQuotaGate) paidDailyRestriction(r ge.Restriction, s *SharedQuotaState, now, admittedAt time.Time, probe bool) ge.Restriction {
	if r.Kind != "daily" || r.Scope != "edition" || r.Feature != "assistant_search" ||
		(g.paidQuotaKey(now) != s.Key && !(probe && s.probePaid)) {
		return r
	}
	deadline := r.RetryAt
	if r.TimeSource == "estimated_pacific_midnight" {
		deadline = time.Time{}
	}
	var readyAt time.Time
	switch {
	case !r.ResetAt.IsZero():
		readyAt = enterprisePaidDailyRecoveryAt(r.ResetAt, s.PoolID)
	case probe && s.Restriction.Kind == "daily" && !now.Before(s.Restriction.RetryAt):
		// Google may briefly keep returning yesterday's quota error after
		// reset. Retry verification without waiting for another whole day.
		r.ResetAt = s.dailyResetAnchor()
		readyAt = now.Add(enterpriseQuotaPressureWindow)
		if resetReadyAt := enterprisePaidDailyRecoveryAt(r.ResetAt, s.PoolID); resetReadyAt.After(readyAt) {
			readyAt = resetReadyAt
		}
	default:
		// Attribute an in-flight rejection to the dispatch's quota day, so
		// a late result across midnight does not pause a fresh day until tomorrow.
		r.ResetAt = ge.NextPacificMidnight(admittedAt)
		readyAt = enterprisePaidDailyRecoveryAt(r.ResetAt, s.PoolID)
		if !now.Before(readyAt) {
			readyAt = now.Add(enterpriseQuotaPressureWindow)
		}
	}
	if deadline.After(readyAt) {
		readyAt = deadline
	}
	r.RetryAt = readyAt.UTC()
	return r
}

func (g *enterpriseQuotaGate) isPaidQuotaRecovery(s *SharedQuotaState, now time.Time) bool {
	return s.PaidRecovery || s.Restriction.Kind == enterpriseNearLimitKind ||
		(s.Restriction.Kind == "daily" && s.Restriction.Scope == "edition" && g.paidQuotaKey(now) == s.Key)
}

// The recovery lease supplies the bound when the separate routing estimate
// still shows no free capacity. Ordinary requests still obey the estimate.
func (g *enterpriseQuotaGate) hasPendingPaidQuotaProbeLocked(now time.Time) bool {
	s := g.quotaPressureStateLocked(now)
	return s != nil && g.isPaidQuotaRecovery(s, now) && quotaPhase(s, now) == "probe_pending"
}
