// Package geminienterprise adapts text protocols to the streamAssist business schema.
package geminienterprise

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

type Error struct {
	Status        int
	Kind, Message string
	Scoped        bool
	clientMessage string
	param         string
	diagnostic    *UpstreamDiagnostic
	stop          bool
}

func (e *Error) Error() string {
	kind := "server_error"
	if e.Status < 500 {
		kind = "invalid_request_error"
	}
	switch e.Status {
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 429:
		kind = "rate_limit_error"
	}
	detail := map[string]any{"type": kind, "code": e.Kind, "message": e.Message}
	if e.param != "" {
		detail["param"] = e.param
	}
	if e.clientMessage != "" && e.diagnostic != nil {
		detail["upstream"] = e.diagnostic
	}
	data, _ := json.Marshal(map[string]any{"error": detail})
	return string(data)
}
func (e *Error) StatusCode() int       { return e.Status }
func (e *Error) IsRequestScoped() bool { return e.Scoped }
func (e *Error) IsRequestStop() bool   { return e.stop || e.Kind == "content_policy_violation" }
func (e *Error) PublicDiagnostic(status int) string {
	if e.Status != status {
		return ""
	}
	return e.clientMessage
}
func Invalid(message string) error {
	return &Error{Status: 400, Kind: "invalid_request", Message: message, Scoped: true}
}
func Failure(kind, message string, started bool) error {
	return &Error{Status: 502, Kind: kind, Message: message, Scoped: started, stop: !started}
}

type Input struct {
	Body                   []byte
	MediaKind              string
	JSON                   bool
	JSONObject             bool
	Ignored                []string
	ClientFunctionsIgnored bool
	IncludeThoughts        bool
}

type normalizer struct {
	err             error
	ignored         map[string]bool
	web, url, code  bool
	clientFunctions bool
}

func (n *normalizer) failField(path, reason string) {
	if n.err == nil {
		n.err = invalidField(path, reason, false)
	}
}
func (n *normalizer) object(v any, path string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		n.failField(path, "must be an object")
	}
	return m
}
func (n *normalizer) array(v any, path string, nonempty bool) []any {
	a, ok := v.([]any)
	if !ok {
		n.failField(path, "must be an array")
	} else if nonempty && len(a) == 0 {
		n.failField(path, "must contain at least one entry")
	}
	return a
}
func (n *normalizer) text(v any, path string) string {
	s, ok := v.(string)
	if !ok {
		n.failField(path, "must be text")
	}
	return s
}
func (n *normalizer) alias(m map[string]any, a, b, prefix string) any {
	x, xok := m[a]
	y, yok := m[b]
	if xok && yok {
		n.failField(prefix+a, "must not be supplied together with its alternate spelling")
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
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		m := n.object(p, itemPath)
		for key := range m {
			if key != "text" && !(chat && key == "type") {
				n.failField(itemPath, "supports text parts only; media and function calls are not supported")
			}
		}
		if chat && m["type"] != "text" {
			n.failField(itemPath+".type", "must be text; media and function calls are not supported")
		}
		text.WriteString(n.text(m["text"], itemPath+".text"))
	}
	return text.String()
}
func (n *normalizer) tools(v any, path string) {
	if v == nil {
		return
	}
	for i, item := range n.array(v, path, false) {
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		m := n.object(item, itemPath)
		if kind, exists := m["type"]; exists {
			if kind != "function" {
				n.failField(itemPath+".type", "contains an unsupported tool type")
			} else {
				n.object(m["function"], itemPath+".function")
				n.ignoreFunction(itemPath)
			}
		}
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			if value, exists := m[key]; exists {
				n.functionDeclarations(value, itemPath+"."+key)
			}
		}
		n.alias(m, "functionDeclarations", "function_declarations", itemPath+".")
		for key := range m {
			switch key {
			case "googleSearch", "google_search", "urlContext", "url_context", "codeExecution", "code_execution", "functionDeclarations", "function_declarations", "type", "function":
			default:
				n.failField(itemPath, "contains an unsupported tool definition")
			}
		}
		if _, exists := m["function"]; exists && m["type"] != "function" {
			n.failField(itemPath+".type", "must be function when function is present")
		}
		for key, value := range m {
			if value == nil {
				n.failField(itemPath+"."+key, "must be an object")
			}
		}
		for _, pair := range [][2]string{{"googleSearch", "google_search"}, {"urlContext", "url_context"}, {"codeExecution", "code_execution"}} {
			if spec := n.alias(m, pair[0], pair[1], itemPath+"."); spec != nil {
				n.unknown(n.object(spec, itemPath+"."+pair[0]), "", itemPath+"."+pair[0]+".")
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
	b, err := decodeRequestObject(body, false)
	if err != nil {
		return Input{}, err
	}
	if strings.TrimSpace(model) == "" {
		return Input{}, invalidField("model", "must be nonempty text", false)
	}
	texts := make([]string, 0)
	jsonMode, objectMode := false, false
	switch protocol {
	case "openai":
		for i, message := range n.array(b["messages"], "messages", true) {
			path := fmt.Sprintf("messages[%d]", i)
			m := n.object(message, path)
			role := n.text(m["role"], path+".role")
			switch role {
			case "system", "developer", "user", "assistant", "tool", "function":
			default:
				n.failField(path+".role", "must be system, developer, user, assistant, tool or function")
			}
			for _, key := range []string{"audio", "refusal"} {
				if v, ok := m[key]; ok && v != nil {
					n.failField(path+"."+key, "is not supported by text requests")
				}
			}
			text := ""
			if role != "assistant" || m["content"] != nil {
				text = n.parts(m["content"], path+".content", true)
			}
			text += n.historicalChatFunctions(m, role, path)
			if role == "tool" || role == "function" {
				text = n.historicalChatResult(m, text, path)
			}
			texts = append(texts, text)
			n.unknown(m, "role|content|tool_calls|function_call", path+".")
		}
		if v, exists := b["stream"]; exists {
			if _, ok := v.(bool); !ok {
				n.failField("stream", "must be a boolean")
			}
		}
		if v, exists := b["tools"]; exists {
			if v == nil {
				n.failField("tools", "must be an array")
			} else {
				n.tools(v, "tools")
			}
		}
		if v, exists := b["gemini_tools"]; exists {
			if v == nil {
				n.failField("gemini_tools", "must be an array")
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
				n.functionChoice(v, "tool_choice", false)
			}
			if v == "none" {
				n.web = false
				n.url = false
				n.code = false
			}
		}
		if v, exists := b["functions"]; exists {
			n.functionDeclarations(v, "functions")
		}
		if v, exists := b["function_call"]; exists {
			n.functionChoice(v, "function_call", true)
		}
		if v, exists := b["parallel_tool_calls"]; exists {
			n.ignoreFunction("parallel_tool_calls")
			if _, ok := v.(bool); !ok {
				n.failField("parallel_tool_calls", "must be a boolean")
			}
		}
		if _, exists := b["tool_choice"]; exists && n.clientFunctions {
			n.ignoreFunction("tool_choice")
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
		known := "model|messages|stream|stream_options|tools|gemini_tools|web_search_options|response_format|tool_choice|functions|function_call|parallel_tool_calls"
		if registry.EnterpriseThinkingSupport(model) != nil {
			known += "|reasoning_effort|extra_body"
			n.chatThinking(b)
		}
		n.unknown(b, known, " ")
	case "gemini":
		for i, content := range n.array(b["contents"], "contents", true) {
			path := fmt.Sprintf("contents[%d]", i)
			m := n.object(content, path)
			role := "user"
			if v, exists := m["role"]; exists {
				role = n.text(v, path+".role")
			}
			if role != "user" && role != "model" {
				n.failField(path+".role", "must be user or model")
			}
			if role == "model" {
				role = "assistant"
			}
			for key := range m {
				if key != "role" && key != "parts" {
					n.failField(path, "supports role and parts fields only")
				}
			}
			text := ""
			parts, isArray := m["parts"].([]any)
			if role != "assistant" || !isArray || len(parts) != 0 {
				text = n.geminiParts(m["parts"], path+".parts", role == "assistant", true)
			}
			texts = append(texts, text)
		}
		if v := n.alias(b, "systemInstruction", "system_instruction", ""); v != nil {
			m := n.object(v, "systemInstruction")
			texts = append([]string{n.geminiParts(m["parts"], "systemInstruction.parts", false, false)}, texts...)
			n.unknown(m, "role|parts", "systemInstruction.")
		}
		if v, exists := b["tools"]; exists {
			if v == nil {
				n.failField("tools", "must be an array")
			} else {
				n.tools(v, "tools")
			}
		}
		if v := n.alias(b, "toolConfig", "tool_config", ""); v != nil {
			m := n.object(v, "toolConfig")
			if call := n.alias(m, "functionCallingConfig", "function_calling_config", "toolConfig."); call != nil {
				c := n.object(call, "toolConfig.functionCallingConfig")
				if mode, exists := c["mode"]; exists && mode != "NONE" && mode != "AUTO" && mode != "ANY" && mode != "VALIDATED" {
					n.failField("toolConfig.functionCallingConfig.mode", "must be NONE, AUTO, ANY or VALIDATED")
				}
				n.ignoreFunction("toolConfig.functionCallingConfig")
			}
			n.ignored["toolConfig"] = true
		}
		if v := n.alias(b, "generationConfig", "generation_config", ""); v != nil {
			g := n.object(v, "generationConfig")
			if mime := n.alias(g, "responseMimeType", "response_mime_type", "generationConfig."); mime != nil {
				m := n.text(mime, "generationConfig.responseMimeType")
				if m == "application/json" {
					jsonMode = true
				} else if m != "text/plain" {
					n.ignored["generationConfig.responseMimeType"] = true
				}
			}
			known := "responseMimeType|response_mime_type"
			if registry.EnterpriseThinkingSupport(model) != nil {
				known += "|thinkingConfig|thinking_config"
				n.thinkingConfig(g)
			}
			n.unknown(g, known, "generationConfig.")
		}
		n.unknown(b, "contents|systemInstruction|system_instruction|tools|toolConfig|tool_config|generationConfig|generation_config|model", "")
	default:
		return Input{}, Invalid("unsupported source protocol")
	}
	// Forward only content from this request, without a conversation wrapper or
	// proxy-authored instructions. StreamAssist still requires a plain-text query.
	hasText := false
	for _, text := range texts {
		hasText = hasText || strings.TrimSpace(text) != ""
	}
	if !hasText {
		path := "contents"
		if protocol == "openai" {
			path = "messages"
		}
		n.failField(path, "must contain nonempty text or system instructions")
	}
	if n.err != nil {
		return Input{}, n.err
	}
	tools := map[string]any{}
	if n.web {
		tools["webGroundingSpec"] = map[string]any{}
	}
	payload, _ := json.Marshal(map[string]any{"query": map[string]string{"text": strings.Join(texts, "\n")}, "generationSpec": map[string]string{"modelId": model}, "toolsSpec": tools})
	ignored := make([]string, 0, len(n.ignored))
	for field := range n.ignored {
		ignored = append(ignored, strings.TrimSpace(field))
	}
	sort.Strings(ignored)
	return Input{Body: payload, JSON: jsonMode, JSONObject: objectMode, Ignored: ignored, ClientFunctionsIgnored: n.clientFunctions}, nil
}
