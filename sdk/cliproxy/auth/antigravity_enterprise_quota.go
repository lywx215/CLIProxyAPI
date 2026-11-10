package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
)

type antigravityEnterpriseGate struct {
	manager     *Manager
	authID      string
	epoch       uint64
	pool        string
	fingerprint string
}

type AntigravityEnterpriseBindingCooldown struct {
	AuthID      string    `json:"auth_id"`
	Fingerprint string    `json:"fingerprint"`
	Reason      string    `json:"reason"`
	RetryAt     time.Time `json:"retry_at"`
}

func aeBindingFingerprint(a *Auth) string {
	return ae.Digest(a.ID, a.Attributes[aeBindingAttribute], fmt.Sprint(a.Metadata["antigravity_enterprise_binding_revision"]), fmt.Sprint(a.Metadata[aeAuthorizationRevisionKey]))
}

// AntigravityEnterprisePoolState contains observations and restrictions, never OAuth.
type AntigravityEnterprisePoolState struct {
	Key          string    `json:"key"`
	Version      uint64    `json:"version"`
	Quota        ae.Quota  `json:"quota"`
	Restriction  *ae.Error `json:"restriction,omitempty"`
	RestrictedAt time.Time `json:"restricted_at,omitempty"`
}
type AntigravityEnterpriseQuotaSnapshot struct {
	SchemaVersion int                                    `json:"schema_version"`
	Version       uint64                                 `json:"version"`
	Pools         []AntigravityEnterprisePoolState       `json:"pools"`
	Cooldowns     []AntigravityEnterpriseBindingCooldown `json:"cooldowns,omitempty"`
	RateLimits    []AntigravityEnterpriseRateLimitState  `json:"binding_rate_limits,omitempty"`
}
type AntigravityEnterpriseQuotaStateStore interface {
	LoadAntigravityEnterpriseQuota(context.Context) (AntigravityEnterpriseQuotaSnapshot, error)
	SaveAntigravityEnterpriseQuota(context.Context, AntigravityEnterpriseQuotaSnapshot) error
}

func cloneAEQuota(q ae.Quota) ae.Quota {
	data, _ := json.Marshal(q)
	var out ae.Quota
	_ = json.Unmarshal(data, &out)
	return out
}
func (c *antigravityEnterpriseCoordinator) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
func (p *AntigravityEnterprisePoolState) blocked(now time.Time) (bool, time.Time) {
	if p == nil {
		return false, time.Time{}
	}
	next := now.Add(time.Minute)
	if p.Restriction != nil {
		if p.Restriction.RetryAt.After(now) {
			next = p.Restriction.RetryAt
		}
		return true, next
	}
	if p.Quota.ObservedAt.IsZero() || p.Quota.ObservedAt.After(now) || p.Quota.Overage == nil || p.Quota.Overage.Enabled {
		return false, time.Time{}
	}
	// Only an observed exhaustion is a routing cooldown. Stale or unknown
	// metadata must reach the executor's refresh-and-admit preflight instead of
	// becoming a synthetic rate limit that prevents the refresh from running.
	bucket, err := p.Quota.Weekly()
	if err != nil {
		return false, time.Time{}
	}
	if bucket.ResetTime != nil && !bucket.ResetTime.After(now) {
		return false, time.Time{}
	}
	if !bucket.Exhausted() {
		return false, time.Time{}
	}
	if bucket.ResetTime != nil {
		next = *bucket.ResetTime
	}
	return true, next
}
func (g *antigravityEnterpriseGate) blocked(now time.Time) (bool, time.Time) {
	if g == nil || g.manager == nil {
		return false, time.Time{}
	}
	c := &g.manager.antigravityEnterprise
	c.mu.Lock()
	defer c.mu.Unlock()
	if deadline := c.rateLimitDeadline(g.authID, g.fingerprint, now); !deadline.IsZero() {
		return true, deadline
	}
	if failure, ok := c.cooldowns[g.authID]; ok && failure.Fingerprint == g.fingerprint && failure.RetryAt.After(now) {
		return true, failure.RetryAt
	}
	return c.pools[g.pool].blocked(now)
}

func (m *Manager) restrictAntigravityEnterpriseBinding(result Result) {
	m.mu.RLock()
	source := m.auths[result.AuthID]
	if !IsAntigravityEnterpriseBindingAuth(source) || isStaleExecutionResult(result, source) {
		m.mu.RUnlock()
		return
	}
	c := &m.antigravityEnterprise
	c.mu.Lock()
	if c.cooldowns == nil {
		c.cooldowns = map[string]AntigravityEnterpriseBindingCooldown{}
	}
	deadline := c.currentTime().Add(30 * time.Minute)
	if result.RetryAfter != nil && *result.RetryAfter > 0 {
		deadline = c.currentTime().Add(*result.RetryAfter)
	}
	prior := c.cooldowns[source.ID]
	if prior.Fingerprint == aeBindingFingerprint(source) && prior.RetryAt.After(deadline) {
		deadline = prior.RetryAt
	}
	c.cooldowns[source.ID] = AntigravityEnterpriseBindingCooldown{AuthID: source.ID, Fingerprint: aeBindingFingerprint(source), Reason: "permission_denied", RetryAt: deadline}
	c.version++
	c.mu.Unlock()
	m.mu.RUnlock()
	m.publishAntigravityEnterpriseQuota()
	m.persistCooldownStates(context.Background())
}
func (m *Manager) currentAEQuotaSourceLocked(source *Auth) bool {
	if !IsAntigravityEnterpriseBindingAuth(source) {
		return false
	}
	current := m.auths[source.ID]
	return IsAntigravityEnterpriseBindingAuth(current) && current.RegistrationEpoch == source.RegistrationEpoch && current.CredentialVersion == source.CredentialVersion && BoundOwnerID(current) == BoundOwnerID(source) && AntigravityEnterpriseLicense(current) == AntigravityEnterpriseLicense(source)
}

func (m *Manager) ObserveAntigravityEnterpriseQuota(source *Auth, q ae.Quota) bool {
	if m == nil || source == nil || q.ObservedAt.IsZero() {
		return false
	}
	m.mu.RLock()
	if !m.currentAEQuotaSourceLocked(source) {
		m.mu.RUnlock()
		return false
	}
	key := AntigravityEnterpriseLicense(source).PoolKey()
	c := &m.antigravityEnterprise
	c.mu.Lock()
	now := c.currentTime()
	if q.ObservedAt.After(now) || now.Sub(q.ObservedAt) > ae.QuotaTTL {
		c.mu.Unlock()
		m.mu.RUnlock()
		return false
	}
	if c.pools == nil {
		c.pools = map[string]*AntigravityEnterprisePoolState{}
	}
	p := c.pools[key]
	if p == nil {
		p = &AntigravityEnterprisePoolState{Key: key}
		c.pools[key] = p
	}
	if p.Quota.ObservedAt.After(q.ObservedAt) {
		c.mu.Unlock()
		m.mu.RUnlock()
		return false
	}
	normalized := cloneAEQuota(q)
	if p.Quota.ObservedAt.Equal(q.ObservedAt) {
		identical := reflect.DeepEqual(p.Quota, normalized)
		c.mu.Unlock()
		m.mu.RUnlock()
		return identical
	}
	p.Quota = normalized
	p.Version++
	c.version++
	if p.Restriction != nil {
		b, err := q.Weekly()
		afterRestriction := q.ObservedAt.After(p.RestrictedAt)
		if err == nil && !b.Exhausted() && q.Overage != nil && !q.Overage.Enabled && afterRestriction && (!p.Restriction.RetryAt.After(q.ObservedAt)) {
			p.Restriction = nil
			p.RestrictedAt = time.Time{}
		}
	}
	c.mu.Unlock()
	m.mu.RUnlock()
	m.publishAntigravityEnterpriseQuota()
	m.persistCooldownStates(context.Background())
	return true
}
func (m *Manager) AntigravityEnterpriseQuota(source *Auth) (ae.Quota, bool) {
	if m == nil || source == nil {
		return ae.Quota{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.currentAEQuotaSourceLocked(source) {
		return ae.Quota{}, false
	}
	c := &m.antigravityEnterprise
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pools[AntigravityEnterpriseLicense(source).PoolKey()]
	if p == nil || p.Quota.ObservedAt.IsZero() {
		return ae.Quota{}, false
	}
	return cloneAEQuota(p.Quota), true
}

func (m *Manager) AntigravityEnterpriseQuotaVersion(source *Auth) uint64 {
	if m == nil || source == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.currentAEQuotaSourceLocked(source) {
		return 0
	}
	c := &m.antigravityEnterprise
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pools[AntigravityEnterpriseLicense(source).PoolKey()]; p != nil {
		return p.Version
	}
	return 0
}
func (m *Manager) CheckAntigravityEnterpriseQuota(source *Auth, now time.Time) error {
	if m == nil || source == nil {
		return ErrAntigravityEnterpriseAccountChanged
	}
	m.mu.RLock()
	if !m.currentAEQuotaSourceLocked(source) {
		m.mu.RUnlock()
		return ErrAntigravityEnterpriseAccountChanged
	}
	current := m.auths[source.ID]
	parent := m.auths[BoundOwnerID(current)]
	if current.Disabled || parent == nil || parent.Disabled || hasUnauthorizedAuthFailure(parent) || hasDisabledInvalidGrantFailure(parent) {
		m.mu.RUnlock()
		return &ae.Error{HTTP: 401, Code: "credential_unavailable"}
	}
	c := &m.antigravityEnterprise
	c.mu.Lock()
	if deadline := c.rateLimitDeadline(source.ID, aeBindingFingerprint(source), now); !deadline.IsZero() {
		c.mu.Unlock()
		m.mu.RUnlock()
		return &ae.Error{HTTP: 429, Code: "binding_rate_limit", Scope: "binding", RetryAt: deadline}
	}
	if failure, ok := c.cooldowns[source.ID]; ok && failure.Fingerprint == aeBindingFingerprint(source) && failure.RetryAt.After(now) {
		c.mu.Unlock()
		m.mu.RUnlock()
		return &ae.Error{HTTP: 403, Code: "binding_cooldown", RetryAt: failure.RetryAt}
	}
	p := c.pools[AntigravityEnterpriseLicense(source).PoolKey()]
	if p == nil {
		c.mu.Unlock()
		m.mu.RUnlock()
		return &ae.Error{HTTP: 503, Code: "metadata_pending"}
	}
	if p.Restriction != nil {
		copyRestriction := *p.Restriction
		c.mu.Unlock()
		m.mu.RUnlock()
		return &copyRestriction
	}
	q := cloneAEQuota(p.Quota)
	c.mu.Unlock()
	m.mu.RUnlock()
	return q.Admit(now)
}
func (m *Manager) RestrictAntigravityEnterpriseQuota(source *Auth, e *ae.Error) {
	if m == nil || source == nil || e == nil || e.Code != "weekly_quota" {
		return
	}
	m.mu.RLock()
	if !m.currentAEQuotaSourceLocked(source) {
		m.mu.RUnlock()
		return
	}
	key := AntigravityEnterpriseLicense(source).PoolKey()
	c := &m.antigravityEnterprise
	c.mu.Lock()
	if c.pools == nil {
		c.pools = map[string]*AntigravityEnterprisePoolState{}
	}
	p := c.pools[key]
	if p == nil {
		p = &AntigravityEnterprisePoolState{Key: key}
		c.pools[key] = p
	}
	copyRestriction := *e
	copyRestriction.Scope = "shared_pool"
	copyRestriction.Started = false
	copyRestriction.Attempted = false
	if p.Restriction != nil && p.Restriction.RetryAt.After(copyRestriction.RetryAt) {
		copyRestriction.RetryAt = p.Restriction.RetryAt
	}
	p.Restriction = &copyRestriction
	p.RestrictedAt = c.currentTime()
	p.Version++
	c.version++
	c.mu.Unlock()
	m.mu.RUnlock()
	m.publishAntigravityEnterpriseQuota()
	m.persistCooldownStates(context.Background())
}
func (g *antigravityEnterpriseGate) views(now time.Time) []CooldownView {
	if g == nil || g.manager == nil {
		return nil
	}
	c := &g.manager.antigravityEnterprise
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pools[g.pool]
	views := []CooldownView{}
	if deadline := c.rateLimitDeadline(g.authID, g.fingerprint, now); !deadline.IsZero() {
		views = append(views, aeRateLimitView(deadline, now))
	}
	if failure, ok := c.cooldowns[g.authID]; ok && failure.Fingerprint == g.fingerprint && failure.RetryAt.After(now) {
		views = append(views, CooldownView{Scope: "credential", Reason: failure.Reason, RetryAt: failure.RetryAt.UTC(), RemainingSeconds: int64(failure.RetryAt.Sub(now).Seconds()), HTTPStatus: 403})
	}
	if p == nil {
		return views
	}
	b, err := p.Quota.Weekly()
	if p.Restriction == nil && (err != nil || !b.Exhausted()) {
		return views
	}
	_, retry := p.blocked(now)
	if retry.IsZero() {
		retry = now.Add(time.Minute)
	}
	phase := "cooling"
	var reset time.Time
	if p.Restriction != nil {
		reset = p.Restriction.RetryAt
	} else if b != nil && b.ResetTime != nil {
		reset = *b.ResetTime
	}
	if reset.IsZero() || !reset.After(now) {
		phase = "probe_pending"
	}
	seconds := max(int64(0), int64(retry.Sub(now).Seconds()))
	var nextCheck *time.Time
	if phase == "probe_pending" {
		at := retry.UTC()
		nextCheck = &at
		retry = reset
		if retry.IsZero() {
			retry = p.Quota.ObservedAt
		}
		if retry.IsZero() {
			retry = p.RestrictedAt
		}
		seconds = 0
	}
	return append(views, CooldownView{Origin: "shared_pool", Feature: "ai_developer_tools", PoolID: ae.Digest(g.pool), PoolVersion: p.Version, Phase: phase, RestrictionKind: "weekly", RestrictionScope: "edition", TimeSource: "google", Scope: "credential", Reason: "weekly_quota", RetryAt: retry.UTC(), NextCheckAt: nextCheck, RemainingSeconds: seconds, SharedResetSupported: true})
}
func (m *Manager) antigravityEnterpriseQuotaSnapshot() AntigravityEnterpriseQuotaSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := &m.antigravityEnterprise
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneRateLimits(c.currentTime())
	c.version++
	snapshot := AntigravityEnterpriseQuotaSnapshot{SchemaVersion: 1, Version: c.version, Pools: []AntigravityEnterprisePoolState{}}
	for _, p := range c.pools {
		copyState := *p
		copyState.Quota = cloneAEQuota(p.Quota)
		if p.Restriction != nil {
			v := *p.Restriction
			copyState.Restriction = &v
		}
		snapshot.Pools = append(snapshot.Pools, copyState)
	}
	for _, failure := range c.cooldowns {
		snapshot.Cooldowns = append(snapshot.Cooldowns, failure)
	}
	for id, state := range c.rateLimits {
		auth := m.auths[id]
		if !IsAntigravityEnterpriseBindingAuth(auth) || m.auths[BoundOwnerID(auth)] == nil || aeBindingFingerprint(auth) != state.Fingerprint {
			delete(c.rateLimits, id)
			continue
		}
		copyState := *state
		copyState.Observations = slices.Clone(state.Observations)
		snapshot.RateLimits = append(snapshot.RateLimits, copyState)
	}
	sort.Slice(snapshot.RateLimits, func(i, j int) bool { return snapshot.RateLimits[i].AuthID < snapshot.RateLimits[j].AuthID })
	sort.Slice(snapshot.Cooldowns, func(i, j int) bool { return snapshot.Cooldowns[i].AuthID < snapshot.Cooldowns[j].AuthID })
	sort.Slice(snapshot.Pools, func(i, j int) bool { return snapshot.Pools[i].Key < snapshot.Pools[j].Key })
	return snapshot
}
func validateAEQuotaSnapshot(snapshot AntigravityEnterpriseQuotaSnapshot) error {
	if snapshot.SchemaVersion != 1 || snapshot.Version == 0 || len(snapshot.Pools) > 10000 || len(snapshot.RateLimits) > 10000 {
		return fmt.Errorf("invalid antigravity-enterprise quota snapshot")
	}
	seen := map[string]bool{}
	for _, state := range snapshot.RateLimits {
		if state.AuthID == "" || len(state.Fingerprint) != 64 || seen[state.AuthID] || len(state.Observations) > aeRateLimitHistoryLimit {
			return fmt.Errorf("invalid antigravity-enterprise binding rate limit")
		}
		seen[state.AuthID] = true
		attempts := map[string]bool{}
		for _, observation := range state.Observations {
			if observation.AttemptID == "" || len(observation.AttemptID) > 128 || observation.RetryAt.IsZero() || attempts[observation.AttemptID] {
				return fmt.Errorf("invalid antigravity-enterprise rate limit observation")
			}
			attempts[observation.AttemptID] = true
		}
	}
	clear(seen)
	for _, failure := range snapshot.Cooldowns {
		if failure.AuthID == "" || len(failure.Fingerprint) != 64 || failure.Reason != "permission_denied" || failure.RetryAt.IsZero() {
			return fmt.Errorf("invalid antigravity-enterprise binding cooldown")
		}
	}
	for _, p := range snapshot.Pools {
		parts := strings.Split(p.Key, "|")
		if len(parts) != 5 || parts[0] != ae.Provider || parts[4] != ae.WeeklyBucket || seen[p.Key] || p.Version == 0 {
			return fmt.Errorf("invalid antigravity-enterprise pool")
		}
		seen[p.Key] = true
		if (ae.License{ProjectID: parts[1], Location: parts[2], UserTier: parts[3]}).Validate() != nil {
			return fmt.Errorf("invalid antigravity-enterprise pool identity")
		}
		if p.Restriction != nil && (p.Restriction.Code != "weekly_quota" || p.Restriction.HTTP != 429) {
			return fmt.Errorf("invalid antigravity-enterprise restriction")
		}
	}
	return nil
}
func (m *Manager) restoreAntigravityEnterpriseQuota(ctx context.Context, store CooldownStateStore) error {
	s, ok := store.(AntigravityEnterpriseQuotaStateStore)
	if !ok || m.HomeEnabled() {
		return nil
	}
	snapshot, err := s.LoadAntigravityEnterpriseQuota(ctx)
	if err != nil {
		return err
	}
	if snapshot.SchemaVersion == 0 {
		return nil
	}
	if err = validateAEQuotaSnapshot(snapshot); err != nil {
		return err
	}
	m.mu.RLock()
	c := &m.antigravityEnterprise
	c.mu.Lock()
	if c.pools == nil {
		c.pools = map[string]*AntigravityEnterprisePoolState{}
	}
	c.version = max(c.version, snapshot.Version)
	for _, p := range snapshot.Pools {
		if c.pools[p.Key] == nil {
			copyState := p
			c.pools[p.Key] = &copyState
		}
	}
	if c.cooldowns == nil {
		c.cooldowns = map[string]AntigravityEnterpriseBindingCooldown{}
	}
	for _, failure := range snapshot.Cooldowns {
		if _, ok := c.cooldowns[failure.AuthID]; !ok {
			c.cooldowns[failure.AuthID] = failure
		}
	}
	if c.rateLimits == nil {
		c.rateLimits = make(map[string]*AntigravityEnterpriseRateLimitState)
	}
	for _, state := range snapshot.RateLimits {
		auth := m.auths[state.AuthID]
		if !IsAntigravityEnterpriseBindingAuth(auth) || aeBindingFingerprint(auth) != state.Fingerprint || c.rateLimits[state.AuthID] != nil {
			continue
		}
		copyState := state
		copyState.Observations = slices.Clone(state.Observations)
		c.rateLimits[state.AuthID] = &copyState
	}
	c.pruneRateLimits(c.currentTime())
	c.mu.Unlock()
	m.mu.RUnlock()
	m.publishAntigravityEnterpriseQuota()
	return nil
}
func (s *FileCooldownStateStore) LoadAntigravityEnterpriseQuota(ctx context.Context) (AntigravityEnterpriseQuotaSnapshot, error) {
	if s == nil || s.dir == "" {
		return AntigravityEnterpriseQuotaSnapshot{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readAntigravityEnterpriseQuota(ctx)
}
func (s *FileCooldownStateStore) readAntigravityEnterpriseQuota(ctx context.Context) (AntigravityEnterpriseQuotaSnapshot, error) {
	var out AntigravityEnterpriseQuotaSnapshot
	if ctx != nil && ctx.Err() != nil {
		return out, ctx.Err()
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "antigravity-enterprise-quota.sqs"))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if len(data) > 8<<20 || json.Unmarshal(data, &out) != nil {
		return out, fmt.Errorf("invalid antigravity-enterprise quota file")
	}
	return out, validateAEQuotaSnapshot(out)
}
func (s *FileCooldownStateStore) SaveAntigravityEnterpriseQuota(ctx context.Context, snapshot AntigravityEnterpriseQuotaSnapshot) error {
	if s == nil || s.dir == "" {
		return nil
	}
	if err := validateAEQuotaSnapshot(snapshot); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	prior, err := s.readAntigravityEnterpriseQuota(ctx)
	if err != nil {
		return err
	}
	if prior.Version > snapshot.Version {
		return fmt.Errorf("stale antigravity-enterprise quota snapshot")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, "antigravity-enterprise-quota-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(s.dir, "antigravity-enterprise-quota.sqs"))
}
