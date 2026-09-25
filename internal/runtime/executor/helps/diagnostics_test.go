package helps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type diagnosticRT func(*http.Request) (*http.Response, error)

func (f diagnosticRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDiagnosticsFinalClientBranchesAndUsage(t *testing.T) {
	var mu sync.Mutex
	var headers []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		_, _ = w.Write([]byte("unchanged-response"))
	}))
	defer server.Close()
	peers := fmt.Sprintf(`[{"alias":"fixture","origin":%q,"pathPrefix":"/v1","service":"gcli2api","deploymentId":null}]`, server.URL)
	for _, builder := range []struct {
		name string
		new  func(context.Context, *config.Config, *cliproxyauth.Auth, time.Duration) *http.Client
	}{
		{"proxy", NewProxyAwareHTTPClient}, {"devin", NewDevinHTTPClient}, {"utls", NewUtlsHTTPClient},
	} {
		for _, branch := range []string{"default", "context_transport", "context_custom", "auth_proxy", "config_proxy", "request_proxy", "invalid_proxy_fallback"} {
			t.Run(builder.name+"/"+branch, func(t *testing.T) {
				var lines bytes.Buffer
				e := diagnostics.NewEngine(diagnostics.ResourceConfig{}, peers, nil, nil, func(b []byte) error { _, err := lines.Write(b); return err })
				ctx, _ := e.StartServer(context.Background(), nil, "local-1")
				ctx = cliproxyexecutor.WithUpstreamAttemptTracker(ctx)
				cfg := &config.Config{}
				auth := &cliproxyauth.Auth{}
				transport := http.DefaultTransport.(*http.Transport).Clone()
				defer transport.CloseIdleConnections()
				switch branch {
				case "context_transport":
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", transport)
				case "context_custom":
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", diagnosticRT(func(req *http.Request) (*http.Response, error) { return transport.RoundTrip(req) }))
				case "auth_proxy":
					auth.ProxyURL = server.URL
				case "config_proxy":
					cfg.ProxyURL = server.URL
				case "request_proxy":
					ctx = cliproxyexecutor.WithRequestProxyURL(ctx, server.URL)
				case "invalid_proxy_fallback":
					cfg.ProxyURL = "invalid://fixture"
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", transport)
				}
				client := builder.new(ctx, cfg, auth, 0)
				if client.Timeout != 0 || client.CheckRedirect != nil {
					t.Fatal("client policy changed")
				}
				if branch == "context_transport" && transport.DisableCompression {
					t.Fatal("cached/context transport mutated")
				}
				reporter := NewUsageReporter(ctx, "synthetic", "synthetic", nil)
				client = reporter.TrackHTTPClient(client)
				ireq, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1", strings.NewReader("unchanged-request"))
				resp, err := client.Do(ireq)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(lines.Bytes(), []byte(`"event":"diag.call"`)) {
					t.Fatal("settled before body")
				}
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if string(body) != "unchanged-response" || bytes.Count(lines.Bytes(), []byte(`"event":"diag.call"`)) != 1 {
					t.Fatal("response/call behavior")
				}
				if !cliproxyexecutor.UpstreamAttempted(ctx) || !reporter.IsTTFTSet() {
					t.Fatal("usage transport observations lost")
				}
				if !bytes.Contains(lines.Bytes(), []byte(`"callKind":"model"`)) {
					t.Fatal("model ownership label")
				}
				mu.Lock()
				got := headers[len(headers)-1]
				mu.Unlock()
				if got.Get("X-Diag-Request-Id") != "local-1" || len(got.Values("Traceparent")) != 1 {
					t.Fatal("branch was not decorated")
				}
				if builder.name == "devin" && got.Get("Accept-Encoding") == "gzip" {
					t.Fatal("Devin compression behavior changed")
				}
				if ireq.Header.Get("Traceparent") != "" {
					t.Fatal("ireq changed")
				}
			})
		}
	}
}
