package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	access "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type webAdmissionAccess struct{ calls int }

func (*webAdmissionAccess) Identifier() string { return "web-admission-test" }
func (p *webAdmissionAccess) Authenticate(ctx context.Context, r *http.Request) (*access.Result, *access.AuthError) {
	p.calls++
	if r.Header.Get("Authorization") != "Bearer synthetic-client" {
		return nil, access.NewInvalidCredentialError()
	}
	return &access.Result{Principal: "synthetic-client", Provider: p.Identifier()}, nil
}
func TestEnterpriseWebAdmissionAuthenticatesOnceBeforeReserving(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "web-admission-native"
	registry.GetGlobalRegistry().RegisterClient("web-admission", "gemini-enterprise", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient("web-admission")
	for _, media := range []bool{false, true} {
		for _, authorized := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "text", true: "media"}[media], map[bool]string{false: "unauthorized", true: "authorized"}[authorized]}, "/"), func(t *testing.T) {
				m := auth.NewManager(nil, nil, nil)
				m.SetConfig(&config.Config{GeminiEnterpriseWeb: config.GeminiEnterpriseWebConfig{Enabled: true}})
				p := &webAdmissionAccess{}
				am := access.NewManager()
				am.SetProviders([]access.Provider{p})
				r := gin.New()
				r.Use(middleware.EnterpriseWebBodyAdmission(m, func(c *gin.Context) bool {
					ok := authenticateAccessRequest(am, c, false)
					if ok {
						c.Set("enterprise_web_access_authorized", true)
					}
					return ok
				}), AuthMiddleware(am))
				r.POST("/v1beta/models/*action", func(c *gin.Context) {
					if (web.ExistingLease(c.Request.Context()) != nil) != media {
						t.Error("incorrect lease")
					}
					c.Status(200)
				})
				payload := `{"contents":[{"parts":[{"text":"ordinary"}]}]}`
				if media {
					payload = `{"contents":[{"parts":[{"text":"mixed"},{"inlineData":{"mimeType":"image/png","data":"YQ=="}}]}]}`
				}
				req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":countTokens", strings.NewReader(payload))
				if authorized {
					req.Header.Set("Authorization", "Bearer synthetic-client")
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				want := 401
				if authorized {
					want = 200
				}
				if w.Code != want || p.calls != 1 {
					t.Fatal(w.Code, p.calls, w.Body.String())
				}
				state := m.WebRuntime().Status()
				if state["active_requests"] != 0 || state["inflight_bytes"] != int64(0) || state["rejected_concurrency"] != uint64(0) {
					t.Fatal("unauthorized or completed request retained capacity", state)
				}
			})
		}
	}
}
