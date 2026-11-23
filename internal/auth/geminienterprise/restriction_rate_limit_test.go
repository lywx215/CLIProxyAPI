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

func TestEnterpriseQuotaPressureHint(t *testing.T) {
	now := time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	binding := config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "us"}
	identity := &QuotaIdentity{Verified: true, Paid: true, Binding: Binding(binding), ProjectNumber: "12345", Location: "us", Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: now}
	const detail = `{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"RATE_LIMIT_EXCEEDED","metadata":{"service":"discoveryengine.googleapis.com","consumer":"projects/12345","location":"us","quota_metric":"discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional","quota_limit":"OpaqueTextLimit"}}`
	const violation = `{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"subject":"project:12345","apiService":"discoveryengine.googleapis.com"}]}`
	for _, test := range []struct {
		name, details string
		wantHint      bool
	}{
		{name: "compatible but unclassified", details: detail, wantHint: true},
		{name: "informational detail", details: `{"@type":"type.googleapis.com/google.rpc.RequestInfo","requestId":"example"}`, wantHint: true},
		{name: "compatible legacy quota subject", details: violation, wantHint: true},
		{name: "another quota service", details: strings.Replace(violation, "discoveryengine.googleapis.com", "compute.googleapis.com", 1)},
		{name: "per-client quota subject", details: strings.Replace(violation, "project:12345", "clientip:192.0.2.50", 1)},
		{name: "foreign project", details: strings.Replace(detail, "12345", "99999", 1)},
		{name: "foreign region", details: strings.Replace(detail, `"us"`, `"in"`, 1)},
		{name: "foreign edition", details: strings.Replace(detail, "enterprise_plus", "enterprise_standard", 1)},
		{name: "per-user", details: strings.Replace(detail, "OpaqueTextLimit", "TextAnswersPerDayPerUser", 1)},
		{name: "minute limit", details: strings.Replace(detail, "OpaqueTextLimit", "TextAnswersPerMinute", 1)},
		{name: "technical feature", details: strings.Replace(detail, "text_answer_gen_tier_enterprise_plus_regional", "requests", 1)},
		{name: "confirmed daily limit", details: strings.Replace(detail, "OpaqueTextLimit", "TextAnswersPerDay", 1)},
		{name: "unrecognized detail type", details: `{"@type":"type.googleapis.com/example.Limit"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[` + test.details + `]}}]`
			for _, status := range []int{http.StatusTooManyRequests, http.StatusOK} {
				got := ParseGenerationError([]byte(body), status, nil, binding, identity, now, false)
				if got.QuotaPressureHint != test.wantHint || got.UnscopedRateLimit || got.HTTPStatus != http.StatusTooManyRequests {
					t.Fatalf("HTTP %d: incompatible pressure classification: %+v", status, got)
				}
			}
		})
	}
}

func TestEnterpriseGenerationErrorWrappersPreserveQuotaScope(t *testing.T) {
	now := time.Date(2026, 11, 19, 4, 20, 0, 0, time.UTC)
	binding := config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "eu"}
	identity := &QuotaIdentity{Verified: true, Paid: true, Binding: Binding(binding), ProjectNumber: "12345", Location: "eu", Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: now}
	const metric = "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional"
	for _, test := range []struct {
		name, scope string
		body        []byte
		pressure    bool
	}{
		{name: "generic429", body: []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`), pressure: true},
		{name: "daily", body: restrictionFixture(metric, "TextAnswersPerDay", "12345", "eu"), scope: "edition"},
		{name: "user", body: restrictionFixture(metric, "TextAnswersPerDayPerUser", "12345", "eu"), scope: "user"},
		{name: "technical", body: restrictionFixture("discoveryengine.googleapis.com/requests", "RequestsPerMinute", "12345", "eu"), scope: "project"},
		{name: "foreign_project", body: restrictionFixture(metric, "TextAnswersPerDay", "99999", "eu")},
		{name: "foreign_region", body: restrictionFixture(metric, "TextAnswersPerDay", "12345", "us")},
		{name: "conflicting_status", body: []byte(`{"error":{"code":403,"status":"RESOURCE_EXHAUSTED"}}`)},
	} {
		for _, wrapper := range []string{"array", "nested_error", "nested_array"} {
			t.Run(test.name+"/"+wrapper, func(t *testing.T) {
				raw := "[" + string(test.body) + "]"
				if wrapper == "nested_error" {
					raw = `{"error":` + string(test.body) + "}"
				}
				if wrapper == "nested_array" {
					raw = `{"error":[` + string(test.body) + "]}"
				}
				got := ParseGenerationError([]byte(raw), 429, nil, binding, identity, now, false)
				if got.UnscopedRateLimit != test.pressure {
					t.Fatalf("wrapped quota pressure evidence = %v, want %v", got.UnscopedRateLimit, test.pressure)
				}
				scope := ""
				if got.Restriction != nil {
					scope = got.Restriction.Scope
				}
				if scope != test.scope {
					t.Fatalf("wrapped restriction scope = %q, want %q", scope, test.scope)
				}
				if (test.pressure || test.scope != "") && (got.UpstreamCode != 429 || got.RPCStatus != "RESOURCE_EXHAUSTED") {
					t.Fatalf("wrapped error lost upstream classification: %+v", got)
				}
			})
		}
	}
	for _, raw := range []string{
		"[]",
		`[{"error":{"code":429}},{"error":{"code":403}}]`,
		`[{"error":{"code":429}}`,
		`{"error":"quota exceeded"}`,
		`[[[[[[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}]]]]]]`,
	} {
		if got := ParseGenerationError([]byte(raw), 429, nil, binding, identity, now, false); got.UnscopedRateLimit || got.Restriction != nil {
			t.Fatalf("ambiguous wrapper contributed shared quota evidence: %s", raw)
		}
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
