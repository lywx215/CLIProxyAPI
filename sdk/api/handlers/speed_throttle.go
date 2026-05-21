package handlers

import (
	"context"
	"math/rand"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

// SpeedThrottleDefaults
const (
	defaultMinTokensPerSecond  = 70
	defaultMaxTokensPerSecond  = 100
	defaultMinFirstTokenDelayMs = 2500
	defaultMaxFirstTokenDelayMs = 3000
)

// RequestThrottler controls the token emission rate for a single request.
// Each request should create its own RequestThrottler instance via NewRequestThrottler.
type RequestThrottler struct {
	targetRate     float64       // tokens per second for this request
	ttftDelay      time.Duration // TTFT delay for this request
	startTime      time.Time     // when the first chunk was sent
	totalTokens    int           // total tokens sent so far
	firstChunkSent bool          // whether the first chunk has been sent
}

// NewRequestThrottler creates a throttler for a single request with randomized parameters.
// Returns nil if throttling is disabled.
func NewRequestThrottler(cfg *config.SDKConfig) *RequestThrottler {
	if cfg == nil || !cfg.SpeedThrottle.Enabled {
		return nil
	}

	minRate := cfg.SpeedThrottle.MinTokensPerSecond
	if minRate <= 0 {
		minRate = defaultMinTokensPerSecond
	}
	maxRate := cfg.SpeedThrottle.MaxTokensPerSecond
	if maxRate <= 0 {
		maxRate = defaultMaxTokensPerSecond
	}
	if maxRate < minRate {
		maxRate = minRate
	}

	minTTFT := cfg.SpeedThrottle.MinFirstTokenDelayMs
	if minTTFT <= 0 {
		minTTFT = defaultMinFirstTokenDelayMs
	}
	maxTTFT := cfg.SpeedThrottle.MaxFirstTokenDelayMs
	if maxTTFT <= 0 {
		maxTTFT = defaultMaxFirstTokenDelayMs
	}
	if maxTTFT < minTTFT {
		maxTTFT = minTTFT
	}

	// Randomize target rate within [minRate, maxRate]
	targetRate := float64(minRate)
	if maxRate > minRate {
		targetRate = float64(minRate) + rand.Float64()*float64(maxRate-minRate)
	}

	// Randomize TTFT within [minTTFT, maxTTFT]
	ttftMs := minTTFT
	if maxTTFT > minTTFT {
		ttftMs = minTTFT + rand.Intn(maxTTFT-minTTFT+1)
	}

	return &RequestThrottler{
		targetRate: targetRate,
		ttftDelay:  time.Duration(ttftMs) * time.Millisecond,
	}
}

// ThrottleFirstChunk enforces the TTFT delay before the first chunk is emitted.
// It accounts for time already spent waiting for the upstream response.
// Returns false if the context was cancelled during the wait.
func (t *RequestThrottler) ThrottleFirstChunk(ctx context.Context, requestStartTime time.Time) bool {
	if t == nil {
		return true
	}

	elapsed := time.Since(requestStartTime)
	remaining := t.ttftDelay - elapsed
	if remaining <= 0 {
		t.startTime = time.Now()
		t.firstChunkSent = true
		return true
	}

	select {
	case <-ctx.Done():
		return false
	case <-time.After(remaining):
		t.startTime = time.Now()
		t.firstChunkSent = true
		return true
	}
}

// ThrottleChunk enforces the target token rate for subsequent chunks.
// Call this BEFORE writing each chunk (after the first).
// Returns false if the context was cancelled during the wait.
func (t *RequestThrottler) ThrottleChunk(ctx context.Context, chunk []byte) bool {
	if t == nil || !t.firstChunkSent {
		return true
	}

	tokens := EstimateChunkTokens(chunk)
	if tokens <= 0 {
		return true
	}

	t.totalTokens += tokens

	// Calculate expected time for this many tokens at target rate
	expectedDuration := time.Duration(float64(t.totalTokens) / t.targetRate * float64(time.Second))
	actualDuration := time.Since(t.startTime)
	sleepDuration := expectedDuration - actualDuration

	if sleepDuration <= 0 {
		return true
	}

	select {
	case <-ctx.Done():
		return false
	case <-time.After(sleepDuration):
		return true
	}
}

// ThrottleNonStreaming enforces a delay for non-streaming responses based on
// the estimated token count and the TTFT requirement.
// Returns false if the context was cancelled during the wait.
func (t *RequestThrottler) ThrottleNonStreaming(ctx context.Context, requestStartTime time.Time, tokenCount int) bool {
	if t == nil {
		return true
	}

	if tokenCount <= 0 {
		tokenCount = 1
	}

	// Target time = max(TTFT delay, tokens / rate)
	rateDelay := time.Duration(float64(tokenCount) / t.targetRate * float64(time.Second))
	targetDelay := t.ttftDelay
	if rateDelay > targetDelay {
		targetDelay = rateDelay
	}

	elapsed := time.Since(requestStartTime)
	remaining := targetDelay - elapsed
	if remaining <= 0 {
		return true
	}

	select {
	case <-ctx.Done():
		return false
	case <-time.After(remaining):
		return true
	}
}

// EstimateChunkTokens estimates the number of tokens in a streaming chunk.
// It tries to extract the actual text content to avoid overestimating tokens
// based on JSON formatting overhead.
func EstimateChunkTokens(chunk []byte) int {
	if len(chunk) == 0 {
		return 0
	}

	// Strip "data: " prefix if present for cleaner parsing/checking
	payload := chunk
	if len(payload) >= 6 && string(payload[:6]) == "data: " {
		payload = payload[6:]
	} else if len(payload) >= 5 && string(payload[:5]) == "data:" {
		payload = payload[5:]
	}

	var textLen int

	// Try Gemini format: candidates[0].content.parts[0].text
	if text := gjson.GetBytes(payload, "candidates.0.content.parts.0.text"); text.Exists() {
		textLen = len(text.String())
	} else if text := gjson.GetBytes(payload, "choices.0.delta.content"); text.Exists() {
		// Try OpenAI format: choices[0].delta.content
		textLen = len(text.String())
	} else if text := gjson.GetBytes(payload, "response.output.0.content.0.text"); text.Exists() {
		// Try Responses format
		textLen = len(text.String())
	}

	// If we found text, use it for estimation
	if textLen > 0 {
		tokens := textLen / 4
		if tokens <= 0 {
			tokens = 1
		}
		return tokens
	}

	// Skip leading whitespace on payload for JSON check
	for len(payload) > 0 && (payload[0] == ' ' || payload[0] == '\t' || payload[0] == '\n' || payload[0] == '\r') {
		payload = payload[1:]
	}

	// If it's a JSON object or array, but we found no text, don't count it.
	// E.g., usage metadata chunks, tool calls, or empty chunks.
	if len(payload) > 0 && (payload[0] == '{' || payload[0] == '[') {
		return 0
	}
	
	// Also ignore "[DONE]" chunks
	if len(payload) >= 6 && string(payload[:6]) == "[DONE]" {
		return 0
	}

	// Fallback for purely non-JSON raw chunks
	tokens := len(chunk) / 4
	if tokens <= 0 {
		tokens = 1
	}
	return tokens
}

// EstimateNonStreamingTokens extracts or estimates the completion token count
// from a non-streaming response body. It tries Gemini format first
// (usageMetadata.candidatesTokenCount), then OpenAI format (usage.completion_tokens),
// and falls back to a byte-length estimate.
func EstimateNonStreamingTokens(resp []byte) int {
	if len(resp) == 0 {
		return 1
	}

	// Try Gemini format: usageMetadata.candidatesTokenCount
	if idx := findJSONIntField(resp, "candidatesTokenCount"); idx > 0 {
		return idx
	}

	// Try OpenAI format: usage.completion_tokens
	if idx := findJSONIntField(resp, "completion_tokens"); idx > 0 {
		return idx
	}

	// Fallback: estimate from response body size
	return EstimateChunkTokens(resp)
}

// findJSONIntField is a lightweight helper to extract an integer value
// from a JSON field without full parsing. Returns 0 if not found.
func findJSONIntField(data []byte, field string) int {
	needle := []byte(`"` + field + `":`)
	idx := bytesIndex(data, needle)
	if idx < 0 {
		// Try with space after colon
		needle = []byte(`"` + field + `": `)
		idx = bytesIndex(data, needle)
		if idx < 0 {
			return 0
		}
	}

	// Move past the field name and colon
	start := idx + len(needle)
	// Skip whitespace
	for start < len(data) && (data[start] == ' ' || data[start] == '\t') {
		start++
	}
	if start >= len(data) {
		return 0
	}

	// Read digits
	end := start
	for end < len(data) && data[end] >= '0' && data[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}

	result := 0
	for i := start; i < end; i++ {
		result = result*10 + int(data[i]-'0')
	}
	return result
}

func bytesIndex(data, needle []byte) int {
	for i := 0; i <= len(data)-len(needle); i++ {
		match := true
		for j := range needle {
			if data[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
