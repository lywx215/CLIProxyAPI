package usage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func fixtureObservation() *coreusage.Observation {
	return &coreusage.Observation{Version: 1, RequestModel: "m", ModelMatch: "not_reported", ResponseModelSequence: []string{}, CandidateFinishes: []coreusage.CandidateFinish{}, UpstreamResult: "terminal_observed", CompletionBasis: "candidate_finish", TailVerification: "unverified", DeliveryResult: "written", UsageResult: "not_reported"}
}

func TestObservationSnapshotOwnershipAndImportIdentity(t *testing.T) {
	stats := NewRequestStatistics()
	o := fixtureObservation()
	r := coreusage.Record{RequestID: "one", TraceID: "parent", APIKey: "fixture", Model: "m", RequestedAt: time.Unix(1, 0), Observation: o, Detail: coreusage.Detail{TotalTokens: 12}}
	stats.Record(context.Background(), r)
	o.DeliveryResult = "write_failed"
	snap := stats.Snapshot()
	detail := snap.APIs["fixture"].Models["m"].Details[0]
	if detail.Observation.DeliveryResult != "written" || detail.ExecutionID != "one" {
		t.Fatal("record was aliased or lost identity")
	}
	detail.Observation.DeliveryResult = "cancelled"
	if stats.Snapshot().APIs["fixture"].Models["m"].Details[0].Observation.DeliveryResult != "written" {
		t.Fatal("snapshot aliases storage")
	}
	clean := stats.Snapshot()
	second := NewRequestStatistics()
	if x := second.MergeSnapshot(clean); x.Added != 1 {
		t.Fatal(x)
	}
	if x := second.MergeSnapshot(clean); x.Added != 0 || x.Skipped != 1 {
		t.Fatal(x)
	}
	other := clean.APIs["fixture"].Models["m"].Details[0]
	other.ExecutionID = "two"
	api := clean.APIs["fixture"]
	model := api.Models["m"]
	model.Details = []RequestDetail{other}
	api.Models["m"] = model
	clean.APIs["fixture"] = api
	if x := second.MergeSnapshot(clean); x.Added != 1 || second.Snapshot().TotalTokens != 24 {
		t.Fatal("distinct execution with equal metrics was merged")
	}
}

func TestObservationCanEnrichLegacyWithoutRecounting(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{APIKey: "fixture", Model: "m", RequestedAt: time.Unix(1, 0), Detail: coreusage.Detail{TotalTokens: 12}})
	snap := stats.Snapshot()
	api := snap.APIs["fixture"]
	model := api.Models["m"]
	model.Details[0].ExecutionID = "new-id"
	model.Details[0].Observation = fixtureObservation()
	api.Models["m"] = model
	snap.APIs["fixture"] = api
	x := stats.MergeSnapshot(snap)
	if x.ObservationsMerged != 1 || x.Added != 0 || stats.Snapshot().TotalTokens != 12 {
		t.Fatalf("merge = %+v", x)
	}
	model.Details[0].Observation.DeliveryResult = "cancelled"
	if x = stats.MergeSnapshot(snap); x.ObservationConflicts != 1 || stats.Snapshot().TotalRequests != 1 {
		t.Fatalf("frozen observation overwritten: %+v", x)
	}
	data, e := json.Marshal(stats.Snapshot())
	if e != nil {
		t.Fatal(e)
	}
	var roundtrip StatisticsSnapshot
	if e = json.Unmarshal(data, &roundtrip); e != nil {
		t.Fatal(e)
	}
	if roundtrip.APIs["fixture"].Models["m"].Details[0].Observation == nil {
		t.Fatal("observation lost on export")
	}
}

func TestObservationImportRejectsConflictingOutcomeWithoutDroppingAccounting(t *testing.T) {
	stats := NewRequestStatistics()
	snapshot := StatisticsSnapshot{APIs: map[string]APISnapshot{"fixture": {Models: map[string]ModelSnapshot{"m": {Details: []RequestDetail{{Timestamp: time.Unix(1, 0), Failed: true, Observation: fixtureObservation(), Tokens: TokenStats{TotalTokens: 12}}}}}}}}
	result := stats.MergeSnapshot(snapshot)
	if result.Added != 1 || result.ObservationConflicts != 1 {
		t.Fatalf("merge: %+v", result)
	}
	got := stats.Snapshot().APIs["fixture"].Models["m"].Details[0]
	if !got.Failed || got.Observation != nil || stats.Snapshot().TotalTokens != 12 {
		t.Fatal("invalid optional observation changed accounting")
	}
}
