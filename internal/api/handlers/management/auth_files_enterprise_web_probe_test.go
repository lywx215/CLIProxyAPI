package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/tidwall/gjson"
)

const webProbeLookup = `{"engineResourceDetails":{"projectNumber":"123","location":"ca","collectionId":"default_collection","engineId":"synthetic"},"anonymousWidgetConfig":{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"gemini-3.8-flash","displayName":"Gemini Flash"}]}}}}`

func TestEnterpriseWebModelProbeUsesMediaAndGlobalAlias(t *testing.T) {
	for _, answer := range []string{"correct", "text_only", "wrong_colors"} {
		t.Run(answer, func(t *testing.T) {
			h, r := webManagementFixture(t)
			name := addWebManagementCredential(t, r)
			a, _ := h.authManager.GetByID(name)
			credential, _ := web.CredentialFrom(a.Metadata)
			credential.SetCatalog([]web.AvailableModel{{ID: "gemini-3.8-flash", DisplayName: "Gemini Flash"}}, time.Now())
			a.Metadata["enterprise_web"] = credential.MetadataValue()
			if _, err := h.authManager.Update(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			h.cfg.GeminiEnterpriseWeb.Enabled = true
			h.authManager.SetConfig(h.cfg)
			h.authManager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{web.Provider: {{Name: "gemini-3.8-flash", Alias: "public-web", Fork: true}}})
			registry.GetGlobalRegistry().RegisterClient(a.ID, web.Provider, []*registry.ModelInfo{{ID: "public-web", MetadataModelID: "gemini-3.8-flash", Type: "gemini", OwnedBy: web.Provider}})
			defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
			var code string
			uploads, generations, sessions := 0, 0, 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", webManagementTransport(func(req *http.Request) (*http.Response, error) {
				response := func(raw string) *http.Response {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}
				}
				if req.Method == http.MethodGet {
					return response(`window.WIZ_global_data={"Wybhlc":"synthetic-key"};`), nil
				}
				if strings.HasSuffix(req.URL.Path, "lookupWidgetConfig") {
					return response(webProbeLookup), nil
				}
				if strings.HasSuffix(req.URL.Path, "widgetCreateSession") {
					sessions++
					return response(`{"session":{"name":"collections/default_collection/engines/synthetic/sessions/probe"}}`), nil
				}
				if strings.HasSuffix(req.URL.Path, ":uploadFile") {
					if req.Header.Get("X-Goog-Upload-Command") == "start" {
						res := response(`{}`)
						res.Header.Set("X-Goog-Upload-URL", req.URL.String()+"?upload_id=synthetic")
						return res, nil
					}
					b, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					uploads++
					if req.Header.Get("Content-Type") == "text/plain" {
						code = strings.TrimSpace(strings.TrimPrefix(string(b), "verification_code: "))
					} else {
						img, errImage := png.Decode(bytes.NewReader(b))
						if errImage != nil || img.Bounds().Dx() != 180 {
							t.Fatal("test did not upload its image", errImage)
						}
					}
					return response(fmt.Sprintf(`{"fileId":"file-%d"}`, uploads)), nil
				}
				if strings.HasSuffix(req.URL.Path, "widgetStreamAssist") {
					generations++
					b, _ := io.ReadAll(req.Body)
					if uploads != 2 || code == "" || gjson.GetBytes(b, "streamAssistRequest.assistGenerationConfig.modelId").String() != "gemini-3.8-flash" || strings.Contains(gjson.GetBytes(b, "streamAssistRequest.query.parts.0.text").String(), code) {
						t.Fatal("probe lost its binding or disclosed expected answers")
					}
					text := code + "|red|blue|green"
					if answer == "text_only" {
						text = "OK"
					}
					if answer == "wrong_colors" {
						text = code + "|blue|red|green"
					}
					body, _ := json.Marshal([]any{map[string]any{"streamAssistResponse": map[string]any{"answer": map[string]any{"state": "SUCCEEDED", "replies": []any{map[string]any{"groundedContent": map[string]any{"content": map[string]string{"text": text}}}}}}}})
					return response(string(body)), nil
				}
				t.Fatal("unexpected probe operation", req.URL.Path)
				return nil, nil
			}))
			request := map[string]any{"name": name, "auth_index": lockedAuthIndex(a), "model": "public-web", "expected_upstream_model": "gemini-3.8-flash", "expected_revision": credential.Revision}
			result := webManagementRequest(t, r, "POST", "/v8/management/credentials/gemini-enterprise-web/models/test", request, ctx)
			want := "failed"
			if answer == "correct" {
				want = "ok"
			}
			if result.Code != 200 || gjson.Get(result.Body.String(), "status").String() != want || gjson.Get(result.Body.String(), "upstream_model").String() != "gemini-3.8-flash" || uploads != 2 || generations != 1 || sessions != 1 {
				t.Fatal(result.Code, result.Body.String(), uploads, generations, sessions)
			}
			if h.authManager.WebRuntime().Status()["active_requests"] != 0 {
				t.Fatal("probe leaked admission")
			}
			request["expected_upstream_model"] = "another-model"
			changed := webManagementRequest(t, r, "POST", "/v8/management/credentials/gemini-enterprise-web/models/test", request, ctx)
			if changed.Code != 409 || generations != 1 {
				t.Fatal("stale alias snapshot generated content", changed.Body.String())
			}
			request["expected_upstream_model"] = "gemini-3.8-flash"
			request["expected_revision"] = 99
			stale := webManagementRequest(t, r, "POST", "/v8/management/credentials/gemini-enterprise-web/models/test", request, ctx)
			if stale.Code != 409 || generations != 1 {
				t.Fatal("stale credential generated content")
			}
			request["expected_revision"] = credential.Revision
			h.authManager.WebRuntime().Configure(config.GeminiEnterpriseWebConfig{Enabled: false})
			disabled := webManagementRequest(t, r, "POST", "/v8/management/credentials/gemini-enterprise-web/models/test", request, ctx)
			if disabled.Code != 409 || generations != 1 {
				t.Fatal("disabled channel generated content")
			}
			models := webManagementRequest(t, r, "GET", "/v8/management/credentials/models?name="+name, nil, nil)
			if models.Code != 200 || gjson.Get(models.Body.String(), "models.0.upstream_model").String() != "gemini-3.8-flash" {
				t.Fatal(models.Body.String())
			}
			definitions := webManagementRequest(t, r, "GET", "/v8/management/model-definitions/gemini-enterprise-web", nil, nil)
			if definitions.Code != 200 || gjson.Get(definitions.Body.String(), "models.0.id").String() != "gemini-3.8-flash" || generations != 1 {
				t.Fatal("available model enumeration generated content", definitions.Body.String())
			}
		})
	}
}
