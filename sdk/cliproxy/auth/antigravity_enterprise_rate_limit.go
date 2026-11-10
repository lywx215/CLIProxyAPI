package auth

import (
	"context"
	"math"
	"slices"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
)

const aeRetryTimeTolerance = 5 * time.Second
const aeRateLimitHistoryLimit = 64

// Matched observations are retained for deduplication but cannot confirm another
// window. This prevents successive near-matches from gradually moving a deadline.
type AntigravityEnterpriseRateLimitObservation struct {
	AttemptID string    `json:"attempt_id"`
	RetryAt   time.Time `json:"retry_at"`
	Matched   bool      `json:"matched,omitempty"`
}

type AntigravityEnterpriseRateLimitState struct {
	AuthID       string                                      `json:"auth_id"`
	Fingerprint  string                                      `json:"fingerprint"`
	RetryAt      time.Time                                   `json:"retry_at,omitempty"`
	Observations []AntigravityEnterpriseRateLimitObservation `json:"observations,omitempty"`
}

func (c *antigravityEnterpriseCoordinator) pruneRateLimits(now time.Time) {
	for id, state := range c.rateLimits {
		state.Observations = slices.DeleteFunc(state.Observations, func(observation AntigravityEnterpriseRateLimitObservation) bool {
			return !observation.RetryAt.After(now)
		})
		if !state.RetryAt.After(now) {
			state.RetryAt = time.Time{}
		}
		if state.RetryAt.IsZero() && len(state.Observations) == 0 {
			delete(c.rateLimits, id)
		}
	}
}

func (c *antigravityEnterpriseCoordinator) rateLimitDeadline(id, fingerprint string, now time.Time) time.Time {
	state := c.rateLimits[id]
	if state != nil && state.Fingerprint == fingerprint && state.RetryAt.After(now) {
		return state.RetryAt
	}
	return time.Time{}
}

// The caller holds m.mu while reconciling or removing a binding.
func (m *Manager) clearStaleAntigravityEnterpriseRateLimitLocked(id, fingerprint string) {
	c := &m.antigravityEnterprise
	c.mu.Lock()
	defer c.mu.Unlock()
	if state := c.rateLimits[id]; state != nil && state.Fingerprint != fingerprint {
		delete(c.rateLimits, id)
		c.version++
	}
}

// ObserveAntigravityEnterpriseRateLimit accepts one real upstream attempt. Only
// explicit upstream retry hints can corroborate a binding-wide restriction.
func (m *Manager) ObserveAntigravityEnterpriseRateLimit(source *Auth, attemptID string, failure *ae.Error) time.Time {
	if m == nil || failure == nil || failure.HTTP != 429 || !failure.Attempted || !failure.RetryHint || attemptID == "" || len(attemptID) > 128 {
		return time.Time{}
	}
	m.mu.RLock()
	if !m.currentAEQuotaSourceLocked(source) {
		m.mu.RUnlock()
		return time.Time{}
	}
	c := &m.antigravityEnterprise
	c.mu.Lock()
	now := c.currentTime()
	c.pruneRateLimits(now)
	if !failure.RetryAt.After(now) {
		c.mu.Unlock()
		m.mu.RUnlock()
		return time.Time{}
	}
	if c.rateLimits == nil {
		c.rateLimits = make(map[string]*AntigravityEnterpriseRateLimitState)
	}
	fingerprint := aeBindingFingerprint(source)
	state := c.rateLimits[source.ID]
	if state == nil || state.Fingerprint != fingerprint {
		state = &AntigravityEnterpriseRateLimitState{AuthID: source.ID, Fingerprint: fingerprint}
		c.rateLimits[source.ID] = state
	}
	for _, observation := range state.Observations {
		if observation.AttemptID == attemptID {
			deadline := state.RetryAt
			c.mu.Unlock()
			m.mu.RUnlock()
			return deadline
		}
	}
	incoming := AntigravityEnterpriseRateLimitObservation{AttemptID: attemptID, RetryAt: failure.RetryAt.UTC()}
	for i := range state.Observations {
		prior := &state.Observations[i]
		difference := incoming.RetryAt.Sub(prior.RetryAt)
		if prior.Matched || difference < -aeRetryTimeTolerance || difference > aeRetryTimeTolerance {
			continue
		}
		prior.Matched, incoming.Matched = true, true
		deadline := incoming.RetryAt
		if prior.RetryAt.After(deadline) {
			deadline = prior.RetryAt
		}
		if deadline.After(state.RetryAt) {
			state.RetryAt = deadline
		}
		break
	}
	state.Observations = append(state.Observations, incoming)
	if len(state.Observations) > aeRateLimitHistoryLimit {
		state.Observations = slices.Clone(state.Observations[len(state.Observations)-aeRateLimitHistoryLimit:])
	}
	deadline := state.RetryAt
	c.version++
	c.mu.Unlock()
	m.mu.RUnlock()
	m.publishAntigravityEnterpriseQuota()
	m.persistCooldownStates(context.Background())
	return deadline
}

func aeRateLimitView(deadline, now time.Time) CooldownView {
	return CooldownView{
		Scope: "credential", RestrictionScope: "binding", Reason: "binding_rate_limit",
		HTTPStatus: 429, TimeSource: "google", RetryAt: deadline.UTC(),
		RemainingSeconds: int64(math.Ceil(deadline.Sub(now).Seconds())),
	}
}
