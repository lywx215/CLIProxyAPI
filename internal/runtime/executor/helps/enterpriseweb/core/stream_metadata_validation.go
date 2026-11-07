// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

// These local enum values are deliberately fixed. Diagnostics never include
// response field names, JSON values, text, signatures or decoder error strings.
type streamDiagnostic string

const (
	streamInvalidUTF8                    streamDiagnostic = "stream_invalid_utf8"
	streamInvalidUnicodeEscape           streamDiagnostic = "stream_invalid_unicode_escape"
	streamNonObjectFrame                 streamDiagnostic = "stream_non_object_frame"
	streamDuplicateJSONKey               streamDiagnostic = "stream_duplicate_json_key"
	streamNestingLimit                   streamDiagnostic = "stream_nesting_limit"
	streamInvalidJSONStructure           streamDiagnostic = "stream_invalid_json_structure"
	streamInvalidEnvelope                streamDiagnostic = "stream_invalid_envelope"
	streamInvalidTypedFrame              streamDiagnostic = "stream_invalid_typed_frame"
	streamUnknownRPCError                streamDiagnostic = "stream_unknown_rpc_error"
	streamDataAfterTerminal              streamDiagnostic = "stream_data_after_terminal"
	streamInvalidRole                    streamDiagnostic = "stream_invalid_role"
	streamMixedInlinePart                streamDiagnostic = "stream_mixed_inline_part"
	streamMixedContentPart               streamDiagnostic = "stream_mixed_content_part"
	streamInvalidExecutableCode          streamDiagnostic = "stream_invalid_executable_code"
	streamUnsupportedExecutableCode      streamDiagnostic = "stream_unsupported_executable_code"
	streamInvalidCodeExecutionResult     streamDiagnostic = "stream_invalid_code_execution_result"
	streamUnsupportedCodeExecutionResult streamDiagnostic = "stream_unsupported_code_execution_result"
	streamInvalidGroundingMetadata       streamDiagnostic = "stream_invalid_grounding_metadata"
	streamInvalidText                    streamDiagnostic = "stream_invalid_text"
	streamInvalidThought                 streamDiagnostic = "stream_invalid_thought"
	streamInvalidSignature               streamDiagnostic = "stream_invalid_signature"
	streamMissingContentValue            streamDiagnostic = "stream_missing_content_value"
	streamUnknownAnswerState             streamDiagnostic = "stream_unknown_answer_state"
	streamUnexpectedClosingToken         streamDiagnostic = "stream_unexpected_closing_token"
	streamMissingTerminalSuccess         streamDiagnostic = "stream_missing_terminal_success"
	streamTrailingData                   streamDiagnostic = "stream_trailing_data"
	streamTruncatedJSON                  streamDiagnostic = "stream_truncated_json"
	streamInvalidJSON                    streamDiagnostic = "stream_invalid_json"
	streamUnsupportedReply               streamDiagnostic = "stream_unsupported_reply"
	streamUnsupportedContent             streamDiagnostic = "stream_unsupported_content"
	streamUnsupportedInlineType          streamDiagnostic = "stream_unsupported_inline_type"
	streamInvalidInlineData              streamDiagnostic = "stream_invalid_inline_data"
	streamInvalidInlineEncoding          streamDiagnostic = "stream_invalid_inline_encoding"
	streamInvalidSuggestionsEncoding     streamDiagnostic = "stream_invalid_suggestions_encoding"
	streamInvalidSuggestionsJSON         streamDiagnostic = "stream_invalid_suggestions_json"
)

func malformedStreamAt(reason streamDiagnostic) error {
	return contract.Fail(502, "DATA_LOSS", "Business response is malformed or incomplete ("+string(reason)+")")
}

func unsupportedStreamAt(reason streamDiagnostic) error {
	return contract.Fail(502, "UNIMPLEMENTED", "Business response is malformed or incomplete ("+string(reason)+")")
}

// validateStreamFrame checks wire integrity before typed decoding can collapse
// duplicate keys or substitute invalid text. It does not interpret or forward
// unknown metadata, nor translate Enterprise grounding into Gemini grounding.
func validateStreamFrame(raw []byte) error {
	if !utf8.Valid(raw) {
		return malformedStreamAt(streamInvalidUTF8)
	}
	if !validStreamUnicodeEscapes(raw) {
		return malformedStreamAt(streamInvalidUnicodeEscape)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return malformedStreamAt(streamNonObjectFrame)
	}
	if err := validateStreamContainer(decoder, '{', 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return malformedStreamAt(streamInvalidJSONStructure)
	}
	return nil
}

// Go's JSON decoder also replaces unpaired UTF-16 escapes with U+FFFD. An
// escaped surrogate must have its matching partner; literal backslashes are
// skipped so text such as "\\ud800" keeps its ordinary string meaning.
func validStreamUnicodeEscapes(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		if i+1 >= len(raw) {
			return false
		}
		if raw[i+1] != 'u' {
			i++
			continue
		}
		if i+6 > len(raw) {
			return false
		}
		value, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
		if err != nil {
			return false
		}
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 11
		} else {
			i += 5
		}
	}
	return !inString
}

func validateStreamContainer(decoder *json.Decoder, kind json.Delim, depth int) error {
	// No supported response field requires deeply recursive object graphs.
	// This also bounds recursion while ignoring uninterpreted metadata.
	if depth > 64 {
		return malformedStreamAt(streamNestingLimit)
	}
	keys := map[string]struct{}{}
	for decoder.More() {
		if kind == '{' {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return malformedStreamAt(streamInvalidJSONStructure)
			}
			if _, exists := keys[key]; exists {
				return malformedStreamAt(streamDuplicateJSONKey)
			}
			keys[key] = struct{}{}
		}
		token, err := decoder.Token()
		if err != nil {
			return malformedStreamAt(streamInvalidJSONStructure)
		}
		if delimiter, ok := token.(json.Delim); ok {
			if delimiter != '{' && delimiter != '[' {
				return malformedStreamAt(streamInvalidJSONStructure)
			}
			if err := validateStreamContainer(decoder, delimiter, depth+1); err != nil {
				return err
			}
		}
	}
	end, err := decoder.Token()
	if err != nil || (kind == '{' && end != json.Delim('}')) || (kind == '[' && end != json.Delim(']')) {
		return malformedStreamAt(streamInvalidJSONStructure)
	}
	return nil
}
