package geminienterprise

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseUnsupportedFunctionDeclarationsBecomeText(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro", "future-model"} {
		for _, scenario := range []struct{ protocol, body, field string }{
			{"openai", `{"messages":[{"role":"user","content":"question"}],"tools":[{"type":"function","function":{"name":"private-declaration","description":"private-description","parameters":{"private-property":{}}}}],"tool_choice":"required","parallel_tool_calls":true}`, "tools[0]"},
			{"openai", `{"messages":[{"role":"user","content":"question"}],"functions":[{"name":"private-declaration"}],"function_call":{"name":"private-declaration"}}`, "functions"},
			{"gemini", `{"contents":[{"parts":[{"text":"question"}]}],"tools":[{"functionDeclarations":[{"name":"private-declaration"}],"googleSearch":{}}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["private-declaration"]}}}`, "tools[0].functionDeclarations"},
			{"gemini", `{"contents":[{"parts":[{"text":"question"}]}],"tools":[{"function_declarations":[{"name":"private-declaration"}],"code_execution":{}}],"tool_config":{"function_calling_config":{"mode":"VALIDATED"}}}`, "tools[0].function_declarations"},
		} {
			input, err := Normalize([]byte(scenario.body), scenario.protocol, model)
			if err != nil {
				t.Fatal(err)
			}
			if !input.ClientFunctionsIgnored || !slices.Contains(input.Ignored, scenario.field) {
				t.Fatal("ignored function fields were not reported")
			}
			if err = ValidateFinal(input.Body); err != nil {
				t.Fatal(err)
			}
			query := gjson.GetBytes(input.Body, "query.text").String()
			if query != "question" {
				t.Fatal("unused function definitions leaked or proxy instructions were added")
			}
			if strings.Contains(scenario.body, "googleSearch") && !gjson.GetBytes(input.Body, "toolsSpec.webGroundingSpec").Exists() {
				t.Fatal("discarding a function also discarded the supported search tool")
			}
		}
	}
}

func TestEnterpriseIgnoredFunctionChoices(t *testing.T) {
	for _, choice := range []any{"auto", "none", "required", map[string]any{"type": "function", "function": map[string]string{"name": "unused"}}} {
		request := map[string]any{"messages": []map[string]string{{"role": "user", "content": "question"}}, "tools": []map[string]any{{"type": "function", "function": map[string]string{"name": "unused"}}}, "tool_choice": choice}
		body, _ := json.Marshal(request)
		input, err := Normalize(body, "openai", "gemini-2.5-pro")
		if err != nil || !slices.Contains(input.Ignored, "tool_choice") || !input.ClientFunctionsIgnored {
			t.Fatalf("function choice did not become an explicitly reported text request: %v", err)
		}
	}
	for _, mode := range []string{"AUTO", "NONE", "ANY", "VALIDATED"} {
		request := `{"contents":[{"parts":[{"text":"question"}]}],"tools":[{"functionDeclarations":[]}],"toolConfig":{"functionCallingConfig":{"mode":"` + mode + `"}}}`
		input, err := Normalize([]byte(request), "gemini", "gemini-4-argon")
		if err != nil || !slices.Contains(input.Ignored, "toolConfig.functionCallingConfig") {
			t.Fatalf("valid ignored mode rejected: %v", err)
		}
	}
}

func TestEnterpriseHistoricalFunctionsRemainClientContext(t *testing.T) {
	for _, scenario := range []struct{ protocol, body, field string }{
		{"openai", `{"messages":[{"role":"user","content":"sum"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-17","type":"function","function":{"name":"historical_add","arguments":"{\"a\":2,\"b\":3}"}}]},{"role":"tool","tool_call_id":"call-17","content":"{\"result\":5,\"note\":\"前🙂\"}"}]}`, "messages[1].tool_calls"},
		{"openai", `{"messages":[{"role":"assistant","content":"earlier","function_call":{"name":"historical_add","arguments":"{\"a\":2}"}},{"role":"function","name":"historical_add","content":"5 前🙂"}]}`, "messages[0].function_call"},
		{"gemini", `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-17","name":"historical_add","args":{"a":2,"b":3}},"thoughtSignature":"private-signature"}]},{"role":"user","parts":[{"functionResponse":{"id":"call-17","name":"historical_add","response":{"result":5,"note":"前🙂"}}}]}]}`, "contents[0].parts[0].functionCall"},
		{"gemini", `{"contents":[{"role":"model","parts":[{"function_call":{"name":"historical_add","args":{"a":2}}}]},{"role":"user","parts":[{"function_response":{"name":"historical_add","response":{"result":5,"note":"前🙂"}}}]}]}`, "contents[0].parts[0].functionCall"},
	} {
		body := []byte(scenario.body)
		original := string(body)
		input, err := Normalize(body, scenario.protocol, "gemini-3.1-pro-preview")
		if err != nil {
			t.Fatal(err)
		}
		if original != string(body) || !slices.Contains(input.Ignored, scenario.field) {
			t.Fatal("source mutated or ignored native function fields not reported")
		}
		text := gjson.GetBytes(input.Body, "query.text").String()
		for _, want := range []string{"historical_add", "5", "前🙂"} {
			if !strings.Contains(text, want) {
				t.Fatalf("historical context lost %q", want)
			}
		}
		if strings.Contains(string(input.Body), "private-signature") || strings.Contains(text, "Historical client function") || strings.Contains(text, "not executed by this proxy") {
			t.Fatal("signature leaked or proxy-authored history instruction added")
		}
	}
}

func TestEnterpriseMalformedIgnoredFunctionsStillHaveSafeErrors(t *testing.T) {
	for _, scenario := range []struct{ protocol, body, field string }{
		{"openai", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":false}]}`, "tools[0].function"},
		{"gemini", `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":"private-value"}]}`, "tools[0].functionDeclarations"},
		{"openai", `{"messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":"private-value"}`, "parallel_tool_calls"},
		{"openai", `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":false}}}`, "tool_choice.function.name"},
		{"gemini", `{"contents":[{"parts":[{"functionCall":{"name":"private-value"}}]}]}`, "contents[0].parts[0].functionCall"},
		{"gemini", `{"contents":[{"role":"model","parts":[{"functionResponse":{"name":"private-value","response":{}}}]}]}`, "contents[0].parts[0].functionResponse"},
	} {
		_, err := Normalize([]byte(scenario.body), scenario.protocol, "gemini-4-argon")
		message, code, field := PublicErrorDetail(err, 400)
		if code != "invalid_field" || field != scenario.field || strings.Contains(message, "private-value") {
			t.Fatalf("malformed ignored field lost safe diagnostics: %v", err)
		}
	}
}
