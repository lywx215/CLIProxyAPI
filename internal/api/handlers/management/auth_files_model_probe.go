package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type enterpriseModelTestResult struct {
	Status            string             `json:"status"`
	Model             string             `json:"model"`
	AuthIndex         string             `json:"auth_index"`
	Resources         ge.ResourceBinding `json:"resources"`
	CheckedAt         time.Time          `json:"checked_at"`
	ElapsedMS         int64              `json:"elapsed_ms"`
	HTTPStatus        int                `json:"http_status"`
	Code              string             `json:"code"`
	Message           string             `json:"message"`
	RetryAfterSeconds int64              `json:"retry_after_seconds,omitempty"`
	BindingID         string             `json:"binding_id,omitempty"`
	Revision          uint64             `json:"revision,omitempty"`
}

// TestGeminiEnterpriseModel performs one credential-pinned text generation.
// Catalog visibility alone is never accepted as evidence of runtime availability.
func (h *Handler) TestGeminiEnterpriseModel(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	fail := func(status int, code, message string) {
		c.JSON(status, gin.H{"error": code, "code": code, "message": message})
	}
	var req struct {
		Name             string              `json:"name"`
		AuthIndex        string              `json:"auth_index"`
		Model            string              `json:"model"`
		Expected         *ge.ResourceBinding `json:"expected"`
		BindingID        string              `json:"binding_id"`
		ExpectedRevision uint64              `json:"expected_revision"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	if c.ShouldBindJSON(&req) != nil || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.AuthIndex) == "" || strings.TrimSpace(req.Model) == "" || req.Expected == nil {
		fail(http.StatusBadRequest, "invalid_model_test", "name, auth_index, model and expected resources are required")
		return
	}
	req.Name, req.AuthIndex, req.Model = strings.TrimSpace(req.Name), strings.TrimSpace(req.AuthIndex), strings.TrimSpace(req.Model)
	if h.authManager == nil {
		fail(http.StatusServiceUnavailable, "credential_unavailable", "Credential execution is unavailable")
		return
	}
	h.mu.Lock()
	home := h.cfg != nil && h.cfg.Home.Enabled
	var excluded []string
	if h.cfg != nil {
		excluded = append(excluded, h.cfg.OAuthExcludedModels[ge.Provider]...)
	}
	h.mu.Unlock()
	if home || h.authManager.HomeEnabled() {
		fail(http.StatusConflict, "home_managed", "Gemini Enterprise model testing is unavailable in Home mode")
		return
	}
	auth, ok := h.enterpriseTarget(req.Name, req.AuthIndex, req.BindingID)
	if !ok || auth == nil || auth.Provider != ge.Provider || coreauth.IsPluginVirtualAuth(auth) {
		fail(http.StatusNotFound, "credential_not_found", "Gemini Enterprise credential not found")
		return
	}
	if req.BindingID != "" && lockedAuthIndex(auth) != req.AuthIndex {
		fail(http.StatusConflict, "credential_changed", "The collection index changed; reload and try again")
		return
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		fail(http.StatusConflict, "credential_disabled", "Enable this credential before testing a model")
		return
	}
	revision := uint64(0)
	if _, graph, exists := h.authManager.EnterpriseAccountSnapshot(auth.ID); exists {
		for _, collection := range graph.Collections {
			if collection.ID == coreauth.EnterpriseBindingID(auth) {
				revision = collection.Revision
				break
			}
		}
		if (len(graph.Collections) > 1 && (req.BindingID == "" || req.ExpectedRevision == 0)) || (req.ExpectedRevision != 0 && req.ExpectedRevision != revision) {
			fail(http.StatusConflict, "credential_changed", "Collection changed; reload and try again")
			return
		}
	}
	binding := ge.FromMetadata(auth.Metadata)
	if binding.Validate() != nil || ge.Resources(req.Expected.Apply(binding)) != ge.Resources(binding) {
		fail(http.StatusConflict, "credential_changed", "Credential resources changed; reload and try again")
		return
	}
	modelID := strings.TrimSpace(req.Model)
	var model *registry.ModelInfo
	for _, candidate := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if candidate != nil && candidate.ID == modelID {
			model = candidate
			break
		}
	}
	if model == nil {
		fail(http.StatusBadRequest, "model_not_supported", "The selected model is not registered for this credential")
		return
	}
	actualModel := strings.TrimSpace(model.MetadataModelID)
	if actualModel == "" {
		actualModel = strings.TrimPrefix(modelID, strings.TrimSpace(auth.Prefix)+"/")
	}
	if model.EnterpriseMedia != "" || registry.EnterpriseMediaKind(modelID) != "" || registry.EnterpriseMediaKind(actualModel) != "" || actualModel == "gemini-3-pro-image" || actualModel == "veo-3.1" {
		fail(http.StatusBadRequest, "model_test_unsupported", "Only Enterprise text models can be tested")
		return
	}
	if value := strings.TrimSpace(auth.Attributes["excluded_models"]); value != "" {
		excluded = strings.Split(value, ",")
	}
	for _, pattern := range excluded {
		if enterpriseTestModelMatches(pattern, modelID) || enterpriseTestModelMatches(pattern, actualModel) {
			fail(http.StatusConflict, "model_excluded", "The selected model is excluded for this credential")
			return
		}
	}
	// A detached Gin context supplies normal usage attribution without exposing the
	// management bearer key as a model API key, or sharing mutable handler state.
	usageContext := c.Copy()
	usageContext.Set("userApiKey", "")
	ctx := context.WithValue(c.Request.Context(), "gin", usageContext)
	ctx = coreauth.WithEnterpriseModelProbe(ctx, auth.ID, ge.Resources(binding), actualModel)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"请只回复 OK。"}]}]}`)
	started := time.Now()
	response, errExec := h.authManager.Execute(ctx, []string{ge.Provider}, coreexec.Request{
		Model: modelID, Payload: body, Format: sdktranslator.Format("gemini"),
	}, coreexec.Options{
		SourceFormat: sdktranslator.Format("gemini"), ResponseFormat: sdktranslator.Format("gemini"), OriginalRequest: body,
		Metadata: map[string]any{coreexec.PinnedAuthMetadataKey: auth.ID, coreexec.OperationMetadataKey: "generate_content", "source": "management_model_test"},
	})
	if c.Request.Context().Err() != nil {
		return
	}
	current, exists := h.enterpriseTarget(req.Name, req.AuthIndex, req.BindingID)
	if !exists || current == nil || current.Disabled || current.Status == coreauth.StatusDisabled || current.RegistrationEpoch != auth.RegistrationEpoch || ge.Resources(ge.FromMetadata(current.Metadata)) != ge.Resources(binding) {
		fail(http.StatusConflict, "credential_changed", "Credential resources changed during testing; reload and try again")
		return
	}
	if revision != 0 {
		_, currentGraph, found := h.authManager.EnterpriseAccountSnapshot(current.ID)
		currentRevision := uint64(0)
		if found {
			for _, collection := range currentGraph.Collections {
				if collection.ID == coreauth.EnterpriseBindingID(current) {
					currentRevision = collection.Revision
					break
				}
			}
		}
		if currentRevision != revision {
			fail(http.StatusConflict, "credential_changed", "Collection settings changed during testing; reload and try again")
			return
		}
	}
	result := enterpriseModelTestResult{
		Status: "ok", Model: modelID, AuthIndex: lockedAuthIndex(auth), Resources: ge.Resources(binding),
		CheckedAt: time.Now().UTC(), ElapsedMS: time.Since(started).Milliseconds(), HTTPStatus: http.StatusOK,
		Code: "ok", Message: "Model completed a text generation successfully",
		BindingID: req.BindingID, Revision: revision,
	}
	if errExec != nil {
		var conflict *coreauth.EnterpriseModelProbeError
		if errors.As(errExec, &conflict) {
			if conflict.Code == "model_changed" {
				fail(http.StatusConflict, "model_changed", "Request configuration changed the model being tested; reload and check the model mapping")
			} else {
				fail(http.StatusConflict, "credential_changed", "Credential resources changed; reload and try again")
			}
			return
		}
		result = enterpriseModelTestError(result, errExec)
	} else if !enterpriseModelTestHasText(response.Payload) {
		result.Status, result.HTTPStatus = "failed", http.StatusBadGateway
		result.Code, result.Message = "empty_or_incomplete_response", "The model did not return a completed text response"
	}
	c.JSON(http.StatusOK, result)
}

func enterpriseTestModelMatches(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$").MatchString(model)
}

func enterpriseModelTestHasText(payload []byte) bool {
	var response struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(payload, &response) != nil || len(response.Candidates) == 0 {
		return false
	}
	for _, candidate := range response.Candidates {
		if candidate.FinishReason != "STOP" {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if !part.Thought && strings.TrimSpace(part.Text) != "" {
				return true
			}
		}
	}
	return false
}

func enterpriseModelTestError(result enterpriseModelTestResult, err error) enterpriseModelTestResult {
	result.Status, result.HTTPStatus = "failed", http.StatusBadGateway
	result.Code, result.Message = "generation_failed", "Google did not complete the model test"
	var cooldown interface{ IsModelCooldown() bool }
	if errors.As(err, &cooldown) && cooldown.IsModelCooldown() {
		result.Status, result.HTTPStatus = "cooling", http.StatusTooManyRequests
		result.ElapsedMS = 0
		result.Code, result.Message = "model_cooldown", "The model is cooling down; test again after the cooldown expires"
		var headers interface{ Headers() http.Header }
		if errors.As(err, &headers) {
			result.RetryAfterSeconds, _ = strconv.ParseInt(headers.Headers().Get("Retry-After"), 10, 64)
		}
	}
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() >= 400 && status.StatusCode() <= 599 {
		result.HTTPStatus = status.StatusCode()
	}
	var retry interface{ RetryAfter() *time.Duration }
	if errors.As(err, &retry) {
		if duration := retry.RetryAfter(); duration != nil && *duration > 0 {
			result.RetryAfterSeconds = int64((*duration + time.Second - 1) / time.Second)
		}
	}
	var generation *ge.GenerationError
	var skipped *ge.SkippedError
	if errors.As(err, &skipped) {
		result.Code, result.Message = "upstream_skipped", skipped.SkipMessage()
		return result
	}
	if errors.As(err, &generation) && generation.ModelNotEnabled {
		result.Code, result.Message = "MODEL_NOT_ENABLED", "Google has not enabled this model for the selected application"
		return result
	}
	if result.Status == "cooling" {
		return result
	}
	switch result.HTTPStatus {
	case http.StatusUnauthorized:
		result.Code, result.Message = "credential_unavailable", "The saved Google authorization is unavailable"
	case http.StatusForbidden:
		result.Code, result.Message = "permission_denied", "Google denied access to this model or application"
	case http.StatusTooManyRequests:
		result.Code, result.Message = "rate_limited", "Google rate limits or quota prevent the model test"
	case http.StatusBadRequest:
		result.Code, result.Message = "invalid_request", "The model rejected the configured request"
	}
	return result
}
