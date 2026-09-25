package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAntigravityDiagnosticsFinalBranches(t *testing.T) {
	for _, branch := range []string{"default", "proxy", "invalid_proxy", "context_transport", "custom_transport", "typed_nil"} {
		t.Run(branch, func(t *testing.T) {
			var got http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				if r.ProtoMajor != 1 {
					t.Error("Antigravity HTTP/1.1 lost")
				}
				_, _ = w.Write([]byte("body"))
			}))
			defer server.Close()
			var lines bytes.Buffer
			e := diagnostics.NewEngine(diagnostics.ResourceConfig{}, fmt.Sprintf(`[{"alias":"fixture","origin":%q,"pathPrefix":"/v1","service":"gcli2api","deploymentId":null}]`, server.URL), nil, nil, func(b []byte) error { _, err := lines.Write(b); return err })
			ctx, _ := e.StartServer(context.Background(), nil, "local-1")
			cfg := &config.Config{}
			auth := &cliproxyauth.Auth{ID: "diagnostics-" + branch}
			base := http.DefaultTransport.(*http.Transport).Clone()
			defer base.CloseIdleConnections()
			switch branch {
			case "proxy":
				auth.ProxyURL = server.URL
			case "invalid_proxy":
				auth.ProxyURL = "invalid://fixture"
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", base)
			case "context_transport":
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", base)
			case "typed_nil":
				var tr *http.Transport
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", tr)
			case "custom_transport":
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", diagAntigravityRT{base})
			}
			client := newAntigravityHTTPClient(ctx, cfg, auth, 0)
			req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if got.Get("X-Diag-Request-Id") != "local-1" || bytes.Count(lines.Bytes(), []byte(`"event":"diag.call"`)) != 1 {
				t.Fatal("final transport not observed exactly once")
			}
		})
	}
}

type diagAntigravityRT struct{ base http.RoundTripper }

func (t diagAntigravityRT) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r)
}
