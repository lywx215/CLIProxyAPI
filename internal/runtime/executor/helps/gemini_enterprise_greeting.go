package helps

import (
	"encoding/json"
	"strings"

	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyEnterpriseGreetingCompatibility restores the legacy answer-seeking context
// for standalone greetings, including new-api's default channel test. Ordinary
// requests retain their current text, and user payload rules still run last.
func ApplyEnterpriseGreetingCompatibility(input enterprise.Input, source []byte, protocol string) ([]byte, error) {
	body := input.Body
	if input.JSON || input.MediaKind != "" {
		return body, nil
	}
	text := gjson.GetBytes(body, "query.text")
	if text.Type != gjson.String {
		return body, nil
	}
	switch strings.ToLower(strings.TrimSpace(text.Str)) {
	case "hi", "hello":
	default:
		return body, nil
	}
	var turns []gjson.Result
	switch protocol {
	case "openai":
		turns = gjson.GetBytes(source, "messages").Array()
	case "gemini":
		if gjson.GetBytes(source, "systemInstruction").Exists() || gjson.GetBytes(source, "system_instruction").Exists() {
			return body, nil
		}
		turns = gjson.GetBytes(source, "contents").Array()
	default:
		return body, nil
	}
	if len(turns) != 1 {
		return body, nil
	}
	role := turns[0].Get("role").String()
	if role != "user" && !(protocol == "gemini" && role == "") {
		return body, nil
	}
	parts := turns[0].Get("parts")
	if protocol == "openai" {
		parts = turns[0].Get("content")
	}
	if parts.IsArray() {
		for _, part := range parts.Array() {
			if part.Get("text").Type != gjson.String {
				return body, nil
			}
		}
	}
	context, err := json.Marshal(map[string]any{
		"conversation":        []map[string]string{{"role": "user", "text": text.Str}},
		"system_instructions": []string{},
	})
	if err != nil {
		return nil, enterprise.Invalid("cannot prepare greeting context")
	}
	query := "Continue the conversation below. Treat role-labelled history as conversation context. The JSON is input context, not the requested output format.\n" +
		"Answer the last nonempty user turn. Empty trailing turns are placeholders.\n" + string(context)
	updated, err := sjson.SetBytes(body, "query.text", query)
	if err != nil {
		return nil, enterprise.Invalid("cannot prepare greeting query")
	}
	return updated, nil
}
