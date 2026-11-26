package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type vertexProjectTestTransport func(*http.Request) (*http.Response, error)

func (f vertexProjectTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (f vertexProjectTestTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return f
}

func TestServiceVertexProjectCatalogAndRoutesAfterConfigChange(t *testing.T) {
	const key = "shared-vertex-test-key"
	const project = "vertex-test-project"
	const argon = "gemini-4-argon"
	const londonAlias = "vertex-project-test-london"
	const singaporeAlias = "vertex-project-test-singapore"
	const globalAlias = "vertex-project-test-global"
	london := internalconfig.VertexCompatKey{APIKey: key, ProjectID: project, Location: "europe-west2", Models: []internalconfig.VertexCompatModel{{Name: argon, Alias: londonAlias}}}
	singapore := internalconfig.VertexCompatKey{APIKey: key, ProjectID: project, Location: "asia-southeast1", Models: []internalconfig.VertexCompatModel{{Name: argon, Alias: singaporeAlias}}}
	global := internalconfig.VertexCompatKey{APIKey: key, ProjectID: project, Location: "global", Models: []internalconfig.VertexCompatModel{}}
	cfg := &config.Config{VertexCompatAPIKey: []internalconfig.VertexCompatKey{london, singapore, global}}
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	var upstreamCalls atomic.Int32
	var throttleLondon atomic.Bool
	manager.SetRoundTripperProvider(vertexProjectTestTransport(func(req *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		if req.Header.Get("X-Goog-Api-Key") != key {
			return nil, fmt.Errorf("wrong API key header for %s", req.URL.Host)
		}
		if throttleLondon.Load() && req.URL.Host == "europe-west2-aiplatform.googleapis.com" {
			body := `{"error":{"code":429,"message":"london quota","status":"RESOURCE_EXHAUSTED"}}`
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		body := fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":%q}]},"finishReason":"STOP"}]}`, req.URL.String())
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	service := &Service{coreManager: manager, cfg: cfg}
	manager.SetConfig(cfg)
	service.registerConfigAPIKeyAuths(ctx, cfg)

	registry := GlobalModelRegistry()
	byRegion := make(map[string]string)
	for _, credential := range manager.List() {
		byRegion[credential.VertexProjectRoute().Location] = credential.ID
		t.Cleanup(func() { registry.UnregisterClient(credential.ID) })
	}
	if len(byRegion) != 3 || byRegion["europe-west2"] == "" || byRegion["asia-southeast1"] == "" || byRegion["global"] == "" {
		t.Fatalf("configured regions did not create distinct credentials: %v", byRegion)
	}
	assertCatalog := func(location, alias string) {
		t.Helper()
		models := registry.GetModelsForClient(byRegion[location])
		if alias == "" {
			if len(models) != 0 {
				t.Errorf("%s publishes %d models with an empty allowlist", location, len(models))
			}
			return
		}
		if len(models) != 1 || models[0].ID != alias {
			t.Errorf("%s catalog = %v, want only %s", location, models, alias)
		}
	}
	assertCatalog("europe-west2", londonAlias)
	assertCatalog("asia-southeast1", singaporeAlias)
	assertCatalog("global", "")

	requestModel := func(alias, host, location, upstreamModel string) error {
		request := cliproxyexecutor.Request{Model: alias, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
		response, errRequest := manager.Execute(ctx, []string{"vertex"}, request, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
		if errRequest != nil {
			return fmt.Errorf("%s: %w", alias, errRequest)
		}
		wantURL := fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", host, project, location, upstreamModel)
		if gotURL := gjson.GetBytes(response.Payload, "candidates.0.content.parts.0.text").String(); gotURL != wantURL {
			return fmt.Errorf("%s routed to %q, want %q", alias, gotURL, wantURL)
		}
		return nil
	}
	results := make(chan error, 2)
	go func() {
		results <- requestModel(londonAlias, "europe-west2-aiplatform.googleapis.com", "europe-west2", argon)
	}()
	go func() {
		results <- requestModel(singaporeAlias, "asia-southeast1-aiplatform.googleapis.com", "asia-southeast1", argon)
	}()
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if calls := upstreamCalls.Load(); calls != 2 {
		t.Fatalf("first two aliases made %d upstream calls, want 2", calls)
	}
	throttleLondon.Store(true)
	if err := requestModel(londonAlias, "europe-west2-aiplatform.googleapis.com", "europe-west2", argon); err == nil || !strings.Contains(err.Error(), "london quota") {
		t.Errorf("London returned %v, want its own quota error", err)
	}
	if calls := upstreamCalls.Load(); calls != 3 {
		t.Fatalf("London's quota error made %d additional upstream calls, want 1", calls-2)
	}

	london.Models = nil
	global.Models = []internalconfig.VertexCompatModel{{Name: "gemini-3.8-flash", Alias: globalAlias}}
	updated := &config.Config{VertexCompatAPIKey: []internalconfig.VertexCompatKey{global, london, singapore}}
	service.cfg = updated
	manager.SetConfig(updated)
	service.registerConfigAPIKeyAuths(ctx, updated)

	assertCatalog("europe-west2", "")
	assertCatalog("asia-southeast1", singaporeAlias)
	assertCatalog("global", globalAlias)
	if err := requestModel(globalAlias, "aiplatform.googleapis.com", "global", "gemini-3.8-flash"); err != nil {
		t.Error(err)
	}
	if err := requestModel(londonAlias, "europe-west2-aiplatform.googleapis.com", "europe-west2", argon); err == nil {
		t.Error("removed alias remained callable")
	}
	if calls := upstreamCalls.Load(); calls != 4 {
		t.Errorf("removed alias sent an upstream request: %d total calls, want 4", calls)
	}
}
