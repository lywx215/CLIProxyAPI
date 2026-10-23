package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestManagementV8EnterpriseQuotaRequiresManagementAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "synthetic-quota-management")
	cfg := &config.Config{}
	h := management.NewHandlerWithoutConfigFilePath(cfg, nil)
	h.SetLocalPassword("synthetic-quota-management")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementV8Routes()
	for _, key := range []string{"", "synthetic-quota-management"} {
		req := httptest.NewRequest(http.MethodGet, "/v8/management/quota/gemini-enterprise?auth_index=missing", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		recorder := httptest.NewRecorder()
		s.engine.ServeHTTP(recorder, req)
		want := http.StatusUnauthorized
		if key != "" {
			want = http.StatusOK
		}
		if recorder.Code != want {
			t.Fatalf("quota route status = %d, want %d", recorder.Code, want)
		}
	}
}
