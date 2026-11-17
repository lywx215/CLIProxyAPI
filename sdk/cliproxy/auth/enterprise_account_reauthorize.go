package auth

import (
	"context"
	"errors"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

type enterpriseReauthorizationSaveKey struct{}
type existingEnterpriseReauthorizationKey struct{}
type enterpriseStoreRevisionKey struct{}

var ErrEnterpriseReauthorizationUnsupported = errors.New("credential store does not support exact Gemini Enterprise reauthorization")

// Explicit reauthorization requires a store that checks the original grant and
// rejects deletion or replacement under its durable write boundary.
func supportsExistingEnterpriseReauthorization(store Store) bool {
	capability, ok := store.(interface{ SupportsExistingEnterpriseReauthorization() bool })
	return ok && capability.SupportsExistingEnterpriseReauthorization()
}

func (m *Manager) SupportsExistingEnterpriseReauthorization() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return supportsExistingEnterpriseReauthorization(m.store)
}

// EnterpriseAccountStoreRevision exposes the graph CAS to transactional stores.
func EnterpriseAccountStoreRevision(ctx context.Context) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	revision, ok := ctx.Value(enterpriseStoreRevisionKey{}).(uint64)
	return revision, ok
}

func withEnterpriseStoreRevision(ctx context.Context, revision uint64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, enterpriseStoreRevisionKey{}, revision)
}

// IsEnterpriseReauthorizationSave lets transactional stores preserve their latest
// graph while installing a verified authorization from an independent login.
func IsEnterpriseReauthorizationSave(ctx context.Context) bool {
	return ctx != nil && ctx.Value(enterpriseReauthorizationSaveKey{}) == true
}

func RequiresExistingEnterpriseAuthorization(ctx context.Context) bool {
	return ctx != nil && ctx.Value(existingEnterpriseReauthorizationKey{}) != nil
}

// CheckExistingEnterpriseAuthorization fences an explicit reauthorization against another grant or deletion.
func CheckExistingEnterpriseAuthorization(ctx context.Context, current *Auth) error {
	if !RequiresExistingEnterpriseAuthorization(ctx) {
		return nil
	}
	base, _ := ctx.Value(existingEnterpriseReauthorizationKey{}).(*Auth)
	if current == nil || current.ID != base.ID || current.Provider != base.Provider || !sameEnterprisePrincipal(base, current) ||
		enterpriseOAuthAuthorizationRevision(current) != enterpriseOAuthAuthorizationRevision(base) ||
		ge.String(current.Metadata, "client_secret_env") != ge.String(base.Metadata, "client_secret_env") ||
		ge.String(current.Metadata, "refresh_token") != ge.String(base.Metadata, "refresh_token") {
		return ErrEnterpriseAccountChanged
	}
	return nil
}

// RebaseEnterpriseAuthorization is shared with stores that atomically reload an
// account under their write lock. It never accepts an incoming collection graph.
func RebaseEnterpriseAuthorization(current, incoming *Auth) (*Auth, error) {
	return installEnterpriseAuthorization(current, incoming)
}

// ReauthorizeEnterpriseAccount commits newly verified OAuth without replaying a
// constructor's old collection graph or user configuration over current state.
func (m *Manager) ReauthorizeEnterpriseAccount(ctx context.Context, record *Auth) (*Auth, error) {
	if m == nil || record == nil || !IsEnterpriseAccountAuth(record) || IsPluginVirtualAuth(record) || ge.String(record.Metadata, "access_token") == "" {
		return nil, ErrEnterpriseAccountChanged
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, enterpriseReauthorizationSaveKey{}, true)
	updated, err := m.updateInternal(ctx, nil, record.Clone(), updateModeEnterpriseReauthorize)
	if updated != nil || err != nil {
		return updated, err
	}
	// Register also rebases against an account that appeared after this lookup.
	if m.EnterpriseOAuthImportJournalStore() != nil && m.SupportsEnterpriseOAuthImport() {
		ctx = WithAuthCreationIntent(ctx)
	}
	return m.Register(ctx, record.Clone())
}

// ReauthorizeExistingGeminiEnterpriseCredential updates only the credential that started the OAuth session.
func (m *Manager) ReauthorizeExistingGeminiEnterpriseCredential(ctx context.Context, base, record *Auth) (*Auth, error) {
	if m == nil || base == nil || record == nil || base.ID != record.ID ||
		record.Provider != ge.Provider || IsBoundChildAuth(record) || IsPluginVirtualAuth(record) ||
		ge.String(record.Metadata, "access_token") == "" || !sameEnterprisePrincipal(base, record) {
		return nil, ErrEnterpriseAccountChanged
	}
	if !m.SupportsExistingEnterpriseReauthorization() {
		return nil, ErrEnterpriseReauthorizationUnsupported
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, enterpriseReauthorizationSaveKey{}, true)
	ctx = context.WithValue(ctx, existingEnterpriseReauthorizationKey{}, base)
	updated, err := m.updateInternal(ctx, base, record.Clone(), updateModeEnterpriseReauthorize)
	if updated == nil && err == nil {
		err = ErrEnterpriseAccountChanged
	}
	return updated, err
}

func installEnterpriseAuthorization(current, incoming *Auth) (*Auth, error) {
	if IsAntigravityEnterpriseAccountAuth(current) && IsAntigravityEnterpriseAccountAuth(incoming) {
		return installAntigravityEnterpriseAuthorization(current, incoming)
	}
	if current == nil || incoming == nil || current.Provider != ge.Provider || incoming.Provider != ge.Provider ||
		IsBoundChildAuth(current) || IsPluginVirtualAuth(current) ||
		IsEnterpriseAccountAuth(current) != IsEnterpriseAccountAuth(incoming) ||
		!sameEnterprisePrincipal(current, incoming) || enterpriseRetired(current) {
		return nil, ErrEnterpriseAccountChanged
	}
	if IsEnterpriseAccountAuth(current) {
		account, err := ge.ReadAccount(current.Metadata)
		if err != nil || account == nil || account.ID != ge.AccountID(ge.String(incoming.Metadata, "email"), ge.String(incoming.Metadata, "client_id")) {
			return nil, ErrEnterpriseAccountChanged
		}
	}
	updated := current.Clone()
	updated.Metadata[EnterpriseOAuthRevisionKey] = enterpriseOAuthAuthorizationRevision(current) + 1
	for _, key := range []string{"expired", "expire", "expires_at", "expiresAt", "expiry", "expires", "expires_in", "timestamp"} {
		delete(updated.Metadata, key)
	}
	for key, value := range incoming.Metadata {
		if IsAuthTokenPayloadKey(key) || key == "expiry" || key == "client_secret_env" {
			if key == "refresh_token" && value == "" {
				continue
			}
			updated.Metadata[key] = cloneAuthMetadataValue(value)
		}
	}
	updated.Storage = nil
	updated.RejectedAccessToken = ""
	updated.LastRefreshedAt = incoming.LastRefreshedAt
	if updated.LastRefreshedAt.IsZero() {
		updated.LastRefreshedAt = time.Now()
	}
	updated.NextRefreshAfter, updated.RefreshFailures = time.Time{}, 0
	updated.LastError, updated.StatusMessage, updated.Unavailable = nil, "", false
	if updated.Disabled {
		updated.Status = StatusDisabled
	} else {
		updated.Status = StatusActive
	}
	reconcileEnterpriseCarbonAuthorization(current, updated, updateModeEnterpriseReauthorize)
	return updated, nil
}
