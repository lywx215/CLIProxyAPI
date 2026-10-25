package geminienterprise

import (
	"encoding/base64"
	"encoding/json"
)

// MediaResponse exposes only verified bytes and a canonical MIME type.
func (o *Output) MediaResponse(model, protocol, kind, mime string, data []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(data)
	switch protocol {
	case "openai-image":
		return marshal(map[string]any{"created": o.created, "data": []map[string]any{{"b64_json": b64}}})
	case "openai-video":
		return marshal(map[string]string{"mime_type": mime, "data": b64})
	case "gemini":
		o.Parts = append(o.Parts, map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": b64}})
		return o.Gemini(true, nil)
	default:
		var result map[string]any
		_ = json.Unmarshal(o.Chat(model), &result)
		message := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		message[kind+"s"] = []map[string]any{{"type": kind + "_url", kind + "_url": map[string]string{"url": "data:" + mime + ";base64," + b64}}}
		return marshal(result)
	}
}

// MediaChatChunks preserves chat media fields in the streaming delta.
func MediaChatChunks(payload []byte) (media, terminal []byte) {
	var result map[string]any
	_ = json.Unmarshal(payload, &result)
	choice := result["choices"].([]any)[0].(map[string]any)
	choice["delta"] = choice["message"]
	delete(choice, "message")
	choice["finish_reason"] = nil
	result["object"] = "chat.completion.chunk"
	delete(result, "usage")
	media = marshal(result)
	choice["delta"] = map[string]any{}
	choice["finish_reason"] = "stop"
	return media, marshal(result)
}

func MediaChatUsageChunk(payload []byte) []byte {
	var result map[string]any
	_ = json.Unmarshal(payload, &result)
	result["choices"] = []any{}
	result["object"] = "chat.completion.chunk"
	return marshal(result)
}
