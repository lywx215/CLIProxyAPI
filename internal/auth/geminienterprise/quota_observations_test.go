package geminienterprise

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestQuotaPoolSelectionDoesNotGuessEdition(t *testing.T) {
	binding := (config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "123", EngineID: "engine"}).WithDefaults()
	identity := &QuotaIdentity{Verified: true, Binding: Binding(binding), ProjectNumber: "123", Location: "global", Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64)}
	for _, tc := range []struct {
		name     string
		identity *QuotaIdentity
		pools    []QuotaPool
		want     string
	}{
		{"unknown-single", nil, []QuotaPool{{Tier: "enterprise_plus"}}, "enterprise_plus"},
		{"unknown-multiple", nil, []QuotaPool{{Tier: "enterprise_plus"}, {Tier: "enterprise_standard"}}, ""},
		{"assigned", identity, []QuotaPool{{Tier: "enterprise_standard"}, {Tier: "enterprise_plus"}}, "enterprise_plus"},
		{"assigned-pool-missing", identity, []QuotaPool{{Tier: "enterprise_standard"}}, ""},
		{"revoked", &QuotaIdentity{Invalidated: true}, []QuotaPool{{Tier: "enterprise_plus"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectQuotaPool(QuotaSnapshot{Pools: tc.pools}, tc.identity, binding)
			if (got == nil) != (tc.want == "") || (got != nil && got.Tier != tc.want) {
				t.Fatalf("selected wrong pool: %+v", got)
			}
		})
	}
}

func TestQuotaKeepsIndependentReadingsOnPermissionFailure(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, failed := range []string{"limit", "usage"} {
		t.Run(failed, func(t *testing.T) {
			client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/licenseConfigs") {
					return quotaResponse(200, map[string]any{"licenseConfigs": []any{map[string]string{"subscriptionTier": "SEARCH_AND_ASSISTANT", "state": "ACTIVE"}}}), nil
				}
				filter := r.URL.Query().Get("filter")
				if strings.Contains(filter, "/"+failed) {
					return quotaResponse(403, map[string]any{}), nil
				}
				suffix, kind, value := "usage", "DELTA", "0"
				if failed == "usage" {
					suffix, kind, value = "limit", "GAUGE", "1000"
				}
				return quotaResponse(200, map[string]any{"timeSeries": []quotaSeries{quotaFixture(suffix, kind, value, now.Add(-time.Minute), now)}}), nil
			})}
			got := FetchQuota(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "123", EngineID: "engine"}, "synthetic", now)
			if len(got.Pools) != 1 {
				t.Fatalf("missing pool: %+v", got)
			}
			pool := got.Pools[0]
			if pool.Status != "partial" || pool.Remaining != nil || (failed == "limit" && (pool.Used == nil || *pool.Used != 0 || pool.Limit != nil)) || (failed == "usage" && (pool.Limit == nil || *pool.Limit != 1000 || pool.Used != nil)) {
				t.Fatalf("independent value lost: %+v", pool)
			}
		})
	}
}

func TestQuotaConflictAndVersionIsolationAreOrderIndependent(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	limit := quotaFixture("limit", "GAUGE", "1000", now, now)
	conflict := quotaFixture("limit", "GAUGE", "2000", now, now)
	usage := quotaFixture("usage", "DELTA", "3", now.Add(-time.Minute), now)
	metric := strings.TrimSuffix(limit.Metric.Type, "/limit")
	for _, gauges := range [][]quotaSeries{{limit, conflict}, {conflict, limit}} {
		l, u, _ := quotaAmounts(gauges, []quotaSeries{usage}, "global", metric, now.Add(-time.Hour), now)
		if l != nil || u != nil {
			t.Fatal("conflicting gauge became known")
		}
	}
	version := quotaFixture("usage", "DELTA", "7", now.Add(-time.Minute), now)
	version.Metric.Labels["limit_name"] += "V2"
	if _, u, _ := quotaAmounts(nil, []quotaSeries{usage, version}, "global", metric, now.Add(-time.Hour), now); u != nil {
		t.Fatal("versions were mixed without a valid limit")
	}
	duplicate := quotaFixture("usage", "DELTA", "4", now.Add(-time.Minute), now)
	if _, u, _ := quotaAmounts([]quotaSeries{limit}, []quotaSeries{usage, duplicate}, "global", metric, now.Add(-time.Hour), now); u != nil {
		t.Fatal("contradictory duplicate delta was accepted")
	}
	overlap := quotaFixture("usage", "DELTA", "9", now.Add(-2*time.Minute), now.Add(-30*time.Second))
	if _, u, _ := quotaAmounts([]quotaSeries{limit}, []quotaSeries{usage, overlap}, "global", metric, now.Add(-time.Hour), now); u != nil {
		t.Fatal("overlapping delta intervals were double counted")
	}
}

func TestQuotaObservationMergePreservesZeroAndDailyBoundary(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	start := now.Add(-5 * time.Hour)
	limit, used := int64(1000), int64(0)
	previous := QuotaSnapshot{Status: "ok", ProjectID: "123", Location: "global", ObservedAt: now, PeriodStart: &start, Pools: []QuotaPool{{Tier: "enterprise_plus", Status: "ok", Limit: &limit, Used: &used, UsageUpdatedAt: &now}}}
	current := CloneQuotaSnapshot(previous)
	current.Status = "partial"
	current.ObservedAt = now.Add(time.Minute)
	current.Pools[0].Used, current.Pools[0].Remaining, current.Pools[0].Status = nil, nil, "partial"
	got := MergeQuotaObservations(previous, current)
	if got.Status != "stale" || got.Pools[0].Used == nil || *got.Pools[0].Used != 0 || got.Pools[0].Remaining == nil || *got.Pools[0].Remaining != 1000 || got.Pools[0].Status != "stale" {
		t.Fatalf("zero was lost: %+v", got)
	}
	*got.Pools[0].Used = 99
	if *previous.Pools[0].Used != 0 {
		t.Fatal("merged readings alias old state")
	}
	nextDay := start.Add(24 * time.Hour)
	current.PeriodStart = &nextDay
	if got = MergeQuotaObservations(previous, current); got.Pools[0].Used != nil {
		t.Fatal("yesterday's usage leaked into new day")
	}
	current.PeriodStart = &start
	current.Pools[0].Status = "no_data"
	if got = MergeQuotaObservations(previous, current); got.Pools[0].Used != nil {
		t.Fatal("successful unknown/conflict reused old data")
	}
	previous.Pools[0].Version = "old-version"
	current.Pools[0].Version = "new-version"
	current.Pools[0].Status = "partial"
	newLimit := int64(500)
	current.Pools[0].Limit = &newLimit
	if got = MergeQuotaObservations(previous, current); got.Pools[0].Used != nil || got.Pools[0].Remaining != nil {
		t.Fatal("a new limit version reused old usage")
	}
}

func TestQuotaPartialPoolIsNotHiddenByFreshPool(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, failedTier := range []string{"enterprise_plus", "enterprise_standard"} {
		t.Run(failedTier, func(t *testing.T) {
			client := &http.Client{Transport: quotaTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/licenseConfigs") {
					return quotaResponse(200, map[string]any{"licenseConfigs": []any{
						map[string]string{"subscriptionTier": "SEARCH_AND_ASSISTANT", "state": "ACTIVE"},
						map[string]string{"subscriptionTier": "ENTERPRISE", "state": "ACTIVE"},
					}}), nil
				}
				filter := r.URL.Query().Get("filter")
				tier := "enterprise_plus"
				if strings.Contains(filter, "tier_enterprise_standard_regional") {
					tier = "enterprise_standard"
				}
				suffix, kind, value := "usage", "DELTA", "0"
				if strings.Contains(filter, "/limit") {
					suffix, kind, value = "limit", "GAUGE", "1000"
				} else if tier == failedTier {
					return quotaResponse(403, map[string]any{}), nil
				}
				series := quotaFixture(suffix, kind, value, now.Add(-time.Minute), now)
				series.Metric.Type = strings.ReplaceAll(series.Metric.Type, "enterprise_plus", tier)
				return quotaResponse(200, map[string]any{"timeSeries": []quotaSeries{series}}), nil
			})}
			got := FetchQuota(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "123", EngineID: "engine"}, "synthetic", now)
			if got.Status != "partial" || len(got.Pools) != 2 {
				t.Fatalf("partial pool was hidden: %+v", got)
			}
			for _, pool := range got.Pools {
				if pool.Tier == failedTier {
					if pool.Status != "partial" || pool.Used != nil || pool.Remaining != nil || pool.Limit == nil {
						t.Fatalf("partial fields changed: %+v", pool)
					}
				} else if pool.Status != "ok" || pool.Used == nil || *pool.Used != 0 || pool.Remaining == nil || *pool.Remaining != 1000 {
					t.Fatalf("fresh zero reading changed: %+v", pool)
				}
			}
		})
	}
}

func TestQuotaStaleSnapshotJSONKeepsZeroAndStatus(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	start := now.Add(-5 * time.Hour)
	limit, used, remaining := int64(1000), int64(0), int64(1000)
	previous := QuotaSnapshot{Status: "ok", ProjectID: "123", Location: "global", ObservedAt: now, PeriodStart: &start, Pools: []QuotaPool{{Tier: "enterprise_plus", Status: "ok", Limit: &limit, Used: &used, Remaining: &remaining, UsageUpdatedAt: &now, Version: "internal-only"}}}
	for _, status := range []string{"partial", "unavailable", "no_permission"} {
		t.Run(status, func(t *testing.T) {
			current := CloneQuotaSnapshot(previous)
			current.Status, current.ObservedAt = status, now.Add(time.Minute)
			if status == "partial" {
				current.Pools[0].Status, current.Pools[0].Used = status, nil
				current.Pools[0].Remaining = nil
			} else {
				current.Pools = nil
			}
			merged := MergeQuotaObservations(previous, current)
			data, err := json.Marshal(merged)
			if err != nil {
				t.Fatal(err)
			}
			var raw struct {
				Status string `json:"status"`
				Pools  []struct {
					Status    string `json:"status"`
					Used      *int64 `json:"used"`
					Remaining *int64 `json:"remaining"`
				} `json:"pools"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			if raw.Status != "stale" || len(raw.Pools) != 1 || raw.Pools[0].Status != "stale" || raw.Pools[0].Used == nil || *raw.Pools[0].Used != 0 || raw.Pools[0].Remaining == nil || *raw.Pools[0].Remaining != 1000 {
				t.Fatalf("stale JSON lost its observations: %s", data)
			}
			if strings.Contains(string(data), "internal-only") || strings.Contains(string(data), "selected_pool") {
				t.Fatalf("standalone quota leaked internal fields: %s", data)
			}
		})
	}
}
