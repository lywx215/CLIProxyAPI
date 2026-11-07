package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type scheduledCookieExecutor struct {
	*runtimeexecutor.GeminiEnterpriseWebExecutor
	completed chan bool
}

func (e *scheduledCookieExecutor) CheckCookie(ctx context.Context, a *auth.Auth) bool {
	checked := e.GeminiEnterpriseWebExecutor.CheckCookie(ctx, a)
	e.completed <- checked
	return checked
}

func TestEnterpriseWebScheduledCookieCheck(t *testing.T) {
	for _, tc := range []struct {
		name, status, redirect    string
		rotate, bootstrap, lookup int
		calls, revision           int64
		oldUsable, noNewCookie    bool
	}{
		{"rotation", "verified", "", 200, 200, 200, 3, 2, false, false},
		{"no new cookie", "verified", "", 200, 200, 200, 3, 1, false, true},
		{"rotation rejected", "renewal_degraded", "", 401, 200, 200, 3, 1, true, false},
		{"stored session also rejected", "relogin_required", "", 401, 200, 401, 3, 1, false, false},
		{"rotation unavailable", "check_failed", "", 503, 200, 200, 1, 1, false, false},
		{"new session rejected", "relogin_required", "https://accounts.google.com/ServiceLogin", 200, 302, 200, 3, 1, false, false},
		{"other redirect", "check_failed", "https://example.com/ServiceLogin", 200, 302, 200, 2, 1, false, false},
		{"bootstrap unavailable", "check_failed", "", 200, 503, 200, 2, 1, false, false},
		{"directory rejects rotated session", "relogin_required", "", 200, 200, 401, 6, 1, false, false},
		{"directory accepts stored session", "renewal_degraded", "", 200, 200, 401, 6, 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, router := webManagementFixture(t)
			name := addWebManagementCredential(t, router)
			completed := make(chan bool, 1)
			h.authManager.RegisterExecutor(&scheduledCookieExecutor{
				GeminiEnterpriseWebExecutor: runtimeexecutor.NewGeminiEnterpriseWebExecutor(h.cfg, h.authManager.WebRuntime()),
				completed:                   completed,
			})
			var calls int64
			var recovering atomic.Bool
			transport := webManagementTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				reply := webProbeLookup
				res := &http.Response{StatusCode: 200, Header: make(http.Header), Request: req}
				if req.Method == http.MethodPost && req.URL.String() == "https://accounts.google.com/RotateCookies" {
					if req.Header.Get("Origin") != "https://accounts.google.com" || req.Header.Get("Content-Type") != "application/json" {
						t.Error("invalid rotation headers")
					}
					if !recovering.Load() {
						res.StatusCode = tc.rotate
					}
					if !tc.noNewCookie || recovering.Load() {
						res.Header.Add("Set-Cookie", "__Secure-1PSIDTS=synthetic-ts-first; Domain=.google.com; Path=/; Secure; Max-Age=3600")
						res.Header.Add("Set-Cookie", "__Secure-3PSIDTS=synthetic-ts-third; Domain=.google.com; Path=/; Secure; Max-Age=3600")
					}
					res.Header.Add("Set-Cookie", "SID=synthetic-wrong-account; Domain=.google.com; Path=/; Secure")
					reply = `{}`
				} else if req.Method == http.MethodGet && req.URL.Host == "vertexaisearch.cloud.google.com" {
					if !recovering.Load() {
						res.StatusCode = tc.bootstrap
						res.Header.Set("Location", tc.redirect)
					}
					res.Header.Set("Set-Cookie", "SIDCC=synthetic-scheduled; Domain=.google.com; Path=/; Secure; Max-Age=3600")
					reply = `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`
				} else if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "lookupWidgetConfig") {
					t.Errorf("scheduled check used unexpected upstream method or path: %s %s", req.Method, req.URL.Path)
					res.StatusCode = 500
				} else if !recovering.Load() {
					res.StatusCode = tc.lookup
					if tc.oldUsable && !strings.Contains(req.Header.Get("Cookie"), "synthetic-ts-first") {
						res.StatusCode = 200
					}
				}
				if req.URL.Host != "accounts.google.com" {
					cookie := req.Header.Get("Cookie")
					if strings.Contains(cookie, "synthetic-ts-first") != strings.Contains(cookie, "synthetic-ts-third") || strings.Contains(cookie, "synthetic-wrong-account") ||
						((tc.noNewCookie || tc.rotate == 401) && !recovering.Load() && strings.Contains(cookie, "synthetic-ts-first")) {
						t.Error("candidate authentication did not use just the rotated PSIDTS and original identity")
					}
				}
				res.Body = io.NopCloser(strings.NewReader(reply))
				return res, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			h.authManager.StartAutoRefresh(ctx, time.Minute)
			defer h.authManager.StopAutoRefresh()
			cfg := h.cfg.CloneForRuntime()
			cfg.GeminiEnterpriseWeb.Enabled = true
			h.authManager.SetConfig(cfg)
			select {
			case checked := <-completed:
				if !checked {
					t.Fatal("scheduled check was skipped")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("enabling the channel did not schedule a check")
			}
			list := webManagementRequest(t, router, "GET", "/v8/management/credentials", nil, nil)
			summary := gjson.Get(list.Body.String(), "files.0.gemini_enterprise_web")
			if calls != tc.calls || summary.Get("cookie_check_status").String() != tc.status || !summary.Get("cookie_checked_at").Exists() {
				t.Fatalf("check result %q, upstream calls %d", summary.Get("cookie_check_status").String(), calls)
			}
			if summary.Get("auth_verified").Bool() != (tc.status == "verified" || tc.status == "renewal_degraded") || strings.Contains(list.Body.String(), "synthetic-scheduled") || strings.Contains(list.Body.String(), "synthetic-ts") {
				t.Fatal("unsafe or inconsistent credential summary")
			}
			raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, name))
			if err != nil || gjson.GetBytes(raw, "enterprise_web.revision").Int() != tc.revision {
				t.Fatalf("unexpected stored revision: %d, %v", gjson.GetBytes(raw, "enterprise_web.revision").Int(), err)
			}
			if strings.Contains(gjson.GetBytes(raw, "enterprise_web.cookies").String(), "synthetic-ts-first") != (tc.revision == 2) ||
				summary.Get("cookie_last_renewed_at").Exists() != (tc.revision == 2) ||
				summary.Get("cookie_next_check_at").Exists() != (tc.status != "relogin_required") {
				t.Fatal("rotation persistence did not match the verification outcome")
			}
			if tc.status == "relogin_required" {
				recovering.Store(true)
				replacement := webManagementRequest(t, router, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{
					"name": name, "expected_revision": 1, "cookies": "SID=synthetic-replacement; SAPISID=synthetic-signing",
				}, nil)
				if replacement.Code != 200 {
					t.Fatal("replacement failed", replacement.Code)
				}
				select {
				case <-completed:
				case <-time.After(5 * time.Second):
					t.Fatal("replacing a rejected session did not resume scheduled checks")
				}
				updated := webManagementRequest(t, router, "GET", "/v8/management/credentials", nil, nil)
				if calls != tc.calls+3 || gjson.Get(updated.Body.String(), "files.0.gemini_enterprise_web.cookie_check_status").String() != "verified" {
					t.Fatal("replacement did not clear the sign-in prompt")
				}
			}
		})
	}
}

func TestEnterpriseWebCookieRotationVerifiesAndPersistsBeforePublication(t *testing.T) {
	for _, scenario := range []string{"accept", "reject", "manual-edit"} {
		t.Run(scenario, func(t *testing.T) {
			h, router := webManagementFixture(t)
			name := addWebManagementCredential(t, router)
			lookupCalls := 0
			transport := webManagementTransport(func(req *http.Request) (*http.Response, error) {
				res := &http.Response{StatusCode: 200, Header: make(http.Header)}
				reply := webProbeLookup
				if req.Method == http.MethodGet {
					reply = `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`
					res.Header.Add("Set-Cookie", "SIDCC=synthetic-rotation; Domain=.google.com; Path=/; Secure; HttpOnly; Max-Age=3600")
					res.Header.Add("Set-Cookie", "SID=synthetic-wrong-account; Domain=.google.com; Path=/; Secure")
					res.Header.Add("Set-Cookie", "__Secure-1PSIDCC=synthetic-wrong-path; Domain=.google.com; Path=/other; Secure")
					res.Header.Add("Set-Cookie", "__Secure-3PSIDCC=synthetic-wrong-domain; Domain=.example.net; Path=/; Secure")
				} else {
					lookupCalls++
					if !strings.HasSuffix(req.URL.Path, "lookupWidgetConfig") || strings.Contains(req.Header.Get("Cookie"), "synthetic-wrong") {
						t.Fatal("rotation widened Cookie scope or invoked generation")
					}
					if candidate := strings.Contains(req.Header.Get("Cookie"), "SIDCC=synthetic-rotation"); candidate != (lookupCalls == 1) {
						t.Fatal("wrong candidate used for authentication")
					}
					if scenario == "reject" && lookupCalls == 1 {
						res.StatusCode = 401
					}
					if scenario == "manual-edit" {
						w := webManagementRequest(t, router, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{
							"name": name, "expected_revision": 1, "cookies": "SID=synthetic-manual; SAPISID=synthetic-signing",
						}, nil)
						if w.Code != 200 {
							t.Fatal("concurrent manual replacement failed", w.Code)
						}
					}
				}
				res.Body = io.NopCloser(strings.NewReader(reply))
				return res, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			w := webManagementRequest(t, router, "POST", "/v8/management/credentials/refresh", map[string]string{"name": name}, ctx)
			raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, name))
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if json.Unmarshal(raw, &metadata) != nil {
				t.Fatal("stored credential is invalid JSON")
			}
			saved, err := web.CredentialFrom(metadata)
			if err != nil || saved.Policy() != web.PolicyExcludeArgon {
				t.Fatal("rotation changed the credential policy", err)
			}
			if strings.Contains(w.Body.String(), "synthetic-rotation") || strings.Contains(w.Body.String(), "synthetic-signing") {
				t.Fatal("Cookie exposed by verification response")
			}
			switch scenario {
			case "accept":
				if w.Code != 200 || lookupCalls != 1 || saved.Revision != 2 || !strings.Contains(saved.Cookies, "SIDCC=synthetic-rotation") || !gjson.GetBytes(w.Body.Bytes(), "credential.auth_verified").Bool() || len(saved.Catalog()) != 1 {
					t.Fatal("verified rotation was not persisted or cached", w.Code, saved.Revision)
				}
				reloaded := auth.NewManager(h.tokenStore, nil, nil)
				reloaded.SetConfig(h.cfg)
				reloaded.RegisterExecutor(runtimeexecutor.NewGeminiEnterpriseWebExecutor(h.cfg, reloaded.WebRuntime()))
				if err := reloaded.Load(context.Background()); err != nil {
					t.Fatal(err)
				}
				account, ok := reloaded.GetByID(name)
				if !ok {
					t.Fatal("credential did not survive reload")
				}
				reloadedCookie, err := web.CredentialFrom(account.Metadata)
				if err != nil || reloadedCookie.Fingerprint() != saved.Fingerprint() || reloadedCookie.Revision != 2 {
					t.Fatal("reload lost verified rotation", err)
				}
			case "reject":
				if w.Code != 200 || lookupCalls != 2 || saved.Revision != 1 || strings.Contains(saved.Cookies, "SIDCC=") {
					t.Fatal("rejected candidate was saved", w.Code, saved.Revision)
				}
			case "manual-edit":
				if w.Code != 500 || saved.Revision != 2 || !strings.Contains(saved.Cookies, "SID=synthetic-manual") || strings.Contains(saved.Cookies, "SIDCC=") {
					t.Fatal("late renewal overwrote manual credential", w.Code, saved.Revision)
				}
			}
		})
	}
}
