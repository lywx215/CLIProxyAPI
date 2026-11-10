package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type enterpriseWebCredentialRequest struct {
	Name             string          `json:"name"`
	ExpectedRevision uint64          `json:"expected_revision"`
	Label            *string         `json:"label"`
	Cookies          *string         `json:"cookies"`
	PageURL          *string         `json:"page_url"`
	UserAgent        *string         `json:"user_agent"`
	ProxyURL         *string         `json:"proxy_url"`
	Priority         *int            `json:"priority"`
	Disabled         *bool           `json:"disabled"`
	ModelPolicy      json.RawMessage `json:"model_policy"`
}

func (h *Handler) GetEnterpriseWebStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(503, gin.H{"error": "auth manager unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, h.authManager.WebRuntime().Status())
}

func applyEnterpriseWebEdit(a *coreauth.Auth, credential *web.Credential, req enterpriseWebCredentialRequest) error {
	previousFingerprint := credential.Fingerprint()
	if len(req.ModelPolicy) > 0 {
		var policy string
		if json.Unmarshal(req.ModelPolicy, &policy) != nil || !web.ValidModelPolicy(policy) {
			return web.Fail(400, "Web model_policy must be argon_only or exclude_argon")
		}
		credential.ModelPolicy = policy
	}
	if req.Cookies != nil {
		if err := credential.SetCookies(*req.Cookies); err != nil {
			return err
		}
	}
	if req.PageURL != nil {
		if err := credential.SetURL(*req.PageURL); err != nil {
			return err
		}
	}
	if req.UserAgent != nil {
		credential.UserAgent = strings.TrimSpace(*req.UserAgent)
	}
	if req.Label != nil {
		label := strings.TrimSpace(*req.Label)
		if len(label) > 160 || strings.ContainsAny(label, "\r\n\x00") {
			return web.Fail(400, "Invalid credential name")
		}
		a.Label = label
		a.Metadata["label"] = label
	}
	if req.ProxyURL != nil {
		proxy := strings.TrimSpace(*req.ProxyURL)
		if proxy != "" && proxy != "direct" {
			u, err := url.Parse(proxy)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				return web.Fail(400, "Invalid credential proxy")
			}
		}
		a.ProxyURL = proxy
		a.Metadata["proxy_url"] = proxy
	}
	if req.Priority != nil {
		if *req.Priority < -1000000 || *req.Priority > 1000000 {
			return web.Fail(400, "Invalid priority")
		}
		a.Metadata["priority"] = float64(*req.Priority)
		coreauth.ApplyAuthPriorityMetadata(a, a.Metadata)
	}
	if req.Disabled != nil {
		a.Disabled = *req.Disabled
		a.Metadata["disabled"] = a.Disabled
		if a.Disabled {
			a.Status = coreauth.StatusDisabled
		} else {
			a.Status = coreauth.StatusActive
		}
	}
	if credential.Fingerprint() != previousFingerprint {
		credential.ClearCatalog()
		credential.LastRenewedAt = time.Time{}
		credential.LastCCRenewedAt = time.Time{}
	}
	return credential.Validate()
}

// SaveEnterpriseWebCredential accepts write-only cookies and a manual model policy.
func (h *Handler) SaveEnterpriseWebCredential(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(503, gin.H{"error": "auth manager unavailable"})
		return
	}
	var req enterpriseWebCredentialRequest
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		c.JSON(400, gin.H{"error": "invalid credential request"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.JSON(400, gin.H{"error": "invalid credential request"})
		return
	}
	// Share the management status lock with ordinary enable/disable edits.
	h.authStatusMu.Lock()
	defer h.authStatusMu.Unlock()
	var record *coreauth.Auth
	var err error
	if c.Request.Method == http.MethodPatch {
		target, ok := h.lookupAuthFile(req.Name, "")
		if !ok || target == nil || target.Provider != web.Provider {
			c.JSON(404, gin.H{"error": "credential not found"})
			return
		}
		record, err = h.authManager.UpdateEnterpriseWebCredential(c.Request.Context(), target.ID, req.ExpectedRevision, func(a *coreauth.Auth, credential *web.Credential) error {
			return applyEnterpriseWebEdit(a, credential, req)
		})
	} else {
		if len(req.ModelPolicy) == 0 {
			c.JSON(400, gin.H{"error": "Choose argon_only or exclude_argon when creating a Web credential"})
			return
		}
		credential := web.Credential{Revision: 1}
		name := web.Provider + "-" + uuid.NewString() + ".json"
		record = &coreauth.Auth{ID: name, FileName: name, Provider: web.Provider, Label: web.Provider, Status: coreauth.StatusActive, CreatedAt: time.Now(), Metadata: map[string]any{"type": web.Provider}, Attributes: map[string]string{coreauth.AttributePath: filepath.Join(h.cfg.AuthDir, name)}}
		err = applyEnterpriseWebEdit(record, &credential, req)
		if err == nil {
			record.Metadata["enterprise_web"] = credential.MetadataValue()
			_, err = h.saveTokenRecord(c.Request.Context(), record)
		}
		if err == nil {
			err = h.upsertAuthRecord(coreauth.WithSkipPersist(c.Request.Context()), record)
		}
	}
	if err != nil {
		status := 500
		var failure interface{ StatusCode() int }
		if errors.As(err, &failure) {
			status = failure.StatusCode()
		}
		message := "Credential could not be saved"
		if status < 500 {
			message = err.Error()
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	if record == nil {
		c.JSON(409, gin.H{"error": "credential changed; reload before saving"})
		return
	}
	entry := h.buildAuthFileEntry(record)
	if entry == nil {
		entry = gin.H{"name": record.FileName, "type": web.Provider}
	}
	if c.Request.Method == http.MethodPatch {
		if errHook := h.invokePostAuthPersistHooks(c.Request.Context(), []*coreauth.Auth{record}); errHook != nil {
			c.JSON(500, gin.H{"error": "Credential saved but runtime synchronization is pending; reload before editing"})
			return
		}
	}
	h.addEnterpriseWebEntry(c, entry, record)
	c.JSON(200, gin.H{"ok": true, "credential": entry})
}

func (h *Handler) addEnterpriseWebSummary(a *coreauth.Auth) map[string]any {
	credential, err := web.CredentialFrom(a.Metadata)
	if err != nil {
		return map[string]any{"cookie_present": false, "available_models": []web.AvailableModel{}, "invalid": true}
	}
	summary := credential.Summary()
	effective := []string{}
	for _, model := range credential.Catalog() {
		allowed := credential.AllowsModel(model.ID)
		if h.cfg != nil {
			for _, pattern := range h.cfg.OAuthExcludedModels[web.Provider] {
				allowed = allowed && !ge.MatchModelPattern(pattern, model.ID)
			}
		}
		if allowed {
			effective = append(effective, model.ID)
		}
	}
	summary["effective_model_ids"] = effective
	summary["proxy_url"] = a.ProxyURL
	if h.authManager != nil {
		for k, v := range h.authManager.WebRuntime().CredentialStatus(a.ID, credential) {
			summary[k] = v
		}
	}
	if a.Disabled || a.Status == coreauth.StatusDisabled {
		if cc, ok := summary["cookie_cc_check"].(map[string]any); ok {
			cc["status"], cc["reason"] = "paused", "credential_disabled"
			delete(cc, "next_check_at")
		}
	}
	return summary
}
func (h *Handler) addEnterpriseWebEntry(c *gin.Context, entry gin.H, a *coreauth.Auth) {
	if c.GetBool(ConfigV8ContextKey) && a.Provider == web.Provider {
		entry["gemini_enterprise_web"] = h.addEnterpriseWebSummary(a)
	}
}

// Common list actions may change safe controls without replacing cookie metadata.
func (h *Handler) patchEnterpriseWebCommonFields(c *gin.Context, target *coreauth.Auth, fields map[string]json.RawMessage) {
	credential, err := web.CredentialFrom(target.Metadata)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid Web credential"})
		return
	}
	req := enterpriseWebCredentialRequest{}
	for key, value := range fields {
		switch key {
		case "proxy_url":
			var v string
			if json.Unmarshal(value, &v) != nil {
				c.JSON(400, gin.H{"error": "invalid proxy"})
				return
			}
			req.ProxyURL = &v
		case "priority":
			var v int
			if json.Unmarshal(value, &v) != nil {
				c.JSON(400, gin.H{"error": "invalid priority"})
				return
			}
			req.Priority = &v
		default:
			c.JSON(400, gin.H{"error": "Use the dedicated Web credential editor for these fields"})
			return
		}
	}
	if len(fields) == 0 {
		c.JSON(400, gin.H{"error": "no editable fields"})
		return
	}
	_, err = h.authManager.UpdateEnterpriseWebCredential(c.Request.Context(), target.ID, credential.Revision, func(a *coreauth.Auth, v *web.Credential) error { return applyEnterpriseWebEdit(a, v, req) })
	if err != nil {
		status := 500
		var f interface{ StatusCode() int }
		if errors.As(err, &f) {
			status = f.StatusCode()
		}
		c.JSON(status, gin.H{"error": "Credential could not be updated; reload and retry"})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}
