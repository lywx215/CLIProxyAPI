package management

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/filelock"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// SyncGeminiEnterpriseOAuthImportRuntime publishes only an existing saved authorization.
func (h *Handler) SyncGeminiEnterpriseOAuthImportRuntime(c *gin.Context) {
	id, directory, ok := h.enterpriseImportRequest(c)
	if !ok {
		return
	}
	if c.Request.Body != nil {
		if body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1)); err != nil || len(body) != 0 {
			enterpriseManagementFailure(c, http.StatusBadRequest, "invalid_runtime_sync", "Runtime publication accepts an empty request body")
			return
		}
	}
	if store := h.sharedEnterpriseImportStore(); store != nil {
		h.syncSharedEnterpriseImport(c, store, id)
		return
	}
	path, journal, ok := readFileEnterpriseImportRequest(c, id, directory)
	if !ok {
		return
	}
	_, releasePrincipal, err := filelock.Try(filepath.Join(filepath.Dir(path), journal.AccountID+".lock"))
	if err != nil {
		enterpriseManagementFailure(c, http.StatusConflict, "account_import_busy", "Another OAuth import or publication owns this account")
		return
	}
	defer releasePrincipal()
	latest, err := readEnterpriseImportJournal(path)
	if err != nil || latest.RequestID != journal.RequestID || latest.Digest != journal.Digest || latest.AccountID != journal.AccountID {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "receipt_unavailable", "Cannot confirm the current OAuth import journal")
		return
	}
	journal = latest
	ctx := c.Request.Context()
	receipt, saved, err := h.persistedEnterpriseImport(ctx, journal.RequestID)
	if err != nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "receipt_unavailable", "Cannot establish the durable OAuth import result")
		return
	}
	if saved == nil {
		enterpriseManagementFailure(c, http.StatusConflict, "import_not_saved", "Runtime publication requires an exact saved grant")
		return
	}
	if receipt.Digest != journal.Digest || receipt.AccountID != journal.AccountID || !coreauth.MatchesEnterpriseOAuthReceipt(saved, receipt) {
		enterpriseManagementFailure(c, http.StatusConflict, "authorization_target_changed", "The original saved OAuth authorization is no longer current")
		return
	}
	source := saved.Attributes[coreauth.AttributePath]
	if source == "" {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "receipt_unavailable", "Cannot confirm the OAuth source path")
		return
	}
	_, releaseSource, err := filelock.Try(source + ".write-lock")
	if err != nil {
		enterpriseManagementFailure(c, http.StatusConflict, "account_import_busy", "Another process is saving this OAuth authorization")
		return
	}
	defer releaseSource()
	// Re-read the source after taking the same file lock used by credential saves.
	latestReceipt, latestSaved, err := h.persistedEnterpriseImport(ctx, journal.RequestID)
	if err != nil || latestSaved == nil || latestReceipt != receipt || latestSaved.ID != saved.ID ||
		latestSaved.Attributes[coreauth.AttributePath] != source || !coreauth.MatchesEnterpriseOAuthReceipt(latestSaved, receipt) {
		enterpriseManagementFailure(c, http.StatusConflict, "authorization_target_changed", "The original saved OAuth authorization is no longer current")
		return
	}
	saved = latestSaved
	current, _ := h.authManager.GetByID(saved.ID)
	if journal.RuntimeSynced && coreauth.MatchesEnterpriseOAuthReceipt(current, receipt) {
		writeEnterpriseSavedImport(c, receipt, true)
		return
	}
	if err := h.authManager.PublishEnterpriseOAuthImport(ctx, saved, receipt); err != nil {
		if errors.Is(err, coreauth.ErrEnterpriseOAuthImportConflict) {
			enterpriseManagementFailure(c, http.StatusConflict, "authorization_target_changed", "The original saved OAuth authorization is no longer current")
		} else {
			enterpriseManagementFailure(c, http.StatusServiceUnavailable, "runtime_sync_pending", "The exact OAuth authorization could not be published from its saved state")
		}
		return
	}
	journal.Status, journal.Code, journal.RuntimeSynced = "saved", "", true
	if err := writeEnterpriseImportJournal(path, journal, false); err != nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "runtime_sync_unconfirmed", "Publication completed but its journal could not be confirmed")
		return
	}
	current, _ = h.authManager.GetByID(saved.ID)
	writeEnterpriseSavedImport(c, receipt, coreauth.MatchesEnterpriseOAuthReceipt(current, receipt))
}
