package executor

import (
	"context"
	"encoding/json"
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

type enterpriseUsageCapture struct{ records chan usage.Record }

func (c *enterpriseUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.AuthID == "enterprise-usage-capture" {
		c.records <- record
	}
}

func TestEnterpriseUsageAcrossChunksAndFailures(t *testing.T) {
	capture := &enterpriseUsageCapture{records: make(chan usage.Record, 16)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	var baseline int64
	for _, scenario := range []struct {
		name, raw               string
		stream, include, failed bool
	}{
		{"full", enterpriseAnswer, false, false, false},
		{"stream", enterpriseAnswer, true, true, false},
		{"split", `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"hel"}},{"content":{"text":"lo"}}]}},{"answer":{"state":"SUCCEEDED"}}]`, true, true, false},
		{"no-usage", enterpriseAnswer, true, false, false},
		{"truncated", `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"hello"}}]}}]`, true, true, true},
		{"json", "", false, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			raw := scenario.raw
			if scenario.name == "json" {
				encoded, _ := json.Marshal([]any{map[string]any{"answer": map[string]any{"state": "IN_PROGRESS", "replies": []any{map[string]any{"content": map[string]any{"text": "```json\n{\"ok\":true}\n```"}}}}}, map[string]any{"answer": map[string]any{"state": "SUCCEEDED"}}})
				raw = string(encoded)
			}
			transport := enterpriseTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(raw))}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			auth := enterpriseExecutorAuth(t, enterpriseExecutorIdentity("enterprise-usage-capture", ""))
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-4-argon", Protocol: "gemini-enterprise"}}, Params: map[string]any{"query.text": "最终 configured input"}}}}}
			e := NewGeminiEnterpriseExecutor(cfg)
			payload := `{"messages":[{"role":"user","content":"must not count this original text"}]}`
			if scenario.include {
				payload = `{"messages":[{"role":"user","content":"ignored"}],"stream_options":{"include_usage":true}}`
			}
			if scenario.name == "json" {
				payload = `{"messages":[{"role":"user","content":"ignored"}],"response_format":{"type":"json_object"}}`
			}
			req := exec.Request{Model: "gemini-4-argon", Payload: []byte(payload)}
			opts := enterpriseOptions(translator.FormatOpenAI)
			var wireUsage int64
			if scenario.stream {
				result, err := e.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				finishes, usageChunks := 0, 0
				var failed bool
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						failed = true
						continue
					}
					if gjson.GetBytes(chunk.Payload, "choices.0.finish_reason").String() == "stop" {
						finishes++
					}
					if gjson.GetBytes(chunk.Payload, "usage").Exists() {
						usageChunks++
						wireUsage = gjson.GetBytes(chunk.Payload, "usage.total_tokens").Int()
						if gjson.GetBytes(chunk.Payload, "choices.#").Int() != 0 {
							t.Fatal("usage packet contained choices")
						}
					}
				}
				if failed != scenario.failed {
					t.Fatalf("failed=%v", failed)
				}
				if scenario.failed {
					if finishes != 0 || usageChunks != 0 {
						t.Fatal("failure sent success terminal")
					}
				} else if finishes != 1 || usageChunks != map[bool]int{true: 1, false: 0}[scenario.include] {
					t.Fatalf("finishes=%d usage=%d", finishes, usageChunks)
				}
			} else {
				result, err := e.Execute(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				wireUsage = gjson.GetBytes(result.Payload, "usage.total_tokens").Int()
			}
			var record usage.Record
			select {
			case record = <-capture.records:
			case <-time.After(5 * time.Second):
				t.Fatal("no usage record")
			}
			if record.Failed != scenario.failed || record.Detail.InputTokens <= 0 || record.Detail.OutputTokens <= 0 || record.Detail.TokenPresence.ReasoningTokens || record.Detail.TokenPresence.CachedTokens {
				t.Fatalf("bad record: %+v", record)
			}
			if scenario.name == "full" {
				baseline = record.Detail.TotalTokens
			}
			if scenario.name != "json" && record.Detail.TotalTokens != baseline {
				t.Fatalf("chunking changed usage: %d vs %d", record.Detail.TotalTokens, baseline)
			}
			if wireUsage > 0 && wireUsage != record.Detail.TotalTokens {
				t.Fatal("response and record differ")
			}
			want := helps.NewEnterpriseTokenCounter([]byte(`{"query":{"text":"最终 configured input"}}`)).Detail(nil).InputTokens
			if record.Detail.InputTokens != want {
				t.Fatal("did not count final payload")
			}
			select {
			case <-capture.records:
				t.Fatal("usage published twice")
			default:
			}
		})
	}
}

type enterpriseUsageNoop struct{}

func (enterpriseUsageNoop) HandleUsage(context.Context, usage.Record) {}
