package geminienterprise

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Daily quota boundaries must also work on Windows without system zoneinfo.

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// QuotaSnapshot contains observations only, never credentials or upstream errors.
type QuotaSnapshot struct {
	Status      string      `json:"status"`
	ProjectID   string      `json:"project_id"`
	Location    string      `json:"location"`
	ObservedAt  time.Time   `json:"observed_at"`
	PeriodStart *time.Time  `json:"period_start"`
	ResetAt     *time.Time  `json:"reset_at"`
	Pools       []QuotaPool `json:"pools"`
}

type QuotaPool struct {
	Tier           string     `json:"tier"`
	Status         string     `json:"status"`
	Limit          *int64     `json:"limit"`
	Used           *int64     `json:"used"`
	Remaining      *int64     `json:"remaining"`
	UsageUpdatedAt *time.Time `json:"usage_updated_at"`
	Version        string     `json:"-"`
}

type quotaSeries struct {
	Metric struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	} `json:"metric"`
	Resource struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	Kind   string `json:"metricKind"`
	Points []struct {
		Interval struct {
			Start time.Time `json:"startTime"`
			End   time.Time `json:"endTime"`
		} `json:"interval"`
		Value struct {
			Int64 string `json:"int64Value"`
		} `json:"value"`
	} `json:"points"`
}

// FetchQuota reads the daily Assistant/Search pools for the saved resource binding.
// Monitoring DELTA usage is summed over the Pacific day, including every page and method.
func FetchQuota(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string, now time.Time) QuotaSnapshot {
	binding = binding.WithDefaults()
	result := QuotaSnapshot{Status: "unavailable", ProjectID: binding.ProjectID, Location: binding.Location, ObservedAt: now.UTC(), Pools: []QuotaPool{}}
	if binding.Validate() != nil || token == "" || client == nil {
		return result
	}
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return result
	}
	local := now.In(zone)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	periodStart, resetAt := start.UTC(), start.AddDate(0, 0, 1).UTC()
	result.PeriodStart, result.ResetAt = &periodStart, &resetAt
	tiers, status := quotaTiers(ctx, client, binding, token)
	if status != "ok" {
		result.Status = status
		return result
	}
	result.Status = "no_data"
	for _, tier := range tiers {
		metric := "discoveryengine.googleapis.com/quota/text_answer_gen_tier_" + tier + "_regional"
		pool := QuotaPool{Tier: tier, Status: "no_data"}
		limits, limitStatus := quotaTimeSeries(ctx, client, binding, token, metric+"/limit", now.Add(-24*time.Hour), now)
		usage, usageStatus := quotaTimeSeries(ctx, client, binding, token, metric+"/usage", start, now)
		pool.Limit, pool.Used, pool.UsageUpdatedAt, pool.Version = quotaAmountsWithVersion(limits, usage, binding.Location, metric, start, now)
		if limitStatus != "ok" || usageStatus != "ok" {
			pool.Status = "unavailable"
			if limitStatus == "no_permission" || usageStatus == "no_permission" {
				pool.Status = "no_permission"
			}
			if pool.Limit != nil || pool.Used != nil {
				pool.Status = "partial"
			}
		} else {
			if pool.Limit != nil || pool.Used != nil {
				pool.Status = "ok"
			}
		}
		if pool.Limit != nil && pool.Used != nil {
			remaining := max(int64(0), *pool.Limit-*pool.Used)
			pool.Remaining = &remaining
		}
		result.Pools = append(result.Pools, pool)
		if pool.Status == "ok" || (pool.Status == "partial" && result.Status != "ok") || result.Status == "no_data" {
			result.Status = pool.Status
		}
	}
	return result
}

func quotaTiers(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string) ([]string, string) {
	// These names correspond to the published SubscriptionTier enum and quota metrics.
	names := map[string]string{
		"SEARCH_AND_ASSISTANT": "enterprise_plus", "ENTERPRISE": "enterprise_standard",
		"ENTERPRISE_EMERGING": "enterprise_emerging", "FRONTLINE_WORKER": "frontline_worker",
		"FRONTLINE_STARTER": "frontline_starter", "AGENTSPACE_STARTER": "starter",
		"AGENTSPACE_BUSINESS": "business_standard", "EDU": "edu", "EDU_PRO": "edu_pro",
		"EDU_EMERGING": "edu_emerging", "EDU_PRO_EMERGING": "edu_pro_emerging",
	}
	found := map[string]bool{}
	params := url.Values{"pageSize": {"100"}}
	endpoint := OriginForLocation(binding.Location) + "/v1/projects/" + binding.ProjectID + "/locations/" + binding.Location + "/licenseConfigs"
	seen := map[string]bool{}
	for range 100 {
		var page struct {
			Configs []struct {
				Tier  string `json:"subscriptionTier"`
				State string `json:"state"`
			} `json:"licenseConfigs"`
			Next string `json:"nextPageToken"`
		}
		if status := quotaJSON(ctx, client, endpoint+"?"+params.Encode(), token, binding.ProjectID, &page); status != "ok" {
			return nil, status
		}
		for _, item := range page.Configs {
			// Deactivating subscriptions retain access until the grace period ends.
			if tier := names[strings.TrimPrefix(item.Tier, "SUBSCRIPTION_TIER_")]; (item.State == "ACTIVE" || item.State == "DEACTIVATING") && tier != "" {
				found[tier] = true
			}
		}
		if page.Next == "" {
			tiers := make([]string, 0, len(found))
			for tier := range found {
				tiers = append(tiers, tier)
			}
			sort.Strings(tiers)
			return tiers, "ok"
		}
		if seen[page.Next] {
			break
		}
		seen[page.Next] = true
		params.Set("pageToken", page.Next)
	}
	return nil, "unavailable"
}

func quotaTimeSeries(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token, metric string, start, end time.Time) ([]quotaSeries, string) {
	filter := fmt.Sprintf("metric.type = %q AND project = %q AND resource.type = %q AND metric.labels.regional_location = %q", metric, binding.ProjectID, "discoveryengine.googleapis.com/Location", binding.Location)
	params := url.Values{"filter": {filter}, "interval.startTime": {start.UTC().Format(time.RFC3339Nano)}, "interval.endTime": {end.UTC().Format(time.RFC3339Nano)}, "view": {"FULL"}, "pageSize": {"100000"}}
	endpoint := "https://monitoring.googleapis.com/v3/projects/" + binding.ProjectID + "/timeSeries"
	var series []quotaSeries
	seen := map[string]bool{}
	for range 100 {
		var page struct {
			Series      []quotaSeries     `json:"timeSeries"`
			Next        string            `json:"nextPageToken"`
			Errors      []json.RawMessage `json:"executionErrors"`
			Unreachable []string          `json:"unreachable"`
		}
		if status := quotaJSON(ctx, client, endpoint+"?"+params.Encode(), token, binding.ProjectID, &page); status != "ok" {
			return nil, status
		}
		if len(page.Errors) != 0 || len(page.Unreachable) != 0 {
			// Partial Monitoring results cannot establish a remaining balance.
			return nil, "unavailable"
		}
		series = append(series, page.Series...)
		if page.Next == "" {
			return series, "ok"
		}
		if seen[page.Next] {
			break
		}
		seen[page.Next] = true
		params.Set("pageToken", page.Next)
	}
	return nil, "unavailable"
}

func quotaAmounts(limits, usage []quotaSeries, location, metric string, start, end time.Time) (limit, used *int64, updated *time.Time) {
	limit, used, updated, _ = quotaAmountsWithVersion(limits, usage, location, metric, start, end)
	return
}

func quotaAmountsWithVersion(limits, usage []quotaSeries, location, metric string, start, end time.Time) (limit, used *int64, updated *time.Time, version string) {
	// Keep edition pools and regions separate; never sum duplicate limit gauges.
	latest := time.Time{}
	limitName := ""
	conflictingLimit := false
	validSeries := func(s quotaSeries, suffix, kind string) bool {
		// The monitored resource can be global for EU/US quota pools. Only the
		// metric's regional_location identifies the quota region.
		return s.Metric.Type == metric+suffix && s.Kind == kind && s.Resource.Type == "discoveryengine.googleapis.com/Location" && s.Metric.Labels["regional_location"] == location && strings.Contains(s.Metric.Labels["limit_name"], "PerDayPerProject")
	}
	for _, s := range limits {
		if !validSeries(s, "/limit", "GAUGE") {
			continue
		}
		for _, p := range s.Points {
			value, err := strconv.ParseInt(p.Value.Int64, 10, 64)
			if err != nil || value < 0 || p.Interval.End.After(end) || p.Interval.End.IsZero() {
				continue
			}
			if p.Interval.End.After(latest) {
				limit, latest, limitName = &value, p.Interval.End, s.Metric.Labels["limit_name"]
				conflictingLimit = false
			} else if p.Interval.End.Equal(latest) && (limit == nil || *limit != value || limitName != s.Metric.Labels["limit_name"]) {
				conflictingLimit = true
			}
		}
	}
	if conflictingLimit {
		return nil, nil, nil, ""
	}
	if limitName == "" {
		for _, s := range usage {
			if !validSeries(s, "/usage", "DELTA") {
				continue
			}
			for _, p := range s.Points {
				if p.Interval.Start.Before(start) || !p.Interval.End.After(start) || p.Interval.End.After(end) || p.Interval.End.Before(p.Interval.Start) {
					continue
				}
				name := s.Metric.Labels["limit_name"]
				if limitName != "" && limitName != name {
					return nil, nil, nil, ""
				}
				limitName = name
			}
		}
	}
	seen := map[string]string{}
	type interval struct{ start, end time.Time }
	intervals := map[string][]interval{}
	version = limitName
	for _, s := range usage {
		if !validSeries(s, "/usage", "DELTA") || (limitName != "" && limitName != s.Metric.Labels["limit_name"]) {
			continue
		}
		identity, _ := json.Marshal([]any{s.Metric, s.Resource})
		for _, p := range s.Points {
			if p.Interval.Start.Before(start) || !p.Interval.End.After(start) || p.Interval.End.After(end) || p.Interval.End.Before(p.Interval.Start) {
				continue
			}
			key := string(identity) + p.Interval.Start.String() + p.Interval.End.String()
			if previous, exists := seen[key]; exists {
				if previous != p.Value.Int64 {
					return limit, nil, nil, version
				}
				continue
			}
			seen[key] = p.Value.Int64
			intervals[string(identity)] = append(intervals[string(identity)], interval{p.Interval.Start, p.Interval.End})
			value, err := strconv.ParseInt(p.Value.Int64, 10, 64)
			if err != nil || value < 0 || (used != nil && value > (1<<63-1)-*used) {
				return limit, nil, nil, version
			}
			if used == nil {
				used = new(int64)
			}
			*used += value
			if updated == nil || p.Interval.End.After(*updated) {
				t := p.Interval.End
				updated = &t
			}
		}
	}
	for _, points := range intervals {
		sort.Slice(points, func(i, j int) bool {
			if points[i].start.Equal(points[j].start) {
				return points[i].end.Before(points[j].end)
			}
			return points[i].start.Before(points[j].start)
		})
		for i := 1; i < len(points); i++ {
			if points[i-1].end.After(points[i].start) {
				return limit, nil, nil, version
			}
		}
	}
	return
}

func quotaJSON(ctx context.Context, client *http.Client, endpoint, token, project string, target any) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Opaque != "" || req.URL.Fragment != "" || (!OfficialResource(req) && req.URL.Host != "monitoring.googleapis.com") {
		return "unavailable"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Goog-User-Project", project)
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := copyClient.Do(req)
	if err != nil {
		return "unavailable"
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil || len(data) > 8*1024*1024 {
		return "unavailable"
	}
	if response.StatusCode == http.StatusForbidden {
		// An API-disabled response also uses HTTP 403, but is not an IAM role failure.
		var body struct {
			Error struct {
				Details []struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &body)
		for _, detail := range body.Error.Details {
			if detail.Reason == "SERVICE_DISABLED" || detail.Reason == "BILLING_DISABLED" {
				return "unavailable"
			}
		}
		return "no_permission"
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(data, target) != nil {
		return "unavailable"
	}
	return "ok"
}
