package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type postLoginTestAuthenticator struct {
	dummyAuthenticator
	after func(*coreauth.Auth)
}

func (a *postLoginTestAuthenticator) PostLogin(_ context.Context, _ *config.Config, record *coreauth.Auth) {
	a.after(record)
}

func TestPostLoginDiscoveryRunsAfterDurableSave(t *testing.T) {
	dir := t.TempDir()
	called := false
	a := &postLoginTestAuthenticator{dummyAuthenticator: dummyAuthenticator{provider: "post-login-test", record: &coreauth.Auth{ID: "saved.json", FileName: "saved.json", Provider: "post-login-test", Metadata: map[string]any{"type": "post-login-test", "access_token": "fixture-access"}}}}
	a.after = func(*coreauth.Auth) {
		called = true
		if _, err := os.Stat(filepath.Join(dir, "saved.json")); err != nil {
			t.Error("discovery ran before the credential was saved")
		}
	}
	_, _, err := NewManager(NewFileTokenStore(), a).Login(context.Background(), a.Provider(), &config.Config{AuthDir: dir}, nil)
	if err != nil || !called {
		t.Fatalf("post login hook not called: %v", err)
	}
}
