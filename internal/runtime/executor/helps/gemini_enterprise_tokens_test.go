package helps

import (
	"errors"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseTokenCounterFinalInputAndSemanticOutput(t *testing.T) {
	counter := NewEnterpriseTokenCounter([]byte(`{"query":{"text":"系统提示 history\n你好 world"},"generationSpec":{"modelId":"do-not-count"}}`))
	input, err := countEnterpriseText("系统提示 history\n你好 world")
	if err != nil {
		t.Fatal(err)
	}
	parts := []map[string]any{{"text": "hello 世界"}, {"executableCode": map[string]any{"language": "PYTHON", "code": "print(7)"}}, {"codeExecutionResult": map[string]any{"outcome": "OK", "output": "7\n"}}}
	wantOutput := int64(0)
	for _, text := range []string{"hello 世界", "PYTHON", "print(7)", "OK", "7\n"} {
		n, err := countEnterpriseText(text)
		if err != nil {
			t.Fatal(err)
		}
		wantOutput += n
	}
	detail := counter.Detail(parts)
	if detail.InputTokens != input || detail.OutputTokens != wantOutput || detail.TotalTokens != input+wantOutput {
		t.Fatalf("unexpected counters: %+v", detail)
	}
	if detail.TokenPresence.ReasoningTokens || detail.TokenPresence.CachedTokens || detail.TokenPresence.CacheReadTokens || !detail.TokenPresence.TotalTokens {
		t.Fatalf("unexpected presence: %+v", detail.TokenPresence)
	}
	for _, gemini := range []bool{false, true} {
		payload := EnterpriseUsagePayload([]byte(`{"choices":[]}`), gemini, detail)
		path := "usage.prompt_tokens"
		if gemini {
			path = "usageMetadata.promptTokenCount"
		}
		if gjson.GetBytes(payload, path).Int() != input {
			t.Fatalf("missing usage: %s", payload)
		}
		for _, forbidden := range []string{"usage.completion_tokens_details", "usage.prompt_tokens_details", "usageMetadata.thoughtsTokenCount", "usageMetadata.cachedContentTokenCount", "usage_source", "estimated"} {
			if gjson.GetBytes(payload, forbidden).Exists() {
				t.Fatalf("unexpected field %s", forbidden)
			}
		}
	}
}

func TestEnterpriseTokenizerFailureAndConcurrency(t *testing.T) {
	if _, err := loadEnterpriseTokenizer([]byte("invalid")); err == nil {
		t.Fatal("corrupt model accepted")
	}
	counter := &EnterpriseTokenCounter{count: func(string) (int64, error) { return 0, errors.New("unavailable") }}
	detail := counter.Detail([]map[string]any{{"text": "answer"}})
	if detail.TokenPresence.InputTokens || detail.TokenPresence.OutputTokens || detail.TokenPresence.TotalTokens {
		t.Fatal("failed counters became available")
	}
	if string(EnterpriseUsagePayload([]byte(`{}`), false, detail)) != `{}` {
		t.Fatal("failed tokenizer fabricated usage")
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Go(func() {
			if n, err := countEnterpriseText("hello 中文🙂"); err != nil || n <= 0 {
				t.Errorf("count=%d err=%v", n, err)
			}
		})
	}
	group.Wait()
}

func TestEnterpriseTokenCounterFailureWithoutText(t *testing.T) {
	counter := &EnterpriseTokenCounter{input: 12, inputKnown: true, count: func(string) (int64, error) {
		return 0, errors.New("unavailable")
	}}
	for _, parts := range [][]map[string]any{nil, {{"executableCode": map[string]any{}}}} {
		detail := counter.Detail(parts)
		if !detail.TokenPresence.InputTokens || detail.TokenPresence.OutputTokens || detail.TokenPresence.TotalTokens {
			t.Fatalf("missing tokenizer became known empty output: %+v", detail)
		}
	}
}
