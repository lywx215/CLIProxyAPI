package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestConfigV8JSONRootPreservesArchivedUnknownData(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	initial := "# Keep the document header\nport: 8317\nspeed-throttle: {enabled: false, max-concurrent: 3, queue-size: 5}\nretired-feature: {nested: [one, {two: 2}]}\n"
	if err := os.WriteFile(file, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: file}
	router := gin.New()
	router.GET("/v8/management/config", h.ConfigV8)
	router.PUT("/v8/management/config", h.ConfigV8)
	router.PATCH("/v8/management/config", h.ConfigV8)
	for iteration := 0; iteration < 3; iteration++ {
		read := httptest.NewRecorder()
		router.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
		if read.Code != http.StatusOK || !json.Valid(read.Body.Bytes()) {
			t.Fatalf("GET status=%d body=%s", read.Code, read.Body.String())
		}
		write := httptest.NewRecorder()
		router.ServeHTTP(write, httptest.NewRequest(http.MethodPut, "/v8/management/config", strings.NewReader(read.Body.String())))
		if write.Code != http.StatusOK {
			t.Fatalf("root PUT status=%d body=%s", write.Code, write.Body.String())
		}
		patch := httptest.NewRecorder()
		router.ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(fmt.Sprintf(`{"server":{"port":%d}}`, 8318+iteration))))
		if patch.Code != http.StatusOK {
			t.Fatalf("root PATCH status=%d body=%s", patch.Code, patch.Body.String())
		}
		saved, errRead := os.ReadFile(file)
		if errRead != nil || strings.Count(string(saved), "# retired-feature:") != 1 || !strings.Contains(string(saved), "two: 2") || !strings.Contains(string(saved), "Keep the document header") {
			t.Fatalf("JSON round-trip lost/duplicated archive: %v\n%s", errRead, saved)
		}
		if errValidate := config.ValidateV8Config(saved); errValidate != nil {
			t.Fatal(errValidate)
		}
		loaded, errParse := config.ParseConfigBytes(saved)
		if errParse != nil || loaded.SpeedThrottle.MaxConcurrent != 3 || loaded.SpeedThrottle.QueueSize != 5 {
			t.Fatalf("JSON round-trip lost reserved throttle settings: %v\n%s", errParse, saved)
		}
	}
}

func TestV8GeminiCLIPluginOAuthKeepsProjectAndCallback(t *testing.T) {
	host := pluginhost.New()
	var captured pluginapi.AuthLoginStartRequest
	state := "fork-v8-gemini-cli-project-1234567890"
	t.Cleanup(func() { CancelOAuthSession(state) })
	host.RegisterPluginForTest("gemini-cli-plugin", pluginapi.Plugin{
		Capabilities: pluginapi.Capabilities{AuthProvider: &testAuthProvider{
			identifier: "gemini-cli",
			startLogin: func(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
				captured = req
				return pluginapi.AuthLoginStartResponse{Provider: req.Provider, URL: "https://login.example.invalid", State: state, ExpiresAt: time.Now().Add(time.Hour)}, nil
			},
		}},
	})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), Port: 8317}, nil)
	h.SetPluginHost(host)
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=gemini-cli&project_id=fork-project&is_webui=true", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if captured.Provider != "gemini-cli" || captured.Metadata["project_id"] != "fork-project" || captured.Metadata["is_webui"] != "true" || captured.BaseURL != "http://127.0.0.1:8317/v8/management/oauth/callback" {
		t.Fatalf("Gemini CLI login contract changed: %#v", captured)
	}
	if _, exists := captured.Metadata["provider"]; exists {
		t.Fatal("routing provider leaked into plugin OAuth metadata")
	}
}
