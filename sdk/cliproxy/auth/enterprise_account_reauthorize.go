package auth

import (
	"context"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

type enterpriseReauthorizationSaveKey struct{}
type enterpriseStoreRevisionKey struct{}

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
	return m.Register(ctx, record.Clone())
}

func installEnterpriseAuthorization(current, incoming *Auth) (*Auth, error) {
	if !IsEnterpriseAccountAuth(current) || !sameEnterprisePrincipal(current, incoming) || enterpriseRetired(current) {
		return nil, ErrEnterpriseAccountChanged
	}
	account, err := ge.ReadAccount(current.Metadata)
	if err != nil || account == nil || account.ID != ge.AccountID(ge.String(incoming.Metadata, "email"), ge.String(incoming.Metadata, "client_id")) {
		return nil, ErrEnterpriseAccountChanged
	}
	updated := current.Clone()
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
	return updated, nil
}
