package geminienterprise

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func discoveryListResponse(kind string, names ...string) *http.Response {
	items := make([]map[string]string, 0, len(names))
	for _, name := range names {
		items = append(items, map[string]string{"name": name})
	}
	data, _ := json.Marshal(map[string]any{kind: items})
	return resourceResponse(200, string(data))
}

func TestEnterpriseAccountDiscoveryPagesAllScopesAndStableSelection(t *testing.T) {
	old := Collection{ResourceBinding: ResourceBinding{ProjectID: "123", Location: "global", CollectionID: "custom", EngineID: "z-engine", AssistantID: "z-assistant"}, Revision: 4, Disabled: true, Settings: map[string]any{"prefix": "old"}}
	old.ID = CollectionID(old.ResourceBinding)
	var projects, scopes, assistants atomic.Int32
	client := &http.Client{Timeout: time.Nanosecond, Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("discovery sent a mutation or lost authorization")
		}
		if _, deadline := req.Context().Deadline(); deadline {
			t.Error("metadata discovery inherited a fixed client deadline")
		}
		if req.URL.Host == "cloudresourcemanager.googleapis.com" {
			projects.Add(1)
			if req.URL.Query().Get("pageToken") == "next" {
				return resourceResponse(200, `{"projects":[{"name":"projects/456","projectId":"other-project","state":"ACTIVE"},{"name":"projects/123","state":"ACTIVE"}]}`), nil
			}
			return resourceResponse(200, `{"projects":[{"name":"projects/123","projectId":"legacy-project","displayName":"Project One","state":"ACTIVE"},{"name":"projects/999","state":"DELETE_REQUESTED"}],"nextPageToken":"next"}`), nil
		}
		if !OfficialResource(req) {
			t.Errorf("untrusted resource URL %s", req.URL.Host)
		}
		parent := strings.TrimPrefix(req.URL.Path, "/v1/")
		if strings.HasSuffix(parent, "/collections") {
			scopes.Add(1)
			parent = strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/v1alpha/"), "/collections")
			if strings.Contains(parent, "/projects/999/") {
				t.Error("inactive project was enumerated")
			}
			return discoveryListResponse("collections", parent+"/collections/custom"), nil
		}
		if strings.HasSuffix(parent, "/engines") {
			parent = strings.TrimSuffix(parent, "/engines")
			if req.URL.Query().Get("pageToken") == "engines-next" {
				return discoveryListResponse("engines", parent+"/engines/a-engine"), nil
			}
			return resourceResponse(200, fmt.Sprintf(`{"engines":[{"name":%q}],"nextPageToken":"engines-next"}`, parent+"/engines/z-engine")), nil
		}
		if strings.HasSuffix(parent, "/assistants") {
			assistants.Add(1)
			parent = strings.TrimSuffix(parent, "/assistants")
			return discoveryListResponse("assistants", parent+"/assistants/a-assistant", parent+"/assistants/z-assistant", parent+"/assistants/default_assistant"), nil
		}
		t.Errorf("unexpected metadata path %s", req.URL.Path)
		return resourceResponse(500, ""), nil
	})}
	result, err := DiscoverResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "application"}, "synthetic-access", []Collection{old})
	wantScopes := 2 * len(SupportedLocations())
	if err != nil || result.Status != "ok" || len(result.Collections) != wantScopes || len(result.Coverage) != wantScopes || len(result.Projects) != 2 || projects.Load() != 2 || scopes.Load() != int32(wantScopes) || assistants.Load() != int32(2*wantScopes) {
		t.Fatalf("discovery status=%s collections=%d coverage=%d err=%v", result.Status, len(result.Collections), len(result.Coverage), err)
	}
	for _, collection := range result.Collections {
		if collection.ID == old.ID {
			if collection.EngineID != old.EngineID || collection.AssistantID != old.AssistantID {
				t.Fatal("existing valid choices changed")
			}
		} else if collection.EngineID != "a-engine" || collection.AssistantID != "default_assistant" || collection.Disabled {
			t.Fatal("new collection selection was not deterministic or enabled")
		}
	}
}

func TestEnterpriseAccountDiscoveryPartialScopesNeverDeleteOldBindings(t *testing.T) {
	for _, region := range SupportedLocations() {
		for _, scenario := range []string{"collection-permission", "engine-permission", "assistant-permission", "engine-missing", "absent-project"} {
			t.Run(region+"/"+scenario, func(t *testing.T) {
				metadata, account := accountFixture(t)
				_ = metadata
				account.Collections[0].Location = region
				account.Collections[0].ID = CollectionID(account.Collections[0].ResourceBinding)
				old := account.Collections[0]
				client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host == "cloudresourcemanager.googleapis.com" {
						if scenario == "absent-project" {
							return resourceResponse(200, `{"projects":[]}`), nil
						}
						return resourceResponse(200, `{"projects":[{"name":"projects/123","state":"ACTIVE"}]}`), nil
					}
					if !OfficialResource(req) {
						t.Error("credential sent to an inconsistent regional endpoint")
					}
					if !strings.Contains(req.URL.Path, "/locations/"+region+"/") {
						return discoveryListResponse("collections"), nil
					}
					parent := collectionResourceName(old.ResourceBinding)
					switch req.URL.Path {
					case "/v1alpha/projects/123/locations/" + region + "/collections":
						if scenario == "collection-permission" {
							return resourceResponse(403, `{"error":{"message":"synthetic-secret"}}`), nil
						}
						return discoveryListResponse("collections", parent), nil
					case "/v1/" + parent + "/engines":
						if scenario == "engine-permission" {
							return resourceResponse(403, `{"error":{"message":"synthetic-secret"}}`), nil
						}
						if scenario == "engine-missing" {
							return discoveryListResponse("engines"), nil
						}
						return discoveryListResponse("engines", parent+"/engines/engine"), nil
					case "/v1/" + parent + "/engines/engine/assistants":
						return resourceResponse(403, `{"error":{"message":"synthetic-secret"}}`), nil
					case "/v1/" + parent + "/engines/engine":
						return resourceResponse(404, `{}`), nil
					}
					return resourceResponse(500, `{}`), nil
				})}
				result, err := DiscoverResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "client"}, "synthetic-access", account.Collections)
				if err != nil {
					t.Fatal(err)
				}
				merged, added := MergeDiscoveredCollections(account, result)
				if len(merged.Collections) != 1 || len(added) != 0 || merged.Collections[0].EngineID != old.EngineID || merged.Collections[0].Missing != (scenario == "engine-missing") {
					t.Fatalf("partial discovery changed or lost saved collection: %+v", merged.Collections)
				}
				raw, _ := json.Marshal(result)
				if strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "synthetic-access") {
					t.Fatal("discovery diagnostic leaked credentials or upstream text")
				}
			})
		}
	}
}

func TestEnterpriseAccountDiscoveryCanonicalProjectMigrationKeepsSettingsAndAliases(t *testing.T) {
	metadata, account := accountFixture(t)
	account.Collections[0].ProjectID = "legacy-project"
	account.Collections[0].ID = CollectionID(account.Collections[0].ResourceBinding)
	account.Collections[0].Disabled, account.Collections[0].Revision = true, 11
	oldID := account.Collections[0].ID
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "cloudresourcemanager.googleapis.com" {
			return resourceResponse(200, `{"projects":[{"name":"projects/123","projectId":"legacy-project","state":"ACTIVE"}]}`), nil
		}
		if strings.Contains(req.URL.Path, "legacy-project") {
			t.Error("legacy slug was sent instead of the canonical project number")
		}
		if !strings.Contains(req.URL.Path, "/locations/global/") {
			return discoveryListResponse("collections"), nil
		}
		resource := strings.TrimPrefix(strings.TrimPrefix(req.URL.Path, "/v1alpha/"), "/v1/")
		if strings.HasSuffix(resource, "/collections") {
			return discoveryListResponse("collections", resource+"/default_collection"), nil
		}
		if strings.HasSuffix(resource, "/engines") {
			return discoveryListResponse("engines", resource+"/engine"), nil
		}
		return discoveryListResponse("assistants", resource+"/default_assistant"), nil
	})}
	result, err := DiscoverResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "client"}, "synthetic", account.Collections)
	if err != nil {
		t.Fatal(err)
	}
	merged, added := MergeDiscoveredCollections(account, result)
	if len(merged.Collections) != 1 || len(added) != 0 || merged.Collections[0].ProjectID != "123" || merged.Collections[0].ID == oldID || !merged.Collections[0].Disabled || merged.Collections[0].Settings["prefix"] != "fixture" || merged.Collections[0].Revision != 12 || !containsResource(merged.Collections[0].LegacyAliases, oldID) {
		t.Fatalf("canonical migration lost collection settings or identity: %+v, added=%v", merged.Collections, added)
	}
	if _, errRead := ReadAccount(PutAccount(metadata, merged)); errRead != nil {
		t.Fatal(errRead)
	}
	canonical := account.Collections[0]
	canonical.ProjectID = "123"
	canonical.ID = CollectionID(canonical.ResourceBinding)
	canonical.Settings = map[string]any{"prefix": "source", "note": "retained-source-note"}
	canonical.LegacyAliases = []string{"numeric-source-file"}
	account.Collections = append(account.Collections, canonical)
	result, err = DiscoverResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "client"}, "synthetic", account.Collections)
	if err != nil {
		t.Fatal(err)
	}
	merged, added = MergeDiscoveredCollections(account, result)
	if len(merged.Collections) != 1 || len(added) != 0 || merged.Collections[0].Settings["prefix"] != "fixture" || merged.Collections[0].Settings["note"] != "retained-source-note" || !containsResource(merged.Collections[0].LegacyAliases, "numeric-source-file") || !containsResource(merged.Collections[0].LegacyAliases, oldID) {
		t.Fatalf("canonical collision duplicated a collection or lost target settings/source aliases: %+v", merged.Collections)
	}
}

func TestEnterpriseAccountDiscoveryCapsConcurrentRequestsAcrossCalls(t *testing.T) {
	var active, maximum atomic.Int32
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "cloudresourcemanager.googleapis.com" {
			return resourceResponse(200, `{"projects":[{"name":"projects/123","state":"ACTIVE"}]}`), nil
		}
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return discoveryListResponse("collections"), nil
	})}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := DiscoverResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "app"}, "synthetic", nil)
			done <- err
		}()
	}
	for i := 0; i < 3; i++ {
		<-entered
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 3 {
		t.Fatalf("metadata request concurrency was %d", maximum.Load())
	}
}

func TestEnterpriseAccountDiscoveryPaginationOriginsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"project-cycle", "collection-cycle", "malformed-name", "redirect", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			var calls atomic.Int32
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if req.URL.Host != "cloudresourcemanager.googleapis.com" && !OfficialResource(req) {
					t.Error("authorization escaped official metadata origins")
				}
				if scenario == "redirect" {
					response := resourceResponse(302, `{}`)
					response.Header.Set("Location", "https://untrusted.example/secret")
					return response, nil
				}
				if req.URL.Host == "cloudresourcemanager.googleapis.com" {
					if scenario == "project-cycle" {
						return resourceResponse(200, `{"nextPageToken":"secret-page-token"}`), nil
					}
					return resourceResponse(200, `{"projects":[{"name":"projects/123","state":"ACTIVE"}]}`), nil
				}
				if scenario == "malformed-name" {
					return discoveryListResponse("collections", "projects/other/locations/global/collections/secret-resource"), nil
				}
				return resourceResponse(200, `{"nextPageToken":"secret-page-token"}`), nil
			})}
			result, err := DiscoverResources(ctx, client, config.GeminiEnterpriseOAuth{ClientID: "app"}, "synthetic-access", nil)
			if err == nil && result.Status != "partial" {
				t.Fatal("invalid or incomplete metadata was accepted")
			}
			if scenario == "redirect" && calls.Load() != 1 {
				t.Fatal("metadata followed a redirect")
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "secret-page-token") || strings.Contains(string(raw), "secret-resource") || strings.Contains(string(raw), "synthetic-access") {
				t.Fatal("untrusted metadata was copied into diagnostics")
			}
		})
	}
}

func TestEnterpriseAccountDiscoveryRequestAndBodyLimits(t *testing.T) {
	budget := &discoveryBudget{}
	ctx := context.WithValue(context.Background(), discoveryBudgetKey{}, budget)
	called := false
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		called = true
		return resourceResponse(200, strings.Repeat(" ", 4*1024*1024+1)), nil
	})}
	var body any
	_, err := resourceJSON(ctx, client, "https://cloudresourcemanager.googleapis.com/v3/projects:search", "synthetic", "", &body)
	if err == nil || called || discoveryWarning("", "", err).Code != "request_limit" {
		t.Fatal("exhausted discovery budget sent another metadata request")
	}
	_, err = resourceJSON(context.Background(), client, "https://cloudresourcemanager.googleapis.com/v3/projects:search", "synthetic", "", &body)
	if err == nil || !called {
		t.Fatal("oversized discovery body was accepted")
	}
}
