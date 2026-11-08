package auth

import (
	"context"
	"fmt"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func (m *Manager) antigravityEnterpriseRuntime(ctx context.Context) (*coreauth.Manager, error) {
	if m.enterpriseManager != nil {
		return m.enterpriseManager, nil
	}
	if m.store == nil {
		return nil, fmt.Errorf("credential store unavailable")
	}
	runtime := coreauth.NewManager(m.store, nil, nil)
	if err := runtime.Load(ctx); err != nil {
		return nil, err
	}
	return runtime, nil
}
func (m *Manager) saveAntigravityEnterpriseAuthorization(ctx context.Context, record *coreauth.Auth) (*coreauth.Auth, string, error) {
	runtime, err := m.antigravityEnterpriseRuntime(ctx)
	if err != nil {
		return nil, "", err
	}
	saved, err := runtime.ReauthorizeAntigravityEnterpriseAccount(coreauth.WithAuthCreationIntent(ctx), record)
	if err != nil {
		return nil, "", err
	}
	return saved, saved.FileName, nil
}
func (m *Manager) saveAntigravityEnterpriseDiscovery(ctx context.Context, base, proposed *coreauth.Auth) (*coreauth.Auth, error) {
	if base == nil || proposed == nil {
		return nil, fmt.Errorf("invalid account discovery")
	}
	before, err := ae.ReadAccount(base.Metadata)
	if err != nil || before == nil {
		return nil, fmt.Errorf("invalid account")
	}
	discovered, err := ae.ReadAccount(proposed.Metadata)
	if err != nil || discovered == nil {
		return nil, fmt.Errorf("invalid discovery")
	}
	if before.Revision == discovered.Revision {
		return base, nil
	}
	runtime, err := m.antigravityEnterpriseRuntime(ctx)
	if err != nil {
		return nil, err
	}
	current, graph, ok := runtime.AntigravityEnterpriseAccountSnapshot(base.ID)
	if !ok {
		return nil, fmt.Errorf("account changed")
	}
	if ae.String(current.Metadata, "antigravity_enterprise_authorization_revision") != ae.String(base.Metadata, "antigravity_enterprise_authorization_revision") || graph.Discovery.CheckedAt.After(discovered.Discovery.CheckedAt) {
		return current, nil
	}
	return runtime.UpdateAntigravityEnterpriseAccount(ctx, current, 0, func(a *ae.Account) error {
		licenses := []ae.License{}
		for _, binding := range discovered.Bindings {
			if !binding.Missing {
				licenses = append(licenses, binding.License)
			}
		}
		merged, errMerge := ae.MergeLicenses(*a, licenses, discovered.Discovery.Unreachable, discovered.Discovery.CheckedAt)
		if errMerge == nil {
			*a = merged
		}
		return errMerge
	})
}
