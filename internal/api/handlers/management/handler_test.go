package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"golang.org/x/crypto/bcrypt"
)

func TestMiddlewareAllowsValidKeyAfterFailedAttempts(t *testing.T) {
	const validKey = "fixture-management-key"
	hash, err := bcrypt.GenerateFromPassword([]byte(validKey), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, addr  string
		allowRemote bool
	}{
		{name: "loopback", addr: "127.0.0.1:12345"},
		{name: "remote", addr: "192.0.2.10:12345", allowRemote: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.RemoteManagement.AllowRemote = test.allowRemote
			cfg.RemoteManagement.SecretKey = string(hash)
			h := &Handler{cfg: cfg}
			const path = "/v8/management/config"
			engine := gin.New()
			engine.GET(path, h.Middleware(), func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			request := func(key string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.RemoteAddr = test.addr
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, req)
				return response
			}
			for i := range 6 {
				if response := request("wrong-secret"); response.Code != http.StatusUnauthorized ||
					!strings.Contains(response.Body.String(), "invalid management key") {
					t.Fatalf("unexpected result at attempt %d: %d %s", i+1, response.Code, response.Body.String())
				}
			}
			if response := request(""); response.Code != http.StatusUnauthorized {
				t.Fatalf("missing key was not rejected: %d %s", response.Code, response.Body.String())
			}
			if response := request(validKey); response.Code != http.StatusNoContent {
				t.Fatalf("valid key was blocked: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAuthenticateManagementKeyPreservesConfigurationChecks(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	if allowed, code, detail := h.AuthenticateManagementKey(false, "fixture-key"); allowed ||
		code != http.StatusForbidden || detail != "remote management disabled" {
		t.Fatalf("expected remote access to be refused, got allowed=%t status=%d detail=%q", allowed, code, detail)
	}
	h.cfg.RemoteManagement.AllowRemote = true
	if allowed, code, detail := h.AuthenticateManagementKey(false, "fixture-key"); allowed ||
		code != http.StatusForbidden || detail != "remote management key not set" {
		t.Fatalf("expected access without a configured key to be refused, got allowed=%t status=%d detail=%q", allowed, code, detail)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:       &config.Config{},
		envSecret: "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}
