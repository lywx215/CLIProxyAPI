package auth

import (
	"context"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

// updatePreparedEnterpriseCollection publishes only runtime quota identity.
// Preparation must never persist a child or replay its OAuth/settings snapshot.
func (m *Manager) updatePreparedEnterpriseCollection(ctx context.Context, base, prepared *Auth) (*Auth, error) {
	if !IsEnterpriseCollectionAuth(base) || base.ID != prepared.ID {
		return nil, ErrEnterpriseAccountChanged
	}
	release, err := m.lockAuthMutationContext(ctx, base.ID)
	if err != nil {
		return nil, err
	}
	defer release()
	m.mu.Lock()
	current := m.auths[base.ID]
	binding := ge.Binding(ge.FromMetadata(base.Metadata))
	if !IsEnterpriseCollectionAuth(current) || current.RegistrationEpoch != base.RegistrationEpoch || prepared.RegistrationEpoch != base.RegistrationEpoch ||
		current.CredentialVersion != base.CredentialVersion || prepared.CredentialVersion != base.CredentialVersion ||
		EnterpriseOwnerID(current) != EnterpriseOwnerID(base) || EnterpriseOwnerID(prepared) != EnterpriseOwnerID(base) ||
		EnterpriseBindingID(current) != EnterpriseBindingID(base) || EnterpriseBindingID(prepared) != EnterpriseBindingID(base) ||
		ge.Binding(ge.FromMetadata(current.Metadata)) != binding || ge.Binding(ge.FromMetadata(prepared.Metadata)) != binding || CredentialsChanged(base, current) {
		m.mu.Unlock()
		return nil, ErrEnterpriseAccountChanged
	}
	if prepared.EnterpriseQuotaIdentity == nil {
		snapshot := current.Clone()
		m.mu.Unlock()
		return snapshot, nil
	}
	if prepared.EnterpriseQuotaIdentity.Binding != binding {
		m.mu.Unlock()
		return nil, ErrEnterpriseAccountChanged
	}
	next := current.Clone()
	identity := *prepared.EnterpriseQuotaIdentity
	next.EnterpriseQuotaIdentity = &identity
	next.Generation++
	m.attachEnterpriseQuotaLocked(next)
	m.auths[next.ID] = next
	m.notifyAuthChangeLocked(next.ID)
	snapshot := next.Clone()
	m.mu.Unlock()
	release()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return snapshot, nil
}
