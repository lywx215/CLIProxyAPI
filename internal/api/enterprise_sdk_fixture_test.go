package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type enterpriseSDKFixtureExecutor struct {
	enterpriseMediaAPIExecutor
	media []byte
	calls atomic.Int32
}

func (e *enterpriseSDKFixtureExecutor) Execute(_ context.Context, _ *auth.Auth, req coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
	e.calls.Add(1)
	if _, err := enterprise.NormalizeMedia(req.Payload, opts.SourceFormat.String(), req.Model, "video"); err != nil {
		return coreexec.Response{}, err
	}
	return coreexec.Response{Payload: enterprise.NewOutput().MediaResponse(req.Model, opts.SourceFormat.String(), "video", "video/mp4", e.media)}, nil
}

// newEnterpriseSDKFixture uses real handlers and shared jobs, without an upstream connection.
func newEnterpriseSDKFixture(t *testing.T, media []byte) (*httptest.Server, string, *enterpriseSDKFixtureExecutor) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	model := "enterprise-sdk-fixture-" + strings.ToLower(t.Name())
	m := auth.NewManager(nil, nil, nil)
	e := &enterpriseSDKFixtureExecutor{media: media}
	m.RegisterExecutor(e)
	credential := &auth.Auth{ID: model, Provider: "gemini-enterprise", Status: auth.StatusActive, Metadata: map[string]any{"model_policy": "catalog"}}
	if _, err := m.Register(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: model, Type: "gemini", EnterpriseMedia: "video"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, m)
	t.Cleanup(func() { base.CloseMediaJobs(context.Background()) })
	o, g := openai.NewOpenAIAPIHandler(base), gemini.NewGeminiAPIHandler(base)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		key := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if key == "" {
			key = c.GetHeader("x-goog-api-key")
		}
		if key == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("userApiKey", key)
	})
	router.POST("/openai/v1/videos", o.VideosCreate)
	router.GET("/openai/v1/videos/:video_id", o.VideosRetrieve)
	router.GET("/openai/v1/videos/:video_id/content", o.VideosContent)
	router.POST("/v1beta/models/*action", g.GeminiHandler)
	router.GET("/v1beta/models/*action", g.GeminiGetHandler)
	router.GET("/v1beta/operations/:operation_id", g.VideoOperation)
	router.GET("/v1beta/files/:file_id", g.VideoDownload)
	router.GET("/download/v1beta/files/:file_id", g.VideoDownload)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, model, e
}

func TestEnterpriseVideoSDKWireFixture(t *testing.T) {
	media := []byte("\x00\x00\x00\x18ftypisomfixture-wire-bytes")
	server, model, executor := newEnterpriseSDKFixture(t, media)
	request := func(method, path, body string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("x-goog-api-key", "fixture-alice")
		req.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if errClose := response.Body.Close(); errClose != nil {
				t.Errorf("close fixture response: %v", errClose)
			}
		}()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status=%d, err=%v", method, path, response.StatusCode, err)
		}
		return data
	}
	created := request("POST", "/v1beta/models/"+model+":predictLongRunning", `{"instances":[{"prompt":"fixture"}],"parameters":{"sampleCount":1}}`)
	name := gjson.GetBytes(created, "name").String()
	id := name[strings.LastIndex(name, "/")+1:]
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var completed []byte
	for {
		completed = request("GET", "/v1beta/"+name, "")
		if gjson.GetBytes(completed, "done").Bool() {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("fixture did not finish")
		default:
		}
	}
	uri := gjson.GetBytes(completed, "response.generateVideoResponse.generatedSamples.0.video.uri").String()
	if uri != "files/"+id || gjson.GetBytes(completed, "response.generateVideoResponse.generatedSamples.0.video.encoding").String() != "video/mp4" {
		t.Fatalf("invalid native SDK video resource: %s", completed)
	}
	global := request("GET", "/v1beta/operations/"+id, "")
	if gjson.GetBytes(global, "name").String() != name || !gjson.GetBytes(global, "done").Bool() {
		t.Fatal("operation route aliases disagree")
	}
	for _, path := range []string{"/v1beta/" + uri + ":download?alt=media", "/download/v1beta/" + uri + ":download?alt=media", "/openai/v1/videos/" + id + "/content"} {
		if data := request("GET", path, ""); !bytes.Equal(data, media) {
			t.Fatalf("download bytes differ for %s", path)
		}
	}
	if executor.calls.Load() != 1 {
		t.Fatal("operation polling or download replayed generation")
	}
}

// Enable explicitly with CLIPROXY_ENTERPRISE_SDK_SMOKE=1. SDKs and media are local inputs.
func TestEnterpriseVideoPythonSDKSmoke(t *testing.T) {
	if os.Getenv("CLIPROXY_ENTERPRISE_SDK_SMOKE") != "1" {
		t.Skip("optional local OpenAI and Google Gen AI Python SDK verification")
	}
	mediaPath := os.Getenv("CLIPROXY_ENTERPRISE_SDK_MEDIA")
	media, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read local MP4 fixture: %v", err)
	}
	server, model, executor := newEnterpriseSDKFixture(t, media)
	python := os.Getenv("CLIPROXY_ENTERPRISE_SDK_PYTHON")
	if python == "" {
		python = "python"
	}
	report := os.Getenv("CLIPROXY_ENTERPRISE_SDK_REPORT")
	if report == "" {
		report = filepath.Join(t.TempDir(), "sdk-fixture-report.json")
	}
	cmd := exec.CommandContext(t.Context(), python, filepath.Join("testdata", "enterprise_media_sdk_smoke.py"))
	cmd.Env = append(os.Environ(), "CLIPROXY_ENTERPRISE_SDK_BASE_URL="+server.URL, "CLIPROXY_ENTERPRISE_SDK_MODEL="+model, "CLIPROXY_ENTERPRISE_SDK_REPORT="+report)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python SDK smoke failed: %v\n%s", err, output)
	}
	if executor.calls.Load() != 2 {
		t.Fatalf("SDK create/poll/download executed %d generations, want 2", executor.calls.Load())
	}
	reportData, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(reportData, &result); err != nil {
		t.Fatal(err)
	}
	result["fixture_executor_calls"] = executor.calls.Load()
	result["upstream_calls"] = 0
	reportData, err = json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, append(reportData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Python SDK fixture verification passed: %s (%d bytes, no upstream calls)", report, len(media))
}
