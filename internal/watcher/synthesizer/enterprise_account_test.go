package synthesizer

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

func TestEnterpriseAccountSynthesizerEmitsParentAndEveryChild(t *testing.T) {
	metadata := ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", EngineID: "app"}, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})
	account, err := ge.AccountFromLegacy(metadata)
	if err != nil {
		t.Fatal(err)
	}
	other := account.Collections[0]
	other.ProjectID, other.Disabled = "456", true
	other.ID = ge.CollectionID(other.ResourceBinding)
	other.Settings = map[string]any{"prefix": "second"}
	account.Collections = append(account.Collections, other)
	metadata = ge.PutAccount(metadata, account)
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: dir, Now: time.Now(), IDGenerator: NewStableIDGenerator()}
	auths, err := SynthesizeAuthFile(ctx, filepath.Join(dir, "account.json"), data)
	if err != nil || len(auths) != 3 || !coreauth.IsEnterpriseAccountAuth(auths[0]) {
		t.Fatalf("synthesis count=%d err=%v", len(auths), err)
	}
	if !coreauth.IsEnterpriseCollectionAuth(auths[1]) || auths[1].Disabled || !auths[2].Disabled || auths[2].Prefix != "second" || auths[1].Index == auths[2].Index {
		t.Fatal("synthesis lost child settings or identities")
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err = manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	if children := manager.EnterpriseCollections(auths[0].ID); len(children) != 2 || children[0].Index != auths[1].Index || children[1].Index != auths[2].Index {
		t.Fatal("manager and synthesizer did not share the same projection")
	}
}
