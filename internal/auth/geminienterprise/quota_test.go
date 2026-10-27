package geminienterprise

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type quotaTransport func(*http.Request) (*http.Response, error)

func (f quotaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func quotaResponse(status int, body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}
}

func quotaFixture(suffix, kind, value string, start, end time.Time) quotaSeries {
	var s quotaSeries
	s.Metric.Type = "discoveryengine.googleapis.com/quota/text_answer_gen_tier_enterprise_plus_regional/" + suffix
	s.Metric.Labels = map[string]string{"regional_location": "global", "limit_name": "TextAnswerGenTierEntPlusPerDayPerProjectRegional", "method": "Assist"}
	s.Resource.Type = "discoveryengine.googleapis.com/Location"
	// Numeric project bindings are intentionally not compared to this canonical ID.
	s.Resource.Labels = map[string]string{"project_id": "fixture-project", "location": "global"}
	s.Kind = kind
	data, _ := json.Marshal([]any{map[string]any{"interval": map[string]any{"startTime": start, "endTime": end}, "value": map[string]string{"int64Value": value}}})
	_ = json.Unmarshal(data, &s.Points)
	return s
}

func TestEnterpriseRegionalQuotaEndpointsAndUsageIsolation(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		region, limit, used string
		remaining           int64
	}{
		{"global", "10000", "100", 9900},
		{"us", "20000", "200", 19800},
		{"eu", "30000", "300", 29700},
		{"ca", "40000", "400", 39600},
		{"au", "50000", "500", 49500},
		{"in", "60000", "600", 59400},
	} {
		t.Run(tc.region, func(t *testing.T) {
			region := tc.region
			binding := config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", Location: region, EngineID: "app"}
			calls := 0
			client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Fatal("quota lost credential")
				}
				if strings.HasSuffix(r.URL.Path, "/licenseConfigs") {
					if !OfficialBinding(r, "123", region) || "https://"+r.URL.Host != OriginForLocation(region) {
						t.Fatal("license request used the wrong region", r.URL)
					}
					return quotaResponse(200, map[string]any{"licenseConfigs": []any{map[string]string{"subscriptionTier": "SEARCH_AND_ASSISTANT", "state": "ACTIVE"}}}), nil
				}
				filter := r.URL.Query().Get("filter")
				if r.URL.Host != "monitoring.googleapis.com" || !strings.Contains(filter, `project = "123"`) || !strings.Contains(filter, `metric.labels.regional_location = "`+region+`"`) || strings.Contains(filter, "resource.labels.location") {
					t.Fatal("Monitoring filter used the wrong region", r.URL)
				}
				suffix, kind, value := "usage", "DELTA", tc.used
				if strings.Contains(filter, "/limit") {
					suffix, kind, value = "limit", "GAUGE", tc.limit
				}
				known := quotaFixture(suffix, kind, value, now.Add(-time.Minute), now)
				// Regional quota labels remain authoritative on a global monitored resource.
				known.Metric.Labels["regional_location"] = region
				series := []quotaSeries{known}
				for _, otherRegion := range SupportedLocations() {
					if otherRegion == region {
						continue
					}
					other := quotaFixture(suffix, kind, "999", now.Add(-time.Minute), now)
					other.Metric.Labels["regional_location"] = otherRegion
					series = append(series, other)
				}
				return quotaResponse(200, map[string]any{"timeSeries": series}), nil
			})}
			result := FetchQuota(context.Background(), client, binding, "synthetic", now)
			if result.Status != "ok" || result.Location != region || len(result.Pools) != 1 || result.Pools[0].Remaining == nil || *result.Pools[0].Remaining != tc.remaining || calls != 3 {
				t.Fatalf("incorrect regional quota: %+v calls=%d", result, calls)
			}
		})
	}
}

func TestEnterpriseQuotaSumsPagedDeltaAndSeparatesRegions(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	first := quotaFixture("usage", "DELTA", "3", now.Add(-3*time.Minute), now.Add(-2*time.Minute))
	second := quotaFixture("usage", "DELTA", "2", now.Add(-2*time.Minute), now.Add(-time.Minute))
	otherMethod := quotaFixture("usage", "DELTA", "1", now.Add(-2*time.Minute), now.Add(-time.Minute))
	otherMethod.Metric.Labels["method"] = "StreamAssist"
	otherRegion := quotaFixture("usage", "DELTA", "999", now.Add(-2*time.Minute), now.Add(-time.Minute))
	otherRegion.Metric.Labels["regional_location"] = "us"
	requests := 0
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("X-Goog-User-Project") != "123456789" || r.URL.Scheme != "https" {
			t.Fatal("quota request lost its credential or project boundary")
		}
		if r.URL.Host == "discoveryengine.googleapis.com" {
			return quotaResponse(200, map[string]any{"licenseConfigs": []any{
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_SEARCH_AND_ASSISTANT", "state": "ACTIVE"},
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_SEARCH_AND_ASSISTANT", "state": "ACTIVE"},
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_ENTERPRISE", "state": "EXPIRED"},
			}}), nil
		}
		filter := r.URL.Query().Get("filter")
		if r.URL.Host != "monitoring.googleapis.com" || !strings.Contains(filter, `project = "123456789"`) || !strings.Contains(filter, `metric.labels.regional_location = "global"`) {
			t.Fatal("quota request did not constrain its monitored project and region")
		}
		if strings.Contains(filter, "/limit") {
			limit := quotaFixture("limit", "GAUGE", "10000", now.Add(-time.Hour), now.Add(-time.Hour))
			return quotaResponse(200, map[string]any{"timeSeries": []quotaSeries{limit, limit}}), nil
		}
		if r.URL.Query().Get("interval.startTime") != "2026-10-23T07:00:00Z" {
			t.Fatal("usage did not start at Pacific midnight")
		}
		if r.URL.Query().Get("pageToken") == "next-page" {
			return quotaResponse(200, map[string]any{"timeSeries": []quotaSeries{first, second, otherMethod, otherRegion}}), nil
		}
		return quotaResponse(200, map[string]any{"timeSeries": []quotaSeries{first}, "nextPageToken": "next-page"}), nil
	})}
	binding := config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "123456789", EngineID: "fixture"}
	result := FetchQuota(context.Background(), client, binding, "synthetic-token", now)
	if result.Status != "ok" || len(result.Pools) != 1 || requests != 4 {
		t.Fatalf("unexpected quota observation: %+v requests=%d", result, requests)
	}
	pool := result.Pools[0]
	if pool.Limit == nil || *pool.Limit != 10000 || pool.Used == nil || *pool.Used != 6 || pool.Remaining == nil || *pool.Remaining != 9994 || pool.UsageUpdatedAt == nil || !pool.UsageUpdatedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("incorrect pool totals: %+v", pool)
	}
}

func TestEnterpriseQuotaRequiresMatchingRegionalLabel(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	for _, region := range SupportedLocations() {
		t.Run(region, func(t *testing.T) {
			for _, label := range []string{"", "other-region"} {
				limitSeries := quotaFixture("limit", "GAUGE", "10000", now, now)
				usageSeries := quotaFixture("usage", "DELTA", "999", now.Add(-time.Minute), now)
				for _, series := range []*quotaSeries{&limitSeries, &usageSeries} {
					// A matching resource location cannot substitute for a missing or foreign quota label.
					series.Resource.Labels["location"] = region
					if label == "" {
						delete(series.Metric.Labels, "regional_location")
					} else {
						series.Metric.Labels["regional_location"] = label
					}
				}
				metric := strings.TrimSuffix(limitSeries.Metric.Type, "/limit")
				limit, used, updated := quotaAmounts([]quotaSeries{limitSeries}, []quotaSeries{usageSeries}, region, metric, start, now)
				if limit != nil || used != nil || updated != nil {
					t.Fatalf("missing or foreign quota region became known: region=%q label=%q", region, label)
				}
			}
		})
	}
}

func TestEnterpriseQuotaEmptyForbiddenAndInvalidPagesRemainSoft(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		status     int
		body       any
	}{
		{"empty", "no_data", 200, map[string]any{}},
		{"permission", "no_permission", 403, map[string]any{"error": map[string]any{"message": "synthetic-secret-do-not-echo"}}},
		{"api-disabled", "unavailable", 403, map[string]any{"error": map[string]any{"details": []any{map[string]string{"reason": "SERVICE_DISABLED"}}}}},
		{"redirect", "unavailable", 302, map[string]any{}},
		{"malformed", "unavailable", 200, "not a JSON object"},
		{"repeated-page", "unavailable", 200, map[string]any{"nextPageToken": "repeated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: quotaTransport(func(*http.Request) (*http.Response, error) { return quotaResponse(tc.status, tc.body), nil })}
			result := FetchQuota(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "fixture", EngineID: "fixture"}, "synthetic-token", time.Now())
			data, _ := json.Marshal(result)
			if result.Status != tc.want || strings.Contains(string(data), "synthetic-secret") || strings.Contains(string(data), "synthetic-token") || len(result.Pools) != 0 {
				t.Fatalf("incorrect soft state: %s", data)
			}
		})
	}
}

func TestEnterpriseQuotaUnknownUsageIsNotZeroAndRejectsPreviousDay(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	start := now.Add(-3 * time.Hour)
	limitSeries := quotaFixture("limit", "GAUGE", "10000", now.Add(-time.Hour), now.Add(-time.Hour))
	metric := strings.TrimSuffix(limitSeries.Metric.Type, "/limit")
	for _, usage := range [][]quotaSeries{
		nil,
		{quotaFixture("usage", "DELTA", "10", start.Add(-time.Minute), start.Add(time.Second))},
		{quotaFixture("usage", "DELTA", "not-a-number", now.Add(-time.Minute), now)},
		{quotaFixture("usage", "GAUGE", "10", now.Add(-time.Minute), now)},
	} {
		limit, used, updated := quotaAmounts([]quotaSeries{limitSeries}, usage, "global", metric, start, now)
		if limit == nil || *limit != 10000 || used != nil || updated != nil {
			t.Fatalf("unknown usage became a numeric balance: %v %v %v", limit, used, updated)
		}
	}
	_, used, _ := quotaAmounts(nil, []quotaSeries{quotaFixture("usage", "DELTA", "0", now.Add(-time.Minute), now)}, "global", metric, start, now)
	if used == nil || *used != 0 {
		t.Fatal("an explicitly reported zero must remain zero")
	}
}

func TestEnterpriseQuotaPacificResetHandlesDST(t *testing.T) {
	client := &http.Client{Transport: quotaTransport(func(*http.Request) (*http.Response, error) { return quotaResponse(200, map[string]any{}), nil })}
	for _, tc := range []struct{ now, start, reset string }{
		{"2026-03-08T12:00:00Z", "2026-03-08T08:00:00Z", "2026-03-09T07:00:00Z"},
		{"2026-11-01T12:00:00Z", "2026-11-01T07:00:00Z", "2026-11-02T08:00:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		result := FetchQuota(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "fixture", EngineID: "fixture"}, "synthetic", now)
		if result.PeriodStart.Format(time.RFC3339) != tc.start || result.ResetAt.Format(time.RFC3339) != tc.reset {
			t.Fatalf("incorrect DST boundary: %v %v", result.PeriodStart, result.ResetAt)
		}
	}
}

func TestEnterpriseQuotaTierPagesIncludeGracePeriodAndKeepEditionsSeparate(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Query().Get("pageToken") == "second" {
			return quotaResponse(200, map[string]any{"licenseConfigs": []any{
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_ENTERPRISE", "state": "DEACTIVATING"},
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_SEARCH_AND_ASSISTANT", "state": "ACTIVE"},
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_FRONTLINE_WORKER", "state": "NOT_STARTED"},
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_EDU", "state": "EXPIRED"},
				map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_CONSUMPTION_ONLY", "state": "ACTIVE"},
			}}), nil
		}
		return quotaResponse(200, map[string]any{"licenseConfigs": []any{
			map[string]string{"subscriptionTier": "SUBSCRIPTION_TIER_SEARCH_AND_ASSISTANT", "state": "ACTIVE"},
		}, "nextPageToken": "second"}), nil
	})}
	tiers, status := quotaTiers(context.Background(), client, config.GeminiEnterpriseOAuth{ProjectID: "fixture", Location: "global"}, "synthetic")
	if status != "ok" || strings.Join(tiers, ",") != "enterprise_plus,enterprise_standard" || requests != 2 {
		t.Fatalf("invalid edition pools: %v %s requests=%d", tiers, status, requests)
	}
}

func TestEnterpriseQuotaRejectsIncompleteMonitoringPages(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	series := quotaFixture("usage", "DELTA", "3", now.Add(-time.Minute), now)
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"execution-error", map[string]any{"timeSeries": []quotaSeries{series}, "executionErrors": []any{map[string]any{"code": 13, "message": "never-expose-partial-error"}}}},
		{"unreachable-region", map[string]any{"timeSeries": []quotaSeries{series}, "unreachable": []string{"global"}}},
		{"pagination-cycle", map[string]any{"timeSeries": []quotaSeries{series}, "nextPageToken": "cycle"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					return quotaResponse(200, map[string]any{"timeSeries": []quotaSeries{series}, "nextPageToken": "next"}), nil
				}
				return quotaResponse(200, tc.body), nil
			})}
			got, status := quotaTimeSeries(context.Background(), client, config.GeminiEnterpriseOAuth{ProjectID: "fixture", Location: "global"}, "synthetic", series.Metric.Type, now.Add(-time.Hour), now)
			if status != "unavailable" || got != nil || requests > 3 {
				t.Fatalf("partial usage accepted: status=%s series=%v requests=%d", status, got, requests)
			}
		})
	}
}

func TestEnterpriseQuotaOverflowDoesNotInventZero(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	first := quotaFixture("usage", "DELTA", "9223372036854775807", now.Add(-2*time.Minute), now.Add(-time.Minute))
	second := quotaFixture("usage", "DELTA", "1", now.Add(-time.Minute), now)
	_, used, updated := quotaAmounts(nil, []quotaSeries{first, second}, "global", strings.TrimSuffix(first.Metric.Type, "/usage"), now.Add(-time.Hour), now)
	if used != nil || updated != nil {
		t.Fatalf("overflow became an observed balance: %v %v", used, updated)
	}
}

func TestEnterpriseQuotaCancellationAndRedirectsDoNotLeakCredentials(t *testing.T) {
	binding := config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "fixture", EngineID: "fixture"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})}
	if result := FetchQuota(ctx, client, binding, "synthetic", time.Now()); result.Status != "unavailable" {
		t.Fatalf("cancelled quota read = %+v", result)
	}
	requests := 0
	client = &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "discoveryengine.googleapis.com" {
			t.Fatal("quota token followed a redirect")
		}
		response := quotaResponse(http.StatusFound, map[string]any{})
		response.Header.Set("Location", "https://untrusted.invalid/")
		return response, nil
	})}
	if result := FetchQuota(context.Background(), client, binding, "synthetic", time.Now()); result.Status != "unavailable" || requests != 1 {
		t.Fatalf("redirect was not rejected: status=%s requests=%d", result.Status, requests)
	}
}
