package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

// Pause serialization after the store has checked the original authorization.
// This schedules deletion at the former check-to-write race without changing production code.
type enterpriseDeleteTokenBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *enterpriseDeleteTokenBarrier) MarshalJSON() ([]byte, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return json.Marshal("fixture-id-token")
}

func TestEnterpriseExactRenewalSerializesSourceDeletion(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		format := "account"
		if legacy {
			format = "legacy"
		}
		for _, mode := range []string{"single", "batch", "all"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				directory := t.TempDir()
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(directory)
				manager := coreauth.NewManager(store, nil, nil)
				record := sdkAuth.NewGeminiEnterpriseRecord(
					config.GeminiEnterpriseOAuth{ClientID: "fixture-old", ClientSecretEnv: "FIXTURE_OLD_SECRET",
						ProjectID: "fixture-project", EngineID: "fixture-engine"},
					"legacy@example.test", &oauth2.Token{AccessToken: "fixture-access",
						RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour)}, nil)
				if !legacy {
					graph, err := ge.AccountFromLegacy(record.Metadata)
					if err != nil {
						t.Fatal(err)
					}
					record.Metadata = ge.PutAccount(record.Metadata, graph)
				}
				base, err := manager.Register(t.Context(), record)
				if err != nil {
					t.Fatal(err)
				}
				path := base.Attributes[coreauth.AttributePath]
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// A separate store instance exercises the cross-process file lock,
				// not only the in-process store mutex.
				deleter := sdkAuth.NewFileTokenStore()
				deleter.SetBaseDir(directory)
				h := &Handler{cfg: &config.Config{AuthDir: directory}, tokenStore: deleter, authManager: manager}
				barrier := &enterpriseDeleteTokenBarrier{entered: make(chan struct{}), release: make(chan struct{})}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
				t.Cleanup(release)
				incoming := base.Clone()
				incoming.Metadata["access_token"] = "fixture-renewed-access"
				incoming.Metadata["id_token"] = barrier
				renewalDone := make(chan error, 1)
				go func() {
					_, err := manager.ReauthorizeExistingGeminiEnterpriseCredential(t.Context(), base, incoming)
					renewalDone <- err
				}()
				select {
				case <-barrier.entered:
				case err := <-renewalDone:
					t.Fatalf("renewal did not reach persistence: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("renewal did not reach persistence")
				}

				query := "name=" + url.QueryEscape(base.FileName)
				if mode == "batch" {
					query += "&name=missing.json"
				} else if mode == "all" {
					query = "all=true"
				}
				deleteFile := func() *httptest.ResponseRecorder {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Set(ConfigV8ContextKey, true)
					c.Request = httptest.NewRequest(http.MethodDelete, "/v8/management/credentials?"+query, nil)
					h.DeleteAuthFile(c)
					return recorder
				}
				deletionDone := make(chan *httptest.ResponseRecorder, 1)
				go func() { deletionDone <- deleteFile() }()
				var rejected *httptest.ResponseRecorder
				select {
				case rejected = <-deletionDone:
				case <-time.After(5 * time.Second):
					release()
					<-renewalDone
					<-deletionDone
					t.Fatal("deletion bypassed the file lock and waited on runtime removal")
				}
				expected := http.StatusConflict
				if mode == "batch" {
					expected = http.StatusMultiStatus
				}
				if rejected.Code != expected {
					t.Fatalf("concurrent deletion status = %d, want %d", rejected.Code, expected)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("rejected deletion changed the credential source")
				}
				if _, exists := manager.GetByID(base.ID); !exists {
					t.Fatal("rejected deletion removed the runtime credential")
				}
				release()
				if err = <-renewalDone; err != nil {
					t.Fatal(err)
				}
				current, _ := manager.GetByID(base.ID)
				if ge.String(current.Metadata, "client_id") != "fixture-old" ||
					ge.String(current.Metadata, "engine_id") != "fixture-engine" {
					t.Fatal("renewal changed the original application or resource binding")
				}

				deleted := deleteFile()
				expected = http.StatusOK
				if mode == "batch" {
					expected = http.StatusMultiStatus
				}
				if deleted.Code != expected {
					t.Fatalf("completed deletion status = %d, want %d", deleted.Code, expected)
				}
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("successful deletion left a durable credential")
				}
				if _, exists := manager.GetByID(base.ID); exists {
					t.Fatal("successful deletion left a runtime credential")
				}
				if _, err = manager.ReauthorizeExistingGeminiEnterpriseCredential(t.Context(), base, incoming); !errors.Is(err, coreauth.ErrEnterpriseAccountChanged) {
					t.Fatalf("deleted target accepted stale renewal: %v", err)
				}
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("stale renewal recreated a deleted credential")
				}
			})
		}
	}
}
