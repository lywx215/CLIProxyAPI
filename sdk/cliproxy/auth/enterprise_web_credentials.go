package auth

import (
	"context"
	"encoding/json"
	"os"

	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
)

type webMutationKey struct{}
type webMutation struct {
	revision          uint64
	mutate            func(*Auth, *web.Credential) error
	verifiedRotation  bool
	sourceFingerprint string
	sourcePolicy      string
}

// UpdateEnterpriseWebCredential rebases edits under the existing credential mutation gate.
func (m *Manager) UpdateEnterpriseWebCredential(ctx context.Context, id string, revision uint64, mutate func(*Auth, *web.Credential) error) (*Auth, error) {
	return m.updateEnterpriseWebCredential(ctx, id, webMutation{revision: revision, mutate: mutate})
}

func (m *Manager) updateEnterpriseWebCredential(ctx context.Context, id string, mutation webMutation) (*Auth, error) {
	if mutation.revision == 0 || mutation.mutate == nil {
		return nil, web.Fail(409, "A current credential revision is required")
	}
	return m.updateInternal(context.WithValue(ctx, webMutationKey{}, mutation), nil, &Auth{ID: id, Provider: web.Provider}, updateModeEnterpriseWeb)
}

func (m *Manager) commitEnterpriseWebCookieRotation(ctx context.Context, id string, epoch uint64, source, candidate web.Credential) (web.Credential, error) {
	mutate := func(auth *Auth, current *web.Credential) error {
		if auth.Disabled || auth.Status == StatusDisabled || current.Fingerprint() != source.Fingerprint() || current.Policy() != source.Policy() || m.webRuntime.CredentialEpoch(id) != epoch {
			return web.Fail(409, "Web credential changed during Cookie renewal")
		}
		current.Cookies = candidate.Cookies
		current.AvailableModels = candidate.AvailableModels
		current.CatalogFingerprint = candidate.CatalogFingerprint
		current.CatalogUpdatedAt = candidate.CatalogUpdatedAt
		current.LastRenewedAt = candidate.LastRenewedAt
		current.LastCCRenewedAt = candidate.LastCCRenewedAt
		return nil
	}
	updated, err := m.updateEnterpriseWebCredential(ctx, id, webMutation{revision: source.Revision, mutate: mutate, verifiedRotation: true})
	if err != nil {
		return web.Credential{}, err
	}
	if updated == nil {
		return web.Credential{}, web.Fail(409, "Web credential removed during Cookie renewal")
	}
	return web.CredentialFrom(updated.Metadata)
}

// CheckEnterpriseWebStoreRevision is called under each store's save lock, before writing.
func CheckEnterpriseWebStoreRevision(ctx context.Context, auth *Auth, path string) error {
	if auth.Provider != web.Provider {
		return nil
	}
	mutation, hasExpected := ctx.Value(webMutationKey{}).(webMutation)
	conflict := func() error { return web.Fail(409, "Credential source changed; reload before saving") }
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) && !hasExpected {
		return nil
	}
	if err != nil {
		return conflict()
	}
	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return conflict()
	}
	current, err := web.CredentialFrom(meta)
	if err != nil {
		return conflict()
	}
	incoming, err := web.CredentialFrom(auth.Metadata)
	if err != nil {
		return err
	}
	if hasExpected && (current.Revision != mutation.revision || current.Fingerprint() != mutation.sourceFingerprint || current.Policy() != mutation.sourcePolicy) {
		return conflict()
	}
	if incoming.Revision < current.Revision || (incoming.Revision == current.Revision && (incoming.Fingerprint() != current.Fingerprint() || incoming.Policy() != current.Policy())) {
		return conflict()
	}
	return nil
}
