package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/oauth2"
)

func regionalTestAuth() *Auth {
	return &Auth{ID: "regional.json", FileName: "regional.json", Provider: ge.Provider, Status: StatusActive,
		Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", EngineID: "app"}, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})}
}

func TestEnterpriseResourcesMergeLatestAndFenceOldResults(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	base, err := m.Register(ctx, regionalTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	latest := base.Clone()
	latest.Metadata["access_token"], latest.Metadata["refresh_token"], latest.Metadata["note"] = "synthetic-new", "synthetic-rotated", "keep-note"
	latest.Success, latest.Failed = 11, 4
	latest.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
	latest.NextRetryAfter, latest.Unavailable = latest.Quota.NextRecoverAt, true
	latest.ModelStates = map[string]*ModelState{"old-model": {Status: StatusError, Unavailable: true}}
	latest, err = m.Update(ctx, latest)
	if err != nil {
		t.Fatal(err)
	}
	// Counters belong to the runtime, not to replacement snapshots.
	m.MarkResult(ctx, Result{AuthID: base.ID, Provider: ge.Provider, Model: "old-model", Success: true, RegistrationEpoch: latest.RegistrationEpoch, CredentialVersion: latest.CredentialVersion})
	latest, _ = m.GetByID(base.ID)
	resources := ge.Resources(ge.FromMetadata(base.Metadata))
	resources.Location, resources.EngineID = "eu", "eu-app"
	updated, err := m.UpdateEnterpriseResources(ctx, base, resources)
	if err != nil {
		t.Fatal(err)
	}
	if ge.Resources(ge.FromMetadata(updated.Metadata)) != resources || ge.String(updated.Metadata, "access_token") != "synthetic-new" || ge.String(updated.Metadata, "refresh_token") != "synthetic-rotated" || ge.String(updated.Metadata, "note") != "keep-note" || updated.Success != latest.Success || updated.Failed != latest.Failed {
		t.Fatal("lost resources, concurrent token rotation, settings or counters")
	}
	if updated.CredentialVersion <= latest.CredentialVersion || updated.Unavailable || updated.Quota.Exceeded || len(updated.ModelStates) != 0 || updated.EnterpriseQuotaIdentity != nil {
		t.Fatal("new binding retained old credential state")
	}
	// Old refresh and request completion must not overwrite or cool down the new region.
	refreshed := base.Clone()
	refreshed.Metadata["access_token"] = "stale-refresh"
	if _, err = m.UpdateRefreshedAuth(ctx, base, refreshed); err != nil {
		t.Fatal(err)
	}
	m.MarkResult(ctx, Result{AuthID: base.ID, Provider: ge.Provider, Model: "old-model", CredentialVersion: latest.CredentialVersion, RegistrationEpoch: latest.RegistrationEpoch, Error: &Error{HTTPStatus: 429, Message: "old resource quota"}})
	after, _ := m.GetByID(base.ID)
	if ge.FromMetadata(after.Metadata).Location != "eu" || ge.String(after.Metadata, "access_token") != "synthetic-new" || after.Unavailable || len(after.ModelStates) != 0 {
		t.Fatal("stale result or refresh mutated new binding")
	}
	if _, err = m.UpdateEnterpriseResources(ctx, base, resources); !errors.Is(err, ErrEnterpriseResourcesChanged) {
		t.Fatal("stale resource snapshot accepted", err)
	}
}

func TestEnterpriseResourceSaveFailureKeepsOriginalBinding(t *testing.T) {
	ctx := context.Background()
	fail := false
	store := &callbackAuthStore{save: func(*Auth) error {
		if fail {
			return errors.New("synthetic disk failure")
		}
		return nil
	}}
	m := NewManager(store, nil, nil)
	base, err := m.Register(ctx, regionalTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	resources := ge.Resources(ge.FromMetadata(base.Metadata))
	resources.Location = "us"
	fail = true
	if _, err = m.UpdateEnterpriseResources(ctx, base, resources); err == nil {
		t.Fatal("save failure accepted")
	}
	after, _ := m.GetByID(base.ID)
	if ge.Binding(ge.FromMetadata(after.Metadata)) != ge.Binding(ge.FromMetadata(base.Metadata)) || after.CredentialVersion != base.CredentialVersion {
		t.Fatal("failed save changed live resources")
	}
}

func TestEnterpriseResourcesFenceResultQueuedDuringSave(t *testing.T) {
	ctx := context.Background()
	var onSave func()
	store := &callbackAuthStore{save: func(*Auth) error {
		if onSave != nil {
			onSave()
		}
		return nil
	}}
	m := NewManager(store, nil, nil)
	base, err := m.Register(ctx, regionalTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	onSave = func() {
		// Store callbacks may read the manager, but cannot synchronously mutate it.
		// A completion queued during Save resumes after the mutation gate opens.
		if _, ok := m.GetByID(base.ID); !ok {
			t.Fatal("save callback could not read manager")
		}
		go func() {
			defer close(done)
			m.MarkResult(ctx, Result{AuthID: base.ID, Provider: ge.Provider, Model: "old-model", CredentialVersion: base.CredentialVersion, RegistrationEpoch: base.RegistrationEpoch, Error: &Error{HTTPStatus: 429, Message: "old quota"}})
		}()
	}
	resources := ge.Resources(ge.FromMetadata(base.Metadata))
	resources.Location = "us"
	updated, err := m.UpdateEnterpriseResources(ctx, base, resources)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	current, _ := m.GetByID(base.ID)
	if updated.Failed != base.Failed || current.Failed != base.Failed || current.Unavailable || len(current.ModelStates) != 0 || current.Quota.Exceeded {
		t.Fatal("a queued old-region result mutated the new binding")
	}
}
