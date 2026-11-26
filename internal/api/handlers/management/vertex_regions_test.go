package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

type vertexProbeTransport func(*http.Request) (*http.Response, error)

func (f vertexProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestVertexRegionPresets(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	(&Handler{}).GetVertexRegions(c)
	var body struct {
		TestedAt   string                 `json:"tested_at"`
		Regions    []vertexRegionPreset   `json:"regions"`
		Candidates []vertexModelCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	all := []string{"gemini-4-neon", "gemini-4-hydrogen", "gemini-4-carbon", "gemini-4-argon"}
	argon := []string{"gemini-4-argon"}
	if !reflect.DeepEqual(body.Regions, []vertexRegionPreset{{"global", []string{}}, {"us", all}, {"eu", all}, {"europe-west2", argon}, {"asia-southeast1", argon}}) || body.TestedAt != "2026-11-26" {
		t.Fatalf("unexpected preset: %#v", body.Regions)
	}
	wantCandidates := []vertexModelCandidate{
		{Model: "gemini-3.1-pro-preview", Locations: []string{"global"}},
		{Model: "gemini-3.8-flash", Locations: []string{"global", "us", "eu", "northamerica-northeast1", "asia-northeast1"}},
		{Model: "gemini-2.5-pro", Locations: []string{}, RetiredAt: "2026-10-20"},
	}
	if len(body.Candidates) != 7 || !reflect.DeepEqual(body.Candidates[4:], wantCandidates) {
		t.Fatalf("unexpected candidates: %#v", body.Candidates)
	}
}

func TestVertexModelTestUsesSavedScopedKeyAndRevision(t *testing.T) {
	const credential = "synthetic-vertex-key"
	const raw = `config-version: 8
api-keys:
  vertex:
    - name: global
      keys:
        - api-key: synthetic-vertex-key
          project-id: '123456789012'
          location: global
          models: []
    - name: eu
      keys:
        - api-key: synthetic-vertex-key
          project-id: '123456789012'
          location: eu
          models: [{name: gemini-4-carbon, alias: gemini-4-carbon-eu}]
    - name: express
      keys:
        - api-key: synthetic-vertex-key
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{configFilePath: path}
	router := gin.New()
	router.GET("/config/*path", h.ConfigV8)
	router.PUT("/config/*path", h.ConfigV8)
	router.POST("/vertex/model-test", h.TestVertexModel)
	configView := httptest.NewRecorder()
	router.ServeHTTP(configView, httptest.NewRequest(http.MethodGet, "/config/api-keys/vertex", nil))
	global := gjson.Get(configView.Body.String(), "0.keys.0.auth_index").String()
	eu := gjson.Get(configView.Body.String(), "1.keys.0.auth_index").String()
	express := gjson.Get(configView.Body.String(), "2.keys.0.auth_index").String()
	tag := configView.Header().Get("ETag")
	if configView.Code != http.StatusOK || global == "" || eu == "" || express == "" || global == eu || global == express || eu == express || tag == "" {
		t.Fatalf("expected a distinct index for each saved route, got %d %q %q %q", configView.Code, global, eu, express)
	}
	upstreamCalls := 0
	test := func(index, match string, upstreamCode int, upstreamBody string) *httptest.ResponseRecorder {
		t.Helper()
		transport := vertexProbeTransport(func(req *http.Request) (*http.Response, error) {
			upstreamCalls++
			if req.URL.String() != "https://aiplatform.googleapis.com/v1/projects/123456789012/locations/global/publishers/google/models/gemini-3.8-flash:generateContent" || req.Header.Get("x-goog-api-key") != credential || req.Header.Get("Authorization") != "" {
				t.Fatal("probe did not select the saved global route")
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int() != 128 || gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel").String() != "LOW" {
				t.Fatalf("unexpected probe payload: %s, %v", body, err)
			}
			return &http.Response{StatusCode: upstreamCode, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(upstreamBody))}, nil
		})
		req := httptest.NewRequest(http.MethodPost, "/vertex/model-test", strings.NewReader(`{"auth_index":"`+index+`","model":"gemini-3.8-flash"}`))
		req.Header.Set("If-Match", match)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req.WithContext(context.WithValue(req.Context(), "cliproxy.roundtripper", transport)))
		return response
	}
	if res := test(global, "", 0, ""); res.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing revision: %d %s", res.Code, res.Body)
	}
	if res := test(global, `"stale"`, 0, ""); res.Code != http.StatusConflict || res.Header().Get("ETag") != tag {
		t.Fatalf("stale revision: %d %s", res.Code, res.Body)
	}
	if res := test(express, tag, 0, ""); res.Code != http.StatusNotFound || upstreamCalls != 0 {
		t.Fatalf("legacy key should not be probed: %d calls=%d", res.Code, upstreamCalls)
	}
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		res := test(global, tag, status, `{"error":{"status":"UPSTREAM_ERROR","message":"synthetic-vertex-key denied"}}`)
		if res.Code != http.StatusOK || gjson.Get(res.Body.String(), "status_code").Int() != int64(status) || strings.Contains(res.Body.String(), credential) || gjson.Get(res.Body.String(), "success").Bool() {
			t.Fatalf("unexpected upstream error response: %d %s", res.Code, res.Body)
		}
	}
	res := test(global, tag, http.StatusOK, `{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}]}`)
	if res.Code != http.StatusOK || gjson.Get(res.Body.String(), "success").Bool() {
		t.Fatalf("partial answer was accepted: %d %s", res.Code, res.Body)
	}
	res = test(global, tag, http.StatusOK, `{"modelVersion":"gemini-3.8-flash","candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`)
	var result vertexModelTestResult
	if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || res.Code != http.StatusOK || !result.Success || result.AuthIndex != global || result.Location != "global" || result.ModelVersion != "gemini-3.8-flash" || `"`+result.Revision+`"` != tag {
		t.Fatalf("expected a successful read-only probe: %d %s, err=%v", res.Code, res.Body, err)
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil || string(saved) != raw {
		t.Fatal("probe modified the persisted configuration")
	}

	var groups []struct {
		Name string                   `json:"name"`
		Keys []config.VertexCompatKey `json:"keys"`
	}
	if err := json.Unmarshal(configView.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	groups[0].Keys[0].Models = []config.VertexCompatModel{{Name: result.Model, Alias: "gemini-3.8-flash-global"}}
	payload, errMarshal := json.Marshal(groups)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	save := httptest.NewRequest(http.MethodPut, "/config/api-keys/vertex", strings.NewReader(string(payload)))
	save.Header.Set("If-Match", tag)
	updated := httptest.NewRecorder()
	router.ServeHTTP(updated, save)
	if updated.Code != http.StatusOK || updated.Header().Get("ETag") == tag {
		t.Fatalf("applying the reviewed change failed: %d %s", updated.Code, updated.Body)
	}
	if res = test(global, tag, 0, ""); res.Code != http.StatusConflict || upstreamCalls != 5 {
		t.Fatalf("old result revision remained usable: %d calls=%d", res.Code, upstreamCalls)
	}
}
