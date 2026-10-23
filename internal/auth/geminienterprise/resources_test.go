package geminienterprise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func resourceResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestResourceDiscoveryUsesPagedProjectsAndEngineGet(t *testing.T) {
	binding := config.GeminiEnterpriseOAuth{ClientID: "app", EngineID: "manual-engine", AssistantID: "custom-assistant", ProjectID: "previous-account", Location: "eu", CollectionID: "previous-collection"}
	pages, reads := 0, 0
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Fatal("missing account token")
		}
		if req.URL.Host == "cloudresourcemanager.googleapis.com" {
			pages++
			if req.URL.Path != "/v3/projects:search" || req.Header.Get("X-Goog-User-Project") != "" {
				t.Fatal("invalid project search")
			}
			if pages == 1 {
				return resourceResponse(200, `{"projects":[{"name":"projects/111","state":"ACTIVE"}],"nextPageToken":"next"}`), nil
			}
			if req.URL.Query().Get("pageToken") != "next" {
				t.Fatal("pagination was not followed")
			}
			return resourceResponse(200, `{"projects":[{"name":"projects/222","state":"ACTIVE"},{"name":"projects/111","state":"ACTIVE"}]}`), nil
		}
		if req.URL.Host != "discoveryengine.googleapis.com" || !strings.HasSuffix(req.URL.Path, "/engines/manual-engine") {
			t.Fatal("discovery must use Engine.Get only")
		}
		reads++
		project := strings.Split(req.URL.Path, "/")[3]
		if req.Header.Get("X-Goog-User-Project") != project {
			t.Fatal("resource project was not used for quota")
		}
		if req.URL.Path == "/v1/projects/222/locations/global/collections/default_collection/engines/manual-engine" {
			return resourceResponse(200, `{"name":"projects/222/locations/global/collections/default_collection/engines/manual-engine"}`), nil
		}
		return resourceResponse(403, `{"error":{"message":"synthetic-access must not leak"}}`), nil
	})}
	resolved, err := ResolveResources(context.Background(), client, binding, "synthetic-access")
	if err != nil || resolved.ProjectID != "222" || resolved.Location != "global" || resolved.CollectionID != "default_collection" || resolved.EngineID != binding.EngineID || resolved.AssistantID != "custom-assistant" || pages != 2 || reads != 2*len(discoveryLocations) {
		t.Fatalf("unexpected resource resolution: %+v, pages=%d reads=%d err=%v", resolved, pages, reads, err)
	}
	if binding.ProjectID != "previous-account" {
		t.Fatal("input binding was mutated")
	}
}

func TestResourceDiscoveryRejectsAmbiguousIncompleteOrInconsistentResults(t *testing.T) {
	for _, scenario := range []string{"ambiguous", "inconsistent", "partial-failure", "project-denied", "not-found", "pagination-cycle"} {
		t.Run(scenario, func(t *testing.T) {
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "cloudresourcemanager.googleapis.com" {
					if scenario == "project-denied" {
						return resourceResponse(403, `{"error":{"message":"secret-response"}}`), nil
					}
					if scenario == "pagination-cycle" {
						return resourceResponse(200, `{"nextPageToken":"repeated"}`), nil
					}
					return resourceResponse(200, `{"projects":[{"name":"projects/111","state":"ACTIVE"},{"name":"projects/222","state":"ACTIVE"}]}`), nil
				}
				if scenario == "not-found" {
					return resourceResponse(404, `{"error":{"message":"secret-response"}}`), nil
				}
				if scenario == "partial-failure" && strings.Contains(req.URL.Path, "/projects/222/") {
					return resourceResponse(503, `{"error":{"message":"secret-response"}}`), nil
				}
				if !strings.Contains(req.URL.Path, "/locations/global/") {
					return resourceResponse(404, `{}`), nil
				}
				name := strings.TrimPrefix(req.URL.Path, "/v1/")
				if scenario == "inconsistent" {
					name = "projects/other/locations/global/collections/default_collection/engines/engine"
				}
				return resourceResponse(200, fmt.Sprintf(`{"name":%q}`, name)), nil
			})}
			_, err := ResolveResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "app", EngineID: "engine"}, "synthetic-access")
			if err == nil || strings.Contains(err.Error(), "secret-response") || strings.Contains(err.Error(), "synthetic-access") {
				t.Fatalf("unsafe or absent discovery error: %v", err)
			}
		})
	}
}

func TestResourceDiscoveryCancellationValidationAndRedirectIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ResolveResources(ctx, &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})}, config.GeminiEnterpriseOAuth{ClientID: "app", EngineID: "engine"}, "token")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	called := 0
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		called++
		if req.URL.Host != "cloudresourcemanager.googleapis.com" {
			t.Fatal("credentials followed a redirect")
		}
		response := resourceResponse(302, `{}`)
		response.Header.Set("Location", "https://untrusted.example/resource")
		return response, nil
	})}
	_, err = ResolveResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "app", EngineID: "../escape"}, "token")
	if err == nil || called != 0 {
		t.Fatal("invalid manual engine accepted")
	}
	_, err = ResolveResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "app", EngineID: "engine"}, "token")
	if err == nil || called != 1 {
		t.Fatal("redirect not rejected")
	}
}
