package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	enterpriseOwnerAttribute = "enterprise_account_owner"
	enterpriseChildAttribute = "enterprise_collection_id"
	enterpriseRootAttribute  = "enterprise_account_parent"
	enterpriseRetiredKey     = "enterprise_retired_to"
)

var ErrEnterpriseAccountChanged = errors.New("gemini-enterprise account changed; reload the credential")

type enterpriseAccountRefresh struct {
	done chan struct{}
	err  error
}

type enterpriseAccountCoordinator struct {
	mu       sync.Mutex
	flights  map[string]*enterpriseAccountRefresh
	syncHook PostAuthHook
	// transfers is protected by Manager.mu, together with runtime counters.
	transfers    map[string]enterpriseLegacyTransfer
	pendingSaves map[string]bool
}

type enterpriseLegacyTransfer struct {
	sourceEpoch uint64
	targetID    string
	targetEpoch uint64
	binding     string
}

// SetEnterpriseAccountSyncHook lets the host synchronize model registrations
// after an account transaction. It runs after manager locks have been released.
func (m *Manager) SetEnterpriseAccountSyncHook(hook PostAuthHook) {
	m.enterpriseAccounts.mu.Lock()
	m.enterpriseAccounts.syncHook = hook
	m.enterpriseAccounts.mu.Unlock()
}

func (m *Manager) enterpriseAccountPublished(ctx context.Context, parent *Auth) {
	if !IsEnterpriseAccountAuth(parent) {
		return
	}
	m.enterpriseAccounts.mu.Lock()
	hook := m.enterpriseAccounts.syncHook
	m.enterpriseAccounts.mu.Unlock()
	if hook != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		_ = hook(context.WithoutCancel(ctx), parent.Clone())
	}
}

// IsEnterpriseAccountAuth identifies persisted account parents, which never infer.
func IsEnterpriseAccountAuth(auth *Auth) bool {
	if auth == nil || auth.Provider != ge.Provider || IsEnterpriseCollectionAuth(auth) {
		return false
	}
	_, present := auth.Metadata["enterprise_account"]
	return present
}

// IsEnterpriseCollectionAuth identifies an account's runtime-only projection.
func IsEnterpriseCollectionAuth(auth *Auth) bool {
	return auth != nil && auth.Provider == ge.Provider && auth.Attributes[enterpriseChildAttribute] != "" && auth.Attributes[enterpriseOwnerAttribute] != ""
}

// EnterpriseOwnerID returns the runtime parent ID, or the legacy record's own ID.
func EnterpriseOwnerID(auth *Auth) string {
	if auth == nil || auth.Provider != ge.Provider {
		return ""
	}
	if IsEnterpriseCollectionAuth(auth) {
		return auth.Attributes[enterpriseOwnerAttribute]
	}
	return auth.ID
}

func EnterpriseBindingID(auth *Auth) string {
	if auth == nil || auth.Provider != ge.Provider {
		return ""
	}
	if IsEnterpriseCollectionAuth(auth) {
		return auth.Attributes[enterpriseChildAttribute]
	}
	if IsEnterpriseAccountAuth(auth) {
		return ""
	}
	return ge.CollectionID(ge.Resources(ge.FromMetadata(auth.Metadata)))
}

func enterpriseRetired(auth *Auth) bool {
	return auth != nil && auth.Provider == ge.Provider && ge.String(auth.Metadata, enterpriseRetiredKey) != ""
}

// IsEnterpriseRetiredAuth identifies a recoverable source superseded by a parent.
func IsEnterpriseRetiredAuth(auth *Auth) bool { return enterpriseRetired(auth) }

func enterpriseChildID(accountID, bindingID string) string {
	return "enterprise-collection:" + accountID + ":" + bindingID
}

// ProjectEnterpriseAuth expands a saved parent into itself and all its children.
// Legacy files remain single inference records until explicit consolidation.
func ProjectEnterpriseAuth(auth *Auth) ([]*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.Provider != ge.Provider || IsEnterpriseCollectionAuth(auth) {
		return []*Auth{auth.Clone()}, nil
	}
	parent := auth.Clone()
	if enterpriseRetired(parent) {
		parent.Disabled, parent.Status = true, StatusDisabled
		return []*Auth{parent}, nil
	}
	account, err := ge.ReadAccount(parent.Metadata)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return []*Auth{parent}, nil
	}
	if parent.Attributes == nil {
		parent.Attributes = map[string]string{}
	}
	parent.Attributes[enterpriseRootAttribute] = "true"
	parent.Attributes[AttributeAuthIndexSeed] = "gemini-enterprise/account/" + account.ID
	parent.Index, parent.indexAssigned = "", false
	parent.EnsureIndex()
	out := []*Auth{parent}
	template := parent.Clone()
	template.Storage = nil
	delete(template.Metadata, "enterprise_account")
	seen := make(map[string]bool, len(account.Collections))
	for _, collection := range account.Collections {
		if collection.ID == "" || seen[collection.ID] {
			return nil, fmt.Errorf("gemini-enterprise: invalid or duplicate collection identity")
		}
		seen[collection.ID] = true
		child := template.Clone()
		child.ID = enterpriseChildID(account.ID, collection.ID)
		child.Storage = nil
		delete(child.Metadata, "enterprise_account")
		delete(child.Metadata, enterpriseCatalogCacheKey)
		// Old single-binding fields remain on the saved parent for compatibility,
		// but only their original collection may inherit those user preferences.
		child.Prefix, child.ProxyURL = "", ""
		for key := range child.Metadata {
			if enterpriseCollectionSetting(CanonicalCredentialMetadataKey(key)) {
				delete(child.Metadata, key)
			}
		}
		for key := range child.Attributes {
			if enterpriseCollectionSetting(key) || key == AttributeFilePriority || key == "excluded_models_hash" || key == oauthModelAliasesAttributeKey || strings.HasPrefix(key, "header:") {
				delete(child.Attributes, key)
			}
		}
		child.Attributes[enterpriseOwnerAttribute] = parent.ID
		child.Attributes[enterpriseChildAttribute] = collection.ID
		delete(child.Attributes, enterpriseRootAttribute)
		child.Attributes[AttributeAuthIndexSeed] = "gemini-enterprise/collection/" + account.ID + "/" + collection.ID
		child.Attributes["runtime_only"] = "true"
		child.Metadata["enterprise_collection_revision"] = collection.Revision
		child.Metadata["enterprise_account_id"] = account.ID
		child.Metadata["enterprise_collection_disabled"] = collection.Disabled
		for key, value := range collection.Settings {
			if enterpriseCollectionSetting(key) {
				child.Metadata[key] = cloneAuthMetadataValue(value)
			}
		}
		putEnterpriseResources(child, collection.ResourceBinding)
		if errSettings := applyEnterpriseCollectionSettings(child); errSettings != nil {
			return nil, errSettings
		}
		child.Disabled = parent.Disabled || parent.Status == StatusDisabled || collection.Disabled || collection.Missing
		if child.Disabled {
			child.Status = StatusDisabled
		} else if child.Status == StatusDisabled {
			child.Status = StatusActive
		}
		if collection.Missing {
			child.StatusMessage = "collection resource is missing"
		}
		child.Success, child.Failed, child.recentRequests = 0, 0, recentRequestRing{}
		child.ModelStates, child.LastError, child.EnterpriseQuotaIdentity, child.enterpriseQuota = nil, nil, nil, nil
		child.Quota, child.Unavailable, child.NextRetryAfter = QuotaState{}, false, time.Time{}
		child.Index, child.indexAssigned = "", false
		child.EnsureIndex()
		out = append(out, child)
	}
	return out, nil
}

func enterpriseCollectionSetting(key string) bool {
	switch key {
	case "prefix", "proxy_url", "note", "weight", "credential_weight", "priority", "catalog_fallback", "model_policy", "allowed_models", "excluded_models", "oauth_model_alias", "model_aliases", "disable_cooling", "headers", "request_retry", "request_scoped_errors", "max_concurrent_requests":
		return true
	}
	return false
}

func applyEnterpriseCollectionSettings(auth *Auth) error {
	if _, err := ge.CatalogFallbackSettings(auth.Metadata); err != nil {
		return err
	}
	if _, _, err := ge.ModelPolicySettings(auth.Metadata); err != nil {
		return err
	}
	for _, key := range []string{"catalog_fallback", "model_policy", "allowed_models"} {
		if value, exists := auth.Metadata[key]; exists {
			data, _ := json.Marshal(value)
			auth.Attributes[key] = string(data)
		}
	}
	if value, exists := auth.Metadata["prefix"].(string); exists {
		auth.Prefix = strings.Trim(strings.TrimSpace(value), "/")
	}
	if value, exists := auth.Metadata["proxy_url"].(string); exists {
		auth.ProxyURL = strings.TrimSpace(value)
	}
	ApplyAuthPriorityMetadata(auth, auth.Metadata)
	if errWeight := ApplyAuthWeightMetadata(auth, auth.Metadata); errWeight != nil {
		return errWeight
	}
	ApplyCustomHeadersFromMetadata(auth)
	if value, exists := auth.Metadata["note"].(string); exists {
		auth.Attributes["note"] = value
	}
	if raw, exists := auth.Metadata["excluded_models"]; exists {
		var models []string
		switch value := raw.(type) {
		case []string:
			models = value
		case []any:
			for _, entry := range value {
				if text, ok := entry.(string); ok {
					models = append(models, text)
				}
			}
		case string:
			models = strings.Split(value, ",")
		}
		auth.Attributes["excluded_models"] = strings.Join(models, ",")
	}
	raw, exists := auth.Metadata["model_aliases"]
	if !exists {
		raw, exists = auth.Metadata["oauth_model_alias"]
	}
	if exists {
		if encoded, err := json.Marshal(raw); err == nil {
			auth.Attributes[oauthModelAliasesAttributeKey] = string(encoded)
		}
	}
	return nil
}

func cloneAuthMetadataValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneAuthMetadataValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneAuthMetadataValue(item)
		}
		return out
	case []string:
		if v == nil {
			return []string(nil)
		}
		out := make([]string, len(v))
		copy(out, v)
		return out
	case map[string]string:
		out := make(map[string]string, len(v))
		for key, item := range v {
			out[key] = item
		}
		return out
	case ge.Account:
		return ge.CloneAccount(v)
	case *ge.Account:
		if v == nil {
			return (*ge.Account)(nil)
		}
		copyAccount := ge.CloneAccount(*v)
		return &copyAccount
	default:
		return value
	}
}

// resolveEnterpriseLocked accepts exact IDs, parent filenames, indices and saved
// legacy aliases. Exact parent IDs retain their administration meaning.
func (m *Manager) resolveEnterpriseLocked(id string) *Auth {
	if auth := m.auths[id]; auth != nil && auth.Provider == ge.Provider && !enterpriseRetired(auth) {
		return auth
	}
	for _, auth := range m.auths {
		if auth == nil || auth.Provider != ge.Provider || enterpriseRetired(auth) {
			continue
		}
		if auth.Index == id || (!IsEnterpriseCollectionAuth(auth) && auth.FileName == id) {
			return auth
		}
	}
	for _, parent := range m.auths {
		if !IsEnterpriseAccountAuth(parent) || enterpriseRetired(parent) {
			continue
		}
		account, err := ge.ReadAccount(parent.Metadata)
		if err != nil || account == nil {
			continue
		}
		for _, collection := range account.Collections {
			for _, alias := range collection.LegacyAliases {
				if alias == id {
					return m.auths[enterpriseChildID(account.ID, collection.ID)]
				}
			}
		}
	}
	if resolved := m.resolveEnterpriseInferenceIDLocked(id); resolved != id {
		return m.auths[resolved]
	}
	return nil
}

func (m *Manager) EnterpriseAccountSnapshot(authID string) (*Auth, ge.Account, bool) {
	if m == nil {
		return nil, ge.Account{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	target := m.resolveEnterpriseLocked(strings.TrimSpace(authID))
	if target == nil {
		return nil, ge.Account{}, false
	}
	parent := m.auths[EnterpriseOwnerID(target)]
	if parent == nil || enterpriseRetired(parent) {
		return nil, ge.Account{}, false
	}
	account, err := ge.ReadAccount(parent.Metadata)
	if err != nil {
		return nil, ge.Account{}, false
	}
	if account == nil {
		legacy, errLegacy := ge.AccountFromLegacy(parent.Metadata)
		if errLegacy != nil {
			return nil, ge.Account{}, false
		}
		account = &legacy
	}
	return parent.Clone(), ge.CloneAccount(*account), true
}

func (m *Manager) EnterpriseCollections(authID string) []*Auth {
	parent, account, ok := m.EnterpriseAccountSnapshot(authID)
	if !ok {
		return nil
	}
	if !IsEnterpriseAccountAuth(parent) {
		return []*Auth{parent}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(account.Collections))
	for _, collection := range account.Collections {
		if child := m.auths[enterpriseChildID(account.ID, collection.ID)]; child != nil && EnterpriseOwnerID(child) == parent.ID {
			out = append(out, child.Clone())
		}
	}
	return out
}

// reconcileEnterpriseAccountLocked publishes every child without resetting its
// counters or cooldowns. A changed application advances the registration epoch.
func (m *Manager) reconcileEnterpriseAccountLocked(parent *Auth) []*Auth {
	if !IsEnterpriseAccountAuth(parent) || enterpriseRetired(parent) {
		return nil
	}
	projected, err := ProjectEnterpriseAuth(parent)
	if err != nil {
		return nil
	}
	parent.Attributes, parent.Index, parent.indexAssigned = projected[0].Attributes, projected[0].Index, true
	account, _ := ge.ReadAccount(parent.Metadata)
	collections := make(map[string]ge.Collection, len(account.Collections))
	for _, collection := range account.Collections {
		collections[collection.ID] = collection
	}
	snapshots := []*Auth{parent.Clone()}
	keep := make(map[string]bool)
	for _, child := range projected[1:] {
		m.applyEnterpriseCollectionConfig(child)
		keep[child.ID] = true
		existing := m.auths[child.ID]
		var migrationSource *Auth
		if existing == nil {
			for _, alias := range collections[EnterpriseBindingID(child)].LegacyAliases {
				candidate := m.auths[enterpriseChildID(account.ID, alias)]
				if IsEnterpriseCollectionAuth(candidate) && EnterpriseOwnerID(candidate) == parent.ID {
					migrationSource = candidate
					break
				}
			}
		}
		if existing != nil && EnterpriseOwnerID(existing) != parent.ID && !containsEnterpriseID(account.MergedSources, EnterpriseOwnerID(existing)) {
			continue
		}
		if existing != nil {
			child.Success, child.Failed, child.recentRequests = existing.Success, existing.Failed, existing.recentRequests
			child.Generation = existing.Generation + 1
			child.RegistrationEpoch = existing.RegistrationEpoch
			child.CredentialVersion = max(existing.CredentialVersion, parent.CredentialVersion)
			if CredentialsChanged(existing, child) {
				child.CredentialVersion = max(existing.CredentialVersion+1, parent.CredentialVersion)
			}
			if ge.Binding(ge.FromMetadata(child.Metadata)) == ge.Binding(ge.FromMetadata(existing.Metadata)) {
				child.ModelStates, child.Quota = existing.Clone().ModelStates, existing.Quota.Clone()
				child.LastError, child.Unavailable, child.NextRetryAfter = existing.LastError, existing.Unavailable, existing.NextRetryAfter
				child.EnterpriseQuotaIdentity = existing.EnterpriseQuotaIdentity
				child.RejectedAccessToken = existing.RejectedAccessToken
				if !child.Disabled {
					child.Status, child.StatusMessage = existing.Status, existing.StatusMessage
					if child.Status == StatusDisabled {
						child.Status, child.StatusMessage = StatusActive, ""
					}
				}
			} else {
				m.authEpochs[child.ID] = max(m.authEpochs[child.ID], existing.RegistrationEpoch) + 1
				child.RegistrationEpoch = m.authEpochs[child.ID]
			}
		} else {
			m.authEpochs[child.ID]++
			child.RegistrationEpoch, child.Generation = m.authEpochs[child.ID], 1
			child.CredentialVersion = parent.CredentialVersion
		}
		if hasUnauthorizedAuthFailure(parent) || hasDisabledInvalidGrantFailure(parent) {
			child.LastError, child.Unavailable, child.StatusMessage = parent.LastError, true, parent.StatusMessage
			if !child.Disabled {
				child.Status = StatusError
			}
		}
		if existing != nil && CredentialsChanged(existing, child) && !hasUnauthorizedAuthFailure(parent) {
			child.RejectedAccessToken = ""
			clearUnauthorizedModelStates(child, time.Now())
			if hasUnauthorizedAuthFailure(child) || hasDisabledInvalidGrantFailure(child) {
				child.LastError, child.Unavailable, child.StatusMessage = nil, false, ""
				if child.Disabled {
					child.Status = StatusDisabled
				} else {
					child.Status = StatusActive
				}
			}
		}
		m.attachEnterpriseQuotaLocked(child)
		m.auths[child.ID] = child
		if migrationSource != nil {
			m.transferEnterpriseLegacyStateLocked(migrationSource, parent, &snapshots)
		}
		m.notifyAuthChangeLocked(child.ID)
		snapshots = append(snapshots, child.Clone())
	}
	for id, child := range m.auths {
		if !IsEnterpriseCollectionAuth(child) || EnterpriseOwnerID(child) != parent.ID || keep[id] {
			continue
		}
		// Keep a disabled tombstone so an already running request keeps its owner.
		child = child.Clone()
		child.Disabled, child.Status, child.Generation = true, StatusDisabled, child.Generation+1
		m.authEpochs[id] = max(m.authEpochs[id], child.RegistrationEpoch) + 1
		child.RegistrationEpoch = m.authEpochs[id]
		m.auths[id] = child
		m.notifyAuthChangeLocked(id)
		snapshots = append(snapshots, child.Clone())
	}
	return snapshots
}

func (m *Manager) applyEnterpriseCollectionConfig(child *Auth) {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || len(cfg.OAuthExcludedModels[ge.Provider]) == 0 {
		return
	}
	seen := make(map[string]bool)
	for _, model := range append(strings.Split(child.Attributes["excluded_models"], ","), cfg.OAuthExcludedModels[ge.Provider]...) {
		if model = strings.ToLower(strings.TrimSpace(model)); model != "" {
			seen[model] = true
		}
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Strings(models)
	child.Attributes["excluded_models"] = strings.Join(models, ",")
}

func (m *Manager) publishEnterpriseSnapshots(snapshots []*Auth) {
	for _, snapshot := range snapshots {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
		if IsEnterpriseCollectionAuth(snapshot) || enterpriseRetired(snapshot) {
			m.queueRefreshUnschedule(snapshot.ID)
		} else {
			m.queueRefreshReschedule(snapshot.ID)
		}
		if IsEnterpriseAccountAuth(snapshot) || snapshot.Disabled {
			registry.GetGlobalRegistry().UnregisterClient(snapshot.ID)
		}
	}
	if len(snapshots) > 0 {
		m.structuralEpoch.Add(1)
	}
}

// A saved target is authoritative even if saving an old source's retirement
// failed. This also prevents duplicate scheduling after a process restart.
func (m *Manager) suppressMergedEnterpriseSourcesLocked(parent *Auth) {
	account, err := ge.ReadAccount(parent.Metadata)
	if err != nil || account == nil {
		return
	}
	for _, sourceID := range account.MergedSources {
		if sourceID == parent.ID {
			continue
		}
		if source := m.auths[sourceID]; source != nil && source.Provider == ge.Provider && sameEnterprisePrincipal(parent, source) {
			retired := source.Clone()
			retired.Disabled, retired.Status = true, StatusDisabled
			retired.Metadata[enterpriseRetiredKey] = parent.ID
			m.auths[sourceID] = retired
		}
	}
}

func (m *Manager) applyEnterpriseRetirementLocked(auth *Auth) {
	if auth == nil || auth.Provider != ge.Provider || IsEnterpriseCollectionAuth(auth) {
		return
	}
	for _, parent := range m.auths {
		if parent.ID == auth.ID || !IsEnterpriseAccountAuth(parent) || enterpriseRetired(parent) || !sameEnterprisePrincipal(parent, auth) {
			continue
		}
		account, err := ge.ReadAccount(parent.Metadata)
		if err == nil && account != nil && containsEnterpriseID(account.MergedSources, auth.ID) {
			auth.Disabled, auth.Status = true, StatusDisabled
			auth.Metadata[enterpriseRetiredKey] = parent.ID
			return
		}
	}
}

// ResolveEnterpriseInferenceID resolves only explicit legacy collection aliases.
// A new account index is never silently assigned to an arbitrary collection.
func (m *Manager) ResolveEnterpriseInferenceID(id string) string {
	if m == nil || id == "" {
		return id
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.resolveEnterpriseInferenceIDLocked(id)
}

func (m *Manager) resolveEnterpriseInferenceIDLocked(id string) string {
	if id == "" {
		return id
	}
	if current := m.auths[id]; current != nil && current.Provider != ge.Provider {
		return id
	}
	for _, parent := range m.auths {
		if !IsEnterpriseAccountAuth(parent) || enterpriseRetired(parent) {
			continue
		}
		account, err := ge.ReadAccount(parent.Metadata)
		if err != nil || account == nil {
			continue
		}
		for _, collection := range account.Collections {
			childID := enterpriseChildID(account.ID, collection.ID)
			if child := m.auths[childID]; child != nil && child.Index == id {
				return childID
			}
			for _, alias := range collection.LegacyAliases {
				if alias == id || enterpriseChildID(account.ID, alias) == id || stableAuthIndex(AttributeAuthIndexSeed+":gemini-enterprise/collection/"+account.ID+"/"+alias) == id {
					return childID
				}
			}
		}
	}
	return id
}

func addEnterpriseAliases(collection *ge.Collection, values ...string) {
	seen := make(map[string]bool)
	for _, value := range collection.LegacyAliases {
		seen[value] = true
	}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			collection.LegacyAliases = append(collection.LegacyAliases, value)
			seen[value] = true
		}
	}
}

func enterpriseAccountForAuth(auth *Auth) (ge.Account, error) {
	account, err := ge.ReadAccount(auth.Metadata)
	if err != nil {
		return ge.Account{}, err
	}
	if account == nil {
		legacy, errLegacy := ge.AccountFromLegacy(auth.Metadata)
		if errLegacy != nil {
			return ge.Account{}, errLegacy
		}
		for i := range legacy.Collections {
			collection := &legacy.Collections[i]
			if collection.Settings == nil {
				collection.Settings = make(map[string]any)
			}
			for key, value := range auth.Metadata {
				if enterpriseCollectionSetting(CanonicalCredentialMetadataKey(key)) {
					collection.Settings[CanonicalCredentialMetadataKey(key)] = cloneAuthMetadataValue(value)
				}
			}
			collection.Settings["prefix"], collection.Settings["proxy_url"] = auth.Prefix, auth.ProxyURL
			if auth.Prefix == "" && ge.String(auth.Metadata, "prefix") != "" {
				collection.Settings["prefix"] = ge.String(auth.Metadata, "prefix")
			}
			if auth.ProxyURL == "" && ge.String(auth.Metadata, "proxy_url") != "" {
				collection.Settings["proxy_url"] = ge.String(auth.Metadata, "proxy_url")
			}
			addEnterpriseAliases(&legacy.Collections[i], auth.ID, auth.EnsureIndex())
		}
		return legacy, nil
	}
	return ge.CloneAccount(*account), nil
}

// UpdateEnterpriseAccount rebases graph changes on current OAuth and user state.
func (m *Manager) UpdateEnterpriseAccount(ctx context.Context, base *Auth, expectedRevision uint64, mutate func(*ge.Account) error) (*Auth, error) {
	if m == nil || base == nil || base.Provider != ge.Provider || IsPluginVirtualAuth(base) || mutate == nil {
		return nil, ErrEnterpriseAccountChanged
	}
	parentID := EnterpriseOwnerID(base)
	release, err := m.lockAuthMutationContext(ctx, parentID)
	if err != nil {
		return nil, err
	}
	defer release()
	m.mu.Lock()
	current := m.auths[parentID]
	if current == nil || current.Provider != ge.Provider || enterpriseRetired(current) || (base.RegistrationEpoch != 0 && !IsEnterpriseCollectionAuth(base) && current.RegistrationEpoch != base.RegistrationEpoch) || !sameEnterprisePrincipal(base, current) {
		m.mu.Unlock()
		return nil, ErrEnterpriseAccountChanged
	}
	account, err := enterpriseAccountForAuth(current)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if expectedRevision != 0 && expectedRevision != account.Revision {
		m.mu.Unlock()
		return nil, ErrEnterpriseAccountChanged
	}
	next := ge.CloneAccount(account)
	if err = mutate(&next); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if next.ID != account.ID {
		m.mu.Unlock()
		return nil, ErrEnterpriseAccountChanged
	}
	if err = advanceEnterpriseRevisions(account, &next); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	updated := current.Clone()
	updated.Metadata = ge.PutAccount(updated.Metadata, next)
	updated.Storage = nil
	if _, errProject := ProjectEnterpriseAuth(updated); errProject != nil {
		m.mu.Unlock()
		return nil, errProject
	}
	updated.Generation, updated.UpdatedAt = current.Generation+1, time.Now()
	before := current.Clone()
	if err = m.persistLocked(withEnterpriseStoreRevision(ctx, account.Revision), updated); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("persist enterprise account: %w", err)
	}
	delete(m.enterpriseAccounts.pendingSaves, parentID)
	mergeAuthSaveDelta(updated, before, current, false)
	m.auths[parentID] = updated
	m.notifyAuthChangeLocked(parentID)
	snapshots := m.reconcileEnterpriseAccountLocked(updated)
	if !IsEnterpriseAccountAuth(current) {
		m.transferEnterpriseLegacyStateLocked(current, updated, &snapshots)
		updated.Success, updated.Failed, updated.recentRequests = 0, 0, recentRequestRing{}
	}
	result := updated.Clone()
	m.mu.Unlock()
	release()
	m.publishEnterpriseSnapshots(snapshots)
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.hook.OnAuthUpdated(ctx, result.Clone())
	m.enterpriseAccountPublished(ctx, result)
	m.persistCooldownStates(context.Background())
	return result, nil
}

func sameEnterprisePrincipal(a, b *Auth) bool {
	return a != nil && b != nil && strings.EqualFold(strings.TrimSpace(ge.String(a.Metadata, "email")), strings.TrimSpace(ge.String(b.Metadata, "email"))) && strings.TrimSpace(ge.String(a.Metadata, "client_id")) == strings.TrimSpace(ge.String(b.Metadata, "client_id"))
}

func advanceEnterpriseRevisions(previous ge.Account, next *ge.Account) error {
	old := make(map[string]ge.Collection, len(previous.Collections))
	for _, collection := range previous.Collections {
		old[collection.ID] = collection
	}
	seen := make(map[string]bool)
	for i := range next.Collections {
		collection := &next.Collections[i]
		if collection.ID == "" || collection.ID != ge.CollectionID(collection.ResourceBinding) || seen[collection.ID] {
			return fmt.Errorf("gemini-enterprise: invalid collection identity")
		}
		seen[collection.ID] = true
		if prior, exists := old[collection.ID]; exists {
			copyCollection := *collection
			copyCollection.Revision = prior.Revision
			collection.Revision = prior.Revision
			if !reflect.DeepEqual(prior, copyCollection) {
				collection.Revision++
			}
		} else {
			collection.Revision = max(collection.Revision, 1)
		}
	}
	next.Revision = previous.Revision
	copyNext := ge.CloneAccount(*next)
	copyNext.Revision = previous.Revision
	if !reflect.DeepEqual(previous, copyNext) {
		next.Revision++
	}
	next.Revision = max(next.Revision, 1)
	return nil
}

// MergeEnterpriseAccounts only accepts sources already verified by the caller.
// Save the target graph first, then retain disabled, recoverable source files.
func (m *Manager) MergeEnterpriseAccounts(ctx context.Context, targetID string, verifiedSourceIDs []string) (*Auth, []string, error) {
	base, _, ok := m.EnterpriseAccountSnapshot(targetID)
	if !ok {
		return nil, nil, ErrEnterpriseAccountChanged
	}
	ids := append([]string{base.ID}, verifiedSourceIDs...)
	sort.Strings(ids)
	ids = compactEnterpriseIDs(ids)
	sourceSnapshots := make(map[string]*Auth, len(verifiedSourceIDs))
	m.mu.RLock()
	for _, id := range ids {
		if id != base.ID {
			sourceSnapshots[id] = m.auths[id].Clone()
		}
	}
	m.mu.RUnlock()
	var releases []func()
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	for _, id := range ids {
		release, err := m.lockAuthMutationContext(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		releases = append(releases, release)
	}
	m.mu.Lock()
	current := m.auths[base.ID]
	if current == nil || current.RegistrationEpoch != base.RegistrationEpoch || !sameEnterprisePrincipal(current, base) || enterpriseRetired(current) {
		m.mu.Unlock()
		return nil, nil, ErrEnterpriseAccountChanged
	}
	account, err := enterpriseAccountForAuth(current)
	if err != nil {
		m.mu.Unlock()
		return nil, nil, err
	}
	previous := ge.CloneAccount(account)
	byBinding := make(map[string]int)
	for i, collection := range account.Collections {
		byBinding[collection.ID] = i
	}
	var sources []*Auth
	var merged []string
	for _, id := range ids {
		if id == current.ID {
			continue
		}
		source := m.auths[id]
		snapshot := sourceSnapshots[id]
		if source == nil || snapshot == nil || source.Provider != ge.Provider || IsPluginVirtualAuth(source) || source.RegistrationEpoch != snapshot.RegistrationEpoch || CredentialsChanged(snapshot, source) || IsEnterpriseCollectionAuth(source) || !sameEnterprisePrincipal(current, source) || (enterpriseRetired(source) && ge.String(source.Metadata, enterpriseRetiredKey) != current.ID) {
			m.mu.Unlock()
			return nil, nil, ErrEnterpriseAccountChanged
		}
		sourceAccount, errSource := enterpriseAccountForAuth(source)
		if errSource != nil || sourceAccount.ID != account.ID {
			m.mu.Unlock()
			return nil, nil, ErrEnterpriseAccountChanged
		}
		for _, collection := range sourceAccount.Collections {
			if !IsEnterpriseAccountAuth(source) {
				addEnterpriseAliases(&collection, source.ID, source.EnsureIndex())
				collection.Disabled = collection.Disabled || source.Disabled
			}
			if index, exists := byBinding[collection.ID]; exists {
				addEnterpriseAliases(&account.Collections[index], collection.LegacyAliases...)
			} else {
				byBinding[collection.ID] = len(account.Collections)
				account.Collections = append(account.Collections, collection)
			}
		}
		if !containsEnterpriseID(account.MergedSources, source.ID) {
			account.MergedSources = append(account.MergedSources, source.ID)
		}
		sources, merged = append(sources, source), append(merged, source.ID)
	}
	if err = advanceEnterpriseRevisions(previous, &account); err != nil {
		m.mu.Unlock()
		return nil, nil, err
	}
	updated := current.Clone()
	updated.Metadata, updated.Storage = ge.PutAccount(updated.Metadata, account), nil
	updated.Generation, updated.UpdatedAt = current.Generation+1, time.Now()
	before := current.Clone()
	if err = m.persistLocked(withEnterpriseStoreRevision(ctx, previous.Revision), updated); err != nil {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("persist merged enterprise account: %w", err)
	}
	delete(m.enterpriseAccounts.pendingSaves, updated.ID)
	mergeAuthSaveDelta(updated, before, current, false)
	m.auths[updated.ID] = updated
	snapshots := m.reconcileEnterpriseAccountLocked(updated)
	if !IsEnterpriseAccountAuth(current) {
		m.transferEnterpriseLegacyStateLocked(current, updated, &snapshots)
		updated.Success, updated.Failed, updated.recentRequests = 0, 0, recentRequestRing{}
	}
	var retireErrors []error
	for _, source := range sources {
		retired := source.Clone()
		retired.Storage = nil
		retired.Metadata[enterpriseRetiredKey] = updated.ID
		retired.Disabled, retired.Status = true, StatusDisabled
		retired.Generation++
		if errRetire := m.persistLocked(ctx, retired); errRetire != nil {
			retireErrors = append(retireErrors, fmt.Errorf("retire enterprise source %s: %w", source.ID, errRetire))
		}
		m.auths[source.ID] = retired
		m.notifyAuthChangeLocked(source.ID)
		snapshots = append(snapshots, retired.Clone())
		m.transferEnterpriseLegacyStateLocked(source, updated, &snapshots)
	}
	m.notifyAuthChangeLocked(updated.ID)
	result := updated.Clone()
	m.mu.Unlock()
	for _, release := range releases {
		release()
	}
	m.publishEnterpriseSnapshots(snapshots)
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.hook.OnAuthUpdated(ctx, result.Clone())
	m.enterpriseAccountPublished(ctx, result)
	m.persistCooldownStates(context.Background())
	return result, merged, errors.Join(retireErrors...)
}

func compactEnterpriseIDs(ids []string) []string {
	out := ids[:0]
	for _, id := range ids {
		if id != "" && (len(out) == 0 || out[len(out)-1] != id) {
			out = append(out, id)
		}
	}
	return out
}

func containsEnterpriseID(ids []string, id string) bool {
	for _, item := range ids {
		if item == id {
			return true
		}
	}
	return false
}

func (m *Manager) transferEnterpriseLegacyStateLocked(source, parent *Auth, snapshots *[]*Auth) {
	if IsEnterpriseAccountAuth(source) {
		return
	}
	account, _ := ge.ReadAccount(parent.Metadata)
	if account == nil {
		return
	}
	bindingID := EnterpriseBindingID(source)
	var child *Auth
	canonicalized := false
	for _, collection := range account.Collections {
		if collection.ID == bindingID {
			child = m.auths[enterpriseChildID(account.ID, bindingID)]
			break
		}
		if containsEnterpriseID(collection.LegacyAliases, bindingID) {
			child = m.auths[enterpriseChildID(account.ID, collection.ID)]
			canonicalized = true
		}
	}
	if child == nil || (!canonicalized && ge.Binding(ge.FromMetadata(child.Metadata)) != ge.Binding(ge.FromMetadata(source.Metadata))) {
		return
	}
	// A per-source marker makes retries idempotent, including partial retirement.
	if m.enterpriseAccounts.transfers == nil {
		m.enterpriseAccounts.transfers = make(map[string]enterpriseLegacyTransfer)
	}
	if previous, exists := m.enterpriseAccounts.transfers[source.ID]; exists && previous.sourceEpoch == source.RegistrationEpoch {
		return
	}
	m.enterpriseAccounts.transfers[source.ID] = enterpriseLegacyTransfer{sourceEpoch: source.RegistrationEpoch, targetID: child.ID, targetEpoch: child.RegistrationEpoch, binding: ge.Binding(ge.FromMetadata(child.Metadata))}
	child.Success += source.Success
	child.Failed += source.Failed
	for i, bucket := range source.recentRequests.buckets {
		current := &child.recentRequests.buckets[i]
		if bucket.bucketID > current.bucketID {
			*current = bucket
		} else if bucket.bucketID == current.bucketID {
			current.success += bucket.success
			current.failed += bucket.failed
		}
	}
	sourceResources, childResources := ge.Resources(ge.FromMetadata(source.Metadata)), ge.Resources(ge.FromMetadata(child.Metadata))
	if canonicalized {
		sourceResources.ProjectID = childResources.ProjectID
	}
	if sourceResources == childResources {
		if child.ModelStates == nil && len(source.ModelStates) > 0 {
			child.ModelStates = make(map[string]*ModelState, len(source.ModelStates))
		}
		for model, state := range source.ModelStates {
			if state != nil {
				child.ModelStates[model] = mergeModelState(child.ModelStates[model], state.Clone())
			}
		}
		if source.NextRetryAfter.After(child.NextRetryAfter) {
			child.NextRetryAfter, child.Unavailable, child.Quota = source.NextRetryAfter, source.Unavailable, source.Quota.Clone()
		}
	}
	*snapshots = append(*snapshots, child.Clone())
}

// recordEnterpriseHistoricalResult keeps completed legacy traffic with its
// original collection, without applying old credential errors to a new binding.
func (m *Manager) recordEnterpriseHistoricalResult(ctx context.Context, result Result) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	transfer, exists := m.enterpriseAccounts.transfers[result.AuthID]
	m.mu.RUnlock()
	if !exists || (result.RegistrationEpoch != 0 && result.RegistrationEpoch != transfer.sourceEpoch) {
		return false
	}
	release := m.lockAuthMutation(transfer.targetID)
	defer release()
	m.mu.Lock()
	var snapshot *Auth
	if child := m.auths[transfer.targetID]; child != nil && child.RegistrationEpoch == transfer.targetEpoch && ge.Binding(ge.FromMetadata(child.Metadata)) == transfer.binding {
		now := time.Now()
		child.recordRecentRequest(now, result.Success)
		if result.Success {
			child.Success++
		} else {
			child.Failed++
		}
		child.Generation++
		snapshot = child.Clone()
		result.AuthID = child.ID
	}
	m.mu.Unlock()
	release()
	m.hook.OnResult(ctx, result)
	m.publishErrorEvent(result, snapshot)
	return true
}
