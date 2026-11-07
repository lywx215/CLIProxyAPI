package helps

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestWebTokenCounterSeparatesMediaAndMergedText(t *testing.T) {
	budget := resources.New(32 << 20)
	ctx, lease := budget.Begin(context.Background())
	c := NewEnterpriseWebTokenCounter(ctx, []byte(`{"configId":"never count this","streamAssistRequest":{"query":{"parts":[{"text":"最终 "},{"text":"history"}]},"fileIds":["ignore-file"],"session":"ignore-session"}}`))
	c.Observe([]contract.Part{{Text: "hel"}, {Text: "reason", Thought: true, ThoughtSignature: "ignore-signature"}})
	c.Observe([]contract.Part{{Text: "lo 世界"}, {Text: " summary", Thought: true}})
	d := c.Detail(true)
	wantInput, _ := countEnterpriseText("最终 history")
	wantOutput, _ := countEnterpriseText("hello 世界")
	wantThought, _ := countEnterpriseText("reason summary")
	if d.InputTokens != 0 || d.TotalTokens != 0 || d.TokenPresence.InputTokens || d.TokenPresence.TotalTokens || d.TokenPresence.CachedTokens {
		t.Fatalf("claimed unknown media usage: %+v", d)
	}
	if d.Estimate == nil || d.Estimate.InputTextTokens == nil || *d.Estimate.InputTextTokens != wantInput ||
		d.Estimate.Source != usage.LocalGemma3SentencePiece || !d.Estimate.Incomplete || !d.Estimate.ResponseComplete ||
		d.OutputTokens != wantOutput || d.ReasoningTokens != wantThought || !d.TokenPresence.ReasoningTokens {
		t.Fatalf("wrong text estimates: %+v %+v", d, d.Estimate)
	}
	payload := EnterpriseWebUsagePayload([]byte(`{"candidates":[]}`), d)
	if gjson.GetBytes(payload, "usageMetadata.promptTokenCount").Exists() || gjson.GetBytes(payload, "usageMetadata.totalTokenCount").Exists() ||
		gjson.GetBytes(payload, "usageEstimate.input_text_tokens").Int() != wantInput {
		t.Fatalf("invalid wire estimates: %s", payload)
	}
	*d.Estimate.InputTextTokens = 99
	if got := c.Detail(false); got.Estimate.ResponseComplete || *got.Estimate.InputTextTokens != wantInput {
		t.Fatal("caller mutated cached measurements")
	}
	lease.Close()
	if budget.Snapshot().InUseBytes != 0 {
		t.Fatal("accounting workspace leaked")
	}
}

func TestWebTokenCounterCodeBoundariesAndMissingThoughts(t *testing.T) {
	c := NewEnterpriseWebTokenCounter(context.Background(), []byte(`{"streamAssistRequest":{"query":{"parts":[{"text":"hi"}]}}}`))
	c.Observe([]contract.Part{{Text: "hel"}, {Text: "lo"}, {ExecutableCode: &contract.ExecutableCode{Language: "PYTHON", Code: "print(7)"}}, {CodeExecutionResult: &contract.CodeExecutionResult{Outcome: "OK", Output: "7"}}, {InlineData: &contract.InlineData{MIMEType: "application/json+suggestions", Data: []byte(`{"text":"not output tokens"}`)}}})
	d := c.Detail(true)
	want := int64(0)
	for _, text := range []string{"hello", "PYTHON", "print(7)", "OK", "7"} {
		n, _ := countEnterpriseText(text)
		want += n
	}
	if d.OutputTokens != want || d.TokenPresence.ReasoningTokens {
		t.Fatalf("wrong code or thought usage: %+v", d)
	}
}

func TestWebTokenCounterBudgetAndTokenizerFailureRemainUnknown(t *testing.T) {
	budget := resources.New(10)
	ctx, lease := budget.Begin(context.Background())
	c := NewEnterpriseWebTokenCounter(ctx, []byte(`{"streamAssistRequest":{"query":{"parts":[{"text":"hello"}]}}}`))
	c.Observe([]contract.Part{{Text: "answer"}, {Text: "summary", Thought: true}})
	d := c.Detail(true)
	if d.Estimate.InputTextTokens != nil || d.TokenPresence.OutputTokens || d.TokenPresence.ReasoningTokens || d.TokenPresence.TotalTokens {
		t.Fatalf("fabricated budget-limited measurement: %+v", d)
	}
	lease.Close()
	c = NewEnterpriseWebTokenCounter(context.Background(), []byte(`{}`))
	c.count = func(string) (int64, error) { return 0, errors.New("unavailable") }
	c.Observe([]contract.Part{{Text: "hello"}})
	if c.Detail(true).TokenPresence.OutputTokens {
		t.Fatal("tokenizer failure became zero")
	}
}

func TestWebTokenCounterNoOutputBeforeFailedSubmission(t *testing.T) {
	c := NewEnterpriseWebTokenCounter(context.Background(), []byte(`{"streamAssistRequest":{"query":{"parts":[{"text":"hello"}]}}}`))
	d := c.Detail(false)
	if d.TokenPresence.OutputTokens || d.Estimate.ResponseComplete || d.Estimate.InputTextTokens == nil {
		t.Fatalf("failed submission usage: %+v", d)
	}
}

func TestWebTokenEstimatePluginProtocol(t *testing.T) {
	for _, raw := range []string{
		`{"usageMetadata":{"candidatesTokenCount":7,"thoughtsTokenCount":3},"usageEstimate":{"source":"local_gemma3_sentencepiece","input_text_tokens":23,"incomplete":true,"response_complete":true}}`,
		`{"usageEstimate":{"source":"local_gemma3_sentencepiece","input_text_tokens":23,"incomplete":true,"response_complete":false}}`,
	} {
		for _, protocol := range []string{"gemini", "gemini-enterprise-web"} {
			for _, stream := range []bool{false, true} {
				d := ParsePluginExecutorResponseUsage(protocol, []byte(raw))
				if stream {
					var buffer StreamUsageBuffer
					ObservePluginExecutorStreamUsage(protocol, []byte("data: "+raw+"\n\n"), &buffer)
					var ok bool
					d, ok = buffer.Detail()
					if !ok {
						t.Fatal("missing estimate")
					}
				}
				if d.TokenPresence == nil || d.TokenPresence.InputTokens || d.TokenPresence.TotalTokens || d.TotalTokens != 0 || d.Estimate == nil || d.Estimate.InputTextTokens == nil || *d.Estimate.InputTextTokens != 23 {
					t.Fatalf("plugin inferred full media total: %+v", d)
				}
			}
		}
	}
}
