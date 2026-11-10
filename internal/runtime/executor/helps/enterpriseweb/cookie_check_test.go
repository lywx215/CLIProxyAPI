package enterpriseweb

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type cookieGateWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *cookieGateWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestCookiePreparationSharesFailedAuthenticationWithWaitingRequests(t *testing.T) {
	r := enabledRuntime()
	c := credentialFixture(t)
	c.ModelPolicy = PolicyArgonOnly
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client := &http.Client{Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-finish:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return &http.Response{StatusCode: http.StatusBadGateway, Body: http.NoBody}, nil
	})}
	prepare := func(ctx context.Context) error {
		credential := c
		_, release, err := r.PrepareCookie(ctx, &credential, client, CookieSelection{ID: "web"}, "route", ArgonModel, false)
		if release != nil {
			release()
		}
		return err
	}
	results := make(chan error, 2)
	go func() { results <- prepare(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first authentication did not start")
	}
	second := &cookieGateWaitContext{Context: ctx, waiting: make(chan struct{})}
	go func() { results <- prepare(second) }()
	select {
	case <-second.waiting:
	case <-ctx.Done():
		t.Fatal("second authentication did not queue")
	}
	close(finish)
	for range 2 {
		if err := <-results; code(err) != http.StatusBadGateway {
			t.Fatalf("expected authentication failure, got %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("queued requests performed %d verifications", got)
	}
	if err := prepare(ctx); code(err) != http.StatusBadGateway || calls.Load() != 2 {
		t.Fatalf("later request did not retry authentication, got %v with %d verifications", err, calls.Load())
	}
}

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
	r.ReplaceVerifiedCookies("web", beforeCC, c, 1, 2)
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

func TestScheduledCookieChecksHaveTheirOwnSlot(t *testing.T) {
	r := NewRuntime()
	if release := r.ClaimCookieCheck(); release != nil {
		release()
		t.Fatal("disabled Web scheduled authentication")
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true})
	releaseCheck := r.ClaimCookieCheck()
	if releaseCheck == nil {
		t.Fatal("idle credential did not admit the check")
	}
	if release := r.ClaimCookieCheck(); release != nil {
		release()
		t.Fatal("scheduled checks were not limited to one")
	}
	releaseCheck()
	if status := r.Status(); status["active_requests"] != 0 || status["rejected_concurrency"] != uint64(0) {
		t.Fatalf("checks were counted as media traffic: %v", status)
	}
}
