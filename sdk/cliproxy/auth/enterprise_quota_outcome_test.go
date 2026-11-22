package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestEnterpriseQuotaLogicalStreakAcrossTimeRetriesAndSuccess(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		limited bool
	}{
		{"nine then 500 and one across seventeen minutes", true},
		{"same request retries successfully", false},
		{"another pool succeeds", true},
		{"later same pool success fences buffered rejection", false},
		{"empty generation does not break streak", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, now := quotaPressureTestManager()
			*now = now.Add(-time.Hour)
			a := quotaPressureAuth(t, m, "outcome-a", "12345", "us", true)
			b := quotaPressureAuth(t, m, "outcome-b", "12345", "us", true)
			other := quotaPressureAuth(t, m, "outcome-other", "12345", "eu", true)
			reject := func(ctx context.Context, auth *Auth) {
				quotaPressureAttempt(t, ctx, auth, plainGoogleRateLimit(auth, *now, quotaPressureTestModel, ""))
			}
			for range 9 {
				reject(WithEnterpriseQuotaRequest(t.Context()), a)
				*now = now.Add(time.Minute)
			}
			*now = now.Add(8 * time.Minute)
			ctx, done := BeginEnterpriseQuotaRequest(t.Context())
			switch scenario.name {
			case "nine then 500 and one across seventeen minutes":
				quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, &ge.GenerationError{HTTPStatus: 500})
			case "empty generation does not break streak":
				ticket, err := b.enterpriseQuota.beginForModel(quotaPressureTestModel)
				if err != nil {
					t.Fatal(err)
				}
				ticket.request = enterpriseQuotaRequestFromContext(ctx)
				ticket.finish(ticket.verifyProbe(ctx, quotaPressureTestModel, false))
			}
			reject(ctx, a)
			reject(ctx, b)
			if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "" {
				t.Fatalf("unsettled retry was treated as the tenth rejection: %+v", v)
			}
			switch scenario.name {
			case "same request retries successfully":
				quotaPressureAttempt(t, ctx, b, nil)
			case "another pool succeeds":
				quotaPressureAttempt(t, ctx, other, nil)
			case "later same pool success fences buffered rejection":
				quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), b, nil)
				for range 9 {
					reject(WithEnterpriseQuotaRequest(t.Context()), a)
				}
			}
			done()
			if v := m.EnterpriseQuotaBalance(a); (v.ProtectionStatus == "rate_limited") != scenario.limited {
				t.Fatalf("unexpected pool state: %+v", v)
			}
			if v := m.EnterpriseQuotaBalance(other); v.ProtectionStatus != "" {
				t.Fatalf("other region: %+v", v)
			}
		})
	}
}

func TestEnterpriseQuotaPendingStreakRestoresOnlyItsQuotaDay(t *testing.T) {
	for _, nextDay := range []bool{false, true} {
		m, now := quotaPressureTestManager()
		a := quotaPressureAuth(t, m, "streak-disk", "12345", "us", true)
		dir := t.TempDir()
		if err := m.ConfigureEnterpriseQuotaProtectionStore(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
		for range 9 {
			quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, plainGoogleRateLimit(a, *now, quotaPressureTestModel, "95"))
		}
		restarted, later := quotaPressureTestManager()
		*later = now.Add(time.Minute)
		if nextDay {
			*later = ge.NextPacificMidnight(*now)
		}
		member := quotaPressureAuth(t, restarted, "streak-disk", "12345", "us", true)
		if err := restarted.ConfigureEnterpriseQuotaProtectionStore(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
		quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), member, plainGoogleRateLimit(member, *later, quotaPressureTestModel, ""))
		view := restarted.EnterpriseQuotaBalance(member)
		if nextDay {
			if view.ProtectionStatus != "" {
				t.Fatalf("yesterday's count triggered protection: %+v", view)
			}
			continue
		}
		if view.ProtectionStatus != "rate_limited" || view.RetryAt == nil || !view.RetryAt.Equal(now.Add(95*time.Second)) {
			t.Fatalf("restored streak lost count or upstream deadline: %+v", view)
		}
		*later = *view.RetryAt
		probe, err := member.enterpriseQuota.beginForModel(quotaPressureTestModel)
		if err != nil {
			t.Fatal(err)
		}
		*later = later.Add(2 * time.Minute)
		probe.finish(plainGoogleRateLimit(member, *later, quotaPressureTestModel, ""))
		view = restarted.EnterpriseQuotaBalance(member)
		if view.RetryAt == nil || !view.RetryAt.Equal(later.Add(enterpriseQuotaProbeSpacing)) {
			t.Fatalf("probe delay started before receipt: %+v", view)
		}
		if _, err = member.enterpriseQuota.beginForModel(quotaPressureTestModel); statusCodeFromError(err) != http.StatusTooManyRequests {
			t.Fatalf("immediate second probe: %v", err)
		}
		*later = *view.RetryAt
		quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), member, nil)
		if restarted.EnterpriseQuotaBalance(member).ProtectionStatus != "" {
			t.Fatal("a successful probe did not reopen the pool")
		}
	}
}

func TestEnterpriseQuotaBalanceExposesOnlyAcceptedPaidObservation(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "amounts", "12345", "us", true)
	trial := quotaPressureAuth(t, m, "trial-amounts", "12345", "us", false)
	snapshot := balanceObservation(a, *now, 1)
	used, limit, watermark := int64(10199), int64(10200), now.Add(-10*time.Minute)
	snapshot.Pools[0].Used, snapshot.Pools[0].Limit, snapshot.Pools[0].UsageUpdatedAt = &used, &limit, &watermark
	if !m.ObserveEnterpriseQuota(a, snapshot) {
		t.Fatal("observation rejected")
	}
	view := m.EnterpriseQuotaBalance(a)
	if view.Used == nil || *view.Used != used || view.Limit == nil || *view.Limit != limit || view.Remaining == nil || *view.Remaining != 1 ||
		view.UsageUpdatedAt == nil || !view.UsageUpdatedAt.Equal(watermark) || view.ObservedAt == nil || !view.ObservedAt.Equal(*now) || view.PeriodStart == nil || view.ResetAt == nil {
		t.Fatalf("incomplete runtime read: %+v", view)
	}
	if v := m.EnterpriseQuotaBalance(trial); v.Used != nil || v.Limit != nil {
		t.Fatalf("paid fields leaked into trial view: %+v", v)
	}
	m.SetConfig(&config.Config{Routing: config.RoutingConfig{GeminiEnterpriseQuotaBalancing: true}})
	observeBalance(t, m, trial, *now, 9000)
	if v := m.EnterpriseQuotaBalance(a); v.Used == nil || *v.Used != used || v.UsedPercent == nil || *v.Remaining != 1 {
		t.Fatalf("trial observation replaced paid evidence: %+v", v)
	}
	if v := m.EnterpriseQuotaBalance(trial); v.Remaining == nil || *v.Remaining != 9000 || v.UsedPercent != nil {
		t.Fatalf("trial lost its routing estimate: %+v", v)
	}
	*now = now.Add(EnterpriseQuotaObservationTTL)
	if m.ObserveEnterpriseQuota(a, snapshot) || m.EnterpriseQuotaBalance(a).UsedPercent != nil {
		t.Fatal("five-minute-old read qualified for protection")
	}
	if m.EnterpriseQuotaBalance(a).Used == nil {
		t.Fatal("last accepted snapshot unavailable to the dashboard")
	}
}
