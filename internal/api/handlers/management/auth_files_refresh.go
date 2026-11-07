package management

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// RefreshAuthFiles triggers active refresh for a single auth file or all auth files.
// Accepts query parameters (?all=true, ?name=file.json) or JSON body ({"all": true, "name": "file.json"}).
func (h *Handler) RefreshAuthFiles(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		All       bool   `json:"all"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&req); errBind != nil && !errors.Is(errBind, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + errBind.Error()})
			return
		}
	}
	if c.Query("all") == "true" {
		req.All = true
	}
	if queryName := strings.TrimSpace(c.Query("name")); queryName != "" && req.Name == "" {
		req.Name = queryName
	}
	if queryAuthIndex := strings.TrimSpace(c.Query("auth_index")); queryAuthIndex != "" && req.AuthIndex == "" {
		req.AuthIndex = queryAuthIndex
	}

	ctx := c.Request.Context()

	if req.All {
		results := h.authManager.ForceRefreshAll(ctx)
		for i := range results {
			if !results[i].Success {
				continue
			}
			account, ok := h.authManager.GetByID(results[i].ID)
			if !ok || account.Provider != web.Provider {
				continue
			}
			if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{account}); errHook != nil {
				results[i].Success, results[i].Error = false, "Model catalog saved but runtime synchronization is pending"
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"results": results,
		})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or all=true is required"})
		return
	}

	targetAuth, ok := h.lookupAuthFile(name, req.AuthIndex)
	if !ok || targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	refreshed, err := h.authManager.ForceRefreshAuth(ctx, targetAuth.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	if targetAuth.Provider == web.Provider {
		if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{refreshed}); errHook != nil {
			c.JSON(500, gin.H{"error": "Model catalog saved but runtime synchronization is pending; refresh the model list"})
			return
		}
		c.JSON(200, gin.H{"ok": true, "credential": h.addEnterpriseWebSummary(refreshed)})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":   true,
		"auth": refreshed,
	})
}
