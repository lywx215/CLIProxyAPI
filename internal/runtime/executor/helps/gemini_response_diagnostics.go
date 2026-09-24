package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// LogGeminiNonStreamingResponse records upstream completion diagnostics before
// translation. It excludes request/response text, signatures, and tool arguments.
func LogGeminiNonStreamingResponse(ctx context.Context, requestedModel, upstreamModel string, request, response []byte) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	summary := geminiNonStreamingResponseSummary(request, response)
	summary["requested_model"] = logging.SafeDiagnosticForLog(requestedModel)
	summaryJSON, errMarshal := json.Marshal(summary)
	if errMarshal != nil {
		return
	}
	LogWithRequestID(ctx).WithFields(log.Fields{
		"provider":         "gemini",
		"model":            logging.SafeDiagnosticForLog(upstreamModel),
		"response_summary": string(summaryJSON),
	}).Debug("gemini non-streaming response summary")
}

func geminiNonStreamingResponseSummary(request, response []byte) log.Fields {
	summary := log.Fields{"valid_json": gjson.ValidBytes(response), "response_bytes": len(response)}
	if limit := gjson.GetBytes(request, "generationConfig.maxOutputTokens"); limit.Type == gjson.Number {
		summary["max_output_tokens"] = limit.Int()
	}
	if !gjson.ValidBytes(response) {
		return summary
	}
	root := gjson.ParseBytes(response)
	if wrapped := root.Get("response"); wrapped.IsObject() {
		root = wrapped
	}
	summary["response_model"] = logging.SafeDiagnosticForLog(root.Get("modelVersion").String())
	summary["error_present"] = root.Get("error").IsObject()
	summary["block_reason"] = logging.SafeDiagnosticForLog(root.Get("promptFeedback.blockReason").String())
	usage := root.Get("usageMetadata")
	summary["usage_present"] = usage.IsObject()
	for field, path := range map[string]string{
		"prompt_tokens": "promptTokenCount", "candidate_tokens": "candidatesTokenCount",
		"thought_tokens": "thoughtsTokenCount", "total_tokens": "totalTokenCount",
	} {
		if value := usage.Get(path); value.Type == gjson.Number {
			summary[field] = value.Int()
		}
	}
	if usage.Get("candidatesTokenCount").Type == gjson.Number || usage.Get("thoughtsTokenCount").Type == gjson.Number {
		if total, ok := safeUsageTokenSum(usage.Get("candidatesTokenCount").Int(), usage.Get("thoughtsTokenCount").Int()); ok {
			summary["output_tokens"] = total
		}
	}
	candidates := root.Get("candidates").Array()
	summary["candidate_count"] = len(candidates)
	details := make([]log.Fields, 0, len(candidates))
	for index, candidate := range candidates {
		textBytes, thoughtBytes, toolCalls, mediaParts := 0, 0, 0, 0
		textHash := sha256.New()
		for _, part := range candidate.Get("content.parts").Array() {
			text := part.Get("text").String()
			if part.Get("thought").Bool() {
				thoughtBytes += len(text)
			} else {
				textBytes += len(text)
				_, _ = io.WriteString(textHash, text)
			}
			if part.Get("functionCall").IsObject() {
				toolCalls++
			}
			if part.Get("inlineData").IsObject() || part.Get("fileData").IsObject() {
				mediaParts++
			}
		}
		detail := log.Fields{
			"index": index, "finish_reason": logging.SafeDiagnosticForLog(candidate.Get("finishReason").String()),
			"text_bytes": textBytes, "thought_text_bytes": thoughtBytes,
			"tool_calls": toolCalls, "media_parts": mediaParts,
		}
		if textBytes > 0 {
			detail["text_sha256"] = hex.EncodeToString(textHash.Sum(nil))
		}
		details = append(details, detail)
	}
	summary["candidates"] = details
	return summary
}
