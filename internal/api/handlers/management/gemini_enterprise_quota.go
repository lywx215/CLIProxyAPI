package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

var newGeminiEnterpriseQuotaClient = ge.Client
var refreshGeminiEnterpriseQuotaToken = ge.Refresh

// GetGeminiEnterpriseQuota reports soft quota states without changing credential health.
func (h *Handler) GetGeminiEnterpriseQuota(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	result := ge.QuotaSnapshot{Status: "unavailable", ObservedAt: time.Now().UTC(), Pools: []ge.QuotaPool{}}
	respond := func() { c.JSON(http.StatusOK, result) }
	auth := h.authByIndex(strings.TrimSpace(c.Query("auth_index")))
	if auth == nil || auth.Provider != ge.Provider || auth.Disabled {
		respond()
		return
	}
	binding := ge.FromMetadata(auth.Metadata)
	if binding.Validate() != nil {
		respond()
		return
	}
	h.mu.Lock()
	proxy := ""
	if h.cfg != nil {
		proxy = h.cfg.ProxyURL
	}
	h.mu.Unlock()
	if auth.ProxyURL != "" {
		proxy = auth.ProxyURL
	}
	token := ge.String(auth.Metadata, "access_token")
	expiry, errExpiry := time.Parse(time.RFC3339Nano, ge.String(auth.Metadata, "expiry"))
	if token == "" || errExpiry != nil || !expiry.After(time.Now().Add(time.Minute)) {
		// Quota reads refresh only in memory and do not mutate auth routing or stored credentials.
		refreshed, errRefresh := refreshGeminiEnterpriseQuotaToken(c.Request.Context(), binding, ge.String(auth.Metadata, "refresh_token"), proxy)
		if errRefresh != nil {
			respond()
			return
		}
		token = refreshed.AccessToken
	}
	client, errClient := newGeminiEnterpriseQuotaClient(proxy)
	if errClient != nil {
		respond()
		return
	}
	defer client.CloseIdleConnections()
	result = ge.FetchQuota(c.Request.Context(), client, binding, token, time.Now())
	respond()
}
