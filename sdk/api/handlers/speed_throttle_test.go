package handlers

import "testing"

func TestSpeedThrottleEstimateChunkTokensRecognizesResponsesOutputTextDelta(t *testing.T) {
	chunk := []byte(`data: {"type":"response.output_text.delta","delta":"abcdefghijkl"}`)
	if got := EstimateChunkTokens(chunk); got != 3 {
		t.Fatalf("EstimateChunkTokens() = %d, want 3", got)
	}
}

func TestSpeedThrottleEstimateChunkTokensRecognizesResponsesEventDataFrame(t *testing.T) {
	chunk := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"abcdefghijkl"}` + "\n\n")
	if got := EstimateChunkTokens(chunk); got != 3 {
		t.Fatalf("EstimateChunkTokens() = %d, want 3", got)
	}
}

func TestSpeedThrottleEstimateChunkTokensSumsOpenAIChoices(t *testing.T) {
	chunk := []byte(`data: {"choices":[{"delta":{"content":"abcdefgh"}},{"delta":{"content":"ijklmnop"}}]}`)
	if got := EstimateChunkTokens(chunk); got != 4 {
		t.Fatalf("EstimateChunkTokens() = %d, want 4", got)
	}
}

func TestSpeedThrottleEstimateChunkTokensSumsGeminiCandidates(t *testing.T) {
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefgh"},{"text":"ijkl"}]}},{"content":{"parts":[{"text":"mnop"}]}}]}`)
	if got := EstimateChunkTokens(chunk); got != 4 {
		t.Fatalf("EstimateChunkTokens() = %d, want 4", got)
	}
}

func TestSpeedThrottleEstimateChunkTokensIgnoresDoneAndMetadataOnlyJSON(t *testing.T) {
	if got := EstimateChunkTokens([]byte("data: [DONE]")); got != 0 {
		t.Fatalf("EstimateChunkTokens([DONE]) = %d, want 0", got)
	}
	if got := EstimateChunkTokens([]byte(`{"usage":{"output_tokens":123}}`)); got != 0 {
		t.Fatalf("EstimateChunkTokens(metadata) = %d, want 0", got)
	}
}

func TestSpeedThrottleEstimateNonStreamingTokensRecognizesOutputTokenFields(t *testing.T) {
	tests := []struct {
		name string
		resp string
		want int
	}{
		{name: "responses usage output tokens", resp: `{"response":{"usage":{"output_tokens":123}}}`, want: 123},
		{name: "usage output tokens", resp: `{"usage":{"output_tokens":234}}`, want: 234},
		{name: "responses usage completion tokens", resp: `{"response":{"usage":{"completion_tokens":345}}}`, want: 345},
		{name: "usage completion tokens", resp: `{"usage":{"completion_tokens":456}}`, want: 456},
		{name: "wrapped gemini usage metadata", resp: `{"response":{"usageMetadata":{"candidatesTokenCount":567}}}`, want: 567},
		{name: "gemini usage metadata", resp: `{"usageMetadata":{"candidatesTokenCount":678}}`, want: 678},
		{name: "wrapped gemini snake usage metadata", resp: `{"response":{"usage_metadata":{"candidatesTokenCount":789}}}`, want: 789},
		{name: "gemini snake usage metadata", resp: `{"usage_metadata":{"candidatesTokenCount":890}}`, want: 890},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateNonStreamingTokens([]byte(tc.resp)); got != tc.want {
				t.Fatalf("EstimateNonStreamingTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}
