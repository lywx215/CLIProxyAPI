package helps

import (
	"strings"
	"testing"
)

func TestEnterprisePluginUsageProtocolNormalization(t *testing.T) {
	for _, payload := range []string{
		`{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100,"thoughtsTokenCount":50,"totalTokenCount":1150}}`,
		`{"usage":{"prompt_tokens":1000,"completion_tokens":150,"completion_tokens_details":{"reasoning_tokens":50},"total_tokens":1150}}`,
	} {
		for _, stream := range []bool{false, true} {
			var d = ParsePluginExecutorResponseUsage("gemini-enterprise", []byte(payload))
			if stream {
				var buffer StreamUsageBuffer
				ObservePluginExecutorStreamUsage("gemini-enterprise", []byte("data: "+payload+"\n\n"), &buffer)
				var ok bool
				d, ok = buffer.Detail()
				if !ok {
					t.Fatal("usage was not observed")
				}
			}
			if d.InputTokens != 1000 || d.OutputTokens != 100 || d.ReasoningTokens != 50 || d.TotalTokens != 1150 || d.TokenPresence == nil || !d.TokenPresence.ReasoningTokens || d.TokenPresence.CachedTokens || !d.TokenBreakdown.Valid() {
				t.Fatalf("plugin accounting changed: %+v", d)
			}
		}
	}
	for _, payload := range []string{
		`{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100,"thoughtsTokenCount":0,"totalTokenCount":1100}}`,
		`{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":100,"totalTokenCount":1100}}`,
	} {
		d := ParsePluginExecutorResponseUsage("gemini-enterprise", []byte(payload))
		if d.ReasoningTokens != 0 || d.TokenPresence.ReasoningTokens != strings.Contains(payload, "thoughtsTokenCount") || d.TokenPresence.CachedTokens {
			t.Fatal("plugin collapsed unknown into zero")
		}
	}
}

func TestObservePluginExecutorStreamUsageKeepsTopLevelResponsesTokensWhenServiceTierPresent(t *testing.T) {
	t.Parallel()

	var buffer StreamUsageBuffer
	payload := []byte("data: {\"type\":\"response.completed\",\"service_tier\":\"default\",\"usage\":{\"input_tokens\":34,\"output_tokens\":499,\"total_tokens\":533}}\n\n")
	ObservePluginExecutorStreamUsage("openai-response", payload, &buffer)
	detail, ok := buffer.Detail()
	if !ok {
		t.Fatal("expected observed usage")
	}
	if detail.InputTokens != 34 || detail.OutputTokens != 499 || detail.TotalTokens != 533 {
		t.Fatalf("detail = %+v, want input=34 output=499 total=533", detail)
	}
	if detail.ResponseServiceTier != "default" {
		t.Fatalf("response service tier = %q, want default", detail.ResponseServiceTier)
	}
}

func TestParsePluginExecutorResponseUsageKeepsTopLevelResponsesTokensWhenServiceTierPresent(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"id":"resp_1","object":"response","service_tier":"default","usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`)
	detail := ParsePluginExecutorResponseUsage("openai-response", payload)
	if detail.InputTokens != 34 || detail.OutputTokens != 499 || detail.TotalTokens != 533 {
		t.Fatalf("detail = %+v, want input=34 output=499 total=533", detail)
	}
	if detail.ResponseServiceTier != "default" {
		t.Fatalf("response service tier = %q, want default", detail.ResponseServiceTier)
	}
}

func TestParsePluginExecutorResponseUsageResponsesShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		protocol     string
		payload      string
		inputTokens  int64
		outputTokens int64
		totalTokens  int64
		tier         string
	}{
		{
			name:         "top-level usage without service tier",
			protocol:     "openai-response",
			payload:      `{"usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`,
			inputTokens:  34,
			outputTokens: 499,
			totalTokens:  533,
		},
		{
			name:         "codex protocol keeps top-level usage with service tier",
			protocol:     "codex",
			payload:      `{"service_tier":"default","usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`,
			inputTokens:  34,
			outputTokens: 499,
			totalTokens:  533,
			tier:         "default",
		},
		{
			name:         "nested response usage wins over top-level usage",
			protocol:     "openai-response",
			payload:      `{"service_tier":"priority","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"response":{"usage":{"input_tokens":18,"output_tokens":22,"total_tokens":40}}}`,
			inputTokens:  18,
			outputTokens: 22,
			totalTokens:  40,
			tier:         "priority",
		},
		{
			name:     "service tier only stays empty of tokens",
			protocol: "openai-response",
			payload:  `{"service_tier":"default"}`,
			tier:     "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			detail := ParsePluginExecutorResponseUsage(tt.protocol, []byte(tt.payload))
			if detail.InputTokens != tt.inputTokens || detail.OutputTokens != tt.outputTokens || detail.TotalTokens != tt.totalTokens {
				t.Fatalf("detail = %+v, want input=%d output=%d total=%d", detail, tt.inputTokens, tt.outputTokens, tt.totalTokens)
			}
			if detail.ResponseServiceTier != tt.tier {
				t.Fatalf("response service tier = %q, want %q", detail.ResponseServiceTier, tt.tier)
			}
		})
	}
}
