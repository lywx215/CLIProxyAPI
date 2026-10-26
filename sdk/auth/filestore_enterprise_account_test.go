package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

func fileEnterpriseFixture(t *testing.T) *coreauth.Auth {
	t.Helper()
	metadata := ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", EngineID: "app"}, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})
	account, err := ge.AccountFromLegacy(metadata)
	if err != nil {
		t.Fatal(err)
	}
	other := account.Collections[0]
	other.ProjectID = "456"
	other.ID = ge.CollectionID(other.ResourceBinding)
	account.Collections = append(account.Collections, other)
	return &coreauth.Auth{ID: "account.json", FileName: "account.json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: ge.PutAccount(metadata, account)}
}

type forbiddenEnterpriseStorage struct{}

func (forbiddenEnterpriseStorage) SaveTokenToFile(string) error {
	return errors.New("stale token storage must not save an account")
}

func TestFileStoreEnterpriseAccountRoundTripProjectsEveryChild(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	fixture := fileEnterpriseFixture(t)
	fixture.Storage = forbiddenEnterpriseStorage{}
	path, err := store.Save(t.Context(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if json.Unmarshal(raw, &metadata) != nil || metadata["access_token"] != "synthetic-access" {
		t.Fatal("OAuth was not saved at the parent level")
	}
	account, err := ge.ReadAccount(metadata)
	if err != nil || len(account.Collections) != 2 {
		t.Fatal("saved graph was not readable", err)
	}
	records, err := store.List(t.Context())
	if err != nil || len(records) != 3 || !coreauth.IsEnterpriseAccountAuth(records[0]) {
		t.Fatalf("roundtrip records=%d err=%v", len(records), err)
	}
	indices := map[string]bool{}
	for _, record := range records {
		if indices[record.Index] {
			t.Fatal("parent and children share an index")
		}
		indices[record.Index] = true
	}
	if _, err = store.Save(t.Context(), records[1]); err == nil {
		t.Fatal("runtime child could overwrite its parent file")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatal("account transaction left a second credential or temporary file")
	}
}

func TestFileStoreEnterpriseReauthorizationRebasesGraphWrittenByAnotherManager(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	runtime := coreauth.NewManager(store, nil, nil)
	old, err := runtime.Register(t.Context(), fileEnterpriseFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	newer := old.Clone()
	graph, _ := ge.ReadAccount(newer.Metadata)
	graph.Revision++
	graph.Collections[0].Revision++
	graph.Collections[0].Disabled = true
	newer.Metadata = ge.PutAccount(newer.Metadata, *graph)
	if _, err = store.Save(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	old.Metadata["access_token"] = "new-authorization"
	updated, err := runtime.ReauthorizeEnterpriseAccount(t.Context(), old)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ = ge.ReadAccount(updated.Metadata)
	if graph.Revision != 2 || !graph.Collections[0].Disabled || ge.String(updated.Metadata, "access_token") != "new-authorization" {
		t.Fatal("reauthorization overwrote the latest on-disk graph")
	}
}
