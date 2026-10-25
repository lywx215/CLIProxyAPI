package geminienterprise

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRegionalEndpointsAndCredentialConfinement(t *testing.T) {
	for region, host := range map[string]string{"global": "discoveryengine.googleapis.com", "us": "us-discoveryengine.googleapis.com", "eu": "eu-discoveryengine.googleapis.com", "ca": "discoveryengine.googleapis.com"} {
		binding := config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", Location: region, EngineID: "app"}
		endpoint := "https://" + host + "/v1alpha/" + Engine(binding) + "/assistants/default_assistant:streamAssist"
		if OriginForLocation(region) != "https://"+host {
			t.Fatal("incorrect endpoint", region)
		}
		for _, candidate := range []struct {
			url   string
			valid bool
		}{
			{endpoint, true},
			{strings.Replace(endpoint, "https:", "http:", 1), false},
			{strings.Replace(endpoint, host, host+".evil.test", 1), false},
			{strings.Replace(endpoint, host, host+":443", 1), false},
			{strings.Replace(endpoint, "https://", "https://user@", 1), false},
			{strings.Replace(endpoint, "projects/123", "projects/124", 1), false},
			{strings.Replace(endpoint, "/locations/"+region, "/locations/other", 1), false},
			{strings.Replace(endpoint, "/projects/", "/projects%2F", 1), false},
			{endpoint + "#fragment", false},
			{endpoint + "/../other", false},
			{endpoint + "//other", false},
		} {
			req, err := http.NewRequest(http.MethodGet, candidate.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if OfficialBinding(req, "123", region) != candidate.valid {
				t.Fatalf("binding guard region=%s target=%s", region, candidate.url)
			}
		}
		if region == "global" {
			req, _ := http.NewRequest(http.MethodGet, strings.Replace(endpoint, host, "global-discoveryengine.googleapis.com", 1), nil)
			if !OfficialBinding(req, "123", "") {
				t.Fatal("lost global alias or missing-location compatibility")
			}
		}
		if region == "us" || region == "eu" {
			for _, foreign := range []string{"discoveryengine.googleapis.com", "global-discoveryengine.googleapis.com", "us-discoveryengine.googleapis.com", "eu-discoveryengine.googleapis.com"} {
				if foreign == host {
					continue
				}
				req, _ := http.NewRequest(http.MethodGet, strings.Replace(endpoint, host, foreign, 1), nil)
				if OfficialResource(req) {
					t.Fatalf("cross-region host accepted: %s", req.URL)
				}
			}
		}
	}
	for _, resource := range []string{"", "projects/p/locations/../x", "projects/p/locations/us.evil", "locations/us/projects/p", "projects/p/locations/"} {
		if _, err := OriginForResource(resource); err == nil {
			t.Fatal("accepted invalid resource", resource)
		}
	}
}

func TestRegionalDiscoveryAndResourceValidation(t *testing.T) {
	for _, region := range []string{"global", "us", "eu"} {
		t.Run(region, func(t *testing.T) {
			binding := (config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", Location: region, EngineID: "app"}).WithDefaults()
			calls := 0
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				if "https://"+req.URL.Host != OriginForLocation(region) || !strings.HasPrefix(req.URL.Path, "/v1") || !strings.Contains(req.URL.Path, "/locations/"+region+"/") || req.Header.Get("X-Goog-User-Project") != "123" || req.Header.Get("Authorization") != "Bearer synthetic" {
					t.Fatal("regional credential routing failed", req.URL)
				}
				if _, deadline := req.Context().Deadline(); deadline {
					t.Fatal("unexpected discovery timeout")
				}
				body := fmt.Sprintf(`{"name":%q}`, Engine(binding))
				if strings.Contains(req.URL.Path, "/widgetConfigs/") {
					body = `{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"regional-model"}]}}}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			if err := ValidateResources(context.Background(), client, binding, "synthetic"); err != nil || calls != 2 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRegionalAutomaticResourceResolution(t *testing.T) {
	for _, region := range []string{"us", "eu"} {
		t.Run(region, func(t *testing.T) {
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				status, body := 404, `{}`
				if req.URL.Host == "cloudresourcemanager.googleapis.com" {
					status, body = 200, `{"projects":[{"name":"projects/123","state":"ACTIVE"}]}`
				} else if !OfficialResource(req) {
					t.Fatal("unexpected resource endpoint", req.URL)
				} else if strings.Contains(req.URL.Path, "/locations/"+region+"/") {
					status, body = 200, fmt.Sprintf(`{"name":%q}`, strings.TrimPrefix(req.URL.Path, "/v1/"))
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			binding, err := ResolveResources(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "client", EngineID: "app"}, "synthetic")
			if err != nil || binding.Location != region || binding.ProjectID != "123" {
				t.Fatalf("resolution: %+v %v", binding, err)
			}
		})
	}
}
