package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

func TestEnterpriseResourceEditorValidatesAndPreservesCredential(t *testing.T) {
	previous := newGeminiEnterpriseResourceClient
	t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
	for _, scenario := range []string{"us", "eu", "global", "ca", "au", "in", "forbidden", "missing", "catalog-failure", "inconsistent", "redirect", "conflict", "concurrent-note", "invalid-region", "invalid-id", "wrong-index", "home"} {
		t.Run(scenario, func(t *testing.T) {
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(t.TempDir())
			manager := coreauth.NewManager(store, nil, nil)
			binding := (config.GeminiEnterpriseOAuth{ClientID: "fixture-client", ProjectID: "123", Location: "global", EngineID: "original"}).WithDefaults()
			metadata := ge.Metadata(binding, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})
			metadata["note"] = "preserve-note"
			auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "regional.json", FileName: "regional.json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			resources := ge.Resources(binding)
			resources.Location = "us"
			resources.EngineID = "target"
			if ge.SupportedLocation(scenario) {
				resources.Location = scenario
			}
			if scenario == "invalid-region" {
				resources.Location = "es"
			}
			if scenario == "invalid-id" {
				resources.EngineID = "../other"
			}
			proposed := resources.Apply(binding)
			calls, hooks := 0, 0
			newGeminiEnterpriseResourceClient = func(string) (*http.Client, error) {
				return &http.Client{Transport: enterpriseTestTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if !ge.OfficialBinding(req, proposed.ProjectID, proposed.Location) || req.Header.Get("Authorization") != "Bearer synthetic-access" {
						t.Fatal("credential sent to wrong regional resource", req.URL)
					}
					if req.Method != http.MethodGet {
						t.Fatal("validation performed a cloud mutation or generation")
					}
					status, body := 200, `{"name":"`+ge.Engine(proposed)+`"}`
					if calls == 1 {
						switch scenario {
						case "forbidden":
							status = 403
						case "missing":
							status = 404
						case "inconsistent":
							body = `{"name":"projects/999/locations/us/collections/default_collection/engines/target"}`
						case "redirect":
							status = 302
						}
					} else {
						if !strings.Contains(req.URL.Path, "/widgetConfigs/") {
							t.Fatal("catalog was not validated")
						}
						body = `{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"regional-only"}]}}}`
						if scenario == "catalog-failure" {
							status = 403
						}
						if scenario == "conflict" || scenario == "concurrent-note" {
							current, _ := manager.GetByID(auth.ID)
							if scenario == "conflict" {
								current.Metadata["engine_id"] = "concurrent-app"
							} else {
								current.Metadata["note"] = "new-note"
								current.Metadata["access_token"] = "rotated-access"
							}
							if _, errUpdate := manager.Update(context.Background(), current); errUpdate != nil {
								t.Fatal(errUpdate)
							}
						}
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://evil.test"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}, nil
			}
			h := &Handler{cfg: &config.Config{}, authManager: manager, postAuthPersistHook: func(_ context.Context, updated *coreauth.Auth) error {
				hooks++
				if ge.Resources(ge.FromMetadata(updated.Metadata)) != resources {
					t.Fatal("runtime hook got stale resources")
				}
				return nil
			}}
			if scenario == "home" {
				h.cfg.Home.Enabled = true
			}
			index := auth.Index
			if scenario == "wrong-index" {
				index = "wrong"
			}
			payload, _ := json.Marshal(map[string]any{"name": auth.FileName, "auth_index": index, "expected": ge.Resources(binding), "resources": resources})
			router := gin.New()
			router.PATCH("/resources", h.PatchGeminiEnterpriseResources)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/resources", strings.NewReader(string(payload))))
			success := ge.SupportedLocation(scenario) || scenario == "concurrent-note"
			if (response.Code == 200) != success {
				t.Fatalf("response %d: %s", response.Code, response.Body)
			}
			if strings.Contains(response.Body.String(), "synthetic-access") || strings.Contains(response.Body.String(), "synthetic-refresh") {
				t.Fatal("response leaked credentials")
			}
			after, _ := manager.GetByID(auth.ID)
			if success {
				if calls != 2 || hooks != 1 || ge.Resources(ge.FromMetadata(after.Metadata)) != resources || ge.String(after.Metadata, "refresh_token") != "synthetic-refresh" {
					t.Fatal("validated save did not retain credential or invoke runtime sync")
				}
				wantNote := "preserve-note"
				if scenario == "concurrent-note" {
					wantNote = "new-note"
				}
				if ge.String(after.Metadata, "note") != wantNote {
					t.Fatal("overwrote concurrent metadata")
				}
				stored, errList := store.List(context.Background())
				if errList != nil || len(stored) != 1 || ge.Resources(ge.FromMetadata(stored[0].Metadata)) != resources {
					t.Fatal("resources not persisted", errList)
				}
			} else {
				if hooks != 0 {
					t.Fatal("invalid resources reached runtime hook")
				}
				want := ge.Resources(binding)
				if scenario == "conflict" {
					want.EngineID = "concurrent-app"
				}
				if ge.Resources(ge.FromMetadata(after.Metadata)) != want {
					t.Fatal("failure changed saved binding")
				}
			}
		})
	}
}
