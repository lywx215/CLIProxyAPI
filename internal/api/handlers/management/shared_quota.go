package management

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ResetQuotaV8 adds an explicit versioned pool operation without changing v0.
func (h *Handler) ResetQuotaV8(c *gin.Context) {
	raw, errRead := io.ReadAll(io.LimitReader(c.Request.Body, 16*1024+1))
	var req struct {
		AuthIndex   string `json:"auth_index"`
		ResetScope  string `json:"reset_scope"`
		PoolID      string `json:"pool_id"`
		PoolVersion uint64 `json:"pool_version"`
	}
	if errRead != nil || len(raw) > 16*1024 || json.Unmarshal(raw, &req) != nil {
		c.JSON(400, gin.H{"error": "invalid request body"})
		return
	}
	if req.ResetScope == "" || req.ResetScope == "credential" {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		h.ResetQuota(c)
		return
	}
	if req.ResetScope != "shared_pool" {
		c.JSON(400, gin.H{"error": "unsupported reset_scope"})
		return
	}
	if h.authManager == nil {
		c.JSON(503, gin.H{"error": "core auth manager unavailable"})
		return
	}
	a := h.authByIndex(strings.TrimSpace(req.AuthIndex))
	if a == nil {
		c.JSON(404, gin.H{"error": "auth not found"})
		return
	}
	result, errReset := h.authManager.ResetSharedQuota(c.Request.Context(), a.ID, req.PoolID, req.PoolVersion)
	if errReset != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "shared pool changed or unavailable; refresh credentials before retrying"})
		return
	}
	c.JSON(http.StatusOK, result)
}
