package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const aeOwnerAttribute = "antigravity_enterprise_owner"
const aeBindingAttribute = "antigravity_enterprise_binding_id"
const aeAuthorizationRevisionKey = "antigravity_enterprise_authorization_revision"

var ErrAntigravityEnterpriseAccountChanged = errors.New("antigravity-enterprise account changed; reload the credential")

type antigravityEnterpriseCoordinator struct {
	mu         sync.Mutex
	flights    map[string]*enterpriseAccountRefresh
	pools      map[string]*AntigravityEnterprisePoolState
	cooldowns  map[string]AntigravityEnterpriseBindingCooldown
	rateLimits map[string]*AntigravityEnterpriseRateLimitState
	version    uint64
	now        func() time.Time
}

func IsAntigravityEnterpriseAccountAuth(a *Auth) bool {
	if a == nil || a.Provider != ae.Provider || IsAntigravityEnterpriseBindingAuth(a) {
		return false
	}
	_, ok := a.Metadata[ae.AccountKey]
	return ok
}
func IsAntigravityEnterpriseBindingAuth(a *Auth) bool {
	return a != nil && a.Provider == ae.Provider && a.Attributes[aeOwnerAttribute] != "" && a.Attributes[aeBindingAttribute] != ""
}

// IsBoundAccountAuth is provider-neutral; provider-specific graph predicates stay strict.
func IsBoundAccountAuth(a *Auth) bool {
	return IsEnterpriseAccountAuth(a) || IsAntigravityEnterpriseAccountAuth(a)
}
func IsBoundChildAuth(a *Auth) bool {
	return IsEnterpriseCollectionAuth(a) || IsAntigravityEnterpriseBindingAuth(a)
}
func BoundOwnerID(a *Auth) string {
	if a == nil {
		return ""
	}
	if IsAntigravityEnterpriseBindingAuth(a) {
		return a.Attributes[aeOwnerAttribute]
	}
	if a.Provider == ae.Provider {
		return a.ID
	}
	return EnterpriseOwnerID(a)
}
func AntigravityEnterpriseLicense(a *Auth) ae.License {
	if a == nil {
		return ae.License{}
	}
	return ae.License{ProjectID: ae.String(a.Metadata, "project_id"), Location: ae.String(a.Metadata, "location"), UserTier: ae.String(a.Metadata, "user_tier"), ProcessingLocation: ae.String(a.Metadata, "processing_location"), TierDisplayName: ae.String(a.Metadata, "tier_display_name")}
}
func aeChildID(accountID, bindingID string) string {
	return ae.Provider + "-binding:" + accountID + ":" + bindingID
}

func projectAntigravityEnterpriseAuth(auth *Auth) ([]*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.Provider != ae.Provider || IsAntigravityEnterpriseBindingAuth(auth) {
		return []*Auth{auth.Clone()}, nil
	}
	account, err := ae.ReadAccount(auth.Metadata)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, fmt.Errorf("%s: account graph required", ae.Provider)
	}
	parent := auth.Clone()
	if parent.Attributes == nil {
		parent.Attributes = map[string]string{}
	}
	parent.Attributes[AttributeAuthIndexSeed] = ae.Provider + "/account/" + account.ID
	parent.Index, parent.indexAssigned = "", false
	parent.EnsureIndex()
	out := []*Auth{parent}
	for _, binding := range account.Bindings {
		child := parent.Clone()
		child.ID = aeChildID(account.ID, binding.ID)
		child.Storage = nil
		delete(child.Metadata, ae.AccountKey)
		child.Prefix, child.ProxyURL = "", ""
		for k := range child.Metadata {
			if aeBindingSetting(CanonicalCredentialMetadataKey(k)) {
				delete(child.Metadata, k)
			}
		}
		for k := range child.Attributes {
			if aeBindingSetting(k) || k == AttributeFilePriority || k == oauthModelAliasesAttributeKey || strings.HasPrefix(k, "header:") {
				delete(child.Attributes, k)
			}
		}
		child.Attributes[aeOwnerAttribute] = parent.ID
		child.Attributes[aeBindingAttribute] = binding.ID
		child.Attributes[AttributeAuthIndexSeed] = ae.Provider + "/binding/" + account.ID + "/" + binding.ID
		child.Attributes["runtime_only"] = "true"
		child.Metadata["antigravity_enterprise_account_id"] = account.ID
		child.Metadata["antigravity_enterprise_binding_id"] = binding.ID
		child.Metadata["antigravity_enterprise_binding_revision"] = binding.Revision
		child.Metadata["project_id"] = binding.License.ProjectID
		child.Metadata["location"] = binding.License.Location
		child.Metadata["user_tier"] = binding.License.UserTier
		child.Metadata["processing_location"] = binding.License.ProcessingLocation
		child.Metadata["tier_display_name"] = binding.License.TierDisplayName
		for k, v := range binding.Settings {
			if aeBindingSetting(CanonicalCredentialMetadataKey(k)) {
				child.Metadata[CanonicalCredentialMetadataKey(k)] = cloneAuthMetadataValue(v)
			}
		}
		if err = applyAntigravityEnterpriseSettings(child); err != nil {
			return nil, err
		}
		child.Disabled = parent.Disabled || parent.Status == StatusDisabled || binding.Disabled || binding.Missing
		if child.Disabled {
			child.Status = StatusDisabled
		} else {
			child.Status = StatusActive
		}
		child.Success, child.Failed, child.recentRequests = 0, 0, recentRequestRing{}
		child.ModelStates, child.LastError = nil, nil
		child.Unavailable = false
		child.NextRetryAfter = time.Time{}
		child.Quota = QuotaState{}
		child.Index, child.indexAssigned = "", false
		child.EnsureIndex()
		out = append(out, child)
	}
	return out, nil
}
func aeBindingSetting(key string) bool {
	switch key {
	case "prefix", "proxy_url", "note", "weight", "credential_weight", "priority", "allowed_models", "excluded_models", "model_aliases", "oauth_model_alias", "disable_cooling", "headers", "request_retry", "request_scoped_errors", "max_concurrent_requests":
		return true
	}
	return false
}
func applyAntigravityEnterpriseSettings(a *Auth) error {
	a.Prefix = strings.Trim(ae.String(a.Metadata, "prefix"), "/")
	a.ProxyURL = ae.String(a.Metadata, "proxy_url")
	ApplyAuthPriorityMetadata(a, a.Metadata)
	if err := ApplyAuthWeightMetadata(a, a.Metadata); err != nil {
		return err
	}
	ApplyCustomHeadersFromMetadata(a)
	for _, key := range []string{"allowed_models", "model_aliases", "oauth_model_alias"} {
		if value, ok := a.Metadata[key]; ok {
			data, err := json.Marshal(value)
			if err != nil {
				return err
			}
			target := key
			if key == "model_aliases" || key == "oauth_model_alias" {
				target = oauthModelAliasesAttributeKey
			}
			a.Attributes[target] = string(data)
		}
	}
	if raw, ok := a.Metadata["excluded_models"]; ok {
		data, _ := json.Marshal(raw)
		var models []string
		_ = json.Unmarshal(data, &models)
		a.Attributes["excluded_models"] = strings.Join(models, ",")
	}
	return nil
}

func (m *Manager) resolveAntigravityEnterpriseLocked(id string) *Auth {
	if a := m.auths[id]; a != nil && a.Provider == ae.Provider {
		return a
	}
	for _, a := range m.auths {
		if a != nil && a.Provider == ae.Provider && (a.Index == id || !IsAntigravityEnterpriseBindingAuth(a) && a.FileName == id) {
			return a
		}
	}
	return nil
}
func (m *Manager) AntigravityEnterpriseAccountSnapshot(id string) (*Auth, ae.Account, bool) {
	if m == nil {
		return nil, ae.Account{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.resolveAntigravityEnterpriseLocked(strings.TrimSpace(id))
	if a == nil {
		return nil, ae.Account{}, false
	}
	parent := m.auths[BoundOwnerID(a)]
	if !IsAntigravityEnterpriseAccountAuth(parent) {
		return nil, ae.Account{}, false
	}
	graph, err := ae.ReadAccount(parent.Metadata)
	if err != nil || graph == nil {
		return nil, ae.Account{}, false
	}
	return parent.Clone(), *graph, true
}
func (m *Manager) AntigravityEnterpriseBindings(id string) []*Auth {
	parent, graph, ok := m.AntigravityEnterpriseAccountSnapshot(id)
	if !ok {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(graph.Bindings))
	for _, b := range graph.Bindings {
		if a := m.auths[aeChildID(graph.ID, b.ID)]; a != nil && BoundOwnerID(a) == parent.ID {
			out = append(out, a.Clone())
		}
	}
	return out
}

func (m *Manager) reconcileAntigravityEnterpriseLocked(parent *Auth) []*Auth {
	if !IsAntigravityEnterpriseAccountAuth(parent) {
		return nil
	}
	projected, err := projectAntigravityEnterpriseAuth(parent)
	if err != nil {
		return nil
	}
	parent.Attributes, parent.Index, parent.indexAssigned = projected[0].Attributes, projected[0].Index, true
	if m.authEpochs == nil {
		m.authEpochs = map[string]uint64{}
	}
	keep := map[string]bool{}
	out := []*Auth{parent.Clone()}
	for _, child := range projected[1:] {
		keep[child.ID] = true
		old := m.auths[child.ID]
		if old != nil {
			child.Success, child.Failed, child.recentRequests = old.Success, old.Failed, old.recentRequests
			child.Generation, child.RegistrationEpoch, child.CredentialVersion = old.Generation+1, old.RegistrationEpoch, max(old.CredentialVersion, parent.CredentialVersion)
			if !reflect.DeepEqual(old.Metadata["antigravity_enterprise_binding_revision"], child.Metadata["antigravity_enterprise_binding_revision"]) || !reflect.DeepEqual(old.Metadata[aeAuthorizationRevisionKey], child.Metadata[aeAuthorizationRevisionKey]) {
				m.authEpochs[child.ID] = max(m.authEpochs[child.ID], old.RegistrationEpoch) + 1
				child.RegistrationEpoch = m.authEpochs[child.ID]
			}
			if AntigravityEnterpriseLicense(old) == AntigravityEnterpriseLicense(child) {
				copyOld := old.Clone()
				child.ModelStates, child.Quota = copyOld.ModelStates, copyOld.Quota
				child.LastError, child.NextRetryAfter, child.Unavailable = copyOld.LastError, copyOld.NextRetryAfter, copyOld.Unavailable
				if !child.Disabled {
					child.Status, child.StatusMessage = old.Status, old.StatusMessage
					if child.Status == StatusDisabled {
						child.Status = StatusActive
					}
				}
			}
		} else {
			m.authEpochs[child.ID]++
			child.RegistrationEpoch, child.Generation, child.CredentialVersion = m.authEpochs[child.ID], 1, parent.CredentialVersion
		}
		if hasUnauthorizedAuthFailure(parent) || hasDisabledInvalidGrantFailure(parent) {
			child.LastError, child.Unavailable, child.StatusMessage = parent.LastError, true, parent.StatusMessage
			if !child.Disabled {
				child.Status = StatusError
			}
		} else if old != nil && (CredentialsChanged(old, child) || !reflect.DeepEqual(old.Metadata[aeAuthorizationRevisionKey], child.Metadata[aeAuthorizationRevisionKey])) {
			child.RejectedAccessToken = ""
			clearUnauthorizedModelStates(child, time.Now())
			if isUnauthorizedError(child.LastError) || isInvalidGrantResultError(child.LastError) {
				child.LastError = nil
				child.Unavailable = false
				child.StatusMessage = ""
				child.NextRetryAfter = time.Time{}
				if !child.Disabled {
					child.Status = StatusActive
				}
			}
		}
		child.antigravityEnterpriseQuota = &antigravityEnterpriseGate{manager: m, authID: child.ID, epoch: child.RegistrationEpoch, pool: AntigravityEnterpriseLicense(child).PoolKey(), fingerprint: aeBindingFingerprint(child)}
		m.clearStaleAntigravityEnterpriseRateLimitLocked(child.ID, aeBindingFingerprint(child))
		m.auths[child.ID] = child
		m.notifyAuthChangeLocked(child.ID)
		out = append(out, child.Clone())
	}
	for id, old := range m.auths {
		if IsAntigravityEnterpriseBindingAuth(old) && BoundOwnerID(old) == parent.ID && !keep[id] {
			m.clearStaleAntigravityEnterpriseRateLimitLocked(id, "")
			next := old.Clone()
			next.Disabled, next.Status = true, StatusDisabled
			next.Generation++
			m.authEpochs[id] = max(m.authEpochs[id], old.RegistrationEpoch) + 1
			next.RegistrationEpoch = m.authEpochs[id]
			m.auths[id] = next
			m.notifyAuthChangeLocked(id)
			out = append(out, next)
		}
	}
	return out
}

// UpdateAntigravityEnterpriseAccount mutates the current graph under the same lock as OAuth saves.
func (m *Manager) UpdateAntigravityEnterpriseAccount(ctx context.Context, base *Auth, expected uint64, mutate func(*ae.Account) error) (*Auth, error) {
	if m == nil || base == nil || base.Provider != ae.Provider || mutate == nil || IsPluginVirtualAuth(base) {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	owner := BoundOwnerID(base)
	release, err := m.lockAuthMutationContext(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	m.mu.Lock()
	current := m.auths[owner]
	if !IsAntigravityEnterpriseAccountAuth(current) || base.RegistrationEpoch != 0 && !IsAntigravityEnterpriseBindingAuth(base) && current.RegistrationEpoch != base.RegistrationEpoch {
		m.mu.Unlock()
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	if IsAntigravityEnterpriseBindingAuth(base) {
		child := m.auths[base.ID]
		if !IsAntigravityEnterpriseBindingAuth(child) || BoundOwnerID(child) != owner || child.RegistrationEpoch != base.RegistrationEpoch || AntigravityEnterpriseLicense(child) != AntigravityEnterpriseLicense(base) {
			m.mu.Unlock()
			return nil, ErrAntigravityEnterpriseAccountChanged
		}
	}
	graph, err := ae.ReadAccount(current.Metadata)
	if err != nil || graph == nil || expected != 0 && expected != graph.Revision {
		m.mu.Unlock()
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	oldGraph, _ := ae.ReadAccount(current.Metadata)
	old := *oldGraph
	if err = mutate(graph); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if graph.ID != old.ID {
		m.mu.Unlock()
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	oldBindings := map[string]ae.Binding{}
	for _, b := range old.Bindings {
		oldBindings[b.ID] = b
	}
	for i := range graph.Bindings {
		b := &graph.Bindings[i]
		if prior, ok := oldBindings[b.ID]; ok {
			v := *b
			v.Revision = prior.Revision
			b.Revision = prior.Revision
			if !reflect.DeepEqual(v, prior) {
				b.Revision++
			}
		} else {
			b.Revision = 1
		}
	}
	graph.Revision = old.Revision + 1
	updated := current.Clone()
	updated.Metadata = ae.PutAccount(updated.Metadata, *graph)
	updated.Storage = nil
	updated.Generation++
	updated.UpdatedAt = time.Now().UTC()
	if _, err = projectAntigravityEnterpriseAuth(updated); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	before := current.Clone()
	if err = m.persistLocked(ctx, updated); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	mergeAuthSaveDelta(updated, before, current, false)
	m.auths[owner] = updated
	m.notifyAuthChangeLocked(owner)
	snapshots := m.reconcileAntigravityEnterpriseLocked(updated)
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

func installAntigravityEnterpriseAuthorization(current, incoming *Auth) (*Auth, error) {
	old, err := ae.ReadAccount(current.Metadata)
	if err != nil || old == nil {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	next, err := ae.ReadAccount(incoming.Metadata)
	if err != nil || next == nil || old.ID != next.ID {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	result := incoming.Clone()
	if next.Revision <= old.Revision {
		result.Metadata = ae.PutAccount(result.Metadata, *old)
	}
	if value, ok := current.Metadata[aeAuthorizationRevisionKey]; ok {
		if _, present := result.Metadata[aeAuthorizationRevisionKey]; !present {
			result.Metadata[aeAuthorizationRevisionKey] = value
		}
	}
	if ae.String(result.Metadata, "refresh_token") == "" {
		result.Metadata["refresh_token"] = ae.String(current.Metadata, "refresh_token")
	}
	return result, nil
}

func installAntigravityEnterpriseReauthorization(current, incoming *Auth) (*Auth, error) {
	graph, err := ae.ReadAccount(current.Metadata)
	if err != nil || graph == nil {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	result, err := installAntigravityEnterpriseAuthorization(current, incoming)
	if err != nil {
		return nil, err
	}
	result.Metadata = ae.PutAccount(result.Metadata, *graph)
	revision, _ := strconv.ParseUint(fmt.Sprint(current.Metadata[aeAuthorizationRevisionKey]), 10, 64)
	result.Metadata[aeAuthorizationRevisionKey] = strconv.FormatUint(revision+1, 10)
	result.Disabled = current.Disabled && !hasDisabledInvalidGrantFailure(current)
	result.LastError, result.Unavailable, result.StatusMessage = nil, false, ""
	result.NextRetryAfter, result.NextRefreshAfter = time.Time{}, time.Time{}
	result.RejectedAccessToken, result.RefreshFailures = "", 0
	result.Status = StatusActive
	if result.Disabled {
		result.Status = StatusDisabled
	}
	return result, nil
}

// ReauthorizeAntigravityEnterpriseAccount installs OAuth onto the live graph.
func (m *Manager) ReauthorizeAntigravityEnterpriseAccount(ctx context.Context, incoming *Auth) (*Auth, error) {
	if !IsAntigravityEnterpriseAccountAuth(incoming) {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	if _, ok := m.GetByID(incoming.ID); !ok {
		return m.Register(ctx, incoming)
	}
	return m.updateInternal(ctx, nil, incoming, updateModeAntigravityEnterpriseReauthorize)
}

func (m *Manager) EnsureAntigravityEnterpriseAccountToken(ctx context.Context, id string) (*Auth, error) {
	if m == nil {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	target := m.resolveAntigravityEnterpriseLocked(id).Clone()
	var parent *Auth
	if target != nil {
		parent = m.auths[BoundOwnerID(target)].Clone()
	}
	m.mu.RUnlock()
	if parent == nil {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	if hasUnauthorizedAuthFailure(parent) || hasDisabledInvalidGrantFailure(parent) {
		return nil, errors.New("antigravity-enterprise credential requires reauthorization")
	}
	if parent.HasValidAccessToken(time.Now().Add(30*time.Second)) && parent.RejectedAccessToken == "" {
		m.mu.RLock()
		pendingSave := m.enterpriseAccounts.pendingSaves[parent.ID]
		m.mu.RUnlock()
		if pendingSave {
			if err := m.retryAntigravityEnterpriseSave(ctx, parent.ID); err != nil {
				return nil, err
			}
		}
		return target, nil
	}
	return m.sharedAntigravityEnterpriseRefresh(ctx, target, "", target.RegistrationEpoch)
}

func (m *Manager) retryAntigravityEnterpriseSave(ctx context.Context, id string) error {
	release, err := m.lockAuthMutationContext(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enterpriseAccounts.pendingSaves[id] {
		return nil
	}
	current := m.auths[id]
	if !IsAntigravityEnterpriseAccountAuth(current) {
		return ErrAntigravityEnterpriseAccountChanged
	}
	updated, before := current.Clone(), current.Clone()
	updated.Generation++
	if err = m.persistLocked(ctx, updated); err != nil {
		return err
	}
	mergeAuthSaveDelta(updated, before, current, false)
	m.auths[id] = updated
	delete(m.enterpriseAccounts.pendingSaves, id)
	m.notifyAuthChangeLocked(id)
	return nil
}

// AntigravityEnterpriseResultError shares normal execution classification with fixed-binding tests.
func AntigravityEnterpriseResultError(err error) *Error { return resultErrorFromError(err) }

// Historical completions keep their original binding counters without changing its new health.
func (m *Manager) recordAntigravityEnterpriseHistoricalResult(ctx context.Context, result Result) bool {
	if m == nil || result.Provider != "antigravity-enterprise" || result.AuthID == "" {
		return false
	}
	m.mu.Lock()
	current := m.auths[result.AuthID]
	if !IsAntigravityEnterpriseBindingAuth(current) || !isStaleExecutionResult(result, current) {
		m.mu.Unlock()
		return false
	}
	current.recordRecentRequest(time.Now(), result.Success)
	if result.Success {
		current.Success++
	} else {
		current.Failed++
	}
	current.Generation++
	snapshot := current.Clone()
	m.notifyAuthChangeLocked(current.ID)
	m.mu.Unlock()
	m.hook.OnResult(ctx, result)
	m.publishErrorEvent(result, snapshot)
	return true
}
func (m *Manager) sharedAntigravityEnterpriseRefresh(ctx context.Context, target *Auth, failed string, epoch uint64) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	owner := BoundOwnerID(target)
	m.mu.RLock()
	current := m.auths[target.ID]
	parent := m.auths[owner].Clone()
	valid := current != nil && (epoch == 0 || current.RegistrationEpoch == epoch)
	m.mu.RUnlock()
	if !valid || parent == nil {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	c := &m.antigravityEnterprise
	c.mu.Lock()
	if c.flights == nil {
		c.flights = map[string]*enterpriseAccountRefresh{}
	}
	flight := c.flights[owner]
	if flight == nil {
		flight = &enterpriseAccountRefresh{done: make(chan struct{})}
		c.flights[owner] = flight
		refreshCtx := context.WithoutCancel(ctx)
		go func() {
			_, err := m.refreshAuthForRequestAtEpochDirect(refreshCtx, owner, failed, parent.RegistrationEpoch)
			m.mu.Lock()
			var snapshots []*Auth
			if latest := m.auths[owner]; latest != nil && latest.RegistrationEpoch == parent.RegistrationEpoch {
				snapshots = m.reconcileAntigravityEnterpriseLocked(latest)
			}
			m.mu.Unlock()
			m.publishEnterpriseSnapshots(snapshots)
			c.mu.Lock()
			flight.err = err
			delete(c.flights, owner)
			close(flight.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
	}
	if flight.err != nil {
		return nil, flight.err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	latest := m.auths[target.ID]
	if latest == nil || latest.RegistrationEpoch != target.RegistrationEpoch || BoundOwnerID(latest) != owner {
		return nil, ErrAntigravityEnterpriseAccountChanged
	}
	return latest.Clone(), nil
}

func (m *Manager) publishAntigravityEnterpriseQuota() {
	m.mu.RLock()
	var snapshots []*Auth
	for _, a := range m.auths {
		if IsAntigravityEnterpriseBindingAuth(a) {
			snapshots = append(snapshots, a.Clone())
		}
	}
	m.mu.RUnlock()
	for _, a := range snapshots {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(a)
		}
	}
	m.structuralEpoch.Add(1)
}

func (m *Manager) BoundAccountBindings(id string) []*Auth {
	if _, _, ok := m.AntigravityEnterpriseAccountSnapshot(id); ok {
		return m.AntigravityEnterpriseBindings(id)
	}
	return m.EnterpriseCollections(id)
}

func (a *Auth) antigravityEnterpriseBlocked(now time.Time) (bool, time.Time) {
	if a == nil || a.antigravityEnterpriseQuota == nil {
		return false, time.Time{}
	}
	return a.antigravityEnterpriseQuota.blocked(now)
}

// Keep the registry dependency here so graph publication remains provider-neutral.
func unregisterBoundParent(a *Auth) {
	if IsBoundAccountAuth(a) {
		registry.GetGlobalRegistry().UnregisterClient(a.ID)
	}
}
