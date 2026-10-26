package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func preparedCollectionIdentity(child *Auth) *ge.QuotaIdentity {
	binding := ge.FromMetadata(child.Metadata)
	return &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(binding), ProjectNumber: binding.ProjectID, Location: binding.Location, Edition: "enterprise_plus", PrincipalHash: quotaDigest("fixture@example.test"), CheckedAt: time.Now()}
}

func TestEnterpriseAccountPreparationPublishesOnlyRuntimeIdentity(t *testing.T) {
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 1))
	base := m.EnterpriseCollections(parent.ID)[0]
	if _, err := m.UpdateEnterpriseAccount(t.Context(), parent, 0, func(account *ge.Account) error {
		account.Collections[0].Disabled = true
		account.Collections[0].Settings = map[string]any{"prefix": "current-choice", "proxy_url": "http://current.invalid"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetByID(base.ID)
	parentBefore, _ := m.GetByID(parent.ID)
	savesBefore := len(store.saves)
	prepared := base.Clone()
	prepared.EnterpriseQuotaIdentity = preparedCollectionIdentity(base)
	prepared.Metadata["access_token"], prepared.Metadata["refresh_token"] = "ignored-access", "ignored-refresh"
	prepared.Prefix, prepared.ProxyURL, prepared.Disabled, prepared.Success = "stale-choice", "http://stale.invalid", false, 999
	updated, err := m.UpdatePreparedAuth(t.Context(), base, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if updated.EnterpriseQuotaIdentity == nil || !updated.EnterpriseQuotaIdentity.ValidFor(ge.FromMetadata(updated.Metadata)) || len(updated.enterpriseQuota.keys()) != 4 {
		t.Fatal("prepared identity did not reach the runtime shared-quota gate")
	}
	if updated.Prefix != current.Prefix || updated.ProxyURL != current.ProxyURL || updated.Disabled != current.Disabled || updated.Success != current.Success || !reflect.DeepEqual(updated.Metadata, current.Metadata) {
		t.Fatal("preparation replayed stale child settings, counters or OAuth")
	}
	parentAfter, _ := m.GetByID(parent.ID)
	if len(store.saves) != savesBefore || !reflect.DeepEqual(parentBefore, parentAfter) {
		t.Fatal("runtime preparation persisted a child or changed its parent")
	}
	prepared.EnterpriseQuotaIdentity.Edition = "changed-after-publication"
	latest, _ := m.GetByID(base.ID)
	if latest.EnterpriseQuotaIdentity.Edition != "enterprise_plus" {
		t.Fatal("published identity shares mutable state with the preparer")
	}
}

type blockedCollectionIdentityPreparer struct {
	started chan struct{}
	release chan struct{}
}

func (*blockedCollectionIdentityPreparer) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth.EnterpriseQuotaIdentity == nil
}

func (p *blockedCollectionIdentityPreparer) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	close(p.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
	}
	auth.EnterpriseQuotaIdentity = preparedCollectionIdentity(auth)
	return auth, nil
}

func TestEnterpriseAccountPreparationRejectsStaleAsyncResults(t *testing.T) {
	for _, change := range []string{"binding", "credential", "registration"} {
		t.Run(change, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 1))
			child := m.EnterpriseCollections(parent.ID)[0]
			preparer := &blockedCollectionIdentityPreparer{started: make(chan struct{}), release: make(chan struct{})}
			done := make(chan error, 1)
			go func() { _, err := m.PrepareRequestAuth(t.Context(), preparer, child); done <- err }()
			<-preparer.started
			switch change {
			case "binding":
				if _, err := m.UpdateEnterpriseAccount(t.Context(), parent, 0, func(account *ge.Account) error { account.Collections[0].EngineID = "replacement-app"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "credential":
				rotated := parent.Clone()
				rotated.Metadata["access_token"] = "rotated-after-prepare"
				if _, err := m.UpdateRefreshedAuth(t.Context(), parent, rotated); err != nil {
					t.Fatal(err)
				}
			case "registration":
				m.Remove(t.Context(), parent.ID)
				if _, err := m.Register(t.Context(), parent.Clone()); err != nil {
					t.Fatal(err)
				}
			}
			close(preparer.release)
			if err := <-done; !errors.Is(err, ErrEnterpriseAccountChanged) {
				t.Fatalf("stale %s preparation accepted: %v", change, err)
			}
			current, _ := m.GetByID(child.ID)
			if current.EnterpriseQuotaIdentity != nil || current.enterpriseQuota.identity != nil {
				t.Fatal("stale preparation polluted replacement runtime identity")
			}
		})
	}
}
