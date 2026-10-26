package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

type enterpriseTestTransport func(*http.Request) (*http.Response, error)

func (f enterpriseTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func enterpriseTestBinding() config.GeminiEnterpriseOAuth {
	return config.GeminiEnterpriseOAuth{ClientID: "synthetic.apps.googleusercontent.com", ProjectID: "test-project", EngineID: "test-engine"}.WithDefaults()
}

func TestEnterpriseV8StartAndManualCallback(t *testing.T) {
	binding := enterpriseTestBinding()
	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir(), OAuth: config.OAuthConfig{Providers: config.OAuthProviders{GeminiEnterprise: &binding}}}}
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.POST("/v8/management/oauth/callback", h.PostOAuthCallback)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=gemini-enterprise&client_id=ignored", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("start status: %d %s", response.Code, response.Body.String())
	}
	var start struct {
		URL, State  string
		CallbackURL string `json:"callback_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	defer CancelOAuthSession(start.State)
	parsed, _ := url.Parse(start.URL)
	query := parsed.Query()
	if parsed.Host != "accounts.google.com" || query.Get("client_id") != binding.ClientID || query.Get("redirect_uri") != geminiEnterpriseCallbackURL || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("state") != start.State || query.Get("access_type") != "offline" || start.CallbackURL != geminiEnterpriseCallbackURL {
		t.Fatal("authorization URL did not retain configured binding, PKCE, offline access and callback")
	}
	body, _ := json.Marshal(map[string]string{"provider": ge.Provider, "redirect_url": geminiEnterpriseCallbackURL + "?state=" + start.State + "&error=access_denied"})
	callback := httptest.NewRecorder()
	router.ServeHTTP(callback, httptest.NewRequest(http.MethodPost, "/v8/management/oauth/callback", strings.NewReader(string(body))))
	if callback.Code != http.StatusOK {
		t.Fatalf("manual callback status: %d", callback.Code)
	}
	// Cancel before the worker can exchange or persist anything.
	CancelOAuthSession(start.State)
}

func TestEnterpriseStartRejectsIncompleteConfigAndHome(t *testing.T) {
	for _, home := range []bool{false, true} {
		binding := enterpriseTestBinding()
		cfg := &config.Config{AuthDir: t.TempDir()}
		if home {
			cfg.OAuth.Providers.GeminiEnterprise = &binding
			cfg.Home.Enabled = true
		}
		h := &Handler{cfg: cfg}
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		h.RequestGeminiEnterpriseToken(c)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("home=%v: %d", home, response.Code)
		}
	}
}

func TestEnterpriseCallbackReceiverSharesPortAndChecksState(t *testing.T) {
	receiver := &enterpriseCallbackReceiver{}
	dir := t.TempDir()
	RegisterOAuthSession("enterprise-callback-one", ge.Provider)
	RegisterOAuthSession("enterprise-callback-two", ge.Provider)
	defer CancelOAuthSession("enterprise-callback-one")
	defer CancelOAuthSession("enterprise-callback-two")
	release1, automatic1 := receiver.acquire("enterprise-callback-one", dir, 0)
	defer release1()
	release2, automatic2 := receiver.acquire("enterprise-callback-two", dir, 0)
	defer release2()
	if !automatic1 || !automatic2 || len(receiver.sessions) != 2 {
		t.Fatal("concurrent receiver registration failed")
	}
	for _, state := range []string{"unknown", "enterprise-callback-one", "enterprise-callback-two"} {
		response := httptest.NewRecorder()
		receiver.callback(response, httptest.NewRequest(http.MethodGet, geminiEnterpriseCallbackURL+"?state="+state+"&code=synthetic-code", nil))
		if state == "unknown" {
			if response.Code != http.StatusBadRequest {
				t.Fatal("unknown callback was accepted")
			}
			continue
		}
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "synthetic-code") {
			t.Fatal("callback acknowledgement invalid or leaked code")
		}
		path := filepath.Join(dir, ".oauth-"+ge.Provider+"-"+state+".oauth")
		result, err := waitEnterpriseCallback(context.Background(), state, path)
		if err != nil || result.State != state || result.Code != "synthetic-code" {
			t.Fatal("callback was not delivered to its matching session")
		}
	}
}

func TestEnterpriseOccupiedCallbackPortLeavesItsOwnerAlone(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	receiver := &enterpriseCallbackReceiver{}
	release, automatic := receiver.acquire("enterprise-port-fixture", t.TempDir(), listener.Addr().(*net.TCPAddr).Port)
	release()
	if automatic || receiver.server != nil {
		t.Fatal("occupied callback port must use manual submission")
	}
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal("another port owner's listener was interrupted")
	}
	_ = connection.Close()
}

func TestEnterpriseManagementExchangePersistenceAndRedaction(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "save", true: "cancel-during-exchange"}[cancelled], func(t *testing.T) {
			binding := enterpriseTestBinding()
			state := "enterprise-exchange-fixture"
			RegisterOAuthSession(state, ge.Provider)
			defer CancelOAuthSession(state)
			h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}, tokenStore: sdkAuth.NewFileTokenStore()}
			original := newGeminiEnterpriseOAuthClient
			t.Cleanup(func() { newGeminiEnterpriseOAuthClient = original })
			newGeminiEnterpriseOAuthClient = func(string) (*http.Client, error) {
				return &http.Client{Transport: enterpriseTestTransport(func(request *http.Request) (*http.Response, error) {
					body := `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expires_in":3600}`
					if request.URL.Host == "oauth2.googleapis.com" {
						if err := request.ParseForm(); err != nil {
							t.Fatal(err)
						}
						if request.Form.Get("code_verifier") != "fixture-verifier" || request.Form.Get("client_id") != binding.ClientID || request.Form.Get("redirect_uri") != geminiEnterpriseCallbackURL {
							t.Fatal("exchange lost configured binding or PKCE")
						}
						if cancelled {
							CancelOAuthSession(state)
						}
					} else if request.URL.Host == "www.googleapis.com" {
						if request.Header.Get("Authorization") != "Bearer synthetic-access" {
							t.Fatal("userinfo authorization missing")
						}
						body = `{"email":"fixture@example.com","verified_email":true}`
					} else {
						t.Fatalf("unexpected upstream: %s", request.URL.Host)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}, nil
			}
			oauth, err := ge.OAuth(binding, geminiEnterpriseCallbackURL)
			if err != nil {
				t.Fatal(err)
			}
			h.finishGeminiEnterpriseLogin(context.Background(), state, binding, oauth, "fixture-verifier", "fixture-code", "")
			files, err := os.ReadDir(h.cfg.AuthDir)
			if err != nil {
				t.Fatal(err)
			}
			if cancelled {
				if len(files) != 0 {
					t.Fatal("cancelled session persisted credentials")
				}
				return
			}
			_, _, _, _, completed, ok := GetOAuthSessionDetails(state)
			if !ok || !completed || len(files) != 1 {
				t.Fatal("credential was not saved before session completion")
			}
			data, _ := os.ReadFile(filepath.Join(h.cfg.AuthDir, files[0].Name()))
			var metadata map[string]any
			if json.Unmarshal(data, &metadata) != nil || metadata["engine_id"] != binding.EngineID || metadata["refresh_token"] != "synthetic-refresh" {
				t.Fatal("saved record lost binding or refresh credential")
			}
			public, _ := json.Marshal(geminiEnterpriseCredentialSummary(metadata))
			if strings.Contains(string(public), "synthetic-access") || strings.Contains(string(public), "synthetic-refresh") || strings.Contains(string(public), "client_secret") {
				t.Fatal("credential summary leaked a secret")
			}
		})
	}
}

func TestEnterpriseManagementExchangeFailureDoesNotExposeBody(t *testing.T) {
	binding := enterpriseTestBinding()
	state := "enterprise-exchange-error"
	RegisterOAuthSession(state, ge.Provider)
	defer CancelOAuthSession(state)
	original := newGeminiEnterpriseOAuthClient
	t.Cleanup(func() { newGeminiEnterpriseOAuthClient = original })
	newGeminiEnterpriseOAuthClient = func(string) (*http.Client, error) {
		return &http.Client{Transport: enterpriseTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"sensitive-upstream-body"}`))}, nil
		})}, nil
	}
	oauth, _ := ge.OAuth(binding, geminiEnterpriseCallbackURL)
	h := &Handler{}
	h.finishGeminiEnterpriseLogin(context.Background(), state, binding, oauth, oauth2.GenerateVerifier(), "synthetic-code", "")
	_, status, ok := GetOAuthSession(state)
	if !ok || !strings.Contains(status, "invalid_grant") || strings.Contains(status, "sensitive-upstream-body") {
		t.Fatal("exchange error not safely actionable")
	}
}

func TestEnterpriseCredentialListMetadata(t *testing.T) {
	for _, fromDisk := range []bool{false, true} {
		t.Run(map[bool]string{false: "manager", true: "disk"}[fromDisk], func(t *testing.T) {
			dir := t.TempDir()
			binding := enterpriseTestBinding()
			metadata := ge.Metadata(binding, "fixture@example.com", &oauth2.Token{
				AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour),
			})
			metadata["client_secret"] = "synthetic-secret"
			metadata["id_token"] = "synthetic-id-token"
			metadata["client_secret_env"] = "SYNTHETIC_SECRET_ENV"
			path := filepath.Join(dir, "enterprise.json")
			data, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			h := &Handler{cfg: &config.Config{AuthDir: dir}}
			if !fromDisk {
				h.authManager = coreauth.NewManager(nil, nil, nil)
				_, err := h.authManager.Register(context.Background(), &coreauth.Auth{
					ID: "enterprise-list", FileName: "enterprise.json", Provider: ge.Provider,
					Attributes: map[string]string{"path": path}, Metadata: metadata,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			router := gin.New()
			router.GET("/v8/management/credentials", func(c *gin.Context) {
				c.Set(ConfigV8ContextKey, true)
				h.ListAuthFiles(c)
			})
			router.GET("/v0/management/auth-files", h.ListAuthFiles)
			for _, route := range []string{"/v8/management/credentials", "/v0/management/auth-files"} {
				for _, query := range []string{"", "?page=1&page_size=1"} {
					response := httptest.NewRecorder()
					router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route+query, nil))
					var body struct {
						Files []map[string]json.RawMessage `json:"files"`
					}
					if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Files) != 1 {
						t.Fatalf("invalid credential list response for %s%s", route, query)
					}
					for _, secret := range []string{"synthetic-access", "synthetic-refresh", "synthetic-secret", "synthetic-id-token", "SYNTHETIC_SECRET_ENV"} {
						if strings.Contains(response.Body.String(), secret) {
							t.Fatalf("credential list exposed %s", secret)
						}
					}
					summary, exists := body.Files[0]["gemini_enterprise"]
					if strings.HasPrefix(route, "/v0/") {
						if exists {
							t.Fatal("Enterprise summary changed the retired v0 contract")
						}
						continue
					}
					var public map[string]any
					if !exists || json.Unmarshal(summary, &public) != nil || len(public) < 10 || public["client_id"] != binding.ClientID || public["project_id"] != binding.ProjectID || public["engine_id"] != binding.EngineID || public["assistant_id"] != binding.AssistantID || public["has_refresh_token"] != true || public["resource_edit_supported"] != true || public["endpoint"] != ge.OriginForLocation(binding.Location) {
						t.Fatal("v8 credential list lost the public binding or renewal capability")
					}
				}
			}
		})
	}
}

type enterpriseSaveStore struct {
	*sdkAuth.FileTokenStore
	save func(context.Context, *coreauth.Auth) (string, error)
}

func (s *enterpriseSaveStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	return s.save(ctx, auth)
}

func TestEnterpriseManagementSaveOutcomePrecedesSuccess(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		t.Run(map[bool]string{false: "persist", true: "save-error"}[failSave], func(t *testing.T) {
			state := "enterprise-save-outcome"
			RegisterOAuthSession(state, ge.Provider)
			defer CancelOAuthSession(state)
			original := newGeminiEnterpriseOAuthClient
			t.Cleanup(func() { newGeminiEnterpriseOAuthClient = original })
			newGeminiEnterpriseOAuthClient = func(string) (*http.Client, error) {
				return &http.Client{Transport: enterpriseTestTransport(func(request *http.Request) (*http.Response, error) {
					body := `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","expires_in":3600}`
					if request.URL.Host == "www.googleapis.com" {
						body = `{"email":"fixture@example.com","verified_email":true}`
					} else if request.URL.Host != "oauth2.googleapis.com" {
						return nil, errors.New("unexpected test upstream")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}, nil
			}
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			store := &enterpriseSaveStore{FileTokenStore: sdkAuth.NewFileTokenStore()}
			store.save = func(ctx context.Context, auth *coreauth.Auth) (string, error) {
				close(entered)
				<-release
				if failSave {
					return "", errors.New("synthetic-sensitive-save-error")
				}
				return store.FileTokenStore.Save(ctx, auth)
			}
			h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}, tokenStore: store}
			oauth, err := ge.OAuth(enterpriseTestBinding(), geminiEnterpriseCallbackURL)
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				defer close(finished)
				h.finishGeminiEnterpriseLogin(context.Background(), state, enterpriseTestBinding(), oauth, "fixture-verifier", "fixture-code", "")
			}()
			defer func() { unblock(); <-finished }()
			select {
			case <-entered:
			case <-finished:
				t.Fatal("OAuth worker finished without attempting persistence")
			}
			router := gin.New()
			router.GET("/status", h.GetAuthStatus)
			router.DELETE("/session", h.CancelAuthSession)
			router.POST("/callback", h.PostOAuthCallback)
			request := func(method, path, body string) string {
				t.Helper()
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
				return response.Body.String()
			}
			if got := request(http.MethodGet, "/status?state="+state, ""); got != `{"status":"wait"}` {
				t.Fatalf("success was reported before persistence: %s", got)
			}
			if got := request(http.MethodDelete, "/session?state="+state, ""); got != `{"cancelled":false,"status":"ok"}` {
				t.Fatalf("cancellation interrupted claimed persistence: %s", got)
			}
			callback := httptest.NewRecorder()
			router.ServeHTTP(callback, httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(`{"provider":"gemini-enterprise","state":"`+state+`","code":"duplicate-code"}`)))
			if callback.Code != http.StatusConflict {
				t.Fatal("a duplicate callback was accepted during persistence")
			}
			unblock()
			<-finished
			outcome := request(http.MethodGet, "/status?state="+state, "")
			files, err := os.ReadDir(h.cfg.AuthDir)
			if err != nil {
				t.Fatal(err)
			}
			if failSave {
				if len(files) != 0 || !strings.Contains(outcome, `"status":"error"`) || strings.Contains(outcome, "synthetic-sensitive-save-error") {
					t.Fatal("persistence failure did not report a safe error without creating credentials")
				}
			} else if len(files) != 1 || outcome != `{"status":"ok"}` {
				t.Fatal("persisted credentials did not produce a completed session")
			}
		})
	}
}

func TestEnterpriseSaveAndCancelCannotBothWin(t *testing.T) {
	sessions := newOAuthSessionStore(time.Minute)
	sessions.Register("cancel-first", ge.Provider)
	if !sessions.Cancel("cancel-first") || sessions.ClaimSave("cancel-first", ge.Provider) {
		t.Fatal("cancelled session can still start persistence")
	}
	sessions.Register("save-first", ge.Provider)
	if !sessions.ClaimSave("save-first", ge.Provider) || sessions.Cancel("save-first") || sessions.IsPending("save-first", ge.Provider) {
		t.Fatal("cancellation or duplicate callbacks can interrupt claimed persistence")
	}
	sessions.Complete("save-first")
	session, ok := sessions.Get("save-first")
	if !ok || !session.Completed {
		t.Fatal("claimed persistence did not report its final outcome")
	}
}
