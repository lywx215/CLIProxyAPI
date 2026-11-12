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

// reconcileEnterpriseAdmissionAuthorization changes only the target model's state. A
// trusted OAuth refresh may rotate refresh material without replacing the grant.
func reconcileEnterpriseAdmissionAuthorization(previous, incoming *Auth, mode updateAuthMode, model ge.AdmissionModel) {
	if previous == nil || incoming == nil || previous.Provider != ge.Provider || incoming.Provider != ge.Provider {
		return
	}
	before := ge.CarbonAuthorizationFingerprint(previous.Metadata)
	after := ge.CarbonAuthorizationFingerprint(incoming.Metadata)
	if mode == updateModeRefresh && before != "" && after != "" && sameEnterprisePrincipal(previous, incoming) {
		if before != after {
			cache := model.ReadEvidenceStore(previous.Metadata)
			for key, value := range cache {
				data, _ := json.Marshal(value)
				var evidence ge.CarbonTestEvidence
				if json.Unmarshal(data, &evidence) == nil && model.ValidEvidence(&evidence) && evidence.AuthorizationFingerprint == before {
					evidence.AuthorizationFingerprint = after
					cache[key] = evidence
				}
			}
			if len(cache) > 0 {
				incoming.Metadata[model.EvidenceStoreKey] = cache
			}
		}
		return
	}
	if mode == updateModeEnterpriseReauthorize || before != after || !sameEnterprisePrincipal(previous, incoming) {
		if !sameEnterprisePrincipal(previous, incoming) {
			resetEnterpriseAdmissionAuthorization(incoming, model)
			return
		}
		// Reauthorizing the same account invalidates diagnostics, not the user's
		// application settings. Subscription eligibility is checked independently.
		clearEnterpriseAdmissionEvidence(incoming, model)
	}
	prior, errPrior := ge.AccountFromLegacy(previous.Metadata)
	next, errNext := ge.ReadAccount(incoming.Metadata)
	if errPrior == nil && errNext == nil && next != nil {
		beforeIncoming := ge.CloneAccount(*next)
		if resetEnterpriseAdmissionResourceChanges(incoming.Metadata, prior, next, model) {
			if err := advanceEnterpriseRevisions(beforeIncoming, next); err == nil {
				if next.Revision == beforeIncoming.Revision {
					next.Revision++
				}
				incoming.Metadata = ge.PutAccount(incoming.Metadata, *next)
			}
		}
	} else if ge.Resources(ge.FromMetadata(previous.Metadata)) != ge.Resources(ge.FromMetadata(incoming.Metadata)) {
		resetEnterpriseAdmissionAuthorization(incoming, model)
	}
}

func clearEnterpriseAdmissionEvidence(auth *Auth, model ge.AdmissionModel) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, model.EvidenceStoreKey)
	delete(auth.Metadata, model.EvidenceKey)
}

func resetEnterpriseAdmissionAuthorization(auth *Auth, model ge.AdmissionModel) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	clearEnterpriseAdmissionEvidence(auth, model)
	if _, exists := auth.Metadata[model.EnabledKey]; exists {
		auth.Metadata[model.EnabledKey] = false
	}
	if account, err := ge.ReadAccount(auth.Metadata); err == nil && account != nil {
		before := ge.CloneAccount(*account)
		for i := range account.Collections {
			if _, exists := account.Collections[i].Settings[model.EnabledKey]; exists {
				account.Collections[i].Settings[model.EnabledKey] = false
			}
		}
		_ = advanceEnterpriseRevisions(before, account)
		auth.Metadata = ge.PutAccount(auth.Metadata, *account)
	}
}

func resetEnterpriseAdmissionResourceChanges(metadata map[string]any, previous ge.Account, next *ge.Account, model ge.AdmissionModel) bool {
	if next == nil {
		return false
	}
	prior := make(map[string]ge.Collection, len(previous.Collections))
	for _, collection := range previous.Collections {
		prior[collection.ID] = collection
	}
	cache := model.ReadEvidenceStore(metadata)
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
		if value, exists := collection.Settings[model.EnabledKey]; exists && value != false {
			collection.Settings[model.EnabledKey] = false
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
		metadata[model.EvidenceStoreKey] = cache
	}
	return changed
}

func reconcileEnterpriseCarbonAuthorization(previous, incoming *Auth, mode updateAuthMode) {
	for _, model := range ge.AdmissionModels() {
		reconcileEnterpriseAdmissionAuthorization(previous, incoming, mode, model)
	}
}
func clearEnterpriseCarbonEvidence(auth *Auth) {
	clearEnterpriseAdmissionEvidence(auth, ge.CarbonAdmissionModel)
}
func resetEnterpriseCarbonAuthorization(auth *Auth) {
	resetEnterpriseAdmissionAuthorization(auth, ge.CarbonAdmissionModel)
}
func resetEnterpriseCarbonResourceChanges(metadata map[string]any, previous ge.Account, next *ge.Account) bool {
	return resetEnterpriseAdmissionResourceChanges(metadata, previous, next, ge.CarbonAdmissionModel)
}
