package geminienterprise

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const restrictionTestMetric = "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional"

func restrictionTestContext() (config.GeminiEnterpriseOAuth, *QuotaIdentity, time.Time) {
	binding := config.GeminiEnterpriseOAuth{ProjectID: "project-id", Location: "us"}
	identity := &QuotaIdentity{Verified: true, Binding: Binding(binding), ProjectNumber: "12345", Location: binding.Location, Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64)}
	return binding, identity, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
}

func structuredRestrictionFixture(t *testing.T, metadata, dimensions map[string]string, extra ...any) []byte {
	t.Helper()
	fields := map[string]string{"service": "discoveryengine.googleapis.com", "consumer": "projects/12345", "location": "us", "quota_metric": restrictionTestMetric, "quota_limit": "TextAnswersPerDay"}
	for key, value := range metadata {
		fields[key] = value
	}
	details := []any{
		map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "domain": "googleapis.com", "metadata": fields},
		map[string]any{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "violations": []any{map[string]any{"quotaDimensions": dimensions}}},
	}
	details = append(details, extra...)
	data, err := json.Marshal(map[string]any{"error": map[string]any{"code": 429, "status": "RESOURCE_EXHAUSTED", "details": details}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEnterpriseQuotaRejectsConflictingStructuredFields(t *testing.T) {
	binding, identity, now := restrictionTestContext()
	for _, test := range []struct {
		name                 string
		metadata, dimensions map[string]string
		wantScope            string
	}{
		{"matching-project-aliases", map[string]string{"project": "project-id", "project_number": "12345"}, nil, "edition"},
		{"foreign-project-alias", map[string]string{"project": "other-project"}, nil, ""},
		{"foreign-project-number", map[string]string{"project_number": "99999"}, nil, ""},
		{"project-number-is-not-id", map[string]string{"project_number": "project-id"}, nil, ""},
		{"foreign-region-alias", map[string]string{"region": "eu"}, nil, ""},
		{"foreign-regional-location", map[string]string{"regional_location": "eu"}, nil, ""},
		{"foreign-service", map[string]string{"service": "aiplatform.googleapis.com"}, nil, ""},
		{"foreign-feature", map[string]string{"feature": "image_generation"}, nil, ""},
		{"conflicting-feature-alias", map[string]string{"feature": "assistant_search", "quota_feature": "technical_requests"}, nil, ""},
		{"foreign-edition", map[string]string{"edition": "enterprise_standard"}, nil, ""},
		{"known-subscription-enum", map[string]string{"subscription_tier": "SUBSCRIPTION_TIER_SEARCH_AND_ASSISTANT"}, nil, "edition"},
		{"unknown-subscription", map[string]string{"subscription_tier": "future-edition"}, nil, ""},
		{"project-scope-is-not-edition", map[string]string{"quota_scope": "project"}, nil, ""},
		{"explicit-shared-pool", map[string]string{"quota_scope": "shared_pool"}, nil, "edition"},
		{"unknown-scope", map[string]string{"quota_scope": "global"}, nil, ""},
		{"conflicting-unit-period", map[string]string{"quota_unit": "1/min/{project}"}, nil, ""},
		{"unknown-unit-dimension", map[string]string{"quota_unit": "1/d/{ip_address}"}, nil, ""},
		{"unit-user-scope", map[string]string{"quota_unit": "1/d/{project}/{user}"}, nil, "user"},
		{"foreign-dimension-project", nil, map[string]string{"project": "99999"}, ""},
		{"foreign-dimension-consumer", nil, map[string]string{"consumer": "projects/99999"}, ""},
		{"foreign-dimension-region", nil, map[string]string{"region": "eu"}, ""},
		{"foreign-dimension-edition", nil, map[string]string{"edition": "enterprise_standard"}, ""},
		{"foreign-dimension-feature", nil, map[string]string{"feature": "deep_research"}, ""},
		{"unknown-dimension", nil, map[string]string{"ip_address": "192.0.2.1"}, ""},
		{"empty-dimension", nil, map[string]string{"project": ""}, ""},
		{"arbitrary-user-is-credential-only", nil, map[string]string{"user": "other-user@example.test"}, "credential"},
		{"explicit-user-with-arbitrary-principal", map[string]string{"quota_scope": "user"}, map[string]string{"user": "other-user@example.test"}, "credential"},
		{"explicit-user-without-principal", map[string]string{"quota_scope": "user"}, nil, "user"},
		{"matching-dimensions", nil, map[string]string{"project": "project-id", "region": "us", "edition": "enterprise_plus", "feature": "assistant_search"}, "edition"},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := ParseGenerationError(structuredRestrictionFixture(t, test.metadata, test.dimensions), 429, nil, binding, identity, now, false)
			if test.wantScope == "" {
				if e.Restriction != nil {
					t.Fatalf("conflicting details created restriction: %+v", e.Restriction)
				}
			} else if e.Restriction == nil || e.Restriction.Scope != test.wantScope {
				t.Fatalf("scope: %+v, want %s", e.Restriction, test.wantScope)
			}
		})
	}
}

func TestEnterpriseQuotaValidatesAllDetailsRegardlessOfOrder(t *testing.T) {
	binding, identity, now := restrictionTestContext()
	for _, kind := range []string{"ErrorInfo", "QuotaFailure"} {
		for _, foreignFirst := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/foreign-first", false: "/foreign-last"}[foreignFirst], func(t *testing.T) {
				var foreign any
				if kind == "ErrorInfo" {
					foreign = map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "domain": "googleapis.com", "metadata": map[string]string{"project_number": "99999"}}
				} else {
					foreign = map[string]any{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "violations": []any{map[string]any{"quotaMetric": restrictionTestMetric, "quotaId": "TextAnswersPerDay", "quotaDimensions": map[string]string{"region": "eu"}}}}
				}
				raw := structuredRestrictionFixture(t, nil, nil, foreign)
				if foreignFirst {
					var body struct {
						Error map[string]any `json:"error"`
					}
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Fatal(err)
					}
					details := body.Error["details"].([]any)
					details[0], details[len(details)-1] = details[len(details)-1], details[0]
					raw, _ = json.Marshal(body)
				}
				if e := ParseGenerationError(raw, 429, nil, binding, identity, now, false); e.Restriction != nil {
					t.Fatalf("a later detail hid a foreign resource: %+v", e.Restriction)
				}
			})
		}
	}
	for _, change := range []map[string]string{{"quota_metric": "discoveryengine.googleapis.com/requests"}, {"quota_limit": "TextAnswersPerMinute"}} {
		extra := map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "domain": "googleapis.com", "metadata": change}
		if e := ParseGenerationError(structuredRestrictionFixture(t, nil, nil, extra), 429, nil, binding, identity, now, false); e.Restriction != nil {
			t.Fatal("conflicting metric or limit details were combined")
		}
	}
}

func TestEnterpriseQuotaDoesNotTrustStaleIdentityOrUnknownMetric(t *testing.T) {
	binding, identity, now := restrictionTestContext()
	for _, mode := range []string{"invalidated", "rebound", "unverified", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			stale := *identity
			switch mode {
			case "invalidated":
				stale.Invalidated = true
			case "rebound":
				stale.Binding = "other-binding"
			case "unverified":
				stale.Verified = false
			case "incomplete":
				stale.PrincipalHash = ""
			}
			if e := ParseGenerationError(structuredRestrictionFixture(t, nil, nil), 429, nil, binding, &stale, now, false); e.Restriction != nil {
				t.Fatal("an untrusted identity established the project number alias")
			}
		})
	}
	for _, metric := range []string{"discoveryengine.googleapis.com/image_generation", "discoveryengine.googleapis.com/arbitrary", "discoveryengine.googleapis.com/quota/text_answer_gen_tier_future_regional"} {
		raw := restrictionFixture(metric, "RequestsPerMinute", "12345", "us")
		if e := ParseGenerationError(raw, 429, nil, binding, identity, now, false); e.Restriction != nil {
			t.Fatalf("unrecognized metric %q created a shared gate", metric)
		}
	}
	for _, suffix := range []string{"/usage/limit", "/limit/limit", "/usage/usage"} {
		raw := restrictionFixture(restrictionTestMetric+suffix, "TextAnswersPerDay", "12345", "us")
		if e := ParseGenerationError(raw, 429, nil, binding, identity, now, false); e.Restriction != nil {
			t.Fatal("malformed metric suffix was normalized into a recognized pool")
		}
	}
}

func TestEnterpriseQuotaHTTP200AndContradictoryCodes(t *testing.T) {
	binding, identity, now := restrictionTestContext()
	for _, test := range []struct {
		body       string
		status     int
		wantStatus int
	}{
		{`{"error":{"code":8,"status":"RESOURCE_EXHAUSTED"}}`, 200, 429},
		{`{"code":7,"status":"PERMISSION_DENIED"}`, 200, 403},
		{`{"error":{"status":"UNAUTHENTICATED"}}`, 0, 401},
		{`{"error":{"code":403,"status":"RESOURCE_EXHAUSTED"}}`, 200, 502},
		{`{"error":{"code":8,"status":"PERMISSION_DENIED"}}`, 200, 502},
		{`{"error":{"code":403,"status":"PERMISSION_DENIED"}}`, 429, 429},
		{`{"error":{"message":"daily quota exhausted"}}`, 200, 502},
		{`{"error":null}`, 200, 502},
		{`not-json`, 200, 502},
	} {
		e := ParseGenerationError([]byte(test.body), test.status, nil, binding, identity, now, true)
		if e.HTTPStatus != test.wantStatus || e.Restriction != nil || !e.IsRequestScoped() {
			t.Fatalf("body %s: status %d, restriction %+v", test.body, e.HTTPStatus, e.Restriction)
		}
	}
	for _, status := range []int{200, 429} {
		raw := structuredRestrictionFixture(t, nil, nil)
		raw = []byte(strings.Replace(string(raw), `"code":429`, `"code":403`, 1))
		if e := ParseGenerationError(raw, status, nil, binding, identity, now, false); e.Restriction != nil {
			t.Fatal("matching quota details overrode a contradictory error code")
		}
	}
}

func TestEnterpriseQuotaRetryHintsNeverShortenDeadline(t *testing.T) {
	binding, identity, now := restrictionTestContext()
	resetLater, resetEarlier := now.Add(4*time.Hour), now.Add(time.Hour)
	extra := []any{
		map[string]any{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "7200.5s"},
		map[string]any{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "1s"},
		map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "domain": "googleapis.com", "metadata": map[string]string{"reset_time": resetEarlier.Format(time.RFC3339Nano)}},
	}
	raw := structuredRestrictionFixture(t, map[string]string{"reset_time": resetLater.Format(time.RFC3339Nano)}, nil, extra...)
	e := ParseGenerationError(raw, 429, http.Header{"Retry-After": {"60", "10800"}}, binding, identity, now, false)
	if e.Restriction == nil || !e.Restriction.ResetAt.Equal(resetLater) || !e.RetryAt.Equal(resetLater) || e.RetryAfter() == nil || *e.RetryAfter() != 4*time.Hour {
		t.Fatalf("known deadline shortened: %+v", e.Restriction)
	}
	raw = structuredRestrictionFixture(t, nil, nil, extra[0], extra[1])
	e = ParseGenerationError(raw, 429, http.Header{"Retry-After": {"60"}}, binding, identity, now, false)
	if e.Restriction == nil || !e.RetryAt.Equal(now.Add(7200*time.Second+500*time.Millisecond)) || e.Restriction.TimeSource != "google" {
		t.Fatalf("fractional RetryInfo delay was lost: %+v", e.Restriction)
	}
	e = ParseGenerationError(raw, 429, http.Header{"Retry-After": {now.Add(3 * time.Hour).Format(http.TimeFormat), "60"}}, binding, identity, now, false)
	if e.Restriction == nil || !e.RetryAt.Equal(now.Add(3*time.Hour)) {
		t.Fatal("an earlier delay replaced a later HTTP-date Retry-After")
	}
	for _, delay := range []string{"1h", "-1s", "9223372036854775808s", "0s", "1.0000000001s"} {
		raw = structuredRestrictionFixture(t, map[string]string{"reset_time": now.Add(-time.Hour).Format(time.RFC3339Nano)}, nil, map[string]any{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": delay})
		e = ParseGenerationError(raw, 429, http.Header{"Retry-After": {"9223372036854775807"}}, binding, identity, now, false)
		if e.Restriction == nil || !e.RetryAt.Equal(NextPacificMidnight(now)) || !e.Restriction.ResetAt.IsZero() || e.Restriction.TimeSource != "estimated_pacific_midnight" {
			t.Fatalf("invalid hint %q created a deadline: %+v", delay, e.Restriction)
		}
	}
	for _, raw = range [][]byte{[]byte(`{"error":{"message":"unknown quota"}}`), []byte("malformed"), []byte(strings.Repeat("x", 1024*1024+1))} {
		e = ParseGenerationError(raw, 429, http.Header{"Retry-After": {"10", "90"}}, binding, identity, now, false)
		if e.Restriction != nil || !e.RetryAt.Equal(now.Add(90*time.Second)) {
			t.Fatal("unknown 429 lost its transport retry hint or inferred a pool")
		}
	}
}
