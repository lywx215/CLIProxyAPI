package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestManagementV8GeminiEnterpriseOAuthAccessControl(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	secretHash, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                      string
		enabled, home, authorized bool
		want                      int
	}{
		{"authorized", true, false, true, http.StatusOK},
		{"missing-key", true, false, false, http.StatusUnauthorized},
		{"management-disabled", false, false, true, http.StatusNotFound},
		{"home-mode", true, true, true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := config.GeminiEnterpriseOAuth{ClientID: "synthetic.apps.googleusercontent.com", ProjectID: "fixture-project", EngineID: "fixture-engine"}
			cfg := &config.Config{AuthDir: t.TempDir(), OAuth: config.OAuthConfig{Providers: config.OAuthProviders{GeminiEnterprise: &binding}}}
			cfg.Home.Enabled = tc.home
			cfg.RemoteManagement.SecretKey = string(secretHash)
			h := management.NewHandlerWithoutConfigFilePath(cfg, nil)
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(tc.enabled)
			s.registerManagementRoutes()
			request := httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=gemini-enterprise&is_webui=true&client_id=untrusted", nil)
			request.RemoteAddr = "127.0.0.1:1234"
			if tc.authorized {
				request.Header.Set("Authorization", "Bearer test-password")
			}
			response := httptest.NewRecorder()
			s.engine.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status=%d, want %d", response.Code, tc.want)
			}
			if response.Code != http.StatusOK {
				return
			}
			var login struct {
				URL, State  string
				CallbackURL string `json:"callback_url"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { management.CancelOAuthSession(login.State) })
			parsed, err := url.Parse(login.URL)
			if err != nil {
				t.Fatal(err)
			}
			query := parsed.Query()
			if response.Header().Get("Cache-Control") != "no-store" || parsed.Host != "accounts.google.com" || query.Get("client_id") != binding.ClientID || query.Get("state") != login.State || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("redirect_uri") != login.CallbackURL || login.CallbackURL != "http://localhost:8788/oauth-callback" || !management.IsOAuthSessionPending(login.State, ge.Provider) {
				t.Fatal("v8 route did not preserve management access control, configured binding, PKCE, or callback contract")
			}
			for _, route := range s.engine.Routes() {
				if route.Path == "/v0/management/gemini-enterprise-auth-url" {
					t.Fatal("Enterprise OAuth registered an unsupported v0 route")
				}
			}
		})
	}
}
