package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

const enterpriseCatalogCacheKey = "enterprise_catalog_cache"

type EnterpriseCatalogSnapshot struct {
	Version     int                `json:"version"`
	Fingerprint string             `json:"fingerprint"`
	Resources   ge.ResourceBinding `json:"resources"`
	Models      []ge.Model         `json:"models"`
	CheckedAt   time.Time          `json:"checked_at"`
}

func validEnterpriseCatalogSnapshot(snapshot EnterpriseCatalogSnapshot, auth *Auth) bool {
	if snapshot.Version != 1 || snapshot.Fingerprint != enterprisePolicyCatalogKey(auth) || snapshot.Resources != ge.Resources(ge.FromMetadata(auth.Metadata)) || snapshot.CheckedAt.IsZero() || snapshot.CheckedAt.After(time.Now().Add(time.Minute)) || snapshot.Models == nil || len(snapshot.Models) > 10000 {
		return false
	}
	seen := map[string]bool{}
	for _, model := range snapshot.Models {
		if model.ID == "" || len(model.ID) > 1024 || len(model.DisplayName) > 4096 || strings.ContainsAny(model.ID+model.DisplayName, "\x00\r\n") || seen[model.ID] {
			return false
		}
		seen[model.ID] = true
		if model.MediaKind != "" && !((model.ID == "gemini-3-pro-image" && model.MediaKind == "image") || (model.ID == "veo-3.1" && model.MediaKind == "video")) {
			return false
		}
	}
	return true
}

func readEnterpriseCatalogSnapshots(auth *Auth) map[string]EnterpriseCatalogSnapshot {
	out := map[string]EnterpriseCatalogSnapshot{}
	if auth != nil {
		data, err := json.Marshal(auth.Metadata[enterpriseCatalogCacheKey])
		if err == nil && len(data) <= 16*1024*1024 {
			_ = json.Unmarshal(data, &out)
		}
	}
	if out == nil {
		out = map[string]EnterpriseCatalogSnapshot{}
	}
	return out
}

// StoredEnterpriseCatalog reads already-loaded credential metadata without I/O.
func (m *Manager) StoredEnterpriseCatalog(auth *Auth) (EnterpriseCatalogSnapshot, bool) {
	if auth == nil || auth.Provider != ge.Provider {
		return EnterpriseCatalogSnapshot{}, false
	}
	m.mu.RLock()
	owner := m.auths[EnterpriseOwnerID(auth)]
	snapshot, ok := readEnterpriseCatalogSnapshots(owner)[enterprisePolicyCatalogKey(auth)]
	m.mu.RUnlock()
	if !ok || !validEnterpriseCatalogSnapshot(snapshot, auth) {
		return EnterpriseCatalogSnapshot{}, false
	}
	return snapshot, true
}

// PersistEnterpriseCatalog updates only server-owned metadata under the same
// mutation gate as settings and token refresh. Graph revisions remain untouched.
func (m *Manager) PersistEnterpriseCatalog(ctx context.Context, auth *Auth, models []ge.Model, checkedAt time.Time) error {
	if auth == nil || auth.Provider != ge.Provider {
		return ErrEnterpriseAccountChanged
	}
	ownerID := EnterpriseOwnerID(auth)
	release, err := m.lockAuthMutationContext(ctx, ownerID)
	if err != nil {
		return err
	}
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	current, owner := m.auths[auth.ID], m.auths[ownerID]
	if current == nil || owner == nil || current.Disabled || current.RegistrationEpoch != auth.RegistrationEpoch || enterprisePolicyCatalogKey(current) != enterprisePolicyCatalogKey(auth) {
		return ErrEnterpriseAccountChanged
	}
	snapshot := EnterpriseCatalogSnapshot{1, enterprisePolicyCatalogKey(auth), ge.Resources(ge.FromMetadata(auth.Metadata)), append([]ge.Model{}, models...), checkedAt.UTC()}
	if !validEnterpriseCatalogSnapshot(snapshot, current) {
		return fmt.Errorf("invalid Enterprise model catalog snapshot")
	}
	updated := owner.Clone()
	cache := readEnterpriseCatalogSnapshots(updated)
	if previous, ok := cache[snapshot.Fingerprint]; ok && previous.CheckedAt.After(snapshot.CheckedAt) {
		return nil
	}
	// Retain only entries for this authorization and an existing full binding.
	valid := map[string]bool{}
	for _, candidate := range m.auths {
		if candidate.Provider == ge.Provider && EnterpriseOwnerID(candidate) == ownerID && !IsEnterpriseAccountAuth(candidate) {
			valid[enterprisePolicyCatalogKey(candidate)] = true
		}
	}
	for key := range cache {
		if !valid[key] {
			delete(cache, key)
		}
	}
	cache[snapshot.Fingerprint] = snapshot
	data, err := json.Marshal(cache)
	if err != nil || len(data) > 16*1024*1024 {
		return fmt.Errorf("Enterprise model catalog cache is too large")
	}
	var stored map[string]any
	if json.Unmarshal(data, &stored) != nil {
		return fmt.Errorf("invalid Enterprise model catalog cache")
	}
	updated.Metadata[enterpriseCatalogCacheKey], updated.Storage = stored, nil
	before := owner.Clone()
	storeContext := ctx
	if account, errRead := ge.ReadAccount(owner.Metadata); errRead != nil {
		return errRead
	} else if account != nil {
		storeContext = withEnterpriseStoreRevision(ctx, account.Revision)
	}
	if err = m.persistLocked(storeContext, updated); err != nil {
		return fmt.Errorf("persist Enterprise model catalog: %w", err)
	}
	currentOwner := m.auths[ownerID]
	if currentOwner == nil {
		return ErrEnterpriseAccountChanged
	}
	mergeAuthSaveDelta(updated, before, currentOwner, false)
	updated.Generation = currentOwner.Generation + 1
	m.auths[ownerID] = updated
	return nil
}

func (m *Manager) PublishEnterpriseCatalogState(auth *Auth, state ge.CatalogState) {
	c := &m.enterprisePolicies
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogStates == nil {
		c.catalogStates = map[string]ge.CatalogState{}
	}
	c.catalogStates[enterprisePolicyCatalogKey(auth)] = cloneEnterpriseCatalogState(state)
}

func cloneEnterpriseCatalogState(state ge.CatalogState) ge.CatalogState {
	if state.CheckedAt != nil {
		value := *state.CheckedAt
		state.CheckedAt = &value
	}
	if state.LastSuccessAt != nil {
		value := *state.LastSuccessAt
		state.LastSuccessAt = &value
	}
	if state.LastError != nil {
		value := *state.LastError
		state.LastError = &value
	}
	return state
}
