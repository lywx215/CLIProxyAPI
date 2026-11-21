package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestEnterpriseQuotaProtectionLimitsTenConsecutiveLogical429s(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "guard-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "guard-b", "12345", "us", true)
	trial := quotaPressureAuth(t, m, "guard-trial", "12345", "us", false)
	other := quotaPressureAuth(t, m, "guard-other", "12345", "eu", true)
	a.Attributes, b.Attributes = map[string]string{"priority": "10"}, map[string]string{"priority": "10"}
	reject := func(ctx context.Context, auth *Auth, retryAfter string) {
		t.Helper()
		quotaPressureAttempt(t, ctx, auth, plainGoogleRateLimit(auth, *now, quotaPressureTestModel, retryAfter))
	}
	logical := WithEnterpriseQuotaRequest(t.Context())
	reject(logical, a, "")
	for range 10 {
		reject(logical, b, "")
	}
	for range 8 {
		reject(WithEnterpriseQuotaRequest(t.Context()), a, "")
	}
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "" {
		t.Fatalf("duplicate retries activated protection: %+v", v)
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), b, nil)
	reject(WithEnterpriseQuotaRequest(t.Context()), a, "")
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "" {
		t.Fatalf("success did not interrupt the streak: %+v", v)
	}
	*now = now.Add(2*time.Minute + time.Second)
	for range 9 {
		reject(WithEnterpriseQuotaRequest(t.Context()), b, "")
	}
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "" {
		t.Fatalf("expired 429 survived the two-minute window: %+v", v)
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, &enterpriseLocalGateError{newModelCooldownError("", ge.Provider, time.Second)})
	inFlight, err := a.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err != nil {
		t.Fatal(err)
	}
	reject(WithEnterpriseQuotaRequest(t.Context()), b, "70")
	v := m.EnterpriseQuotaBalance(a)
	if v.ProtectionStatus != "rate_limited" || v.ProtectionReason != "consecutive_429" || v.RetryAt == nil ||
		!v.RetryAt.Equal(now.Add(70*time.Second)) {
		t.Fatalf("tenth 429 did not apply the longer upstream deadline: %+v", v)
	}
	if !m.EnterpriseQuotaRefreshNeeded(b) {
		t.Fatal("protected pool did not request a background observation")
	}
	for _, independent := range []*Auth{trial, other} {
		if blocked, _ := independent.enterpriseQuota.blocked(*now); blocked {
			t.Fatalf("protection escaped its paid quota pool: %s", independent.ID)
		}
	}
	*now = *v.RetryAt
	if lease, err := b.enterpriseQuota.beginForModel(quotaPressureTestModel); statusCodeFromError(err) != http.StatusTooManyRequests {
		lease.finish(nil)
		t.Fatalf("rate-limited pool admitted while a pre-limit request was in flight: %v", err)
	}
	inFlight.finish(nil)
	available, err := getAvailableAuths([]*Auth{a, b, other}, ge.Provider, quotaPressureTestModel, *now)
	if err != nil || len(available) != 1 || available[0].ID != other.ID {
		t.Fatalf("eligible healthy region was not preferred: %v %v", available, err)
	}
	probe, err := b.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := a.enterpriseQuota.beginForModel(quotaPressureTestModel); statusCodeFromError(err) != http.StatusTooManyRequests {
		second.finish(nil)
		t.Fatalf("same pool admitted a second request: %v", err)
	}
	probe.finish(nil)
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "rate_limited" || v.RetryAt == nil ||
		!v.RetryAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("success without a current quota reading opened the limited pool early: %+v", v)
	}
	observeBalance(t, m, b, *now, 600)
	*now = now.Add(30 * time.Second)
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, nil)
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "" {
		t.Fatalf("verified single-probe recovery left a protection record: %+v", v)
	}
}

func TestEnterpriseQuotaProtectionPausesOnlyLimitedHighUsage(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "guard-95", "12345", "us", true)
	observeBalance(t, m, a, *now, 500)
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "" || v.UsedPercent == nil || *v.UsedPercent != 95 {
		t.Fatalf("high usage by itself changed admission or lost the percentage: %+v", v)
	}
	for range 10 {
		quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, plainGoogleRateLimit(a, *now, quotaPressureTestModel, ""))
	}
	v := m.EnterpriseQuotaBalance(a)
	if v.ProtectionStatus != "paused" || v.ProtectionReason != "high_usage" || v.RetryAt == nil ||
		v.RetryAt.Before(ge.NextPacificMidnight(*now)) {
		t.Fatalf("95%% paid pool with rate pressure was not paused: %+v", v)
	}
	*now = now.Add(2 * time.Minute)
	stale := balanceObservation(a, *now, 900)
	stale.Pools[0].Status = "stale"
	if m.ObserveEnterpriseQuota(a, stale) {
		t.Fatal("cache fallback qualified as an authoritative observation")
	}
	if _, err := a.enterpriseQuota.begin(); statusCodeFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("stale reading let a request reach the paused pool: %v", err)
	}
	observeBalance(t, m, a, *now, 900)
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "recovering" {
		t.Fatalf("fresh low usage did not propose a single recovery request: %+v", v)
	}
	quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, nil)
	if v := m.EnterpriseQuotaBalance(a); v.ProtectionStatus != "" {
		t.Fatalf("successful recovery kept a paid pool paused: %+v", v)
	}
}

func TestEnterpriseQuotaProtectionRestoresConfirmedZeroWithoutGeneralCooldownPersistence(t *testing.T) {
	dir := t.TempDir()
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "guard-zero-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "guard-zero-b", "12345", "us", true)
	if err := m.ConfigureEnterpriseQuotaProtectionStore(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	observeBalance(t, m, a, *now, 0)
	v := m.EnterpriseQuotaBalance(b)
	if v.ProtectionStatus != "paused" || v.ProtectionReason != "quota_exhausted" || v.RetryAt == nil {
		t.Fatalf("verified zero did not immediately pause the shared pool: %+v", v)
	}
	restarted, restartNow := quotaPressureTestManager()
	*restartNow = now.Add(EnterpriseQuotaObservationTTL + time.Second)
	for _, original := range []*Auth{a, b} {
		cloned := original.Clone()
		cloned.EnterpriseQuotaIdentity = nil
		if _, err := restarted.Register(t.Context(), cloned); err != nil {
			t.Fatal(err)
		}
	}
	if err := restarted.ConfigureEnterpriseQuotaProtectionStore(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	restored, _ := restarted.GetByID(a.ID)
	if current := restarted.EnterpriseQuotaBalance(restored); current.ProtectionStatus != "paused" || current.RetryAt == nil ||
		!current.RetryAt.Equal(*v.RetryAt) {
		t.Fatalf("restart reopened the previously verified depletion: %+v", current)
	}
	if _, err := restored.enterpriseQuota.begin(); statusCodeFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("restored depletion allowed upstream dispatch: %v", err)
	}
	*restartNow = *v.RetryAt
	probe, err := restored.enterpriseQuota.beginForModel(quotaPressureTestModel)
	if err != nil {
		t.Fatalf("reset did not admit a recovery request: %v", err)
	}
	sibling, _ := restarted.GetByID(b.ID)
	if second, err := sibling.enterpriseQuota.begin(); statusCodeFromError(err) != http.StatusTooManyRequests {
		second.finish(nil)
		t.Fatalf("restart allowed a concurrent recovery request: %v", err)
	}
	probe.finish(nil)
	if current := restarted.EnterpriseQuotaBalance(restored); current.ProtectionStatus != "" {
		t.Fatalf("successful post-reset recovery did not clear the pause: %+v", current)
	}
}

func TestEnterpriseQuotaProtectionReducesHighUsageWeight(t *testing.T) {
	m, now := quotaPressureTestManager()
	m.SetConfig(&config.Config{Routing: config.RoutingConfig{GeminiEnterpriseQuotaBalancing: true}})
	a := quotaPressureAuth(t, m, "guard-weight-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "guard-weight-b", "12345", "eu", true)
	observeBalance(t, m, a, *now, 500)
	observeBalance(t, m, b, *now, 501)
	selector := &QuotaBalancedSelector{}
	pickedA := 0
	for range 60 {
		if balancedPick(t, selector, a, b).ID == a.ID {
			pickedA++
		}
	}
	if pickedA != 20 {
		t.Fatalf("95%% pool selected %d times in 60 requests, want 20", pickedA)
	}
}
