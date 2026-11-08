package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityEnterpriseNativeArgonThinkingAndRawResponse(t *testing.T) {
	for _, level := range []string{"low", "medium", "high"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", level, stream), func(t *testing.T) {
				model := "gemini-4-argon-" + level
				parts := `[ {"text":"UPSTREAM_ANSWER_40"} ]`
				usage := `{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}`
				if level != "low" {
					parts = `[ {"thought":true,"text":"Visible summary"}, {"text":"UPSTREAM_ANSWER_81"} ]`
					usage = `{"promptTokenCount":3,"candidatesTokenCount":1,"thoughtsTokenCount":2,"totalTokenCount":6}`
				}
				raw := []byte(`{"candidates":[{"content":{"role":"model","parts":` + parts + `},"finishReason":"STOP"}],"usageMetadata":` + usage + `,"modelVersion":"gemini-4-argon"}`)
				calls := 0
				ctx := aeMockContext(t, aeQuotaPositive, func(request *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					if got := gjson.GetBytes(body, "model").String(); got != model {
						t.Fatalf("native model = %q, want %q", got, model)
					}
					thinking := gjson.GetBytes(body, "generationConfig.thinkingConfig")
					if thinking.Get("thinkingLevel").String() != strings.ToUpper(level) || !thinking.Get("includeThoughts").Bool() {
						t.Fatalf("thinking intent changed before transport: %s", thinking.Raw)
					}
					if thinking.Get("thinkingBudget").Exists() || thinking.Get("thinking_budget").Exists() {
						t.Fatalf("numeric budget injected into a native level request: %s", thinking.Raw)
					}
					if stream {
						return aeResponse(http.StatusOK, "text/event-stream", "data: "+string(raw)+"\n\n"), nil
					}
					return aeResponse(http.StatusOK, "application/json", string(raw)), nil
				})
				e := NewAntigravityEnterpriseExecutor(&config.Config{})
				body := []byte(`{"contents":[{"role":"user","parts":[{"text":"Synthetic arithmetic fixture"}]}],"generationConfig":{"maxOutputTokens":8192,"thinkingConfig":{"thinkingLevel":"` + strings.ToUpper(level) + `","includeThoughts":true}}}`)
				req := coreexec.Request{Model: model, Payload: body}
				opts := coreexec.Options{SourceFormat: translator.FormatGemini}
				var documents [][]byte
				if stream {
					result, err := e.ExecuteStream(ctx, aeExecutorAuth("global"), req, opts)
					if err != nil {
						t.Fatal(err)
					}
					var output []byte
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output = append(output, chunk.Payload...)
					}
					for _, line := range bytes.Split(output, []byte("\n")) {
						line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
						if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
							continue
						}
						if !json.Valid(line) {
							t.Fatalf("invalid native SSE document: %s", line)
						}
						documents = append(documents, line)
					}
				} else {
					result, err := e.Execute(ctx, aeExecutorAuth("global"), req, opts)
					if err != nil {
						t.Fatal(err)
					}
					documents = append(documents, result.Payload)
				}
				if calls != 1 || len(documents) != 1 {
					t.Fatalf("generation calls=%d documents=%d, want one of each", calls, len(documents))
				}
				var want, got any
				if err := json.Unmarshal(raw, &want); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(documents[0], &got); err != nil {
					t.Fatal(err)
				}
				wantJSON, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				gotJSON, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(wantJSON, gotJSON) {
					t.Fatalf("upstream modelVersion, summary, answer, STOP or usage changed:\nwant %s\ngot  %s", wantJSON, gotJSON)
				}
			})
		}
	}
}
