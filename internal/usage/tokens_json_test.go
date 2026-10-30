package usage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestEnterpriseReasoningSnapshotProviderAndDedup(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{Provider: "gemini-enterprise", APIKey: "fixture", Model: "enterprise", RequestedAt: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), Detail: coreusage.Detail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 50, TotalTokens: 1150, TokenPresence: &coreusage.TokenPresence{InputTokens: true, OutputTokens: true, ReasoningTokens: true, TotalTokens: true}}})
	data, err := json.Marshal(stats.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	row := gjson.GetBytes(data, "apis.fixture.models.enterprise.details.0")
	if row.Get("provider").String() != "gemini-enterprise" || row.Get("tokens.reasoning_tokens").Int() != 50 || row.Get("tokens.output_tokens").Int() != 100 || row.Get("tokens.cached_tokens").Type != gjson.Null {
		t.Fatalf("metadata or buckets changed: %s", data)
	}
	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	imported := NewRequestStatistics()
	if result := imported.MergeSnapshot(snapshot); result.Added != 1 || imported.Snapshot().TotalTokens != 1150 {
		t.Fatal("snapshot totals changed")
	}
	api := snapshot.APIs["fixture"]
	model := api.Models["enterprise"]
	model.Details[0].Provider = ""
	api.Models["enterprise"] = model
	snapshot.APIs["fixture"] = api
	if result := imported.MergeSnapshot(snapshot); result.Added != 0 || result.Skipped != 1 {
		t.Fatal("optional provider changed the historical dedup key")
	}
	copyData, _ := json.Marshal(imported.Snapshot())
	if gjson.GetBytes(copyData, "apis.fixture.models.enterprise.details.0.provider").String() != "gemini-enterprise" {
		t.Fatal("snapshot lost provider")
	}
}

func TestTokenPresenceSnapshotRoundTrip(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{RequestedAt: time.Now(), APIKey: "fixture", Model: "enterprise", Detail: coreusage.Detail{InputTokens: 12, OutputTokens: 3, TotalTokens: 15, TokenPresence: &coreusage.TokenPresence{InputTokens: true, OutputTokens: true, TotalTokens: true}}})
	data, err := json.Marshal(stats.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	tokens := gjson.GetBytes(data, "apis.fixture.models.enterprise.details.0.tokens")
	if tokens.Get("reasoning_tokens").Type != gjson.Null || tokens.Get("cached_tokens").Type != gjson.Null || tokens.Get("input_tokens").Int() != 12 || tokens.Get("total_tokens").Int() != 15 {
		t.Fatalf("wrong tokens: %s", tokens.Raw)
	}
	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	imported := NewRequestStatistics()
	imported.MergeSnapshot(snapshot)
	copyData, _ := json.Marshal(imported.Snapshot())
	if !strings.Contains(string(copyData), `"reasoning_tokens":null`) || imported.Snapshot().TotalTokens != 15 {
		t.Fatalf("lost unknown fields: %s", copyData)
	}
	if result := imported.MergeSnapshot(snapshot); result.Added != 0 {
		t.Fatalf("duplicate import: %+v", result)
	}
}

func TestTokenPresencePreservesLegacyZero(t *testing.T) {
	data, _ := json.Marshal(TokenStats{})
	if strings.Contains(string(data), "null") {
		t.Fatalf("legacy zero changed: %s", data)
	}
	var tokens TokenStats
	if err := json.Unmarshal(data, &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens.TokenPresence != nil {
		t.Fatal("legacy counters changed")
	}
}

func TestTokenPresenceUnknownTotalAndExplicitZeroAreNotRecomputed(t *testing.T) {
	for _, totalKnown := range []bool{false, true} {
		stats := NewRequestStatistics()
		stats.Record(context.Background(), coreusage.Record{APIKey: "fixture", Model: "enterprise", Detail: coreusage.Detail{
			InputTokens: 12, OutputTokens: 3,
			TokenPresence: &coreusage.TokenPresence{InputTokens: true, OutputTokens: true, TotalTokens: totalKnown},
		}})
		snapshot := stats.Snapshot()
		if snapshot.TotalTokens != 0 || snapshot.APIs["fixture"].TotalTokens != 0 {
			t.Fatalf("unavailable or explicit zero total was replaced: %+v", snapshot)
		}
		encoded, _ := json.Marshal(snapshot)
		total := gjson.GetBytes(encoded, "apis.fixture.models.enterprise.details.0.tokens.total_tokens")
		if totalKnown && (total.Type != gjson.Number || total.Int() != 0) || !totalKnown && total.Type != gjson.Null {
			t.Fatalf("total availability changed: %s", encoded)
		}
	}
}

func TestTokenPresenceRecordAndSnapshotDoNotAlias(t *testing.T) {
	presence := &coreusage.TokenPresence{InputTokens: true, TotalTokens: true}
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{APIKey: "fixture", Model: "enterprise", Detail: coreusage.Detail{
		InputTokens: 12, TotalTokens: 12, TokenPresence: presence,
	}})
	presence.InputTokens = false
	first := stats.Snapshot()
	first.APIs["fixture"].Models["enterprise"].Details[0].Tokens.TokenPresence.InputTokens = false
	second := stats.Snapshot()
	if !second.APIs["fixture"].Models["enterprise"].Details[0].Tokens.TokenPresence.InputTokens {
		t.Fatal("caller changed retained availability")
	}
}

func TestTokenPresenceDedupDistinguishesUnknownFromZero(t *testing.T) {
	stats := NewRequestStatistics()
	for _, raw := range []string{
		`{"input_tokens":0,"output_tokens":0,"reasoning_tokens":null,"cached_tokens":null,"total_tokens":0}`,
		`{"input_tokens":0,"output_tokens":0,"reasoning_tokens":0,"cached_tokens":0,"total_tokens":0}`,
	} {
		var tokens TokenStats
		if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
			t.Fatal(err)
		}
		snapshot := StatisticsSnapshot{APIs: map[string]APISnapshot{"fixture": {Models: map[string]ModelSnapshot{
			"enterprise": {Details: []RequestDetail{{Timestamp: time.Unix(1, 0), Tokens: tokens}}},
		}}}}
		if result := stats.MergeSnapshot(snapshot); result.Added != 1 {
			t.Fatalf("distinct availability lost: %+v", result)
		}
		if result := stats.MergeSnapshot(snapshot); result.Skipped != 1 {
			t.Fatalf("duplicate was not skipped: %+v", result)
		}
	}
}
