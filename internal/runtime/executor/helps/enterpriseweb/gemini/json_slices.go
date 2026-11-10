// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
)

// These views retain the original, budgeted body instead of successively
// copying contents, parts and base64 strings into multiple RawMessages.
func rawObject(data []byte) (map[string]json.RawMessage, error) {
	return objectContext(context.Background(), data)
}

type duplicateJSONFieldError struct {
	error
	field string
}

func (e duplicateJSONFieldError) Unwrap() error { return e.error }

func objectContext(ctx context.Context, data []byte) (map[string]json.RawMessage, error) {
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '{' || !utf8.Valid(data) || !json.Valid(data) {
		return nil, invalid("expected a JSON object")
	}
	result := map[string]json.RawMessage{}
	maximum := inputLimits(ctx).MaxJSONObjectFields
	at := skipJSONSpace(data, 1)
	for at < len(data)-1 {
		if len(result) >= maximum {
			return nil, contract.LimitFailure("", "Business JSON object exceeds its configured field limit", contract.InputLimit{Name: contract.LimitJSONObjectFields, Observed: int64(maximum + 1), Maximum: int64(maximum), AtLeast: true})
		}
		end := jsonValueEnd(data, at)
		if end-at > 1<<20 {
			return nil, invalid("JSON field name exceeds 1 MiB")
		}
		var name string
		keyBytes, err := jsonStringSize(data[at:end])
		if err != nil {
			return nil, err
		}
		if _, err := resources.Reserve(ctx, keyBytes); err != nil {
			return nil, err
		}
		if json.Unmarshal(data[at:end], &name) != nil {
			return nil, invalid("invalid JSON key")
		}
		if _, exists := result[name]; exists {
			return nil, duplicateJSONFieldError{invalid("duplicate JSON field"), name}
		}
		at = skipJSONSpace(data, end) + 1
		at = skipJSONSpace(data, at)
		end = jsonValueEnd(data, at)
		result[name] = data[at:end]
		at = skipJSONSpace(data, end)
		if at < len(data) && data[at] == ',' {
			at = skipJSONSpace(data, at+1)
		}
	}
	return result, nil
}

// jsonStringSize computes the exact decoded UTF-8 size before Unmarshal can
// allocate a large string. Unpaired JSON surrogates become U+FFFD, as in Go.
func jsonStringSize(raw []byte) (int64, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || !json.Valid(raw) {
		return 0, invalid("expected a JSON string")
	}
	n := int64(0)
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			n++
			continue
		}
		i++
		if raw[i] != 'u' {
			n++
			continue
		}
		value, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return 0, invalid("invalid JSON string")
		}
		i += 4
		r := rune(value)
		if r >= 0xd800 && r <= 0xdbff && i+6 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' {
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err == nil && low >= 0xdc00 && low <= 0xdfff {
				n += 4
				i += 6
				continue
			}
		}
		if r >= 0xd800 && r <= 0xdfff {
			n += 3
		} else {
			n += int64(utf8.RuneLen(r))
		}
	}
	return n, nil
}

func rawArray(data []byte, maximum int) ([]json.RawMessage, error) {
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '[' || !json.Valid(data) {
		return nil, invalid("expected a JSON array")
	}
	result := make([]json.RawMessage, 0, min(maximum, 8))
	at := skipJSONSpace(data, 1)
	for at < len(data)-1 {
		if len(result) >= maximum {
			return nil, invalid("JSON array has too many entries")
		}
		end := jsonValueEnd(data, at)
		result = append(result, data[at:end])
		at = skipJSONSpace(data, end)
		if at < len(data) && data[at] == ',' {
			at = skipJSONSpace(data, at+1)
		}
	}
	return result, nil
}

func skipJSONSpace(data []byte, at int) int {
	for at < len(data) {
		switch data[at] {
		case ' ', '\t', '\r', '\n':
			at++
		default:
			return at
		}
	}
	return at
}

// json.Valid has already checked syntax and depth. This scanner only finds a
// value's ending offset; quoted braces and escaped quotes are skipped intact.
func jsonValueEnd(data []byte, at int) int {
	if data[at] == '"' {
		for i := at + 1; i < len(data); i++ {
			if data[i] == '\\' {
				i++
				continue
			}
			if data[i] == '"' {
				return i + 1
			}
		}
	}
	if data[at] == '{' || data[at] == '[' {
		depth := 0
		for i := at; i < len(data); i++ {
			switch data[i] {
			case '"':
				i = jsonValueEnd(data, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
	}
	i := at
	for i < len(data) && data[i] != ',' && data[i] != ']' && data[i] != '}' && data[i] != ' ' && data[i] != '\t' && data[i] != '\r' && data[i] != '\n' {
		i++
	}
	return i
}
