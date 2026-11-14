package geminienterprise

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

var fieldPathPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[0-9]+\])?(\.[A-Za-z_][A-Za-z0-9_]*(\[[0-9]+\])?)*$`)

func safeFieldPath(path string) string {
	if len(path) > 192 || !fieldPathPattern.MatchString(path) {
		return "body"
	}
	identifier := strings.ToLower(path)
	for _, separator := range []string{"_", "-", " "} {
		identifier = strings.ReplaceAll(identifier, separator, "")
	}
	for _, internal := range []string{"geminienterprise", "streamassist", "discoveryengine"} {
		if strings.Contains(identifier, internal) {
			return "body"
		}
	}
	return path
}

func invalidField(path, reason string, payload bool) error {
	path = safeFieldPath(path)
	message := path + " " + reason + "."
	if payload {
		message += " Check the server request payload configuration."
	}
	return &Error{Status: 400, Kind: "invalid_field", Message: message, Scoped: true, clientMessage: message, param: path}
}

// PublicErrorDetail returns only locally constructed diagnostics, never an upstream message.
// Error text alone is not proof that a diagnostic is safe to expose.
func PublicErrorDetail(err error, status int) (message, code, param string) {
	var detail *Error
	if !errors.As(err, &detail) || detail == nil || detail.Status != status || detail.clientMessage == "" {
		return "", "", ""
	}
	return detail.clientMessage, detail.Kind, detail.param
}

type fieldSpec struct {
	kind     string
	fields   map[string]fieldSpec
	required bool
	values   string
}

// This is the supported text/grounding subset of StreamAssistRequest, not GenerateContent.
// Session reuse, files, agents and media tools remain outside this provider's contract.
// https://docs.cloud.google.com/gemini/enterprise/docs/reference/rest/v1alpha/projects.locations.collections.engines.assistants/streamAssist
var finalRequestFields = map[string]fieldSpec{
	"query": {kind: "object", required: true, fields: map[string]fieldSpec{
		"text": {kind: "nonempty text", required: true},
	}},
	"generationSpec": {kind: "object", required: true, fields: map[string]fieldSpec{
		"modelId": {kind: "nonempty text", required: true},
		"thinkingConfig": {kind: "object", fields: map[string]fieldSpec{
			"thinkingLevel":   {kind: "enum", values: "LOW|HIGH"},
			"includeThoughts": {kind: "boolean"},
		}},
	}},
	"toolsSpec": {kind: "object", fields: map[string]fieldSpec{
		"webGroundingSpec": {kind: "object", fields: map[string]fieldSpec{}},
		"vertexAiSearchSpec": {kind: "object", fields: map[string]fieldSpec{
			"disabled": {kind: "boolean"},
			"filter":   {kind: "text"},
		}},
	}},
	"userMetadata": {kind: "object", fields: map[string]fieldSpec{
		"timeZone":              {kind: "text"},
		"preferredLanguageCode": {kind: "text"},
	}},
	"actionSpec": {kind: "object", fields: map[string]fieldSpec{
		"actionDisabled": {kind: "boolean"},
	}},
	"isSessionLess":                {kind: "boolean"},
	"languageCode":                 {kind: "text"},
	"googleSearchGroundingEnabled": {kind: "boolean"},
	"webGroundingEnabled":          {kind: "boolean"},
	"answerGenerationMode":         {kind: "enum", values: "ANSWER_GENERATION_MODE_UNSPECIFIED|NORMAL"},
	"assistSkippingMode":           {kind: "enum", values: "ASSIST_SKIPPING_MODE_UNSPECIFIED|REQUEST_ASSIST"},
}

func validateFields(object map[string]any, fields map[string]fieldSpec, prefix string) error {
	keys := make([]string, 0, len(object)+len(fields))
	for key := range object {
		keys = append(keys, key)
	}
	for key, spec := range fields {
		if _, exists := object[key]; !exists && spec.required {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := prefix + key
		spec, supported := fields[key]
		if !supported {
			return invalidField(path, "is not supported by the request schema", true)
		}
		value, exists := object[key]
		if !exists {
			return invalidField(path, "is required", true)
		}
		switch spec.kind {
		case "object":
			nested, ok := value.(map[string]any)
			if !ok || nested == nil {
				return invalidField(path, "must be an object", true)
			}
			if err := validateFields(nested, spec.fields, path+"."); err != nil {
				return err
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				return invalidField(path, "must be a boolean", true)
			}
		case "text", "nonempty text", "enum":
			text, ok := value.(string)
			if !ok || (spec.kind == "nonempty text" && strings.TrimSpace(text) == "") {
				return invalidField(path, "must be "+spec.kind, true)
			}
			if spec.kind == "enum" && !slices.Contains(strings.Split(spec.values, "|"), text) {
				return invalidField(path, "must be one of "+strings.ReplaceAll(spec.values, "|", ", "), true)
			}
		}
	}
	return nil
}

func decodeValue(decoder *json.Decoder, path string, depth int, payload bool) (any, error) {
	if depth > 64 {
		return nil, invalidField(path, "exceeds the supported JSON nesting depth", payload)
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for decoder.More() {
			keyToken, errKey := decoder.Token()
			if errKey != nil {
				return nil, errKey
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
			field := key
			if path != "" {
				field = path + "." + key
			}
			if _, exists := object[key]; exists {
				return nil, invalidField(field, "must not be repeated in the same JSON object", payload)
			}
			value, errValue := decodeValue(decoder, field, depth+1, payload)
			if errValue != nil {
				return nil, errValue
			}
			object[key] = value
		}
		_, err = decoder.Token()
		return object, err
	case json.Delim('['):
		array := []any{}
		for decoder.More() {
			value, errValue := decodeValue(decoder, fmt.Sprintf("%s[%d]", path, len(array)), depth+1, payload)
			if errValue != nil {
				return nil, errValue
			}
			array = append(array, value)
		}
		_, err = decoder.Token()
		return array, err
	default:
		return token, nil
	}
}

func decodeRequestObject(body []byte, payload bool) (map[string]any, error) {
	if !utf8.Valid(body) {
		return nil, invalidField("body", "must be a valid UTF-8 JSON object", payload)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	value, err := decodeValue(decoder, "", 0, payload)
	var fieldErr *Error
	if errors.As(err, &fieldErr) {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if err != nil || !ok || object == nil {
		return nil, invalidField("body", "must be a valid JSON object", payload)
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, invalidField("body", "must contain exactly one JSON object", payload)
	}
	return object, nil
}

// ValidateFinal is read-only and model-independent: user overrides are never repaired.
func ValidateFinal(body []byte) error {
	object, err := decodeRequestObject(body, true)
	if err != nil {
		return err
	}
	if err := validateFields(object, finalRequestFields, ""); err != nil {
		return err
	}
	return nil
}

// RawAssistModels reads the model selectors without rewriting a raw management
// request. Protobuf JSON spellings must identify a single value for each field.
func RawAssistModels(body []byte) ([]string, error) {
	object, err := decodeRequestObject(body, false)
	if err != nil {
		return nil, err
	}
	generation, err := rawAssistObject(object, "generationSpec", "generation_spec")
	if err != nil {
		return nil, err
	}
	modelID, err := rawAssistField(generation, "modelId", "model_id")
	if err != nil {
		return nil, err
	}
	var models []string
	if modelID != nil {
		model, ok := modelID.(string)
		if !ok || strings.TrimSpace(model) == "" {
			return nil, Invalid("the upstream model must be a nonempty string")
		}
		models = append(models, model)
	}
	tools, err := rawAssistObject(object, "toolsSpec", "tools_spec")
	if err != nil {
		return nil, err
	}
	for _, tool := range []struct{ camel, snake, model string }{
		{"imageGenerationSpec", "image_generation_spec", "gemini-3-pro-image"},
		{"videoGenerationSpec", "video_generation_spec", "veo-3.1"},
	} {
		spec, errTool := rawAssistObject(tools, tool.camel, tool.snake)
		if errTool != nil {
			return nil, errTool
		}
		if spec != nil {
			models = append(models, tool.model)
		}
	}
	return models, nil
}

func rawAssistField(object map[string]any, camel, snake string) (any, error) {
	value, present := object[camel]
	if alternative, exists := object[snake]; exists {
		if present {
			return nil, Invalid("use one JSON spelling for each generation field")
		}
		return alternative, nil
	}
	return value, nil
}

func rawAssistObject(object map[string]any, camel, snake string) (map[string]any, error) {
	value, err := rawAssistField(object, camel, snake)
	if err != nil || value == nil {
		return nil, err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, Invalid("generation settings must be JSON objects")
	}
	return fields, nil
}

// UpstreamHTTPError classifies a bounded Google error body without echoing its values.
func UpstreamHTTPError(status int, raw []byte) error {
	code, message := "upstream_error", fmt.Sprintf("The upstream service returned HTTP %d; retry later or contact the server administrator.", status)
	param := ""
	body := decodeUpstreamError(raw, 0)
	diagnostic := upstreamDiagnostic(body, status)
	switch status {
	case 400, 422:
		code, message = "upstream_invalid_request", "The upstream service rejected the request; check model availability and supported request fields."
		if diagnostic != nil && len(diagnostic.FieldViolations) > 0 {
			field := diagnostic.FieldViolations[0]
			param = field.Field
			message = "The upstream service rejected field " + param + "; check its type and value."
			switch field.Reason {
			case "required":
				message = "The upstream service requires a nonempty value for field " + param + "."
			case "invalid_type":
				message = "The upstream service rejected the type of field " + param + "."
			case "unsupported_field":
				message = "The upstream service does not support field " + param + "."
			}
		}
		lower := strings.ToLower(body.Message)
		if param == "" && strings.Contains(lower, "model") && (strings.Contains(lower, "permission") || strings.Contains(lower, "not allowed") || strings.Contains(lower, "not authorized") || strings.Contains(lower, "not have access")) {
			code, message, param = "model_access_denied", "The account cannot access the requested model; check the account's model permissions.", "model"
		}
		if param == "" && diagnostic != nil && diagnostic.Status == "FAILED_PRECONDITION" && strings.Contains(lower, "model") && strings.Contains(lower, "not available during the free trial") && strings.Contains(lower, "paid subscription") {
			code, message, param = "model_subscription_required", "The requested model is unavailable with the account's current subscription; use an eligible subscription or select another model.", "model"
		}
	case 401:
		code, message = "upstream_authentication_required", "The upstream service rejected the authorization; refresh or authorize the account again."
	case 403:
		code, message = "permission_denied", "Access to the requested resource or model was denied; check the account's resource and model permissions."
		if diagnostic != nil && diagnostic.Reason == "SERVICE_DISABLED" {
			code, message = "upstream_service_disabled", "A required upstream API service is disabled; ask its administrator to check API enablement."
		}
	case 404:
		code, message = "resource_not_found", "The requested resource or model is unavailable; check the saved resource settings and model availability."
		if diagnostic != nil && diagnostic.Status == "NOT_FOUND" && strings.Contains(strings.ToLower(body.Message), "method not found") {
			code, message = "upstream_method_unavailable", "The upstream method is unavailable for the current resource; check method availability and the saved resource settings."
		}
	case 429:
		code, message = "rate_limit_exceeded", "The quota or rate limit was reached; check usage and retry after the limit resets."
	case 499:
		code, message = "upstream_error", "The upstream service cancelled generation."
	case 500:
		code, message = "upstream_error", "The upstream service encountered an internal generation error."
	case 503:
		code, message = "upstream_error", "The upstream service is temporarily unavailable."
	case 504:
		code, message = "upstream_error", "The upstream service reported that its generation deadline was exceeded."
	}
	if diagnostic != nil {
		switch diagnostic.Reason {
		case "MODEL_NOT_ENABLED":
			code, message, param = "model_access_denied", "The upstream application has not enabled the requested model.", "model"
		case "CUSTOMER_POLICY_VIOLATION":
			code, message = "content_policy_violation", "The upstream service blocked the request under the application's content policy."
		case "LLM_POLICY_CHECK_VIOLATION":
			code, message = "upstream_policy_violation", "The upstream service blocked the request under its model policy."
		}
		if diagnostic.Status != "" {
			message += " Upstream status: " + diagnostic.Status + "."
		}
		if diagnostic.Reason != "" {
			message += " Reason: " + diagnostic.Reason + "."
		}
	}
	return &Error{Status: status, Kind: code, Message: message, clientMessage: message, param: param, diagnostic: diagnostic, logDiagnostic: logUpstreamDiagnostic(body, diagnostic, status), Scoped: code == "content_policy_violation" || code == "upstream_policy_violation"}
}

// UpstreamStreamError never describes an HTTP 200 error frame as an HTTP 502 response.
func UpstreamStreamError(status int, raw []byte) error {
	err := UpstreamHTTPError(status, raw).(*Error)
	if status == 502 {
		err.Message = "The upstream generation stream reported an unrecognized error."
		err.clientMessage = err.Message
	}
	return err
}
