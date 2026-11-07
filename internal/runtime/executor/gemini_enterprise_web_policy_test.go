package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseWebMediaUsesPersistedCookieRotationAndKeepsAuthCache(t *testing.T) {
	m, _, _, ctx, f := phaseOneManager(t)
	original := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet && !strings.Contains(req.Header.Get("Cookie"), "SIDCC=synthetic-rotated") {
			t.Fatal("media request did not use the verified rotation")
		}
		resp, err := original.RoundTrip(req)
		if err == nil && req.Method == http.MethodGet {
			resp.Header.Add("Set-Cookie", "SIDCC=synthetic-rotated; Domain=.google.com; Path=/; Secure; Max-Age=3600")
		}
		return resp, err
	}))
	for i := 0; i < 2; i++ {
		if _, err := m.Execute(ctx, []string{web.Provider}, phaseOneRequest("text/plain"), phaseOneOptions()); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := m.GetByID("web-phase-one-cookie")
	c, err := web.CredentialFrom(a.Metadata)
	if err != nil || c.Revision != 2 || f.calls["lookupWidgetConfig"] != 1 || f.calls["widgetStreamAssist"] != 2 {
		t.Fatal("rotation did not survive the next request or caused extra authentication", c.Revision, f.calls, err)
	}
	status := m.WebRuntime().CredentialStatus(a.ID, c)
	if !status["auth_verified"].(bool) || len(status["tested_model_ids"].([]string)) != 1 {
		t.Fatal("rotation lost verified authentication or successful model evidence")
	}
}

func TestEnterpriseWebExecutorRejectsPendingAndWrongPolicyWithoutAuthentication(t *testing.T) {
	for _, policy := range []string{"", web.PolicyArgonOnly} {
		e, a, ctx, f := phaseOneExecutor(t, nil)
		c, _ := web.CredentialFrom(a.Metadata)
		c.ModelPolicy = policy
		a.Metadata["enterprise_web"] = c.MetadataValue()
		_, err := e.Execute(ctx, a, phaseOneRequest("text/plain"), phaseOneOptions())
		if err == nil || len(f.calls) != 0 || e.runtime.Status()["active_requests"] != 0 {
			t.Fatal("policy failure acquired credentials", policy, err, f.calls, e.runtime.Status())
		}
	}
}

func TestEnterpriseWebPolicyChangesFenceUploadsButNotSubmittedGeneration(t *testing.T) {
	testEnterpriseWebCredentialChangesFenceGeneration(t, false, false)
}
func TestEnterpriseWebSerializedPolicyChangesFenceUploadsButNotSubmittedGeneration(t *testing.T) {
	testEnterpriseWebCredentialChangesFenceGeneration(t, true, false)
}

func TestEnterpriseWebSerializedCookieReplacementIsolatesLateResults(t *testing.T) {
	testEnterpriseWebCredentialChangesFenceGeneration(t, true, true)
}

func testEnterpriseWebCredentialChangesFenceGeneration(t *testing.T, serialized, replaceCookie bool) {
	for _, boundary := range []string{"authentication", "upload", "submitted"} {
		t.Run(boundary, func(t *testing.T) {
			m, _, e, ctx, f := phaseOneManager(t)
			if serialized {
				cfg := *e.cfg
				cfg.GeminiEnterpriseWeb.HistoryMode = "serialized-context"
				m.SetConfig(&cfg)
			}
			id := "web-phase-one-cookie"
			a, _ := m.GetByID(id)
			before, _ := web.CredentialFrom(a.Metadata)
			epoch := m.WebRuntime().CredentialEpoch(id)
			entered, release := make(chan struct{}), make(chan struct{})
			original := ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(req *http.Request) (*http.Response, error) {
				resp, err := original.RoundTrip(req)
				pause := (boundary == "authentication" && strings.HasSuffix(req.URL.Path, "lookupWidgetConfig")) || (boundary == "upload" && strings.HasSuffix(req.URL.Path, ":uploadFile") && req.Header.Get("X-Goog-Upload-Command") != "start") || (boundary == "submitted" && strings.HasSuffix(req.URL.Path, "widgetStreamAssist"))
				if pause {
					close(entered)
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				return resp, err
			}))
			completed := make(chan error, 1)
			go func() {
				_, err := m.Execute(ctx, []string{"gemini-enterprise"}, historyTestRequest(phaseOneRequest("text/plain"), serialized), phaseOneOptions())
				completed <- err
			}()
			select {
			case <-entered:
			case err := <-completed:
				t.Fatal("request did not reach controlled boundary", err)
			}
			updated, err := m.UpdateEnterpriseWebCredential(context.Background(), id, before.Revision, func(_ *auth.Auth, c *web.Credential) error {
				if replaceCookie {
					return c.SetCookies("SID=synthetic-replacement; SAPISID=synthetic-new-signing")
				}
				c.ModelPolicy = web.PolicyArgonOnly
				return nil
			})
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			after, _ := web.CredentialFrom(updated.Metadata)
			if replaceCookie && (after.Fingerprint() == before.Fingerprint() || m.WebRuntime().CredentialEpoch(id) == epoch) {
				close(release)
				t.Fatal("replacement did not invalidate the old credential")
			}
			if (!replaceCookie && (after.Fingerprint() != before.Fingerprint() || len(after.Catalog()) != len(before.Catalog()) || m.WebRuntime().CredentialEpoch(id) != epoch)) || m.WebRuntime().Status()["active_requests"] != 1 {
				close(release)
				t.Fatal("policy edit lost auth/catalog or lease")
			}
			close(release)
			err = <-completed
			if boundary == "submitted" {
				if err != nil || f.calls["widgetStreamAssist"] != 1 {
					t.Fatal("submitted request was interrupted", err, f.calls)
				}
			} else {
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != 409 || f.calls["widgetStreamAssist"] != 0 {
					t.Fatal("changed policy generated", err, f.calls)
				}
			}
			if boundary == "authentication" && f.calls["uploadFile"] != 0 {
				t.Fatal("policy changed before upload", f.calls)
			}
			if m.WebRuntime().Status()["active_requests"] != 0 || m.WebRuntime().Status()["inflight_bytes"] != int64(0) {
				t.Fatal("policy rejection leaked resource", m.WebRuntime().Status())
			}
			m.WebRuntime().RecordModel(id, a.Metadata, "late", epoch)
			if m.WebRuntime().CredentialStatus(id, after)["auth_verified"].(bool) == replaceCookie {
				t.Fatal("credential change retained the wrong authentication state")
			}
			if len(m.WebRuntime().CredentialStatus(id, after)["tested_model_ids"].([]string)) != 0 {
				t.Fatal("old result acquired new classification evidence")
			}
		})
	}
}
