package auth

import (
	"context"
	"errors"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

var ErrAuthFileWriteConflict = errors.New("credential source changed or is being written; reload and retry")

type authFileWriteLockKey struct{}

// WithAuthFileWriteLock makes an explicit file upload participate in the store's
// write transaction, including replacement with a different provider.
func WithAuthFileWriteLock(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, authFileWriteLockKey{}, true)
}

func RequiresAuthFileWriteLock(ctx context.Context) bool {
	return ctx != nil && ctx.Value(authFileWriteLockKey{}) == true
}

// OwnsAuthFileWrites identifies stores that persist the uploaded source in Save.
// Management must not write that source before entering the store transaction.
func (m *Manager) OwnsAuthFileWrites() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	store, ok := m.store.(interface{ OwnsAuthFileWrites() bool })
	return ok && store.OwnsAuthFileWrites()
}

// CheckEnterpriseAuthorizationWrite prevents an old settings/refresh snapshot
// from restoring OAuth material replaced by a completed exact reauthorization.
func CheckEnterpriseAuthorizationWrite(current, incoming *Auth) error {
	if current != nil && incoming != nil && current.Provider == ge.Provider && incoming.Provider == ge.Provider &&
		enterpriseOAuthAuthorizationRevision(current) > enterpriseOAuthAuthorizationRevision(incoming) {
		return ErrAuthFileWriteConflict
	}
	return nil
}
