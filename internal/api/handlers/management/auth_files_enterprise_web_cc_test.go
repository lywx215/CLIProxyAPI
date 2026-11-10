package management

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnterpriseWebCCConfigHotReloadAndExplicitFalse(t *testing.T) {
	h, router := webManagementFixture(t)
	const settings = "/v8/management/config/upstream/gemini-enterprise-web"
	const status = "/v8/management/credentials/gemini-enterprise-web/status"
	for _, value := range []bool{true, false} {
		response := webManagementRequest(t, router, "PATCH", settings, map[string]any{"cookie-cc-polling-enabled": value}, nil)
		if response.Code != 200 {
			t.Fatal("configuration patch rejected", response.Code, response.Body.String())
		}
		actual := webManagementRequest(t, router, "GET", status, nil, nil)
		flag := gjson.Get(actual.Body.String(), "cookie_cc_polling_enabled")
		if !flag.Exists() || flag.Bool() != value || gjson.Get(actual.Body.String(), "enabled").Bool() {
			t.Fatal("unexpected effective configuration", actual.Body.String())
		}
		reloaded, err := config.LoadConfig(h.configFilePath)
		if err != nil || reloaded.GeminiEnterpriseWeb.CookieCCPollingEnabled != value {
			t.Fatal("configuration did not survive a reload", err)
		}
	}
	bad := webManagementRequest(t, router, "PATCH", settings, `{"cookie-cc-polling-enabled":"yes"}`, nil)
	if bad.Code < 400 || h.authManager.WebRuntime().Status()["cookie_cc_polling_enabled"] != false {
		t.Fatal("invalid setting was applied", bad.Code)
	}
}

func TestEnterpriseWebScheduledCCStoreAndFences(t *testing.T) {
	for _, scenario := range []string{"renew", "manual edit", "turn off"} {
		t.Run(scenario, func(t *testing.T) {
			h, router := webManagementFixture(t)
			authDir := h.cfg.AuthDir
			// This fixture ID has a zero-second CC offset. Only the initial rounds
			// run on the real scheduler. Longer periods use the controllable clock.
			const name = "synthetic-cc-13.json"
			const settings = "/v8/management/config/upstream/gemini-enterprise-web"
			const credentials = "/v8/management/credentials/gemini-enterprise-web"
			credential := web.Credential{Revision: 1}
			if credential.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/config") != nil || credential.SetCookies("SID=synthetic-saved; SAPISID=synthetic-signing; __Secure-1PSIDTS=synthetic-ts") != nil {
				t.Fatal("invalid credential fixture")
			}
			registered, err := h.authManager.Register(t.Context(), &auth.Auth{
				ID: name, FileName: name, Provider: web.Provider, Status: auth.StatusActive,
				Attributes: map[string]string{auth.AttributePath: filepath.Join(authDir, name)},
				Metadata:   map[string]any{"type": web.Provider, "enterprise_web": credential.MetadataValue()},
			})
			if err != nil || registered == nil {
				t.Fatal("fixture registration failed", err)
			}
			completed := make(chan bool, 4)
			h.authManager.RegisterExecutor(&scheduledCookieExecutor{
				GeminiEnterpriseWebExecutor: runtimeexecutor.NewGeminiEnterpriseWebExecutor(h.cfg, h.authManager.WebRuntime()),
				completed:                   completed,
			})
			var calls []string
			transport := webManagementTransport(func(req *http.Request) (*http.Response, error) {
				reply := `{}`
				res := &http.Response{StatusCode: 200, Header: make(http.Header)}
				switch {
				case req.URL.Host == "accounts.google.com" && req.URL.Path == "/RotateCookies":
					calls = append(calls, "ts")
				case req.URL.String() == credential.PageURL:
					calls = append(calls, "home")
					reply = `window.WIZ_global_data={"Wybhlc":"synthetic-key"};`
				case strings.HasSuffix(req.URL.Path, "/widgetAdvancedCompleteQuery"):
					calls = append(calls, "cc")
					res.Header.Set("Set-Cookie", "SIDCC=synthetic-cc-value; Domain=.google.com; Path=/; Secure")
				case strings.HasSuffix(req.URL.Path, "/lookupWidgetConfig"):
					calls = append(calls, "lookup")
					reply = webProbeLookup
					if strings.Contains(req.Header.Get("Cookie"), "SIDCC=synthetic-cc-value") {
						switch scenario {
						case "manual edit":
							result := webManagementRequest(t, router, "PATCH", credentials, map[string]any{
								"name": name, "expected_revision": 1, "label": "Concurrent edit",
							}, nil)
							if result.Code != 200 {
								t.Error("concurrent edit failed", result.Code)
							}
						case "turn off":
							result := webManagementRequest(t, router, "PATCH", settings, `{"cookie-cc-polling-enabled":false}`, nil)
							if result.Code != 200 {
								t.Error("concurrent shutdown failed", result.Code)
							}
						}
					}
				default:
					t.Errorf("unexpected network operation %s", req.URL.Path)
					res.StatusCode = 500
				}
				res.Body = io.NopCloser(strings.NewReader(reply))
				return res, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
			h.authManager.StartAutoRefresh(ctx, time.Minute)
			defer h.authManager.StopAutoRefresh()
			result := webManagementRequest(t, router, "PATCH", settings, `{"enabled":true,"cookie-cc-polling-enabled":true}`, nil)
			if result.Code != 200 {
				t.Fatal(result.Code, result.Body.String())
			}
			for _, want := range []bool{true, scenario == "renew"} {
				select {
				case got := <-completed:
					if got != want {
						t.Fatal("unexpected completion state", got, want)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("scheduler did not dispatch the expected cookie check", h.authManager.WebRuntime().Status())
				}
			}
			h.authManager.StopAutoRefresh()
			if got := strings.Join(calls, ","); got != "ts,home,lookup,cc,home,lookup" {
				t.Fatal("unexpected network sequence", got)
			}
			data, err := os.ReadFile(filepath.Join(authDir, name))
			if err != nil {
				t.Fatal(err)
			}
			saved := gjson.GetBytes(data, "enterprise_web")
			renewed := scenario == "renew"
			wantRevision := int64(2)
			if scenario == "turn off" {
				wantRevision = 1
			}
			if saved.Get("revision").Int() != wantRevision || saved.Get("cookie_cc_last_renewed_at").Exists() != renewed ||
				strings.Contains(saved.Get("cookies").String(), "synthetic-cc-value") != renewed || !strings.Contains(saved.Get("cookies").String(), "__Secure-1PSIDTS=synthetic-ts") {
				t.Fatal("the store did not preserve its validated revision")
			}
			list := webManagementRequest(t, router, "GET", "/v8/management/credentials?name="+name, nil, nil)
			summary := gjson.Get(list.Body.String(), "files.0.gemini_enterprise_web")
			if strings.Contains(list.Body.String(), "synthetic-saved") || strings.Contains(list.Body.String(), "synthetic-cc-value") ||
				(summary.Get("cookie_cc_check.status").String() == "renewed") != renewed || summary.Get("revision").Int() != wantRevision {
				t.Fatal("inconsistent or unsafe management summary")
			}
			if renewed {
				restarted := auth.NewManager(h.tokenStore, nil, nil)
				restarted.SetConfig(h.cfg)
				if err = restarted.Load(t.Context()); err != nil {
					t.Fatal("could not reload the validated credential", err)
				}
				loaded, ok := restarted.GetByID(name)
				if !ok {
					t.Fatal("credential missing after restart")
				}
				cc, err := web.CredentialFrom(loaded.Metadata)
				if err != nil || cc.Revision != 2 || cc.LastCCRenewedAt.IsZero() {
					t.Fatal("restart lost the saved revision or timestamp", err)
				}
				checker := runtimeexecutor.NewGeminiEnterpriseWebExecutor(h.cfg, restarted.WebRuntime())
				if !checker.CheckCookie(ctx, loaded) || !checker.CheckCookie(ctx, loaded) ||
					restarted.WebRuntime().CredentialStatus(name, cc)["cookie_cc_check"].(map[string]any)["status"] != "no_change" {
					t.Fatal("restart did not resume both checks against the persisted Cookie")
				}
			}
		})
	}
}
