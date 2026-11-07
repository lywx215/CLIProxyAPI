package enterpriseweb

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCookieCheckCadenceAndCredentialChanges(t *testing.T) {
	r := NewRuntime()
	now := time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	c := Credential{Cookies: "SID=synthetic-old; SAPISID=synthetic-signing"}
	if _, ok := r.NextCookieCheck("web", c, now); ok {
		t.Fatal("disabled channel scheduled authentication")
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true})
	assertDue := func(when time.Time) {
		t.Helper()
		if next, ok := r.NextCookieCheck("web", c, now); !ok || !next.Equal(when) {
			t.Fatalf("next check = %s, %t, want %s", next, ok, when)
		}
	}
	assertDue(now)
	r.recordCookieOutcome("web", 0, c, nil, true, "")
	due := now.Add(10*time.Minute + cookieCheckOffset("web"))
	assertDue(due)
	now = now.Add(4 * time.Minute)
	r.recordCookieOutcome("web", 0, c, nil, false, "")
	assertDue(due)
	beforeCC := c
	c.Cookies += "; SIDCC=synthetic-new-cc"
	r.ReplaceVerifiedCookies("web", beforeCC, c)
	assertDue(due)
	now = due
	r.recordCookieOutcome("web", 1, c, nil, true, cookieRotationUnauthorized)
	assertDue(now.Add(5 * time.Minute))
	if status := r.CredentialStatus("web", c); status["cookie_check_status"] != "renewal_degraded" || status["cookie_last_verified_at"] != now {
		t.Fatal(status)
	}
	r.recordCookieOutcome("web", 1, c, &upstreamCookieFailure{error: Fail(http.StatusTooManyRequests, "rate limited"), retry: 12 * time.Minute}, true, "")
	assertDue(now.Add(12 * time.Minute))
	if status := r.CredentialStatus("web", c); status["cookie_check_reason"] != cookieRateLimited {
		t.Fatal(status)
	}
	r.recordCookieOutcome("web", 1, c, Fail(http.StatusForbidden, "permission denied"), true, "")
	assertDue(now.Add(5 * time.Minute))
	if status := r.CredentialStatus("web", c); status["cookie_check_reason"] != cookieAccessDenied {
		t.Fatal(status)
	}
	r.recordCookieOutcome("web", 1, c, loginRequired(), true, "")
	if _, ok := r.NextCookieCheck("web", c, now); ok {
		t.Fatal("rejected session remained scheduled")
	}
	r.recordCookieOutcome("web", 1, c, nil, false, "")
	assertDue(now)
	r.ForgetCredential("web")
	r.recordCookieOutcome("web", 1, c, loginRequired(), true, "")
	c.Cookies = "SID=synthetic-new; SAPISID=synthetic-signing"
	assertDue(now)
	if _, exists := r.CredentialStatus("web", c)["cookie_check_status"]; exists {
		t.Fatal("late result contaminated the replacement session")
	}
}

func TestCookieCheckYieldsToBusyCredential(t *testing.T) {
	r := NewRuntime()
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true})
	c := Credential{Cookies: "SID=synthetic; SAPISID=synthetic-signing"}
	releaseMedia, err := r.ClaimCredential("cookie:" + c.CookieKey())
	if err != nil {
		t.Fatal(err)
	}
	if release := r.ClaimCookieCheck("web", c); release != nil {
		release()
		t.Fatal("check overlapped media on the same session")
	}
	releaseMedia()
	releaseCheck := r.ClaimCookieCheck("web", c)
	if releaseCheck == nil {
		t.Fatal("idle credential did not admit the check")
	}
	if release := r.ClaimCookieCheck("other", Credential{Cookies: "SID=synthetic-other; SAPISID=synthetic-signing"}); release != nil {
		release()
		t.Fatal("scheduled checks were not limited to one")
	}
	releaseCheck()
	if status := r.Status(); status["active_requests"] != 0 || status["rejected_concurrency"] != uint64(0) {
		t.Fatalf("checks were counted as media traffic: %v", status)
	}
}
