package geminienterprise

import (
	"bytes"
	"encoding/json"
)

type enterpriseMediaKind uint8

const (
	enterpriseMediaAbsent enterpriseMediaKind = iota
	enterpriseMediaSuggestions
	enterpriseMediaUnsupported
)

// Google emits follow-up suggestions as a UI-only protobuf blob. Ignore only
// this observed content type; arbitrary empty objects are not evidence of it.
func classifyEnterpriseMedia(file, inline json.RawMessage) enterpriseMediaKind {
	present := func(value json.RawMessage) bool {
		value = bytes.TrimSpace(value)
		return len(value) > 0 && !bytes.Equal(value, []byte("null"))
	}
	if present(file) {
		return enterpriseMediaUnsupported
	}
	if !present(inline) {
		return enterpriseMediaAbsent
	}
	var blob map[string]json.RawMessage
	if json.Unmarshal(inline, &blob) != nil || len(blob) != 2 {
		return enterpriseMediaUnsupported
	}
	var mime, data string
	if json.Unmarshal(blob["mimeType"], &mime) != nil || mime != "application/json+suggestions" {
		return enterpriseMediaUnsupported
	}
	dataRaw, ok := blob["data"]
	if !ok || bytes.Equal(bytes.TrimSpace(dataRaw), []byte("null")) || json.Unmarshal(dataRaw, &data) != nil {
		return enterpriseMediaUnsupported
	}
	return enterpriseMediaSuggestions
}
