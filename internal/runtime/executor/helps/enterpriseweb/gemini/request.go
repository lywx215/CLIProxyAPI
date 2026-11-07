// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
// Package gemini owns Business-native Gemini wire types. It does not translate
// requests through OpenAI or the existing AI Studio DTOs.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"mime"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
)

// object preserves raw values (including 0/false/null and opaque schemas), and
// rejects duplicate keys before typed decoding can collapse their meaning.
func object(data []byte) (map[string]json.RawMessage, error) {
	return rawObject(data)
}
func invalid(message string) error { return contract.Fail(400, "INVALID_ARGUMENT", message) }
func unsupported(field string) error {
	return invalid("Business does not have a verified mapping for " + field)
}

var errUnsupportedParameter = errors.New("unsupported optional generation parameter")

func Parse(r io.Reader, binding config.Binding) (contract.Request, error) {
	return ParseWithFiles(context.Background(), r, binding, nil)
}

// ParseWithFiles keeps request validation ahead of account selection. Resolvers
// are supplied by the Business host; no legacy AI Studio file pool is consulted.
func ParseWithFiles(ctx context.Context, r io.Reader, binding config.Binding, resolver contract.FileResolver) (contract.Request, error) {
	maxRequestBytes := inputLimits(ctx).MaxRequestBytes
	data, err := resources.ReadAll(ctx, r, maxRequestBytes)
	if err != nil {
		var failure *contract.Failure
		if errors.As(err, &failure) {
			return contract.Request{}, failure
		}
		return contract.Request{}, invalid("cannot read request body")
	}
	if int64(len(data)) > maxRequestBytes {
		return contract.Request{}, contract.LimitFailure("body", "Business request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitRequestBytes, Observed: int64(len(data)), Maximum: maxRequestBytes})
	}
	return ParseBytes(ctx, data, binding, resolver)
}

// ParseBytes validates a request-owned body without copying the base64 payload.
func ParseBytes(ctx context.Context, data []byte, binding config.Binding, resolver contract.FileResolver) (contract.Request, error) {
	return parseBytes(ctx, data, binding, resolver, false)
}

// ParseSerializedBytes builds a self-contained visible conversation for a new Plus session.
func ParseSerializedBytes(ctx context.Context, data []byte, binding config.Binding) (contract.Request, error) {
	return parseBytes(ctx, data, binding, nil, true)
}

func parseBytes(ctx context.Context, data []byte, binding config.Binding, resolver contract.FileResolver, serialized bool) (contract.Request, error) {
	limits := inputLimits(ctx)
	maxRequestBytes, maxTextBytes := limits.MaxRequestBytes, limits.MaxTextBytes
	if int64(len(data)) > maxRequestBytes {
		return contract.Request{}, contract.LimitFailure("body", "Media request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitRequestBytes, Observed: int64(len(data)), Maximum: maxRequestBytes})
	}
	root, err := objectContext(ctx, data)
	if err != nil {
		return contract.Request{}, inputLimitField(err, "body")
	}
	request := contract.Request{Raw: root, Generation: map[string]json.RawMessage{}}
	// Unimplemented optional fields (for example safetySettings and function declarations) are
	// intentionally ignored. Raw retains the wire input for inspection only;
	// only explicitly mapped fields are used for upstream requests.
	generation, hasGeneration, err := alias(root, "generationConfig", "generation_config")
	if err != nil {
		return request, err
	}
	request.GoogleSearch, err = parseGoogleSearch(ctx, root["tools"])
	if err != nil {
		return request, inputLimitField(err, "tools")
	}
	if request.GoogleSearch {
		if err = validateGoogleSearchToolConfig(ctx, root); err != nil {
			return request, inputLimitField(err, "toolConfig")
		}
	}
	var plan *historyPlan
	if !serialized {
		plan, err = planHistory(ctx, root["contents"])
		if err != nil {
			return request, err
		}
	}
	if !serialized && len(plan.pending) == 0 && int64(len(data)) > maxTextBytes {
		return request, contract.LimitFailure("body", "Business text request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitTextBytes, Observed: int64(len(data)), Maximum: maxTextBytes})
	}
	if hasGeneration {
		if int64(len(generation)) > limits.MaxGenerationConfigBytes {
			return request, contract.LimitFailure("generationConfig", "Business generationConfig exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitGenerationConfigBytes, Observed: int64(len(generation)), Maximum: limits.MaxGenerationConfigBytes})
		}
		parameters, err := objectContext(ctx, generation)
		if err != nil {
			return request, inputLimitField(err, "generationConfig")
		}
		parameters, err = parameterAliases(parameters)
		if err != nil {
			return request, err
		}
		// Filter before checking combinations: an ignored schema or sampling
		// option must not conflict with a supported option that will be sent.
		for field := range parameters {
			mapping, ok := binding.GenerationParameters[field]
			if !ok || !mapping.Verified || strings.TrimSpace(mapping.Evidence) == "" {
				delete(parameters, field)
			}
		}
		if _, first := parameters["responseSchema"]; first {
			if _, second := parameters["responseJsonSchema"]; second {
				return request, invalid("responseSchema and responseJsonSchema cannot be combined")
			}
		}
		for field, value := range parameters {
			mapping := binding.GenerationParameters[field]
			if source, target := parameterKind(field), parameterKind(mapping.UpstreamField); source != "" && target != "" && source != target {
				return request, invalid("generation parameter mapping has incompatible value types")
			}
			if err := validateParameter(field, value); err != nil {
				if errors.Is(err, errUnsupportedParameter) {
					continue
				}
				return request, err
			}
			if field == "thinkingConfig" {
				value = canonicalThinking(value)
			}
			if _, exists := request.Generation[mapping.UpstreamField]; exists {
				return request, invalid("generation parameter mappings collide")
			}
			request.Generation[mapping.UpstreamField] = append(json.RawMessage(nil), value...)
		}
	}
	if serialized {
		err = serializeHistory(ctx, root, &request)
		return request, err
	}
	request.OptionsHash, err = historyOptionsHash(ctx, root, binding, request.Generation)
	if err != nil {
		return request, err
	}
	// All content roles, parts and optional generation fields are checked before
	// resolving any local file. Only the final user turn becomes an upstream query.
	if err = plan.resolve(ctx, resolver, &request); err != nil {
		return request, err
	}
	request.InputUsage, err = plan.localInputUsage(ctx)
	if err != nil {
		return request, err
	}
	// Search results and internal tool prompts are not visible to this counter.
	request.InputUsage.Incomplete = request.InputUsage.Incomplete || request.GoogleSearch
	return request, nil
}

const (
	maxRequestBytes    = 68 << 20
	maxTextBytes       = 1 << 20
	maxAttachmentBytes = 50 << 20
	maxAttachments     = 32
)

const MaxRequestBytes = maxRequestBytes

func alias(m map[string]json.RawMessage, camel, snake string) (json.RawMessage, bool, error) {
	first, hasFirst := m[camel]
	second, hasSecond := m[snake]
	if hasFirst && hasSecond {
		return nil, false, invalid("conflicting field aliases for " + camel)
	}
	if hasFirst {
		return first, true, nil
	}
	return second, hasSecond, nil
}

func requiredString(raw json.RawMessage, name string) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", invalid(name + " must be a nonempty string")
	}
	return value, nil
}

func supportedMIME(value string) (string, error) {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || len(params) != 0 {
		return "", invalid("attachment MIME type must be a media type without parameters")
	}
	supported := strings.HasPrefix(mediaType, "text/")
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "image/heic", "image/heif", "application/pdf", "application/json", "application/xml":
		supported = true
	}
	if !supported {
		return "", unsupported("attachment MIME type " + mediaType)
	}
	return mediaType, nil
}

func inlineFields(ctx context.Context, raw json.RawMessage) (map[string]json.RawMessage, string, error) {
	var a contract.Attachment
	blob, err := objectContext(ctx, raw)
	if err != nil {
		return nil, "", err
	}
	for key := range blob {
		if key != "data" && key != "mimeType" && key != "mime_type" {
			return nil, "", unsupported("inlineData." + key)
		}
	}
	mimeRaw, _, err := alias(blob, "mimeType", "mime_type")
	if err != nil {
		return nil, "", err
	}
	if len(mimeRaw) > 1024 {
		return nil, "", invalid("inlineData.mimeType is too long")
	}
	mimeType, err := requiredString(mimeRaw, "inlineData.mimeType")
	if err != nil {
		return nil, "", err
	}
	a.MIMEType, err = supportedMIME(mimeType)
	if err != nil {
		return nil, "", err
	}
	value := bytes.TrimSpace(blob["data"])
	if len(value) < 3 || value[0] != '"' {
		return nil, "", invalid("inlineData.data must be a nonempty base64 string")
	}
	return blob, a.MIMEType, nil
}

func parseInline(ctx context.Context, raw json.RawMessage, index int, remaining int64) (contract.Attachment, error) {
	maxAttachmentBytes := inputLimits(ctx).MaxAttachmentBytes
	var a contract.Attachment
	blob, mimeType, err := inlineFields(ctx, raw)
	if err != nil {
		return a, err
	}
	a.MIMEType = mimeType
	encodedSize, err := jsonStringSize(blob["data"])
	if err != nil {
		return a, err
	}
	if encodedSize/4*3-2 > remaining {
		return a, contract.LimitFailure("inlineData", "Business attachments exceed their configured byte limit", contract.InputLimit{Name: contract.LimitAttachmentBytes, Observed: maxAttachmentBytes - remaining + encodedSize/4*3 - 2, Maximum: maxAttachmentBytes, AtLeast: true})
	}
	releaseString, err := resources.Reserve(ctx, encodedSize)
	if err != nil {
		return a, err
	}
	defer releaseString()
	encoded, err := requiredString(blob["data"], "inlineData.data")
	if err != nil {
		return a, err
	}
	// Protobuf JSON accepts both alphabets, with or without padding. Reject
	// whitespace and invalid trailing bits rather than silently repairing them.
	if strings.ContainsAny(encoded, " \t\r\n") {
		return a, invalid("inlineData.data must be valid base64")
	}
	urlAlphabet := strings.ContainsAny(encoded, "-_")
	if urlAlphabet && strings.ContainsAny(encoded, "+/") {
		return a, invalid("inlineData.data must be valid base64")
	}
	encoding := base64.StdEncoding.Strict()
	if urlAlphabet {
		encoding = base64.URLEncoding.Strict()
	}
	padding := len(encoded) - len(strings.TrimRight(encoded, "="))
	if padding == 0 {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	decodedBytes := int64(encoding.DecodedLen(len(encoded)))
	if padding > 0 {
		decodedBytes -= int64(padding)
	}
	if padding > 2 || decodedBytes < 1 {
		return a, invalid("inlineData.data must be nonempty valid base64")
	}
	if decodedBytes > remaining {
		return a, contract.LimitFailure("inlineData", "Business attachments exceed their configured byte limit", contract.InputLimit{Name: contract.LimitAttachmentBytes, Observed: maxAttachmentBytes - remaining + decodedBytes, Maximum: maxAttachmentBytes, AtLeast: true})
	}
	if _, err := resources.Reserve(ctx, int64(encoding.DecodedLen(len(encoded)))); err != nil {
		return a, err
	}
	a.Data, err = encoding.DecodeString(encoded)
	if err != nil || len(a.Data) == 0 {
		return a, invalid("inlineData.data must be nonempty valid base64")
	}
	ext := ".bin"
	switch a.MIMEType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/heic":
		ext = ".heic"
	case "image/heif":
		ext = ".heif"
	case "application/pdf":
		ext = ".pdf"
	case "application/json":
		ext = ".json"
	case "application/xml":
		ext = ".xml"
	default:
		if strings.HasPrefix(a.MIMEType, "text/") {
			ext = ".txt"
		}
	}
	a.Filename = fmt.Sprintf("inline-%03d%s", index, ext)
	return a, nil
}

func parseFile(ctx context.Context, raw json.RawMessage) (string, string, error) {
	file, err := objectContext(ctx, raw)
	if err != nil {
		return "", "", err
	}
	for key := range file {
		if key != "fileUri" && key != "file_uri" && key != "mimeType" && key != "mime_type" {
			return "", "", unsupported("fileData." + key)
		}
	}
	uriRaw, _, err := alias(file, "fileUri", "file_uri")
	if err != nil {
		return "", "", err
	}
	mimeRaw, _, err := alias(file, "mimeType", "mime_type")
	if err != nil {
		return "", "", err
	}
	if len(mimeRaw) > 1024 {
		return "", "", invalid("fileData.mimeType is too long")
	}
	uriSize, err := jsonStringSize(uriRaw)
	if err != nil {
		return "", "", err
	}
	if _, err := resources.Reserve(ctx, uriSize); err != nil {
		return "", "", err
	}
	uri, err := requiredString(uriRaw, "fileData.fileUri")
	if err != nil {
		return "", "", err
	}
	mimeType, err := requiredString(mimeRaw, "fileData.mimeType")
	if err != nil {
		return "", "", err
	}
	mimeType, err = supportedMIME(mimeType)
	if err != nil {
		return "", "", err
	}
	resource := uri
	if strings.Contains(uri, "://") {
		u, err := url.Parse(uri)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/v1beta/") {
			return "", "", invalid("fileData requires a Business file reference")
		}
		resource = strings.TrimPrefix(u.Path, "/v1beta/")
	}
	if !strings.HasPrefix(resource, "files/biz-") || len(strings.TrimPrefix(resource, "files/biz-")) == 0 || strings.ContainsAny(resource, "\\?#% \t\r\n") || strings.Count(resource, "/") != 1 {
		return "", "", invalid("fileData requires a Business file reference")
	}
	return uri, mimeType, nil
}
func validateParameter(field string, value json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return invalid("generationConfig." + field + " cannot be null")
	}
	switch field {
	case "temperature", "topP", "topK", "maxOutputTokens", "candidateCount", "seed", "presencePenalty", "frequencyPenalty":
		var number float64
		if json.Unmarshal(value, &number) != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return invalid("invalid numeric generation parameter")
		}
		exact, ok := new(big.Rat).SetString(string(value))
		if !ok {
			return invalid("invalid numeric generation parameter")
		}
		if (field == "topK" || field == "maxOutputTokens" || field == "candidateCount" || field == "seed") && !exact.IsInt() {
			return invalid("generation parameter requires an integer")
		}
		if field == "candidateCount" && number != 1 {
			return errUnsupportedParameter
		}
		if (field == "temperature" || field == "topK" || field == "topP") && number < 0 {
			return invalid("generation parameter cannot be negative")
		}
		if field == "topP" && number > 1 {
			return invalid("topP cannot exceed 1")
		}
		if field == "maxOutputTokens" && number < 1 {
			return invalid("maxOutputTokens must be positive")
		}
	case "stopSequences", "responseModalities":
		var values []string
		if json.Unmarshal(value, &values) != nil || len(values) > 64 {
			return invalid("invalid list generation parameter")
		}
		if field == "responseModalities" && (len(values) != 1 || values[0] != "TEXT") {
			return errUnsupportedParameter
		}
	case "responseMimeType", "mediaResolution":
		var text string
		if json.Unmarshal(value, &text) != nil || text == "" {
			return invalid("invalid string generation parameter")
		}
	case "responseSchema", "responseJsonSchema":
		if _, err := object(value); err != nil {
			return invalid("generation parameter requires an object")
		}
	case "thinkingConfig":
		parameters, err := object(value)
		if err != nil {
			return invalid("thinkingConfig requires an object")
		}
		for _, names := range [][2]string{{"includeThoughts", "include_thoughts"}, {"thinkingBudget", "thinking_budget"}, {"thinkingLevel", "thinking_level"}} {
			if _, _, err := alias(parameters, names[0], names[1]); err != nil {
				return err
			}
		}
		for key := range parameters {
			switch key {
			case "includeThoughts", "include_thoughts", "thinkingBudget", "thinking_budget", "thinkingLevel", "thinking_level":
			default:
				return errUnsupportedParameter
			}
		}
		for key, raw := range parameters {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return invalid("thinkingConfig fields cannot be null")
			}
			switch key {
			case "includeThoughts", "include_thoughts":
				var value bool
				if json.Unmarshal(raw, &value) != nil {
					return invalid("thinkingConfig.includeThoughts must be a boolean")
				}
			case "thinkingBudget", "thinking_budget":
				var value int32
				if json.Unmarshal(raw, &value) != nil || value < -1 {
					return invalid("thinkingConfig.thinkingBudget must be an integer at least -1")
				}
			case "thinkingLevel", "thinking_level":
				var value string
				if json.Unmarshal(raw, &value) != nil || (value != "MINIMAL" && value != "LOW" && value != "MEDIUM" && value != "HIGH") {
					return invalid("unsupported thinkingConfig.thinkingLevel")
				}
			}
		}
	default:
		return errUnsupportedParameter
	}
	return nil
}

func parameterKind(field string) string {
	switch field {
	case "temperature", "topP", "topK", "maxOutputTokens", "candidateCount", "seed", "presencePenalty", "frequencyPenalty":
		return "number"
	case "stopSequences", "responseModalities":
		return "list"
	case "responseMimeType", "mediaResolution":
		return "string"
	case "responseSchema", "responseJsonSchema", "thinkingConfig":
		return "object"
	}
	return ""
}

func parameterAliases(fields map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	aliases := map[string]string{
		"top_p": "topP", "top_k": "topK", "max_output_tokens": "maxOutputTokens",
		"stop_sequences": "stopSequences", "candidate_count": "candidateCount",
		"presence_penalty": "presencePenalty", "frequency_penalty": "frequencyPenalty",
		"response_mime_type": "responseMimeType", "response_schema": "responseSchema",
		"response_json_schema": "responseJsonSchema", "thinking_config": "thinkingConfig",
		"response_modalities": "responseModalities", "media_resolution": "mediaResolution",
	}
	result := make(map[string]json.RawMessage, len(fields))
	for field, value := range fields {
		canonical := field
		if name, ok := aliases[field]; ok {
			canonical = name
		}
		if _, exists := result[canonical]; exists {
			return nil, invalid("conflicting field aliases for " + canonical)
		}
		result[canonical] = value
	}
	return result, nil
}

func canonicalThinking(raw json.RawMessage) json.RawMessage {
	fields, _ := object(raw) // validateParameter already checked the full object.
	changed := false
	for _, names := range [][2]string{{"includeThoughts", "include_thoughts"}, {"thinkingBudget", "thinking_budget"}, {"thinkingLevel", "thinking_level"}} {
		if value, ok := fields[names[1]]; ok {
			fields[names[0]] = value
			delete(fields, names[1])
			changed = true
		}
	}
	if !changed {
		return raw
	}
	canonical, _ := json.Marshal(fields)
	return canonical
}
