package gemini

import (
	"context"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func init() {
	enterpriseWebPerformanceSetup = func(m *auth.Manager, cfg *config.Config, enabled bool) gin.HandlerFunc {
		cfg.GeminiEnterpriseWeb = (config.GeminiEnterpriseWebConfig{
			Enabled: enabled, HistoryMode: os.Getenv("ENTERPRISE_WEB_BENCH_HISTORY_MODE"),
			CookieCCPollingEnabled: enabled && os.Getenv("ENTERPRISE_WEB_BENCH_CC_POLLING") == "1",
		}).WithDefaults()
		m.SetConfig(cfg)
		c := web.Credential{ModelPolicy: web.PolicyExcludeArgon, Revision: 1, AvailableModels: []web.AvailableModel{{DisplayName: "gemini-3.8-flash", ID: "gemini-3.8-flash"}}}
		_ = c.SetCookies("SID=synthetic; SAPISID=synthetic-signing")
		_ = c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/synthetic")
		c.SetCatalog(c.AvailableModels, time.Now())
		a := &auth.Auth{ID: "web-text-performance-cookie", Provider: web.Provider, Status: auth.StatusActive, Metadata: map[string]any{"enterprise_web": c.MetadataValue()}}
		_, _ = m.Register(context.Background(), a)
		registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash"}})
		return middleware.EnterpriseWebBodyAdmission(m)
	}
}
