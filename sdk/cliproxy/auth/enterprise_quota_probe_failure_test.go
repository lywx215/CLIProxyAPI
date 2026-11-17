package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const probeTestAmbiguousQuota = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"RATE_LIMIT_EXCEEDED","metadata":{"service":"discoveryengine.googleapis.com","consumer":"projects/12345","location":"us","quota_metric":"discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional","quota_limit":"TextAnswersBurstLimit"}}]}}`

func TestEnterpriseProbeFailureCooldown(t *testing.T) {
	now := time.Date(2026, 11, 17, 8, 4, 0, 0, time.UTC)
	binding := config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "us"}
	parse := func(status int, body, model string) *ge.GenerationError {
		generation := ge.ParseGenerationError([]byte(body), status, http.Header{"Retry-After": {"180"}}, binding, nil, now, false)
		generation.Model = model
		return generation
	}
	userQuota := strings.Replace(probeTestAmbiguousQuota, "TextAnswersBurstLimit", "TextAnswersPerDayPerUser", 1)
	projectQuota := strings.NewReplacer(
		"text_answer_gen_tier_enterprise_plus_regional", "requests",
		"TextAnswersBurstLimit", "RequestsPerMinute",
	).Replace(probeTestAmbiguousQuota)
	tests := []struct {
		name    string
		err     error
		wait    time.Duration
		limited bool
		scope   string
	}{
		{name: "success"},
		{name: "local cancellation", err: context.Canceled},
		{name: "canceled transport", err: &ge.TransportError{Cause: context.Canceled}},
		{name: "local rejection", err: &enterpriseLocalGateError{newModelCooldownError("", ge.Provider, time.Minute)}},
		{name: "status without upstream provenance", err: &ge.GenerationError{HTTPStatus: 429}},
		{name: "missing stream terminal event", err: io.ErrUnexpectedEOF, wait: time.Minute},
		{name: "upstream transport failure", err: &ge.TransportError{Cause: context.DeadlineExceeded}, wait: time.Minute},
		{name: "upstream 503", err: parse(503, "", quotaPressureTestModel), wait: time.Minute},
		{name: "429 without a retry hint", err: ge.ParseGenerationError(nil, 429, nil, binding, nil, now, false), wait: time.Minute, limited: true},
		{name: "ambiguous text 429", err: parse(429, probeTestAmbiguousQuota, quotaPressureTestModel), wait: 3 * time.Minute, limited: true},
		{name: "in-band 429", err: parse(200, `{"error":{"code":8,"status":"RESOURCE_EXHAUSTED"}}`, quotaPressureTestModel), wait: 3 * time.Minute, limited: true},
		{name: "in-band HTTP equivalent", err: parse(200, `{"error":{"code":429}}`, quotaPressureTestModel), wait: 3 * time.Minute, limited: true},
		{name: "known user quota", err: parse(429, userQuota, quotaPressureTestModel), wait: time.Minute, limited: true, scope: "user"},
		{name: "known project quota", err: parse(429, projectQuota, quotaPressureTestModel), wait: time.Minute, limited: true, scope: "project"},
		{name: "another quota feature", err: parse(429, "", "gemini-3-pro-image"), wait: time.Minute, limited: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := enterpriseGenerationError(tc.err)
			if tc.scope != "" && (upstream == nil || upstream.Restriction == nil || upstream.Restriction.Scope != tc.scope) {
				t.Fatalf("test fixture did not establish %s scope: %+v", tc.scope, upstream)
			}
			want := time.Time{}
			if tc.wait != 0 {
				want = now.Add(tc.wait)
			}
			if got := enterpriseProbeGoogleRateLimited(upstream); got != tc.limited {
				t.Errorf("Google 429 classification = %t, want %t", got, tc.limited)
			}
			if got := enterpriseProbeFailureCooldown(tc.err, upstream, now); !got.Equal(want) {
				t.Errorf("next probe = %s, want %s", got, want)
			}
		})
	}
}

func TestEnterpriseDailyQuotaProbeAmbiguous429DelaysSibling(t *testing.T) {
	m, now := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "pressure-ambiguous-probe-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "pressure-ambiguous-probe-b", "12345", "us", true)
	attempts := 0
	mock := &mockCustomErrorExecutor{
		identifier: ge.Provider,
		executeFn: func(_ context.Context, auth *Auth, req exec.Request, _ exec.Options) (exec.Response, error) {
			attempts++
			switch attempts {
			case 1, 2:
				body := probeTestAmbiguousQuota
				retryAfter := "180"
				if attempts == 1 {
					body = strings.Replace(body, "TextAnswersBurstLimit", "TextAnswersPerDay", 1)
					retryAfter = "15"
				}
				rejection := ge.ParseGenerationError([]byte(body), http.StatusTooManyRequests, http.Header{"Retry-After": {retryAfter}},
					ge.FromMetadata(auth.Metadata), auth.EnterpriseQuotaIdentity, *now, false)
				rejection.Model = req.Model
				return exec.Response{}, rejection
			default:
				return exec.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP"}]}`)}, nil
			}
		},
	}
	execute := func(auth *Auth) error {
		_, err := executorForAuth(mock, auth).Execute(WithEnterpriseQuotaRequest(t.Context()), auth, exec.Request{Model: quotaPressureTestModel}, exec.Options{})
		return err
	}
	if err := execute(a); statusCodeFromError(err) != http.StatusTooManyRequests || attempts != 1 {
		t.Fatalf("daily rejection did not reach the execution boundary: attempts=%d error=%v", attempts, err)
	}
	views := b.enterpriseQuota.views(*now)
	if len(views) != 1 || views[0].RestrictionKind != "daily" || views[0].Phase != "cooling" {
		t.Fatalf("parsed daily error did not restrict the sibling: %+v", views)
	}
	*now = views[0].RetryAt
	err := execute(b)
	upstream := enterpriseGenerationError(err)
	if upstream == nil || upstream.HTTPStatus != http.StatusTooManyRequests || upstream.UnscopedRateLimit || upstream.Restriction != nil || attempts != 2 {
		t.Fatalf("probe fixture did not return an ambiguous upstream 429: attempts=%d error=%v", attempts, err)
	}
	next := now.Add(3 * time.Minute)
	views = a.enterpriseQuota.views(*now)
	if len(views) != 1 || views[0].Phase != "cooling" || !views[0].RetryAt.Equal(next) {
		t.Fatalf("sibling did not honor ambiguous probe Retry-After: %+v", views)
	}
	*now = next.Add(-time.Nanosecond)
	var local *enterpriseLocalGateError
	if err := execute(a); !errors.As(err, &local) || attempts != 2 {
		t.Fatalf("sibling sent another probe too early: attempts=%d error=%v", attempts, err)
	}
	*now = next
	if err := execute(a); err != nil || attempts != 3 {
		t.Fatalf("sibling could not verify recovery at the next deadline: attempts=%d error=%v", attempts, err)
	}
	if views := b.enterpriseQuota.views(*now); len(views) != 0 {
		t.Fatalf("successful probe did not open the shared pool: %+v", views)
	}
}

func TestEnterpriseSupersededProbeRetryAtKeepsScope(t *testing.T) {
	now := time.Date(2026, 11, 17, 8, 4, 0, 0, time.UTC)
	binding := config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "us"}
	body := strings.Replace(probeTestAmbiguousQuota, "TextAnswersBurstLimit", "TextAnswersPerDay", 1)
	parsed := ge.ParseGenerationError([]byte(body), 429, http.Header{"Retry-After": {"180"}}, binding, nil, now, false)
	estimated := ge.ParseGenerationError([]byte(body), 429, nil, binding, nil, now, false)
	if parsed.Restriction == nil || parsed.Restriction.TimeSource != "google" ||
		estimated.Restriction == nil || estimated.Restriction.TimeSource != "estimated_pacific_midnight" {
		t.Fatal("fixtures did not establish the expected deadline provenance")
	}
	tests := []struct {
		name         string
		upstream     *ge.GenerationError
		scopedToPool bool
		want         time.Time
	}{
		{name: "local rejection does not extend the pool", upstream: &ge.GenerationError{HTTPStatus: 429}},
		{name: "upstream 5xx does not change a newer restriction", upstream: ge.ParseGenerationError(nil, 503, nil, binding, nil, now, false)},
		{name: "another pool owns the hint", upstream: parsed, want: now.Add(time.Minute)},
		{name: "same pool uses the Google hint", upstream: parsed, scopedToPool: true, want: now.Add(3 * time.Minute)},
		{name: "estimated midnight is not a Google hint", upstream: estimated, scopedToPool: true, want: now.Add(time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := enterpriseSupersededProbeRetryAt(tc.upstream, tc.scopedToPool, now.Add(time.Minute)); !got.Equal(tc.want) {
				t.Fatalf("late probe deadline = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestEnterpriseSupersededProbe429ExtendsOnlyDeadline(t *testing.T) {
	tests := []struct {
		name       string
		newerHint  string
		lateLimit  string
		lateHint   string
		delay      time.Duration
		retryAfter time.Duration
		userWait   time.Duration
	}{
		{name: "unscoped 429 at the newer deadline", newerHint: "90", lateHint: "120", delay: 90 * time.Second, retryAfter: 2 * time.Minute},
		{name: "edition minute 429 keeps the newer daily restriction", newerHint: "90", lateLimit: "TextAnswersPerMinute", lateHint: "180", delay: 90 * time.Second, retryAfter: 3 * time.Minute},
		{name: "another scope keeps its own hint", newerHint: "90", lateLimit: "TextAnswersPerDayPerUser", lateHint: "180", delay: 90 * time.Second, retryAfter: time.Minute, userWait: 3 * time.Minute},
		{name: "the longer existing deadline still applies", newerHint: "300", lateLimit: "TextAnswersPerMinute", lateHint: "120", delay: 10 * time.Second, retryAfter: 290 * time.Second},
		{name: "estimated reset does not defer the pool by another day", newerHint: "90", lateLimit: "TextAnswersPerDay", delay: 90 * time.Second, retryAfter: time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, now := quotaPressureTestManager()
			a := quotaPressureAuth(t, m, "paid-superseded-probe-a", "12345", "us", true)
			b := quotaPressureAuth(t, m, "paid-superseded-probe-b", "12345", "us", true)
			parsed429 := func(auth *Auth, limit, retryAfter string) *ge.GenerationError {
				body := ""
				if limit != "" {
					body = strings.Replace(probeTestAmbiguousQuota, "TextAnswersBurstLimit", limit, 1)
				}
				upstream := ge.ParseGenerationError([]byte(body), 429, http.Header{"Retry-After": {retryAfter}},
					ge.FromMetadata(auth.Metadata), auth.EnterpriseQuotaIdentity, *now, false)
				upstream.Model = quotaPressureTestModel
				return upstream
			}
			state := func() SharedQuotaState {
				c := &m.enterpriseQuota
				c.mu.Lock()
				defer c.mu.Unlock()
				return *c.pools[quotaDigest(a.enterpriseQuota.keys()["edition"])]
			}
			prior, err := a.enterpriseQuota.begin()
			if err != nil {
				t.Fatal(err)
			}
			quotaPressureAttempt(t, WithEnterpriseQuotaRequest(t.Context()), a, parsed429(a, "TextAnswersPerDay", "15"))
			*now = state().Restriction.RetryAt
			probe, err := b.enterpriseQuota.begin()
			if err != nil {
				t.Fatalf("expected the first recovery lease: %v", err)
			}
			prior.finish(parsed429(a, "TextAnswersPerDay", tc.newerHint))
			newer := state()
			if newer.Restriction.Kind != "daily" || !newer.Restriction.RetryAt.After(*now) || newer.probe == 0 {
				t.Fatalf("the newer restriction did not supersede the active probe: %+v", newer)
			}
			*now = now.Add(tc.delay)
			probe.finish(parsed429(b, tc.lateLimit, tc.lateHint))

			want := newer.Restriction
			want.RetryAt = now.Add(tc.retryAfter)
			if got := state(); got.Phase != "cooling" || got.probe != 0 || !sameEnterpriseRestriction(got.Restriction, want) {
				t.Fatalf("the late 429 must preserve the newer restriction and delay recovery: got %+v, want %+v", got, want)
			}
			if tc.userWait > 0 {
				c := &m.enterpriseQuota
				c.mu.Lock()
				user := c.pools[quotaDigest(b.enterpriseQuota.keys()["user"])].Restriction
				c.mu.Unlock()
				if user.Scope != "user" || !user.RetryAt.Equal(now.Add(tc.userWait)) {
					t.Fatalf("the user's own restriction lost its upstream deadline: %+v", user)
				}
			}
			*now = want.RetryAt.Add(-time.Nanosecond)
			if _, err := a.enterpriseQuota.begin(); err == nil {
				t.Fatal("sibling started another probe before the deadline")
			}
			*now = want.RetryAt
			next, err := a.enterpriseQuota.begin()
			if err != nil {
				t.Fatalf("expected one new probe after the deadline: %v", err)
			}
			next.finish(nil)
			if got := state(); got.Phase != "normal" {
				t.Fatalf("successful next probe did not reopen the pool: %+v", got)
			}
		})
	}
}
