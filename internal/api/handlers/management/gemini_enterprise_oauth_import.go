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

func (h *Handler) enterpriseImportConfiguration() (config.GeminiEnterpriseOAuth, string, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil || h.cfg.OAuth.Providers.GeminiEnterprise == nil {
		return config.GeminiEnterpriseOAuth{}, "", "", false
	}
	app := h.cfg.OAuth.Providers.GeminiEnterprise.WithDefaults()
	return app, h.cfg.AuthDir, h.cfg.ProxyURL, true
}

func (h *Handler) GeminiEnterpriseOAuthCapabilities(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	app, directory, _, configured := h.enterpriseImportConfiguration()
	ready := false
	scopes := []string{}
	if configured {
		if oauth, err := ge.ApplicationOAuth(app, ""); err == nil {
			ready, scopes = true, oauth.Scopes
		}
	}
	c.JSON(http.StatusOK, gin.H{"protocol_version": 1, "provider": ge.Provider,
		"client_id": app.ClientID, "client_secret_ready": ready, "required_scopes": scopes,
		"import_supported":  configured && directory != "" && h.authManager.SupportsEnterpriseOAuthImport(),
		"initially_enabled": true})
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
	// Consult the durable store: a lost save acknowledgment may precede runtime publication.
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return coreauth.EnterpriseOAuthImportReceipt{}, false, errors.New("credential store unavailable")
	}
	rows, err := store.List(ctx)
	if err != nil {
		return coreauth.EnterpriseOAuthImportReceipt{}, false, err
	}
	var found coreauth.EnterpriseOAuthImportReceipt
	count := 0
	for _, row := range rows {
		if row == nil || row.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(row) {
			continue
		}
		if receipt, ok := coreauth.EnterpriseOAuthReceipt(row, id); ok {
			found, count = receipt, count+1
		}
	}
	if count > 1 {
		return found, false, errors.New("duplicate persistent import receipt")
	}
	return found, count == 1, nil
}

func (h *Handler) renderEnterpriseImportReceipt(c *gin.Context, journal enterpriseImportJournal) {
	receipt, exists, err := h.persistedEnterpriseImportReceipt(c.Request.Context(), journal.RequestID)
	if err != nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "receipt_unavailable", "Cannot establish the durable OAuth import result")
		return
	}
	if exists {
		if receipt.Digest != journal.Digest {
			enterpriseManagementFailure(c, http.StatusConflict, "request_id_conflict", "Request ID belongs to a different authorization import")
			return
		}
		c.JSON(http.StatusOK, gin.H{"request_id": receipt.RequestID, "status": "saved", "account_id": receipt.AccountID,
			"name": receipt.Name, "auth_index": receipt.AuthIndex, "authorization_revision": receipt.AuthorizationRevision,
			"email": receipt.Email, "subject": receipt.Subject, "client_id": receipt.ClientID, "saved_at": receipt.SavedAt,
			"runtime_synced": journal.RuntimeSynced})
		return
	}
	c.JSON(http.StatusOK, gin.H{"request_id": journal.RequestID, "status": journal.Status, "account_id": journal.AccountID,
		"runtime_synced": false, "error": journal.Code})
}

func (h *Handler) GetGeminiEnterpriseOAuthImport(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	id := c.Param("request_id")
	if !enterpriseImportRequestID.MatchString(id) {
		enterpriseManagementFailure(c, http.StatusBadRequest, "invalid_request_id", "A valid import request ID is required")
		return
	}
	_, directory, _, configured := h.enterpriseImportConfiguration()
	if !configured || directory == "" {
		enterpriseManagementFailure(c, 409, "oauth_application_not_ready", "Configure the Enterprise OAuth application first")
		return
	}
	journal, err := readEnterpriseImportJournal(enterpriseJournalPath(directory, id))
	if err != nil {
		if os.IsNotExist(err) {
			enterpriseManagementFailure(c, 404, "import_not_found", "No recorded import request; absence does not authorize a replay")
		} else {
			enterpriseManagementFailure(c, 503, "receipt_unavailable", "Cannot read the OAuth import request")
		}
		return
	}
	h.renderEnterpriseImportReceipt(c, journal)
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
	app, directory, proxy, configured := h.enterpriseImportConfiguration()
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
	var target *coreauth.Auth
	matches := 0
	for _, auth := range h.authManager.List() {
		if auth == nil || auth.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(auth) || coreauth.IsPluginVirtualAuth(auth) || !strings.EqualFold(ge.String(auth.Metadata, "email"), input.Email) {
			continue
		}
		matches++
		target = auth
	}
	if (input.Mode == "create" && (matches != 0 || input.ExpectedRevision != 0 || input.Target.Name != "" || input.Target.AccountID != "" || input.Target.AuthIndex != "")) ||
		(input.Mode == "reauthorize" && (matches != 1 || target == nil || !coreauth.IsEnterpriseAccountAuth(target) ||
			input.Target.Name != target.FileName || input.Target.AuthIndex != target.Index || input.Target.AccountID != accountID ||
			ge.String(target.Metadata, "client_id") != input.ClientID || coreauth.EnterpriseOAuthAuthorizationRevision(target) != input.ExpectedRevision || input.ExpectedRevision == 0)) {
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
	token, err := verifyEnterpriseImportedGrant(c.Request.Context(), app, input.RefreshToken, input.Email, input.Subject, proxy)
	input.RefreshToken = ""
	if err != nil {
		var failure *enterpriseImportFailure
		if errors.As(err, &failure) {
			journal.Code = failure.Code
			if !failure.Uncertain {
				journal.Status = "failed"
			}
		}
		_ = writeEnterpriseImportJournal(journalPath, journal, false)
		h.renderEnterpriseImportReceipt(c, journal)
		return
	}
	// The account builder must not seed a configured legacy resource during import.
	app.ProjectID, app.EngineID = "", ""
	record, err := sdkAuth.NewGeminiEnterpriseAccountRecord(app, input.Email, token, h.authManager.List())
	if err != nil {
		journal.Code = "account_record_invalid"
		_ = writeEnterpriseImportJournal(journalPath, journal, false)
		h.renderEnterpriseImportReceipt(c, journal)
		return
	}
	if target != nil {
		record.ID, record.FileName, record.Index = target.ID, target.FileName, target.Index
	}
	options := coreauth.EnterpriseOAuthImportOptions{RequestID: input.RequestID, Digest: digest, Mode: input.Mode,
		ExpectedRevision: input.ExpectedRevision, Subject: input.Subject}
	saved, err := h.authManager.ImportEnterpriseAuthorization(c.Request.Context(), record, options)
	if err != nil {
		journal.Code = "authorization_save_unknown"
		if errors.Is(err, coreauth.ErrEnterpriseOAuthImportConflict) {
			journal.Status, journal.Code = "failed", "authorization_target_changed"
		}
		_ = writeEnterpriseImportJournal(journalPath, journal, false)
		h.renderEnterpriseImportReceipt(c, journal)
		return
	}
	journal.Status, journal.Code = "saved", ""
	journal.RuntimeSynced = h.invokePostAuthPersistHooks(c.Request.Context(), []*coreauth.Auth{saved}) == nil
	if !journal.RuntimeSynced {
		journal.Code = "runtime_sync_pending"
	}
	_ = writeEnterpriseImportJournal(journalPath, journal, false)
	h.renderEnterpriseImportReceipt(c, journal)
}
