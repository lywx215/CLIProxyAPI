package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

func TestGeminiResponseDiagnosticsPreserveShortOutputEvidence(t *testing.T) {
	request := []byte(`{"generationConfig":{"maxOutputTokens":4096},"contents":[{"parts":[{"text":"private prompt"}]}]}`)
	response := []byte(`{"modelVersion":"gemini-test","candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"private thought","thought":true},{"text":"repeated answer","thoughtSignature":"private signature"},{"functionCall":{"name":"private tool","args":{"key":"private arguments"}}}]}}],"usageMetadata":{"promptTokenCount":15886,"candidatesTokenCount":87,"thoughtsTokenCount":13,"totalTokenCount":15986}}`)
	summary := geminiNonStreamingResponseSummary(request, response)
	for key, want := range map[string]int64{"candidate_tokens": 87, "thought_tokens": 13, "output_tokens": 100, "prompt_tokens": 15886, "max_output_tokens": 4096} {
		if got := summary[key]; got != want {
			t.Errorf("%s = %v, want %d", key, got, want)
		}
	}
	detail := summary["candidates"].([]log.Fields)[0]
	digest := sha256.Sum256([]byte("repeated answer"))
	if detail["finish_reason"] != "STOP" || detail["text_sha256"] != hex.EncodeToString(digest[:]) || detail["tool_calls"] != 1 {
		t.Fatalf("unexpected candidate diagnostics: %v", detail)
	}
	encoded, errMarshal := json.Marshal(summary)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	entry := log.NewEntry(log.New())
	entry.Data["request_id"] = "test-87"
	entry.Data["response_summary"] = string(encoded)
	formatted, errFormat := (&logging.LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	for _, want := range []string{"[test-87]", `"candidate_tokens":87`, `"finish_reason":"STOP"`, `"max_output_tokens":4096`} {
		if !strings.Contains(string(formatted), want) {
			t.Errorf("formatted log missing %s: %s", want, formatted)
		}
	}
	for _, secret := range []string{"private prompt", "private thought", "repeated answer", "private signature", "private tool", "private arguments"} {
		if strings.Contains(string(formatted), secret) {
			t.Errorf("diagnostics leaked content: %q", secret)
		}
	}
}

func TestGeminiResponseDiagnosticsDistinguishCompletionStates(t *testing.T) {
	for _, tc := range []struct {
		name, response, reason string
		valid, usage, blocked  bool
	}{
		{name: "truncated", response: `{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"candidatesTokenCount":87}}`, reason: "MAX_TOKENS", valid: true, usage: true},
		{name: "blocked", response: `{"promptFeedback":{"blockReason":"SAFETY"}}`, valid: true, blocked: true},
		{name: "missing usage", response: `{"candidates":[{"finishReason":"STOP"}]}`, reason: "STOP", valid: true},
		{name: "malformed", response: `<html>private upstream error</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := geminiNonStreamingResponseSummary(nil, []byte(tc.response))
			if summary["valid_json"] != tc.valid {
				t.Fatalf("valid_json = %v, want %v", summary["valid_json"], tc.valid)
			}
			if !tc.valid {
				return
			}
			if summary["usage_present"] != tc.usage {
				t.Errorf("usage_present = %v, want %v", summary["usage_present"], tc.usage)
			}
			if !tc.usage {
				if _, exists := summary["output_tokens"]; exists {
					t.Error("missing usage must not be reported as zero output tokens")
				}
			}
			if tc.blocked && summary["block_reason"] != "SAFETY" {
				t.Errorf("missing block reason: %v", summary)
			}
			if tc.reason != "" && summary["candidates"].([]log.Fields)[0]["finish_reason"] != tc.reason {
				t.Errorf("missing finish reason: %v", summary)
			}
		})
	}
}
