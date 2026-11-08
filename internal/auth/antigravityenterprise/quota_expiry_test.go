package antigravityenterprise

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestQuotaAdmitExpiresAtExactTTL(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var quota Quota
	err := json.Unmarshal([]byte(`{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":1}]}],"overageInfo":{}}`), &quota)
	if err != nil {
		t.Fatal(err)
	}
	quota.ObservedAt = now
	if err = quota.Admit(now.Add(QuotaTTL - time.Nanosecond)); err != nil {
		t.Fatalf("valid observation expired early: %v", err)
	}
	for _, age := range []time.Duration{QuotaTTL, QuotaTTL + time.Nanosecond} {
		var failure *Error
		if err = quota.Admit(now.Add(age)); !errors.As(err, &failure) || failure.HTTP != 503 || failure.Code != "metadata_pending" || !failure.IsRequestScoped() {
			t.Fatalf("age %v should require fresh metadata, got %v", age, err)
		}
	}
}
