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

// UpstreamHTTPError classifies a bounded Google error body without echoing its values.
func UpstreamHTTPError(status int, raw []byte) error {
	code, message := "upstream_error", fmt.Sprintf("The upstream service returned HTTP %d; retry later or contact the server administrator.", status)
	param := ""
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Details []struct {
				Type            string `json:"@type"`
				FieldViolations []struct {
					Field string `json:"field"`
				} `json:"fieldViolations"`
			} `json:"details"`
		} `json:"error"`
	}
	if len(raw) <= 64*1024 {
		_ = json.Unmarshal(raw, &envelope)
	}
	switch status {
	case 400, 422:
		code, message = "upstream_invalid_request", "The upstream service rejected the request; check model availability and supported request fields."
		for _, detail := range envelope.Error.Details {
			if detail.Type != "type.googleapis.com/google.rpc.BadRequest" {
				continue
			}
			for _, violation := range detail.FieldViolations {
				if path := safeFieldPath(violation.Field); path != "body" {
					param = path
					message = "The upstream service rejected field " + path + "; check its type and value."
					break
				}
			}
		}
		lower := strings.ToLower(envelope.Error.Message)
		if param == "" && strings.Contains(lower, "model") && (strings.Contains(lower, "permission") || strings.Contains(lower, "not allowed") || strings.Contains(lower, "not authorized") || strings.Contains(lower, "not have access")) {
			code, message, param = "model_access_denied", "The account cannot access the requested model; check the account's model permissions.", "model"
		}
	case 401:
		code, message = "upstream_authentication_required", "The upstream service rejected the authorization; refresh or authorize the account again."
	case 403:
		code, message = "permission_denied", "Access to the requested resource or model was denied; check the account's resource and model permissions."
	case 404:
		code, message = "resource_not_found", "The requested resource or model is unavailable; check the saved resource settings and model availability."
	case 429:
		code, message = "rate_limit_exceeded", "The quota or rate limit was reached; check usage and retry after the limit resets."
	}
	return &Error{Status: status, Kind: code, Message: message, clientMessage: message, param: param}
}
