package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

// EnsureEnterpriseAccountToken returns the requested parent's or child's current
// snapshot. Administrative reads may refresh disabled accounts without enabling them.
func (m *Manager) EnsureEnterpriseAccountToken(ctx context.Context, authID string) (*Auth, error) {
	if m == nil {
		return nil, ErrEnterpriseAccountChanged
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	target := m.resolveEnterpriseLocked(strings.TrimSpace(authID)).Clone()
	var parent *Auth
	if target != nil {
		parent = m.auths[EnterpriseOwnerID(target)].Clone()
	}
	pendingSave := parent != nil && m.enterpriseAccounts.pendingSaves[parent.ID]
	m.mu.RUnlock()
	if parent == nil || enterpriseRetired(parent) {
		return nil, ErrEnterpriseAccountChanged
	}
	if hasUnauthorizedAuthFailure(parent) || hasDisabledInvalidGrantFailure(parent) {
		return nil, errors.New("gemini-enterprise credential requires reauthorization")
	}
	if parent.HasValidAccessToken(time.Now().Add(30*time.Second)) && parent.RejectedAccessToken == "" {
		if pendingSave {
			if err := m.retryEnterpriseAccountSave(ctx, parent.ID); err != nil {
				return nil, err
			}
		}
		return target, nil
	}
	return m.sharedEnterpriseAccountRefresh(ctx, target, "", target.RegistrationEpoch)
}

func (m *Manager) retryEnterpriseAccountSave(ctx context.Context, ownerID string) error {
	release, err := m.lockAuthMutationContext(ctx, ownerID)
	if err != nil {
		return err
	}
	defer release()
	m.mu.Lock()
	if !m.enterpriseAccounts.pendingSaves[ownerID] {
		m.mu.Unlock()
		return nil
	}
	current := m.auths[ownerID]
	if current == nil || enterpriseRetired(current) {
		m.mu.Unlock()
		return ErrEnterpriseAccountChanged
	}
	account, err := ge.ReadAccount(current.Metadata)
	if err != nil || account == nil {
		m.mu.Unlock()
		return ErrEnterpriseAccountChanged
	}
	updated, before := current.Clone(), current.Clone()
	updated.Generation++
	if err = m.persistLocked(withEnterpriseStoreRevision(ctx, account.Revision), updated); err != nil {
		m.mu.Unlock()
		return err
	}
	mergeAuthSaveDelta(updated, before, current, false)
	m.auths[ownerID] = updated
	delete(m.enterpriseAccounts.pendingSaves, ownerID)
	m.notifyAuthChangeLocked(ownerID)
	m.mu.Unlock()
	return nil
}

func (m *Manager) refreshAuthForRequestAtEpoch(ctx context.Context, id, failedAccessToken string, registrationEpoch uint64) (*Auth, error) {
	if m != nil {
		m.mu.RLock()
		target := m.auths[id].Clone()
		m.mu.RUnlock()
		if IsEnterpriseAccountAuth(target) || IsEnterpriseCollectionAuth(target) {
			return m.sharedEnterpriseAccountRefresh(ctx, target, failedAccessToken, registrationEpoch)
		}
	}
	return m.refreshAuthForRequestAtEpochDirect(ctx, id, failedAccessToken, registrationEpoch)
}

func (m *Manager) sharedEnterpriseAccountRefresh(ctx context.Context, target *Auth, failedAccessToken string, registrationEpoch uint64) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if target == nil || target.Provider != ge.Provider {
		return nil, ErrEnterpriseAccountChanged
	}
	ownerID := EnterpriseOwnerID(target)
	m.mu.RLock()
	current := m.auths[target.ID]
	parent := m.auths[ownerID].Clone()
	valid := current != nil && (registrationEpoch == 0 || current.RegistrationEpoch == registrationEpoch)
	m.mu.RUnlock()
	if !valid || parent == nil || enterpriseRetired(parent) {
		return nil, ErrEnterpriseAccountChanged
	}
	coordinator := &m.enterpriseAccounts
	coordinator.mu.Lock()
	if coordinator.flights == nil {
		coordinator.flights = make(map[string]*enterpriseAccountRefresh)
	}
	flight := coordinator.flights[ownerID]
	if flight == nil {
		flight = &enterpriseAccountRefresh{done: make(chan struct{})}
		coordinator.flights[ownerID] = flight
		// The shared credential acquisition owns its lifetime. Cancelling one
		// waiter cannot cancel a refresh already needed by sibling requests.
		refreshCtx := context.WithoutCancel(ctx)
		go func() {
			_, err := m.refreshAuthForRequestAtEpochDirect(refreshCtx, ownerID, failedAccessToken, parent.RegistrationEpoch)
			if err != nil {
				m.mu.Lock()
				var snapshots []*Auth
				if latest := m.auths[ownerID]; latest != nil && latest.RegistrationEpoch == parent.RegistrationEpoch {
					snapshots = m.reconcileEnterpriseAccountLocked(latest)
				}
				m.mu.Unlock()
				m.publishEnterpriseSnapshots(snapshots)
			}
			coordinator.mu.Lock()
			flight.err = err
			delete(coordinator.flights, ownerID)
			close(flight.done)
			coordinator.mu.Unlock()
		}()
	}
	coordinator.mu.Unlock()
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
	if latest == nil || latest.RegistrationEpoch != target.RegistrationEpoch || EnterpriseOwnerID(latest) != ownerID {
		return nil, ErrEnterpriseAccountChanged
	}
	return latest.Clone(), nil
}
