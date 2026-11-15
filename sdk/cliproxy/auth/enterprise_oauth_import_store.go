package auth

import (
	"context"
	"errors"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

// AttributeEnterpriseObjectETag is runtime-only provenance for an auth snapshot
// obtained from authoritative object storage.
const AttributeEnterpriseObjectETag = "enterprise_object_etag"

// EnterpriseOAuthCredentialPrefix reserves the canonical root credential IDs.
const EnterpriseOAuthCredentialPrefix = "gemini-enterprise-account-"

var (
	ErrEnterpriseOAuthImportNotFound   = errors.New("OAuth import intent not found")
	ErrEnterpriseOAuthImportIDConflict = errors.New("OAuth import request ID already binds a different request")
	ErrEnterpriseOAuthImportBusy       = errors.New("previous OAuth import is active or unresolved")
	ErrEnterpriseAuthVersionRequired   = errors.New("Enterprise credential requires a verified object version")
)

// EnterpriseOAuthCredentialID returns the canonical ID for new credentials.
func EnterpriseOAuthCredentialID(accountID string) string {
	if len(accountID) < 32 {
		return ""
	}
	return EnterpriseOAuthCredentialPrefix + accountID[:32] + ".json"
}

// EnterpriseOAuthImportJournal locates one immutable request and its credential.
// It contains no OAuth tokens or request body.
type EnterpriseOAuthImportJournal struct {
	RequestID        string `json:"request_id"`
	Digest           string `json:"digest"`
	AccountID        string `json:"account_id"`
	CredentialID     string `json:"credential_id"`
	Email            string `json:"email"`
	ClientID         string `json:"client_id"`
	Subject          string `json:"subject"`
	Mode             string `json:"mode"`
	TargetIndex      string `json:"target_index,omitempty"`
	ExpectedRevision uint64 `json:"expected_authorization_revision"`
	Status           string `json:"status"`
	Code             string `json:"code,omitempty"`
	CreatedAt        string `json:"created_at"`
}

// EnterpriseOAuthImportJournalStore is optional for stores with a shared
// remote commit point. A confirmed Begin owner is the only caller that may
// verify OAuth. Lookup never publishes or writes. Save commits the grant.
type EnterpriseOAuthImportJournalStore interface {
	BeginEnterpriseOAuthImport(context.Context, EnterpriseOAuthImportJournal) (EnterpriseOAuthImportJournal, bool, error)
	LookupEnterpriseOAuthImport(context.Context, string) (EnterpriseOAuthImportJournal, *Auth, error)
	FinishEnterpriseOAuthImport(context.Context, EnterpriseOAuthImportJournal) error
}

// EnterpriseOAuthImportMirror restores the local copy of an already saved
// authorization during explicit runtime recovery.
type EnterpriseOAuthImportMirror interface {
	RestoreEnterpriseOAuthImportMirror(context.Context, *Auth, EnterpriseOAuthImportReceipt) error
}

// EnterpriseOAuthImportJournalStore returns the optional interface on the same
// store that will commit the credential.
func (m *Manager) EnterpriseOAuthImportJournalStore() EnterpriseOAuthImportJournalStore {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	store, _ := m.store.(EnterpriseOAuthImportJournalStore)
	return store
}

// EnterpriseOAuthImportFromContext identifies a dedicated grant save. Stores
// still have to check its durable intent and current credential identity.
func EnterpriseOAuthImportFromContext(ctx context.Context) (EnterpriseOAuthImportOptions, bool) {
	if ctx == nil {
		return EnterpriseOAuthImportOptions{}, false
	}
	options, ok := ctx.Value(enterpriseOAuthImportContextKey{}).(EnterpriseOAuthImportOptions)
	return options, ok
}

// ConfirmEnterpriseOAuthRuntime records a successful publication for this process.
// The caller must already have checked the exact durable credential revision.
func (m *Manager) ConfirmEnterpriseOAuthRuntime(saved *Auth, receipt EnterpriseOAuthImportReceipt) bool {
	if m == nil || !MatchesEnterpriseOAuthReceipt(saved, receipt) {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[saved.ID]
	if !matchesEnterpriseObjectRuntime(current, saved, receipt) {
		return false
	}
	m.enterpriseOAuthPublications.Store(saved.ID, enterpriseOAuthPublication{receipt.RequestID, current.RegistrationEpoch})
	return true
}

// EnterpriseOAuthRuntimeConfirmed is scoped to the manager that published it.
func (m *Manager) EnterpriseOAuthRuntimeConfirmed(saved *Auth, receipt EnterpriseOAuthImportReceipt) bool {
	if m == nil || !MatchesEnterpriseOAuthReceipt(saved, receipt) {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[saved.ID]
	if !matchesEnterpriseObjectRuntime(current, saved, receipt) {
		return false
	}
	prior, ok := m.enterpriseOAuthPublications.Load(saved.ID)
	return ok && prior == (enterpriseOAuthPublication{receipt.RequestID, current.RegistrationEpoch})
}

type enterpriseOAuthPublication struct {
	requestID string
	epoch     uint64
}

func matchesEnterpriseObjectRuntime(current, saved *Auth, receipt EnterpriseOAuthImportReceipt) bool {
	etag := saved.Attributes[AttributeEnterpriseObjectETag]
	return etag != "" && MatchesEnterpriseOAuthReceipt(current, receipt) &&
		current.Attributes[AttributeEnterpriseObjectETag] == etag
}

// An unversioned file-watcher snapshot cannot replace a versioned object record.
func retainEnterpriseObjectSnapshot(ctx context.Context, current, incoming *Auth) bool {
	if !shouldSkipPersist(ctx) || current == nil || incoming == nil ||
		current.Provider != ge.Provider || incoming.Provider != current.Provider ||
		incoming.Attributes[AttributeEnterpriseObjectETag] != "" || current.FileName != incoming.FileName {
		return false
	}
	return current.Attributes[AttributeEnterpriseObjectETag] != ""
}
