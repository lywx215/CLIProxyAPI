package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestConfigV8WebInputLimitsPersistAndReloadWithoutEnablingChannel(t *testing.T) {
	h, router, path := newConfigV8TransactionTest(t)
	h.authManager = coreauth.NewManager(nil, nil, nil)
	h.authManager.SetConfig(h.cfg)
	router.GET("/config/*path", h.ConfigV8)
	router.PATCH("/config/*path", h.ConfigV8)
	endpoint := "/config/upstream/gemini-enterprise-web/input-limits"
	patch := httptest.NewRecorder()
	router.ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, endpoint, strings.NewReader(`{"max-contents":70,"max-text-bytes":8388608,"max-attachments":20}`)))
	if patch.Code != http.StatusOK {
		t.Fatalf("input-limit PATCH failed: %d %s", patch.Code, patch.Body.String())
	}
	reloaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	limits := reloaded.GeminiEnterpriseWeb.WithDefaults().InputLimits
	if limits.MaxContents != 70 || limits.MaxTextBytes != 8<<20 || limits.MaxAttachments != 20 || reloaded.GeminiEnterpriseWeb.Enabled {
		t.Fatal("persisted limits or channel state changed", reloaded.GeminiEnterpriseWeb)
	}
	status := h.authManager.WebRuntime().Status()
	actual, ok := status["input_limits"].(config.GeminiEnterpriseWebInputLimits)
	if !ok || actual.MaxContents != 70 || actual.MaxAttachments != 20 || status["enabled"] != false {
		t.Fatal("runtime did not apply limits while keeping Web disabled", status)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := httptest.NewRecorder()
	router.ServeHTTP(invalid, httptest.NewRequest(http.MethodPatch, endpoint, strings.NewReader(`{"max-attachments":-1}`)))
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Code != http.StatusUnprocessableEntity || !bytes.Equal(before, after) || h.cfg.GeminiEnterpriseWeb.InputLimits.MaxAttachments != 20 {
		t.Fatal("invalid limit patch changed persisted configuration", invalid.Code, invalid.Body.String())
	}
}
