package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type durablePostLoginStore struct {
	snapshots []*coreauth.Auth
	failPost  bool
}

func (*durablePostLoginStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (*durablePostLoginStore) Delete(context.Context, string) error           { return nil }
func (s *durablePostLoginStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if s.failPost && len(s.snapshots) == 1 {
		return "", errors.New("synthetic second-save failure")
	}
	s.snapshots = append(s.snapshots, auth.Clone())
	return auth.ID, nil
}

type durablePostLoginAuthenticator struct {
	*dummyAuthenticator
	store           *durablePostLoginStore
	called          bool
	savedBeforePost bool
}

func (*durablePostLoginAuthenticator) PostLoginNeedsPersistence() bool { return true }
func (a *durablePostLoginAuthenticator) PostLogin(_ context.Context, _ *config.Config, auth *coreauth.Auth) {
	a.called = true
	a.savedBeforePost = a.store != nil && len(a.store.snapshots) == 1
	account, _ := ge.ReadAccount(auth.Metadata)
	account.Discovery = map[string]any{"status": "failed", "warnings": []any{map[string]any{"code": "synthetic_failure"}}}
	account.Revision++
	auth.Metadata = ge.PutAccount(auth.Metadata, *account)
}

func TestManagerEnterprisePostLoginUsesSelectedStoreAndKeepsAuthorization(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "post_save_failure"}[fail], func(t *testing.T) {
			store := &durablePostLoginStore{failPost: fail}
			auth := &durablePostLoginAuthenticator{dummyAuthenticator: &dummyAuthenticator{provider: ge.Provider, record: fileEnterpriseFixture(t)}, store: store}
			manager := NewManager(store, auth)
			record, path, err := manager.Login(t.Context(), ge.Provider, &config.Config{}, nil)
			if !auth.called || !auth.savedBeforePost || record == nil || path == "" || (err != nil) != fail {
				t.Fatalf("post-login order or persistence contract failed: path=%s err=%v", path, err)
			}
			if ge.String(store.snapshots[0].Metadata, "refresh_token") != "synthetic-refresh" {
				t.Fatal("initial authorization was lost")
			}
			if !fail {
				if len(store.snapshots) != 2 {
					t.Fatal("discovery graph was not saved once after authorization")
				}
				graph, _ := ge.ReadAccount(store.snapshots[1].Metadata)
				if graph.Discovery["status"] != "failed" {
					t.Fatal("failed discovery diagnostics were not saved")
				}
			}
		})
	}
}

func TestManagerEnterpriseNoStoreSkipsDurableDiscovery(t *testing.T) {
	auth := &durablePostLoginAuthenticator{dummyAuthenticator: &dummyAuthenticator{provider: ge.Provider, record: fileEnterpriseFixture(t)}}
	manager := NewManager(nil, auth)
	if _, _, err := manager.Login(t.Context(), ge.Provider, &config.Config{}, nil); err != nil || auth.called {
		t.Fatal("durable discovery ran without a selected store", err)
	}
}

type enterpriseLoginTransport func(*http.Request) (*http.Response, error)

func (f enterpriseLoginTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestManagerEnterpriseReauthorizationVerifiesLegacySourceBeforeMerge(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	target := fileEnterpriseFixture(t)
	if _, err := store.Save(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	source := fileEnterpriseFixture(t)
	source.ID, source.FileName = "legacy-source.json", "legacy-source.json"
	delete(source.Metadata, "enterprise_account")
	source.Metadata["project_id"], source.Metadata["access_token"], source.Metadata["prefix"] = "789", "synthetic-legacy-access", "legacy"
	if _, err := store.Save(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	calls := 0
	http.DefaultTransport = enterpriseLoginTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://www.googleapis.com/oauth2/v2/userinfo" || request.Header.Get("Authorization") != "Bearer synthetic-legacy-access" {
			t.Fatal("legacy verification used the wrong credential or origin")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"email":"fixture@example.test","verified_email":true}`))}, nil
	})
	manager := NewManager(store)
	merged, err := manager.consolidateEnterpriseLogin(t.Context(), &config.Config{}, target)
	if err != nil {
		t.Fatal(err)
	}
	account, err := ge.ReadAccount(merged.Metadata)
	if err != nil || calls != 1 || len(account.Collections) != 3 || len(account.MergedSources) != 1 {
		t.Fatalf("legacy consolidation failed: calls=%d err=%v", calls, err)
	}
	records, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var retired bool
	for _, record := range records {
		if record.ID == source.ID {
			retired = record.Disabled
		}
	}
	if !retired {
		t.Fatal("verified legacy credential was not retained recoverably")
	}
}

func TestManagerEnterprisePostDiscoveryRebasesCurrentChildSettings(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	runtime := coreauth.NewManager(store, nil, nil)
	base, err := runtime.Register(t.Context(), fileEnterpriseFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	proposed := base.Clone()
	graph, _ := ge.ReadAccount(proposed.Metadata)
	graph.Collections[0].Settings = map[string]any{"prefix": "stale-prefix"}
	graph.Discovery = map[string]any{"status": "ok"}
	proposed.Metadata = ge.PutAccount(proposed.Metadata, *graph)
	if _, err = runtime.UpdateEnterpriseAccount(t.Context(), base, 0, func(account *ge.Account) error {
		account.Collections[0].Disabled = true
		account.Collections[0].Settings = map[string]any{"prefix": "new-choice"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	manager.SetEnterpriseAccountManager(runtime)
	proposed.Metadata["access_token"] = "new-authorization"
	saved, _, err := manager.saveEnterpriseAuthorization(t.Context(), proposed)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ = ge.ReadAccount(saved.Metadata)
	if !graph.Collections[0].Disabled || graph.Collections[0].Settings["prefix"] != "new-choice" {
		t.Fatal("CLI authorization replayed constructor settings")
	}
	updated, err := manager.saveEnterpriseDiscovery(t.Context(), base, proposed)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ = ge.ReadAccount(updated.Metadata)
	if !graph.Collections[0].Disabled || graph.Collections[0].Settings["prefix"] != "new-choice" || graph.Discovery["status"] != "ok" || ge.String(updated.Metadata, "access_token") != "new-authorization" {
		t.Fatal("post-login discovery rolled back concurrent settings or OAuth")
	}
}
