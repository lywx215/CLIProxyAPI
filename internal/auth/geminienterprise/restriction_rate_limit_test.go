package geminienterprise

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestEnterpriseUnscopedRateLimitClassification(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	binding := config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "us"}
	identity := &QuotaIdentity{Verified: true, Paid: true, Binding: Binding(binding), ProjectNumber: "12345", Location: "us", Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: now}
	const textMetric = "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional"
	tests := []struct {
		name       string
		status     int
		body       []byte
		count      bool
		scope      string
		retryDelay time.Duration
	}{
		{name: "bare HTTP 429", status: 429, body: []byte("Too Many Requests"), count: true},
		{name: "RPC stream 429", status: 200, body: []byte(`{"error":{"code":8,"status":"RESOURCE_EXHAUSTED"}}`), count: true},
		{name: "only RetryInfo", status: 429, body: []byte(`{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"95s"}]}}`), count: true, retryDelay: 95 * time.Second},
		{name: "conflicting RPC code", status: 429, body: []byte(`{"error":{"code":7,"status":"RESOURCE_EXHAUSTED"}}`)},
		{name: "conflicting transport status", status: 503, body: []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`)},
		{name: "structured edition quota", status: 429, body: restrictionFixture(textMetric, "TextAnswersPerDay", "12345", "us"), scope: "edition"},
		{name: "structured user quota", status: 429, body: restrictionFixture(textMetric, "TextAnswersPerDayPerUser", "12345", "us"), scope: "user"},
		{name: "structured project quota", status: 429, body: restrictionFixture("discoveryengine.googleapis.com/requests", "RequestsPerMinute", "12345", "us"), scope: "project"},
		{name: "unknown quota limit", status: 429, body: restrictionFixture(textMetric, "UnknownLimit", "12345", "us")},
		{name: "foreign project", status: 429, body: restrictionFixture(textMetric, "TextAnswersPerDay", "67890", "us")},
		{name: "foreign location", status: 429, body: restrictionFixture(textMetric, "TextAnswersPerDay", "12345", "eu")},
		{name: "invalid QuotaFailure detail", status: 429, body: []byte(`{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":"invalid"}]}}`)},
		{name: "invalid details envelope", status: 429, body: []byte(`{"error":{"code":429,"details":{"@type":"type.googleapis.com/google.rpc.QuotaFailure"}}}`)},
		{name: "invalid top-level details", status: 429, body: []byte(`{"code":429,"details":false}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseGenerationError(test.body, test.status, nil, binding, identity, now, false)
			if got.UnscopedRateLimit != test.count {
				t.Errorf("unscoped rate limit = %t, want %t", got.UnscopedRateLimit, test.count)
			}
			if test.count && got.HTTPStatus != http.StatusTooManyRequests {
				t.Errorf("unscoped rate limit used HTTP %d, want 429", got.HTTPStatus)
			}
			scope := ""
			if got.Restriction != nil {
				scope = got.Restriction.Scope
			}
			if scope != test.scope {
				t.Errorf("restriction scope = %q, want %q", scope, test.scope)
			}
			if test.retryDelay != 0 && !got.RetryAt.Equal(now.Add(test.retryDelay)) {
				t.Errorf("retry at = %s, want %s", got.RetryAt, now.Add(test.retryDelay))
			}
		})
	}
}

func TestEnterpriseVerifiedDailyResetPrecedence(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	binding := config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "us"}
	identity := &QuotaIdentity{Verified: true, Paid: true, Binding: Binding(binding), ProjectNumber: "12345", Location: "us", Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: now}
	const textMetric = "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional"
	explicitReset := now.Add(2 * time.Hour)
	pacificMidnight := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	withExplicitReset := []byte(fmt.Sprintf(
		`{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","metadata":{"consumer":"projects/12345","location":"us","quota_metric":%q,"quota_limit":"TextAnswersPerDay","reset_time":%q}}]}}`,
		textMetric, explicitReset.Format(time.RFC3339),
	))
	tests := []struct {
		name       string
		body       []byte
		retryAfter string
		resetAt    time.Time
		retryAt    time.Time
		timeSource string
	}{
		{"short upstream hint", restrictionFixture(textMetric, "TextAnswersPerDay", "12345", "us"), "30", time.Time{}, now.Add(30 * time.Second), "google"},
		{"no upstream hint", restrictionFixture(textMetric, "TextAnswersPerDay", "12345", "us"), "", time.Time{}, pacificMidnight, "estimated_pacific_midnight"},
		{"explicit reset takes precedence", withExplicitReset, "30", explicitReset, explicitReset, "google"},
		{"longer upstream hint extends wait", withExplicitReset, "21600", explicitReset, now.Add(6 * time.Hour), "google"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseGenerationError(test.body, 429, http.Header{"Retry-After": {test.retryAfter}}, binding, identity, now, false)
			r := got.Restriction
			if r == nil || r.Kind != "daily" || r.Scope != "edition" || r.TimeSource != test.timeSource ||
				!r.ResetAt.Equal(test.resetAt) || !r.RetryAt.Equal(test.retryAt) || !got.RetryAt.Equal(test.retryAt) {
				t.Fatalf("daily restriction = %+v, retry at = %s, want reset %s and retry %s", r, got.RetryAt, test.resetAt, test.retryAt)
			}
		})
	}
}
