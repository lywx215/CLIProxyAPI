package api

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestNewServerDiagnosticRequestIDMatchesLegacyContextAndLog(t *testing.T) {
	logger := log.StandardLogger()
	oldHooks, oldLevel := logger.Hooks, logger.GetLevel()
	logger.ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel) })
	var ginID, contextID string
	server := newTestServerWithOptions(t, WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
		engine.GET("/v1/diagnostic-fixture", func(c *gin.Context) {
			ginID, contextID = logging.GetGinRequestID(c), logging.GetRequestID(c.Request.Context())
			if diagnostics.ServerFromContext(c.Request.Context()) == nil {
				t.Error("production middleware missing server span")
			}
			c.String(201, "fixture-response")
		})
	}))
	w := httptest.NewRecorder()
	server.engine.ServeHTTP(w, httptest.NewRequest("GET", "/v1/diagnostic-fixture", nil))
	id := w.Header().Get("X-Diag-Request-Id")
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(id) || id != ginID || id != contextID || w.Code != 201 || w.Body.String() != "fixture-response" {
		t.Fatalf("request identity/response: header=%q gin=%q context=%q status=%d body=%q", id, ginID, contextID, w.Code, w.Body.String())
	}
	found := false
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, `"/v1/diagnostic-fixture"`) {
			found = true
			if entry.Data["request_id"] != id {
				t.Fatalf("legacy log ID = %v, want %s", entry.Data["request_id"], id)
			}
		}
	}
	if !found {
		t.Fatal("legacy access log missing")
	}
}
