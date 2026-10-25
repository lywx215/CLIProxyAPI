package geminienterprise

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ApplicationBinding excludes OAuth credentials and uses canonical resource defaults.
func ApplicationBinding(c config.GeminiEnterpriseOAuth) string {
	data, _ := json.Marshal(Resources(c))
	return string(data)
}

// IsModelNotEnabledResponse accepts only an unambiguous, structured Google rejection.
// Messages and arbitrary metadata never determine retry or cooldown policy.
func IsModelNotEnabledResponse(raw []byte, status int) bool {
	return modelNotEnabledResponse(raw, status, 0)
}

func modelNotEnabledResponse(raw []byte, status, depth int) bool {
	if depth > 4 {
		return false
	}
	if status != http.StatusBadRequest && status != http.StatusOK && status != 0 || len(raw) > 1024*1024 {
		return false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil || len(items) != 1 {
			return false
		}
		return modelNotEnabledResponse(items[0], status, depth+1)
	}
	var body struct {
		Error   json.RawMessage `json:"error"`
		Code    int             `json:"code"`
		Status  string          `json:"status"`
		Details []struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
			Domain string `json:"domain"`
		} `json:"details"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	if len(body.Error) > 0 && string(body.Error) != "null" {
		return modelNotEnabledResponse(body.Error, status, depth+1)
	}
	if (body.Code != 0 && body.Code != 3 && body.Code != 400) || (body.Status != "" && body.Status != "INVALID_ARGUMENT") || body.Code == 0 && body.Status == "" {
		return false
	}
	found := false
	for _, detail := range body.Details {
		if detail.Type != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		if detail.Domain != "discoveryengine.googleapis.com" || detail.Reason != "MODEL_NOT_ENABLED" {
			return false
		}
		found = true
	}
	return found
}
