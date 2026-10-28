package geminienterprise

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// UpstreamDiagnostic contains only recognized protocol constants and schema paths.
// Descriptions, metadata values, resource identifiers and free-form text are excluded.
type UpstreamDiagnostic struct {
	Status          string           `json:"status,omitempty"`
	Reason          string           `json:"reason,omitempty"`
	FieldViolations []FieldViolation `json:"field_violations,omitempty"`
}

type FieldViolation struct {
	Field  string `json:"field"`
	Reason string `json:"reason,omitempty"`
}

func PublicUpstreamDiagnostic(err error, status int) *UpstreamDiagnostic {
	var detail *Error
	if !errors.As(err, &detail) || detail == nil || detail.Status != status || detail.clientMessage == "" || detail.diagnostic == nil {
		return nil
	}
	copy := *detail.diagnostic
	copy.FieldViolations = slices.Clone(copy.FieldViolations)
	return &copy
}

type upstreamErrorBody struct {
	Error   json.RawMessage `json:"error"`
	Code    int             `json:"code"`
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Details []struct {
		Type            string `json:"@type"`
		Domain          string `json:"domain"`
		Reason          string `json:"reason"`
		FieldViolations []struct {
			Field       string `json:"field"`
			Reason      string `json:"reason"`
			Description string `json:"description"`
		} `json:"fieldViolations"`
	} `json:"details"`
}

func decodeUpstreamError(raw []byte, depth int) upstreamErrorBody {
	if len(raw) > 64*1024 || depth > 4 {
		return upstreamErrorBody{}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return upstreamErrorBody{}
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil || len(items) != 1 {
			return upstreamErrorBody{}
		}
		return decodeUpstreamError(items[0], depth+1)
	}
	var body upstreamErrorBody
	if json.Unmarshal(raw, &body) != nil {
		return upstreamErrorBody{}
	}
	if len(body.Error) > 0 && string(body.Error) != "null" {
		return decodeUpstreamError(body.Error, depth+1)
	}
	return body
}

func upstreamDiagnostic(body upstreamErrorBody, status int) *UpstreamDiagnostic {
	diagnostic := &UpstreamDiagnostic{}
	// A status must agree with the actual HTTP response or its decoded stream status.
	statuses := map[string][]int{
		"INVALID_ARGUMENT": {400, 422}, "FAILED_PRECONDITION": {400, 422}, "OUT_OF_RANGE": {400, 422},
		"UNAUTHENTICATED": {401}, "PERMISSION_DENIED": {403}, "NOT_FOUND": {404},
		"RESOURCE_EXHAUSTED": {429}, "INTERNAL": {500, 502}, "UNAVAILABLE": {502, 503},
		"DEADLINE_EXCEEDED": {504}, "CANCELLED": {499}, "UNKNOWN": {500}, "DATA_LOSS": {500},
		"ALREADY_EXISTS": {409}, "ABORTED": {409}, "UNIMPLEMENTED": {501},
	}
	codes := map[int][]int{400: {3, 9, 11, 400}, 422: {3, 9, 11, 422}, 401: {16, 401}, 403: {7, 403}, 404: {5, 404}, 409: {6, 10, 409}, 429: {8, 429}, 499: {1, 499}, 500: {2, 13, 15, 500}, 501: {12, 501}, 502: {13, 14, 502}, 503: {14, 503}, 504: {4, 504}}
	if body.Code != 0 && !slices.Contains(codes[status], body.Code) || body.Status != "" && !slices.Contains(statuses[body.Status], status) {
		return nil
	}
	if slices.Contains(statuses[body.Status], status) {
		diagnostic.Status = body.Status
	}
	for _, detail := range body.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" && slices.Contains([]string{"googleapis.com", "discoveryengine.googleapis.com", "serviceusage.googleapis.com", "iam.googleapis.com"}, detail.Domain) {
			switch detail.Reason {
			case "MODEL_NOT_ENABLED", "SERVICE_DISABLED", "BILLING_DISABLED", "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "IAM_PERMISSION_DENIED", "USER_PROJECT_DENIED", "CONSUMER_INVALID", "CUSTOMER_POLICY_VIOLATION":
				if diagnostic.Reason == "" {
					diagnostic.Reason = detail.Reason
				}
			}
		}
		if detail.Type != "type.googleapis.com/google.rpc.BadRequest" || status != 400 && status != 422 {
			continue
		}
		for _, violation := range detail.FieldViolations {
			path := safeUpstreamFieldPath(violation.Field)
			if path == "" || len(diagnostic.FieldViolations) >= 8 {
				continue
			}
			field := FieldViolation{Field: path, Reason: fieldViolationReason(violation.Reason, violation.Description)}
			if !slices.Contains(diagnostic.FieldViolations, field) {
				diagnostic.FieldViolations = append(diagnostic.FieldViolations, field)
			}
		}
	}
	if diagnostic.Status == "" && diagnostic.Reason == "" && len(diagnostic.FieldViolations) == 0 {
		return nil
	}
	return diagnostic
}

func safeUpstreamFieldPath(path string) string {
	if safeFieldPath(path) == "body" {
		return ""
	}
	// Field names supplied by the upstream are not trusted arbitrary identifiers.
	// Keep public protocol paths while excluding user-defined arguments and values.
	const fields = "|model|contents|parts|role|text|functionCall|functionResponse|function_call|function_response|name|args|response|query|generationSpec|modelId|generationConfig|thinkingConfig|thinkingLevel|thinkingBudget|includeThoughts|responseMimeType|temperature|topP|topK|maxOutputTokens|stopSequences|responseSchema|systemInstruction|tools|functionDeclarations|toolConfig|functionCallingConfig|mode|allowedFunctionNames|toolsSpec|webGroundingSpec|vertexAiSearchSpec|disabled|filter|userMetadata|timeZone|preferredLanguageCode|actionSpec|actionDisabled|isSessionLess|languageCode|googleSearchGroundingEnabled|webGroundingEnabled|answerGenerationMode|assistSkippingMode|"
	for _, part := range strings.Split(path, ".") {
		name, _, _ := strings.Cut(part, "[")
		if !strings.Contains(fields, "|"+name+"|") {
			return ""
		}
	}
	return path
}

func fieldViolationReason(reason, description string) string {
	switch reason {
	case "REQUIRED_FIELD_MISSING", "FIELD_REQUIRED", "MISSING_REQUIRED_FIELD":
		return "required"
	case "INVALID_TYPE", "FIELD_TYPE_MISMATCH":
		return "invalid_type"
	case "UNKNOWN_FIELD", "UNSUPPORTED_FIELD":
		return "unsupported_field"
	case "INVALID_VALUE", "FIELD_VALUE_INVALID":
		return "invalid_value"
	}
	// Recognize categories only; descriptions can contain prompts and secrets.
	lower := strings.ToLower(description)
	switch {
	case strings.Contains(lower, "unknown field"), strings.Contains(lower, "unknown name"), strings.Contains(lower, "not supported"):
		return "unsupported_field"
	case strings.Contains(lower, "required"), strings.Contains(lower, "must be nonempty"), strings.Contains(lower, "must not be empty"):
		return "required"
	case strings.Contains(lower, "type mismatch"), strings.Contains(lower, "must be a string"), strings.Contains(lower, "must be an object"), strings.Contains(lower, "must be an array"):
		return "invalid_type"
	}
	return ""
}
