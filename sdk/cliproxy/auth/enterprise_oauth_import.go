package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

const EnterpriseOAuthRevisionKey = "enterprise_oauth_authorization_revision"
const EnterpriseOAuthSubjectKey = "enterprise_oauth_subject"
const EnterpriseOAuthReceiptsKey = "enterprise_oauth_import_receipts"

var ErrEnterpriseOAuthImportConflict = errors.New("enterprise OAuth import target or authorization revision changed")

type enterpriseOAuthImportContextKey struct{}
type enterpriseOAuthRuntimeContextKey struct{}

// EnterpriseOAuthImportOptions contains no OAuth secret material.
type EnterpriseOAuthImportOptions struct {
	RequestID        string
	Digest           string
	Mode             string
	ExpectedRevision uint64
	Subject          string
}

// EnterpriseOAuthImportReceipt is committed in the same record as the grant.
type EnterpriseOAuthImportReceipt struct {
	RequestID             string `json:"request_id"`
	Digest                string `json:"digest"`
	Status                string `json:"status"`
	AccountID             string `json:"account_id"`
	Name                  string `json:"name"`
	AuthIndex             string `json:"auth_index"`
	AuthorizationRevision uint64 `json:"authorization_revision"`
	Email                 string `json:"email"`
	Subject               string `json:"subject"`
	ClientID              string `json:"client_id"`
	SavedAt               string `json:"saved_at"`
}

func EnterpriseOAuthAuthorizationRevision(auth *Auth) uint64 {
	if auth == nil || !IsEnterpriseAccountAuth(auth) {
		return 0
	}
	return enterpriseOAuthAuthorizationRevision(auth)
}

func enterpriseOAuthAuthorizationRevision(auth *Auth) uint64 {
	v := fmt.Sprint(auth.Metadata[EnterpriseOAuthRevisionKey])
	if revision, err := strconv.ParseUint(v, 10, 64); err == nil && revision > 0 {
		return revision
	}
	return 1
}

func EnterpriseOAuthReceipt(auth *Auth, requestID string) (EnterpriseOAuthImportReceipt, bool) {
	if auth == nil {
		return EnterpriseOAuthImportReceipt{}, false
	}
	data, err := json.Marshal(auth.Metadata[EnterpriseOAuthReceiptsKey])
	if err != nil {
		return EnterpriseOAuthImportReceipt{}, false
	}
	var receipts map[string]EnterpriseOAuthImportReceipt
	if json.Unmarshal(data, &receipts) != nil {
		return EnterpriseOAuthImportReceipt{}, false
	}
	receipt, ok := receipts[requestID]
	return receipt, ok && receipt.RequestID == requestID && receipt.Status == "saved"
}

// MatchesEnterpriseOAuthReceipt identifies the exact active authorization in a saved or runtime parent.
func MatchesEnterpriseOAuthReceipt(auth *Auth, receipt EnterpriseOAuthImportReceipt) bool {
	if !IsEnterpriseAccountAuth(auth) || enterpriseRetired(auth) || IsPluginVirtualAuth(auth) ||
		auth.FileName != receipt.Name || auth.Index != receipt.AuthIndex ||
		EnterpriseOAuthAuthorizationRevision(auth) != receipt.AuthorizationRevision ||
		ge.String(auth.Metadata, "email") != receipt.Email || ge.String(auth.Metadata, "client_id") != receipt.ClientID ||
		ge.String(auth.Metadata, EnterpriseOAuthSubjectKey) != receipt.Subject {
		return false
	}
	account, err := ge.ReadAccount(auth.Metadata)
	stored, ok := EnterpriseOAuthReceipt(auth, receipt.RequestID)
	return err == nil && account != nil && account.ID == receipt.AccountID &&
		account.ID == ge.AccountID(receipt.Email, receipt.ClientID) && ok && stored == receipt
}

// PublishEnterpriseOAuthImport installs a confirmed durable revision in memory.
// If that exact revision is already live, reuse it to preserve current tokens and settings.
func (m *Manager) PublishEnterpriseOAuthImport(ctx context.Context, saved *Auth, receipt EnterpriseOAuthImportReceipt) error {
	if !MatchesEnterpriseOAuthReceipt(saved, receipt) {
		return ErrEnterpriseOAuthImportConflict
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(WithSkipPersist(ctx), enterpriseOAuthRuntimeContextKey{}, receipt)
	current, found := m.GetByID(saved.ID)
	version := saved.Attributes[AttributeEnterpriseObjectETag]
	if !MatchesEnterpriseOAuthReceipt(current, receipt) || (version != "" && current.Attributes[AttributeEnterpriseObjectETag] != version) {
		var err error
		if found {
			_, err = m.Update(ctx, saved.Clone())
		} else {
			_, err = m.Register(ctx, saved.Clone())
		}
		if err != nil {
			return err
		}
	}
	current, _ = m.GetByID(saved.ID)
	if !MatchesEnterpriseOAuthReceipt(current, receipt) {
		return ErrEnterpriseOAuthImportConflict
	}
	if err := m.syncEnterpriseAccount(ctx, current); err != nil {
		return err
	}
	current, _ = m.GetByID(saved.ID)
	if !MatchesEnterpriseOAuthReceipt(current, receipt) {
		return ErrEnterpriseOAuthImportConflict
	}
	return nil
}

// IsEnterpriseOAuthRuntimeSync reports whether this publication is a local-only recovery.
func IsEnterpriseOAuthRuntimeSync(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(enterpriseOAuthRuntimeContextKey{}).(EnterpriseOAuthImportReceipt)
	return ok
}

func checkEnterpriseOAuthRuntime(ctx context.Context, current, saved *Auth, create bool) error {
	if !IsEnterpriseOAuthRuntimeSync(ctx) {
		return nil
	}
	receipt := ctx.Value(enterpriseOAuthRuntimeContextKey{}).(EnterpriseOAuthImportReceipt)
	if !MatchesEnterpriseOAuthReceipt(saved, receipt) {
		return ErrEnterpriseOAuthImportConflict
	}
	if current == nil && create {
		return nil
	}
	if !create && current != nil && IsEnterpriseAccountAuth(current) && !enterpriseRetired(current) && !IsPluginVirtualAuth(current) &&
		sameEnterprisePrincipal(current, saved) && current.FileName == receipt.Name && current.Index == receipt.AuthIndex &&
		EnterpriseOAuthAuthorizationRevision(current) < receipt.AuthorizationRevision &&
		(ge.String(current.Metadata, EnterpriseOAuthSubjectKey) == "" || ge.String(current.Metadata, EnterpriseOAuthSubjectKey) == receipt.Subject) {
		return nil
	}
	if !create && saved.Attributes[AttributeEnterpriseObjectETag] != "" && MatchesEnterpriseOAuthReceipt(current, receipt) {
		return nil
	}
	return ErrEnterpriseOAuthImportConflict
}

// SupportsEnterpriseOAuthImport is deliberately opt-in for conditional stores.
func (m *Manager) SupportsEnterpriseOAuthImport() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	store, ok := m.store.(interface{ SupportsEnterpriseOAuthImport() bool })
	return ok && store.SupportsEnterpriseOAuthImport()
}

// ImportEnterpriseAuthorization installs a verified grant with a durable CAS receipt.
func (m *Manager) ImportEnterpriseAuthorization(ctx context.Context, record *Auth, options EnterpriseOAuthImportOptions) (*Auth, error) {
	if !m.SupportsEnterpriseOAuthImport() || record == nil || options.RequestID == "" || options.Subject == "" || options.Digest == "" {
		return nil, ErrEnterpriseOAuthImportConflict
	}
	if options.Mode != "create" && options.Mode != "reauthorize" {
		return nil, ErrEnterpriseOAuthImportConflict
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, enterpriseOAuthImportContextKey{}, options)
	ctx = context.WithValue(ctx, enterpriseReauthorizationSaveKey{}, true)
	if options.Mode == "create" {
		return m.Register(WithAuthCreationIntent(ctx), record.Clone())
	}
	updated, err := m.updateInternal(ctx, nil, record.Clone(), updateModeEnterpriseReauthorize)
	if updated == nil && err == nil {
		err = ErrEnterpriseOAuthImportConflict
	}
	return updated, err
}

// CheckEnterpriseOAuthImport is called under both runtime and durable-store locks.
func CheckEnterpriseOAuthImport(ctx context.Context, current *Auth) error {
	if ctx == nil {
		return nil
	}
	options, ok := ctx.Value(enterpriseOAuthImportContextKey{}).(EnterpriseOAuthImportOptions)
	if !ok {
		return nil
	}
	if options.Mode == "create" {
		if current != nil {
			return ErrEnterpriseOAuthImportConflict
		}
		return nil
	}
	if current == nil || !IsEnterpriseAccountAuth(current) || enterpriseRetired(current) ||
		options.ExpectedRevision == 0 || EnterpriseOAuthAuthorizationRevision(current) != options.ExpectedRevision {
		return ErrEnterpriseOAuthImportConflict
	}
	if subject := ge.String(current.Metadata, EnterpriseOAuthSubjectKey); subject != "" && subject != options.Subject {
		return ErrEnterpriseOAuthImportConflict
	}
	return nil
}

// ApplyEnterpriseOAuthImport embeds the non-secret receipt in the atomic grant save.
func ApplyEnterpriseOAuthImport(ctx context.Context, auth *Auth) {
	if ctx == nil || auth == nil {
		return
	}
	options, ok := ctx.Value(enterpriseOAuthImportContextKey{}).(EnterpriseOAuthImportOptions)
	if !ok {
		return
	}
	revision := options.ExpectedRevision + 1
	auth.EnsureIndex()
	auth.Metadata[EnterpriseOAuthRevisionKey] = revision
	auth.Metadata[EnterpriseOAuthSubjectKey] = options.Subject
	receipts := map[string]EnterpriseOAuthImportReceipt{}
	if data, err := json.Marshal(auth.Metadata[EnterpriseOAuthReceiptsKey]); err == nil {
		_ = json.Unmarshal(data, &receipts)
	}
	if receipts == nil {
		receipts = map[string]EnterpriseOAuthImportReceipt{}
	}
	receipts[options.RequestID] = EnterpriseOAuthImportReceipt{
		RequestID: options.RequestID, Digest: options.Digest, Status: "saved",
		AccountID: ge.AccountID(ge.String(auth.Metadata, "email"), ge.String(auth.Metadata, "client_id")),
		Name:      auth.FileName, AuthIndex: auth.Index, AuthorizationRevision: revision,
		Email: ge.String(auth.Metadata, "email"), Subject: options.Subject,
		ClientID: ge.String(auth.Metadata, "client_id"), SavedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	auth.Metadata[EnterpriseOAuthReceiptsKey] = receipts
}
