// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

// decodeInlineData validates the native Blob envelope, not the media format.
// A media Part may be a fragment, so never decode containers or join Parts.
func decodeInlineData(raw json.RawMessage) (*contract.InlineData, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, malformedStreamAt(streamInvalidInlineData)
	}
	fields := map[string]string{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || (name != "mimeType" && name != "data") {
			return nil, malformedStreamAt(streamInvalidInlineData)
		}
		if _, exists := fields[name]; exists {
			return nil, malformedStreamAt(streamDuplicateJSONKey)
		}
		var value string
		if decoder.Decode(&value) != nil || value == "" {
			return nil, malformedStreamAt(streamInvalidInlineData)
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(fields) != 2 {
		return nil, malformedStreamAt(streamInvalidInlineData)
	}
	if !contract.ValidOutputInlineMIME(fields["mimeType"]) {
		return nil, unsupportedStreamAt(streamUnsupportedInlineType)
	}
	encodingFailure := streamInvalidInlineEncoding
	if fields["mimeType"] == contract.SuggestionsMIMEType {
		encodingFailure = streamInvalidSuggestionsEncoding
	}
	data, err := decodeStreamBase64(fields["data"], encodingFailure)
	if err != nil {
		return nil, err
	}
	blob := &contract.InlineData{MIMEType: fields["mimeType"], Data: data}
	if !contract.ValidOutputInlineData(blob) {
		// The MIME, nonempty bytes and size have already been checked. Only
		// the auxiliary suggestions MIME additionally requires UTF-8 JSON.
		return nil, malformedStreamAt(streamInvalidSuggestionsJSON)
	}
	return blob, nil
}

// ProtoJSON bytes accept either base64 alphabet, with or without padding.
// Whitespace, mixed alphabets and nonzero padding bits remain invalid. Charge
// the decoded bytes AND canonical output encoding before allocating the blob.
func decodeStreamBase64(encoded string, diagnostic streamDiagnostic) ([]byte, error) {
	if len(encoded) > 4<<20 {
		return nil, contract.Fail(502, "RESOURCE_EXHAUSTED", "Business output exceeds 4 MiB")
	}
	if len(encoded) == 0 || strings.ContainsAny(encoded, " \r\n\t") {
		return nil, malformedStreamAt(diagnostic)
	}
	encoding := base64.RawStdEncoding
	if strings.ContainsAny(encoded, "-_") {
		if strings.ContainsAny(encoded, "+/") {
			return nil, malformedStreamAt(diagnostic)
		}
		encoding = base64.RawURLEncoding
	}
	decodedSize := encoding.DecodedLen(len(encoded))
	if padding := strings.IndexByte(encoded, '='); padding >= 0 {
		count := len(encoded) - padding
		if len(encoded)%4 != 0 || count > 2 || (count == 2 && encoded[len(encoded)-1] != '=') {
			return nil, malformedStreamAt(diagnostic)
		}
		encoding = encoding.WithPadding(base64.StdPadding)
		decodedSize = encoding.DecodedLen(len(encoded)) - count
	} else if len(encoded)%4 == 1 {
		return nil, malformedStreamAt(diagnostic)
	}
	if decodedSize <= 0 {
		return nil, malformedStreamAt(diagnostic)
	}
	if decodedSize+base64.StdEncoding.EncodedLen(decodedSize) > 4<<20 {
		return nil, contract.Fail(502, "RESOURCE_EXHAUSTED", "Business output exceeds 4 MiB")
	}
	data, err := encoding.Strict().DecodeString(encoded)
	if err != nil || len(data) != decodedSize {
		return nil, malformedStreamAt(diagnostic)
	}
	return data, nil
}
