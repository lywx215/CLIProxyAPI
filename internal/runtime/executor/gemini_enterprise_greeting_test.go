package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestEnterpriseDefaultChannelGreetingRegionsAndUsage(t *testing.T) {
	capture := &enterpriseUsageCapture{records: make(chan usage.Record, 16)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	for _, model := range []string{"gemini-3.8-flash", "gemini-3.1-pro-preview"} {
		for _, region := range []string{"global", "us", "eu"} {
			for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
				for _, mode := range []string{"full", "stream", "stream-usage"} {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", model, region, format, mode), func(t *testing.T) {
						auth := enterpriseExecutorAuth()
						auth.ID = "enterprise-usage-capture"
						auth.Metadata["location"] = region
						var finalBody []byte
						calls := 0
						ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(r *http.Request) (*http.Response, error) {
							calls++
							if !ge.OfficialBinding(r, "project", region) || r.Header.Get("Authorization") != "Bearer access" {
								t.Fatal("greeting changed the regional credential boundary")
							}
							finalBody, _ = io.ReadAll(r.Body)
							query := gjson.GetBytes(finalBody, "query.text").String()
							raw := enterpriseAnswer
							if !strings.HasPrefix(query, "Continue the conversation below.") || !strings.Contains(query, `"conversation":[{"role":"user","text":"hi"}]`) {
								raw = `{"answer":{"state":"SKIPPED","assistSkippedReasons":["NON_ASSIST_SEEKING_QUERY_IGNORED"]}}`
							}
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}, nil
						}))
						body := []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":3000}`)
						if mode == "stream-usage" {
							body = []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":3000,"stream_options":{"include_usage":true}}`)
						}
						if format == translator.FormatGemini {
							body = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":3000}}`)
						}
						opts := enterpriseOptions(format)
						opts.Stream = mode != "full"
						e := NewGeminiEnterpriseExecutor(&config.Config{})
						req := exec.Request{Model: model, Payload: body}
						var packets [][]byte
						if opts.Stream {
							result, err := e.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								packets = append(packets, chunk.Payload)
							}
						} else {
							result, err := e.Execute(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							packets = append(packets, result.Payload)
						}
						var record usage.Record
						select {
						case record = <-capture.records:
						case <-time.After(5 * time.Second):
							t.Fatal("missing greeting usage")
						}
						want := helps.NewEnterpriseTokenCounter(finalBody).Detail([]map[string]any{{"text": "hello"}})
						if calls != 1 || record.Failed || record.Detail.InputTokens != want.InputTokens || record.Detail.OutputTokens != want.OutputTokens || record.Detail.TotalTokens != want.TotalTokens || want.InputTokens <= 1 {
							t.Fatalf("greeting generation or final-text accounting failed: calls=%d detail=%+v", calls, record.Detail)
						}
						if record.Detail.TokenPresence.ReasoningTokens || record.Detail.TokenPresence.CachedTokens {
							t.Fatal("unknown reasoning or cache became a known zero")
						}
						usagePackets, finishes := 0, 0
						visible := false
						for _, packet := range packets {
							visible = visible || strings.Contains(string(packet), "hello")
							key, inputKey, outputKey, totalKey := "usage", "prompt_tokens", "completion_tokens", "total_tokens"
							if format == translator.FormatGemini {
								key, inputKey, outputKey, totalKey = "usageMetadata", "promptTokenCount", "candidatesTokenCount", "totalTokenCount"
							}
							if gjson.GetBytes(packet, "choices.0.finish_reason").String() == "stop" || gjson.GetBytes(packet, "candidates.0.finishReason").String() == "STOP" {
								finishes++
							}
							if values := gjson.GetBytes(packet, key); values.Exists() {
								usagePackets++
								if len(values.Map()) != 3 || values.Get(inputKey).Int() != want.InputTokens || values.Get(outputKey).Int() != want.OutputTokens || values.Get(totalKey).Int() != want.TotalTokens {
									t.Fatalf("response and reported counters differ: %s", packet)
								}
								if opts.Stream && format == translator.FormatOpenAI && gjson.GetBytes(packet, "choices.#").Int() != 0 {
									t.Fatal("usage-only chunk contains choices")
								}
							}
							if gjson.GetBytes(packet, "error").Exists() {
								t.Fatal("greeting returned an error")
							}
						}
						wantUsage := 1
						if format == translator.FormatOpenAI && mode == "stream" {
							wantUsage = 0
						}
						if !visible || finishes != 1 || usagePackets != wantUsage {
							t.Fatalf("invalid terminal response: visible=%t finishes=%d usage=%d", visible, finishes, usagePackets)
						}
						select {
						case <-capture.records:
							t.Fatal("usage published more than once")
						default:
						}
					})
				}
			}
		}
	}
}

func TestEnterpriseGreetingPayloadRulesRemainFinal(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"configured greeting", "configured question", "filter"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format, stream, mode), func(t *testing.T) {
					rule := config.PayloadRule{Models: []config.PayloadModelRule{{Name: "gemini-3.8-flash", Protocol: ge.Provider}}, Params: map[string]any{"query.text": "hi"}}
					if mode == "configured question" {
						rule.Params["query.text"] = "Only return 2."
					}
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{rule}}}
					if mode == "filter" {
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: rule.Models, Params: []string{"query.text"}}}
					}
					calls := 0
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						body, _ := io.ReadAll(r.Body)
						if gjson.GetBytes(body, "query.text").String() != rule.Params["query.text"] {
							t.Fatal("greeting compatibility overwrote final payload rules")
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
					}))
					body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
					if format == translator.FormatGemini {
						body = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
					}
					opts := enterpriseOptions(format)
					e := NewGeminiEnterpriseExecutor(cfg)
					req := exec.Request{Model: "gemini-3.8-flash", Payload: body}
					var err error
					if stream {
						var result *exec.StreamResult
						result, err = e.ExecuteStream(ctx, enterpriseExecutorAuth(), req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
							}
						}
					} else {
						_, err = e.Execute(ctx, enterpriseExecutorAuth(), req, opts)
					}
					if mode == "filter" {
						if err == nil || calls != 0 {
							t.Fatal("filtered query was restored or sent")
						}
					} else if err != nil || calls != 1 {
						t.Fatalf("calls=%d err=%v", calls, err)
					}
				})
			}
		}
	}
}

func TestEnterpriseGreetingEmptyReplyRemainsFailure(t *testing.T) {
	capture := &enterpriseUsageCapture{records: make(chan usage.Record, 16)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				calls := 0
				var finalBody []byte
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					finalBody, _ = io.ReadAll(r.Body)
					raw := `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"\n"}}]}},{"answer":{"state":"SUCCEEDED"}}]`
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}, nil
				}))
				body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`)
				if format == translator.FormatGemini {
					body = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
				}
				auth := enterpriseExecutorAuth()
				auth.ID = "enterprise-usage-capture"
				e := NewGeminiEnterpriseExecutor(&config.Config{})
				opts := enterpriseOptions(format)
				opts.Stream = stream
				req := exec.Request{Model: "gemini-3.8-flash", Payload: body}
				var err error
				if stream {
					var result *exec.StreamResult
					result, err = e.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					failures := 0
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
							failures++
						}
						for _, path := range []string{"usage", "usageMetadata", "choices.0.finish_reason", "candidates.0.finishReason"} {
							if gjson.GetBytes(chunk.Payload, path).Exists() && gjson.GetBytes(chunk.Payload, path).Type != gjson.Null {
								t.Fatal("empty response fabricated a success terminal")
							}
						}
					}
					if failures != 1 {
						t.Fatalf("terminal failures=%d", failures)
					}
				} else {
					_, err = e.Execute(ctx, auth, req, opts)
				}
				var failure *enterprise.Error
				if !errors.As(err, &failure) || failure.Kind != "empty_upstream" || calls != 1 {
					t.Fatalf("empty response was replayed or accepted: calls=%d err=%v", calls, err)
				}
				select {
				case record := <-capture.records:
					want := helps.NewEnterpriseTokenCounter(finalBody).Detail([]map[string]any{{"text": "\n"}})
					if !record.Failed || record.Detail.InputTokens != want.InputTokens || record.Detail.OutputTokens != want.OutputTokens || record.Detail.TotalTokens != want.TotalTokens {
						t.Fatalf("failed observed usage was lost: %+v", record.Detail)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("missing failed usage")
				}
				select {
				case <-capture.records:
					t.Fatal("failed usage published more than once")
				default:
				}
			})
		}
	}
}
