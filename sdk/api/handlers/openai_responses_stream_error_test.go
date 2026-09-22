package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestBuildOpenAIResponsesStreamErrorChunk(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusInternalServerError, "unexpected EOF", 0)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	errorObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("error is not an object: %v", payload["error"])
	}
	if errorObj["code"] != "internal_server_error" {
		t.Fatalf("code = %v, want %q", errorObj["code"], "internal_server_error")
	}
	if errorObj["message"] != "Internal server error" {
		t.Fatalf("message = %v, want %q", errorObj["message"], "Internal server error")
	}
	if payload["sequence_number"] != float64(0) {
		t.Fatalf("sequence_number = %v, want %v", payload["sequence_number"], 0)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkExtractsHTTPErrorBody(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(
		http.StatusInternalServerError,
		`{"error":{"message":"oops","type":"server_error","code":"internal_server_error"}}`,
		0,
	)
	var payload map[string]any
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["type"] != "error" {
		t.Fatalf("type = %v, want %q", payload["type"], "error")
	}
	errorObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("error is not an object: %v", payload["error"])
	}
	if errorObj["code"] != "internal_server_error" {
		t.Fatalf("code = %v, want %q", errorObj["code"], "internal_server_error")
	}
	if errorObj["message"] != "Internal server error" {
		t.Fatalf("message = %v, want %q", errorObj["message"], "Internal server error")
	}
	if errorObj["type"] != "server_error" {
		t.Fatalf("error.type = %v, want %q", errorObj["type"], "server_error")
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkPreservesNestedError(t *testing.T) {
	errText := `{"error":{"type":"invalid_request","code":"cyber_policy","message":"This content was flagged for possible cybersecurity risk.","param":null}}`
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadRequest, errText, 2)
	var payload struct {
		Type           string         `json:"type"`
		Error          map[string]any `json:"error"`
		SequenceNumber int            `json:"sequence_number"`
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Type != "error" {
		t.Fatalf("type = %q, want %q", payload.Type, "error")
	}
	if payload.SequenceNumber != 2 {
		t.Fatalf("sequence_number = %d, want 2", payload.SequenceNumber)
	}
	if payload.Error["type"] != "invalid_request" {
		t.Fatalf("error.type = %v, want invalid_request", payload.Error["type"])
	}
	if payload.Error["code"] != "cyber_policy" {
		t.Fatalf("error.code = %v, want cyber_policy", payload.Error["code"])
	}
	if payload.Error["message"] != FixedErrorMessage(http.StatusBadRequest) {
		t.Fatalf("error.message = %v", payload.Error["message"])
	}
	if param, exists := payload.Error["param"]; !exists || param != nil {
		t.Fatalf("error.param = %v, want nil", param)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkSanitizesCustomAndEmptyFields(t *testing.T) {
	// Empty upstream objects still produce a meaningful safe error.
	emptyChunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadRequest, `{"error":{}}`, 0)
	var emptyPayload struct {
		Type           string         `json:"type"`
		Error          map[string]any `json:"error"`
		SequenceNumber int            `json:"sequence_number"`
	}
	if err := json.Unmarshal(emptyChunk, &emptyPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if emptyPayload.Error["message"] != FixedErrorMessage(http.StatusBadRequest) {
		t.Fatalf("expected fixed safe error, got %v", emptyPayload.Error)
	}

	// Preserve classifications while removing arbitrary upstream diagnostic fields.
	customText := `{"error":{"type":"custom_type","code":"custom_code","custom_key":"custom_val","is_flag":true,"count":42}}`
	customChunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadRequest, customText, 5)
	var customPayload struct {
		Type           string         `json:"type"`
		Error          map[string]any `json:"error"`
		SequenceNumber int            `json:"sequence_number"`
	}
	if err := json.Unmarshal(customChunk, &customPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if customPayload.SequenceNumber != 5 {
		t.Fatalf("sequence_number = %d, want 5", customPayload.SequenceNumber)
	}
	if customPayload.Error["type"] != "custom_type" || customPayload.Error["code"] != "custom_code" {
		t.Fatalf("classification was lost: %v", customPayload.Error)
	}
	for _, key := range []string{"custom_key", "is_flag", "count"} {
		if _, exists := customPayload.Error[key]; exists {
			t.Fatalf("upstream diagnostic %q leaked: %v", key, customPayload.Error)
		}
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkPrioritizesPayloadSequenceNumber(t *testing.T) {
	errText := `{"error":{"type":"invalid_request","code":"blocked"},"sequence_number":7}`
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadRequest, errText, 2)
	var payload struct {
		Type           string         `json:"type"`
		Error          map[string]any `json:"error"`
		SequenceNumber int            `json:"sequence_number"`
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.SequenceNumber != 7 {
		t.Fatalf("sequence_number = %d, want 7 (from payload)", payload.SequenceNumber)
	}
}

func TestBuildOpenAIResponsesStreamFailedChunkPreservesNestedError(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamFailedChunk(
		http.StatusBadRequest,
		`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked","param":null}}`,
		0,
	)

	var payload struct {
		Type           string `json:"type"`
		SequenceNumber int    `json:"sequence_number"`
		Response       struct {
			Status string `json:"status"`
			Error  struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Type != "response.failed" {
		t.Fatalf("type = %q, want %q", payload.Type, "response.failed")
	}
	if payload.SequenceNumber != 0 {
		t.Fatalf("sequence_number = %d, want 0", payload.SequenceNumber)
	}
	if payload.Response.Status != "failed" {
		t.Fatalf("response.status = %q, want %q", payload.Response.Status, "failed")
	}
	if payload.Response.Error.Type != "invalid_request" {
		t.Fatalf("response.error.type = %q, want %q", payload.Response.Error.Type, "invalid_request")
	}
	if payload.Response.Error.Code != "cyber_policy" {
		t.Fatalf("response.error.code = %q, want %q", payload.Response.Error.Code, "cyber_policy")
	}
	wantMessage := FixedErrorMessage(http.StatusBadRequest)
	if payload.Response.Error.Message != wantMessage {
		t.Fatalf("response.error.message = %q, want %q", payload.Response.Error.Message, wantMessage)
	}
}

func TestBuildOpenAIResponsesStreamFailedChunkPrioritizesPayloadSequenceNumber(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamFailedChunk(
		http.StatusBadRequest,
		`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"},"sequence_number":7}`,
		2,
	)

	var payload struct {
		Type           string `json:"type"`
		SequenceNumber int    `json:"sequence_number"`
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.SequenceNumber != 7 {
		t.Fatalf("sequence_number = %d, want 7 (from payload over arg 2)", payload.SequenceNumber)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkPreservesLargeIntPrecision(t *testing.T) {
	errText := `{"error":{"type":"invalid_request","code":"blocked"},"sequence_number":9007199254740993}`
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadRequest, errText, 0)
	raw := string(chunk)
	if !strings.Contains(raw, "9007199254740993") {
		t.Fatalf("large integer precision was lost in error chunk: %s", raw)
	}
	if strings.Contains(raw, "9007199254740992") {
		t.Fatalf("large integer was corrupted by float64 in error chunk: %s", raw)
	}

	failedChunk := BuildOpenAIResponsesStreamFailedChunk(http.StatusBadRequest, errText, 0)
	failedRaw := string(failedChunk)
	if !strings.Contains(failedRaw, "9007199254740993") {
		t.Fatalf("large integer precision was lost in failed chunk: %s", failedRaw)
	}
	if strings.Contains(failedRaw, "9007199254740992") {
		t.Fatalf("large integer was corrupted by float64 in failed chunk: %s", failedRaw)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkRequestTimeoutIsServerError(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(http.StatusRequestTimeout, "stream disconnected before completion", 0)
	var payload struct {
		Type           string         `json:"type"`
		Error          map[string]any `json:"error"`
		SequenceNumber int            `json:"sequence_number"`
	}
	if errUnmarshal := json.Unmarshal(chunk, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if payload.Type != "error" {
		t.Fatalf("type = %q, want %q", payload.Type, "error")
	}
	if got := payload.Error["code"]; got != "request_timeout" {
		t.Fatalf("error.code = %v, want %q", got, "request_timeout")
	}
	if got := payload.Error["type"]; got != "server_error" {
		t.Fatalf("error.type = %v, want %q", got, "server_error")
	}

	failedChunk := BuildOpenAIResponsesStreamFailedChunk(http.StatusRequestTimeout, "stream disconnected before completion", 0)
	var failedPayload struct {
		Type     string `json:"type"`
		Response struct {
			Status string         `json:"status"`
			Error  map[string]any `json:"error"`
		} `json:"response"`
	}
	if errUnmarshal := json.Unmarshal(failedChunk, &failedPayload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if got := failedPayload.Response.Error["code"]; got != "request_timeout" {
		t.Fatalf("failed.response.error.code = %v, want %q", got, "request_timeout")
	}
	if got := failedPayload.Response.Error["type"]; got != "server_error" {
		t.Fatalf("failed.response.error.type = %v, want %q", got, "server_error")
	}
}
