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

func TestEnterpriseDefaultLoginHintsUseAccountDiscovery(t *testing.T) {
	for _, hint := range []config.GeminiEnterpriseOAuth{
		{ClientID: "fixture-client", EngineID: "optional-engine"},
		{ClientID: "fixture-client", ProjectID: "optional-project"},
	} {
		h := &Handler{cfg: &config.Config{AuthDir: t.TempDir(), OAuth: config.OAuthConfig{Providers: config.OAuthProviders{GeminiEnterprise: &hint}}}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		h.RequestGeminiEnterpriseToken(c)
		var result struct {
			State     string `json:"state"`
			Automatic bool   `json:"automatic_resource_discovery"`
		}
		if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &result) != nil || !result.Automatic {
			t.Fatalf("partial config skipped discovery: %d %s", recorder.Code, recorder.Body.String())
		}
		CancelOAuthSession(result.State)
	}
}

func TestEnterpriseAuthorizationIsSavedBeforeOptionalDiscovery(t *testing.T) {
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
						body = `{"email":"account@example.com","verified_email":true}`
					case "cloudresourcemanager.googleapis.com":
						body = `{"projects":[{"name":"projects/222","state":"ACTIVE"}]}`
					case "discoveryengine.googleapis.com", "us-discoveryengine.googleapis.com", "eu-discoveryengine.googleapis.com":
						if scenario == "cancelled" {
							CancelOAuthSession(state)
						}
						body = `{}`
						if scenario == "success" && strings.Contains(req.URL.Path, "/locations/global/") {
							parent := "projects/222/locations/global/collections/default_collection"
							switch {
							case strings.HasSuffix(req.URL.Path, "/collections"):
								body = `{"collections":[{"name":"` + parent + `"}]}`
							case strings.HasSuffix(req.URL.Path, "/engines"):
								body = `{"engines":[{"name":"` + parent + `/engines/manual-engine"}]}`
							case strings.HasSuffix(req.URL.Path, "/assistants"):
								body = `{"assistants":[{"name":"` + parent + `/engines/manual-engine/assistants/custom-assistant"}]}`
							}
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
			if len(files) != 1 {
				t.Fatal("authorization was not retained independently of discovery")
			}
			data, _ := os.ReadFile(filepath.Join(h.cfg.AuthDir, files[0].Name()))
			var record map[string]any
			if json.Unmarshal(data, &record) != nil || ge.String(record, "refresh_token") != "synthetic-refresh" {
				t.Fatal("saved authorization missing")
			}
			graph, errGraph := ge.ReadAccount(record)
			if errGraph != nil || graph == nil {
				t.Fatal("shared account graph missing")
			}
			wantCollections := 0
			if scenario == "success" {
				wantCollections = 1
			}
			if len(graph.Collections) != wantCollections {
				t.Fatalf("incorrect discovery graph: %+v", graph)
			}
			if scenario == "success" && (graph.Collections[0].ProjectID != "222" || graph.Collections[0].AssistantID != "custom-assistant") {
				t.Fatal("discovered application or assistant missing")
			}
			if scenario == "cancelled" {
				return
			}
			_, _, _, _, completed, ok := GetOAuthSessionDetails(state)
			if !ok || !completed {
				t.Fatal("resource completion not reported after persistence")
			}
		})
	}
}
