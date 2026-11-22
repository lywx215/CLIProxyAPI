package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// Lock order: Manager.mu -> enterpriseQuota.mu. The coordinator never calls the
// manager, storage or network while locked. Publication and persistence follow unlock.
type enterpriseQuotaCoordinator struct {
	storeVersion     uint64
	protectionMu     sync.Mutex
	protectionPath   string
	mu               sync.Mutex
	pools            map[string]*SharedQuotaState
	bindings         map[string]*enterpriseQuotaGate
	nowFunc          func() time.Time
	nextProbe        uint64
	balances         map[string]*enterpriseBalancePool
	balanceTurn      chan struct{}
	rotationInFlight map[string]int64
	refreshQuota     func(*Auth)
	streaks          map[string]enterpriseQuotaStreak
	reads            map[enterpriseQuotaReadKey]*enterpriseQuotaRead
}

// SharedQuotaState is one authoritative restriction, independent of Auth/ModelState.
// Normal records are retained as fencing tombstones across explicit resets.
type SharedQuotaState struct {
	PoolID         string         `json:"pool_id"`
	Key            string         `json:"key"`
	Version        uint64         `json:"version"`
	ResetEpoch     uint64         `json:"reset_epoch"`
	Phase          string         `json:"phase"`
	Restriction    ge.Restriction `json:"restriction"`
	LastRejectedAt time.Time      `json:"last_rejected_at,omitempty"`
	// PaidRecovery stays set across quota kinds until a current probe succeeds.
	PaidRecovery         bool      `json:"paid_recovery,omitempty"`
	ProtectionStartedAt  time.Time `json:"protection_started_at,omitzero"`
	ProtectionObservedAt time.Time `json:"protection_observed_at,omitzero"`
	ProtectionUsageAt    time.Time `json:"protection_usage_at,omitzero"`
	ProtectionRetryAt    time.Time `json:"protection_retry_at,omitzero"`
	probe                uint64
	probeOwner           *enterpriseQuotaGate
	probePaid            bool
	legacyReadyAt        time.Time
	rejections           []enterpriseQuotaRejection
	successEpoch         uint64 // Fences pending logical rejections received before a successful generation.
	nextPressureRead     time.Time
}

func (s *SharedQuotaState) clearProbe() {
	s.probe = 0
	s.probeOwner = nil
	s.probePaid = false
}

type enterpriseQuotaGate struct {
	manager                *Manager
	authID, fingerprint    string
	epoch, credential      uint64
	identity               *ge.QuotaIdentity
	policy                 *Auth
	restoredProtectionPaid bool
}

type quotaTicketVersion struct{ reset, version uint64 }
type enterpriseQuotaTicket struct {
	gate             *enterpriseQuotaGate
	balance          *enterpriseBalancePool
	versions         map[string]quotaTicketVersion
	probes           map[string]uint64
	request          *enterpriseQuotaRequest
	model            string
	admittedAt       time.Time
	paidRecovery     bool
	sharedRateLimit  bool
	refreshQuota     bool
	emptyGeneration  bool
	finished         sync.Once
	balanceFinished  sync.Once
	rotationFinished sync.Once
}

func quotaDigest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
func enterpriseFingerprint(auth *Auth) string {
	secret := ge.String(auth.Metadata, "refresh_token")
	if secret == "" {
		secret = ge.String(auth.Metadata, "access_token")
	}
	return quotaDigest(auth.ID + "\x00" + ge.Binding(ge.FromMetadata(auth.Metadata)) + "\x00" + secret)
}
func (c *enterpriseQuotaCoordinator) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}
func (g *enterpriseQuotaGate) enabled() bool { return !g.manager.cooldownDisabledForAuth(g.policy) }
func (g *enterpriseQuotaGate) keys() map[string]string {
	base := ge.Provider + "|"
	keys := map[string]string{"credential": base + "credential|" + g.fingerprint + "|assistant_search"}
	i := g.identity
	if i.ValidFor(ge.FromMetadata(g.policy.Metadata)) {
		resource := base + i.ProjectNumber + "|" + i.Location + "|"
		keys["edition"] = resource + i.Edition + "|assistant_search"
		keys["user"] = resource + "user|" + i.PrincipalHash + "|assistant_search"
		keys["project"] = resource + "project|technical_requests"
	}
	return keys
}

// attachEnterpriseQuotaLocked runs only at credential publication boundaries.
func (m *Manager) attachEnterpriseQuotaLocked(auth *Auth) {
	if auth == nil || auth.Provider != ge.Provider {
		return
	}
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bindings == nil {
		c.bindings = map[string]*enterpriseQuotaGate{}
	}
	if c.pools == nil {
		c.pools = map[string]*SharedQuotaState{}
	}
	fingerprint := enterpriseFingerprint(auth)
	identity := auth.EnterpriseQuotaIdentity
	if identity != nil && identity.Binding != ge.Binding(ge.FromMetadata(auth.Metadata)) {
		identity = nil
	}
	old := c.bindings[auth.ID]
	if old != nil && old.fingerprint == fingerprint && old.epoch == auth.RegistrationEpoch && old.credential == auth.CredentialVersion {
		if identity == nil {
			identity = old.identity
		}
	}
	policy := auth.Clone()
	policy.enterpriseQuota = nil
	g := &enterpriseQuotaGate{manager: m, authID: auth.ID, fingerprint: fingerprint, epoch: auth.RegistrationEpoch, credential: auth.CredentialVersion, identity: identity, policy: policy}
	if old != nil && old.restoredProtectionPaid && old.fingerprint == g.fingerprint && old.epoch == g.epoch &&
		old.credential == g.credential && identity != nil && old.identity != nil && *identity == *old.identity {
		g.restoredProtectionPaid = true
	}
	c.replaceProbeOwnerLocked(old, g)
	c.migrateBalanceLocked(old, g)
	auth.EnterpriseQuotaIdentity = identity
	auth.enterpriseQuota = g
	c.bindings[auth.ID] = g
	c.alignLegacyPaidDailyRecoveryLocked(g, c.now())
}

func (c *enterpriseQuotaCoordinator) releaseGateLocked(g *enterpriseQuotaGate) {
	c.replaceProbeOwnerLocked(g, nil)
}

func (c *enterpriseQuotaCoordinator) replaceProbeOwnerLocked(old, next *enterpriseQuotaGate) {
	if old == nil {
		return
	}
	for _, s := range c.pools {
		if s.probeOwner == old && s.Phase == "probing" {
			if old.sameCredential(next) && next.keys()[s.Restriction.Scope] == s.Key && next.appliesToQuota(s, c.now()) {
				// A subscription or metadata refresh can republish the same
				// credential while its recovery request is still in flight.
				s.probeOwner = next
				continue
			}
			if s.probePaid || !s.legacyReadyAt.IsZero() {
				// Keep the slot occupied for paid and still-attributable
				// legacy recovery until the dispatched request ends.
				continue
			}
			s.clearProbe()
			s.Phase = "probe_pending"
			s.Version++
		}
	}
}

// Fence both directions before publishing a Home-mode configuration. A local
// request started before either transition must not mutate the resumed authority.
func (m *Manager) fenceEnterpriseQuotaHomeTransition(previousHome, nextHome bool) {
	if m == nil || previousHome == nextHome {
		return
	}
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.pools {
		s.ResetEpoch++
		s.Version++
		if s.Phase == "probing" {
			s.Phase = "probe_pending"
		}
		s.clearProbe()
		s.rejections = nil
	}
	clear(c.streaks)
}

func (g *enterpriseQuotaGate) sameCredential(current *enterpriseQuotaGate) bool {
	return current != nil && current.epoch == g.epoch && current.credential == g.credential &&
		current.fingerprint == g.fingerprint && current.policy.ProxyURL == g.policy.ProxyURL &&
		!current.policy.Disabled && current.policy.Status != StatusDisabled
}
func quotaPhase(state *SharedQuotaState, now time.Time) string {
	if state.Phase == "cooling" && !state.Restriction.RetryAt.After(now) {
		if state.probe != 0 {
			return "probing"
		}
		return "probe_pending"
	}
	return state.Phase
}

func (c *enterpriseQuotaCoordinator) poolStateLocked(key string) *SharedQuotaState {
	id := quotaDigest(key)
	if s := c.pools[id]; s != nil {
		return s
	}
	s := &SharedQuotaState{PoolID: id, Key: key, Version: 1, Phase: "normal"}
	c.pools[id] = s
	return s
}

func (g *enterpriseQuotaGate) probeOccupancyWaitLocked(s *SharedQuotaState, now time.Time) time.Time {
	if enterpriseLocalProtectionKind(s.Restriction.Kind) {
		if pool := g.manager.enterpriseQuota.balances[s.Key]; pool != nil && pool.liveGeneration > 0 {
			return now.Add(time.Second)
		}
	}
	return time.Time{}
}
func (g *enterpriseQuotaGate) blocked(now time.Time) (bool, time.Time) {
	if g == nil || !g.enabled() {
		return false, time.Time{}
	}
	c := &g.manager.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	g = c.bindings[g.authID]
	if g == nil || !g.enabled() {
		return false, time.Time{}
	}
	var next time.Time
	if blocked, retryAt := g.balanceBlockedLocked(now); blocked {
		next = retryAt
	}
	for _, key := range g.keys() {
		s := c.pools[quotaDigest(key)]
		if s == nil || !g.appliesToQuota(s, now) {
			continue
		}
		switch quotaPhase(s, now) {
		case "cooling":
			if s.Restriction.RetryAt.After(next) {
				next = s.Restriction.RetryAt
			}
		case "probing":
			if t := now.Add(time.Second); t.After(next) {
				next = t
			}
		case "probe_pending":
			if readyAt := g.legacyProbeReadyAt(s, now); readyAt.After(next) {
				next = readyAt
			}
			next = maxTime(next, g.probeOccupancyWaitLocked(s, now))
		}
	}
	return !next.IsZero(), next
}

func (g *enterpriseQuotaGate) begin() (*enterpriseQuotaTicket, error) {
	return g.beginForModel("")
}

func (g *enterpriseQuotaGate) beginForModel(model string) (*enterpriseQuotaTicket, error) {
	media := model != "" && !enterpriseQuotaTextModel(model)
	c := &g.manager.enterpriseQuota
	c.mu.Lock()
	now := c.now()
	if g.manager.HomeEnabled() {
		c.mu.Unlock()
		return nil, &Error{Code: ErrorCodeRequestScoped, Message: "Gemini Enterprise shared quota is unsupported in Home mode", HTTPStatus: 409}
	}
	current := c.bindings[g.authID]
	if !g.sameCredential(current) {
		c.mu.Unlock()
		return nil, &Error{Code: ErrorCodeRequestScoped, Message: "Gemini Enterprise credential changed; retry the request", HTTPStatus: 409}
	}
	// Identity discovery and metadata edits publish a new gate without changing
	// credentials. Admit those snapshots against the latest quota identity/policy.
	g = current
	t := &enterpriseQuotaTicket{gate: g, model: model, admittedAt: now, versions: map[string]quotaTicketVersion{}, probes: map[string]uint64{}}
	keys := g.keys()
	enabled := g.enabled()
	var wait time.Duration
	if blocked, retryAt := g.balanceBlockedLocked(now); blocked {
		wait = retryAt.Sub(now)
	}
	for _, key := range keys {
		s := c.poolStateLocked(key)
		t.versions[s.PoolID] = quotaTicketVersion{s.ResetEpoch, s.Version}
		if !enabled || !g.appliesToQuota(s, now) {
			continue
		}
		switch quotaPhase(s, now) {
		case "cooling":
			if d := s.Restriction.RetryAt.Sub(now); d > wait {
				wait = d
			}
		case "probing":
			if wait < time.Second {
				wait = time.Second
			}
		case "probe_pending":
			if delay := g.legacyProbeReadyAt(s, now).Sub(now); delay > wait {
				wait = delay
			}
			if delay := g.probeOccupancyWaitLocked(s, now).Sub(now); delay > wait {
				wait = delay
			}
			if media && g.isPaidQuotaRecovery(s, now) && wait < time.Second {
				wait = time.Second
			}
		}
	}
	if wait > 0 {
		c.mu.Unlock()
		return nil, &enterpriseLocalGateError{newModelCooldownError("", ge.Provider, wait)}
	}
	if enabled {
		for _, key := range keys {
			s := c.pools[quotaDigest(key)]
			if !g.appliesToQuota(s, now) || quotaPhase(s, now) != "probe_pending" {
				continue
			}
			c.nextProbe++
			s.probe = c.nextProbe
			s.probeOwner = g
			s.probePaid = g.isPaidQuotaRecovery(s, now)
			s.PaidRecovery = s.PaidRecovery || s.probePaid
			s.Phase = "probing"
			s.Version++
			t.probes[s.PoolID] = s.probe
			t.versions[s.PoolID] = quotaTicketVersion{s.ResetEpoch, s.Version}
			t.paidRecovery = t.paidRecovery || s.probePaid
		}
	}
	t.balance = c.balancePoolLocked(g)
	t.balance.inFlight++
	t.balance.liveGeneration++
	t.balance.byAuth[g.authID]++
	c.startRotationRequestLocked(g.authID)
	c.mu.Unlock()
	if len(t.probes) > 0 {
		g.manager.publishEnterpriseQuota()
	}
	return t, nil
}

// finish accepts rejection independently of replay safety (thinking may have started).
// Only a complete successful terminal result holding the current lease clears a pool.
func (t *enterpriseQuotaTicket) finish(err error) {
	if t == nil {
		return
	}
	t.finishQuota(err)
	t.finishBalance(err)
	t.finishRotation()
}

func (t *enterpriseQuotaTicket) finishBalance(err error) {
	t.balanceFinished.Do(func() {
		c := &t.gate.manager.enterpriseQuota
		c.mu.Lock()
		defer c.mu.Unlock()
		// Always release live occupancy, even if a refresh/removal fenced results.
		t.finishBalanceLocked(c.now(), err)
	})
}

func (t *enterpriseQuotaTicket) finishRotation() {
	t.rotationFinished.Do(func() {
		c := &t.gate.manager.enterpriseQuota
		c.mu.Lock()
		defer c.mu.Unlock()
		if p := t.balance.current(); p != nil {
			p.liveGeneration--
		}
		c.finishRotationRequestLocked(t.gate.authID)
	})
}

func (c *enterpriseQuotaCoordinator) startRotationRequestLocked(authID string) {
	if c.rotationInFlight == nil {
		c.rotationInFlight = map[string]int64{}
	}
	c.rotationInFlight[authID]++
}

func (c *enterpriseQuotaCoordinator) finishRotationRequest(authID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishRotationRequestLocked(authID)
}

func (c *enterpriseQuotaCoordinator) finishRotationRequestLocked(authID string) {
	if c.rotationInFlight[authID] > 1 {
		c.rotationInFlight[authID]--
	} else {
		delete(c.rotationInFlight, authID)
	}
}

func (g *enterpriseQuotaGate) beginRotationRequest() (func(), error) {
	c := &g.manager.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.bindings[g.authID]
	if current == nil || current.epoch != g.epoch || current.credential != g.credential ||
		current.fingerprint != g.fingerprint || current.policy.Disabled || current.policy.Status == StatusDisabled ||
		current.policy.Metadata["enterprise_rotation_held"] == true {
		return nil, &Error{Code: ErrorCodeRequestScoped, Message: "Gemini Enterprise application is paused or changed", HTTPStatus: 409}
	}
	c.startRotationRequestLocked(g.authID)
	return sync.OnceFunc(func() { c.finishRotationRequest(g.authID) }), nil
}

// EnterpriseRotationInFlight counts admitted generation requests independently
// of the current quota identity, including requests started before a hold.
func (m *Manager) EnterpriseRotationInFlight(authID string) int64 {
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rotationInFlight[authID]
}

func (t *enterpriseQuotaTicket) finishQuota(err error) {
	if t != nil {
		t.finished.Do(func() { t.finishOnce(err) })
	}
}

func enterpriseGenerationError(err error) *ge.GenerationError {
	var upstream *ge.GenerationError
	_ = errors.As(err, &upstream)
	return upstream
}

func (t *enterpriseQuotaTicket) finishOnce(err error) {
	g := t.gate
	c := &g.manager.enterpriseQuota
	upstream := enterpriseGenerationError(err)
	unscopedRateLimit := enterpriseUnscopedRateLimit(upstream)
	probeRetryAt := enterpriseProbeFailureCooldown(err, upstream, c.now())
	c.mu.Lock()
	now := c.now()
	if !probeRetryAt.IsZero() && probeRetryAt.Before(now.Add(enterpriseQuotaPressureWindow)) {
		probeRetryAt = now.Add(enterpriseQuotaPressureWindow).UTC()
	}
	current := c.bindings[g.authID]
	if g.manager.HomeEnabled() {
		c.mu.Unlock()
		return
	}
	currentCredential := g.sameCredential(current)
	outcome := t.quotaOutcomeLocked(current, currentCredential, upstream, err, now)
	changed, persist, persistProtection := false, false, false
	if outcome.success {
		persistProtection = c.streaks[outcome.poolID].Count > 0
		delete(c.streaks, outcome.poolID)
	}
	restrictionID, deniedID := "", ""
	if upstream != nil && upstream.Restriction != nil {
		r, id := g.restrictionPool(*upstream.Restriction)
		restrictionID = id
		previous := c.pools[id]
		wasProtected := previous != nil && previous.durableProtection()
		if currentCredential && t.applyRestrictionLocked(current, id, r, probeRetryAt, now) {
			deniedID = id
			changed, persist = true, true
			persistProtection = wasProtected || previous.durableProtection()
		}
	} else if currentCredential && unscopedRateLimit {
		changed, persistProtection = t.recordQuotaPressureLocked(current, upstream, now)
	}
	for id, probe := range t.probes {
		s := c.pools[id]
		version := t.versions[id]
		if id == deniedID || s == nil || s.ResetEpoch != version.reset || s.probe != probe {
			continue
		}
		ownerCurrent := currentCredential && s.probeOwner == current
		persistProtection = persistProtection || s.durableProtection()
		paidProbe := s.probePaid
		legacyReadyAt := g.legacyProbeReadyAt(s, now)
		s.clearProbe()
		changed = true
		superseded := s.Version != version.version
		s.Version++
		rateLimitRetryAt := enterpriseSupersededProbeRetryAt(upstream, id == restrictionID, probeRetryAt)
		if superseded || !legacyReadyAt.IsZero() {
			// Only the newer restriction or the legacy schedule can
			// authorize recovery. A real 429 can still delay verification.
			if !superseded {
				s.Phase = "probe_pending"
				persist = persist || s.Restriction.Kind != enterpriseNearLimitKind
			}
			if paidProbe || !legacyReadyAt.IsZero() {
				retryAt := rateLimitRetryAt
				if !retryAt.IsZero() {
					if legacyReadyAt.After(retryAt) {
						retryAt = legacyReadyAt
					}
					s.coolProbeUntil(retryAt, now)
					t.sharedRateLimit = unscopedRateLimit
					persist = persist || s.Restriction.Kind != enterpriseNearLimitKind
				}
			}
			continue
		}
		persist = persist || s.Restriction.Kind != enterpriseNearLimitKind
		if t.retainProtectionAfterProbeLocked(s, err, upstream, now, ownerCurrent) {
			continue
		}
		switch {
		case err == nil && ownerCurrent:
			delete(c.streaks, s.PoolID)
			s.Phase = "normal"
			s.Restriction = ge.Restriction{}
			s.legacyReadyAt = time.Time{}
			s.PaidRecovery = false
			s.ProtectionStartedAt, s.ProtectionObservedAt, s.ProtectionUsageAt, s.ProtectionRetryAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		case paidProbe && !probeRetryAt.IsZero():
			// Failed upstream verification closes the pool again. Returning
			// to probe_pending here would let a burst dispatch serial probes.
			retryAt := probeRetryAt
			if rateLimitRetryAt.After(retryAt) {
				retryAt = rateLimitRetryAt
			}
			lastRejected := time.Time{}
			if enterpriseProbeGoogleRateLimited(upstream) {
				lastRejected = now
				t.sharedRateLimit = unscopedRateLimit
			}
			s.coolProbeUntil(retryAt, lastRejected)
		default:
			s.Phase = "probe_pending"
		}
	}
	refresh := c.refreshQuota
	c.mu.Unlock()
	if t.refreshQuota && refresh != nil {
		if auth, ok := g.manager.GetByID(g.authID); ok && current.sameCredential(auth.enterpriseQuota) &&
			current.paidQuotaKey(now) == auth.enterpriseQuota.paidQuotaKey(now) {
			refresh(auth)
		}
	}
	if outcome.poolID != "" {
		t.request.recordOutcome(outcome)
	}
	if changed {
		g.manager.publishEnterpriseQuota()
	}
	if persist {
		g.manager.persistCooldownStates(context.Background())
	}
	if persistProtection {
		g.manager.persistEnterpriseQuotaProtection()
	}
}

func (g *enterpriseQuotaGate) restrictionPool(r ge.Restriction) (ge.Restriction, string) {
	keys := g.keys()
	key := keys[r.Scope]
	if key == "" || (r.Edition != "" && (g.identity == nil || !g.identity.Verified || r.Edition != g.identity.Edition)) {
		key = keys["credential"]
		r.Scope = "credential"
	}
	return r, quotaDigest(key)
}

func (t *enterpriseQuotaTicket) applyRestrictionLocked(current *enterpriseQuotaGate, id string, r ge.Restriction, probeRetryAt, now time.Time) bool {
	c := &t.gate.manager.enterpriseQuota
	s := c.pools[id]
	captured, ok := t.versions[id]
	if s == nil || !ok || s.ResetEpoch != captured.reset {
		return false
	}
	isProbe := t.probes[id] != 0 && s.probe == t.probes[id]
	if t.gate != current && (!isProbe || s.probeOwner != current) {
		return false
	}
	legacyReadyAt := t.gate.legacyProbeReadyAt(s, now)
	if isProbe && (s.probePaid && s.Version != captured.version || !legacyReadyAt.IsZero()) {
		return false
	}
	r = current.paidDailyRestriction(r, s, now, t.admittedAt, isProbe)
	if isProbe && s.probePaid && probeRetryAt.After(r.RetryAt) {
		r.RetryAt = probeRetryAt
	}
	paidRecovery := s.PaidRecovery || (r.Kind == "daily" && r.Scope == "edition" &&
		r.Feature == "assistant_search" && current.paidQuotaKey(now) == s.Key)
	if enterpriseObservedPause(s.Restriction.Kind) && r.Kind != "daily" {
		s.ProtectionRetryAt = maxTime(s.ProtectionRetryAt, r.RetryAt)
		r = s.Restriction
		r.RetryAt = maxTime(r.RetryAt, s.ProtectionRetryAt)
	}
	// Merge a concurrent batch without progressive backoff or deadline shortening.
	if s.Restriction.RetryAt.After(r.RetryAt) {
		if s.Restriction.Kind == enterpriseNearLimitKind || s.Restriction.Kind == enterpriseRateLimitedKind || r.Kind == "daily" {
			r.RetryAt = s.Restriction.RetryAt
		} else {
			resetAt := r.ResetAt
			r = s.Restriction
			if resetAt.After(r.ResetAt) {
				r.ResetAt = resetAt
			}
		}
	}
	if s.Restriction.ResetAt.After(r.ResetAt) {
		r.ResetAt = s.Restriction.ResetAt
	}
	if legacyReadyAt.After(r.RetryAt) {
		r.RetryAt = legacyReadyAt.UTC()
	}
	if s.Phase != "cooling" || s.PaidRecovery != paidRecovery || !sameEnterpriseRestriction(s.Restriction, r) {
		s.Version++
	}
	s.Restriction = r
	s.Phase = "cooling"
	s.PaidRecovery = paidRecovery
	s.legacyReadyAt = time.Time{}
	s.LastRejectedAt = now.UTC()
	// An unrelated rejection can update the restriction while the running
	// probe still occupies the slot.
	if isProbe {
		s.clearProbe()
	}
	s.rejections = nil
	if r.Kind == "daily" || enterpriseObservedPause(r.Kind) {
		delete(c.streaks, s.PoolID)
	}
	return true
}

func (s *SharedQuotaState) coolProbeUntil(retryAt, rejectedAt time.Time) {
	if s.PaidRecovery && s.Restriction.Kind == "daily" && s.Restriction.ResetAt.IsZero() {
		// Retain the old quota day's anchor before recording a post-reset 429.
		anchor := s.dailyResetAnchor()
		if !anchor.After(s.Restriction.RetryAt) {
			s.Restriction.ResetAt = anchor.UTC()
		}
	}
	s.Phase = "cooling"
	if retryAt.After(s.Restriction.RetryAt) {
		s.Restriction.RetryAt = retryAt.UTC()
	}
	if s.Restriction.ResetAt.After(s.Restriction.RetryAt) {
		s.Restriction.RetryAt = s.Restriction.ResetAt
	}
	if !rejectedAt.IsZero() {
		s.LastRejectedAt = rejectedAt.UTC()
	}
}

func sameEnterpriseRestriction(a, b ge.Restriction) bool {
	return a.Kind == b.Kind && a.Scope == b.Scope && a.Feature == b.Feature && a.Edition == b.Edition &&
		a.RetryAt.Equal(b.RetryAt) && a.ResetAt.Equal(b.ResetAt) && a.TimeSource == b.TimeSource
}

func (m *Manager) publishEnterpriseQuota() {
	if m == nil {
		return
	}
	var snapshots []*Auth
	m.mu.Lock()
	for _, a := range m.auths {
		if a != nil && a.Provider == ge.Provider {
			a.Generation++
			snapshots = append(snapshots, a.Clone())
		}
	}
	m.mu.Unlock()
	now := m.enterpriseQuota.now()
	for _, a := range snapshots {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(a)
		}
		models, epoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(a.ID)
		projections := make([]registry.ClientModelProjection, 0, len(models))
		for _, model := range models {
			if model != nil {
				projections = append(projections, m.clientModelProjectionForAuth(a, model.ID, now))
			}
		}
		registry.GetGlobalRegistry().ApplyClientModelProjections(a.ID, epoch, a.Generation, projections)
	}
}

func (g *enterpriseQuotaGate) views(now time.Time) []CooldownView {
	if g == nil || !g.enabled() {
		return nil
	}
	c := &g.manager.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	var views []CooldownView
	for _, key := range g.keys() {
		s := c.pools[quotaDigest(key)]
		if s == nil || s.Phase == "normal" || !g.appliesToQuota(s, now) {
			continue
		}
		phase := quotaPhase(s, now)
		retryAt := s.Restriction.RetryAt
		if phase == "probe_pending" {
			if readyAt := g.legacyProbeReadyAt(s, now); !readyAt.IsZero() {
				phase, retryAt = "cooling", readyAt
			}
		}
		count := 0
		for _, member := range c.bindings {
			if !member.appliesToQuota(s, now) {
				continue
			}
			for _, memberKey := range member.keys() {
				if memberKey == key {
					count++
					break
				}
			}
		}
		remaining := int64(0)
		if phase == "cooling" {
			remaining = int64((retryAt.Sub(now) + time.Second - 1) / time.Second)
		}
		kind := s.Restriction.Kind
		if kind == enterpriseNearLimitKind {
			// Older management clients already accept shared cooldowns with
			// no upstream restriction kind.
			kind = ""
		}
		views = append(views, CooldownView{Scope: "credential", Reason: "credential_quota", RetryAt: retryAt.UTC(), RemainingSeconds: remaining, Origin: "shared_pool", Feature: s.Restriction.Feature, PoolID: s.PoolID, PoolVersion: s.Version, RelatedAuthCount: count, Phase: phase, RestrictionKind: kind, RestrictionScope: s.Restriction.Scope, TimeSource: s.Restriction.TimeSource, SharedResetSupported: !g.manager.HomeEnabled() && !enterpriseLocalProtectionKind(s.Restriction.Kind)})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].PoolID < views[j].PoolID })
	return views
}

type SharedQuotaResetResult struct {
	ResetScope          string   `json:"reset_scope"`
	PoolID              string   `json:"pool_id"`
	PoolVersion         uint64   `json:"pool_version"`
	Phase               string   `json:"phase"`
	AffectedAuthIndexes []string `json:"affected_auth_indexes"`
}

func (m *Manager) ResetSharedQuota(ctx context.Context, authID, poolID string, version uint64) (*SharedQuotaResetResult, error) {
	if m.HomeEnabled() {
		return nil, fmt.Errorf("shared quota is unsupported in Home mode")
	}
	c := &m.enterpriseQuota
	c.mu.Lock()
	g := c.bindings[authID]
	s := c.pools[poolID]
	belongs := false
	if g != nil {
		for _, key := range g.keys() {
			if quotaDigest(key) == poolID {
				belongs = true
			}
		}
	}
	now := c.now()
	if !belongs || s == nil || !g.appliesToQuota(s, now) || version == 0 || s.Version != version || s.probe != 0 || enterpriseLocalProtectionKind(s.Restriction.Kind) {
		c.mu.Unlock()
		return nil, fmt.Errorf("shared pool changed; refresh credentials before retrying")
	}
	s.ResetEpoch++
	s.Version++
	s.Phase = "probe_pending"
	s.clearProbe()
	s.legacyReadyAt = time.Time{}
	_, hadStreak := c.streaks[s.PoolID]
	delete(c.streaks, s.PoolID)
	s.rejections = nil
	result := &SharedQuotaResetResult{ResetScope: "shared_pool", PoolID: poolID, PoolVersion: s.Version, Phase: s.Phase, AffectedAuthIndexes: []string{}}
	for _, member := range c.bindings {
		if !member.appliesToQuota(s, now) {
			continue
		}
		for _, key := range member.keys() {
			if quotaDigest(key) == poolID {
				result.AffectedAuthIndexes = append(result.AffectedAuthIndexes, member.policy.Index)
				break
			}
		}
	}
	durableProtection := s.durableProtection()
	c.mu.Unlock()
	sort.Strings(result.AffectedAuthIndexes)
	m.publishEnterpriseQuota()
	m.persistCooldownStates(ctx)
	if durableProtection || hadStreak {
		m.persistEnterpriseQuotaProtection()
	}
	return result, nil
}

func isEnterpriseQuotaResult(result Result) bool {
	return result.Provider == ge.Provider && result.Error != nil && (result.Error.Code == "enterprise_quota" || result.Error.Code == "enterprise_local_gate")
}

// Bound identities are detached before serialization; tokens never enter this state.
func validSharedQuotaState(s SharedQuotaState) bool {
	scope := sharedQuotaKeyScope(s.Key)
	if s.PoolID != quotaDigest(s.Key) || scope == "" || len(s.Key) > 1024 || s.Version == 0 {
		return false
	}
	if s.Phase == "normal" {
		return !s.PaidRecovery && s.Restriction == (ge.Restriction{})
	}
	if s.Phase != "cooling" && s.Phase != "probe_pending" && s.Phase != "probing" {
		return false
	}
	r := s.Restriction
	if r.Kind == enterpriseNearLimitKind {
		return s.PaidRecovery && scope == "edition" && r.Scope == scope && r.Feature == "assistant_search" &&
			r.TimeSource == "local_quota_pressure" && r.ResetAt.IsZero() && !r.RetryAt.IsZero()
	}
	if enterpriseLocalProtectionKind(r.Kind) {
		if scope != "edition" || r.Scope != scope || r.Feature != "assistant_search" || r.RetryAt.IsZero() || !s.PaidRecovery {
			return false
		}
		if r.Kind == enterpriseRateLimitedKind {
			return r.TimeSource == "local_quota_pressure" && r.ResetAt.IsZero() && !s.ProtectionStartedAt.IsZero()
		}
		return r.TimeSource == "quota_observation" && !r.ResetAt.IsZero() && !r.ResetAt.After(r.RetryAt) &&
			!s.ProtectionObservedAt.IsZero() && !s.ProtectionUsageAt.IsZero() && !s.ProtectionUsageAt.After(s.ProtectionObservedAt)
	}
	if (r.Kind != "daily" && r.Kind != "minute") || (r.Feature != "assistant_search" && r.Feature != "technical_requests") || r.RetryAt.IsZero() {
		return false
	}
	if r.Scope != scope || r.ResetAt.After(r.RetryAt) || (scope == "project" && r.Feature != "technical_requests") {
		return false
	}
	if s.PaidRecovery && (scope != "edition" || r.Feature != "assistant_search") {
		return false
	}
	return r.TimeSource == "google" || r.TimeSource == "estimated_pacific_midnight" || r.TimeSource == "estimated_minute"
}

func sharedQuotaKeyScope(key string) string {
	parts := strings.Split(key, "|")
	if len(parts) < 4 || parts[0] != ge.Provider {
		return ""
	}
	isDigest := func(value string) bool {
		if len(value) != sha256HexLength {
			return false
		}
		_, err := hex.DecodeString(value)
		return err == nil
	}
	if len(parts) == 4 && parts[1] == "credential" && isDigest(parts[2]) && parts[3] == "assistant_search" {
		return "credential"
	}
	if len(parts[1]) == 0 || parts[1][0] == '0' || parts[2] == "" {
		return ""
	}
	for _, digit := range parts[1] {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	if len(parts) == 5 && parts[3] != "" {
		if parts[3] == "project" && parts[4] == "technical_requests" {
			return "project"
		}
		if parts[3] != "project" && parts[4] == "assistant_search" {
			return "edition"
		}
	}
	if len(parts) == 6 && parts[3] == "user" && isDigest(parts[4]) && parts[5] == "assistant_search" {
		return "user"
	}
	return ""
}
