package handlers

import (
	"context"
	"testing"
	"time"
)

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

func TestSpeedThrottleFirstChunkKeepsRequestStartForCumulativeRate(t *testing.T) {
	throttler := &RequestThrottler{
		targetRate: 1,
		ttftDelay:  1,
	}
	requestStart := time.Now().Add(-10 * time.Second)

	if ok := throttler.ThrottleFirstChunk(context.Background(), requestStart); !ok {
		t.Fatal("ThrottleFirstChunk() = false, want true")
	}
	if !throttler.startTime.Equal(requestStart) {
		t.Fatalf("startTime = %v, want %v", throttler.startTime, requestStart)
	}
}

func TestSpeedThrottleSlowRequestDoesNotThrottleAgainOnFinalUsage(t *testing.T) {
	throttler := &RequestThrottler{
		targetRate: 1,
		ttftDelay:  1,
	}
	requestStart := time.Now().Add(-10 * time.Second)
	if ok := throttler.ThrottleFirstChunk(context.Background(), requestStart); !ok {
		t.Fatal("ThrottleFirstChunk() = false, want true")
	}

	start := time.Now()
	chunk := []byte(`{"usageMetadata":{"candidatesTokenCount":7,"thoughtsTokenCount":3}}`)
	if ok := throttler.ThrottleChunk(context.Background(), chunk); !ok {
		t.Fatal("ThrottleChunk() = false, want true")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("ThrottleChunk() slept for %v, want no extra throttle", elapsed)
	}
}
func TestSpeedThrottleFirstChunkWithPayloadAccountsGeminiCandidateTokens(t *testing.T) {
	throttler := &RequestThrottler{
		targetRate: 1,
		ttftDelay:  1,
	}
	requestStart := time.Now().Add(-10 * time.Second)
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefgh"}]}}]}`)

	if ok := throttler.ThrottleFirstChunkWithPayload(context.Background(), requestStart, chunk); !ok {
		t.Fatal("ThrottleFirstChunkWithPayload() = false, want true")
	}
	if !throttler.firstChunkSent {
		t.Fatal("firstChunkSent = false, want true")
	}
	if throttler.totalTokens != 2 {
		t.Fatalf("totalTokens = %d, want 2", throttler.totalTokens)
	}
	if !throttler.startTime.Equal(requestStart) {
		t.Fatalf("startTime = %v, want %v", throttler.startTime, requestStart)
	}
}

func TestSpeedThrottleEstimateChunkTokenTotalIncludesGeminiThoughts(t *testing.T) {
	chunk := []byte(`data: {"usageMetadata":{"candidatesTokenCount":30,"thoughtsTokenCount":12}}`)
	if got := EstimateChunkTokenTotal(chunk); got != 42 {
		t.Fatalf("EstimateChunkTokenTotal() = %d, want 42", got)
	}
}

func TestSpeedThrottleChunkUsesGeminiUsageMetadataTotal(t *testing.T) {
	throttler := &RequestThrottler{
		targetRate:     1,
		startTime:      time.Now().Add(-10 * time.Second),
		totalTokens:    2,
		firstChunkSent: true,
	}
	chunk := []byte(`{"usageMetadata":{"candidatesTokenCount":7,"thoughtsTokenCount":3}}`)

	if ok := throttler.ThrottleChunk(context.Background(), chunk); !ok {
		t.Fatal("ThrottleChunk() = false, want true")
	}
	if throttler.totalTokens != 10 {
		t.Fatalf("totalTokens = %d, want 10", throttler.totalTokens)
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
		{name: "wrapped gemini usage metadata with thoughts", resp: `{"response":{"usageMetadata":{"candidatesTokenCount":567,"thoughtsTokenCount":12}}}`, want: 579},
		{name: "gemini usage metadata with thoughts", resp: `{"usageMetadata":{"candidatesTokenCount":678,"thoughtsTokenCount":13}}`, want: 691},
		{name: "wrapped gemini snake usage metadata with thoughts", resp: `{"response":{"usage_metadata":{"candidatesTokenCount":789,"thoughtsTokenCount":14}}}`, want: 803},
		{name: "gemini snake usage metadata with thoughts", resp: `{"usage_metadata":{"candidatesTokenCount":890,"thoughtsTokenCount":15}}`, want: 905},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateNonStreamingTokens([]byte(tc.resp)); got != tc.want {
				t.Fatalf("EstimateNonStreamingTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}
