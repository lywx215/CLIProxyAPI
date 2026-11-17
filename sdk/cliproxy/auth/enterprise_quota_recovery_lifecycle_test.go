package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func recoveryLifecycleRejection(t *testing.T, a *Auth, now time.Time, period, retryAfter string) *ge.GenerationError {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{
		"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{
			"@type":"type.googleapis.com/google.rpc.ErrorInfo",
			"domain":"googleapis.com",
			"metadata":{
				"service":"discoveryengine.googleapis.com",
				"consumer":"projects/12345",
				"location":"us",
				"quota_metric":"discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional",
				"quota_limit":"TextAnswersPer%s"
			}
		}]}
	}`, period))
	result := ge.ParseGenerationError(raw, http.StatusTooManyRequests, http.Header{"Retry-After": {retryAfter}},
		ge.FromMetadata(a.Metadata), a.EnterpriseQuotaIdentity, now, false)
	if result.Restriction == nil || result.Restriction.Scope != "edition" {
		t.Fatalf("fixture did not parse as a shared edition quota: %+v", result)
	}
	result.Model = quotaPressureTestModel
	return result
}

func assertRecoveryLifecycleBlocked(t *testing.T, a *Auth) {
	t.Helper()
	next, err := a.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err == nil {
		next.finish(context.Canceled)
		t.Fatal("another recovery request was admitted")
	}
	if _, ok := errors.AsType[*enterpriseLocalGateError](err); !ok {
		t.Fatalf("expected a shared-quota block, got %v", err)
	}
}

func TestEnterprisePaidQuotaRetiredProbeHonorsSamePoolStructuredRetryAfter(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "retired-structured-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "retired-structured-b", "12345", "us", true)
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, recoveryLifecycleRejection(t, a, *now, "Day", ""))
	views := b.enterpriseQuota.views(*now)
	if len(views) != 1 || views[0].RestrictionKind != "daily" {
		t.Fatalf("expected an initial paid daily cooldown: %+v", views)
	}
	*now = views[0].RetryAt
	probe, err := a.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.finish(context.Canceled)

	updated, _ := m.GetByID(a.ID)
	updated.Metadata["refresh_token"] = "synthetic-structured-rotation"
	if _, errUpdate := m.Update(t.Context(), updated); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	assertRecoveryLifecycleBlocked(t, b)

	retryAt := now.Add(180 * time.Second)
	probe.finish(recoveryLifecycleRejection(t, a, *now, "Minute", "180"))
	views = b.enterpriseQuota.views(*now)
	if len(views) != 1 || views[0].Phase != "cooling" || views[0].RestrictionKind != "daily" || !views[0].RetryAt.Equal(retryAt) {
		t.Fatalf("retired same-pool probe did not extend the existing restriction to the upstream deadline: %+v", views)
	}
	*now = now.Add(time.Minute)
	assertRecoveryLifecycleBlocked(t, b)
	*now = retryAt
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), b, nil)
	if views := b.enterpriseQuota.views(*now); len(views) != 0 {
		t.Fatalf("current recovery probe did not clear the pool: %+v", views)
	}
}

func TestEnterprisePaidQuotaRecoverySurvivesMinuteRestriction(t *testing.T) {
	for _, test := range []struct {
		name    string
		restart bool
	}{
		{name: "in memory"},
		{name: "after restoring cooldown", restart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, now := quotaPressureTestManager()
			a := quotaPressureAuth(t, m, "minute-lifecycle-a", "12345", "us", true)
			b := quotaPressureAuth(t, m, "minute-lifecycle-b", "12345", "us", true)
			store := NewFileCooldownStateStore(t.TempDir())
			m.SetCooldownStateStore(store)
			quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, recoveryLifecycleRejection(t, a, *now, "Day", ""))
			views := b.enterpriseQuota.views(*now)
			if len(views) != 1 || views[0].Phase != "cooling" || views[0].RestrictionKind != "daily" {
				t.Fatalf("daily quota rejection did not close the shared pool: %+v", views)
			}

			*now = views[0].RetryAt
			first, errFirst := a.enterpriseQuota.beginForModel(quotaPressureTestModel)
			if errFirst != nil || !first.paidRecovery {
				t.Fatalf("daily cooldown did not admit a protected probe: %v", errFirst)
			}
			first.finish(recoveryLifecycleRejection(t, a, *now, "Minute", "5"))
			minuteRetry := now.Add(time.Minute)
			views = b.enterpriseQuota.views(*now)
			if len(views) != 1 || views[0].Phase != "cooling" || views[0].RestrictionKind != "minute" || !views[0].RetryAt.Equal(minuteRetry) {
				t.Fatalf("minute rejection did not retain the protected probe cooldown: %+v", views)
			}
			*now = minuteRetry
			if test.restart {
				m, now = quotaPressureTestManager()
				*now = minuteRetry
				// A cold manager has no current paid verification yet.
				a = sharedQuotaAuth(t, m, a.ID, "12345", "us", "enterprise_plus", a.ID)
				b = sharedQuotaAuth(t, m, b.ID, "12345", "us", "enterprise_plus", b.ID)
				m.SetCooldownStateStore(store)
				if errRestore := m.RestoreCooldownStates(t.Context()); errRestore != nil {
					t.Fatal(errRestore)
				}
				if a.enterpriseQuota.paidQuotaKey(*now) != "" || b.enterpriseQuota.paidQuotaKey(*now) != "" {
					t.Fatal("fixture unexpectedly republished paid verification after the restart")
				}
			}

			second, errSecond := a.enterpriseQuota.beginForModel(quotaPressureTestModel)
			if errSecond != nil {
				t.Fatalf("could not begin minute recovery: %v", errSecond)
			}
			t.Cleanup(func() { second.finish(context.Canceled) })
			if !second.paidRecovery {
				t.Error("minute limit lost paid recovery protection")
			}
			current, _ := m.GetByID(a.ID)
			current.Metadata["refresh_token"] = "synthetic-rotated-minute-lifecycle"
			if _, errUpdate := m.Update(t.Context(), current); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			assertRecoveryLifecycleBlocked(t, b)

			second.finish(plainGoogleRateLimit(a, *now, quotaPressureTestModel, "120"))
			googleRetry := now.Add(2 * time.Minute)
			views = b.enterpriseQuota.views(*now)
			if len(views) != 1 || views[0].Phase != "cooling" || views[0].RestrictionKind != "minute" || !views[0].RetryAt.Equal(googleRetry) {
				t.Fatalf("rejected minute recovery probe lost Retry-After: %+v", views)
			}
			*now = googleRetry.Add(-time.Nanosecond)
			assertRecoveryLifecycleBlocked(t, b)
			*now = googleRetry
			third, errThird := b.enterpriseQuota.beginForModel(quotaPressureTestModel)
			if errThird != nil || !third.paidRecovery {
				t.Fatalf("shared pool did not admit one protected probe at Retry-After: %v", errThird)
			}
			third.finish(nil)
			if views := b.enterpriseQuota.views(*now); len(views) != 0 {
				t.Fatalf("successful recovery did not reopen the shared pool: %+v", views)
			}
		})
	}
}

func TestEnterpriseLegacyMinuteSnapshotWithoutPaidRecoveryField(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "minute-legacy-trial", "12345", "us", false)
	dir := t.TempDir()
	retryAt := now.Add(time.Minute)
	key := a.enterpriseQuota.keys()["edition"]
	data := fmt.Appendf(nil, `{"schema_version":1,"version":1,"pools":[{
		"pool_id":%q,"key":%q,"version":1,"reset_epoch":0,"phase":"cooling",
		"restriction":{"kind":"minute","scope":"edition","feature":"assistant_search","edition":"enterprise_plus","retry_at":%q,"time_source":"google"}
	}],"bindings":[]}`, quotaDigest(key), key, retryAt.Format(time.RFC3339))
	if errWrite := os.WriteFile(filepath.Join(dir, "shared-quota-state.sqs"), data, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	m.SetCooldownStateStore(NewFileCooldownStateStore(dir))
	if errRestore := m.RestoreCooldownStates(t.Context()); errRestore != nil {
		t.Fatalf("could not restore an older minute snapshot: %v", errRestore)
	}

	assertRecoveryLifecycleBlocked(t, a)
	*now = retryAt
	ticket, errBegin := a.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if errBegin != nil || len(ticket.probes) != 1 || ticket.paidRecovery {
		t.Fatalf("older trial cooldown did not admit its ordinary shared-quota probe: %v", errBegin)
	}
	ticket.finish(nil)
}
