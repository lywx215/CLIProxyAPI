package test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Every upstream operation is either rejected or redirected to this loopback server.
type enterprise502Transport struct {
	target    *url.URL
	transport http.RoundTripper
}

func (f *enterprise502Transport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return f }
func (f *enterprise502Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "discoveryengine.googleapis.com" || req.Method != "POST" || !strings.HasSuffix(req.URL.Path, ":streamAssist") {
		return nil, fmt.Errorf("offline fixture rejects non-generation operations")
	}
	copyReq := req.Clone(req.Context())
	target := *req.URL
	target.Scheme, target.Host = f.target.Scheme, f.target.Host
	copyReq.URL = &target
	copyReq.Host = ""
	return f.transport.RoundTrip(copyReq)
}

func TestEnterprise502ErrorsAndSafeFailoverAtHTTPBoundary(t *testing.T) {
	const success = `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"offline success"}}]}},{"answer":{"state":"SUCCEEDED"}}]`
	const unavailable = `{"error":{"code":14,"status":"UNAVAILABLE","message":"private-upstream-message private-token"}}`
	const partial = `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"partial"}}]}}` + "\n"
	for _, tt := range []struct {
		name                 string
		http                 int
		body                 string
		retry, started, json bool
		want                 int
	}{
		{"http502", 502, `<html>private-gateway-message</html>`, true, false, false, 200},
		{"http503", 503, `{"error":{"code":503,"status":"UNAVAILABLE"}}`, true, false, false, 200},
		{"rpc503", 200, unavailable, true, false, false, 200},
		{"rpc500", 200, `{"error":{"code":13,"status":"INTERNAL"}}`, true, false, false, 200},
		{"rpc504", 200, `{"error":{"code":4,"status":"DEADLINE_EXCEEDED"}}`, true, false, false, 200},
		{"sse503", 200, "event: error\ndata: " + unavailable + "\n\n", true, false, false, 200},
		{"http400", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT"}}`, false, false, false, 400},
		{"rpc400", 200, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`, false, false, false, 400},
		{"rpc404", 200, `{"error":{"code":5,"status":"NOT_FOUND"}}`, false, false, false, 404},
		{"unknown_rpc", 200, `{"error":{"code":999,"message":"private-value"}}`, false, false, false, 502},
		{"empty_completed", 200, `[{"answer":{"state":"SUCCEEDED"}}]`, false, false, false, 502},
		{"json_invalid", 200, `[{"answer":{"state":"SUCCEEDED","replies":[{"content":{"text":"invalid JSON"}}]}}]`, false, false, true, 502},
		{"invalid_stream", 200, `private-malformed-response`, false, false, false, 502},
		{"partial503", 200, partial + unavailable, false, true, false, 503},
		{"truncated", 200, partial, false, true, false, 502},
		{"json_buffered503", 200, partial + unavailable, false, false, true, 503},
		{"hidden_thought503", 200, `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"private-thought"}}]}}` + "\n" + unavailable, false, false, false, 503},
		{"in_progress503", 200, `{"answer":{"state":"IN_PROGRESS"}}` + "\n" + unavailable, false, false, false, 503},
		{"policy", 200, `[{"answer":{"state":"SKIPPED","assistSkippedReasons":["CUSTOMER_POLICY_VIOLATION"],"customerPolicyEnforcementResult":{"verdict":"BLOCK"}}}]`, false, false, false, 400},
	} {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tt.name, protocol, stream), func(t *testing.T) {
					cfg := &config.Config{OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{
						ge.Provider: {{Status: 400, Match: []string{"upstream"}, Action: coreauth.RequestScopedActionContinue}, {Status: 502, Match: []string{"upstream"}, Action: coreauth.RequestScopedActionContinue}, {Status: 503, Match: []string{"upstream"}, Action: coreauth.RequestScopedActionContinue}},
					}}
					router, _, manager := enterpriseFixtureRouterWithConfig(t, cfg)
					// Zero additional rounds; only the existing two-credential failover budget applies.
					manager.SetRetryConfig(0, 0, 2)
					first, _ := manager.GetByID("enterprise-wire-fixture")
					second := first.Clone()
					second.ID = "enterprise-502-second"
					second, err := manager.Register(t.Context(), second)
					if err != nil {
						t.Fatal(err)
					}
					trial := true
					enterprisePublishSubscription(t, manager, second, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()})
					manager.PublishEnterprisePolicyCatalog(second, []ge.Model{{ID: "gemini-3.8-flash"}})
					registry.GetGlobalRegistry().RegisterClient(second.ID, ge.Provider, []*registry.ModelInfo{{ID: "gemini-3.8-flash", Type: "gemini"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(second.ID) })
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						n := calls.Add(1)
						body, _ := io.ReadAll(r.Body)
						if gjson.GetBytes(body, "generationSpec.modelId").String() != "gemini-3.8-flash" {
							t.Error("retry silently changed the upstream model")
						}
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("X-Goog-Request-Id", "00000000-1111-2222-3333-444444444444")
						if n > 1 && tt.retry {
							w.WriteHeader(200)
							_, _ = io.WriteString(w, success)
							return
						}
						w.WriteHeader(tt.http)
						_, _ = io.WriteString(w, tt.body)
					}))
					t.Cleanup(server.Close)
					target, _ := url.Parse(server.URL)
					manager.SetRoundTripperProvider(&enterprise502Transport{target: target, transport: server.Client().Transport})
					fields := map[string]any{}
					if tt.json {
						if protocol == "gemini" {
							fields["generationConfig"] = map[string]any{"responseMimeType": "application/json"}
						} else {
							fields["response_format"] = map[string]any{"type": "json_object"}
						}
					}
					path, body := enterprisePublicRequest(t, "gemini-3.8-flash", protocol, stream, fields)
					response := enterprisePublicCall(router, path, body)
					raw := response.Body.String()
					wantCalls := int32(1)
					if tt.retry {
						wantCalls = 2
					}
					if calls.Load() != wantCalls {
						t.Fatalf("generation calls=%d, want %d; status=%d body=%s", calls.Load(), wantCalls, response.Code, raw)
					}
					wantHTTP := tt.want
					if tt.started && stream {
						wantHTTP = 200
					}
					if response.Code != wantHTTP {
						t.Fatalf("status=%d, want %d; body=%s", response.Code, wantHTTP, raw)
					}
					if strings.Contains(raw, "private-") {
						t.Fatalf("raw upstream data leaked: %s", raw)
					}
					if tt.retry {
						if !strings.Contains(raw, "offline success") || strings.Contains(raw, `"error"`) {
							t.Fatalf("permitted failover failed: %s", raw)
						}
					} else {
						if !strings.Contains(raw, `"error"`) || strings.Contains(raw, `"finish_reason":"stop"`) || strings.Contains(raw, `"finishReason":"STOP"`) || strings.Contains(raw, "[DONE]") {
							t.Fatalf("failure signalled success: %s", raw)
						}
						if tt.http == 200 && tt.want == 502 && strings.Contains(raw, "returned HTTP 502") {
							t.Fatal("stream error was mislabeled as an HTTP 502 response")
						}
					}
					if cfg.NonStreamKeepAliveInterval != 0 || cfg.RequestRetry != 0 {
						t.Fatal("test or execution changed keepalive/retry configuration")
					}
				})
			}
		}
	}
}
