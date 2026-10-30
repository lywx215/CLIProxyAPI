package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestEnterpriseReasoningUsageMatchesResponseAndFailure(t *testing.T) {
	capture := &enterpriseUsageCapture{records: make(chan usage.Record, 32)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"summary", "reported", "zero", "partial", "policy", "no-summary", "json"} {
				t.Run(format.String()+"/"+map[bool]string{false: "full", true: "stream"}[stream]+"/"+scenario, func(t *testing.T) {
					upstream := enterpriseThoughtAnswer
					known, wantR, failed := true, int64(0), scenario == "partial" || scenario == "policy"
					counter := helps.NewEnterpriseTokenCounter([]byte(`{"query":{"text":"hi"}}`))
					counter.ObserveThought("summary 你好🙂summary continued")
					wantR = counter.Detail(nil).ReasoningTokens
					switch scenario {
					case "reported":
						upstream = strings.TrimSuffix(upstream, "]") + `,{"usageMetadata":{"thoughtsTokenCount":50}}]`
						wantR = 50
					case "zero":
						upstream = strings.TrimSuffix(upstream, "]") + `,{"usageMetadata":{"thoughtsTokenCount":0}}]`
						wantR = 0
					case "partial":
						upstream = strings.Replace(upstream, `,{"answer":{"state":"SUCCEEDED"}}]`, "", 1)
					case "policy":
						upstream = strings.Replace(upstream, `"state":"SUCCEEDED"`, `"state":"SKIPPED","assistSkippedReasons":["CUSTOMER_POLICY_VIOLATION"]`, 1)
					case "no-summary":
						upstream = enterpriseAnswer
						known, wantR = false, 0
					}
					calls := 0
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
						calls++
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstream))}, nil
					}))
					body := `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":false}}}`
					if format == translator.FormatOpenAI {
						body = `{"messages":[{"role":"user","content":"hi"}],"extra_body":{"google":{"thinking_config":{"include_thoughts":false}}},"stream_options":{"include_usage":true}}`
					}
					if scenario == "json" {
						if format == translator.FormatGemini {
							body = strings.Replace(body, `"generationConfig":{`, `"generationConfig":{"responseMimeType":"application/json",`, 1)
						} else {
							body = strings.TrimSuffix(body, "}") + `,"response_format":{"type":"json_object"}}`
						}
					}
					e := NewGeminiEnterpriseExecutor(&config.Config{})
					req := exec.Request{Model: "gemini-3.8-flash", Payload: []byte(body)}
					auth := enterpriseExecutorAuth(t, enterpriseExecutorIdentity("enterprise-usage-capture", ""))
					var packets [][]byte
					seenFailure := false
					if stream {
						response, err := e.ExecuteStream(ctx, auth, req, enterpriseOptions(format))
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								seenFailure = true
							} else {
								packets = append(packets, chunk.Payload)
							}
						}
					} else {
						response, err := e.Execute(ctx, auth, req, enterpriseOptions(format))
						seenFailure = err != nil
						if err == nil {
							packets = append(packets, response.Payload)
						}
					}
					if calls != 1 || seenFailure != failed {
						t.Fatalf("calls=%d failed=%v", calls, seenFailure)
					}
					var record usage.Record
					select {
					case record = <-capture.records:
					case <-time.After(5 * time.Second):
						t.Fatal("missing usage")
					}
					d := record.Detail
					if record.Failed != failed || d.ReasoningTokens != wantR || d.TokenPresence.ReasoningTokens != known || d.TokenPresence.CachedTokens || d.TotalTokens != d.InputTokens+d.OutputTokens+wantR {
						t.Fatalf("wrong record: %+v", record)
					}
					usages := 0
					for _, packet := range packets {
						if strings.Contains(string(packet), "summary") {
							t.Fatal("hidden summaries were forwarded")
						}
						root, out, reason, total := "usage", "completion_tokens", "completion_tokens_details.reasoning_tokens", "total_tokens"
						wantOutput := d.OutputTokens + wantR
						if format == translator.FormatGemini {
							root, out, reason, total = "usageMetadata", "candidatesTokenCount", "thoughtsTokenCount", "totalTokenCount"
							wantOutput = d.OutputTokens
						}
						if gjson.GetBytes(packet, root).Exists() {
							usages++
							if gjson.GetBytes(packet, root+"."+out).Int() != wantOutput || gjson.GetBytes(packet, root+"."+reason).Exists() != known || gjson.GetBytes(packet, root+"."+total).Int() != d.TotalTokens {
								t.Fatal("wire/record usage mismatch")
							}
						}
						if failed && (gjson.GetBytes(packet, "choices.0.finish_reason").String() == "stop" || gjson.GetBytes(packet, "candidates.0.finishReason").String() == "STOP") {
							t.Fatal("failure emitted success terminal")
						}
					}
					if usages != map[bool]int{false: 1, true: 0}[failed] {
						t.Fatal("unexpected terminal usage count")
					}
					select {
					case <-capture.records:
						t.Fatal("usage settled twice")
					default:
					}
				})
			}
		}
	}
}
