package auth_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

type enterprisePreparationStore struct{ saves atomic.Int32 }

func (*enterprisePreparationStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (*enterprisePreparationStore) Delete(context.Context, string) error           { return nil }
func (s *enterprisePreparationStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.saves.Add(1)
	return auth.ID, nil
}

func TestEnterpriseAccountRealPreparationCachesIdentityAcrossRefreshAndClearsOnRebind(t *testing.T) {
	store := &enterprisePreparationStore{}
	manager := coreauth.NewManager(store, nil, nil)
	binding := config.GeminiEnterpriseOAuth{ClientID: "synthetic-client", ProjectID: "123", EngineID: "app"}.WithDefaults()
	metadata := ge.Metadata(binding, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})
	account, err := ge.AccountFromLegacy(metadata)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := manager.Register(t.Context(), &coreauth.Auth{ID: "prepared-account.json", FileName: "prepared-account.json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: ge.PutAccount(metadata, account)})
	if err != nil {
		t.Fatal(err)
	}
	executor := runtimeexec.NewGeminiEnterpriseExecutor(&config.Config{})
	requests := map[string]int{}
	transport := enterpriseRefreshTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || (request.Header.Get("Authorization") != "Bearer synthetic-access" && request.Header.Get("Authorization") != "Bearer synthetic-rotated") {
			return nil, fmt.Errorf("unexpected identity credential or scheme")
		}
		key := request.URL.Host + request.URL.Path
		requests[key]++
		var body string
		switch key {
		case "www.googleapis.com/oauth2/v2/userinfo":
			body = `{"email":"fixture@example.test","verified_email":true}`
		case "cloudresourcemanager.googleapis.com/v3/projects/123":
			body = `{"name":"projects/123","projectId":"fixture-project"}`
		case "discoveryengine.googleapis.com/v1/projects/123/locations/global/userStores/default_user_store/userLicenses":
			body = `{"userLicenses":[{"userPrincipal":"fixture@example.test","licenseAssignmentState":"ASSIGNED","licenseConfig":"projects/123/locations/global/licenseConfigs/assigned"}]}`
		case "discoveryengine.googleapis.com/v1/projects/123/locations/global/licenseConfigs/assigned":
			body = `{"name":"projects/123/locations/global/licenseConfigs/assigned","subscriptionTier":"SEARCH_AND_ASSISTANT","state":"ACTIVE"}`
		default:
			return nil, fmt.Errorf("unexpected metadata request %s", key)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	child := manager.EnterpriseCollections(parent.ID)[0]
	for range 2 {
		prepared, errPrepare := manager.PrepareRequestAuth(ctx, executor, child.Clone())
		if errPrepare != nil || prepared.EnterpriseQuotaIdentity == nil || !prepared.EnterpriseQuotaIdentity.ValidFor(binding) {
			t.Fatalf("real executor identity was discarded: %v", errPrepare)
		}
	}
	for key, count := range requests {
		if count != 1 {
			t.Fatalf("consecutive preparation repeated %s: %d", key, count)
		}
	}
	if len(requests) != 4 || store.saves.Load() != 1 {
		t.Fatal("preparation skipped identity verification or persisted a child")
	}
	rotated := parent.Clone()
	rotated.Metadata["access_token"] = "synthetic-rotated"
	if _, err = manager.UpdateRefreshedAuth(t.Context(), parent, rotated); err != nil {
		t.Fatal(err)
	}
	child = manager.EnterpriseCollections(parent.ID)[0]
	if child.EnterpriseQuotaIdentity == nil || executor.ShouldPrepareRequestAuth(child) {
		t.Fatal("parent token refresh discarded the verified collection identity")
	}
	if _, err = manager.PrepareRequestAuth(ctx, executor, child); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.UpdateEnterpriseAccount(t.Context(), parent, 0, func(account *ge.Account) error { account.Collections[0].EngineID = "replacement-app"; return nil }); err != nil {
		t.Fatal(err)
	}
	child = manager.EnterpriseCollections(parent.ID)[0]
	if child.EnterpriseQuotaIdentity != nil || !executor.ShouldPrepareRequestAuth(child) {
		t.Fatal("application rebind retained the old identity")
	}
	prepared, err := manager.PrepareRequestAuth(ctx, executor, child)
	if err != nil || prepared.EnterpriseQuotaIdentity == nil || !prepared.EnterpriseQuotaIdentity.ValidFor(ge.FromMetadata(child.Metadata)) {
		t.Fatal("replacement binding did not get its own identity", err)
	}
	for key, count := range requests {
		if count != 2 {
			t.Fatalf("refresh repeated identity or rebind missed %s: %d", key, count)
		}
	}
	if store.saves.Load() != 3 {
		t.Fatal("runtime identity preparation persisted account or child OAuth")
	}
}
