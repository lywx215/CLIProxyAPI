// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
)

const maxHistoryContents = 33

// Official Chat SDKs retain each streamed model delta as a Content. Bound the
// raw fragments separately; their adjacent model parts form one history turn.
const maxHistorySegments = 4096 + maxHistoryContents

type historyTurn struct {
	role  string
	parts []contract.HistoryPart
}

type historyAttachment struct {
	turn, part, ordinal int
	inline              json.RawMessage
	field               string
	uri, mime           string
	model               bool
}

type historyPlan struct {
	turns   []historyTurn
	pending []historyAttachment
}

// planHistory validates every turn before the file resolver is called. Raw
// slices refer to the single budgeted request body; history is never a prompt.
func planHistory(ctx context.Context, raw json.RawMessage) (*historyPlan, error) {
	limits := inputLimits(ctx)
	webInput := usesWebInputLimits(ctx)
	maxHistorySegments, maxHistoryContents, maxAttachments := limits.MaxSegments, limits.MaxContents, limits.MaxAttachments
	contents, err := rawArray(raw, maxHistorySegments)
	if err != nil || len(contents) == 0 {
		return nil, invalid("contents must contain alternating user/model messages within the configured limits and end with user")
	}
	plan := &historyPlan{}
	textBytes := int64(0)
	userAttachments := 0
	for i, rawContent := range contents {
		if err := ctx.Err(); err != nil {
			return nil, resources.ContextError(err)
		}
		content, err := objectContext(ctx, rawContent)
		if err != nil {
			return nil, err
		}
		for key := range content {
			if key != "role" && key != "parts" {
				return nil, unsupported("contents." + key)
			}
		}
		role := "user"
		if value, present := content["role"]; present {
			if len(value) > 32 || json.Unmarshal(value, &role) != nil || (role != "user" && role != "model") {
				return nil, invalid("contents roles must alternate user/model and end with user")
			}
		} else if len(contents) != 1 {
			return nil, invalid("multi-turn contents require explicit user/model roles")
		}
		if i == 0 && role != "user" {
			return nil, invalid("contents must start with user")
		}
		last := len(plan.turns) - 1
		if last < 0 || plan.turns[last].role != role {
			if len(plan.turns) >= maxHistoryContents {
				return nil, invalid("Business history exceeds its configured content limit")
			}
			plan.turns = append(plan.turns, historyTurn{role: role})
		} else if role != "model" {
			return nil, invalid("consecutive user contents are not supported")
		}
		turnIndex := len(plan.turns) - 1
		turn := &plan.turns[turnIndex]
		partLimit := limits.MaxUserParts
		if role == "model" {
			partLimit = limits.MaxModelParts - len(turn.parts)
		}
		if partLimit <= 0 {
			return nil, invalid("model history exceeds its configured part limit")
		}
		parts, err := rawArray(content["parts"], partLimit)
		if err != nil || len(parts) == 0 {
			return nil, invalid("user and model contents require valid parts within their configured limits")
		}
		// Account for the parsed descriptors and raw-array views as well as
		// their strings. Many small historical parts cannot bypass the budget.
		if _, err := resources.Reserve(ctx, int64(len(parts))*160); err != nil {
			return nil, err
		}
		partBase := len(turn.parts)
		turn.parts = append(turn.parts, make([]contract.HistoryPart, len(parts))...)
		groups, ordinal, previous := 0, 0, ""
		for j, rawPart := range parts {
			part, err := objectContext(ctx, rawPart)
			if err != nil {
				return nil, err
			}
			inline, hasInline, err := alias(part, "inlineData", "inline_data")
			if err != nil {
				if webInput {
					return nil, inlineField(inlineFailure(contract.AttachmentFieldConflict, "", "A part must not contain both inlineData aliases"), fmt.Sprintf("contents[%d].parts[%d].inlineData", i, j))
				}
				return nil, err
			}
			file, hasFile, err := alias(part, "fileData", "file_data")
			if err != nil {
				return nil, err
			}
			code, hasCode, err := alias(part, "executableCode", "executable_code")
			if err != nil {
				return nil, err
			}
			result, hasResult, err := alias(part, "codeExecutionResult", "code_execution_result")
			if err != nil {
				return nil, err
			}
			value := &turn.parts[partBase+j]
			if role == "user" {
				if len(part) != 1 {
					if hasInline && webInput {
						return nil, inlineField(inlineFailure(contract.AttachmentFieldConflict, "", "An inlineData part must not contain other fields"), fmt.Sprintf("contents[%d].parts[%d].inlineData", i, j))
					}
					return nil, unsupported("mixed or annotated user content parts")
				}
			} else {
				for key := range part {
					if key != "text" && key != "inlineData" && key != "inline_data" && key != "thought" && key != "thoughtSignature" && key != "thought_signature" && key != "executableCode" && key != "executable_code" && key != "codeExecutionResult" && key != "code_execution_result" {
						return nil, unsupported("model history part " + key)
					}
				}
				if thought, ok := part["thought"]; ok {
					if bytes.Equal(bytes.TrimSpace(thought), []byte("null")) || json.Unmarshal(thought, &value.Thought) != nil {
						return nil, invalid("model history thought must be a boolean")
					}
				}
				signature, present, err := alias(part, "thoughtSignature", "thought_signature")
				if err != nil {
					return nil, err
				}
				if present {
					value.ThoughtSignature, err = historyString(ctx, signature, &textBytes)
					if err != nil {
						return nil, err
					}
				}
			}
			text, hasText := part["text"]
			union := 0
			for _, present := range []bool{hasText, hasInline, hasFile, hasCode, hasResult} {
				if present {
					union++
				}
			}
			if union != 1 {
				return nil, unsupported("mixed content part")
			}
			kind := "attachment"
			switch {
			case hasCode && role == "model":
				value.ExecutableCode, err = parseHistoryCode(ctx, code, &textBytes)
				if err != nil {
					return nil, err
				}
			case hasResult && role == "model":
				value.CodeExecutionResult, err = parseHistoryCodeResult(ctx, result, &textBytes)
				if err != nil {
					return nil, err
				}
			case hasText && !hasInline && !hasFile:
				kind = "text"
				decoded, err := historyString(ctx, text, &textBytes)
				if err != nil {
					return nil, err
				}
				value.Text = &decoded
			case hasInline && !hasText && !hasFile:
				ordinal++
				field := fmt.Sprintf("contents[%d].parts[%d].inlineData", i, j)
				if role == "model" {
					if webInput {
						return nil, inlineField(inlineFailure(contract.AttachmentUnsupportedModel, "", "Model history inlineData is unsupported"), field)
					}
					blob, err := modelHistoryBlob(ctx, inline)
					if err != nil {
						return nil, err
					}
					mimeRaw, _, _ := alias(blob, "mimeType", "mime_type")
					var mimeType string
					_ = json.Unmarshal(mimeRaw, &mimeType)
					if mimeType == contract.SuggestionsMIMEType && (value.Thought || value.ThoughtSignature != "") {
						return nil, invalid("model suggestions cannot carry thought annotations")
					}
				} else {
					userAttachments++
					if webInput && userAttachments > maxAttachments {
						return nil, contextLimit(field, contract.LimitAttachments, int64(userAttachments), int64(maxAttachments), true)
					}
					if _, err := inlineFields(ctx, inline); err != nil {
						return nil, inlineField(err, field)
					}
				}
				plan.pending = append(plan.pending, historyAttachment{turn: turnIndex, part: partBase + j, ordinal: ordinal, inline: inline, field: field, model: role == "model"})
			case hasFile && !hasText && !hasInline && role == "user":
				ordinal++
				userAttachments++
				uri, mime, err := parseFile(ctx, file)
				if err != nil {
					return nil, err
				}
				plan.pending = append(plan.pending, historyAttachment{turn: turnIndex, part: partBase + j, ordinal: ordinal, uri: uri, mime: mime})
			default:
				return nil, unsupported("content part")
			}
			if kind != previous {
				groups++
				previous = kind
			}
			if role == "user" && groups > 2 {
				return nil, unsupported("interleaved text and file parts")
			}
			if userAttachments > maxAttachments {
				return nil, invalid("Business requests exceed the configured user attachment count")
			}
		}
	}
	if len(plan.turns)%2 != 1 || plan.turns[len(plan.turns)-1].role != "user" {
		return nil, invalid("contents must end with user")
	}
	return plan, nil
}

func parseHistoryCode(ctx context.Context, raw json.RawMessage, total *int64) (*contract.ExecutableCode, error) {
	fields, err := objectContext(ctx, raw)
	if err != nil {
		return nil, err
	}
	if len(fields) != 2 {
		return nil, invalid("model executableCode requires language and code")
	}
	var language string
	if len(fields["language"]) > 32 || json.Unmarshal(fields["language"], &language) != nil || language != "PYTHON" {
		return nil, invalid("model executableCode language must be PYTHON")
	}
	code, err := historyString(ctx, fields["code"], total)
	if err != nil {
		return nil, err
	}
	if code == "" {
		return nil, invalid("model executableCode must not be empty")
	}
	return &contract.ExecutableCode{Language: language, Code: code}, nil
}
func parseHistoryCodeResult(ctx context.Context, raw json.RawMessage, total *int64) (*contract.CodeExecutionResult, error) {
	fields, err := objectContext(ctx, raw)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "outcome" && key != "output" {
			return nil, unsupported("model codeExecutionResult field")
		}
	}
	var outcome string
	if len(fields["outcome"]) > 64 || json.Unmarshal(fields["outcome"], &outcome) != nil || !contract.ValidExecutionOutcome(outcome) {
		return nil, invalid("model codeExecutionResult outcome is invalid")
	}
	output := ""
	if value, present := fields["output"]; present {
		output, err = historyString(ctx, value, total)
		if err != nil {
			return nil, err
		}
	}
	return &contract.CodeExecutionResult{Outcome: outcome, Output: output}, nil
}

func historyString(ctx context.Context, raw json.RawMessage, total *int64) (string, error) {
	maxTextBytes := inputLimits(ctx).MaxTextBytes
	size, err := jsonStringSize(raw)
	if err != nil {
		return "", err
	}
	if size > maxTextBytes-*total {
		return "", contract.LimitFailure("contents", "Business history text and signatures exceed their configured byte limit", contract.InputLimit{Name: contract.LimitHistoryTextBytes, Observed: *total + size, Maximum: maxTextBytes, AtLeast: true})
	}
	if _, err := resources.Reserve(ctx, size); err != nil {
		return "", err
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", invalid("history text or signature must be a string")
	}
	*total += size
	return value, nil
}

func modelHistoryBlob(ctx context.Context, raw json.RawMessage) (map[string]json.RawMessage, error) {
	blob, err := objectContext(ctx, raw)
	if err != nil {
		return nil, err
	}
	for key := range blob {
		if key != "mimeType" && key != "mime_type" && key != "data" {
			return nil, unsupported("model inlineData." + key)
		}
	}
	mime, _, err := alias(blob, "mimeType", "mime_type")
	if err != nil {
		return nil, err
	}
	var mimeType string
	if len(mime) > 1024 || json.Unmarshal(mime, &mimeType) != nil || !contract.ValidOutputInlineMIME(mimeType) {
		return nil, unsupported("model history inlineData MIME type")
	}
	if _, err := jsonStringSize(blob["data"]); err != nil {
		return nil, err
	}
	return blob, nil
}

func decodeModelHistoryBlob(ctx context.Context, raw json.RawMessage, remaining int64) ([]byte, error) {
	blob, err := modelHistoryBlob(ctx, raw)
	if err != nil {
		return nil, err
	}
	size, err := jsonStringSize(blob["data"])
	if err != nil {
		return nil, err
	}
	if size > 4<<20 || size/4*3-2 > remaining {
		return nil, contract.Fail(413, "RESOURCE_EXHAUSTED", "Business model history inline data exceeds its limit")
	}
	release, err := resources.Reserve(ctx, size)
	if err != nil {
		return nil, err
	}
	defer release()
	var encoded string
	if json.Unmarshal(blob["data"], &encoded) != nil || encoded == "" || strings.ContainsAny(encoded, " \r\n\t") {
		return nil, invalid("model history inlineData must contain valid base64")
	}
	encoding := base64.StdEncoding.Strict()
	if strings.ContainsAny(encoded, "-_") {
		if strings.ContainsAny(encoded, "+/") {
			return nil, invalid("model history inlineData must contain valid base64")
		}
		encoding = base64.URLEncoding.Strict()
	}
	padding := len(encoded) - len(strings.TrimRight(encoded, "="))
	if padding == 0 {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	decodedSize := int64(encoding.DecodedLen(len(encoded))) - int64(padding)
	if padding > 2 || decodedSize < 1 {
		return nil, invalid("model history inlineData must contain valid base64")
	}
	if decodedSize > remaining || decodedSize+int64(base64.StdEncoding.EncodedLen(int(decodedSize))) > 4<<20 {
		return nil, contract.Fail(413, "RESOURCE_EXHAUSTED", "Business model history inline data exceeds its limit")
	}
	if _, err := resources.Reserve(ctx, int64(encoding.DecodedLen(len(encoded)))); err != nil {
		return nil, err
	}
	decoded, err := encoding.DecodeString(encoded)
	mimeRaw, _, _ := alias(blob, "mimeType", "mime_type")
	var mimeType string
	_ = json.Unmarshal(mimeRaw, &mimeType)
	if err != nil || !contract.ValidOutputInlineData(&contract.InlineData{MIMEType: mimeType, Data: decoded}) {
		return nil, invalid("model history inlineData must contain valid data")
	}
	if len(decoded)+base64.StdEncoding.EncodedLen(len(decoded)) > 4<<20 || int64(len(decoded)) > remaining {
		return nil, contract.Fail(413, "RESOURCE_EXHAUSTED", "Business model history inline data exceeds its limit")
	}
	return decoded, nil
}

func (p *historyPlan) resolve(ctx context.Context, resolver contract.FileResolver, request *contract.Request) error {
	maxAttachmentBytes := inputLimits(ctx).MaxAttachmentBytes
	total := 0
	last := len(p.turns) - 1
	for _, pending := range p.pending {
		if err := ctx.Err(); err != nil {
			return resources.ContextError(err)
		}
		var a contract.Attachment
		var err error
		switch {
		case pending.model:
			blob, mimeErr := modelHistoryBlob(ctx, pending.inline)
			if mimeErr != nil {
				return mimeErr
			}
			mimeRaw, _, _ := alias(blob, "mimeType", "mime_type")
			_ = json.Unmarshal(mimeRaw, &a.MIMEType)
			a.Data, err = decodeModelHistoryBlob(ctx, pending.inline, maxAttachmentBytes-int64(total))
		case pending.inline != nil:
			a, err = parseInline(ctx, pending.inline, pending.ordinal, maxAttachmentBytes-int64(total))
			if err != nil {
				err = inlineField(err, pending.field)
			}
		default:
			if resolver == nil {
				return unsupported("fileData without a Business file resolver")
			}
			a, err = resolver(resources.WithAttachmentAllowance(ctx, maxAttachmentBytes-int64(total)), pending.uri, pending.mime)
			if err != nil {
				var failure *contract.Failure
				if errors.As(err, &failure) {
					return failure
				}
				return invalid("Business file reference could not be resolved")
			}
			resolvedMIME, mimeErr := supportedMIME(a.MIMEType)
			if mimeErr != nil || resolvedMIME != pending.mime {
				return invalid("fileData MIME type does not match the Business file")
			}
			a.MIMEType = resolvedMIME
		}
		if err != nil {
			return err
		}
		total += len(a.Data)
		if len(a.Data) == 0 {
			return invalid("attachment data must not be empty")
		}
		if int64(total) > maxAttachmentBytes {
			return contract.LimitFailure("contents", "Business attachments exceed their configured byte limit", contract.InputLimit{Name: contract.LimitAttachmentBytes, Observed: int64(total), Maximum: maxAttachmentBytes, AtLeast: true})
		}
		if pending.inline == nil && pending.turn == last {
			if _, err := resources.Reserve(ctx, int64(len(a.Data))); err != nil {
				return err
			}
			a.Data = append([]byte(nil), a.Data...)
		}
		sum := sha256.Sum256(a.Data)
		a.Hash = hex.EncodeToString(sum[:])
		part := &p.turns[pending.turn].parts[pending.part]
		part.MIMEType, part.FileHash, part.Filename = a.MIMEType, a.Hash, a.Filename
		if pending.turn == last {
			request.Attachments = append(request.Attachments, a)
		}
	}
	for i, turn := range p.turns {
		request.History = append(request.History, contract.HistoryDigest(turn.role, turn.parts))
		if i == last {
			for _, part := range turn.parts {
				if part.Text != nil {
					request.QueryParts = append(request.QueryParts, map[string]string{"text": *part.Text})
				}
			}
		}
	}
	return nil
}

func hashString(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}

// Canonical object order and decoded strings avoid false mismatches caused by
// JSON formatting. Number lexemes and aliases remain conservative identities;
// a changed option can reject continuation even when that option is ignored.
func historyOptionsHash(ctx context.Context, root map[string]json.RawMessage, binding config.Binding, generation map[string]json.RawMessage) (string, error) {
	h := sha256.New()
	hashString(h, "business-history-options-v1")
	nodes := 0
	for _, value := range []any{binding, generation} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", invalid("Business history options cannot be encoded")
		}
		if err := canonicalHistoryJSON(ctx, h, encoded, 0, &nodes); err != nil {
			return "", err
		}
	}
	keys, size := make([]string, 0, len(root)), 0
	for key, value := range root {
		if key == "contents" {
			continue
		}
		size += len(key) + len(value)
		if size > maxTextBytes {
			return "", contract.LimitFailure("body", "Business history options exceed 1 MiB", contract.InputLimit{Name: contract.LimitHistoryOptionsBytes, Observed: int64(size), Maximum: maxTextBytes, AtLeast: true})
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hashString(h, "options")
	hashString(h, strconv.Itoa(len(keys)))
	for _, key := range keys {
		hashString(h, key)
		if err := canonicalHistoryJSON(ctx, h, root[key], 0, &nodes); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalHistoryJSON(ctx context.Context, h hash.Hash, raw json.RawMessage, depth int, nodes *int) error {
	*nodes++
	if depth > 64 {
		return contract.LimitFailure("body", "Business history options are too complex", contract.InputLimit{Name: contract.LimitHistoryOptionsDepth, Observed: int64(depth), Maximum: 64, AtLeast: true})
	}
	if *nodes > 16384 {
		return contract.LimitFailure("body", "Business history options are too complex", contract.InputLimit{Name: contract.LimitHistoryOptionsNodes, Observed: int64(*nodes), Maximum: 16384, AtLeast: true})
	}
	if err := ctx.Err(); err != nil {
		return resources.ContextError(err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return invalid("invalid history option")
	}
	switch raw[0] {
	case '{':
		fields, err := objectContext(ctx, raw)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		hashString(h, "object")
		hashString(h, strconv.Itoa(len(keys)))
		for _, key := range keys {
			hashString(h, key)
			if err := canonicalHistoryJSON(ctx, h, fields[key], depth+1, nodes); err != nil {
				return err
			}
		}
		hashString(h, "end-object")
	case '[':
		values, err := rawArray(raw, 4096)
		if err != nil {
			return err
		}
		hashString(h, "array")
		hashString(h, strconv.Itoa(len(values)))
		for _, value := range values {
			if err := canonicalHistoryJSON(ctx, h, value, depth+1, nodes); err != nil {
				return err
			}
		}
		hashString(h, "end-array")
	case '"':
		size, err := jsonStringSize(raw)
		if err != nil {
			return err
		}
		release, err := resources.Reserve(ctx, size)
		if err != nil {
			return err
		}
		defer release()
		var decoded string
		if json.Unmarshal(raw, &decoded) != nil {
			return invalid("invalid history option string")
		}
		hashString(h, "string")
		hashString(h, decoded)
	default:
		hashString(h, "scalar")
		hashString(h, string(raw))
	}
	return nil
}
