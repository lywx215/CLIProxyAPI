package auth

import (
	"context"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

// BeginEnterpriseQuotaRequest keeps per-pool outcomes open across retries. The
// outermost dispatcher releases its hold after deciding whether to retry, and
// a returned stream keeps another hold until its upstream result is known.
func BeginEnterpriseQuotaRequest(ctx context.Context) (context.Context, func()) {
	ctx = WithEnterpriseQuotaRequest(ctx)
	return ctx, enterpriseQuotaRequestFromContext(ctx).hold()
}

type enterpriseQuotaStreak struct {
	Day     time.Time `json:"period_start"`
	Count   int       `json:"count"`
	RetryAt time.Time `json:"retry_at,omitzero"`
}

type enterpriseQuotaOutcome struct {
	gate                *enterpriseQuotaGate
	poolID              string
	version             quotaTicketVersion
	successEpoch        uint64
	admittedAt, retryAt time.Time
	success, pending    bool
}

func (r *enterpriseQuotaRequest) hold() func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.holds++
	r.mu.Unlock()
	return r.release
}

func (r *enterpriseQuotaRequest) release() {
	r.mu.Lock()
	r.holds--
	var ready []enterpriseQuotaOutcome
	if r.holds == 0 {
		for id, o := range r.outcomes {
			if o.pending {
				o.pending = false
				r.outcomes[id] = o
				ready = append(ready, o)
			}
		}
	}
	r.mu.Unlock()
	for _, o := range ready {
		o.settle()
	}
}

func (r *enterpriseQuotaRequest) recordOutcome(o enterpriseQuotaOutcome) {
	r.mu.Lock()
	last, exists := r.outcomes[o.poolID]
	if exists && (last.success || !o.success && !last.pending) {
		r.mu.Unlock()
		return
	}
	if exists && !o.success && last.successEpoch == o.successEpoch {
		o.retryAt = maxTime(o.retryAt, last.retryAt)
	}
	if r.outcomes == nil {
		r.outcomes = make(map[string]enterpriseQuotaOutcome)
	}
	o.pending = !o.success && r.holds > 0
	r.outcomes[o.poolID] = o
	r.mu.Unlock()
	if !o.success && !o.pending {
		o.settle()
	}
}

func (t *enterpriseQuotaTicket) quotaOutcomeLocked(g *enterpriseQuotaGate, current bool, upstream *ge.GenerationError, err error, now time.Time) enterpriseQuotaOutcome {
	if !current || t.request == nil || t.admittedAt.Before(enterpriseQuotaDayStart(now)) {
		return enterpriseQuotaOutcome{}
	}
	if err != nil && !enterpriseUnscopedRateLimit(upstream) || err == nil && (t.emptyGeneration || !enterpriseQuotaTextModel(t.model)) {
		return enterpriseQuotaOutcome{}
	}
	s := g.quotaPressureStateLocked(now)
	if s == nil || s.Phase != "normal" && s.Restriction.Kind != enterpriseNearLimitKind {
		return enterpriseQuotaOutcome{}
	}
	v, ok := t.versions[s.PoolID]
	if !ok || v.reset != s.ResetEpoch || s.Phase == "normal" && v.version != s.Version {
		return enterpriseQuotaOutcome{}
	}
	if err == nil {
		s.successEpoch++
	}
	o := enterpriseQuotaOutcome{gate: g, poolID: s.PoolID, version: v, admittedAt: t.admittedAt, success: err == nil, successEpoch: s.successEpoch}
	if upstream != nil {
		o.retryAt = upstream.RetryAt
	}
	return o
}

func (o enterpriseQuotaOutcome) settle() {
	m := o.gate.manager
	c := &m.enterpriseQuota
	c.mu.Lock()
	now := c.now()
	g := c.bindings[o.gate.authID]
	s := c.pools[o.poolID]
	if m.HomeEnabled() || !o.gate.sameCredential(g) || !g.enabled() || s == nil || g.paidQuotaKey(now) != s.Key ||
		o.admittedAt.Before(enterpriseQuotaDayStart(now)) || s.ResetEpoch != o.version.reset || s.successEpoch != o.successEpoch ||
		(s.Restriction.Kind != enterpriseNearLimitKind && (s.Phase != "normal" || s.Version != o.version.version)) {
		c.mu.Unlock()
		return
	}
	changed := c.recordRateLimit(s, now, o.retryAt)
	if changed {
		g.protectObservedQuotaLocked(s, c.balances[s.Key], now)
	}
	c.mu.Unlock()
	if changed {
		m.publishEnterpriseQuota()
	}
	m.persistEnterpriseQuotaProtection()
}
