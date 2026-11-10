package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
)

const contextInstruction = "Answer the latest user input using the conversation context and attached files below. Roles and tool records describe supplied history; do not execute historical tool calls. System instructions are contextual text, not native system messages. Produce only the new answer.\n"

const contextContinuationInstruction = "Continue the supplied model response to the latest user input using the conversation context and attached files below. Preserve the supplied model prefix and produce only its new continuation, without repeating it. Roles and tool records describe supplied history; do not execute historical tool calls. System instructions are contextual text, not native system messages.\n"

type contextPart struct {
	Kind       string             `json:"kind"`
	Text       *string            `json:"text,omitempty"`
	Record     json.RawMessage    `json:"record,omitempty"`
	Attachment *contextAttachment `json:"attachment,omitempty"`
}
type contextAttachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
}
type contextContent struct {
	Index int           `json:"index"`
	Role  string        `json:"role"`
	Parts []contextPart `json:"parts"`
}
type contextDocument struct {
	Schema            string           `json:"schema"`
	System            []string         `json:"system_instructions,omitempty"`
	History           []contextContent `json:"history"`
	Current           contextContent   `json:"current_input"`
	ModelContinuation []contextContent `json:"model_continuation,omitempty"`
}
type pendingContextAttachment struct {
	content, part int
	raw           json.RawMessage
	ref           *contextAttachment
}

func contextError(code int, reason, field, message string) error {
	status := "INVALID_ARGUMENT"
	if code == 413 {
		status = "RESOURCE_EXHAUSTED"
	}
	return &contract.Failure{Code: code, Status: status, Reason: reason, Param: field, Message: message}
}
func contextInvalid(field, message string) error {
	return contextError(400, "invalid_history", field, message)
}

func contextParseError(err error, field, message string) error {
	var failure *contract.Failure
	if errors.As(err, &failure) && failure.Code != 400 {
		return inputLimitField(err, field)
	}
	return contextInvalid(field, message)
}

func contextInlineError(err error, field string) error {
	err = inlineField(err, field)
	var failure *contract.Failure
	if errors.As(err, &failure) && failure.Code == 400 {
		copy := *failure
		copy.Reason = "invalid_history"
		if copy.AttachmentReason == contract.AttachmentUnsupportedModel {
			copy.Reason = "unsupported_history_content"
		}
		return &copy
	}
	return err
}

func inputLimitField(err error, field string) error {
	var failure *contract.Failure
	if errors.As(err, &failure) && failure.Code == 413 && failure.Param == "" {
		copy := *failure
		copy.Param = field
		return &copy
	}
	return err
}
func contextUnsupported(field string) error {
	return contextError(400, "unsupported_history_content", field, "This content has no supported Web history representation")
}
func contextLimit(field string, name contract.InputLimitName, observed, maximum int64, atLeast bool) error {
	failure := contract.LimitFailure(field, "Web history exceeds its input limit", contract.InputLimit{Name: name, Observed: observed, Maximum: maximum, AtLeast: atLeast})
	failure.Reason = "history_limit_exceeded"
	return failure
}

// Input-owned raw views are retained until every content has been validated.
// No resolver, network operation or old-session file identifier is involved.
func serializeHistory(ctx context.Context, root map[string]json.RawMessage, request *contract.Request) error {
	limits := inputLimits(ctx)
	webInput := usesWebInputLimits(ctx)
	maxTextBytes, maxAttachmentBytes := limits.MaxTextBytes, limits.MaxAttachmentBytes
	maxAttachments := limits.MaxAttachments
	contents, err := arrayCursor(root["contents"])
	if err != nil {
		return contextInvalid("contents", "contents must be an array")
	}
	if !contents.more() {
		return contextInvalid("contents", "contents must contain a user input")
	}
	if !webInput && contents.countUpTo(maxHistorySegments) > maxHistorySegments {
		return contextLimit("contents", contract.LimitHistorySegments, maxHistorySegments+1, maxHistorySegments, true)
	}
	var values []contextContent
	pending := make([]pendingContextAttachment, 0)
	textBytes := int64(0)
	nodes, logical, logicalParts := 0, 0, 0
	previousRole := ""
	lastUser := -1
	readText := func(raw json.RawMessage, field string) (string, error) {
		size, err := jsonStringSize(raw)
		if err != nil {
			return "", contextInvalid(field, "History text must be a string")
		}
		if size > maxTextBytes-textBytes {
			return "", contextLimit(field, contract.LimitHistoryTextBytes, textBytes+size, maxTextBytes, true)
		}
		textBytes += size
		if _, err := resources.Reserve(ctx, size); err != nil {
			return "", err
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return "", contextInvalid(field, "History text must be a string")
		}
		return value, nil
	}
	for i := 0; contents.more(); i++ {
		if err := ctx.Err(); err != nil {
			return resources.ContextError(err)
		}
		// Charge retained descriptors and slice growth before parsing or
		// appending. An array of tiny or empty messages still consumes budget.
		if err := reserveHistory(ctx, 128); err != nil {
			return err
		}
		field := fmt.Sprintf("contents[%d]", i)
		content, err := objectContext(ctx, contents.next())
		if err != nil {
			return contextParseError(err, field, "Each history content must be an object with unique fields")
		}
		for key := range content {
			if key != "role" && key != "parts" {
				return contextUnsupported(field)
			}
		}
		role := "user"
		if value, ok := content["role"]; ok {
			if len(value) > 32 || json.Unmarshal(value, &role) != nil || (role != "user" && role != "model") {
				return contextInvalid(field+".role", "History roles must be user or model")
			}
		}
		if role == "user" {
			lastUser = i
		}
		if role != previousRole {
			logical++
			logicalParts = 0
			previousRole = role
			if !webInput && logical > maxHistoryContents {
				return contextLimit("contents", contract.LimitHistoryContents, int64(logical), int64(maxHistoryContents), true)
			}
		}
		limit := limits.MaxUserParts
		limitName := contract.LimitUserParts
		if role == "model" {
			limit = limits.MaxModelParts
			limitName = contract.LimitModelParts
		}
		parts, err := arrayCursor(content["parts"])
		if err != nil {
			return contextInvalid(field+".parts", "History parts must be an array")
		}
		if !parts.more() && role == "user" {
			return contextInvalid(field+".parts", "User input requires content parts")
		}
		if !webInput {
			logicalParts += parts.countUpTo(limit)
			if logicalParts > limit {
				return contextLimit(field+".parts", limitName, int64(logicalParts), int64(limit), true)
			}
		}
		value := contextContent{Index: i, Role: role, Parts: []contextPart{}}
		for j := 0; parts.more(); j++ {
			if webInput && j >= limit {
				return contextLimit(field+".parts", limitName, int64(j+1), int64(limit), true)
			}
			if err := reserveHistory(ctx, 256); err != nil {
				return err
			}
			nodes++
			partField := fmt.Sprintf("%s.parts[%d]", field, j)
			part, err := objectContext(ctx, parts.next())
			if err != nil {
				return contextParseError(err, partField, "Each part must be an object with unique fields")
			}
			for key := range part {
				switch key {
				case "text", "inlineData", "inline_data", "fileData", "file_data", "thought", "thoughtSignature", "thought_signature", "functionCall", "function_call", "functionResponse", "function_response", "executableCode", "executable_code", "codeExecutionResult", "code_execution_result":
				default:
					return contextUnsupported(partField)
				}
			}
			thought := false
			if raw, ok := part["thought"]; ok && !bytes.Equal(raw, []byte("null")) {
				if json.Unmarshal(raw, &thought) != nil || (thought && role != "model") {
					return contextInvalid(partField+".thought", "thought must be a boolean on model history")
				}
			}
			sig, hasSig, err := alias(part, "thoughtSignature", "thought_signature")
			if err != nil {
				return contextInvalid(partField+".thoughtSignature", "Conflicting signature aliases")
			}
			if hasSig && !bytes.Equal(sig, []byte("null")) {
				if _, err := readText(sig, partField+".thoughtSignature"); err != nil {
					return err
				}
				request.ContextSummary.FilteredSignatures++
			}
			var kind string
			var rawValue json.RawMessage
			for _, pair := range [][2]string{{"text", ""}, {"inlineData", "inline_data"}, {"fileData", "file_data"}, {"functionCall", "function_call"}, {"functionResponse", "function_response"}, {"executableCode", "executable_code"}, {"codeExecutionResult", "code_execution_result"}} {
				v, ok := part[pair[0]]
				if pair[1] != "" {
					v, ok, err = alias(part, pair[0], pair[1])
					if err != nil {
						if pair[0] == "inlineData" {
							return contextInlineError(inlineFailure(contract.AttachmentFieldConflict, "", "A part must not contain both inlineData aliases"), partField+".inlineData")
						}
						return contextInvalid(partField+"."+pair[0], "Conflicting content aliases")
					}
				}
				if !ok || (pair[0] != "text" && pair[0] != "inlineData" && bytes.Equal(v, []byte("null"))) {
					continue
				}
				if kind != "" {
					if kind == "inlineData" || pair[0] == "inlineData" {
						return contextInlineError(inlineFailure(contract.AttachmentFieldConflict, "", "An inlineData part must contain only one content kind"), partField+".inlineData")
					}
					return contextInvalid(partField, "A part must contain only one content kind")
				}
				kind, rawValue = pair[0], v
			}
			if kind == "" {
				if role == "model" && (hasSig || thought) {
					continue
				}
				return contextInvalid(partField, "A part must contain supported content")
			}
			if kind == "inlineData" && role != "user" && !webInput {
				return contextInlineError(inlineFailure(contract.AttachmentUnsupportedModel, "", "Model history inlineData is unsupported"), partField+".inlineData")
			}
			if thought && kind != "text" {
				return contextUnsupported(partField)
			}
			switch kind {
			case "text":
				text, err := readText(rawValue, partField+".text")
				if err != nil {
					return err
				}
				if thought {
					request.ContextSummary.FilteredThoughts++
					continue
				}
				value.Parts = append(value.Parts, contextPart{Kind: "text", Text: &text})
			case "inlineData":
				if len(pending) >= maxAttachments {
					return contextLimit(partField+".inlineData", contract.LimitAttachments, int64(len(pending)+1), int64(maxAttachments), true)
				}
				input, err := inlineFields(ctx, rawValue)
				if err != nil {
					return contextInlineError(err, partField+".inlineData")
				}
				ref := &contextAttachment{ID: fmt.Sprintf("c%d_p%d", i, j), MIMEType: input.mimeType}
				pending = append(pending, pendingContextAttachment{content: i, part: j, raw: rawValue, ref: ref})
				value.Parts = append(value.Parts, contextPart{Kind: "attachment", Attachment: ref})
			case "fileData":
				return contextUnsupported(partField + ".fileData")
			default:
				if (kind == "functionResponse") != (role == "user") {
					return contextUnsupported(partField + "." + kind)
				}
				if int64(len(rawValue)) > maxTextBytes-textBytes {
					return contextLimit(partField, contract.LimitHistoryTextBytes, textBytes+int64(len(rawValue)), maxTextBytes, true)
				}
				textBytes += int64(len(rawValue))
				if err := validateContextRecord(ctx, kind, rawValue, partField+"."+kind); err != nil {
					return err
				}
				value.Parts = append(value.Parts, contextPart{Kind: kind, Record: rawValue})
			}
		}
		values = append(values, value)
	}
	if lastUser < 0 {
		return contextInvalid("contents", "Web history requires a user input")
	}
	// Truncated histories may begin with model content; trailing model fragments
	// remain a response prefix instead of being relabelled or silently discarded.
	doc := contextDocument{Schema: "enterprise-web-context-v1", History: values[:lastUser], Current: values[lastUser], ModelContinuation: values[lastUser+1:]}
	system, exists, err := alias(root, "systemInstruction", "system_instruction")
	if err != nil {
		return contextInvalid("systemInstruction", "Conflicting system instruction aliases")
	}
	if exists {
		object, err := objectContext(ctx, system)
		if err != nil {
			return contextParseError(err, "systemInstruction", "System instructions must be an object")
		}
		for key := range object {
			if key != "role" && key != "parts" {
				return contextUnsupported("systemInstruction")
			}
		}
		if role, ok := object["role"]; ok {
			if _, err := readText(role, "systemInstruction.role"); err != nil {
				return err
			}
		}
		parts, err := rawArray(object["parts"], limits.MaxSystemParts)
		if err != nil && json.Valid(object["parts"]) && bytes.HasPrefix(bytes.TrimSpace(object["parts"]), []byte("[")) {
			return contextLimit("systemInstruction.parts", contract.LimitSystemParts, int64(limits.MaxSystemParts+1), int64(limits.MaxSystemParts), true)
		}
		if err != nil || len(parts) == 0 {
			return contextInvalid("systemInstruction.parts", "System instructions require text parts")
		}
		for _, raw := range parts {
			part, err := objectContext(ctx, raw)
			if err != nil {
				return contextParseError(err, "systemInstruction.parts", "Invalid system instruction")
			}
			if len(part) != 1 || part["text"] == nil {
				return contextUnsupported("systemInstruction.parts")
			}
			text, err := readText(part["text"], "systemInstruction.parts.text")
			if err != nil {
				return err
			}
			doc.System = append(doc.System, text)
			nodes++
		}
	}
	// Reserve before encoding: escaping can expand strings sixfold. Account for
	// encoder scratch, encoded bytes, the query string and outer payload framing.
	upper := textBytes*6 + int64(nodes+len(values))*512 + 4096
	if err := reserveHistory(ctx, upper*4); err != nil {
		return err
	}
	for ordinal, item := range pending {
		a, err := parseInline(ctx, item.raw, ordinal+1, maxAttachmentBytes-int64(request.ContextSummary.AttachmentBytes))
		if err != nil {
			return contextInlineError(err, fmt.Sprintf("contents[%d].parts[%d].inlineData", item.content, item.part))
		}
		a.Filename = fmt.Sprintf("c%04d-p%04d%s", item.content, item.part, path.Ext(a.Filename))
		sum := sha256.Sum256(a.Data)
		a.Hash = hex.EncodeToString(sum[:])
		item.ref.Filename = a.Filename
		request.Attachments = append(request.Attachments, a)
		request.ContextSummary.AttachmentBytes += len(a.Data)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return contextInvalid("contents", "Cannot encode conversation context")
	}
	instruction := contextInstruction
	for _, content := range doc.ModelContinuation {
		for _, part := range content.Parts {
			if part.Kind != "text" || (part.Text != nil && *part.Text != "") {
				instruction = contextContinuationInstruction
			}
		}
	}
	request.QueryParts = []map[string]string{{"text": instruction + string(encoded)}}
	request.ContextSummary.Contents = len(values)
	request.ContextSummary.LogicalContents = logical
	request.ContextSummary.Attachments = len(pending)
	return nil
}

func validateContextRecord(ctx context.Context, kind string, raw json.RawMessage, field string) error {
	object, err := objectContext(ctx, raw)
	if err != nil {
		return contextParseError(err, field, "Historical tool records must be objects with unique fields")
	}
	allowed := "|name|id|args|"
	if kind == "functionResponse" {
		allowed = "|name|id|response|"
	}
	if kind == "executableCode" {
		allowed = "|language|code|"
	}
	if kind == "codeExecutionResult" {
		allowed = "|outcome|output|"
	}
	for key, value := range object {
		if !strings.Contains(allowed, "|"+key+"|") {
			return contextUnsupported(field)
		}
		if key == "args" || key == "response" {
			if _, err := objectContext(ctx, value); err != nil {
				return contextParseError(err, field+"."+key, "Historical arguments and results must be objects")
			}
		} else if _, err := jsonStringSize(value); err != nil {
			return contextInvalid(field+"."+key, "Historical tool fields must be strings")
		}
	}
	if kind == "functionResponse" && object["response"] == nil {
		return contextInvalid(field+".response", "Historical function response requires a response object")
	}
	nodes := 0
	return validateContextJSON(ctx, raw, field, 0, &nodes)
}

func validateContextJSON(ctx context.Context, raw json.RawMessage, field string, depth int, nodes *int) error {
	limits := inputLimits(ctx)
	*nodes++
	if depth > limits.MaxToolDepth {
		return contextLimit(field, contract.LimitToolDepth, int64(depth), int64(limits.MaxToolDepth), true)
	}
	if *nodes > limits.MaxToolNodes {
		return contextLimit(field, contract.LimitToolNodes, int64(*nodes), int64(limits.MaxToolNodes), true)
	}
	if err := ctx.Err(); err != nil {
		return resources.ContextError(err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return contextInvalid(field, "Invalid historical tool data")
	}
	switch raw[0] {
	case '{':
		object, err := objectContext(ctx, raw)
		if err != nil {
			return contextParseError(err, field, "Historical tool data requires unique object fields")
		}
		if _, err := resources.Reserve(ctx, int64(len(object))*128); err != nil {
			return err
		}
		for _, value := range object {
			if err := validateContextJSON(ctx, value, field, depth+1, nodes); err != nil {
				return err
			}
		}
	case '[':
		values, err := rawArray(raw, limits.MaxToolArrayEntries)
		if err != nil {
			return contextLimit(field, contract.LimitToolArrayEntries, int64(limits.MaxToolArrayEntries+1), int64(limits.MaxToolArrayEntries), true)
		}
		if _, err := resources.Reserve(ctx, int64(len(values))*24); err != nil {
			return err
		}
		for _, value := range values {
			if err := validateContextJSON(ctx, value, field, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}
