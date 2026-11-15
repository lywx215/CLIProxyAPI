package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type enterpriseImportTransport func(*http.Request) (*http.Response, error)

func (f enterpriseImportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type enterpriseImportTest struct {
	h            *Handler
	store        *sdkAuth.FileTokenStore
	calls        int
	scope        string
	email        string
	onRefresh    func()
	refreshError bool
}

func newEnterpriseImportTest(t *testing.T) *enterpriseImportTest {
	t.Helper()
	store := sdkAuth.NewFileTokenStore()
	directory := t.TempDir()
	store.SetBaseDir(directory)
	cfg := &config.Config{AuthDir: directory}
	cfg.OAuth.Providers.GeminiEnterprise = &config.GeminiEnterpriseOAuth{ClientID: "test-client", ClientSecretEnv: "TEST_GE_IMPORT_SECRET", ProjectID: "legacy-project", EngineID: "legacy-app"}
	t.Setenv("TEST_GE_IMPORT_SECRET", "synthetic-client-secret")
	x := &enterpriseImportTest{h: &Handler{cfg: cfg, authManager: coreauth.NewManager(store, nil, nil), tokenStore: store}, store: store,
		email: "test@example.test", scope: "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile"}
	before := newGeminiEnterpriseOAuthClient
	t.Cleanup(func() { newGeminiEnterpriseOAuthClient = before })
	newGeminiEnterpriseOAuthClient = func(string) (*http.Client, error) {
		return &http.Client{Transport: enterpriseImportTransport(func(r *http.Request) (*http.Response, error) {
			var result any
			switch r.URL.Host + r.URL.Path {
			case "oauth2.googleapis.com/token":
				x.calls++
				if x.refreshError {
					return nil, errors.New("synthetic transport interruption")
				}
				if x.onRefresh != nil {
					x.onRefresh()
				}
				result = map[string]any{"access_token": "synthetic-private-access", "refresh_token": "synthetic-private-rotated", "expires_in": 3600, "token_type": "Bearer"}
			case "www.googleapis.com/oauth2/v2/userinfo":
				result = map[string]any{"id": "stable-subject", "email": x.email, "verified_email": true}
			case "oauth2.googleapis.com/tokeninfo":
				if r.Method != "POST" {
					t.Errorf("tokeninfo method = %s", r.Method)
				}
				if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer synthetic-private-access" {
					t.Error("tokeninfo must carry authorization only in its bearer header")
				}
				result = map[string]any{"aud": "test-client", "sub": "stable-subject", "scope": x.scope}
			default:
				t.Errorf("unexpected external operation: %s %s", r.Method, r.URL.Host+r.URL.Path)
				return nil, errors.New("unexpected external operation")
			}
			data, _ := json.Marshal(result)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		})}, nil
	}
	return x
}

func (x *enterpriseImportTest) request(id string) enterpriseOAuthImportRequest {
	return enterpriseOAuthImportRequest{RequestID: id, Mode: "create", Email: "test@example.test", Subject: "stable-subject", ClientID: "test-client", RefreshToken: "synthetic-private-refresh"}
}

func importResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err, recorder.Body.String())
	}
	for _, secret := range []string{"synthetic-private", "synthetic-client-secret"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatal("response leaked authorization material")
		}
	}
	return body
}

func (x *enterpriseImportTest) post(t *testing.T, r enterpriseOAuthImportRequest) map[string]any {
	t.Helper()
	return importResponse(t, enterpriseManagementRequest(t, x.h.ImportGeminiEnterpriseOAuth, "POST", r))
}

func (x *enterpriseImportTest) get(t *testing.T, id string) map[string]any {
	t.Helper()
	router := gin.New()
	router.GET("/imports/:request_id", x.h.GetGeminiEnterpriseOAuthImport)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/imports/"+id, nil))
	return importResponse(t, w)
}

func (x *enterpriseImportTest) runtimeSync(t *testing.T, id, body string) map[string]any {
	t.Helper()
	router := gin.New()
	router.POST("/imports/:request_id/runtime-sync", x.h.SyncGeminiEnterpriseOAuthImportRuntime)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/imports/"+id+"/runtime-sync", strings.NewReader(body)))
	return importResponse(t, w)
}

func (x *enterpriseImportTest) root(t *testing.T) *coreauth.Auth {
	t.Helper()
	for _, row := range x.h.authManager.List() {
		if coreauth.IsEnterpriseAccountAuth(row) && !coreauth.IsEnterpriseCollectionAuth(row) {
			return row
		}
	}
	t.Fatal("account not installed")
	return nil
}

func TestEnterpriseOAuthImportCreateReplayAndRestart(t *testing.T) {
	x := newEnterpriseImportTest(t)
	r := x.request("create_request_0001")
	first := x.post(t, r)
	if first["status"] != "saved" || first["authorization_revision"] != float64(1) {
		t.Fatalf("create: %v", first)
	}
	root := x.root(t)
	if len(x.h.authManager.EnterpriseCollections(root.ID)) != 0 {
		t.Fatal("import seeded resources from server config")
	}
	if x.post(t, r)["status"] != "saved" || x.calls != 1 {
		t.Fatal("duplicate import refreshed or installed again")
	}
	r.RefreshToken = "other-synthetic-private-refresh"
	if x.post(t, r)["code"] != "request_id_conflict" || x.calls != 1 {
		t.Fatal("request ID was reused")
	}
	rows, err := x.store.List(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("journals treated as credentials: %d %v", len(rows), err)
	}
	x.h.authManager = coreauth.NewManager(x.store, nil, nil)
	if result := x.get(t, r.RequestID); result["status"] != "saved" {
		t.Fatalf("restart receipt: %v", result)
	}
	data, err := os.ReadFile(enterpriseJournalPath(x.h.cfg.AuthDir, r.RequestID))
	if err != nil || strings.Contains(string(data), "synthetic-") {
		t.Fatal("unsafe or missing journal", err)
	}
}

func TestEnterpriseOAuthImportReauthorizePreservesConcurrentSettings(t *testing.T) {
	x := newEnterpriseImportTest(t)
	x.post(t, x.request("create_request_0002"))
	root := x.root(t)
	r := x.request("reauth_request_0002")
	r.Mode, r.ExpectedRevision = "reauthorize", 1
	r.Target.Name, r.Target.AccountID, r.Target.AuthIndex = root.FileName, ge.AccountID(r.Email, r.ClientID), root.Index
	x.onRefresh = func() {
		current, _ := x.h.authManager.GetByID(root.ID)
		current.Disabled, current.Status = true, coreauth.StatusDisabled
		current.Metadata["model_policy"] = "catalog"
		if _, err := x.h.authManager.Update(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		_, err := x.h.authManager.UpdateEnterpriseAccount(t.Context(), root, 0, func(graph *ge.Account) error {
			binding := ge.ResourceBinding{ProjectID: "123", Location: "eu", CollectionID: "default_collection", EngineID: "existing", AssistantID: "default_assistant"}
			graph.Collections = append(graph.Collections, ge.Collection{ID: ge.CollectionID(binding), Revision: 1, ResourceBinding: binding, Disabled: true, Settings: map[string]any{"model_policy": "catalog", "hydrogen_manual_enabled": true}})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	result := x.post(t, r)
	if result["status"] != "saved" || result["authorization_revision"] != float64(2) {
		t.Fatalf("reauthorize: %v", result)
	}
	root = x.root(t)
	if !root.Disabled || root.Metadata["model_policy"] != "catalog" {
		t.Fatal("account settings lost")
	}
	children := x.h.authManager.EnterpriseCollections(root.ID)
	if len(children) != 1 || !children[0].Disabled {
		t.Fatalf("application graph lost: %+v", children)
	}
	x.onRefresh = nil
	r.RequestID = "stale_request_0002"
	if x.post(t, r)["code"] != "authorization_target_changed" || x.calls != 2 {
		t.Fatal("stale revision was refreshed")
	}
}

func TestEnterpriseOAuthImportRejectsIdentityScopesAndClient(t *testing.T) {
	for _, scenario := range []string{"identity", "scope", "client", "secret"} {
		t.Run(scenario, func(t *testing.T) {
			x := newEnterpriseImportTest(t)
			r := x.request("rejected_request_001")
			want := "oauth_application_not_ready"
			switch scenario {
			case "identity":
				x.email, want = "other@example.test", "oauth_identity_mismatch"
			case "scope":
				x.scope, want = "openid", "oauth_scope_missing"
			case "client":
				r.ClientID = "other-client"
			case "secret":
				t.Setenv("TEST_GE_IMPORT_SECRET", "")
			}
			result := x.post(t, r)
			if result["error"] != want {
				t.Fatalf("%s: %v", want, result)
			}
			if len(x.h.authManager.List()) != 0 {
				t.Fatal("invalid grant installed")
			}
			if (scenario == "client" || scenario == "secret") && x.calls != 0 {
				t.Fatal("invalid application made Google request")
			}
		})
	}
}

type enterpriseLostSaveAck struct{ *sdkAuth.FileTokenStore }

func (s enterpriseLostSaveAck) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	path, err := s.FileTokenStore.Save(ctx, auth)
	if err != nil {
		return path, err
	}
	return path, errors.New("synthetic lost save acknowledgment")
}

func TestEnterpriseOAuthImportLostSaveAcknowledgment(t *testing.T) {
	x := newEnterpriseImportTest(t)
	store := enterpriseLostSaveAck{x.store}
	x.h.authManager, x.h.tokenStore = coreauth.NewManager(store, nil, nil), store
	r := x.request("lost_save_ack_0001")
	result := x.post(t, r)
	if result["status"] != "saved" || result["runtime_synced"] != false {
		t.Fatalf("durable receipt not honored: %v", result)
	}
	if len(x.h.authManager.List()) != 0 || x.get(t, r.RequestID)["runtime_synced"] != false || len(x.h.authManager.List()) != 0 {
		t.Fatal("read-only receipt unexpectedly published the grant")
	}
	x.h.authManager = coreauth.NewManager(x.store, nil, nil)
	if x.get(t, r.RequestID)["status"] != "saved" || x.post(t, r)["status"] != "saved" || x.calls != 1 {
		t.Fatal("save acknowledgment loss caused replay")
	}
	x.h.authManager = coreauth.NewManager(store, nil, nil)
	publications := 0
	x.h.authManager.SetEnterpriseAccountSyncHook(func(ctx context.Context, auth *coreauth.Auth) error {
		publications++
		if !coreauth.IsEnterpriseOAuthRuntimeSync(ctx) || coreauth.EnterpriseOAuthAuthorizationRevision(auth) != 1 {
			t.Error("runtime publication was not fenced to the saved grant")
		}
		return nil
	})
	if rejected := x.runtimeSync(t, r.RequestID, "{}"); rejected["code"] != "invalid_runtime_sync" || publications != 0 {
		t.Fatalf("runtime sync accepted/wrote request body: %v", rejected)
	}
	if result = x.runtimeSync(t, r.RequestID, ""); result["runtime_synced"] != true || result["authorization_revision"] != float64(1) {
		t.Fatalf("runtime recovery: %v", result)
	}
	before := x.root(t)
	if result = x.runtimeSync(t, r.RequestID, ""); result["runtime_synced"] != true || x.calls != 1 || publications != 1 {
		t.Fatalf("repeated runtime recovery: %v, refreshes=%d, publications=%d", result, x.calls, publications)
	}
	after := x.root(t)
	if before.Generation != after.Generation || before.CredentialVersion != after.CredentialVersion ||
		x.get(t, r.RequestID)["runtime_synced"] != true {
		t.Fatal("repeated runtime recovery rewrote the authorization")
	}
}

func TestEnterpriseOAuthRuntimeSyncRejectsSupersededAuthorization(t *testing.T) {
	x := newEnterpriseImportTest(t)
	first := x.request("runtime_original_0001")
	if result := x.post(t, first); result["runtime_synced"] != true {
		t.Fatalf("import: %v", result)
	}
	before := x.root(t)
	grant := before.Clone()
	grant.Metadata["access_token"] = "synthetic-private-new-native"
	if _, err := x.h.authManager.ReauthorizeEnterpriseAccount(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	if result := x.get(t, first.RequestID); result["status"] != "saved" || result["runtime_synced"] != false {
		t.Fatalf("old receipt must remain historical: %v", result)
	}
	if result := x.runtimeSync(t, first.RequestID, ""); result["code"] != "authorization_target_changed" {
		t.Fatalf("superseded grant was accepted for publication: %v", result)
	}
	if after := x.root(t); coreauth.EnterpriseOAuthAuthorizationRevision(after) != 2 || !coreauth.CredentialsChanged(before, after) || x.calls != 1 {
		t.Fatal("stale publication changed the new authorization or refreshed")
	}
}

func TestEnterpriseOAuthRuntimeSyncRequiresSuccessfulRegistrationHook(t *testing.T) {
	x := newEnterpriseImportTest(t)
	r := x.request("runtime_hook_failure_01")
	x.h.SetPostAuthPersistHook(func(context.Context, *coreauth.Auth) error { return errors.New("synthetic hook failure") })
	if result := x.post(t, r); result["status"] != "saved" || result["runtime_synced"] != false {
		t.Fatalf("import: %v", result)
	}
	ready, publications := false, 0
	x.h.authManager.SetEnterpriseAccountSyncHook(func(context.Context, *coreauth.Auth) error {
		publications++
		if !ready {
			return errors.New("synthetic runtime registration failure")
		}
		return nil
	})
	before := x.root(t)
	if result := x.runtimeSync(t, r.RequestID, ""); result["code"] != "runtime_sync_pending" {
		t.Fatalf("failed hook: %v", result)
	}
	if result := x.get(t, r.RequestID); result["runtime_synced"] != false || publications != 1 {
		t.Fatalf("failed hook was reported ready: %v, publications=%d", result, publications)
	}
	ready = true
	if result := x.runtimeSync(t, r.RequestID, ""); result["runtime_synced"] != true || publications != 2 || x.calls != 1 {
		t.Fatalf("recover hook: %v, publications=%d, refreshes=%d", result, publications, x.calls)
	}
	if after := x.root(t); before.Generation != after.Generation || before.CredentialVersion != after.CredentialVersion {
		t.Fatal("recovery replaced an existing exact runtime credential")
	}
}

func TestEnterpriseOAuthImportUnknownRefreshBlocksDifferentRequest(t *testing.T) {
	x := newEnterpriseImportTest(t)
	x.refreshError = true
	r := x.request("unknown_refresh_001")
	if x.post(t, r)["status"] != "reconcile_required" {
		t.Fatal("transport interruption was reported definitive")
	}
	x.refreshError = false
	if x.post(t, r)["status"] != "reconcile_required" || x.calls != 1 {
		t.Fatal("unknown result replayed")
	}
	r.RequestID = "different_request_01"
	if x.post(t, r)["code"] != "previous_import_unresolved" || x.calls != 1 {
		t.Fatal("new request bypassed resource quarantine")
	}
	if _, err := os.Stat(filepath.Join(x.h.cfg.AuthDir, ".enterprise-oauth-imports")); err != nil {
		t.Fatal(err)
	}
}
