package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// EnterpriseWebBodyAdmission runs before request-body logging; no media body is copied by the logger.
func EnterpriseWebBodyAdmission(m *auth.Manager, authorize ...func(*gin.Context) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if m == nil || c.Request.Method != "POST" || !strings.HasPrefix(c.Request.URL.Path, "/v1beta/models/") {
			c.Next()
			return
		}
		model, action, ok := strings.Cut(strings.TrimPrefix(c.Request.URL.Path, "/v1beta/models/"), ":")
		if !ok || (action != "generateContent" && action != "streamGenerateContent" && action != "countTokens") {
			c.Next()
			return
		}
		model, eligible := handlers.EnterpriseWebNativeModel(m, model)
		if !eligible {
			c.Next()
			return
		}
		if encoding := c.GetHeader("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
			c.AbortWithStatusJSON(415, gin.H{"error": "Native Enterprise requests require uncompressed JSON"})
			return
		}
		beforeMedia := func() error {
			if len(authorize) > 0 && authorize[0] != nil && !authorize[0](c) {
				return web.Fail(401, "Authentication required")
			}
			return nil
		}
		fallbackEligibility := func() (bool, bool) {
			if action == "countTokens" || m.HomeEnabled() || handlers.PinnedAuthIDFromContext(c.Request.Context()) != "" {
				return false, false
			}
			return true, m.EnterpriseWebHasEligibleCredential(model)
		}
		body, ctx, lease, err := web.ReadGenerationBody(c.Request.Context(), c.Request.Body, c.Request.ContentLength, m.WebRuntime(), beforeMedia, fallbackEligibility)
		if err != nil {
			if c.IsAborted() {
				return
			}
			code := 503
			var s interface{ StatusCode() int }
			if errors.As(err, &s) {
				code = s.StatusCode()
			}
			c.Abort()
			// Match executor errors and expose only safe local field paths/reasons.
			c.Data(code, "application/json", handlers.BuildErrorResponseBodyWithError(code, "", err))
			return
		}
		if lease != nil {
			defer lease.Release()
		}
		_, projected := web.MediaFallbackFrom(ctx)
		if !projected {
			ctx = web.WithBody(ctx, body)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Set("enterprise_web_body_managed", lease != nil || projected)
		c.Request.Body = http.NoBody
		if lease == nil {
			c.Request.ContentLength = int64(len(body))
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		c.Next()
	}
}
