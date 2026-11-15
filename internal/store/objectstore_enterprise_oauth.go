package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	objectImportPrefix    = ".enterprise-oauth-imports/"
	maxObjectImportIntent = 64 * 1024
	maxObjectAuthRecord   = 16 * 1024 * 1024
)

var (
	objectImportID     = regexp.MustCompile(`^[A-Za-z0-9_-]{16,100}$`)
	objectImportDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	objectImportPart   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,240}$`)
)

var _ auth.EnterpriseOAuthImportJournalStore = (*ObjectTokenStore)(nil)

// SupportsEnterpriseOAuthImport reports the operator's explicit storage
// compatibility setting. An S3-compatible endpoint alone is insufficient.
func (s *ObjectTokenStore) SupportsEnterpriseOAuthImport() bool {
	return s != nil && s.cfg.EnterpriseOAuthImportCAS
}

func objectImportRequestKey(id string) string { return objectImportPrefix + "requests/" + id + ".json" }
func objectImportOwnerKey(id string) string   { return objectImportPrefix + "owners/" + id + ".json" }

func validObjectAuthID(id string) bool {
	if len(id) > 1024 || path.Clean(id) != id || !strings.HasSuffix(strings.ToLower(id), ".json") {
		return false
	}
	for _, part := range strings.Split(id, "/") {
		if part == "." || part == ".." || !objectImportPart.MatchString(part) {
			return false
		}
	}
	return true
}

func validObjectImportIntent(j auth.EnterpriseOAuthImportJournal) bool {
	if !objectImportID.MatchString(j.RequestID) || !objectImportDigest.MatchString(j.Digest) ||
		!objectImportDigest.MatchString(j.AccountID) || !validObjectAuthID(j.CredentialID) ||
		ge.AccountID(j.Email, j.ClientID) != j.AccountID || len(j.Email) > 320 || len(j.ClientID) > 512 ||
		j.Subject == "" || len(j.Subject) > 200 || strings.ContainsAny(j.Email+j.ClientID+j.Subject, "\r\n\x00") {
		return false
	}
	switch j.Status {
	case "reconcile_required", "failed", "saved":
	default:
		return false
	}
	switch j.Mode {
	case "create":
		return j.CredentialID == auth.EnterpriseOAuthCredentialID(j.AccountID) && j.TargetIndex == "" && j.ExpectedRevision == 0
	case "reauthorize":
		return j.TargetIndex != "" && j.ExpectedRevision > 0 && j.ExpectedRevision < ^uint64(0)
	default:
		return false
	}
}

func sameObjectImport(a, b auth.EnterpriseOAuthImportJournal) bool {
	return a.RequestID == b.RequestID && a.Digest == b.Digest && a.AccountID == b.AccountID &&
		a.CredentialID == b.CredentialID && a.Mode == b.Mode && a.TargetIndex == b.TargetIndex &&
		a.ExpectedRevision == b.ExpectedRevision && a.Email == b.Email && a.ClientID == b.ClientID &&
		a.Subject == b.Subject
}

func (s *ObjectTokenStore) loadImportIntent(ctx context.Context, id string) (auth.EnterpriseOAuthImportJournal, string, error) {
	var value auth.EnterpriseOAuthImportJournal
	if !objectImportID.MatchString(id) {
		return value, "", auth.ErrEnterpriseOAuthImportConflict
	}
	body, etag, err := s.getVersionedObject(ctx, objectImportRequestKey(id), maxObjectImportIntent)
	if errors.Is(err, errObjectMissing) {
		return value, "", auth.ErrEnterpriseOAuthImportNotFound
	}
	if err != nil {
		return value, "", err
	}
	if json.Unmarshal(body, &value) != nil || value.RequestID != id || !validObjectImportIntent(value) {
		return value, "", errors.New("invalid persisted OAuth import intent")
	}
	return value, etag, nil
}

func (s *ObjectTokenStore) putImportIntent(ctx context.Context, j auth.EnterpriseOAuthImportJournal, etag string) error {
	body, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.putVersionedObject(ctx, objectImportRequestKey(j.RequestID), body, etag)
	return err
}

type objectImportOwner struct {
	RequestID string `json:"request_id"`
}

func (s *ObjectTokenStore) loadImportOwner(ctx context.Context, account string) (string, string, error) {
	body, etag, err := s.getVersionedObject(ctx, objectImportOwnerKey(account), maxObjectImportIntent)
	if err != nil {
		return "", "", err
	}
	var owner objectImportOwner
	if json.Unmarshal(body, &owner) != nil || !objectImportID.MatchString(owner.RequestID) {
		return "", "", errors.New("invalid persisted OAuth import owner")
	}
	return owner.RequestID, etag, nil
}

func (s *ObjectTokenStore) BeginEnterpriseOAuthImport(ctx context.Context, j auth.EnterpriseOAuthImportJournal) (auth.EnterpriseOAuthImportJournal, bool, error) {
	if !s.SupportsEnterpriseOAuthImport() || !validObjectImportIntent(j) || j.Status != "reconcile_required" {
		return j, false, auth.ErrEnterpriseOAuthImportConflict
	}
	j.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	err := s.putImportIntent(ctx, j, "")
	if errors.Is(err, errObjectChanged) {
		existing, _, readErr := s.loadImportIntent(ctx, j.RequestID)
		// A 409 without a readable intent cannot establish either ownership or absence.
		if readErr != nil {
			return j, false, fmt.Errorf("cannot confirm current OAuth import intent: %w", readErr)
		}
		if !sameObjectImport(existing, j) {
			return existing, false, auth.ErrEnterpriseOAuthImportIDConflict
		}
		return existing, false, nil
	}
	if err != nil {
		return j, false, err
	}
	if err = s.validateObjectImportTarget(ctx, j); err != nil {
		if errors.Is(err, auth.ErrEnterpriseOAuthImportConflict) {
			j.Status, j.Code = "failed", "authorization_target_changed"
			if finishErr := s.FinishEnterpriseOAuthImport(ctx, j); finishErr != nil {
				return j, false, finishErr
			}
		}
		return j, false, err
	}
	previousID, ownerETag, err := s.loadImportOwner(ctx, j.AccountID)
	if err != nil && !errors.Is(err, errObjectMissing) {
		return j, false, err
	}
	if previousID != "" {
		previous, saved, readErr := s.LookupEnterpriseOAuthImport(ctx, previousID)
		if readErr != nil || previous.AccountID != j.AccountID || (saved == nil && previous.Status != "failed") {
			return s.rejectBusyImport(ctx, j)
		}
	}
	owner, _ := json.Marshal(objectImportOwner{RequestID: j.RequestID})
	if _, err = s.putVersionedObject(ctx, objectImportOwnerKey(j.AccountID), owner, ownerETag); errors.Is(err, errObjectChanged) {
		// Only classify a definite rejection when another request owns the account.
		actualID, _, readErr := s.loadImportOwner(ctx, j.AccountID)
		if readErr == nil && actualID != j.RequestID {
			return s.rejectBusyImport(ctx, j)
		}
		return j, false, errObjectOutcomeUnknown
	} else if err != nil {
		return j, false, err
	}
	return j, true, nil
}

func (s *ObjectTokenStore) validateObjectImportTarget(ctx context.Context, j auth.EnterpriseOAuthImportJournal) error {
	prefix := s.prefixedKey(objectStoreAuthPrefix + "/")
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(prefix),
	})
	matched := false
	seenTokens := make(map[string]bool)
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return safeObjectRequestError("list OAuth credential identities", err)
		}
		if aws.ToBool(page.IsTruncated) {
			token := aws.ToString(page.NextContinuationToken)
			if token == "" || seenTokens[token] {
				return errors.New("object storage returned an incomplete credential list")
			}
			seenTokens[token] = true
		}
		for _, object := range page.Contents {
			fullKey := aws.ToString(object.Key)
			if !strings.HasPrefix(fullKey, prefix) {
				return errors.New("object storage returned an out-of-scope credential")
			}
			key := strings.TrimPrefix(fullKey, prefix)
			if !strings.HasSuffix(strings.ToLower(key), ".json") {
				continue
			}
			body, _, err := s.getVersionedObject(ctx, objectStoreAuthPrefix+"/"+key, maxObjectAuthRecord)
			if err != nil {
				return err
			}
			var identity struct {
				Provider string `json:"type"`
				Email    string `json:"email"`
			}
			if json.Unmarshal(body, &identity) != nil {
				return errors.New("cannot establish saved OAuth credential identity")
			}
			if !strings.EqualFold(strings.TrimSpace(identity.Provider), ge.Provider) ||
				!strings.EqualFold(strings.TrimSpace(identity.Email), j.Email) {
				continue
			}
			if matched || j.Mode != "reauthorize" || key != j.CredentialID {
				return auth.ErrEnterpriseOAuthImportConflict
			}
			// Reauthorize only the immutable identity and revision named by the client.
			target, _, err := s.getEnterpriseCredential(ctx, key)
			if err != nil {
				return err
			}
			if ge.String(target.Metadata, "client_id") != j.ClientID ||
				target.EnsureIndex() != j.TargetIndex || auth.IsEnterpriseRetiredAuth(target) ||
				auth.EnterpriseOAuthAuthorizationRevision(target) != j.ExpectedRevision {
				return auth.ErrEnterpriseOAuthImportConflict
			}
			matched = true
		}
	}
	if j.Mode == "reauthorize" && !matched {
		return auth.ErrEnterpriseOAuthImportConflict
	}
	return nil
}

func (s *ObjectTokenStore) rejectBusyImport(ctx context.Context, j auth.EnterpriseOAuthImportJournal) (auth.EnterpriseOAuthImportJournal, bool, error) {
	j.Status, j.Code = "failed", "previous_import_unresolved"
	if err := s.FinishEnterpriseOAuthImport(ctx, j); err != nil {
		return j, false, err
	}
	return j, false, auth.ErrEnterpriseOAuthImportBusy
}

func (s *ObjectTokenStore) LookupEnterpriseOAuthImport(ctx context.Context, requestID string) (auth.EnterpriseOAuthImportJournal, *auth.Auth, error) {
	j, _, err := s.loadImportIntent(ctx, requestID)
	if err != nil {
		return j, nil, err
	}
	current, _, err := s.getEnterpriseCredential(ctx, j.CredentialID)
	if errors.Is(err, errObjectMissing) {
		return j, nil, nil
	}
	if err != nil {
		return j, nil, err
	}
	receipt, ok := auth.EnterpriseOAuthReceipt(current, j.RequestID)
	if !ok {
		return j, nil, nil
	}
	if receipt.Digest != j.Digest || receipt.AccountID != j.AccountID || receipt.Name != current.FileName ||
		receipt.AuthIndex != current.EnsureIndex() || receipt.AuthorizationRevision != j.ExpectedRevision+1 ||
		receipt.Subject != j.Subject || receipt.Email != j.Email || receipt.ClientID != j.ClientID ||
		(j.TargetIndex != "" && receipt.AuthIndex != j.TargetIndex) ||
		receipt.AccountID != ge.AccountID(receipt.Email, receipt.ClientID) ||
		ge.AccountID(ge.String(current.Metadata, "email"), ge.String(current.Metadata, "client_id")) != j.AccountID {
		return j, nil, errors.New("saved OAuth import receipt does not match the request")
	}
	return j, current, nil
}

func (s *ObjectTokenStore) FinishEnterpriseOAuthImport(ctx context.Context, j auth.EnterpriseOAuthImportJournal) error {
	existing, etag, err := s.loadImportIntent(ctx, j.RequestID)
	if err != nil {
		return err
	}
	if !validObjectImportIntent(j) || !sameObjectImport(existing, j) {
		return auth.ErrEnterpriseOAuthImportIDConflict
	}
	if existing.Status != "reconcile_required" {
		if existing.Status == j.Status && existing.Code == j.Code {
			return nil
		}
		return auth.ErrEnterpriseOAuthImportConflict
	}
	if j.Status == "saved" {
		_, saved, lookupErr := s.LookupEnterpriseOAuthImport(ctx, j.RequestID)
		if lookupErr != nil {
			return lookupErr
		}
		if saved == nil {
			return auth.ErrEnterpriseOAuthImportConflict
		}
	}
	existing.Status, existing.Code = j.Status, j.Code
	return s.putImportIntent(ctx, existing, etag)
}

func (s *ObjectTokenStore) importSaveIntent(ctx context.Context, record *auth.Auth, id string, options auth.EnterpriseOAuthImportOptions) (auth.EnterpriseOAuthImportJournal, error) {
	j, _, err := s.loadImportIntent(ctx, options.RequestID)
	if err != nil {
		return j, err
	}
	account := ge.AccountID(ge.String(record.Metadata, "email"), ge.String(record.Metadata, "client_id"))
	if j.CredentialID != id || j.Digest != options.Digest || j.AccountID != account || j.Mode != options.Mode ||
		j.ExpectedRevision != options.ExpectedRevision || j.Subject != options.Subject || j.Status != "reconcile_required" {
		return j, auth.ErrEnterpriseOAuthImportConflict
	}
	owner, _, err := s.loadImportOwner(ctx, account)
	if err != nil {
		return j, err
	}
	if owner != j.RequestID {
		return j, auth.ErrEnterpriseOAuthImportConflict
	}
	return j, nil
}
