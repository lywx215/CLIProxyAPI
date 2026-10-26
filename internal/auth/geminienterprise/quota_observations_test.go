package geminienterprise

import (
	"context"
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
	previous := QuotaSnapshot{ProjectID: "123", Location: "global", ObservedAt: now, PeriodStart: &start, Pools: []QuotaPool{{Tier: "enterprise_plus", Status: "ok", Limit: &limit, Used: &used, UsageUpdatedAt: &now}}}
	current := CloneQuotaSnapshot(previous)
	current.ObservedAt = now.Add(time.Minute)
	current.Pools[0].Used, current.Pools[0].Remaining, current.Pools[0].Status = nil, nil, "partial"
	got := MergeQuotaObservations(previous, current)
	if got.Pools[0].Used == nil || *got.Pools[0].Used != 0 || got.Pools[0].Remaining == nil || *got.Pools[0].Remaining != 1000 || got.Pools[0].Status != "stale" {
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
