package auth

import (
	"context"
	"testing"
	"time"

	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
)

func TestEnterpriseWebCredentialPublicationInvalidatesOldEvidence(t *testing.T) {
	for _, registerAgain := range []bool{false, true} {
		name := "update"
		if registerAgain {
			name = "register"
		}
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			c := web.Credential{Revision: 1, AvailableModels: []web.AvailableModel{{ID: "public", DisplayName: "Public"}}}
			if err := c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/synthetic"); err != nil {
				t.Fatal(err)
			}
			if err := c.SetCookies("SID=synthetic-one; SAPISID=synthetic-signing"); err != nil {
				t.Fatal(err)
			}
			c.SetCatalog(c.AvailableModels, time.Now())
			a, err := m.Register(context.Background(), &Auth{ID: "web:synthetic", Provider: web.Provider, Status: StatusActive, Metadata: map[string]any{"enterprise_web": c.MetadataValue()}})
			if err != nil {
				t.Fatal(err)
			}
			epoch := m.WebRuntime().CredentialEpoch(a.ID)
			m.WebRuntime().RecordModel(a.ID, a.Metadata, "observed", epoch)
			a.Label = "renamed"
			a, err = m.Update(context.Background(), a)
			if err != nil || m.WebRuntime().CredentialEpoch(a.ID) != epoch {
				t.Fatal("a label edit invalidated authentication evidence", err)
			}
			version := a.CredentialVersion
			c.Cookies, c.Revision = "SID=synthetic-two; SAPISID=synthetic-signing", c.Revision+1
			c.SetCatalog(c.AvailableModels, time.Now())
			c.SetCatalog(c.AvailableModels, time.Now())
			a.Metadata["enterprise_web"] = c.MetadataValue()
			if registerAgain {
				a, err = m.Register(context.Background(), a)
			} else {
				a, err = m.Update(context.Background(), a)
			}
			if err != nil || a.CredentialVersion <= version || m.WebRuntime().CredentialEpoch(a.ID) == epoch {
				t.Fatal("credential publication missed cache/version invalidation", err)
			}
			m.WebRuntime().RecordModel(a.ID, a.Metadata, "observed", epoch)
			if len(m.WebRuntime().CredentialStatus(a.ID, c)["tested_model_ids"].([]string)) != 0 {
				t.Fatal("a late result polluted replacement evidence")
			}
			m.Remove(context.Background(), a.ID)
			if m.EnterpriseWebModel("public") {
				t.Fatal("removed credential retained a model mapping")
			}
		})
	}
}

func TestEnterpriseWebLoadRebuildsModelsAndInvalidatesReplacedEvidence(t *testing.T) {
	ctx := context.Background()
	store := newMemoryAuthTestStore()
	c := web.Credential{Revision: 1, AvailableModels: []web.AvailableModel{{ID: "public", DisplayName: "Public"}}}
	if err := c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/synthetic"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCookies("SID=synthetic-one; SAPISID=synthetic-signing"); err != nil {
		t.Fatal(err)
	}
	c.SetCatalog(c.AvailableModels, time.Now())
	a := &Auth{ID: "persisted-web", Provider: web.Provider, Status: StatusActive, Metadata: map[string]any{"enterprise_web": c.MetadataValue()}}
	if _, err := store.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	m := NewManager(store, nil, nil)
	if err := m.Load(ctx); err != nil || !m.EnterpriseWebModel("public") {
		t.Fatal("persisted mapping unavailable", err)
	}
	epoch := m.WebRuntime().CredentialEpoch(a.ID)
	m.WebRuntime().RecordModel(a.ID, a.Metadata, "observed", epoch)
	if err := m.Load(ctx); err != nil || m.WebRuntime().CredentialEpoch(a.ID) != epoch {
		t.Fatal("unchanged reload invalidated evidence", err)
	}
	if err := c.SetCookies("SID=synthetic-two; SAPISID=synthetic-signing"); err != nil {
		t.Fatal(err)
	}
	c.AvailableModels[0].ID = "renamed-public"
	c.SetCatalog(c.AvailableModels, time.Now())
	a.Metadata["enterprise_web"] = c.MetadataValue()
	if _, err := store.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if m.EnterpriseWebModel("public") || !m.EnterpriseWebModel("renamed-public") || m.WebRuntime().CredentialEpoch(a.ID) == epoch {
		t.Fatal("reload retained stale mappings or authentication")
	}
	m.WebRuntime().RecordModel(a.ID, a.Metadata, "observed", epoch)
	if len(m.WebRuntime().CredentialStatus(a.ID, c)["tested_model_ids"].([]string)) != 0 {
		t.Fatal("late result survived replacement")
	}
	if err := store.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(ctx); err != nil || m.EnterpriseWebModel("renamed-public") {
		t.Fatal("removed credential retained a routing entry", err)
	}
}
