package geminienterprise

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func restrictionFixture(metric, limit, project, location string) []byte {
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 429, "status": "RESOURCE_EXHAUSTED", "message": "secret daily quota assistToken=session-secret", "details": []any{
		map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "domain": "googleapis.com", "metadata": map[string]string{"service": "discoveryengine.googleapis.com", "consumer": "projects/" + project, "location": location, "quota_metric": metric, "quota_limit": limit, "HelpToken": "fixture-secret"}},
		map[string]any{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "violations": []any{map[string]any{"quotaMetric": metric, "quotaId": limit, "description": "sensitive"}}},
	}}})
	return data
}
func TestParseEnterpriseQuotaStructuredClassification(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	b := config.GeminiEnterpriseOAuth{ProjectID: "project-id", Location: "us"}
	i := &QuotaIdentity{Verified: true, Binding: Binding(b), ProjectNumber: "12345", Location: b.Location, Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64)}
	metric := "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional"
	for _, test := range []struct{ name, metric, limit, project, location, kind, scope string }{
		{"daily", metric, "TextAnswersPerDay", "12345", "us", "daily", "edition"},
		{"user", metric, "TextAnswersPerDayPerUser", "12345", "us", "daily", "user"},
		{"minute", metric, "TextAnswersPerMinute", "12345", "us", "minute", "edition"},
		{"technical", "discoveryengine.googleapis.com/requests", "RequestsPerMinute", "project-id", "us", "minute", "project"},
		{"foreign-project", metric, "TextAnswersPerDay", "99999", "us", "", ""},
		{"foreign-region", metric, "TextAnswersPerDay", "12345", "eu", "", ""},
		{"unknown-limit", metric, "UnknownLimit", "12345", "us", "", ""},
		{"generic-quota", "discoveryengine.googleapis.com/quota/daily", "DailyQuota", "12345", "us", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := ParseGenerationError(restrictionFixture(test.metric, test.limit, test.project, test.location), 429, nil, b, i, now, true)
			if test.kind == "" {
				if e.Restriction != nil {
					t.Fatal("ambiguous error propagated")
				}
				return
			}
			if e.Restriction == nil || e.Restriction.Kind != test.kind || e.Restriction.Scope != test.scope || !e.IsRequestScoped() {
				t.Fatalf("classification: %+v", e)
			}
			if strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "sensitive") {
				t.Fatal("error text leaked")
			}
			encoded, _ := json.Marshal(e.Restriction)
			if strings.Contains(string(encoded), "Token") || strings.Contains(string(encoded), "sensitive") {
				t.Fatal("persisted error leaked")
			}
		})
	}
	for _, status := range []int{401, 403, 500} {
		if e := ParseGenerationError(restrictionFixture(metric, "TextAnswersPerDay", "12345", "us"), status, nil, b, i, now, false); e.Restriction != nil {
			t.Fatalf("HTTP %d created quota", status)
		}
	}
	if e := ParseGenerationError([]byte(`{"error":{"code":429,"message":"daily quota exhausted"}}`), 429, nil, b, i, now, false); e.Restriction != nil {
		t.Fatal("description inferred a daily quota")
	}
}

func TestEnterpriseRetryHintsAndPacificDST(t *testing.T) {
	for _, date := range []struct{ now, want string }{
		{"2026-03-08T08:00:00Z", "2026-03-09T07:00:00Z"},
		{"2026-11-01T07:00:00Z", "2026-11-02T08:00:00Z"},
	} {
		n, _ := time.Parse(time.RFC3339, date.now)
		if got := NextPacificMidnight(n).Format(time.RFC3339); got != date.want {
			t.Fatalf("DST: %s != %s", got, date.want)
		}
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	metric := "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional"
	raw := restrictionFixture(metric, "TextAnswersPerDay", "12345", "us")
	e := ParseGenerationError(raw, 200, http.Header{"Retry-After": {"7200"}}, config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "us"}, nil, now, false)
	if e.HTTPStatus != 429 || e.Restriction == nil || !e.Restriction.RetryAt.Equal(now.Add(2*time.Hour)) || e.Restriction.TimeSource != "google" {
		t.Fatalf("retry hint lost: %+v", e)
	}
}
