package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// GetAntigravityStats returns Antigravity channel request statistics.
// Query parameters:
//   - view: "auth" (default), "model", or "detail"
//   - auth_id: optional filter by auth ID
func (h *Handler) GetAntigravityStats(c *gin.Context) {
	view := strings.ToLower(strings.TrimSpace(c.DefaultQuery("view", "auth")))
	authID := strings.TrimSpace(c.Query("auth_id"))

	summary := coreauth.GetStatsSummary()

	switch view {
	case "model":
		stats := coreauth.GetModelSummary()
		c.JSON(http.StatusOK, gin.H{
			"view":    "model",
			"stats":   stats,
			"summary": summary,
		})

	case "detail":
		var stats []*coreauth.RequestStatEntry
		if authID != "" {
			stats = coreauth.GetStatsByAuth(authID)
		} else {
			stats = coreauth.GetAllStats()
		}
		c.JSON(http.StatusOK, gin.H{
			"view":    "detail",
			"stats":   stats,
			"summary": summary,
		})

	default: // "auth"
		if authID != "" {
			// Return detail view filtered by auth
			stats := coreauth.GetStatsByAuth(authID)
			c.JSON(http.StatusOK, gin.H{
				"view":    "auth_detail",
				"auth_id": authID,
				"stats":   stats,
				"summary": summary,
			})
			return
		}
		stats := coreauth.GetAuthSummary()
		c.JSON(http.StatusOK, gin.H{
			"view":    "auth",
			"stats":   stats,
			"summary": summary,
		})
	}
}

// ResetAntigravityStats clears Antigravity request statistics.
// Query parameters:
//   - auth_id: optional, reset only the specified auth's stats
func (h *Handler) ResetAntigravityStats(c *gin.Context) {
	authID := strings.TrimSpace(c.Query("auth_id"))

	var cleared int
	if authID != "" {
		cleared = coreauth.ResetStatsByAuth(authID)
		c.JSON(http.StatusOK, gin.H{
			"message":         "antigravity stats reset for auth",
			"auth_id":         authID,
			"cleared_entries": cleared,
		})
	} else {
		cleared = coreauth.ResetAllStats()
		c.JSON(http.StatusOK, gin.H{
			"message":         "antigravity stats reset",
			"cleared_entries": cleared,
		})
	}
}
