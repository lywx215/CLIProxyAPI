package geminienterprise

import (
	"fmt"
	"strings"
)

// Client functions are unavailable on this upstream path. Keep declarations out
// of the request, report ignored field names, and preserve past calls as context.
func (n *normalizer) ignoreFunction(path string) {
	n.clientFunctions = true
	n.ignored[path] = true
}

func (n *normalizer) functionDeclarations(value any, path string) {
	n.ignoreFunction(path)
	for i, item := range n.array(value, path, false) {
		n.object(item, fmt.Sprintf("%s[%d]", path, i))
	}
}

func (n *normalizer) functionChoice(value any, path string, legacy bool) {
	n.ignoreFunction(path)
	if choice, ok := value.(string); ok {
		if choice != "auto" && choice != "none" && (choice != "required" || legacy) {
			n.failField(path, "must be auto, none or a valid function selection")
		}
		return
	}
	m := n.object(value, path)
	if !legacy {
		if m["type"] != "function" {
			n.failField(path+".type", "must be function")
		}
		m = n.object(m["function"], path+".function")
		path += ".function"
	}
	if strings.TrimSpace(n.text(m["name"], path+".name")) == "" {
		n.failField(path+".name", "must be nonempty text")
	}
}

func (n *normalizer) historicalGeminiFunction(value any, path string, call bool) string {
	n.ignoreFunction(path)
	m := n.object(value, path)
	// History is serialized as client context, not submitted as a native function.
	// Preserve missing or empty names for the upstream to interpret without repair.
	field := "response"
	if call {
		field = "args"
	}
	if args, exists := m[field]; exists || !call {
		n.object(args, path+"."+field)
	}
	return "\n" + string(marshal(m)) + "\n"
}

func (n *normalizer) historicalChatFunctions(m map[string]any, role, path string) string {
	var text strings.Builder
	if calls, exists := m["tool_calls"]; exists && calls != nil {
		n.ignoreFunction(path + ".tool_calls")
		if role != "assistant" {
			n.failField(path+".tool_calls", "is accepted only in assistant history")
		}
		for i, value := range n.array(calls, path+".tool_calls", false) {
			prefix := fmt.Sprintf("%s.tool_calls[%d]", path, i)
			call := n.object(value, prefix)
			if call["type"] != "function" {
				n.failField(prefix+".type", "must be function")
			}
			function := n.object(call["function"], prefix+".function")
			n.text(function["name"], prefix+".function.name")
			n.text(function["arguments"], prefix+".function.arguments")
			if id, exists := call["id"]; exists {
				n.text(id, prefix+".id")
			}
			text.WriteString("\n" + string(marshal(call)) + "\n")
		}
	}
	if call, exists := m["function_call"]; exists && call != nil {
		n.ignoreFunction(path + ".function_call")
		if role != "assistant" {
			n.failField(path+".function_call", "is accepted only in assistant history")
		}
		function := n.object(call, path+".function_call")
		n.text(function["name"], path+".function_call.name")
		n.text(function["arguments"], path+".function_call.arguments")
		text.WriteString("\n" + string(marshal(function)) + "\n")
	}
	return text.String()
}

func (n *normalizer) historicalChatResult(m map[string]any, content, path string) string {
	n.ignoreFunction(path + ".role")
	result := map[string]string{"role": n.text(m["role"], path+".role"), "content": content}
	for _, key := range []string{"tool_call_id", "name"} {
		if value, exists := m[key]; exists {
			n.ignoreFunction(path + "." + key)
			result[key] = n.text(value, path+"."+key)
		}
	}
	return "\n" + string(marshal(result)) + "\n"
}
