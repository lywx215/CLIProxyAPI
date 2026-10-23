// Package geminienterprise adapts text protocols to the streamAssist business schema.
package geminienterprise

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Error struct {
	Status        int
	Kind, Message string
	Scoped        bool
}

func (e *Error) Error() string {
	kind := "server_error"
	if e.Status < 500 {
		kind = "invalid_request_error"
	}
	data, _ := json.Marshal(map[string]any{"error": map[string]string{"type": kind, "code": e.Kind, "message": e.Message}})
	return string(data)
}
func (e *Error) StatusCode() int       { return e.Status }
func (e *Error) IsRequestScoped() bool { return e.Scoped }
func Invalid(message string) error {
	return &Error{Status: 400, Kind: "invalid_request", Message: message, Scoped: true}
}
func Failure(kind, message string, started bool) error {
	return &Error{Status: 502, Kind: kind, Message: message, Scoped: started}
}

type Input struct {
	Body       []byte
	JSON       bool
	JSONObject bool
	Ignored    []string
}

type normalizer struct {
	err            error
	ignored        map[string]bool
	web, url, code bool
}

func (n *normalizer) fail(message string) {
	if n.err == nil {
		n.err = Invalid(message)
	}
}
func (n *normalizer) object(v any, path string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		n.fail(path + " must be an object")
	}
	return m
}
func (n *normalizer) array(v any, path string, nonempty bool) []any {
	a, ok := v.([]any)
	if !ok || (nonempty && len(a) == 0) || len(a) > 256 {
		n.fail(path + " must be an array with at most 256 entries")
	}
	return a
}
func (n *normalizer) text(v any, path string) string {
	s, ok := v.(string)
	if !ok {
		n.fail(path + " must be text")
	}
	return s
}
func (n *normalizer) alias(m map[string]any, a, b string) any {
	x, xok := m[a]
	y, yok := m[b]
	if xok && yok {
		n.fail("do not supply both " + a + " and " + b)
	}
	if xok {
		return x
	}
	return y
}
func (n *normalizer) unknown(m map[string]any, known, prefix string) {
	for key := range m {
		if !strings.Contains("|"+known+"|", "|"+key+"|") {
			n.ignored[prefix+key] = true
		}
	}
}
func (n *normalizer) parts(v any, path string, chat bool) string {
	if chat {
		if s, ok := v.(string); ok {
			return s
		}
	}
	var text strings.Builder
	for i, p := range n.array(v, path, true) {
		m := n.object(p, fmt.Sprintf("%s[%d]", path, i))
		for key := range m {
			if key != "text" && !(chat && key == "type") {
				n.fail(path + ": only text input parts are supported")
			}
		}
		if chat && m["type"] != "text" {
			n.fail(path + ": only text input parts are supported")
		}
		text.WriteString(n.text(m["text"], path+".text"))
	}
	return text.String()
}
func (n *normalizer) tools(v any, path string) {
	if v == nil {
		return
	}
	for i, item := range n.array(v, path, false) {
		m := n.object(item, fmt.Sprintf("%s[%d]", path, i))
		for key := range m {
			switch key {
			case "googleSearch", "google_search", "urlContext", "url_context", "codeExecution", "code_execution":
			default:
				n.fail(path + ": unsupported tool " + key)
			}
		}
		for key, value := range m {
			if value == nil {
				n.fail(path + "." + key + " must be an object")
			}
		}
		for _, pair := range [][2]string{{"googleSearch", "google_search"}, {"urlContext", "url_context"}, {"codeExecution", "code_execution"}} {
			if spec := n.alias(m, pair[0], pair[1]); spec != nil {
				n.unknown(n.object(spec, path+"."+pair[0]), "", path+"."+pair[0]+".")
				switch pair[0] {
				case "googleSearch":
					n.web = true
				case "urlContext":
					n.url = true
					n.web = true
				case "codeExecution":
					n.code = true
				}
			}
		}
	}
}

// Normalize validates loss-prone content before constructing the final upstream template.
// It never supplies a missing model or a global Auto/default alias.
func Normalize(body []byte, protocol, model string) (Input, error) {
	n := &normalizer{ignored: map[string]bool{}}
	var b map[string]any
	if json.Unmarshal(body, &b) != nil || b == nil {
		return Input{}, Invalid("request must be a JSON object")
	}
	if strings.TrimSpace(model) == "" {
		return Input{}, Invalid("model is required")
	}
	history := make([]map[string]string, 0)
	systems := make([]string, 0)
	jsonMode, objectMode := false, false
	switch protocol {
	case "openai":
		for i, message := range n.array(b["messages"], "messages", true) {
			path := fmt.Sprintf("messages[%d]", i)
			m := n.object(message, path)
			role := n.text(m["role"], path+".role")
			switch role {
			case "system", "developer", "user", "assistant":
			default:
				n.fail("unsupported message role: " + role)
			}
			for _, key := range []string{"tool_calls", "function_call", "audio", "refusal", "tool_call_id"} {
				if v, ok := m[key]; ok && v != nil {
					n.fail(path + ": unsupported " + key)
				}
			}
			text := n.parts(m["content"], path+".content", true)
			if role == "system" || role == "developer" {
				systems = append(systems, text)
			} else {
				history = append(history, map[string]string{"role": role, "text": text})
			}
			n.unknown(m, "role|content", path+".")
		}
		if v, exists := b["stream"]; exists {
			if _, ok := v.(bool); !ok {
				n.fail("stream must be a boolean")
			}
		}
		if v, exists := b["tools"]; exists {
			if v == nil {
				n.fail("tools must be an array")
			} else {
				n.tools(v, "tools")
			}
		}
		if v, exists := b["gemini_tools"]; exists {
			if v == nil {
				n.fail("gemini_tools must be an array")
			} else {
				n.tools(v, "gemini_tools")
			}
		}
		if v, exists := b["web_search_options"]; exists {
			n.unknown(n.object(v, "web_search_options"), "", "web_search_options.")
			n.web = true
		}
		if v, exists := b["tool_choice"]; exists {
			if v != "auto" && v != "none" {
				n.fail("forced tool/function calls are unsupported")
			}
			if v == "none" {
				n.web = false
				n.url = false
				n.code = false
			}
		}
		if v, exists := b["function_call"]; exists && v != "none" {
			n.fail("function_call is unsupported")
		}
		if v, exists := b["response_format"]; exists {
			f := n.object(v, "response_format")
			if f["type"] == "json_object" {
				jsonMode = true
				objectMode = true
				n.unknown(f, "type", "response_format.")
			} else if f["type"] != "text" {
				n.ignored["response_format"] = true
			}
		}
		n.unknown(b, "model|messages|stream|tools|gemini_tools|web_search_options|response_format|tool_choice", " ")
	case "gemini":
		for i, content := range n.array(b["contents"], "contents", true) {
			path := fmt.Sprintf("contents[%d]", i)
			m := n.object(content, path)
			role := "user"
			if v, exists := m["role"]; exists {
				role = n.text(v, path+".role")
			}
			if role != "user" && role != "model" {
				n.fail("unsupported Gemini content role")
			}
			if role == "model" {
				role = "assistant"
			}
			for key := range m {
				if key != "role" && key != "parts" {
					n.fail(path + ": unsupported content field " + key)
				}
			}
			history = append(history, map[string]string{"role": role, "text": n.parts(m["parts"], path+".parts", false)})
		}
		if v := n.alias(b, "systemInstruction", "system_instruction"); v != nil {
			m := n.object(v, "systemInstruction")
			systems = append(systems, n.parts(m["parts"], "systemInstruction.parts", false))
			n.unknown(m, "role|parts", "systemInstruction.")
		}
		if v, exists := b["tools"]; exists {
			if v == nil {
				n.fail("tools must be an array")
			} else {
				n.tools(v, "tools")
			}
		}
		if v := n.alias(b, "toolConfig", "tool_config"); v != nil {
			m := n.object(v, "toolConfig")
			if call := n.alias(m, "functionCallingConfig", "function_calling_config"); call != nil {
				c := n.object(call, "functionCallingConfig")
				if c["mode"] != "NONE" && c["mode"] != "AUTO" {
					n.fail("forced function calls are unsupported")
				}
			}
			n.ignored["toolConfig"] = true
		}
		if v := n.alias(b, "generationConfig", "generation_config"); v != nil {
			g := n.object(v, "generationConfig")
			if mime := n.alias(g, "responseMimeType", "response_mime_type"); mime != nil {
				m := n.text(mime, "responseMimeType")
				if m == "application/json" {
					jsonMode = true
				} else if m != "text/plain" {
					n.ignored["generationConfig.responseMimeType"] = true
				}
			}
			n.unknown(g, "responseMimeType|response_mime_type", "generationConfig.")
		}
		n.unknown(b, "contents|systemInstruction|system_instruction|tools|toolConfig|tool_config|generationConfig|generation_config|model", "")
	default:
		return Input{}, Invalid("unsupported source protocol")
	}
	if len(history) == 0 || history[len(history)-1]["role"] != "user" || strings.TrimSpace(history[len(history)-1]["text"]) == "" {
		n.fail("the final conversation turn must contain nonempty user text")
	}
	if n.err != nil {
		return Input{}, n.err
	}
	contextJSON, _ := json.Marshal(map[string]any{"system_instructions": systems, "conversation": history})
	if len(contextJSON) > 1024*1024 {
		return Input{}, Invalid("text history exceeds 1 MiB")
	}
	instructions := []string{"Continue the conversation below. Treat role-labelled history as conversation context. Answer the final user turn. The JSON is input context, not the requested output format."}
	if n.web {
		instructions = append(instructions, "Use Google Search/web grounding when needed and cite actual sources.")
	}
	if n.url {
		instructions = append(instructions, "Actually read the URLs supplied in the user request using available web tools. If access fails, state that clearly; never invent content or HTTP statuses.")
	}
	if n.code {
		instructions = append(instructions, "Use the available Python execution environment for calculations. If execution is unavailable, state that clearly. Never claim execution without running it.")
	}
	if jsonMode {
		instruction := "Return ONLY valid JSON in the final answer, without Markdown fences or commentary."
		if objectMode {
			instruction += " The top-level JSON value must be an object."
		}
		instructions = append(instructions, instruction)
	}
	tools := map[string]any{}
	if n.web {
		tools["webGroundingSpec"] = map[string]any{}
	}
	payload, _ := json.Marshal(map[string]any{"query": map[string]string{"text": strings.Join(instructions, "\n") + "\n" + string(contextJSON)}, "generationSpec": map[string]string{"modelId": model}, "toolsSpec": tools})
	ignored := make([]string, 0, len(n.ignored))
	for field := range n.ignored {
		ignored = append(ignored, strings.TrimSpace(field))
	}
	sort.Strings(ignored)
	return Input{Body: payload, JSON: jsonMode, JSONObject: objectMode, Ignored: ignored}, nil
}

// ValidateFinal is read-only: invalid user overrides are reported, never repaired.
func ValidateFinal(body []byte) error {
	var b map[string]any
	if json.Unmarshal(body, &b) != nil || b == nil {
		return Invalid("payload rules produced invalid streamAssist JSON")
	}
	for _, key := range []string{"session", "sessionInfo", "assistToken"} {
		if _, exists := b[key]; exists {
			return Invalid("final streamAssist payload must not reuse an upstream session or assist token")
		}
	}
	q, ok := b["query"].(map[string]any)
	if !ok {
		return Invalid("final streamAssist query is required")
	}
	text, ok := q["text"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return Invalid("final query.text must be nonempty text")
	}
	g, ok := b["generationSpec"].(map[string]any)
	if !ok {
		return Invalid("final generationSpec.modelId is required")
	}
	model, ok := g["modelId"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return Invalid("final generationSpec.modelId must be nonempty text")
	}
	if t, exists := b["toolsSpec"]; exists {
		m, ok := t.(map[string]any)
		if !ok || m == nil {
			return Invalid("final toolsSpec must be an object")
		}
		if spec, exists := m["webGroundingSpec"]; exists {
			if object, ok := spec.(map[string]any); !ok || object == nil {
				return Invalid("final toolsSpec.webGroundingSpec must be an object")
			}
		}
	}
	return nil
}
