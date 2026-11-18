package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
)

const modelVerificationJournalLimit = 64 * 1024

var modelVerificationID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// SupportsEnterpriseModelVerificationJournal requires the same verified
// conditional object operations as the Enterprise OAuth import journal.
func (s *ObjectTokenStore) SupportsEnterpriseModelVerificationJournal() bool {
	return s.SupportsEnterpriseOAuthImport()
}

func modelVerificationObjectKey(id string) string {
	return ".enterprise-model-verifications/" + id + ".receipt"
}

func (s *ObjectTokenStore) ReadEnterpriseModelVerification(ctx context.Context, id string) ([]byte, string, error) {
	if !s.SupportsEnterpriseModelVerificationJournal() || !modelVerificationID.MatchString(id) {
		return nil, "", errors.New("invalid Enterprise model verification journal scope")
	}
	body, etag, err := s.getVersionedObject(ctx, modelVerificationObjectKey(id), modelVerificationJournalLimit)
	if errors.Is(err, errObjectMissing) {
		return nil, "", os.ErrNotExist
	}
	return body, etag, err
}

func (s *ObjectTokenStore) WriteEnterpriseModelVerification(ctx context.Context, id string, body []byte, etag string) error {
	if !s.SupportsEnterpriseModelVerificationJournal() || !modelVerificationID.MatchString(id) ||
		len(body) > modelVerificationJournalLimit || !json.Valid(body) {
		return errors.New("invalid Enterprise model verification journal write")
	}
	if _, err := s.putVersionedObject(ctx, modelVerificationObjectKey(id), body, etag); err != nil {
		if errors.Is(err, errObjectChanged) {
			return os.ErrExist
		}
		return err
	}
	return nil
}
