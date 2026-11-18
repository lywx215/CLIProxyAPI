package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

var modelVerificationRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type enterpriseVerificationRequest struct {
	RequestID             string             `json:"request_id"`
	ModelID               string             `json:"model_id"`
	RuleRevision          uint64             `json:"rule_revision"`
	Name                  string             `json:"name"`
	AuthIndex             string             `json:"auth_index"`
	AccountID             string             `json:"account_id"`
	BindingID             string             `json:"binding_id"`
	ExpectedRevision      uint64             `json:"expected_revision"`
	AuthorizationRevision uint64             `json:"expected_authorization_revision"`
	Expected              ge.ResourceBinding `json:"expected"`
}

type enterpriseVerificationResult struct {
	RequestID                 string   `json:"request_id"`
	ModelID                   string   `json:"model_id"`
	RuleRevision              uint64   `json:"rule_revision"`
	BindingID                 string   `json:"binding_id"`
	State                     string   `json:"state"`
	ActualModelID             string   `json:"actual_model_id"`
	HTTPStatus                int      `json:"http_status"`
	Reason                    string   `json:"reason,omitempty"`
	AssistSkippedReasons      []string `json:"assist_skipped_reasons,omitempty"`
	AssistSkippingModeRetried bool     `json:"assist_skipping_mode_retried,omitempty"`
}

type enterpriseVerificationJournal struct {
	Request enterpriseVerificationRequest  `json:"request"`
	Result  enterpriseVerificationResult   `json:"result"`
	Proof   *coreauth.EnterpriseModelProof `json:"proof,omitempty"`
}

type enterpriseVerificationJournalLocation struct {
	requestID string
	path      string
	shared    coreauth.EnterpriseModelVerificationJournalStore
}

func (location enterpriseVerificationJournalLocation) read(ctx context.Context) (enterpriseVerificationJournal, string, error) {
	var value enterpriseVerificationJournal
	var reader io.Reader
	tag := "file"
	if location.shared != nil {
		data, etag, errRead := location.shared.ReadEnterpriseModelVerification(ctx, location.requestID)
		if errRead != nil {
			return value, "", errRead
		}
		if etag == "" || len(data) > 64*1024 {
			return value, "", errors.New("invalid shared model verification receipt version")
		}
		reader, tag = bytes.NewReader(data), etag
	} else {
		file, errOpen := os.Open(location.path)
		if errOpen != nil {
			return value, "", errOpen
		}
		defer func() { _ = file.Close() }()
		reader = io.LimitReader(file, 64*1024)
	}
	if errDecode := json.NewDecoder(reader).Decode(&value); errDecode != nil {
		return value, "", errDecode
	}
	if value.Request.RequestID != location.requestID || value.Result.RequestID != location.requestID || value.Result.State == "" {
		return value, "", errors.New("invalid Enterprise model verification receipt")
	}
	return value, tag, nil
}

func (location enterpriseVerificationJournalLocation) write(ctx context.Context, value enterpriseVerificationJournal, tag string) error {
	if location.shared == nil {
		return writeEnterpriseJournalJSON(location.path, value, tag == "")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return location.shared.WriteEnterpriseModelVerification(ctx, location.requestID, data, tag)
}

func (h *Handler) enterpriseVerificationLocation(c *gin.Context, id string) (enterpriseVerificationJournalLocation, bool) {
	var location enterpriseVerificationJournalLocation
	if !modelVerificationRequestID.MatchString(id) {
		enterpriseManagementFailure(c, http.StatusBadRequest, "invalid_request_id", "Use a model verification request ID")
		return location, false
	}
	location.requestID = id
	if store := h.authManager.EnterpriseModelVerificationJournalStore(); store != nil {
		location.shared = store
		return location, true
	}
	_, directory, _ := h.enterpriseOAuthSettings()
	if strings.TrimSpace(directory) == "" || !h.authManager.SupportsLocalEnterpriseModelVerifications() {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "verification_store_unavailable", "This credential store has no durable model verification journal")
		return location, false
	}
	location.path = filepath.Join(directory, ".enterprise-model-verifications", id+".receipt")
	return location, true
}

func (h *Handler) configuredVerificationAuth(req enterpriseVerificationRequest) (*coreauth.Auth, bool) {
	auth, ok := h.enterpriseTarget(req.Name, req.AuthIndex, req.BindingID)
	if !ok || auth == nil || !coreauth.IsEnterpriseCollectionAuth(auth) || lockedAuthIndex(auth) != req.AuthIndex || auth.Disabled || auth.Status == coreauth.StatusDisabled ||
		ge.Resources(ge.FromMetadata(auth.Metadata)) != req.Expected {
		return nil, false
	}
	owner, account, exists := h.authManager.EnterpriseAccountSnapshot(auth.ID)
	if !exists || owner.Disabled || owner.Status == coreauth.StatusDisabled || account.ID != req.AccountID ||
		coreauth.EnterpriseOAuthAuthorizationRevision(owner) != req.AuthorizationRevision {
		return nil, false
	}
	for _, collection := range account.Collections {
		if collection.ID == req.BindingID && collection.Revision == req.ExpectedRevision && !collection.Disabled && !collection.Missing && collection.ResourceBinding == req.Expected {
			return auth, true
		}
	}
	return nil, false
}

func (h *Handler) configuredVerificationTarget(req enterpriseVerificationRequest) (config.GeminiEnterpriseModelTarget, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil || h.cfg.GeminiEnterpriseModels.Revision != h.authManager.AppliedEnterpriseModelDefinitionsRevision() {
		return config.GeminiEnterpriseModelTarget{}, false
	}
	definition, ok := h.cfg.GeminiEnterpriseModels.Definition(req.ModelID)
	if !ok {
		return config.GeminiEnterpriseModelTarget{}, false
	}
	for _, target := range definition.Targets {
		resources := ge.ResourceBinding{ProjectID: target.ProjectID, Location: target.Location, CollectionID: target.CollectionID, EngineID: target.EngineID, AssistantID: target.AssistantID}
		if target.AccountID == req.AccountID && target.BindingID == req.BindingID && target.RuleRevision == req.RuleRevision && resources == req.Expected {
			return target, true
		}
	}
	return config.GeminiEnterpriseModelTarget{}, false
}

func (h *Handler) GetConfiguredEnterpriseModelVerification(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	c.Header("Cache-Control", "no-store")
	id := c.Param("request_id")
	location, ok := h.enterpriseVerificationLocation(c, id)
	if !ok {
		return
	}
	journal, _, err := location.read(c.Request.Context())
	if errors.Is(err, os.ErrNotExist) {
		enterpriseManagementFailure(c, http.StatusNotFound, "verification_not_found", "Model verification receipt not found")
		return
	}
	if err != nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "verification_receipt_unavailable", "Could not read the durable verification receipt")
		return
	}
	if journal.Result.State == "runtime_pending" && journal.Proof != nil {
		if auth, exists := h.configuredVerificationAuth(journal.Request); exists && h.authManager.ConfiguredEnterpriseModelPublished(auth, *journal.Proof) {
			journal.Result.State = "published"
			journal.Result.Reason = ""
		}
	}
	c.JSON(http.StatusOK, journal.Result)
}

// VerifyConfiguredEnterpriseModel owns one generation per durable request,
// with one compatibility retry after a confirmed schema rejection.
func (h *Handler) VerifyConfiguredEnterpriseModel(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	c.Header("Cache-Control", "no-store")
	var req enterpriseVerificationRequest
	if !strictEnterpriseJSON(c, &req, 16*1024) || req.ModelID == "" || req.Name == "" || req.AuthIndex == "" || req.AccountID == "" ||
		req.BindingID == "" || req.RuleRevision == 0 || req.ExpectedRevision == 0 || req.AuthorizationRevision == 0 ||
		req.Expected.ProjectID == "" || req.Expected.Location == "" || req.Expected.CollectionID == "" || req.Expected.EngineID == "" || req.Expected.AssistantID == "" {
		enterpriseManagementFailure(c, http.StatusBadRequest, "invalid_model_verification", "Exact application, authorization, rule, model and expected resources are required")
		return
	}
	location, ok := h.enterpriseVerificationLocation(c, req.RequestID)
	if !ok {
		return
	}
	journal := enterpriseVerificationJournal{Request: req, Result: enterpriseVerificationResult{
		RequestID: req.RequestID, ModelID: req.ModelID, BindingID: req.BindingID, RuleRevision: req.RuleRevision,
		State: "reconcile_required", HTTPStatus: http.StatusConflict, Reason: "submission_unconfirmed",
	}}
	if err := location.write(c.Request.Context(), journal, ""); err != nil {
		if errors.Is(err, os.ErrExist) {
			h.replayConfiguredEnterpriseVerification(c, location, req)
			return
		}
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "verification_store_unavailable", "Could not reserve a durable model verification before generation")
		return
	}
	reserved, tag, err := location.read(c.Request.Context())
	if err != nil || reserved.Request != req || !reflect.DeepEqual(reserved.Result, journal.Result) {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "verification_receipt_unavailable", "Could not confirm the verification reservation before generation")
		return
	}
	h.runConfiguredEnterpriseVerification(c, location, tag, journal)
}

func (h *Handler) replayConfiguredEnterpriseVerification(c *gin.Context, location enterpriseVerificationJournalLocation, req enterpriseVerificationRequest) {
	journal, tag, err := location.read(c.Request.Context())
	if err != nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "verification_receipt_unavailable", "Could not read the existing verification reservation")
		return
	}
	if journal.Request != req {
		enterpriseManagementFailure(c, http.StatusConflict, "verification_id_conflict", "This verification request ID belongs to a different scope")
		return
	}
	if journal.Result.State == "runtime_pending" {
		h.publishConfiguredEnterpriseVerification(c.Request.Context(), location, tag, &journal)
	}
	c.JSON(http.StatusOK, journal.Result)
}

func (h *Handler) runConfiguredEnterpriseVerification(c *gin.Context, location enterpriseVerificationJournalLocation, tag string, journal enterpriseVerificationJournal) {
	req := journal.Request
	result := &journal.Result
	result.State, result.HTTPStatus = "failed", http.StatusConflict
	auth, exact := h.configuredVerificationAuth(req)
	target, defined := h.configuredVerificationTarget(req)
	if !exact || !defined {
		result.Reason = "model_scope_changed"
		h.finishConfiguredEnterpriseVerification(c, location, tag, journal)
		return
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		result.Reason, result.HTTPStatus = "verification_unavailable", http.StatusServiceUnavailable
		h.finishConfiguredEnterpriseVerification(c, location, tag, journal)
		return
	}
	nonce := "VERIFY_" + hex.EncodeToString(random[:])
	query := "Why does water expand when it freezes? Please explain in one short sentence. Verification reference: " + nonce
	usageContext := c.Copy()
	usageContext.Set("userApiKey", "")
	ctx := context.WithValue(c.Request.Context(), "gin", usageContext)
	ctx, proof, err := h.authManager.BeginConfiguredEnterpriseModelProbe(ctx, auth, req.ModelID, target, req.ExpectedRevision, req.AuthorizationRevision, query)
	if err != nil {
		diagnostic := enterpriseModelTestError(enterpriseModelTestResult{}, err)
		result.Reason, result.HTTPStatus = diagnostic.Code, diagnostic.HTTPStatus
		h.finishConfiguredEnterpriseVerification(c, location, tag, journal)
		return
	}
	body, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": query}}}}})
	response, errExecute := h.authManager.Execute(ctx, []string{ge.Provider}, coreexec.Request{Model: req.ModelID, Payload: body, Format: sdktranslator.FormatGemini}, coreexec.Options{
		SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, OriginalRequest: body,
		Metadata: map[string]any{coreexec.PinnedAuthMetadataKey: auth.ID, coreexec.OperationMetadataKey: "generate_content", "source": "management_enterprise_model_verification"},
	})
	result.AssistSkippingModeRetried = coreauth.ConfiguredEnterpriseProbeRetried(ctx)
	if errExecute != nil {
		var skipped *ge.SkippedError
		if errors.As(errExecute, &skipped) {
			result.AssistSkippedReasons = skipped.SkipReasons()
		}
		diagnostic := enterpriseModelTestError(enterpriseModelTestResult{}, errExecute)
		result.Reason, result.HTTPStatus = diagnostic.Code, diagnostic.HTTPStatus
		var conflict *coreauth.EnterpriseModelProbeError
		if errors.As(errExecute, &conflict) {
			result.Reason = conflict.Code
		}
		if coreauth.ConfiguredEnterpriseProbeSent(ctx) && (ctx.Err() != nil || result.HTTPStatus == http.StatusRequestTimeout || result.HTTPStatus >= http.StatusInternalServerError) {
			result.State = "reconcile_required"
		}
	} else if text, complete := enterpriseVerificationText(response.Payload); !complete || text == "" {
		result.Reason, result.HTTPStatus = "verification_incomplete", http.StatusBadGateway
	} else {
		result.ActualModelID, result.Reason, result.HTTPStatus = h.verifyEnterpriseAdmissionSession(ctx, auth.ID, response.Metadata, query, req.ModelID)
		if result.Reason == "" && result.ActualModelID == req.ModelID && h.authManager.EnterpriseModelProbeMatches(ctx) {
			result.State, result.Reason = "runtime_pending", "runtime_sync_pending"
			journal.Proof = &proof
		} else if result.Reason == "" {
			result.State, result.Reason = "reconcile_required", "credential_changed"
		} else if result.ActualModelID == "" {
			result.State = "reconcile_required"
		}
	}
	h.finishConfiguredEnterpriseVerification(c, location, tag, journal)
}

func (h *Handler) finishConfiguredEnterpriseVerification(c *gin.Context, location enterpriseVerificationJournalLocation, tag string, journal enterpriseVerificationJournal) {
	ctx := context.WithoutCancel(c.Request.Context())
	if err := location.write(ctx, journal, tag); err != nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "verification_receipt_unavailable", "Could not confirm the model verification receipt")
		return
	}
	if journal.Result.State == "runtime_pending" {
		current, currentTag, err := location.read(ctx)
		if err == nil && reflect.DeepEqual(current.Result, journal.Result) {
			h.publishConfiguredEnterpriseVerification(ctx, location, currentTag, &journal)
		} else if err == nil && current.Result.State == "published" {
			journal = current
		}
	}
	c.JSON(http.StatusOK, journal.Result)
}

func (h *Handler) publishConfiguredEnterpriseVerification(ctx context.Context, location enterpriseVerificationJournalLocation, tag string, journal *enterpriseVerificationJournal) {
	if journal.Proof == nil || journal.Result.ActualModelID != journal.Request.ModelID || journal.Result.HTTPStatus != http.StatusOK {
		return
	}
	auth, exists := h.configuredVerificationAuth(journal.Request)
	if !exists {
		return
	}
	ctx = context.WithoutCancel(ctx)
	updated, err := h.authManager.SaveConfiguredEnterpriseModelProof(ctx, auth, *journal.Proof)
	if err != nil {
		return
	}
	if !h.authManager.ConfiguredEnterpriseModelPublished(auth, *journal.Proof) &&
		(h.authManager.SyncConfiguredEnterpriseModelProof(ctx, updated) != nil || !h.authManager.ConfiguredEnterpriseModelPublished(auth, *journal.Proof)) {
		return
	}
	next := *journal
	next.Result.State, next.Result.Reason = "published", ""
	if location.write(ctx, next, tag) == nil {
		*journal = next
	}
}
