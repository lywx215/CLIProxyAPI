package auth

import (
	"encoding/json"
	"strings"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func (m *Manager) reconcileEnterpriseCarbonMutationLocked(previous, incoming *Auth, mode updateAuthMode) {
	if previous == nil || incoming == nil || previous.Provider != ge.Provider || incoming.Provider != ge.Provider {
		return
	}
	before, after := ge.CarbonAuthorizationFingerprint(previous.Metadata), ge.CarbonAuthorizationFingerprint(incoming.Metadata)
	coordinator := &m.enterpriseCarbonEvidence
	coordinator.mu.Lock()
	if mode == updateModeRefresh && before != "" && after != "" && before != after && sameEnterprisePrincipal(previous, incoming) {
		if coordinator.authorizations == nil {
			coordinator.authorizations = map[string]string{}
		}
		coordinator.authorizations[EnterpriseOwnerID(previous)+"\x00"+before] = after
	} else if mode == updateModeEnterpriseReauthorize || before != after || !sameEnterprisePrincipal(previous, incoming) {
		if coordinator.authorizationVersions == nil {
			coordinator.authorizationVersions = map[string]uint64{}
		}
		coordinator.authorizationVersions[EnterpriseOwnerID(previous)]++
		prefix := EnterpriseOwnerID(previous) + "\x00"
		for key := range coordinator.tests {
			if strings.HasPrefix(key, prefix) {
				delete(coordinator.tests, key)
			}
		}
		for key := range coordinator.authorizations {
			if strings.HasPrefix(key, prefix) {
				delete(coordinator.authorizations, key)
			}
		}
	}
	coordinator.mu.Unlock()
	reconcileEnterpriseCarbonAuthorization(previous, incoming, mode)
}

func (m *Manager) sameCarbonAuthorizationLocked(ownerID, before, after string) bool {
	if before == "" || after == "" {
		return false
	}
	if before == after {
		return true
	}
	coordinator := &m.enterpriseCarbonEvidence
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for range 16 {
		next := coordinator.authorizations[ownerID+"\x00"+before]
		if next == "" || next == before {
			return false
		}
		if next == after {
			return true
		}
		before = next
	}
	return false
}

// reconcileEnterpriseCarbonAuthorization changes only Carbon-owned state. A
// trusted OAuth refresh may rotate refresh material without replacing the grant.
func reconcileEnterpriseCarbonAuthorization(previous, incoming *Auth, mode updateAuthMode) {
	if previous == nil || incoming == nil || previous.Provider != ge.Provider || incoming.Provider != ge.Provider {
		return
	}
	before := ge.CarbonAuthorizationFingerprint(previous.Metadata)
	after := ge.CarbonAuthorizationFingerprint(incoming.Metadata)
	if mode == updateModeRefresh && before != "" && after != "" && sameEnterprisePrincipal(previous, incoming) {
		if before != after {
			cache := ge.ReadCarbonEvidenceStore(previous.Metadata)
			for key, value := range cache {
				data, _ := json.Marshal(value)
				var evidence ge.CarbonTestEvidence
				if json.Unmarshal(data, &evidence) == nil && ge.ValidCarbonEvidence(&evidence) && evidence.AuthorizationFingerprint == before {
					evidence.AuthorizationFingerprint = after
					cache[key] = evidence
				}
			}
			if len(cache) > 0 {
				incoming.Metadata[ge.CarbonEvidenceStoreKey] = cache
			}
		}
		return
	}
	if mode == updateModeEnterpriseReauthorize || before != after || !sameEnterprisePrincipal(previous, incoming) {
		if !sameEnterprisePrincipal(previous, incoming) {
			resetEnterpriseCarbonAuthorization(incoming)
			return
		}
		// Reauthorizing the same account invalidates diagnostics, not the user's
		// application settings. Subscription eligibility is checked independently.
		clearEnterpriseCarbonEvidence(incoming)
	}
	prior, errPrior := ge.AccountFromLegacy(previous.Metadata)
	next, errNext := ge.ReadAccount(incoming.Metadata)
	if errPrior == nil && errNext == nil && next != nil {
		beforeIncoming := ge.CloneAccount(*next)
		if resetEnterpriseCarbonResourceChanges(incoming.Metadata, prior, next) {
			if err := advanceEnterpriseRevisions(beforeIncoming, next); err == nil {
				if next.Revision == beforeIncoming.Revision {
					next.Revision++
				}
				incoming.Metadata = ge.PutAccount(incoming.Metadata, *next)
			}
		}
	} else if ge.Resources(ge.FromMetadata(previous.Metadata)) != ge.Resources(ge.FromMetadata(incoming.Metadata)) {
		resetEnterpriseCarbonAuthorization(incoming)
	}
}

func clearEnterpriseCarbonEvidence(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, ge.CarbonEvidenceStoreKey)
	delete(auth.Metadata, ge.CarbonEvidenceKey)
}

func resetEnterpriseCarbonAuthorization(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	clearEnterpriseCarbonEvidence(auth)
	if _, exists := auth.Metadata[ge.CarbonEnabledKey]; exists {
		auth.Metadata[ge.CarbonEnabledKey] = false
	}
	if account, err := ge.ReadAccount(auth.Metadata); err == nil && account != nil {
		before := ge.CloneAccount(*account)
		for i := range account.Collections {
			if _, exists := account.Collections[i].Settings[ge.CarbonEnabledKey]; exists {
				account.Collections[i].Settings[ge.CarbonEnabledKey] = false
			}
		}
		_ = advanceEnterpriseRevisions(before, account)
		auth.Metadata = ge.PutAccount(auth.Metadata, *account)
	}
}

func resetEnterpriseCarbonResourceChanges(metadata map[string]any, previous ge.Account, next *ge.Account) bool {
	if next == nil {
		return false
	}
	prior := make(map[string]ge.Collection, len(previous.Collections))
	for _, collection := range previous.Collections {
		prior[collection.ID] = collection
	}
	cache := ge.ReadCarbonEvidenceStore(metadata)
	changed := false
	for i := range next.Collections {
		collection := &next.Collections[i]
		old, existed := prior[collection.ID]
		if !existed {
			for _, alias := range collection.LegacyAliases {
				if candidate, ok := prior[alias]; ok {
					old, existed = candidate, true
					break
				}
			}
		}
		if !existed || old.ResourceBinding == collection.ResourceBinding {
			continue
		}
		if value, exists := collection.Settings[ge.CarbonEnabledKey]; exists && value != false {
			collection.Settings[ge.CarbonEnabledKey] = false
			changed = true
		}
		for _, key := range []string{ge.CarbonBindingFingerprint(old.ResourceBinding.Apply(ge.FromMetadata(metadata))), ge.CarbonBindingFingerprint(collection.ResourceBinding.Apply(ge.FromMetadata(metadata)))} {
			if _, exists := cache[key]; exists {
				delete(cache, key)
				changed = true
			}
		}
	}
	if changed {
		metadata[ge.CarbonEvidenceStoreKey] = cache
	}
	return changed
}
