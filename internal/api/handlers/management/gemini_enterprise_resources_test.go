package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

func TestEnterpriseSessionResourceInputsWithoutGlobalBinding(t *testing.T) {
	h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
	for _, suffix := range []string{
		"&auto_discover_resources=true&engine_id=manual-engine&assistant_id=custom-assistant",
		"&auto_discover_resources=false&engine_id=manual-engine&project_id=manual-project",
	} {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodGet, "/?client_id=session.apps.googleusercontent.com"+suffix, nil)
		h.RequestGeminiEnterpriseToken(c)
		if response.Code != 200 {
			t.Fatalf("session inputs rejected: %d %s", response.Code, response.Body.String())
		}
		var start struct {
			State     string `json:"state"`
			Supported bool   `json:"resource_inputs_supported"`
		}
		if json.Unmarshal(response.Body.Bytes(), &start) != nil || !start.Supported {
			t.Fatal("resource input capability missing")
		}
		CancelOAuthSession(start.State)
	}
	if h.cfg.OAuth.Providers.GeminiEnterprise != nil {
		t.Fatal("login inputs changed global provider configuration")
	}
}

func TestEnterpriseDiscoveredBindingSavedOnlyAfterSuccessfulResolution(t *testing.T) {
	for _, scenario := range []string{"success", "missing", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			binding := config.GeminiEnterpriseOAuth{ClientID: "session-app", EngineID: "manual-engine", AssistantID: "custom-assistant"}.WithDefaults()
			state := "enterprise-auto-" + scenario
			RegisterOAuthSession(state, ge.Provider)
			defer CancelOAuthSession(state)
			h := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}, tokenStore: sdkAuth.NewFileTokenStore()}
			original := newGeminiEnterpriseOAuthClient
			t.Cleanup(func() { newGeminiEnterpriseOAuthClient = original })
			newGeminiEnterpriseOAuthClient = func(string) (*http.Client, error) {
				return &http.Client{Transport: enterpriseTestTransport(func(req *http.Request) (*http.Response, error) {
					status, body := 200, ""
					switch req.URL.Host {
					case "oauth2.googleapis.com":
						body = `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"token_type":"Bearer"}`
					case "www.googleapis.com":
						body = `{"email":"account@example.com"}`
					case "cloudresourcemanager.googleapis.com":
						body = `{"projects":[{"name":"projects/222","state":"ACTIVE"}]}`
					case "discoveryengine.googleapis.com", "us-discoveryengine.googleapis.com", "eu-discoveryengine.googleapis.com":
						if scenario == "cancelled" {
							CancelOAuthSession(state)
						}
						if scenario == "missing" || !strings.Contains(req.URL.Path, "/locations/global/") {
							status, body = 404, `{}`
						} else {
							body = `{"name":"projects/222/locations/global/collections/default_collection/engines/manual-engine"}`
						}
					default:
						t.Fatalf("unexpected credential destination: %s", req.URL.Host)
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}, nil
			}
			oauth, err := ge.ApplicationOAuth(binding, geminiEnterpriseCallbackURL)
			if err != nil {
				t.Fatal(err)
			}
			h.finishGeminiEnterpriseLoginWithDiscovery(context.Background(), state, binding, oauth, "verifier", "code", "", true)
			files, err := os.ReadDir(h.cfg.AuthDir)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "success" {
				if len(files) != 0 {
					t.Fatal("unresolved or cancelled credentials were persisted")
				}
				return
			}
			if len(files) != 1 {
				t.Fatal("resolved credentials missing")
			}
			data, _ := os.ReadFile(filepath.Join(h.cfg.AuthDir, files[0].Name()))
			var record map[string]any
			if json.Unmarshal(data, &record) != nil || record["project_id"] != "222" || record["location"] != "global" || record["collection_id"] != "default_collection" || record["assistant_id"] != "custom-assistant" {
				t.Fatal("saved account did not retain discovered resources and manual assistant")
			}
			_, _, _, _, completed, ok := GetOAuthSessionDetails(state)
			if !ok || !completed {
				t.Fatal("resource completion not reported after persistence")
			}
		})
	}
}
