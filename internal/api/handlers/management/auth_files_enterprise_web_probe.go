package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// TestEnterpriseWebModel exercises both image understanding and attachment reading.
func (h *Handler) TestEnterpriseWebModel(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	fail := func(status int, code, message string) {
		c.JSON(status, gin.H{"error": code, "code": code, "message": message})
	}
	var req struct {
		Name          string `json:"name"`
		AuthIndex     string `json:"auth_index"`
		Model         string `json:"model"`
		ExpectedModel string `json:"expected_upstream_model"`
		Revision      uint64 `json:"expected_revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || req.Name == "" || req.AuthIndex == "" || strings.TrimSpace(req.Model) == "" || strings.TrimSpace(req.ExpectedModel) == "" || req.Revision == 0 {
		fail(400, "invalid_model_test", "A credential, model and current revision are required")
		return
	}
	if h.authManager == nil {
		fail(503, "credential_unavailable", "Credential execution is unavailable")
		return
	}
	if h.authManager.HomeEnabled() {
		fail(409, "home_managed", "Web model testing is unavailable in Home mode")
		return
	}
	a, ok := h.lookupAuthFile(req.Name, req.AuthIndex)
	if !ok || a.Provider != web.Provider || coreauth.IsPluginVirtualAuth(a) {
		fail(404, "credential_not_found", "Web credential not found")
		return
	}
	credential, err := web.CredentialFrom(a.Metadata)
	if err != nil || credential.Revision != req.Revision || lockedAuthIndex(a) != req.AuthIndex {
		fail(409, "credential_changed", "Credential changed; reload before testing")
		return
	}
	if a.Disabled || a.Status == coreauth.StatusDisabled {
		fail(409, "credential_disabled", "Enable this credential before testing")
		return
	}
	if !h.authManager.WebRuntime().Enabled() {
		fail(409, "channel_disabled", "Enable the Web channel in Configuration before testing")
		return
	}
	if credential.Policy() == web.PolicyUnclassified {
		fail(409, "model_policy_unclassified", "Classify this Web credential before testing models")
		return
	}
	if !credential.AllowsModel(req.ExpectedModel) {
		fail(400, "model_policy_denied", "This Web credential's manual policy does not allow the upstream model")
		return
	}
	actual := ""
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(a.ID) {
		if model != nil && model.ID == req.Model {
			actual = model.MetadataModelID
			if actual == "" {
				actual = strings.TrimPrefix(model.ID, a.Prefix+"/")
			}
			break
		}
	}
	listed := false
	for _, model := range credential.Catalog() {
		listed = listed || model.ID == actual
	}
	if actual == "" || !listed {
		fail(400, "model_not_supported", "Refresh the available model list and choose a listed model")
		return
	}
	if actual != req.ExpectedModel {
		fail(409, "model_changed", "Model alias changed; refresh the model list before testing")
		return
	}
	current := func() bool {
		latest, exists := h.authManager.GetByID(a.ID)
		if !exists || latest.Disabled || latest.Status == coreauth.StatusDisabled || latest.RegistrationEpoch != a.RegistrationEpoch {
			return false
		}
		now, errNow := web.CredentialFrom(latest.Metadata)
		return errNow == nil && now.Revision == req.Revision && now.Fingerprint() == credential.Fingerprint()
	}
	ctx := web.WithModelProbe(c.Request.Context(), a.ID, credential.Fingerprint(), actual, h.authManager.WebRuntime().EvidenceEpoch(a.ID), current)
	usageContext := c.Copy()
	usageContext.Set("userApiKey", "")
	ctx = context.WithValue(ctx, "gin", usageContext)
	body, code, err := web.NewModelProbe()
	if err != nil {
		fail(500, "fixture_unavailable", "Model test inputs could not be prepared")
		return
	}
	started := time.Now()
	response, errExec := h.authManager.Execute(ctx, []string{web.Provider}, coreexec.Request{Model: req.Model, Payload: body, Format: sdktranslator.Format("gemini")}, coreexec.Options{
		SourceFormat: sdktranslator.Format("gemini"), ResponseFormat: sdktranslator.Format("gemini"), OriginalRequest: body,
		Metadata: map[string]any{coreexec.PinnedAuthMetadataKey: a.ID, coreexec.OperationMetadataKey: "generate_content", "source": "management_model_test"},
	})
	if c.Request.Context().Err() != nil {
		return
	}
	if !current() {
		fail(409, "credential_changed", "Credential changed during testing; reload before testing again")
		return
	}
	result := enterpriseModelTestResult{Status: "ok", Model: req.Model, AuthIndex: req.AuthIndex, Revision: req.Revision, CheckedAt: time.Now().UTC(), ElapsedMS: time.Since(started).Milliseconds(), HTTPStatus: 200, Code: "ok", Message: "Model read the generated image and text attachment successfully"}
	if errExec != nil {
		var changed *web.ModelProbeError
		if errors.As(errExec, &changed) {
			fail(409, changed.Code, "Model test binding changed; reload before testing again")
			return
		}
		result = enterpriseModelTestError(result, errExec)
	} else if !web.ModelProbeMatches(response.Payload, code) {
		result.Status, result.HTTPStatus, result.Code, result.Message = "failed", 502, "media_verification_failed", "The model did not return the image colors and attachment verification code correctly"
	}
	c.JSON(200, gin.H{"status": result.Status, "model": result.Model, "upstream_model": actual, "auth_index": result.AuthIndex, "revision": result.Revision, "checked_at": result.CheckedAt, "elapsed_ms": result.ElapsedMS, "http_status": result.HTTPStatus, "code": result.Code, "message": result.Message, "retry_after_seconds": result.RetryAfterSeconds, "provider": web.Provider, "test_kind": "inline_media"})
}
