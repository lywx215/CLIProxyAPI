package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func enterpriseCarbonManagementFailure(c *gin.Context, err error) {
	var policy *coreauth.EnterpriseModelPolicyError
	if errors.As(err, &policy) {
		enterpriseManagementFailure(c, policy.HTTPStatus, policy.Code, policy.Message)
		return
	}
	var evidence *coreauth.EnterpriseCarbonEvidenceError
	if errors.As(err, &evidence) {
		enterpriseManagementFailure(c, evidence.StatusCode(), evidence.Code, evidence.Message)
		return
	}
	enterpriseManagementFailure(c, http.StatusInternalServerError, "carbon_save_failed", "Could not save Carbon application state; reload and try again")
}

// testGeminiEnterpriseCarbonModel is the only HTTP entry that creates the
// internal closed-application test context. Ordinary request headers cannot do so.
func (h *Handler) testGeminiEnterpriseAdmissionModel(c *gin.Context, auth *coreauth.Auth, req enterpriseModelTestRequest, revision uint64, model ge.AdmissionModel) {
	if revision == 0 {
		if account, err := ge.AccountFromLegacy(auth.Metadata); err == nil {
			for _, collection := range account.Collections {
				if collection.ID == req.BindingID {
					revision = collection.Revision
					break
				}
			}
		}
	}
	if req.BindingID == "" || req.ExpectedRevision == 0 || revision != req.ExpectedRevision {
		enterpriseManagementFailure(c, 409, "credential_changed", strings.ReplaceAll("Select the current application and revision before testing Carbon", "Carbon", model.Label))
		return
	}
	snapshot, err := h.authManager.BeginEnterpriseAdmissionTestContext(c.Request.Context(), auth, revision, model)
	if err != nil {
		enterpriseCarbonManagementFailure(c, err)
		return
	}
	defer h.authManager.CancelEnterpriseCarbonTest(snapshot)
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		enterpriseManagementFailure(c, 500, "test_unavailable", "Could not create an independent model verification code")
		return
	}
	nonce := strings.ToUpper(model.Label) + "_" + hex.EncodeToString(random[:])
	body, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "Please return exactly this verification code and nothing else: " + nonce}}}}})
	usageContext := c.Copy()
	usageContext.Set("userApiKey", "")
	ctx := context.WithValue(c.Request.Context(), "gin", usageContext)
	ctx = coreauth.WithEnterpriseAdmissionTest(ctx, auth.ID, snapshot.Resources, model)
	started := time.Now()
	response, errExec := h.authManager.Execute(ctx, []string{ge.Provider}, coreexec.Request{Model: model.ID, Payload: body, Format: sdktranslator.FormatGemini}, coreexec.Options{
		SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, OriginalRequest: body,
		Metadata: map[string]any{coreexec.PinnedAuthMetadataKey: auth.ID, coreexec.OperationMetadataKey: "generate_content", "source": "management_carbon_model_test"},
	})
	if c.Request.Context().Err() != nil {
		return
	}
	result := enterpriseModelTestResult{Status: "ok", Model: strings.TrimSpace(req.Model), AuthIndex: lockedAuthIndex(auth), Resources: snapshot.Resources,
		CheckedAt: time.Now().UTC(), ElapsedMS: time.Since(started).Milliseconds(), HTTPStatus: 200, Code: "ok",
		Message: strings.ReplaceAll("Carbon completed its fixed-application verification", "Carbon", model.Label), BindingID: req.BindingID, Revision: revision,
		ExpectedRevision: req.ExpectedRevision, CarbonSupported: true, CarbonTestID: snapshot.TestID}
	evidence := &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", HTTPStatus: 200}}
	if errExec != nil {
		result = enterpriseModelTestError(result, errExec)
		evidence.Status, evidence.Reason, evidence.HTTPStatus = "failed", strings.ToLower(result.Code), result.HTTPStatus
		if result.Code == "MODEL_NOT_ENABLED" {
			evidence.Status, evidence.Reason = "needs_review", "model_not_enabled"
		}
		var conflict *coreauth.EnterpriseModelProbeError
		if errors.As(errExec, &conflict) {
			result.Code, result.Message, result.HTTPStatus = "model_changed", strings.ReplaceAll("Request configuration changed the fixed Carbon model or binding", "Carbon", model.Label), 409
			evidence.Status, evidence.Reason, evidence.HTTPStatus = "incomplete", "model_changed", 409
		}
	} else if !enterpriseCarbonNonceMatches(response.Payload, nonce) {
		result.Status, result.HTTPStatus, result.Code = "failed", 502, "verification_incomplete"
		result.Message = strings.ReplaceAll("Carbon did not return a complete matching verification code", "Carbon", model.Label)
		evidence.Status, evidence.Reason, evidence.HTTPStatus = "incomplete", result.Code, result.HTTPStatus
	} else {
		actual, reason, status := h.verifyEnterpriseAdmissionSession(ctx, auth.ID, response.Metadata, nonce, model.ID)
		if c.Request.Context().Err() != nil {
			return
		}
		if reason != "" {
			result.Status, result.HTTPStatus, result.Code = "failed", status, reason
			result.Message = "The generated response could not be verified against the same application's saved model selection"
			evidence.Status, evidence.Reason, evidence.HTTPStatus = "incomplete", reason, result.HTTPStatus
			if reason == "model_verification_unavailable" {
				evidence.Status = "failed"
			}
		}
		evidence.UpstreamModel, result.UpstreamModel = actual, actual
	}
	updated, errSave := h.authManager.RecordEnterpriseCarbonTestEvidence(c.Request.Context(), snapshot, evidence)
	if c.Request.Context().Err() != nil {
		return
	}
	if errSave != nil {
		enterpriseCarbonManagementFailure(c, errSave)
		return
	}
	if errHook := h.invokePostAuthPersistHooks(c.Request.Context(), []*coreauth.Auth{updated}); errHook != nil {
		enterpriseManagementFailure(c, 500, "runtime_sync_failed", strings.ReplaceAll("Carbon verification was saved, but runtime synchronization failed; reload", "Carbon", model.Label))
		return
	}
	current, exists := h.enterpriseTarget(req.Name, req.AuthIndex, req.BindingID)
	if !exists {
		enterpriseManagementFailure(c, 409, "credential_changed", "Application changed during verification; reload")
		return
	}
	admission := h.authManager.EnterpriseModelAdmissionView(current, model)
	if model.ID == ge.HydrogenModel {
		result.HydrogenSupported, result.HydrogenAdmission, result.HydrogenTestID = true, &admission, result.CarbonTestID
		result.CarbonSupported, result.CarbonTestID = false, ""
	} else {
		result.CarbonAdmission = &admission
	}
	if admission.Test != nil && result.Status == "ok" {
		result.CheckedAt = admission.Test.CheckedAt
		if model.ID == ge.HydrogenModel {
			result.HydrogenTestID = admission.Test.TestID
		} else {
			result.CarbonTestID = admission.Test.TestID
		}
	}
	if result.Code == "model_changed" {
		c.JSON(http.StatusConflict, result)
		return
	}
	c.JSON(200, result)
}

func enterpriseCarbonNonceMatches(payload []byte, nonce string) bool {
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
	if json.Unmarshal(payload, &response) != nil || len(response.Candidates) != 1 || response.Candidates[0].FinishReason != "STOP" {
		return false
	}
	var text strings.Builder
	for _, part := range response.Candidates[0].Content.Parts {
		if !part.Thought {
			text.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(text.String()) == nonce
}

var enterpriseCarbonSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

// verifyEnterpriseCarbonSession performs a read only for this explicit Carbon
// test. The saved binding determines the origin and path, never upstream URLs.
func (h *Handler) verifyEnterpriseAdmissionSession(ctx context.Context, authID string, metadata map[string]any, nonce string, modelID string) (string, string, int) {
	session, _ := metadata["enterprise_carbon_session"].(string)
	model, _ := metadata["enterprise_carbon_model"].(string)
	if model != modelID || session == "" {
		return "", "model_verification_incomplete", http.StatusBadGateway
	}
	current, ok := h.authManager.GetByID(authID)
	if !ok || current.Provider != ge.Provider || current.Disabled || current.Status == coreauth.StatusDisabled {
		return "", "credential_changed", http.StatusConflict
	}
	binding := ge.FromMetadata(current.Metadata)
	prefix := ge.Engine(binding) + "/sessions/"
	if !strings.HasPrefix(session, prefix) || !enterpriseCarbonSessionID.MatchString(strings.TrimPrefix(session, prefix)) {
		return "", "model_verification_incomplete", http.StatusBadGateway
	}
	fresh, err := h.authManager.EnsureEnterpriseAccountToken(ctx, authID)
	if err != nil {
		return "", "credential_unavailable", http.StatusUnauthorized
	}
	if ge.Resources(ge.FromMetadata(fresh.Metadata)) != ge.Resources(binding) {
		return "", "credential_changed", http.StatusConflict
	}
	client, err := newGeminiEnterpriseResourceClient(h.enterpriseProxy(fresh))
	if err != nil {
		return "", "model_verification_unavailable", http.StatusServiceUnavailable
	}
	defer client.CloseIdleConnections()
	endpoint := ge.OriginForLocation(binding.Location) + "/v1alpha/" + session + "?fields=" + url.QueryEscape("name,turns(query(text),queryConfig)")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "model_verification_unavailable", http.StatusServiceUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+ge.String(fresh.Metadata, "access_token"))
	request.Header.Set("X-Goog-User-Project", binding.ProjectID)
	response, err := client.Do(request)
	if err != nil {
		return "", "model_verification_unavailable", http.StatusServiceUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500 {
		return "", "model_verification_unavailable", response.StatusCode
	}
	if response.StatusCode == http.StatusForbidden {
		return "", "permission_denied", http.StatusForbidden
	}
	if response.StatusCode == http.StatusUnauthorized {
		return "", "credential_unavailable", http.StatusUnauthorized
	}
	if response.StatusCode != http.StatusOK {
		return "", "model_verification_incomplete", http.StatusBadGateway
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return "", "model_verification_unavailable", http.StatusServiceUnavailable
	}
	if len(data) > 2*1024*1024 {
		return "", "model_verification_incomplete", http.StatusBadGateway
	}
	var saved struct {
		Name  string `json:"name"`
		Turns []struct {
			Query struct {
				Text string `json:"text"`
			} `json:"query"`
			QueryConfig map[string]any `json:"queryConfig"`
		} `json:"turns"`
	}
	if json.Unmarshal(data, &saved) != nil || saved.Name != session {
		return "", "model_verification_incomplete", http.StatusBadGateway
	}
	matched := ""
	for _, turn := range saved.Turns {
		if !strings.Contains(turn.Query.Text, nonce) {
			continue
		}
		actual, valid := turn.QueryConfig["google.discoveryengine.googleapis.com.Assistant.model_id"].(string)
		if !valid || actual == "" {
			return "", "model_verification_incomplete", http.StatusBadGateway
		}
		if matched != "" && matched != actual {
			return "", "model_verification_mismatch", http.StatusBadGateway
		}
		matched = actual
	}
	if matched != modelID {
		return matched, "model_verification_mismatch", http.StatusBadGateway
	}
	return matched, "", http.StatusOK
}

func (h *Handler) testGeminiEnterpriseCarbonModel(c *gin.Context, auth *coreauth.Auth, req enterpriseModelTestRequest, revision uint64) {
	h.testGeminiEnterpriseAdmissionModel(c, auth, req, revision, ge.CarbonAdmissionModel)
}
func (h *Handler) verifyEnterpriseCarbonSession(ctx context.Context, authID string, metadata map[string]any, nonce string) (string, string, int) {
	return h.verifyEnterpriseAdmissionSession(ctx, authID, metadata, nonce, ge.CarbonModel)
}
