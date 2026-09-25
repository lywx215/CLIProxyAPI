package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// Exercise the real Execute paths, including translation, headers, client
// selection, usage wrapping, status handling and response consumption.
func TestDiagnosticRealExecutorEquivalence(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		name, model, format, payload, reply, path, textPath string
		execute                                             func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	}{
		{"codex", "gpt-5", "codex", `{"model":"gpt-5","instructions":"fixture","input":[{"role":"user","content":[{"type":"input_text","text":"fixture-input"}]}]}`, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"fixture-output\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", "/responses", "input.0.content.0.text", NewCodexExecutor(cfg).Execute},
		{"claude-utls", "claude-3-5-sonnet-20241022", "claude", `{"model":"claude-3-5-sonnet-20241022","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"fixture-input"}]}]}`, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"fixture-output"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, "/v1/messages", "messages.0.content.0.text", NewClaudeExecutor(cfg).Execute},
		{"gemini", "gemini-3.1-pro-preview", "gemini", `{"contents":[{"role":"user","parts":[{"text":"fixture-input"}]}],"generationConfig":{"maxOutputTokens":500000}}`, `{"candidates":[{"content":{"role":"model","parts":[{"text":"fixture-output"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`, "/v1beta/models/gemini-3.1-pro-preview:generateContent", "contents.0.parts.0.text", NewGeminiExecutor(cfg).Execute},
		{"kimi", "kimi-k3", "openai", `{"model":"kimi-k3","messages":[{"role":"user","content":"fixture-input"}]}`, `{"id":"chatcmpl_fixture","object":"chat.completion","model":"k3","choices":[{"index":0,"message":{"role":"assistant","content":"fixture-output"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, "/v1/chat/completions", "messages.0.content", NewKimiExecutor(cfg).Execute},
	} {
		for _, status := range []int{200, 503} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, status), func(t *testing.T) {
				type received struct {
					body         []byte
					header       http.Header
					path, method string
					proto        int
				}
				seen := make(chan received, 2)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					seen <- received{body, r.Header.Clone(), r.URL.Path, r.Method, r.ProtoMajor}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					if status == 200 {
						_, _ = io.WriteString(w, tc.reply)
					} else {
						_, _ = io.WriteString(w, `{"error":{"message":"fixture-unavailable"}}`)
					}
				}))
				defer upstream.Close()
				auth := &cliproxyauth.Auth{ID: "diagnostic-fixture", Provider: strings.Split(tc.name, "-")[0], Attributes: map[string]string{"api_key": "fixture-key", "base_url": upstream.URL}, Metadata: map[string]any{"access_token": "fixture-key"}}
				var mu sync.Mutex
				var records []diagnostics.Record
				peers := fmt.Sprintf(`[{"alias":"fixture","origin":%q,"pathPrefix":"/","service":"gcli2api","deploymentId":null}]`, upstream.URL)
				engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, peers, nil, nil, func(line []byte) error {
					var record diagnostics.Record
					if err := json.Unmarshal(bytes.TrimPrefix(line, []byte("@diag ")), &record); err != nil {
						return err
					}
					mu.Lock()
					records = append(records, record)
					mu.Unlock()
					return nil
				})
				var original cliproxyexecutor.Response
				var originalErr error
				for _, instrumented := range []bool{false, true} {
					ctx := context.Background()
					var span *diagnostics.ServerSpan
					if instrumented {
						ctx, span = engine.StartServer(ctx, nil, "fixture-request")
					}
					ctx = cliproxyexecutor.WithUpstreamAttemptTracker(ctx)
					response, err := tc.execute(ctx, auth, cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.payload)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.format)})
					if status == 200 && err != nil {
						t.Fatal(err)
					}
					if status != 200 {
						statusErr, ok := err.(interface{ StatusCode() int })
						if !ok || statusErr.StatusCode() != status || !strings.Contains(err.Error(), "fixture-unavailable") {
							t.Fatalf("status/body changed: %v", err)
						}
					}
					if !cliproxyexecutor.UpstreamAttempted(ctx) {
						t.Error("usage attempt tracker lost")
					}
					got := <-seen
					if got.method != "POST" || got.path != tc.path || got.proto != 1 || gjson.GetBytes(got.body, tc.textPath).String() != "fixture-input" {
						t.Fatalf("request changed: %+v", got)
					}
					if tc.name == "gemini" && gjson.GetBytes(got.body, "generationConfig.maxOutputTokens").Int() != 65536 {
						t.Fatal("token cap changed")
					}
					if tc.name == "kimi" && gjson.GetBytes(got.body, "model").String() != "k3" {
						t.Fatal("model normalization changed")
					}
					if tc.name == "codex" && !gjson.GetBytes(got.body, "stream").Bool() {
						t.Fatal("native SSE behavior changed")
					}
					if instrumented {
						if got.header.Get("X-Diag-Request-Id") != "fixture-request" || got.header.Get("Traceparent") == "" {
							t.Fatal("real executor did not reach decorated final boundary")
						}
						if !bytes.Equal(response.Payload, original.Payload) || fmt.Sprint(err) != fmt.Sprint(originalErr) {
							t.Fatalf("response differs: %s / %s; %v / %v", original.Payload, response.Payload, originalErr, err)
						}
						span.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
					} else {
						original, originalErr = response, err
						if got.header.Get("Traceparent") != "" {
							t.Fatal("unexpected uninstrumented trace")
						}
					}
				}
				mu.Lock()
				defer mu.Unlock()
				calls := 0
				for _, record := range records {
					if record.Event == "diag.call" {
						calls++
						data := record.Data.(map[string]any)
						if data["upstreamStatus"] != float64(status) || data["callKind"] != "model" || data["endReason"] != "eof" {
							t.Fatalf("call terminal: %v", data)
						}
					}
				}
				if calls != 1 {
					t.Fatalf("call count = %d", calls)
				}
			})
		}
	}
}
