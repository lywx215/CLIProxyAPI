package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

type enterpriseAccountTransport func(*http.Request) (*http.Response, error)

func (transport enterpriseAccountTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestGeminiEnterpriseAccountRecordResourceIndependentIdentityAndRefresh(t *testing.T) {
	application := config.GeminiEnterpriseOAuth{ClientID: "client"}
	token := &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)}
	first, err := NewGeminiEnterpriseAccountRecord(application, " Fixture@Example.com ", token, nil)
	if err != nil {
		t.Fatal(err)
	}
	account, err := ge.ReadAccount(first.Metadata)
	if err != nil || len(account.Collections) != 0 {
		t.Fatalf("account-only authorization was rejected: %v", err)
	}
	first.Metadata["custom"] = map[string]any{"nested": []any{"original"}}
	account.Collections = []ge.Collection{{ResourceBinding: ge.ResourceBinding{ProjectID: "123", Location: "global", CollectionID: "custom", EngineID: "existing", AssistantID: "selected"}, Revision: 7, Disabled: true, Settings: map[string]any{"prefix": "saved"}}}
	account.Collections[0].ID = ge.CollectionID(account.Collections[0].ResourceBinding)
	first.Metadata = ge.PutAccount(first.Metadata, *account)
	first.Disabled = true
	changed := application
	changed.ClientSecretEnv, changed.ProjectID, changed.EngineID = "DIFFERENT_SECRET_REF", "456", "other"
	renewed, err := NewGeminiEnterpriseAccountRecord(changed, "fixture@example.com", &oauth2.Token{AccessToken: "new"}, []*coreauth.Auth{first})
	if err != nil {
		t.Fatal(err)
	}
	read, _ := ge.ReadAccount(renewed.Metadata)
	if renewed.ID != first.ID || renewed.FileName != first.FileName || !renewed.Disabled || ge.String(renewed.Metadata, "refresh_token") != "synthetic-refresh" || len(read.Collections) != 1 || read.Collections[0].EngineID != "existing" || !read.Collections[0].Disabled || read.Collections[0].Settings["prefix"] != "saved" || read.Collections[0].Revision != 7 {
		t.Fatal("reauthorization changed account identity or discarded saved graph/preferences")
	}
	renewed.Metadata["custom"].(map[string]any)["nested"].([]any)[0] = "changed"
	if first.Metadata["custom"].(map[string]any)["nested"].([]any)[0] != "original" || token.RefreshToken != "synthetic-refresh" {
		t.Fatal("constructor mutated existing account or input token")
	}
	for _, key := range []string{"engine_id", "project_id", "assistant_id", "location", "collection_id"} {
		if _, exists := renewed.Metadata[key]; exists {
			t.Fatal("parent retained an inference resource field")
		}
	}
	changed.ClientID = "other-client"
	other, err := NewGeminiEnterpriseAccountRecord(changed, "fixture@example.com", &oauth2.Token{AccessToken: "new"}, []*coreauth.Auth{first})
	if err != nil || other.ID == first.ID || ge.String(other.Metadata, "refresh_token") != "" {
		t.Fatal("unrelated client inherited account or refresh token")
	}
}

func TestGeminiEnterpriseAccountRecordLegacyRefreshAndNewTokenPrecedence(t *testing.T) {
	legacyBinding := config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", EngineID: "old"}
	legacy := NewGeminiEnterpriseRecord(legacyBinding, "fixture@example.com", &oauth2.Token{AccessToken: "old", RefreshToken: "legacy-refresh"}, nil)
	application := config.GeminiEnterpriseOAuth{ClientID: "client"}
	account, err := NewGeminiEnterpriseAccountRecord(application, "fixture@example.com", &oauth2.Token{AccessToken: "new"}, []*coreauth.Auth{legacy})
	if err != nil || ge.String(account.Metadata, "refresh_token") != "legacy-refresh" {
		t.Fatal("same-account legacy refresh credential was not retained")
	}
	renewed, err := NewGeminiEnterpriseAccountRecord(application, "fixture@example.com", &oauth2.Token{AccessToken: "new", RefreshToken: "new-refresh"}, []*coreauth.Auth{account})
	if err != nil || ge.String(renewed.Metadata, "refresh_token") != "new-refresh" || ge.String(account.Metadata, "refresh_token") != "legacy-refresh" {
		t.Fatal("new refresh credential did not win immutably")
	}
}

func TestGeminiEnterprisePostLoginDiscoveryPreservesSavedAuthorizationOnFailure(t *testing.T) {
	application := config.GeminiEnterpriseOAuth{ClientID: "client"}
	record, err := NewGeminiEnterpriseAccountRecord(application, "fixture@example.com", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	if _, err = store.Save(coreauth.WithAuthCreationIntent(context.Background()), record); err != nil {
		t.Fatal(err)
	}
	previousTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	http.DefaultTransport = enterpriseAccountTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://cloudresourcemanager.googleapis.com/v3/projects:search?pageSize=100" {
			t.Error("post-login performed an unexpected request")
		}
		listed, errList := store.List(context.Background())
		if errList != nil || len(listed) != 1 || ge.String(listed[0].Metadata, "refresh_token") != "synthetic-refresh" {
			t.Error("discovery began before authorization was durable")
		}
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"secret-upstream-text"}}`))}, nil
	})
	result, err := DiscoverGeminiEnterpriseAccount(context.Background(), &config.Config{}, record)
	if err == nil || strings.Contains(err.Error(), "secret-upstream-text") || result.Status != "failed" || ge.String(record.Metadata, "refresh_token") != "synthetic-refresh" {
		t.Fatal("failed discovery lost OAuth or exposed upstream diagnostics")
	}
	account, _ := ge.ReadAccount(record.Metadata)
	if account.Discovery["status"] != "failed" {
		t.Fatal("discovery failure was not retained on graph")
	}
	listed, err := store.List(context.Background())
	if err != nil || len(listed) != 1 || ge.String(listed[0].Metadata, "access_token") != "synthetic-access" {
		t.Fatal("discovery failure removed saved authorization")
	}
}
