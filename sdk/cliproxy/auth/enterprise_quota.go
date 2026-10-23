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
	storeVersion uint64
	mu           sync.Mutex
	pools        map[string]*SharedQuotaState
	bindings     map[string]*enterpriseQuotaGate
	nowFunc      func() time.Time
	nextProbe    uint64
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
	probe          uint64
	probeOwner     *enterpriseQuotaGate
}

type enterpriseQuotaGate struct {
	manager             *Manager
	authID, fingerprint string
	epoch, credential   uint64
	identity            *ge.QuotaIdentity
	policy              *Auth
}

type quotaTicketVersion struct{ reset, version uint64 }
type enterpriseQuotaTicket struct {
	gate     *enterpriseQuotaGate
	versions map[string]quotaTicketVersion
	probes   map[string]uint64
	finished sync.Once
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
	c.releaseGateLocked(old)
	g := &enterpriseQuotaGate{manager: m, authID: auth.ID, fingerprint: fingerprint, epoch: auth.RegistrationEpoch, credential: auth.CredentialVersion, identity: identity, policy: policy}
	auth.EnterpriseQuotaIdentity = identity
	auth.enterpriseQuota = g
	c.bindings[auth.ID] = g
}

func (c *enterpriseQuotaCoordinator) releaseGateLocked(g *enterpriseQuotaGate) {
	if g == nil {
		return
	}
	for _, s := range c.pools {
		if s.probeOwner == g && s.Phase == "probing" {
			s.probe = 0
			s.probeOwner = nil
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
		s.probe = 0
		s.probeOwner = nil
	}
}

func (g *enterpriseQuotaGate) currentLocked() bool {
	c := &g.manager.enterpriseQuota
	current := c.bindings[g.authID]
	return current == g
}
func quotaPhase(state *SharedQuotaState, now time.Time) string {
	if state.Phase == "cooling" && !state.Restriction.RetryAt.After(now) {
		return "probe_pending"
	}
	return state.Phase
}
func (g *enterpriseQuotaGate) blocked(now time.Time) (bool, time.Time) {
	if g == nil || !g.enabled() {
		return false, time.Time{}
	}
	c := &g.manager.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	var next time.Time
	for _, key := range g.keys() {
		s := c.pools[quotaDigest(key)]
		if s == nil {
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
		}
	}
	return !next.IsZero(), next
}

func (g *enterpriseQuotaGate) begin() (*enterpriseQuotaTicket, error) {
	c := &g.manager.enterpriseQuota
	now := c.now()
	c.mu.Lock()
	if g.manager.HomeEnabled() {
		c.mu.Unlock()
		return nil, &Error{Code: ErrorCodeRequestScoped, Message: "Gemini Enterprise shared quota is unsupported in Home mode", HTTPStatus: 409}
	}
	if !g.currentLocked() {
		c.mu.Unlock()
		return nil, &Error{Code: ErrorCodeRequestScoped, Message: "Gemini Enterprise credential changed; retry the request", HTTPStatus: 409}
	}
	t := &enterpriseQuotaTicket{gate: g, versions: map[string]quotaTicketVersion{}, probes: map[string]uint64{}}
	keys := g.keys()
	enabled := g.enabled()
	var wait time.Duration
	for _, key := range keys {
		id := quotaDigest(key)
		s := c.pools[id]
		if s == nil {
			s = &SharedQuotaState{PoolID: id, Key: key, Version: 1, Phase: "normal"}
			c.pools[id] = s
		}
		t.versions[id] = quotaTicketVersion{s.ResetEpoch, s.Version}
		if !enabled {
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
		}
	}
	if wait > 0 {
		c.mu.Unlock()
		return nil, &enterpriseLocalGateError{newModelCooldownError("", ge.Provider, wait)}
	}
	if enabled {
		for _, key := range keys {
			s := c.pools[quotaDigest(key)]
			if quotaPhase(s, now) != "probe_pending" {
				continue
			}
			c.nextProbe++
			s.probe = c.nextProbe
			s.probeOwner = g
			s.Phase = "probing"
			s.Version++
			t.probes[s.PoolID] = s.probe
			t.versions[s.PoolID] = quotaTicketVersion{s.ResetEpoch, s.Version}
		}
	}
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
	t.finished.Do(func() { t.finishOnce(err) })
}

func (t *enterpriseQuotaTicket) finishOnce(err error) {
	g := t.gate
	c := &g.manager.enterpriseQuota
	now := c.now()
	var upstream *ge.GenerationError
	_ = errors.As(err, &upstream)
	c.mu.Lock()
	if !g.currentLocked() || g.manager.HomeEnabled() {
		c.mu.Unlock()
		return
	}
	changed := false
	deniedID := ""
	if upstream != nil && upstream.Restriction != nil {
		r := *upstream.Restriction
		keys := g.keys()
		key := keys[r.Scope]
		if key == "" || (r.Edition != "" && (g.identity == nil || !g.identity.Verified || r.Edition != g.identity.Edition)) {
			key = keys["credential"]
			r.Scope = "credential"
		}
		id := quotaDigest(key)
		s := c.pools[id]
		captured, ok := t.versions[id]
		if s != nil && ok && s.ResetEpoch == captured.reset {
			deniedID = id
			// Merge a concurrent batch without progressive backoff or deadline shortening.
			if s.Restriction.RetryAt.After(r.RetryAt) {
				resetAt := r.ResetAt
				r = s.Restriction
				if resetAt.After(r.ResetAt) {
					r.ResetAt = resetAt
				}
			}
			if s.Restriction.ResetAt.After(r.ResetAt) {
				r.ResetAt = s.Restriction.ResetAt
			}
			if s.Phase != "cooling" || !sameEnterpriseRestriction(s.Restriction, r) {
				s.Version++
			}
			s.Restriction = r
			s.Phase = "cooling"
			s.LastRejectedAt = now.UTC()
			s.probe = 0
			s.probeOwner = nil
			changed = true
		}
	}
	for id, probe := range t.probes {
		s := c.pools[id]
		version := t.versions[id]
		if id == deniedID || s == nil || s.ResetEpoch != version.reset || s.Version != version.version || s.probe != probe {
			continue
		}
		s.probe = 0
		s.probeOwner = nil
		s.Version++
		if err == nil {
			s.Phase = "normal"
			s.Restriction = ge.Restriction{}
		} else {
			s.Phase = "probe_pending"
		}
		changed = true
	}
	c.mu.Unlock()
	if changed {
		g.manager.publishEnterpriseQuota()
		g.manager.persistCooldownStates(context.Background())
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
		if s == nil || s.Phase == "normal" {
			continue
		}
		phase := quotaPhase(s, now)
		count := 0
		for _, member := range c.bindings {
			for _, memberKey := range member.keys() {
				if memberKey == key {
					count++
					break
				}
			}
		}
		remaining := int64(0)
		if phase == "cooling" {
			remaining = int64((s.Restriction.RetryAt.Sub(now) + time.Second - 1) / time.Second)
		}
		views = append(views, CooldownView{Scope: "credential", Reason: "credential_quota", RetryAt: s.Restriction.RetryAt.UTC(), RemainingSeconds: remaining, Origin: "shared_pool", Feature: s.Restriction.Feature, PoolID: s.PoolID, PoolVersion: s.Version, RelatedAuthCount: count, Phase: phase, RestrictionKind: s.Restriction.Kind, RestrictionScope: s.Restriction.Scope, TimeSource: s.Restriction.TimeSource, SharedResetSupported: !g.manager.HomeEnabled()})
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
	if !belongs || s == nil || version == 0 || s.Version != version {
		c.mu.Unlock()
		return nil, fmt.Errorf("shared pool changed; refresh credentials before retrying")
	}
	s.ResetEpoch++
	s.Version++
	s.Phase = "probe_pending"
	s.probe = 0
	s.probeOwner = nil
	result := &SharedQuotaResetResult{ResetScope: "shared_pool", PoolID: poolID, PoolVersion: s.Version, Phase: s.Phase, AffectedAuthIndexes: []string{}}
	for _, member := range c.bindings {
		for _, key := range member.keys() {
			if quotaDigest(key) == poolID {
				result.AffectedAuthIndexes = append(result.AffectedAuthIndexes, member.policy.Index)
				break
			}
		}
	}
	c.mu.Unlock()
	sort.Strings(result.AffectedAuthIndexes)
	m.publishEnterpriseQuota()
	m.persistCooldownStates(ctx)
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
		return s.Restriction == (ge.Restriction{})
	}
	if s.Phase != "cooling" && s.Phase != "probe_pending" && s.Phase != "probing" {
		return false
	}
	r := s.Restriction
	if (r.Kind != "daily" && r.Kind != "minute") || (r.Feature != "assistant_search" && r.Feature != "technical_requests") || r.RetryAt.IsZero() {
		return false
	}
	if r.Scope != scope || r.ResetAt.After(r.RetryAt) || (scope == "project" && r.Feature != "technical_requests") {
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
