package auth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	log "github.com/sirupsen/logrus"
)

// SharedQuotaStateStore is optional so existing cooldown stores remain compatible.
type SharedQuotaStateStore interface {
	LoadSharedQuota(context.Context) (SharedQuotaSnapshot, error)
	SaveSharedQuota(context.Context, SharedQuotaSnapshot) error
}

type SharedQuotaBinding struct {
	AuthID      string            `json:"auth_id"`
	Fingerprint string            `json:"fingerprint"`
	Identity    *ge.QuotaIdentity `json:"identity,omitempty"`
}

type SharedQuotaSnapshot struct {
	SchemaVersion int                  `json:"schema_version"`
	Version       uint64               `json:"version"`
	Pools         []SharedQuotaState   `json:"pools"`
	Bindings      []SharedQuotaBinding `json:"bindings"`
}

// Validate rejects corrupt snapshots before any state is published or persisted.
func (s SharedQuotaSnapshot) Validate() error {
	if s.SchemaVersion != 1 || s.Version == 0 {
		return fmt.Errorf("unsupported shared quota state schema or version")
	}
	pools := make(map[string]struct{}, len(s.Pools))
	for _, state := range s.Pools {
		if !validSharedQuotaState(state) {
			return fmt.Errorf("invalid shared quota state")
		}
		if _, duplicate := pools[state.PoolID]; duplicate {
			return fmt.Errorf("duplicate shared quota state")
		}
		pools[state.PoolID] = struct{}{}
	}
	bindings := make(map[string]struct{}, len(s.Bindings))
	for _, binding := range s.Bindings {
		if binding.AuthID == "" || len(binding.Fingerprint) != sha256HexLength {
			return fmt.Errorf("invalid shared quota binding")
		}
		if _, err := hex.DecodeString(binding.Fingerprint); err != nil {
			return fmt.Errorf("invalid shared quota binding")
		}
		if _, duplicate := bindings[binding.AuthID]; duplicate {
			return fmt.Errorf("duplicate shared quota binding")
		}
		bindings[binding.AuthID] = struct{}{}
	}
	return nil
}

const sha256HexLength = 64

func (m *Manager) sharedQuotaSnapshot() SharedQuotaSnapshot {
	return m.quotaSnapshot(false)
}

func (m *Manager) quotaSnapshot(protectionOnly bool) SharedQuotaSnapshot {
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeVersion++
	snapshot := SharedQuotaSnapshot{SchemaVersion: 1, Version: c.storeVersion, Pools: []SharedQuotaState{}, Bindings: []SharedQuotaBinding{}}
	for _, s := range c.pools {
		if protectionOnly && !s.durableProtection() ||
			!protectionOnly && (s.Restriction.Kind == enterpriseNearLimitKind || enterpriseLocalProtectionKind(s.Restriction.Kind)) {
			continue
		}
		state := *s
		state.clearProbe()
		state.legacyReadyAt = time.Time{}
		state.rejections = nil
		state.streak = nil
		snapshot.Pools = append(snapshot.Pools, state)
	}
	for _, g := range c.bindings {
		if protectionOnly {
			s := c.pools[quotaDigest(g.keys()["edition"])]
			if s == nil || !s.durableProtection() || !g.matchesProtectedPool(s) {
				continue
			}
		}
		binding := SharedQuotaBinding{AuthID: g.authID, Fingerprint: g.fingerprint}
		if g.identity != nil {
			identity := *g.identity
			binding.Identity = &identity
		}
		snapshot.Bindings = append(snapshot.Bindings, binding)
	}
	sort.Slice(snapshot.Pools, func(i, j int) bool { return snapshot.Pools[i].PoolID < snapshot.Pools[j].PoolID })
	sort.Slice(snapshot.Bindings, func(i, j int) bool { return snapshot.Bindings[i].AuthID < snapshot.Bindings[j].AuthID })
	return snapshot
}

func (m *Manager) restoreSharedQuota(ctx context.Context, store CooldownStateStore) error {
	shared, ok := store.(SharedQuotaStateStore)
	if !ok || m.HomeEnabled() {
		return nil
	}
	snapshot, err := shared.LoadSharedQuota(ctx)
	if err != nil {
		return err
	}
	if snapshot.SchemaVersion == 0 && snapshot.Version == 0 && len(snapshot.Pools) == 0 && len(snapshot.Bindings) == 0 {
		return nil
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	m.restoreQuotaSnapshot(snapshot, false)
	return nil
}

// ConfigureEnterpriseQuotaProtectionStore restores paid-pool protection before
// traffic starts, even when ordinary cooldown persistence is disabled.
func (m *Manager) ConfigureEnterpriseQuotaProtectionStore(ctx context.Context, dir string) error {
	if m == nil {
		return nil
	}
	c := &m.enterpriseQuota
	c.protectionMu.Lock()
	defer c.protectionMu.Unlock()
	if dir == "" || m.HomeEnabled() {
		c.protectionPath = ""
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path := filepath.Join(dir, "enterprise-quota-protection.sqs")
	if path == c.protectionPath {
		return nil
	}
	snapshot, err := readSharedQuotaSnapshot(ctx, path)
	if err != nil {
		return err
	}
	for _, pool := range snapshot.Pools {
		if !pool.durableProtection() {
			return fmt.Errorf("unexpected pool in Enterprise quota protection state")
		}
	}
	m.restoreQuotaSnapshot(snapshot, true)
	c.protectionPath = path
	current := m.quotaSnapshot(true)
	if len(current.Pools) == 0 && snapshot.Version == 0 {
		return nil
	}
	return writeSharedQuotaSnapshot(ctx, path, current)
}

func (m *Manager) persistEnterpriseQuotaProtection() {
	c := &m.enterpriseQuota
	c.protectionMu.Lock()
	defer c.protectionMu.Unlock()
	if c.protectionPath == "" || m.HomeEnabled() {
		return
	}
	if err := writeSharedQuotaSnapshot(context.Background(), c.protectionPath, m.quotaSnapshot(true)); err != nil {
		log.WithError(err).Warn("failed to persist Gemini Enterprise quota protection")
	}
}

func (m *Manager) restoreQuotaSnapshot(snapshot SharedQuotaSnapshot, protectionOnly bool) {
	c := &m.enterpriseQuota
	m.mu.Lock()
	c.mu.Lock()
	if c.pools == nil {
		c.pools = map[string]*SharedQuotaState{}
	}
	c.storeVersion = max(c.storeVersion, snapshot.Version)
	now := c.now()
	for _, s := range snapshot.Pools {
		// Versions from independently configured stores are not a shared clock.
		// Existing memory, including explicit-reset tombstones, remains authoritative.
		if c.pools[s.PoolID] != nil {
			continue
		}
		// Keep the automatic daily restriction distinguishable until its
		// paid identity is available. quotaPhase still exposes elapsed waits.
		if s.Phase == "probing" || (s.Phase == "cooling" && !s.Restriction.RetryAt.After(now) && !s.canAlignLegacyDailyRecovery()) {
			s.Phase = "probe_pending"
			s.Version++
		}
		s.clearProbe()
		s.legacyReadyAt = time.Time{}
		if s.canAlignLegacyDailyRecovery() {
			readyAt := enterprisePaidDailyRecoveryAt(s.dailyResetAnchor(), s.PoolID)
			if readyAt.After(s.Restriction.RetryAt) && readyAt.After(now) {
				s.legacyReadyAt = readyAt
			}
		}
		copyState := s
		c.pools[s.PoolID] = &copyState
	}
	for _, gate := range c.bindings {
		c.alignLegacyPaidDailyRecoveryLocked(gate, now)
	}
	c.mu.Unlock()
	for _, binding := range snapshot.Bindings {
		a := m.auths[binding.AuthID]
		if a == nil || a.Provider != ge.Provider || enterpriseFingerprint(a) != binding.Fingerprint || !binding.Identity.ValidFor(ge.FromMetadata(a.Metadata)) {
			continue
		}
		if a.EnterpriseQuotaIdentity == nil {
			identity := *binding.Identity
			a.EnterpriseQuotaIdentity = &identity
			m.attachEnterpriseQuotaLocked(a)
		}
		if protectionOnly {
			c.mu.Lock()
			if g := c.bindings[a.ID]; g != nil && g.identity != nil && *g.identity == *binding.Identity {
				if s := c.pools[quotaDigest(g.keys()["edition"])]; s != nil && s.durableProtection() {
					g.restoredProtectionPaid = true
				}
			}
			c.mu.Unlock()
		}
	}
	m.mu.Unlock()
	m.publishEnterpriseQuota()
}

func (s *FileCooldownStateStore) LoadSharedQuota(ctx context.Context) (SharedQuotaSnapshot, error) {
	if s == nil || s.dir == "" {
		return SharedQuotaSnapshot{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readSharedQuota(ctx)
}
func (s *FileCooldownStateStore) readSharedQuota(ctx context.Context) (SharedQuotaSnapshot, error) {
	return readSharedQuotaSnapshot(ctx, filepath.Join(s.dir, "shared-quota-state.sqs"))
}

func readSharedQuotaSnapshot(ctx context.Context, path string) (SharedQuotaSnapshot, error) {
	var snapshot SharedQuotaSnapshot
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, fmt.Errorf("read shared quota state: %w", err)
	}
	if len(data) > 8*1024*1024 {
		return snapshot, fmt.Errorf("shared quota state exceeds size limit")
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return snapshot, fmt.Errorf("invalid shared quota state file")
	}
	if err := snapshot.Validate(); err != nil {
		return SharedQuotaSnapshot{}, err
	}
	return snapshot, nil
}
func (s *FileCooldownStateStore) SaveSharedQuota(ctx context.Context, snapshot SharedQuotaSnapshot) error {
	if s == nil || s.dir == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeSharedQuotaSnapshot(ctx, filepath.Join(s.dir, "shared-quota-state.sqs"), snapshot)
}

func writeSharedQuotaSnapshot(ctx context.Context, path string, snapshot SharedQuotaSnapshot) error {
	previous, err := readSharedQuotaSnapshot(ctx, path)
	if err != nil {
		return err
	}
	if previous.Version > snapshot.Version {
		return fmt.Errorf("stale shared quota snapshot")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode shared quota state: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create shared quota state directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "shared-quota-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if _, errWrite := file.Write(data); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("write shared quota state: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return errSync
	}
	if errClose := file.Close(); errClose != nil {
		return errClose
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace shared quota state: %w", err)
	}
	return nil
}
