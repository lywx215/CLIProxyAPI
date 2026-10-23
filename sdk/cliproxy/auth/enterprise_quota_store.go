package auth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
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
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeVersion++
	snapshot := SharedQuotaSnapshot{SchemaVersion: 1, Version: c.storeVersion, Pools: []SharedQuotaState{}, Bindings: []SharedQuotaBinding{}}
	for _, s := range c.pools {
		state := *s
		state.probe = 0
		state.probeOwner = nil
		snapshot.Pools = append(snapshot.Pools, state)
	}
	for _, g := range c.bindings {
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
	c := &m.enterpriseQuota
	m.mu.Lock()
	c.mu.Lock()
	if c.pools == nil {
		c.pools = map[string]*SharedQuotaState{}
	}
	c.storeVersion = max(c.storeVersion, snapshot.Version)
	for _, s := range snapshot.Pools {
		// Versions from independently configured stores are not a shared clock.
		// Existing memory, including explicit-reset tombstones, remains authoritative.
		if c.pools[s.PoolID] != nil {
			continue
		}
		if s.Phase == "probing" || (s.Phase == "cooling" && !s.Restriction.RetryAt.After(c.now())) {
			s.Phase = "probe_pending"
			s.Version++
		}
		s.probe = 0
		s.probeOwner = nil
		copyState := s
		c.pools[s.PoolID] = &copyState
	}
	c.mu.Unlock()
	for _, binding := range snapshot.Bindings {
		a := m.auths[binding.AuthID]
		if a == nil || a.Provider != ge.Provider || a.EnterpriseQuotaIdentity != nil || enterpriseFingerprint(a) != binding.Fingerprint || !binding.Identity.ValidFor(ge.FromMetadata(a.Metadata)) {
			continue
		}
		identity := *binding.Identity
		a.EnterpriseQuotaIdentity = &identity
		m.attachEnterpriseQuotaLocked(a)
	}
	m.mu.Unlock()
	m.publishEnterpriseQuota()
	return nil
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
	var snapshot SharedQuotaSnapshot
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "shared-quota-state.sqs"))
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
	previous, err := s.readSharedQuota(ctx)
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
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("create shared quota state directory: %w", err)
	}
	file, err := os.CreateTemp(s.dir, "shared-quota-*.tmp")
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
	if err := os.Rename(name, filepath.Join(s.dir, "shared-quota-state.sqs")); err != nil {
		return fmt.Errorf("replace shared quota state: %w", err)
	}
	return nil
}
