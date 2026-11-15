package management

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func (h *Handler) sharedEnterpriseImportStore() coreauth.EnterpriseOAuthImportJournalStore {
	return h.authManager.EnterpriseOAuthImportJournalStore()
}

func (h *Handler) importSharedEnterpriseOAuth(c *gin.Context, store coreauth.EnterpriseOAuthImportJournalStore,
	input enterpriseOAuthImportRequest, app config.GeminiEnterpriseOAuth, proxy, accountID, digest string) {
	ctx := c.Request.Context()
	existing, saved, err := store.LookupEnterpriseOAuthImport(ctx, input.RequestID)
	if err == nil {
		if existing.Digest != digest || existing.AccountID != accountID {
			enterpriseManagementFailure(c, http.StatusConflict, "request_id_conflict", "Request ID cannot be reused for a different import")
			return
		}
		h.writeSharedEnterpriseReceipt(c, existing, saved)
		return
	}
	if !errors.Is(err, coreauth.ErrEnterpriseOAuthImportNotFound) {
		writeSharedImportReadError(c, err)
		return
	}
	target, err := h.enterpriseImportTarget(input, accountID)
	if err != nil {
		enterpriseManagementFailure(c, http.StatusConflict, "authorization_target_changed", "Select one exact Enterprise account and its current authorization revision")
		return
	}
	intent := coreauth.EnterpriseOAuthImportJournal{
		RequestID: input.RequestID, Digest: digest, AccountID: accountID,
		CredentialID: coreauth.EnterpriseOAuthCredentialID(accountID), Mode: input.Mode,
		Email: input.Email, ClientID: input.ClientID, Subject: input.Subject,
		Status: "reconcile_required", Code: "import_in_progress",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if target != nil {
		intent.CredentialID, intent.TargetIndex, intent.ExpectedRevision = target.ID, target.Index, input.ExpectedRevision
	}
	intent, acquired, err := store.BeginEnterpriseOAuthImport(ctx, intent)
	switch {
	case errors.Is(err, coreauth.ErrEnterpriseOAuthImportIDConflict):
		enterpriseManagementFailure(c, http.StatusConflict, "request_id_conflict", "Request ID cannot be reused for a different import")
	case errors.Is(err, coreauth.ErrEnterpriseOAuthImportBusy):
		enterpriseManagementFailure(c, http.StatusConflict, "previous_import_unresolved", "Another import for this account is active or requires reconciliation")
	case errors.Is(err, coreauth.ErrEnterpriseOAuthImportConflict):
		enterpriseManagementFailure(c, http.StatusConflict, "authorization_target_changed", "The exact authorization target changed")
	case err != nil:
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "import_intent_unknown", "The OAuth import intent could not be confirmed")
	case !acquired:
		h.renderSharedEnterpriseImport(c, store, intent.RequestID)
	default:
		intent.Status, intent.Code, _ = h.commitEnterpriseImport(ctx, input, app, proxy, digest, target)
		// An intent update is advisory after a grant has committed. The reply
		// always comes from a fresh authoritative lookup of the embedded receipt.
		_ = store.FinishEnterpriseOAuthImport(ctx, intent)
		h.renderSharedEnterpriseImport(c, store, intent.RequestID)
	}
}

func writeSharedImportReadError(c *gin.Context, err error) {
	if errors.Is(err, coreauth.ErrEnterpriseOAuthImportNotFound) {
		enterpriseManagementFailure(c, http.StatusNotFound, "import_not_found", "No recorded import request; absence does not authorize a replay")
		return
	}
	enterpriseManagementFailure(c, http.StatusServiceUnavailable, "receipt_unavailable", "Cannot establish the durable OAuth import result")
}

func (h *Handler) renderSharedEnterpriseImport(c *gin.Context, store coreauth.EnterpriseOAuthImportJournalStore, id string) {
	intent, saved, err := store.LookupEnterpriseOAuthImport(c.Request.Context(), id)
	if err != nil {
		writeSharedImportReadError(c, err)
		return
	}
	h.writeSharedEnterpriseReceipt(c, intent, saved)
}

func (h *Handler) writeSharedEnterpriseReceipt(c *gin.Context, intent coreauth.EnterpriseOAuthImportJournal, saved *coreauth.Auth) {
	if saved != nil {
		receipt, synced := h.sharedEnterprisePublication(intent, saved)
		writeEnterpriseSavedImport(c, receipt, synced)
		return
	}
	status, code := intent.Status, intent.Code
	if status == "saved" {
		status, code = "reconcile_required", "durable_receipt_unavailable"
	}
	c.JSON(http.StatusOK, gin.H{"request_id": intent.RequestID, "status": status, "account_id": intent.AccountID,
		"runtime_synced": false, "error": code})
}

func (h *Handler) sharedEnterprisePublication(intent coreauth.EnterpriseOAuthImportJournal, saved *coreauth.Auth) (coreauth.EnterpriseOAuthImportReceipt, bool) {
	receipt, _ := coreauth.EnterpriseOAuthReceipt(saved, intent.RequestID)
	return receipt, h.authManager.EnterpriseOAuthRuntimeConfirmed(saved, receipt)
}

func (h *Handler) syncSharedEnterpriseImport(c *gin.Context, store coreauth.EnterpriseOAuthImportJournalStore, id string) {
	ctx := c.Request.Context()
	intent, saved, err := store.LookupEnterpriseOAuthImport(ctx, id)
	if err != nil {
		writeSharedImportReadError(c, err)
		return
	}
	if saved == nil {
		enterpriseManagementFailure(c, http.StatusConflict, "import_not_saved", "Runtime publication requires an exact saved grant")
		return
	}
	receipt, published := h.sharedEnterprisePublication(intent, saved)
	if !coreauth.MatchesEnterpriseOAuthReceipt(saved, receipt) {
		enterpriseManagementFailure(c, http.StatusConflict, "authorization_target_changed", "The original saved OAuth authorization is no longer current")
		return
	}
	if !published {
		if mirror, ok := store.(coreauth.EnterpriseOAuthImportMirror); ok {
			if err := mirror.RestoreEnterpriseOAuthImportMirror(ctx, saved, receipt); err != nil {
				enterpriseManagementFailure(c, http.StatusServiceUnavailable, "runtime_sync_pending", "Could not restore the exact saved authorization to the local mirror")
				return
			}
		}
		if err = h.authManager.PublishEnterpriseOAuthImport(ctx, saved, receipt); err != nil {
			if errors.Is(err, coreauth.ErrEnterpriseOAuthImportConflict) {
				enterpriseManagementFailure(c, http.StatusConflict, "authorization_target_changed", "The original saved OAuth authorization is no longer current")
			} else {
				enterpriseManagementFailure(c, http.StatusServiceUnavailable, "runtime_sync_pending", "The exact OAuth authorization could not be published from its saved state")
			}
			return
		}
	}
	latest, current, err := store.LookupEnterpriseOAuthImport(ctx, id)
	if err != nil {
		writeSharedImportReadError(c, err)
		return
	}
	if current == nil || latest.Digest != intent.Digest || latest.AccountID != intent.AccountID ||
		current.Attributes[coreauth.AttributeEnterpriseObjectETag] != saved.Attributes[coreauth.AttributeEnterpriseObjectETag] {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "runtime_sync_unconfirmed", "The credential changed during runtime publication")
		return
	}
	if !published && !h.authManager.ConfirmEnterpriseOAuthRuntime(current, receipt) {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "runtime_sync_unconfirmed", "The credential changed during runtime publication")
		return
	}
	h.writeSharedEnterpriseReceipt(c, latest, current)
}
