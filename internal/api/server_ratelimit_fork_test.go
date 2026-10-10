package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestV8RateLimitZeroOverrideRemainsUnlimitedAfterReload(t *testing.T) {
	cfg, err := config.ParseConfigBytes([]byte(`config-version: 8
access:
  api-key-rate-limit:
    enabled: true
    default-rpm: 1
    overrides: [{api-key: unlimited-key, rpm: 0}]
`))
	if err != nil {
		t.Fatal(err)
	}
	limiter := middleware.NewAPIKeyRateLimiter(buildRateLimitConfig(cfg))
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userApiKey", c.GetHeader("X-Test-Key")); c.Next() }, middleware.RateLimitMiddleware(limiter))
	router.GET("/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	request := func(key string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/models", nil)
		req.Header.Set("X-Test-Key", key)
		router.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("key=%s status=%d want=%d body=%s", key, response.Code, want, response.Body.String())
		}
		if want == http.StatusTooManyRequests && response.Header().Get("Retry-After") == "" {
			t.Fatal("limited key lost retry guidance")
		}
	}
	request("ordinary-key", http.StatusOK)
	request("ordinary-key", http.StatusTooManyRequests)
	for i := 0; i < 3; i++ {
		request("unlimited-key", http.StatusOK)
	}
	cfg.APIKeyRateLimit.Overrides = nil
	limiter.UpdateConfig(buildRateLimitConfig(cfg))
	request("unlimited-key", http.StatusOK)
	request("unlimited-key", http.StatusTooManyRequests)
	cfg.APIKeyRateLimit.Overrides = []config.APIKeyRateLimitEntry{{APIKey: "unlimited-key", RPM: 0}}
	limiter.UpdateConfig(buildRateLimitConfig(cfg))
	request("unlimited-key", http.StatusOK)
	request("unlimited-key", http.StatusOK)
}
