package handlers

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

var speedThrottleBenchmarkSink int

func BenchmarkSpeedThrottleDisabled(b *testing.B) {
	var throttler *RequestThrottler
	ctx := context.Background()
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop"}]}}]}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !throttler.ThrottleChunk(ctx, chunk) {
			b.Fatal("ThrottleChunk() = false, want true")
		}
	}
}

func BenchmarkSpeedThrottleEstimateChunkTokens(b *testing.B) {
	tests := []struct {
		name  string
		chunk []byte
	}{
		{name: "gemini-visible", chunk: []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop"}]}}]}`)},
		{name: "gemini-thought-summary", chunk: []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop","thought":true}]}}]}`)},
		{name: "openai-sse", chunk: []byte(`data: {"choices":[{"delta":{"content":"abcdefghijklmnop"}}]}`)},
		{name: "usage-only", chunk: []byte(`{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}`)},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				speedThrottleBenchmarkSink = EstimateChunkTokens(tc.chunk)
			}
		})
	}
}

func BenchmarkSpeedThrottleEnabledChunk(b *testing.B) {
	tests := []struct {
		name  string
		chunk []byte
	}{
		{name: "gemini-visible", chunk: []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop"}]}}]}`)},
		{name: "gemini-thought-summary", chunk: []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop","thought":true}]}}]}`)},
		{name: "openai-sse", chunk: []byte(`data: {"choices":[{"delta":{"content":"abcdefghijklmnop"}}]}`)},
		{name: "usage-only", chunk: []byte(`{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}`)},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			throttler := &RequestThrottler{
				targetRate:     1_000_000_000_000,
				startTime:      time.Now().Add(-time.Hour),
				firstChunkSent: true,
			}
			ctx := context.Background()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !throttler.ThrottleChunk(ctx, tc.chunk) {
					b.Fatal("ThrottleChunk() = false, want true")
				}
			}
		})
	}
}

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

func TestSpeedThrottleEstimateChunkTokensIncludesThoughtSummaries(t *testing.T) {
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijkl","thought":true}]}}]}`)
	if got := EstimateChunkTokens(chunk); got != 3 {
		t.Fatalf("EstimateChunkTokens() = %d, want 3", got)
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

func TestSpeedThrottleFirstChunkWithUsageOnlyDoesNotCountReportedTokens(t *testing.T) {
	throttler := &RequestThrottler{
		targetRate: 1,
		ttftDelay:  1,
	}
	requestStart := time.Now().Add(-10 * time.Second)
	chunk := []byte(`{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}`)

	if ok := throttler.ThrottleFirstChunkWithPayload(context.Background(), requestStart, chunk); !ok {
		t.Fatal("ThrottleFirstChunkWithPayload() = false, want true")
	}
	if throttler.totalTokens != 0 {
		t.Fatalf("totalTokens = %d, want 0", throttler.totalTokens)
	}
}

func TestSpeedThrottleEstimateChunkTokenTotalIncludesGeminiThoughts(t *testing.T) {
	chunk := []byte(`data: {"usageMetadata":{"candidatesTokenCount":30,"thoughtsTokenCount":12}}`)
	if got := EstimateChunkTokenTotal(chunk); got != 42 {
		t.Fatalf("EstimateChunkTokenTotal() = %d, want 42", got)
	}
}

func TestSpeedThrottleChunkIgnoresGeminiUsageMetadataTotal(t *testing.T) {
	throttler := &RequestThrottler{
		targetRate:     1,
		startTime:      time.Now(),
		totalTokens:    2,
		firstChunkSent: true,
	}
	chunk := []byte(`{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if ok := throttler.ThrottleChunk(ctx, chunk); !ok {
		t.Fatal("ThrottleChunk() = false, want true")
	}
	if throttler.totalTokens != 2 {
		t.Fatalf("totalTokens = %d, want 2", throttler.totalTokens)
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

func TestSpeedThrottleMetadataOnlyChunksBypassCancelledContext(t *testing.T) {
	chunks := []string{
		`{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}`,
		`{"usage_metadata":{"candidates_token_count":7000,"thoughts_token_count":3000}}`,
		`{"response":{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}}`,
		`[{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}]`,
		`{"candidates":[{"content":{"parts":[{"thoughtSignature":"signature"}]}}]}`,
		`data: [DONE]`,
	}

	for _, chunk := range chunks {
		throttler := &RequestThrottler{
			targetRate:     1,
			startTime:      time.Now(),
			totalTokens:    2,
			firstChunkSent: true,
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if ok := throttler.ThrottleChunk(ctx, []byte(chunk)); !ok {
			t.Fatalf("ThrottleChunk(%s) = false, want true", chunk)
		}
		if throttler.totalTokens != 2 {
			t.Fatalf("ThrottleChunk(%s) totalTokens = %d, want 2", chunk, throttler.totalTokens)
		}
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
		{name: "wrapped gemini usage metadata includes thoughts", resp: `{"response":{"usageMetadata":{"candidatesTokenCount":567,"thoughtsTokenCount":12}}}`, want: 579},
		{name: "gemini usage metadata includes thoughts", resp: `{"usageMetadata":{"candidatesTokenCount":678,"thoughtsTokenCount":13}}`, want: 691},
		{name: "wrapped gemini snake usage metadata includes thoughts", resp: `{"response":{"usage_metadata":{"candidatesTokenCount":789,"thoughtsTokenCount":14}}}`, want: 803},
		{name: "gemini snake usage metadata includes thoughts", resp: `{"usage_metadata":{"candidatesTokenCount":890,"thoughtsTokenCount":15}}`, want: 905},
		{name: "gemini array usage metadata includes thoughts", resp: `[{"usageMetadata":{"candidatesTokenCount":901,"thoughtsTokenCount":16}}]`, want: 917},
		{name: "gemini usage takes precedence over text", resp: `{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop"},{"text":"qrstuvwxyz","thought":true}]}}],"usageMetadata":{"candidatesTokenCount":999,"thoughtsTokenCount":999}}`, want: 1998},
		{name: "gemini thinking only", resp: `{"usageMetadata":{"thoughtsTokenCount":2080}}`, want: 2080},
		{name: "gemini snake token fields", resp: `{"usage_metadata":{"candidates_token_count":1836,"thoughts_token_count":244}}`, want: 2080},
		{name: "chat usage takes precedence over text without double counting reasoning", resp: `{"choices":[{"message":{"content":"hello"}}],"usage":{"completion_tokens":2080,"completion_tokens_details":{"reasoning_tokens":1800}}}`, want: 2080},
		{name: "responses usage takes precedence over text", resp: `{"output":[{"content":[{"text":"hello"}]}],"usage":{"output_tokens":2080,"output_tokens_details":{"reasoning_tokens":1800}}}`, want: 2080},
		{name: "wrapped responses usage takes precedence over text", resp: `{"response":{"output":[{"content":[{"text":"hello"}]}],"usage":{"output_tokens":2080}}}`, want: 2080},
		{name: "tool calls use output usage", resp: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"test","arguments":"{}"}}]}}],"usage":{"completion_tokens":123}}`, want: 123},
		{name: "text fallback without usage", resp: `{"choices":[{"message":{"content":"abcdefghijklmnop"}}]}`, want: 4},
		{name: "text fallback with empty usage", resp: `{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop"}]}}],"usageMetadata":{}}`, want: 4},
		{name: "input usage is not output", resp: `{"usage":{"prompt_tokens":15886,"total_tokens":15886}}`, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateNonStreamingTokens([]byte(tc.resp)); got != tc.want {
				t.Fatalf("EstimateNonStreamingTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSpeedThrottleNonStreamingDuration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream time.Duration
		tokens   int
		want     time.Duration
	}{
		{name: "reported output rate", upstream: 8 * time.Second, tokens: 2080, want: 20800 * time.Millisecond},
		{name: "first token delay", tokens: 10, want: 3500 * time.Millisecond},
		{name: "slow upstream needs no extra delay", upstream: 30 * time.Second, tokens: 2080, want: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				throttler := &RequestThrottler{targetRate: 100, ttftDelay: 3500 * time.Millisecond}
				start := time.Now().Add(-tc.upstream)
				if !throttler.ThrottleNonStreaming(context.Background(), start, tc.tokens) {
					t.Fatal("ThrottleNonStreaming() = false, want true")
				}
				if got := time.Since(start); got != tc.want {
					t.Fatalf("request duration = %v, want %v", got, tc.want)
				}
			})
		})
	}
}
