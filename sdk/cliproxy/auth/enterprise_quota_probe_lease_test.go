package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func TestEnterprisePaidDailyProbeSurvivesExpiredLicenseAndCredentialUpdate(t *testing.T) {
	for _, scenario := range []string{"superseded success", "superseded Google 429"} {
		t.Run(scenario, func(t *testing.T) {
			m, now := quotaPressureTestManager()
			reset := ge.NextPacificMidnight(*now)
			key := "gemini-enterprise|12345|us|enterprise_plus|assistant_search"
			due := enterprisePaidDailyRecoveryAt(reset, quotaDigest(key))
			*now = due.Add(-enterpriseSubscriptionMaxStale + 10*time.Second)
			owner := quotaPressureAuth(t, m, "lease-expiring-owner", "12345", "us", true)
			quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), owner, &ge.GenerationError{
				HTTPStatus: http.StatusTooManyRequests, UpstreamHTTPStatus: http.StatusTooManyRequests,
				Restriction: &ge.Restriction{Kind: "daily", Scope: "edition", Edition: "enterprise_plus", Feature: "assistant_search",
					RetryAt: reset, TimeSource: "estimated_pacific_midnight"},
			})
			*now = due.Add(-time.Minute)
			sibling := quotaPressureAuth(t, m, "lease-fresh-sibling", "12345", "us", true)
			*now = due
			first, err := owner.enterpriseQuota.beginForModel(quotaPressureTestModel)
			if err != nil || !first.paidRecovery {
				t.Fatalf("first paid recovery request was not admitted: %v", err)
			}
			t.Cleanup(func() { first.finish(context.Canceled) })

			*now = due.Add(11 * time.Second)
			if owner.enterpriseQuota.paidQuotaKey(*now) != "" {
				t.Fatal("fixture did not expire the owner's license proof")
			}
			latest, _ := m.GetByID(owner.ID)
			latest.Metadata["refresh_token"] = "synthetic-renewed"
			latest.Metadata["note"] = "updated during the probe"
			if _, err := m.Update(t.Context(), latest); err != nil {
				t.Fatal(err)
			}
			if next, err := sibling.enterpriseQuota.beginForModel(quotaPressureTestModel); err == nil {
				next.finish(context.Canceled)
				t.Fatal("credential update released the occupied paid probe slot")
			} else if _, blocked := errors.AsType[*enterpriseLocalGateError](err); !blocked {
				t.Fatalf("expected the occupied probe to block the sibling, got: %v", err)
			}
			views := sibling.enterpriseQuota.views(*now)
			if len(views) != 1 || views[0].Phase != "probing" {
				t.Fatalf("expected shared pool to remain probing after credential update: %+v", views)
			}
			if _, err := m.ResetSharedQuota(t.Context(), sibling.ID, views[0].PoolID, views[0].PoolVersion); err == nil {
				t.Fatal("explicit retry was allowed to race the occupied probe")
			}

			if scenario == "superseded Google 429" {
				first.finish(plainGoogleRateLimit(owner, *now, quotaPressureTestModel, "90"))
				views = sibling.enterpriseQuota.views(*now)
				if len(views) != 1 || views[0].Phase != "cooling" || !views[0].RetryAt.Equal(now.Add(90*time.Second)) {
					t.Fatalf("the retired probe's upstream rate limit did not space the next attempt: %+v", views)
				}
				*now = views[0].RetryAt
			} else {
				first.finish(nil)
			}
			views = sibling.enterpriseQuota.views(*now)
			if len(views) != 1 || views[0].Phase != "probe_pending" {
				t.Fatalf("the superseded probe changed the current pool's verification requirement: %+v", views)
			}
			quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), sibling, nil)
			if views := sibling.enterpriseQuota.views(*now); len(views) != 0 {
				t.Fatalf("current credential could not recover the shared pool: %+v", views)
			}
			saved, _ := m.GetByID(owner.ID)
			if saved.Metadata["refresh_token"] != "synthetic-renewed" || saved.Metadata["note"] != "updated during the probe" || saved.Disabled {
				t.Fatal("a recovery result changed the refreshed credential or concurrent settings")
			}
		})
	}
}
