package geminienterprise

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

func (n *normalizer) contentKeys(object map[string]any, allowed, prefix string) {
	allowedKeys := strings.Split(allowed, "|")
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !slices.Contains(allowedKeys, key) {
			n.failField(prefix+key, "is not supported in text history")
		}
	}
}

func (n *normalizer) optionalText(object map[string]any, key, path string) string {
	if value, exists := object[key]; exists && value != nil {
		return n.text(value, path+key)
	}
	return ""
}

// Gemini clients replay response metadata and built-in code events with model history.
// Signatures are opaque native-Gemini state, never streamAssist fields or prompt text.
func (n *normalizer) geminiParts(value any, path string, modelHistory, conversation bool) string {
	var text strings.Builder
	for i, item := range n.array(value, path, true) {
		partPath := fmt.Sprintf("%s[%d]", path, i)
		prefix := partPath + "."
		part := n.object(item, partPath)
		metadata := false
		for _, key := range []string{"thoughtSignature", "thought_signature"} {
			if signature, exists := part[key]; exists {
				metadata = true
				n.ignored[prefix+key] = true
				if signature != nil {
					n.text(signature, prefix+key)
				}
			}
		}
		n.alias(part, "thoughtSignature", "thought_signature", prefix)
		thought := false
		if flag, exists := part["thought"]; exists {
			metadata = true
			n.ignored[prefix+"thought"] = true
			if flag != nil {
				var ok bool
				thought, ok = flag.(bool)
				if !ok {
					n.failField(prefix+"thought", "must be a boolean")
				}
			}
			if thought && !modelHistory {
				n.failField(prefix+"thought", "is accepted only in model history")
			}
		}
		for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data", "functionCall", "function_call", "functionResponse", "function_response", "mediaResolution", "media_resolution", "videoMetadata", "video_metadata"} {
			if v, exists := part[key]; exists && v == nil {
				n.ignored[prefix+key] = true
				delete(part, key)
			}
		}
		n.contentKeys(part, "text|thought|thoughtSignature|thought_signature|executableCode|executable_code|codeExecutionResult|code_execution_result|functionCall|function_call|functionResponse|function_response", prefix)
		code := n.alias(part, "executableCode", "executable_code", prefix)
		result := n.alias(part, "codeExecutionResult", "code_execution_result", prefix)
		call := n.alias(part, "functionCall", "function_call", prefix)
		functionResult := n.alias(part, "functionResponse", "function_response", prefix)
		_, hasText := part["text"]
		kinds := 0
		for _, present := range []bool{hasText && part["text"] != nil, code != nil, result != nil, call != nil, functionResult != nil} {
			if present {
				kinds++
			}
		}
		if kinds > 1 {
			n.failField(partPath, "must contain exactly one content kind")
		}
		if (code != nil || result != nil) && (!modelHistory || thought) {
			n.failField(partPath, "built-in code events are accepted only as visible model history")
		}
		switch {
		case call != nil:
			if !conversation || !modelHistory || thought {
				n.failField(prefix+"functionCall", "is accepted only as visible model history")
			}
			text.WriteString(n.historicalGeminiFunction(call, prefix+"functionCall", true))
		case functionResult != nil:
			if !conversation || modelHistory || thought {
				n.failField(prefix+"functionResponse", "is accepted only as visible user history")
			}
			text.WriteString(n.historicalGeminiFunction(functionResult, prefix+"functionResponse", false))
		case code != nil:
			object := n.object(code, prefix+"executableCode")
			n.contentKeys(object, "language|code", prefix+"executableCode.")
			language := n.optionalText(object, "language", prefix+"executableCode.")
			codeText := n.optionalText(object, "code", prefix+"executableCode.")
			text.WriteString("\n[Previous model code (" + language + ")]\n" + codeText + "\n")
		case result != nil:
			object := n.object(result, prefix+"codeExecutionResult")
			n.contentKeys(object, "outcome|output", prefix+"codeExecutionResult.")
			outcome := n.optionalText(object, "outcome", prefix+"codeExecutionResult.")
			output := n.optionalText(object, "output", prefix+"codeExecutionResult.")
			text.WriteString("\n[Previous model code execution result (" + outcome + ")]\n" + output + "\n")
		case hasText:
			body := n.text(part["text"], prefix+"text")
			if !thought {
				text.WriteString(body)
			}
		case !metadata || !modelHistory:
			n.failField(prefix+"text", "must contain text")
		}
	}
	return text.String()
}
