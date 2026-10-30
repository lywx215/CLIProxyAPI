package helps

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestEnterpriseReasoningSnapshotsAndSummaryFallback(t *testing.T) {
	const summary = "思考 hello🙂"
	want, err := countEnterpriseText(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		snapshots []string
		summary   bool
		known     bool
		want      int64
	}{
		{"unknown", nil, false, false, 0},
		{"summary", nil, true, true, want},
		{"zero", []string{`{"thoughtsTokenCount":0}`}, true, true, 0},
		{"reported", []string{`{"thoughtsTokenCount":50}`}, true, true, 50},
		{"cumulative", []string{`{"thoughtsTokenCount":10}`, `{"thoughtsTokenCount":10}`, `{"promptTokenCount":1000}`, `{"thoughtsTokenCount":50}`}, true, true, 50},
		{"decreasing", []string{`{"thoughtsTokenCount":50}`, `{"thoughtsTokenCount":10}`}, true, true, want},
		{"duplicate-key", []string{`{"thoughtsTokenCount":50,"thoughtsTokenCount":10}`}, true, true, want},
		{"no-recursive-lookup", []string{`{"metadata":{"thoughtsTokenCount":50},"text":"thoughtsTokenCount:50"}`}, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewEnterpriseTokenCounter([]byte(`{"query":{"text":"input"}}`))
			if tc.summary {
				for _, text := range []string{"思", "考 hello", "🙂"} {
					c.ObserveThought(text)
				}
			}
			for _, raw := range tc.snapshots {
				c.ObserveUsage(json.RawMessage(raw))
			}
			detail := c.Detail([]map[string]any{{"text": "answer"}})
			if detail.TokenPresence.ReasoningTokens != tc.known || detail.ReasoningTokens != tc.want || detail.TokenPresence.CachedTokens {
				t.Fatalf("wrong availability/count: %+v", detail)
			}
			if detail.TotalTokens != detail.InputTokens+detail.OutputTokens+tc.want {
				t.Fatal("reasoning was omitted or counted twice")
			}
		})
	}
	for _, invalid := range []string{`-1`, `1.5`, `1e100`, `9223372036854775808`, `"50"`, `true`, `null`, `[]`, `{}`} {
		for _, fallback := range []bool{false, true} {
			t.Run("invalid="+invalid+"/"+map[bool]string{false: "unknown", true: "fallback"}[fallback], func(t *testing.T) {
				c := NewEnterpriseTokenCounter([]byte(`{"query":{"text":"input"}}`))
				c.ObserveUsage(json.RawMessage(`{"thoughtsTokenCount":10}`))
				c.ObserveUsage(json.RawMessage(`{"thoughtsTokenCount":` + invalid + `}`))
				if fallback {
					c.ObserveThought(summary)
				}
				d := c.Detail(nil)
				if d.TokenPresence.ReasoningTokens != fallback || fallback && d.ReasoningTokens != want {
					t.Fatal("invalid report survived validation")
				}
			})
		}
	}
}

func TestEnterpriseReasoningFailuresAreFieldLocal(t *testing.T) {
	c := &EnterpriseTokenCounter{input: 1000, inputKnown: true, count: func(text string) (int64, error) {
		if text == "bad-summary" {
			return 0, errors.New("tokenizer failure")
		}
		if text == "answer" {
			return 100, nil
		}
		return 0, nil
	}}
	c.ObserveThought("bad-summary")
	d := c.Detail([]map[string]any{{"text": "answer"}})
	if d.TokenPresence.ReasoningTokens || d.OutputTokens != 100 || d.TotalTokens != 1100 {
		t.Fatal("summary failure damaged known counts")
	}
	c.ObserveUsage(json.RawMessage(`{"thoughtsTokenCount":50}`))
	d = c.Detail([]map[string]any{{"text": "answer"}})
	if !d.TokenPresence.ReasoningTokens || d.ReasoningTokens != 50 || d.TotalTokens != 1150 {
		t.Fatal("valid upstream count did not survive local failure")
	}
	c.count = func(string) (int64, error) { return 0, errors.New("unavailable") }
	d = c.Detail(nil)
	if !d.TokenPresence.ReasoningTokens || d.ReasoningTokens != 50 || d.TokenPresence.OutputTokens || d.TokenPresence.TotalTokens {
		t.Fatal("reported reasoning depended on tokenizer availability")
	}

	bounded := NewEnterpriseTokenCounter([]byte(`{"query":{"text":"input"}}`))
	bounded.ObserveThought(strings.Repeat("x", 4*1024*1024+1))
	bounded.ObserveThought("later")
	if bounded.Detail(nil).TokenPresence.ReasoningTokens || bounded.thoughts.Len() != 0 {
		t.Fatal("oversized summary was retained or counted partially")
	}
	bounded.ObserveUsage(json.RawMessage(`{"thoughtsTokenCount":9223372036854775807}`))
	d = bounded.Detail([]map[string]any{{"text": "answer"}})
	if d.ReasoningTokens != math.MaxInt64 || !d.TokenPresence.ReasoningTokens || d.TokenPresence.TotalTokens {
		t.Fatal("total overflow was not isolated")
	}
}

func TestEnterpriseReasoningProtocolAccounting(t *testing.T) {
	for _, known := range []bool{true, false} {
		d := usage.Detail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 50, TotalTokens: 1150, TokenPresence: &usage.TokenPresence{InputTokens: true, OutputTokens: true, ReasoningTokens: known, TotalTokens: true}}
		if !known {
			d.TotalTokens = 1100
		}
		for _, gemini := range []bool{true, false} {
			payload := EnterpriseUsagePayload([]byte(`{}`), gemini, d)
			root, output, reason, total := "usage", "completion_tokens", "completion_tokens_details.reasoning_tokens", "total_tokens"
			want := int64(100)
			if gemini {
				root, output, reason, total = "usageMetadata", "candidatesTokenCount", "thoughtsTokenCount", "totalTokenCount"
			} else if known {
				want = 150
			}
			if gjson.GetBytes(payload, root+"."+output).Int() != want || gjson.GetBytes(payload, root+"."+total).Int() != d.TotalTokens || gjson.GetBytes(payload, root+"."+reason).Exists() != known {
				t.Fatalf("wrong wire mapping: %s", payload)
			}
		}
	}
}

func TestEnterpriseReasoningPartialStreamUsageAfterCountFailure(t *testing.T) {
	c := NewEnterpriseTokenCounter([]byte(`{"query":{"text":"input"}}`))
	c.ObserveUsage(json.RawMessage(`{"thoughtsTokenCount":50}`))
	c.count = func(text string) (int64, error) {
		if text == "" {
			return 0, nil
		}
		return 0, errors.New("synthetic output tokenizer failure")
	}
	d := c.Detail([]map[string]any{{"text": "body"}})
	if !EnterpriseUsageAvailable(d) || d.TokenPresence.OutputTokens || d.TokenPresence.TotalTokens {
		t.Fatalf("wrong partial usage: %+v", d)
	}
	payload := EnterpriseChatUsageChunk([]byte(`{"choices":[{"finish_reason":"stop"}]}`), d)
	if gjson.GetBytes(payload, "choices.#").Int() != 0 || gjson.GetBytes(payload, "usage.completion_tokens_details.reasoning_tokens").Int() != 50 || gjson.GetBytes(payload, "usage.completion_tokens").Exists() || gjson.GetBytes(payload, "usage.total_tokens").Exists() {
		t.Fatalf("valid partial usage was lost: %s", payload)
	}
	if EnterpriseUsageAvailable(usage.Detail{TokenPresence: &usage.TokenPresence{}}) {
		t.Fatal("unknown counters are unavailable")
	}
	if !EnterpriseUsageAvailable(usage.Detail{TokenPresence: &usage.TokenPresence{ReasoningTokens: true}}) {
		t.Fatal("real reported zero must remain available")
	}
}
