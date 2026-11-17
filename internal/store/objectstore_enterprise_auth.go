package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type objectAuthSnapshot struct {
	digest [32]byte
	etag   string
}

func (s *ObjectTokenStore) SupportsExistingEnterpriseReauthorization() bool {
	return s.SupportsEnterpriseOAuthImport()
}

// GuardUnversionedEnterpriseAuth rejects file-based writers before they can
// alter a protected mirror. Save performs a conditional write for such records.
func (s *ObjectTokenStore) GuardUnversionedEnterpriseAuth(ctx context.Context, file string) error {
	if !s.SupportsEnterpriseOAuthImport() {
		return nil
	}
	id, err := s.relativeObjectAuthID(file)
	if err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(filepath.Base(id)), auth.EnterpriseOAuthCredentialPrefix) {
		return auth.ErrEnterpriseAuthVersionRequired
	}
	if !strings.HasSuffix(strings.ToLower(id), ".json") {
		return nil
	}
	body, err := os.ReadFile(file)
	if err == nil && enterpriseObjectBody(body) {
		return auth.ErrEnterpriseAuthVersionRequired
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Check the authoritative type as well. A stale or replaced local file
	// cannot make a protected remote credential eligible for generic writes.
	_, err = s.unprotectedAuthETag(ctx, id)
	return err
}

func (s *ObjectTokenStore) unprotectedAuthETag(ctx context.Context, id string) (string, error) {
	if strings.HasPrefix(strings.ToLower(filepath.Base(id)), auth.EnterpriseOAuthCredentialPrefix) {
		return "", auth.ErrEnterpriseAuthVersionRequired
	}
	body, etag, err := s.getVersionedObject(ctx, objectStoreAuthPrefix+"/"+id, maxObjectAuthRecord)
	if errors.Is(err, errObjectMissing) {
		return "", nil
	}
	if err == nil && enterpriseObjectBody(body) {
		return "", auth.ErrEnterpriseAuthVersionRequired
	}
	return etag, err
}

func enterpriseObjectBody(body []byte) bool {
	var record struct {
		Provider string `json:"type"`
	}
	return json.Unmarshal(body, &record) == nil && strings.EqualFold(strings.TrimSpace(record.Provider), ge.Provider)
}

func (s *ObjectTokenStore) relativeObjectAuthID(file string) (string, error) {
	rel, err := filepath.Rel(s.authDir, file)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("credential is outside the object store mirror")
	}
	return filepath.ToSlash(rel), nil
}

func (s *ObjectTokenStore) getEnterpriseCredential(ctx context.Context, id string) (*auth.Auth, string, error) {
	if !validObjectAuthID(id) {
		return nil, "", auth.ErrEnterpriseOAuthImportConflict
	}
	key := objectStoreAuthPrefix + "/" + id
	body, etag, err := s.getVersionedObject(ctx, key, maxObjectAuthRecord)
	if err != nil {
		return nil, "", err
	}
	record, err := s.decodeEnterpriseObject(id, body, etag)
	return record, etag, err
}

func (s *ObjectTokenStore) decodeEnterpriseObject(id string, body []byte, etag string) (*auth.Auth, error) {
	file := filepath.Join(s.authDir, filepath.FromSlash(id))
	record, err := decodeObjectAuth(body, file, id, time.Time{})
	if err != nil || record == nil || record.Provider != ge.Provider {
		return nil, auth.ErrEnterpriseOAuthImportConflict
	}
	projected, err := auth.ProjectEnterpriseAuth(record)
	if err != nil || len(projected) == 0 {
		return nil, auth.ErrEnterpriseOAuthImportConflict
	}
	record = projected[0]
	record.Attributes[auth.AttributeEnterpriseObjectETag] = etag
	return record, nil
}

func (s *ObjectTokenStore) RestoreEnterpriseOAuthImportMirror(ctx context.Context, saved *auth.Auth, receipt auth.EnterpriseOAuthImportReceipt) error {
	if !auth.MatchesEnterpriseOAuthReceipt(saved, receipt) || !validObjectAuthID(saved.ID) {
		return auth.ErrEnterpriseOAuthImportConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	body, etag, err := s.getVersionedObject(ctx, objectStoreAuthPrefix+"/"+saved.ID, maxObjectAuthRecord)
	if err != nil {
		return err
	}
	current, err := s.decodeEnterpriseObject(saved.ID, body, etag)
	if err != nil || etag != saved.Attributes[auth.AttributeEnterpriseObjectETag] || !auth.MatchesEnterpriseOAuthReceipt(current, receipt) {
		return auth.ErrEnterpriseOAuthImportConflict
	}
	file := current.Attributes[auth.AttributePath]
	if snapshot, errRead := os.ReadFile(file); errRead == nil && s.mirroredEnterpriseETag(file, snapshot) == etag {
		return nil
	}
	return s.updateEnterpriseMirror(file, body, etag)
}

func (s *ObjectTokenStore) saveVersionedEnterprise(ctx context.Context, record *auth.Auth, file string) (string, error) {
	id, err := s.relativeObjectAuthID(file)
	if err != nil || !validObjectAuthID(id) || record.ID != id || (record.FileName != "" && record.FileName != id) ||
		record.Metadata == nil || auth.IsEnterpriseCollectionAuth(record) {
		return "", auth.ErrEnterpriseOAuthImportConflict
	}
	incoming := record.Clone()
	incoming.Storage = nil
	attributes := incoming.Attributes
	etag := incoming.Attributes[auth.AttributeEnterpriseObjectETag]
	options, importing := auth.EnterpriseOAuthImportFromContext(ctx)
	if importing {
		incoming, etag, err = s.prepareObjectImport(ctx, incoming, id, options)
	} else if auth.IsEnterpriseReauthorizationSave(ctx) {
		incoming, err = s.prepareObjectReauthorization(ctx, incoming, id, etag)
	} else {
		err = s.checkOrdinaryEnterpriseSave(ctx, incoming, id, etag)
	}
	if err != nil {
		return "", err
	}
	incoming.Attributes = attributes
	incoming.Metadata["disabled"] = incoming.Disabled
	body, err := json.Marshal(incoming.Metadata)
	if err != nil {
		return "", fmt.Errorf("encode credential: %w", err)
	}
	if len(body) > maxObjectAuthRecord {
		return "", errors.New("credential exceeds the object size limit")
	}
	newETag, err := s.putVersionedObject(ctx, objectStoreAuthPrefix+"/"+id, body, etag)
	if errors.Is(err, errObjectChanged) {
		return "", auth.ErrEnterpriseOAuthImportConflict
	}
	if err != nil {
		return "", err
	}
	if err = s.updateEnterpriseMirror(file, body, newETag); err != nil {
		return "", fmt.Errorf("remote credential saved but mirror update failed: %w", err)
	}
	if incoming.Attributes == nil {
		incoming.Attributes = make(map[string]string)
	}
	incoming.Attributes[auth.AttributePath] = file
	incoming.Attributes[auth.AttributeSource] = file
	incoming.Attributes[auth.AttributeSourceBackend] = auth.AuthSourceObjectStore
	incoming.Attributes[auth.AttributeEnterpriseObjectETag] = newETag
	// The embedded receipt and the manager's publication must describe the
	// exact rebased credential, while manager-owned versions stay unchanged.
	incoming.RegistrationEpoch = record.RegistrationEpoch
	incoming.CredentialVersion = record.CredentialVersion
	incoming.Generation = record.Generation
	incoming.FileName = id
	*record = *incoming
	return file, nil
}

func (s *ObjectTokenStore) prepareObjectImport(ctx context.Context, record *auth.Auth, id string, options auth.EnterpriseOAuthImportOptions) (*auth.Auth, string, error) {
	j, err := s.importSaveIntent(ctx, record, id, options)
	if err != nil {
		return nil, "", err
	}
	if err = s.validateObjectImportTarget(ctx, j); err != nil {
		return nil, "", err
	}
	current, etag, err := s.getEnterpriseCredential(ctx, id)
	if err != nil && !errors.Is(err, errObjectMissing) {
		return nil, "", err
	}
	if err = auth.CheckEnterpriseOAuthImport(ctx, current); err != nil {
		return nil, "", err
	}
	if current != nil {
		graph, errGraph := ge.ReadAccount(current.Metadata)
		if errGraph != nil || graph == nil || graph.ID != j.AccountID || current.FileName != id ||
			current.EnsureIndex() != j.TargetIndex {
			return nil, "", auth.ErrEnterpriseOAuthImportConflict
		}
		record, err = auth.RebaseEnterpriseAuthorization(current, record)
		if err != nil {
			return nil, "", auth.ErrEnterpriseOAuthImportConflict
		}
	}
	auth.ApplyEnterpriseOAuthImport(ctx, record)
	return record, etag, nil
}

func (s *ObjectTokenStore) prepareObjectReauthorization(ctx context.Context, record *auth.Auth, id, etag string) (*auth.Auth, error) {
	if etag == "" {
		if auth.HasAuthCreationIntent(ctx) {
			return record, s.guardEnterpriseCreation(ctx, record, id)
		}
		return nil, auth.ErrEnterpriseAuthVersionRequired
	}
	current, remoteETag, err := s.getEnterpriseCredential(ctx, id)
	if errors.Is(err, errObjectMissing) || (err == nil && remoteETag != etag) {
		return nil, auth.ErrEnterpriseOAuthImportConflict
	}
	if err != nil {
		return nil, err
	}
	if err = auth.CheckExistingEnterpriseAuthorization(ctx, current); err != nil {
		return nil, err
	}
	return auth.RebaseEnterpriseAuthorization(current, record)
}

func (s *ObjectTokenStore) checkOrdinaryEnterpriseSave(ctx context.Context, record *auth.Auth, id, etag string) error {
	if etag == "" {
		if !auth.HasAuthCreationIntent(ctx) {
			return auth.ErrEnterpriseAuthVersionRequired
		}
		return s.guardEnterpriseCreation(ctx, record, id)
	}
	current, remoteETag, err := s.getEnterpriseCredential(ctx, id)
	if errors.Is(err, errObjectMissing) || (err == nil && remoteETag != etag) {
		return auth.ErrEnterpriseOAuthImportConflict
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(ge.String(current.Metadata, "email"), ge.String(record.Metadata, "email")) ||
		ge.String(current.Metadata, "client_id") != ge.String(record.Metadata, "client_id") ||
		(auth.IsEnterpriseAccountAuth(current) && !auth.IsEnterpriseAccountAuth(record)) ||
		(auth.IsEnterpriseRetiredAuth(current) && !auth.IsEnterpriseRetiredAuth(record)) {
		return auth.ErrEnterpriseOAuthImportConflict
	}
	// Internal refresh, graph edits and legacy OAuth saves cannot delete the
	// durable evidence or roll back its authorization generation.
	if auth.IsEnterpriseAccountAuth(current) &&
		auth.EnterpriseOAuthAuthorizationRevision(current) != auth.EnterpriseOAuthAuthorizationRevision(record) {
		return auth.ErrEnterpriseOAuthImportConflict
	}
	for _, key := range []string{auth.EnterpriseOAuthReceiptsKey, auth.EnterpriseOAuthSubjectKey, auth.EnterpriseOAuthRevisionKey} {
		if value, present := current.Metadata[key]; present {
			record.Metadata[key] = value
		} else {
			delete(record.Metadata, key)
		}
	}
	return nil
}

func (s *ObjectTokenStore) guardEnterpriseCreation(ctx context.Context, record *auth.Auth, id string) error {
	account := ge.AccountID(ge.String(record.Metadata, "email"), ge.String(record.Metadata, "client_id"))
	if account == "" || id != auth.EnterpriseOAuthCredentialID(account) || !auth.IsEnterpriseAccountAuth(record) {
		return auth.ErrEnterpriseAuthVersionRequired
	}
	requestID, _, err := s.loadImportOwner(ctx, account)
	if errors.Is(err, errObjectMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	intent, saved, err := s.LookupEnterpriseOAuthImport(ctx, requestID)
	if err != nil {
		return err
	}
	if saved != nil || intent.Status != "failed" {
		return auth.ErrEnterpriseOAuthImportConflict
	}
	return nil
}

func (s *ObjectTokenStore) mirroredEnterpriseETag(file string, body []byte) string {
	snapshot, ok := s.enterpriseMirror.Load(file)
	if ok && snapshot.(objectAuthSnapshot).digest == sha256.Sum256(body) {
		return snapshot.(objectAuthSnapshot).etag
	}
	return ""
}

func (s *ObjectTokenStore) bootstrapVersionedAuth(ctx context.Context, file, key string) error {
	body, etag, err := s.getVersionedObject(ctx, key, maxObjectAuthRecord)
	if err != nil {
		return err
	}
	if enterpriseObjectBody(body) {
		return s.updateEnterpriseMirror(file, body, etag)
	}
	s.enterpriseMirror.Delete(file)
	return os.WriteFile(file, body, 0o600)
}

func (s *ObjectTokenStore) updateEnterpriseMirror(file string, body []byte, etag string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".enterprise-auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(body)
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	s.enterpriseMirror.Store(file, objectAuthSnapshot{digest: sha256.Sum256(body), etag: etag})
	return nil
}
