package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var newGeminiEnterpriseResourceClient = ge.Client
var refreshGeminiEnterpriseResourceToken = ge.Refresh

// PatchGeminiEnterpriseResources is a v8-only validated resource editor. It
// never changes the OAuth application or creates resources in another region.
func (h *Handler) PatchGeminiEnterpriseResources(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	fail := func(status int, code, message string) {
		c.JSON(status, gin.H{"error": code, "message": message})
	}
	var req struct {
		Name      string              `json:"name"`
		AuthIndex string              `json:"auth_index"`
		Expected  *ge.ResourceBinding `json:"expected"`
		Resources *ge.ResourceBinding `json:"resources"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	if c.ShouldBindJSON(&req) != nil || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.AuthIndex) == "" || req.Expected == nil || req.Resources == nil {
		fail(http.StatusBadRequest, "invalid_resources", "name, auth_index, expected and resources are required")
		return
	}
	h.mu.Lock()
	home, proxy := false, ""
	if h.cfg != nil {
		home, proxy = h.cfg.Home.Enabled, h.cfg.ProxyURL
	}
	h.mu.Unlock()
	if home {
		fail(http.StatusConflict, "home_managed", "Gemini Enterprise resource editing is unavailable in Home mode")
		return
	}
	auth, ok := h.lookupAuthFile(req.Name, req.AuthIndex)
	if !ok || auth == nil || auth.Provider != ge.Provider || coreauth.IsPluginVirtualAuth(auth) {
		fail(http.StatusNotFound, "credential_not_found", "Gemini Enterprise credential not found")
		return
	}
	if coreauth.IsEnterpriseAccountAuth(auth) || coreauth.IsEnterpriseCollectionAuth(auth) {
		fail(http.StatusConflict, "collection_required", "Use the collection editor with its binding identity and revision")
		return
	}
	binding := ge.FromMetadata(auth.Metadata)
	if ge.Resources(req.Expected.Apply(binding)) != ge.Resources(binding) {
		fail(http.StatusConflict, "credential_changed", "Credential resources changed; reload and try again")
		return
	}
	proposed := req.Resources.Apply(binding)
	if proposed.Validate() != nil || (!ge.SupportedLocation(proposed.Location) && proposed.Location != binding.Location) {
		fail(http.StatusBadRequest, "invalid_resources", "Use valid resource IDs and a supported region: global, us, eu, ca, au or in")
		return
	}
	if auth.ProxyURL != "" {
		proxy = auth.ProxyURL
	}
	token := ge.String(auth.Metadata, "access_token")
	expiry, errExpiry := time.Parse(time.RFC3339Nano, ge.String(auth.Metadata, "expiry"))
	if token == "" || errExpiry != nil || !expiry.After(time.Now().Add(time.Minute)) {
		refreshed, errRefresh := refreshGeminiEnterpriseResourceToken(c.Request.Context(), binding, ge.String(auth.Metadata, "refresh_token"), proxy)
		if errRefresh != nil || refreshed == nil || refreshed.AccessToken == "" {
			fail(http.StatusUnprocessableEntity, "credential_unavailable", "Cannot refresh the saved Google authorization; renew it before switching resources")
			return
		}
		token = refreshed.AccessToken
	}
	client, errClient := newGeminiEnterpriseResourceClient(proxy)
	if errClient != nil {
		fail(http.StatusUnprocessableEntity, "resource_validation_failed", "Cannot create the configured Google connection")
		return
	}
	defer client.CloseIdleConnections()
	if errValidate := ge.ValidateResources(c.Request.Context(), client, proposed, token); errValidate != nil {
		fail(http.StatusUnprocessableEntity, "resource_validation_failed", errValidate.Error())
		return
	}
	updated, errUpdate := h.authManager.UpdateEnterpriseResources(c.Request.Context(), auth, ge.Resources(proposed))
	if errUpdate != nil {
		if errors.Is(errUpdate, coreauth.ErrEnterpriseResourcesChanged) {
			fail(http.StatusConflict, "credential_changed", "Credential changed during validation; reload and try again")
		} else {
			fail(http.StatusInternalServerError, "resource_save_failed", "Could not save credential resources")
		}
		return
	}
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(c.Request.Context(), updated); errHook != nil {
			fail(http.StatusInternalServerError, "runtime_sync_failed", "Resources were saved, but runtime synchronization failed; reload the credential")
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "gemini_enterprise": geminiEnterpriseCredentialSummary(updated.Metadata)})
}
