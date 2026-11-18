package store

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

func TestObjectModelVerificationUsesConditionalDurableJournal(t *testing.T) {
	_, endpoint := newTestS3(t)
	first := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	second := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	unknown := checkedTestObjectStore(t, endpoint, t.TempDir(), false)
	const requestID = "model-verification-fixture"
	reservation := []byte(`{"state":"reconcile_required"}`)
	if unknown.SupportsEnterpriseModelVerificationJournal() ||
		unknown.WriteEnterpriseModelVerification(t.Context(), requestID, reservation, "") == nil {
		t.Fatal("object store without CAS accepted a verification reservation")
	}
	if _, _, err := first.ReadEnterpriseModelVerification(t.Context(), requestID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing journal entry: %v", err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for _, store := range []*ObjectTokenStore{first, second} {
		go func() {
			<-start
			results <- store.WriteEnterpriseModelVerification(t.Context(), requestID, reservation, "")
		}()
	}
	close(start)
	left, right := <-results, <-results
	if left == nil {
		left, right = right, left
	}
	if right != nil || !errors.Is(left, os.ErrExist) {
		t.Fatalf("want one reservation and one conflict: %v, %v", left, right)
	}
	body, etag, err := second.ReadEnterpriseModelVerification(t.Context(), requestID)
	if err != nil || !bytes.Equal(body, reservation) || etag == "" {
		t.Fatalf("independent reservation read failed: %v", err)
	}
	proof := []byte(`{"state":"runtime_pending","actual_model_id":"gemini-system-object-fixture"}`)
	if err = first.WriteEnterpriseModelVerification(t.Context(), requestID, proof, etag); err != nil {
		t.Fatal(err)
	}
	if err = second.WriteEnterpriseModelVerification(t.Context(), requestID, reservation, etag); !errors.Is(err, os.ErrExist) {
		t.Fatalf("a stale writer could overwrite the saved proof: %v", err)
	}
	body, newETag, err := second.ReadEnterpriseModelVerification(t.Context(), requestID)
	if err != nil || !bytes.Equal(body, proof) || newETag == etag {
		t.Fatalf("independent final receipt read failed: %v", err)
	}
}
