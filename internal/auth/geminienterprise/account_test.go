package geminienterprise

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/oauth2"
)

func accountFixture(t *testing.T) (map[string]any, Account) {
	t.Helper()
	metadata := Metadata(config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", EngineID: "engine"}, "fixture@example.com", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic-refresh"})
	metadata["custom"] = map[string]any{"nested": []any{map[string]any{"value": "original"}}}
	metadata["prefix"] = "fixture"
	account, err := AccountFromLegacy(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return metadata, account
}

func TestEnterpriseAccountAndCollectionIdentity(t *testing.T) {
	if AccountID(" Fixture@Example.com ", " client ") != AccountID("fixture@example.com", "client") || AccountID("fixture@example.com", "client") == AccountID("fixture@example.com", "other") || AccountID("", "client") != "" {
		t.Fatal("invalid account identity normalization")
	}
	resources := ResourceBinding{ProjectID: "123", EngineID: "engine"}
	id := CollectionID(resources)
	resources.ProjectID, resources.EngineID, resources.AssistantID = "projects/00123", "different", "other"
	if CollectionID(resources) != id {
		t.Fatal("collection identity included app choices or a project spelling")
	}
	resources.Location = "eu"
	if CollectionID(resources) == id {
		t.Fatal("collection identity conflated regions")
	}
	resources.ProjectID = "../bad"
	if CollectionID(resources) != "" {
		t.Fatal("unsafe collection identity accepted")
	}
}

func TestEnterpriseAccountRoundtripAndNestedIsolation(t *testing.T) {
	metadata, account := accountFixture(t)
	account.Collections[0].Settings["nested"] = map[string]any{"items": []string{"old"}}
	account.Collections[0].LegacyAliases = []string{"legacy"}
	account.Discovery = map[string]any{"coverage": []any{map[string]any{"status": "ok"}}}
	account.MergedSources = []string{"old.json"}
	stored := PutAccount(metadata, account)
	clone, err := ReadAccount(stored)
	if err != nil || clone == nil {
		t.Fatalf("read account: %v", err)
	}
	clone.Collections[0].Settings["nested"].(map[string]any)["items"].([]any)[0] = "changed"
	clone.Collections[0].LegacyAliases[0] = "changed"
	clone.Discovery["coverage"].([]any)[0].(map[string]any)["status"] = "changed"
	clone.MergedSources[0] = "changed"
	stored["custom"].(map[string]any)["nested"].([]any)[0].(map[string]any)["value"] = "changed"
	if metadata["custom"].(map[string]any)["nested"].([]any)[0].(map[string]any)["value"] != "original" || account.Collections[0].Settings["nested"].(map[string]any)["items"].([]string)[0] != "old" || account.Collections[0].LegacyAliases[0] != "legacy" || account.MergedSources[0] != "old.json" {
		t.Fatal("mutable graph or unknown metadata was aliased")
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		t.Fatal("invalid account JSON")
	}
	read, err := ReadAccount(decoded)
	if err != nil || read.ID != account.ID || String(decoded, "refresh_token") != "synthetic-refresh" {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if _, exists := decoded["enterprise_account"].(map[string]any)["refresh_token"]; exists {
		t.Fatal("OAuth material duplicated into graph")
	}
}

func TestEnterpriseAccountRejectsCorruptGraphAndAdaptsLegacy(t *testing.T) {
	metadata, account := accountFixture(t)
	metadata["disabled"] = true
	legacy, err := AccountFromLegacy(metadata)
	if err != nil || legacy.Collections[0].Disabled || legacy.Collections[0].Settings["prefix"] != "fixture" {
		t.Fatal("legacy account settings or parent switch changed child preferences")
	}
	if read, errRead := ReadAccount(metadata); errRead != nil || read != nil {
		t.Fatal("legacy metadata was not recognized")
	}
	for _, scenario := range []string{"null", "version", "identity", "duplicate", "binding", "revision"} {
		t.Run(scenario, func(t *testing.T) {
			broken := CloneAccount(account)
			switch scenario {
			case "version":
				broken.Version++
			case "identity":
				broken.ID = "other"
			case "duplicate":
				broken.Collections = append(broken.Collections, broken.Collections[0])
			case "binding":
				broken.Collections[0].ProjectID = "456"
			case "revision":
				broken.Collections[0].Revision = 0
			}
			stored := PutAccount(metadata, broken)
			if scenario == "null" {
				stored["enterprise_account"] = nil
			}
			if _, errRead := ReadAccount(stored); errRead == nil {
				t.Fatal("corrupt graph was accepted")
			}
		})
	}
	applicationOnly := map[string]any{"email": "fixture@example.com", "client_id": "client"}
	empty, err := AccountFromLegacy(applicationOnly)
	if err != nil || len(empty.Collections) != 0 {
		t.Fatalf("account-only OAuth rejected: %v", err)
	}
}

func TestEnterpriseDiscoveryMergePreservesSettingsAndConcurrentBinding(t *testing.T) {
	_, account := accountFixture(t)
	account.Collections[0].Disabled = true
	account.Collections[0].Revision = 7
	old := account.Collections[0]
	added := Collection{ResourceBinding: ResourceBinding{ProjectID: "456", Location: "eu", CollectionID: "custom", EngineID: "new", AssistantID: "default_assistant"}}
	added.ID = CollectionID(added.ResourceBinding)
	discovered := old
	discovered.Disabled, discovered.Settings, discovered.Missing = false, nil, true
	result := DiscoveryResult{Collections: []Collection{discovered, added}, Status: "partial", CheckedAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), previous: map[string]ResourceBinding{old.ID: old.ResourceBinding}}
	merged, ids := MergeDiscoveredCollections(account, result)
	if len(merged.Collections) != 2 || !reflect.DeepEqual(ids, []string{added.ID}) || !merged.Collections[0].Disabled || merged.Collections[0].Settings["prefix"] != "fixture" || merged.Collections[0].Revision != 8 || !merged.Collections[0].Missing || merged.Collections[1].Disabled {
		t.Fatal("merge replaced settings, lost revision, or disabled new collection")
	}
	account.Collections[0].EngineID = "concurrent-edit"
	merged, _ = MergeDiscoveredCollections(account, result)
	if merged.Collections[0].EngineID != "concurrent-edit" || merged.Collections[0].Missing {
		t.Fatal("stale discovery overwrote concurrent resource edit")
	}
	merged.Collections[0].Settings["prefix"] = "changed"
	if account.Collections[0].Settings["prefix"] != "fixture" {
		t.Fatal("merge shared mutable settings")
	}
}
