// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"encoding/json"
	"errors"
)

// WidgetGroundingType reads the exact enum advertised for this widget.
func WidgetGroundingType(raw []byte) (string, error) {
	if err := validateStreamFrame(raw); err != nil {
		return "", err
	}
	malformed := &protocolError{errors.New("invalid widget configuration")}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return "", malformed
	}
	if rpcError := object["error"]; len(rpcError) != 0 && !bytes.Equal(bytes.TrimSpace(rpcError), []byte("null")) {
		if err, ok := rpcEnvelopeFailure(raw); ok {
			return "", err
		}
		return "", malformed
	}
	for _, key := range []string{"anonymousWidgetConfig", "assistantSettings"} {
		value := object[key]
		if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return "", nil
		}
		object = nil
		if json.Unmarshal(value, &object) != nil {
			return "", malformed
		}
	}
	value := object["webGroundingType"]
	if len(value) == 0 {
		return "", nil
	}
	var mode string
	if json.Unmarshal(value, &mode) != nil {
		return "", malformed
	}
	return mode, nil
}
