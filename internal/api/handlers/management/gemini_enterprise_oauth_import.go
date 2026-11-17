package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/filelock"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var enterpriseImportRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,100}$`)

type enterpriseOAuthImportRequest struct {
	RequestID        string `json:"request_id"`
	Mode             string `json:"mode"`
	Email            string `json:"email"`
	Subject          string `json:"subject"`
	ClientID         string `json:"client_id"`
	RefreshToken     string `json:"refresh_token"`
	ExpectedRevision uint64 `json:"expected_authorization_revision"`
	Target           struct {
		Name      string `json:"name"`
		AccountID string `json:"account_id"`
		AuthIndex string `json:"auth_index"`
	} `json:"target"`
}

// The request ledger contains no grant material. Saved receipts are authoritative
// only when embedded in the credential's atomic persistence boundary.
type enterpriseImportJournal struct {
	RequestID     string `json:"request_id"`
	Digest        string `json:"digest"`
	AccountID     string `json:"account_id"`
	Status        string `json:"status"`
	Code          string `json:"code,omitempty"`
	RuntimeSynced bool   `json:"runtime_synced"`
	CreatedAt     string `json:"created_at"`
}

func (h *Handler) enterpriseOAuthSettings() (config.OAuthConfig, string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return config.OAuthConfig{}, "", ""
	}
	return h.cfg.OAuth, h.cfg.AuthDir, h.cfg.ProxyURL
}

type enterpriseOAuthApplicationCapability struct {
	Name              string   `json:"name"`
	ClientID          string   `json:"client_id"`
	ClientSecretReady bool     `json:"client_secret_ready"`
	RequiredScopes    []string `json:"required_scopes"`
	ImportSupported   bool     `json:"import_supported"`
}

func enterpriseApplicationCapability(name string, app config.GeminiEnterpriseOAuth, importSupported bool) enterpriseOAuthApplicationCapability {
	capability := enterpriseOAuthApplicationCapability{
		Name: name, ClientID: app.ClientID, RequiredScopes: []string{}, ImportSupported: importSupported,
	}
	if oauth, err := ge.ApplicationOAuth(app, ""); err == nil {
		capability.ClientSecretReady, capability.RequiredScopes = true, oauth.Scopes
	}
	return capability
}

func (h *Handler) GeminiEnterpriseOAuthCapabilities(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	settings, directory, _ := h.enterpriseOAuthSettings()
	app, configured := settings.GeminiEnterpriseApplication("")
	storeSupported := directory != "" && h.authManager.SupportsEnterpriseOAuthImport()
	legacy := enterpriseApplicationCapability("", app, configured && storeSupported)
	applications := make([]enterpriseOAuthApplicationCapability, 0)
	if configured {
		applications = append(applications, legacy)
	}
	newAccountDefault := legacy.ClientID
	if added := settings.GeminiEnterpriseApplications; added != nil {
		if preferred := strings.TrimSpace(added.NewAccountDefaultClientID); preferred != "" {
			newAccountDefault = preferred
		}
		for _, entry := range added.Entries {
			applications = append(applications, enterpriseApplicationCapability(strings.TrimSpace(entry.Name), entry.OAuth(), storeSupported))
		}
	}
	c.JSON(http.StatusOK, gin.H{"protocol_version": 1, "provider": ge.Provider,
		"client_id": legacy.ClientID, "client_secret_ready": legacy.ClientSecretReady, "required_scopes": legacy.RequiredScopes,
		"import_supported": legacy.ImportSupported, "initially_enabled": true,
		"reauthorization_supported": directory != "" && h.authManager.SupportsExistingEnterpriseReauthorization(),
		"applications":              applications, "new_account_default_client_id": newAccountDefault})
}

func enterpriseJournalPath(directory, requestID string) string {
	// Credential scanners consume .json recursively. Journals are not credentials.
	return filepath.Join(directory, ".enterprise-oauth-imports", requestID+".receipt")
}

func writeEnterpriseImportJournal(path string, value enterpriseImportJournal, exclusive bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if exclusive {
		file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errOpen != nil {
			return errOpen
		}
		_, errWrite := file.Write(data)
		if errWrite == nil {
			errWrite = file.Sync()
		}
		errClose := file.Close()
		if errWrite != nil {
			return errWrite
		}
		return errClose
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	errClose := file.Close()
	if err != nil {
		return err
	}
	if errClose != nil {
		return errClose
	}
	return os.Rename(temporary, path)
}

func readEnterpriseImportJournal(path string) (enterpriseImportJournal, error) {
	var result enterpriseImportJournal
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer func() { _ = file.Close() }()
	err = json.NewDecoder(io.LimitReader(file, 65536)).Decode(&result)
	return result, err
}

func (h *Handler) persistedEnterpriseImportReceipt(ctx context.Context, id string) (coreauth.EnterpriseOAuthImportReceipt, bool, error) {
	receipt, auth, err := h.persistedEnterpriseImport(ctx, id)
	return receipt, auth != nil, err
}

func (h *Handler) persistedEnterpriseImport(ctx context.Context, id string) (coreauth.EnterpriseOAuthImportReceipt, *coreauth.Auth, error) {
	// Consult the durable store: a lost save acknowledgment may precede runtime publication.
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return coreauth.EnterpriseOAuthImportReceipt{}, nil, errors.New("credential store unavailable")
	}
	rows, err := store.List(ctx)
	if err != nil {
		return coreauth.EnterpriseOAuthImportReceipt{}, nil, err
	}
	var found coreauth.EnterpriseOAuthImportReceipt
	var auth *coreauth.Auth
	for _, row := range rows {
		if row == nil || row.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(row) {
			continue
		}
		if receipt, ok := coreauth.EnterpriseOAuthReceipt(row, id); ok {
			if auth != nil {
				return found, nil, errors.New("duplicate persistent import receipt")
			}
			found, auth = receipt, row
		}
	}
	return found, auth, nil
}

func (h *Handler) renderEnterpriseImportReceipt(c *gin.Context, journal enterpriseImportJournal) {
	receipt, saved, err := h.persistedEnterpriseImport(c.Request.Context(), journal.RequestID)
	if err != nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "receipt_unavailable", "Cannot establish the durable OAuth import result")
		return
	}
	if saved != nil {
		if receipt.Digest != journal.Digest || receipt.AccountID != journal.AccountID {
			enterpriseManagementFailure(c, http.StatusConflict, "request_id_conflict", "Request ID belongs to a different authorization import")
			return
		}
		current, _ := h.authManager.GetByID(saved.ID)
		writeEnterpriseSavedImport(c, receipt, journal.RuntimeSynced &&
			coreauth.MatchesEnterpriseOAuthReceipt(saved, receipt) && coreauth.MatchesEnterpriseOAuthReceipt(current, receipt))
		return
	}
	status, code := journal.Status, journal.Code
	if status == "saved" {
		status, code = "reconcile_required", "durable_receipt_unavailable"
	}
	c.JSON(http.StatusOK, gin.H{"request_id": journal.RequestID, "status": status, "account_id": journal.AccountID,
		"runtime_synced": false, "error": code})
}

func writeEnterpriseSavedImport(c *gin.Context, receipt coreauth.EnterpriseOAuthImportReceipt, runtimeSynced bool) {
	c.JSON(http.StatusOK, gin.H{"request_id": receipt.RequestID, "status": "saved", "account_id": receipt.AccountID,
		"name": receipt.Name, "auth_index": receipt.AuthIndex, "authorization_revision": receipt.AuthorizationRevision,
		"email": receipt.Email, "subject": receipt.Subject, "client_id": receipt.ClientID, "saved_at": receipt.SavedAt,
		"runtime_synced": runtimeSynced})
}

func (h *Handler) enterpriseImportRequest(c *gin.Context) (string, string, bool) {
	c.Header("Cache-Control", "no-store")
	if !h.enterpriseManagementAllowed(c) {
		return "", "", false
	}
	id := c.Param("request_id")
	if !enterpriseImportRequestID.MatchString(id) {
		enterpriseManagementFailure(c, http.StatusBadRequest, "invalid_request_id", "A valid import request ID is required")
		return "", "", false
	}
	_, directory, _ := h.enterpriseOAuthSettings()
	if directory == "" {
		enterpriseManagementFailure(c, 409, "oauth_application_not_ready", "Configure the Enterprise OAuth credential directory first")
		return "", "", false
	}
	return id, directory, true
}

func readFileEnterpriseImportRequest(c *gin.Context, id, directory string) (string, enterpriseImportJournal, bool) {
	path := enterpriseJournalPath(directory, id)
	journal, err := readEnterpriseImportJournal(path)
	if err != nil {
		if os.IsNotExist(err) {
			enterpriseManagementFailure(c, 404, "import_not_found", "No recorded import request; absence does not authorize a replay")
		} else {
			enterpriseManagementFailure(c, 503, "receipt_unavailable", "Cannot read the OAuth import request")
		}
		return "", enterpriseImportJournal{}, false
	}
	if journal.RequestID != id || !enterpriseImportRequestID.MatchString(journal.AccountID) {
		enterpriseManagementFailure(c, 503, "receipt_unavailable", "Cannot confirm the OAuth import request identity")
		return "", enterpriseImportJournal{}, false
	}
	return path, journal, true
}

func (h *Handler) GetGeminiEnterpriseOAuthImport(c *gin.Context) {
	id, directory, ok := h.enterpriseImportRequest(c)
	if !ok {
		return
	}
	if store := h.sharedEnterpriseImportStore(); store != nil {
		h.renderSharedEnterpriseImport(c, store, id)
		return
	}
	_, journal, ok := readFileEnterpriseImportRequest(c, id, directory)
	if ok {
		h.renderEnterpriseImportReceipt(c, journal)
	}
}

func (h *Handler) ImportGeminiEnterpriseOAuth(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var input enterpriseOAuthImportRequest
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		enterpriseManagementFailure(c, 400, "invalid_import", "Invalid Enterprise OAuth import fields")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || !enterpriseImportRequestID.MatchString(input.RequestID) ||
		(input.Mode != "create" && input.Mode != "reauthorize") || input.RefreshToken == "" || len(input.RefreshToken) > 16384 ||
		input.Subject == "" || len(input.Subject) > 200 || !strings.Contains(input.Email, "@") ||
		strings.ContainsAny(input.Email+input.Subject+input.ClientID, "\r\n\x00") {
		enterpriseManagementFailure(c, 400, "invalid_import", "Required import identity and authorization fields are invalid")
		return
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	settings, directory, proxy := h.enterpriseOAuthSettings()
	app, configured := settings.GeminiEnterpriseApplication(input.ClientID)
	if !configured || directory == "" || input.ClientID != app.ClientID || !h.authManager.SupportsEnterpriseOAuthImport() {
		enterpriseManagementFailure(c, 409, "oauth_application_not_ready", "The configured application or credential store does not support this import")
		return
	}
	if _, err := ge.ApplicationOAuth(app, ""); err != nil {
		enterpriseManagementFailure(c, 409, "oauth_application_not_ready", "The server OAuth application refresh configuration is unavailable")
		return
	}
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	accountID := ge.AccountID(input.Email, input.ClientID)
	if store := h.sharedEnterpriseImportStore(); store != nil {
		h.importSharedEnterpriseOAuth(c, store, input, app, proxy, accountID, digest)
		return
	}
	journalPath := enterpriseJournalPath(directory, input.RequestID)
	if existing, err := readEnterpriseImportJournal(journalPath); err == nil {
		if existing.Digest != digest {
			enterpriseManagementFailure(c, 409, "request_id_conflict", "Request ID cannot be reused for a different import")
			return
		}
		h.renderEnterpriseImportReceipt(c, existing)
		return
	} else if !os.IsNotExist(err) {
		enterpriseManagementFailure(c, 503, "receipt_unavailable", "Cannot read existing import intent")
		return
	}
	target, err := h.enterpriseImportTarget(input, accountID)
	if err != nil {
		enterpriseManagementFailure(c, 409, "authorization_target_changed", "Select one exact Enterprise account and its current authorization revision")
		return
	}
	journal := enterpriseImportJournal{RequestID: input.RequestID, Digest: digest, AccountID: accountID,
		Status: "reconcile_required", Code: "import_in_progress", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeEnterpriseImportJournal(journalPath, journal, true); err != nil {
		enterpriseManagementFailure(c, 409, "import_already_recorded", "Import intent exists or could not be durably recorded; query its receipt")
		return
	}
	principalPath := filepath.Join(filepath.Dir(journalPath), accountID+".lock")
	principal, releasePrincipalLock, err := filelock.Try(principalPath)
	if err != nil {
		journal.Status, journal.Code = "failed", "account_import_busy"
		_ = writeEnterpriseImportJournal(journalPath, journal, false)
		enterpriseManagementFailure(c, 409, journal.Code, "Another import for this account is active or requires reconciliation")
		return
	}
	defer releasePrincipalLock()
	previousData, errPrevious := io.ReadAll(io.LimitReader(principal, 256))
	if errPrevious != nil {
		enterpriseManagementFailure(c, 503, "receipt_unavailable", "Cannot read the previous account import owner")
		return
	}
	if previousID := string(previousData); previousID != "" && previousID != input.RequestID {
		if !enterpriseImportRequestID.MatchString(previousID) {
			enterpriseManagementFailure(c, 409, "previous_import_unresolved", "The previous account import owner cannot be identified")
			return
		}
		previous, errJournal := readEnterpriseImportJournal(enterpriseJournalPath(directory, previousID))
		_, savedBefore, errReceipt := h.persistedEnterpriseImportReceipt(c.Request.Context(), previousID)
		if errJournal != nil || errReceipt != nil || (!savedBefore && previous.Status != "failed") {
			journal.Status, journal.Code = "failed", "previous_import_unresolved"
			_ = writeEnterpriseImportJournal(journalPath, journal, false)
			enterpriseManagementFailure(c, 409, journal.Code, "Previous account import requires read-only reconciliation")
			return
		}
	}
	errWrite := principal.Truncate(0)
	if errWrite == nil {
		_, errWrite = principal.WriteAt([]byte(input.RequestID), 0)
	}
	if errWrite == nil {
		errWrite = principal.Sync()
	}
	if errWrite != nil {
		enterpriseManagementFailure(c, 503, "import_intent_unknown", "Account import intent could not be durably confirmed")
		return
	}
	journal.Status, journal.Code, journal.RuntimeSynced = h.commitEnterpriseImport(c.Request.Context(), input, app, proxy, digest, target)
	_ = writeEnterpriseImportJournal(journalPath, journal, false)
	h.renderEnterpriseImportReceipt(c, journal)
}

func (h *Handler) enterpriseImportTarget(input enterpriseOAuthImportRequest, accountID string) (*coreauth.Auth, error) {
	var target *coreauth.Auth
	matches := 0
	for _, record := range h.authManager.List() {
		if record == nil || record.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(record) ||
			coreauth.IsPluginVirtualAuth(record) || !strings.EqualFold(ge.String(record.Metadata, "email"), input.Email) {
			continue
		}
		matches++
		target = record
	}
	if input.Mode == "create" {
		if matches == 0 && input.ExpectedRevision == 0 &&
			input.Target.Name == "" && input.Target.AccountID == "" && input.Target.AuthIndex == "" {
			return nil, nil
		}
	} else if matches == 1 && coreauth.IsEnterpriseAccountAuth(target) &&
		input.Target.Name == target.FileName && input.Target.AuthIndex == target.Index && input.Target.AccountID == accountID &&
		ge.String(target.Metadata, "client_id") == input.ClientID && input.ExpectedRevision > 0 &&
		coreauth.EnterpriseOAuthAuthorizationRevision(target) == input.ExpectedRevision {
		return target, nil
	}
	return nil, coreauth.ErrEnterpriseOAuthImportConflict
}

func (h *Handler) commitEnterpriseImport(ctx context.Context, input enterpriseOAuthImportRequest, app config.GeminiEnterpriseOAuth, proxy, digest string, target *coreauth.Auth) (status, code string, runtimeSynced bool) {
	token, err := verifyEnterpriseImportedGrant(ctx, app, input.RefreshToken, input.Email, input.Subject, proxy)
	if err != nil {
		var failure *enterpriseImportFailure
		if errors.As(err, &failure) {
			if !failure.Uncertain {
				return "failed", failure.Code, false
			}
			return "reconcile_required", failure.Code, false
		}
		return "reconcile_required", "oauth_verification_unknown", false
	}
	// Clear discovery hints before constructing the authorization record.
	app.ProjectID, app.EngineID = "", ""
	record, err := sdkAuth.NewGeminiEnterpriseAccountRecord(app, input.Email, token, h.authManager.List())
	if err != nil {
		return "reconcile_required", "account_record_invalid", false
	}
	if target != nil {
		record.ID, record.FileName, record.Index = target.ID, target.FileName, target.Index
	}
	options := coreauth.EnterpriseOAuthImportOptions{RequestID: input.RequestID, Digest: digest, Mode: input.Mode,
		ExpectedRevision: input.ExpectedRevision, Subject: input.Subject}
	saved, err := h.authManager.ImportEnterpriseAuthorization(ctx, record, options)
	if errors.Is(err, coreauth.ErrEnterpriseOAuthImportConflict) {
		return "failed", "authorization_target_changed", false
	}
	if err != nil {
		return "reconcile_required", "authorization_save_unknown", false
	}
	if h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{saved}) != nil {
		return "saved", "runtime_sync_pending", false
	}
	if saved.Attributes[coreauth.AttributeEnterpriseObjectETag] != "" {
		receipt, _ := coreauth.EnterpriseOAuthReceipt(saved, input.RequestID)
		if !h.authManager.ConfirmEnterpriseOAuthRuntime(saved, receipt) {
			return "saved", "runtime_sync_pending", false
		}
	}
	return "saved", "", true
}
