package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// This matrix executes the production executors and translators against a local
// HTTP upstream twice, with and without observation. No credentials are loaded.
func TestDIAG05RealSemanticBoundaries(t *testing.T) {
	var artifact bytes.Buffer
	for _, provider := range []string{"gemini", "antigravity", "antigravity-collected"} {
		for _, stream := range []bool{false, true} {
			if provider == "antigravity-collected" && stream {
				continue
			}
			for _, tc := range []struct {
				name, request, reply, want string
				status                     int
			}{
				{"normal", `{"contents":[{"role":"user","parts":[{"text":"fixture-first"}]},{"role":"model","parts":[{"text":"fixture-answer"}]},{"role":"user","parts":[{"text":"fixture-next"}]}]}`, `{"candidates":[{"content":{"parts":[{"text":"fixture-result"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7,"thoughtsTokenCount":80,"totalTokenCount":92}}`, "success", 200},
				{"empty-user", `{"contents":[{"role":"model","parts":[{"text":"fixture-answer"}]},{"role":"user","parts":[{"text":""}]},{"role":"model","parts":[{"text":"fixture-tail"}]}]}`, `{"candidates":[{"content":{"parts":[{"text":"fixture-result"}]},"finishReason":"STOP"}]}`, "success", 200},
				{"zero", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`, `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"cachedContentTokenCount":0}}`, "empty", 200},
				{"missing-terminal", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`, `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, "incomplete", 200},
				{"tool-media", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"fixture_tool","args":{}}},{"inlineData":{"mimeType":"image/png","data":"Zg=="}}]},"finishReason":"STOP"}]}`, "success", 200},
				{"error-frame", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`, `{"error":{"message":"FORBIDDEN_BODY_SECRET","code":500}}`, "error", 200},
				{"http-error", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`, `{"error":{"message":"FORBIDDEN_BODY_SECRET","code":503}}`, "error", 503},
				{"malformed", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`, `{"candidates":`, "incomplete", 200},
			} {
				t.Run(fmt.Sprintf("%s/%t/%s", provider, stream, tc.name), func(t *testing.T) {
					upstreamStreaming := stream || provider == "antigravity-collected"
					var seenMu sync.Mutex
					var requests [][]byte
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						b, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						seenMu.Lock()
						requests = append(requests, b)
						seenMu.Unlock()
						w.WriteHeader(tc.status)
						reply := tc.reply
						if strings.HasPrefix(provider, "antigravity") && gjson.Valid(reply) {
							reply = `{"response":` + reply + `}`
						}
						if upstreamStreaming && tc.status == 200 {
							_, _ = io.WriteString(w, "data: "+reply+"\n\n")
						} else {
							_, _ = io.WriteString(w, reply)
						}
					}))
					defer upstream.Close()
					cfg := &config.Config{}
					gem, ag := NewGeminiExecutor(cfg), NewAntigravityExecutor(cfg)
					execute, executeStream := gem.Execute, gem.ExecuteStream
					model := "gemini-2.5-flash"
					if provider != "gemini" {
						execute, executeStream = ag.Execute, ag.ExecuteStream
					}
					if provider == "antigravity-collected" {
						model = "gemini-3-pro"
					}
					auth := &cliproxyauth.Auth{ID: "fixture-auth", Attributes: map[string]string{"base_url": upstream.URL, "api_key": "fixture-key"}, Metadata: map[string]any{"access_token": "fixture-token", "expired": time.Now().Add(24 * time.Hour).Format(time.RFC3339), "project_id": "fixture-project", "disable_cooling": true}}
					var mu sync.Mutex
					var records []diagnostics.Record
					engine := diagnostics.NewEngine(diagnostics.ResourceConfig{Environment: "test", DeploymentID: "synthetic", InstanceID: "diag05-fixture"}, "", nil, func() bool { return true }, func(line []byte) error {
						if len(line) > 4096 || bytes.Contains(line, []byte("FORBIDDEN_BODY_SECRET")) || bytes.Contains(line, []byte("fixture-key")) {
							t.Error("unsafe public record")
						}
						var r diagnostics.Record
						if err := json.Unmarshal(line[6:], &r); err != nil {
							return err
						}
						mu.Lock()
						records = append(records, r)
						artifact.Write(line)
						mu.Unlock()
						return nil
					})
					var baseline [][]byte
					var baselineErr string
					for _, observed := range []bool{false, true} {
						ctx := context.Background()
						var span *diagnostics.ServerSpan
						if observed {
							ctx, span = engine.StartServer(ctx, nil, "fixture-request")
						}
						ctx = cliproxyexecutor.WithUpstreamAttemptTracker(ctx)
						req := cliproxyexecutor.Request{Model: model, Payload: []byte(tc.request)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: stream}
						var payloads [][]byte
						var err error
						if stream {
							var result *cliproxyexecutor.StreamResult
							result, err = executeStream(ctx, auth, req, opts)
							if err == nil {
								for c := range result.Chunks {
									payloads = append(payloads, bytes.Clone(c.Payload))
									if c.Err != nil {
										err = c.Err
									}
								}
							}
						} else {
							var response cliproxyexecutor.Response
							response, err = execute(ctx, auth, req, opts)
							payloads = append(payloads, response.Payload)
						}
						if !cliproxyexecutor.UpstreamAttempted(ctx) {
							t.Fatal("usage attempt marker lost")
						}
						if observed {
							if fmt.Sprint(err) != baselineErr || !equalPayloads(payloads, baseline) {
								t.Fatalf("business result changed: %v/%v", err, baselineErr)
							}
							span.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
						} else {
							baseline, baselineErr = payloads, fmt.Sprint(err)
						}
					}
					mu.Lock()
					defer mu.Unlock()
					attempts, calls, normalized, converted := 0, 0, 0, 0
					var seq uint64
					for _, r := range records {
						if r.SpanKind == "server" {
							seq++
							if r.LogSeq != seq {
								t.Errorf("server sequence %d != %d", r.LogSeq, seq)
							}
						}
						d, _ := r.Data.(map[string]any)
						switch r.Event {
						case "diag.call":
							calls++
						case "request.normalized":
							normalized++
							if tc.name == "empty-user" && provider == "gemini" {
								if d["before"].(map[string]any)["emptyMessageCount"] != float64(1) || d["after"].(map[string]any)["emptyMessageCount"] != float64(3) {
									t.Fatalf("empty turn provenance: %v", d)
								}
							}
						case "upstream.attempt_finished":
							attempts++
							if d["resultClass"] != tc.want {
								t.Fatalf("result = %v want %s", d, tc.want)
							}
							u := d["usage"].(map[string]any)
							if tc.name == "normal" {
								if u["outputTotal"].(map[string]any)["value"] != float64(87) {
									t.Fatalf("usage = %v", u)
								}
							}
							if tc.name == "empty-user" && u["candidate"].(map[string]any)["present"] != false {
								t.Fatal("missing usage became zero")
							}
							if tc.name == "zero" && (u["candidate"].(map[string]any)["present"] != true || u["candidate"].(map[string]any)["value"] != float64(0)) {
								t.Fatal("zero lost")
							}
						case "response.converted":
							converted++
							if provider == "antigravity-collected" && d["deliveryMode"] != "collected" {
								t.Fatal("collection mode lost")
							}
						case "diag.server":
							if d["coverage"].(map[string]any)["expectedLastLogSeq"] != float64(seq) {
								t.Fatal("terminal sequence")
							}
						case "diag.truncated":
							t.Fatal("ordinary event unexpectedly truncated")
						}
					}
					if attempts != 1 || calls != 1 || normalized != 1 {
						t.Fatalf("events: attempts=%d calls=%d normalized=%d converted=%d", attempts, calls, normalized, converted)
					}
					seenMu.Lock()
					defer seenMu.Unlock()
					if len(requests) != 2 {
						t.Fatal("send count changed")
					}
					// Native Antigravity generates per-send request IDs. Compare the
					// actual business contents, preserving the provider's own IDs.
					path := "contents"
					if provider != "gemini" {
						path = "request.contents"
					}
					if gjson.GetBytes(requests[0], path).Raw != gjson.GetBytes(requests[1], path).Raw {
						t.Fatal("normalized body changed")
					}
				})
			}
		}
	}
	if path := os.Getenv("DIAG05_TEST_RECORDS"); path != "" {
		if err := os.WriteFile(path, artifact.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func equalPayloads(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestDIAG05FinalProtocolUsage(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", format, stream), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reply := `{"responseId":"fixture-id","candidates":[{"content":{"parts":[{"text":"fixture-output"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7,"thoughtsTokenCount":80,"totalTokenCount":92}}`
					if stream {
						reply = "data: " + reply + "\n\n"
					}
					_, _ = io.WriteString(w, reply)
				}))
				defer upstream.Close()
				var mu sync.Mutex
				var records []diagnostics.Record
				e := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
					var r diagnostics.Record
					if err := json.Unmarshal(b[6:], &r); err != nil {
						return err
					}
					mu.Lock()
					records = append(records, r)
					mu.Unlock()
					return nil
				})
				ctx, s := e.StartServer(context.Background(), nil, "fixture")
				ex := NewGeminiExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": upstream.URL}}
				req := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture-input"}]}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: format, Stream: stream}
				if stream {
					result, err := ex.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for c := range result.Chunks {
						if c.Err != nil {
							t.Fatal(c.Err)
						}
					}
				} else {
					_, err := ex.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
				}
				s.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
				mu.Lock()
				defer mu.Unlock()
				found := false
				for _, r := range records {
					if r.Event != "response.converted" {
						continue
					}
					found = true
					u := r.Data.(map[string]any)["deliveredUsage"].(map[string]any)
					if u["outputTotal"].(map[string]any)["value"] != float64(87) || u["reasoningIncludedInOutput"] != true || u["outputTotal"].(map[string]any)["source"] != "converted" {
						t.Fatalf("converted usage %v", u)
					}
				}
				if !found {
					t.Fatal("conversion event missing")
				}
			})
		}
	}
}
