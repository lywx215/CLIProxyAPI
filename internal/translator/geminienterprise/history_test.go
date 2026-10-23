package geminienterprise

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseLongTextHistory(t *testing.T) {
	for _, protocol := range []string{"gemini", "openai"} {
		contents := make([]map[string]any, 0, 513)
		messages := make([]map[string]any, 0, 513)
		for i := range 513 {
			role, chatRole := "user", "user"
			if i%2 == 1 {
				role, chatRole = "model", "assistant"
			}
			text := fmt.Sprintf("turn-%d", i)
			contents = append(contents, map[string]any{"role": role, "parts": []map[string]any{{"text": text, "thoughtSignature": "private-signature"}}})
			messages = append(messages, map[string]any{"role": chatRole, "content": text})
		}
		body, _ := json.Marshal(map[string]any{"contents": contents})
		if protocol == "openai" {
			body, _ = json.Marshal(map[string]any{"messages": messages})
		}
		input, err := Normalize(body, protocol, "gemini-3.1-pro-preview")
		if err != nil {
			t.Fatalf("%s long history rejected: %v", protocol, err)
		}
		query := gjson.GetBytes(input.Body, "query.text").String()
		for _, want := range []string{"turn-0", "turn-257", "turn-512"} {
			if !strings.Contains(query, want) {
				t.Fatalf("%s history truncated %q", protocol, want)
			}
		}
		if strings.Contains(query, "private-signature") {
			t.Fatal("signature leaked into prompt")
		}
	}
	for _, scenario := range []struct{ body, field, reason string }{
		{`{"contents":{}}`, "contents", "must be an array"},
		{`{"contents":[]}`, "contents", "must contain at least one entry"},
		{`{"contents":[{"parts":{}}]}`, "contents[0].parts", "must be an array"},
		{`{"contents":[{"parts":[]}]}`, "contents[0].parts", "must contain at least one entry"},
		{`{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 1024*1024) + `"}]}]}`, "body", "exceeds 1 MiB"},
	} {
		_, err := Normalize([]byte(scenario.body), "gemini", "gemini-4-argon")
		message, _, field := PublicErrorDetail(err, 400)
		if field != scenario.field || !strings.Contains(message, scenario.reason) {
			t.Fatalf("wrong long-history diagnostic: %v", err)
		}
	}
}

func historyRequest(parts string, role string) []byte {
	return []byte(`{"contents":[{"role":"` + role + `","parts":` + parts + `},{"role":"user","parts":[{"text":"continue"}]}]}`)
}

func TestGeminiHistoryMetadata(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro", "future-model"} {
		for _, part := range []string{
			`[{"text":"visible answer","thoughtSignature":"private-signature"}]`,
			`[{"text":"visible answer","thought_signature":"private-signature","thought":false}]`,
			`[{"text":"visible answer","thoughtSignature":null,"thought":null}]`,
			`[{"text":"private-thought","thought":true,"thoughtSignature":"private-signature"},{"text":"visible answer"}]`,
			`[{"thoughtSignature":"private-signature"},{"text":"visible answer"}]`,
			`[{"text":"visible answer","inlineData":null,"file_data":null,"functionCall":null,"function_response":null}]`,
		} {
			t.Run(model+"/"+part, func(t *testing.T) {
				body := historyRequest(part, "model")
				original := string(body)
				input, err := Normalize(body, "gemini", model)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != original {
					t.Fatal("source bytes were mutated")
				}
				if err = ValidateFinal(input.Body); err != nil {
					t.Fatal(err)
				}
				query := gjson.GetBytes(input.Body, "query.text").String()
				if !strings.Contains(query, "visible answer") || strings.Contains(query, "private-") || strings.Contains(query, "thoughtSignature") {
					t.Fatalf("incorrect text-only history: %s", query)
				}
			})
		}
	}
}

func TestGeminiHistoryRejectsLossyContent(t *testing.T) {
	for _, scenario := range []struct{ part, role, field string }{
		{`[{"text":"hi","thoughtSignature":{}}]`, "model", "thoughtSignature"},
		{`[{"text":"hi","thought":1}]`, "model", "thought"},
		{`[{"text":"hi","thought":true}]`, "user", "thought"},
		{`[{"text":"hi","thoughtSignature":"private-signature","thought_signature":null}]`, "model", "thoughtSignature"},
		{`[{"text":"hi","inlineData":{"data":"private-value"},"thoughtSignature":"private-signature"}]`, "model", "inlineData"},
		{`[{"functionCall":{"name":"private-value"},"thoughtSignature":"private-signature"}]`, "model", "functionCall"},
		{`[{"function_response":{"name":"private-value"}}]`, "user", "function_response"},
		{`[{"text":"hi","fileData":{}}]`, "model", "fileData"},
		{`[{"text":"hi","unknown":true}]`, "model", "unknown"},
		{`[{"executableCode":{"code":"private-code"}}]`, "user", ""},
		{`[{"executableCode":{"code":"private-code"},"thought":true}]`, "model", ""},
		{`[{"executableCode":{"code":false}}]`, "model", "executableCode.code"},
		{`[{"codeExecutionResult":{"output":4}}]`, "model", "codeExecutionResult.output"},
		{`[{"text":"hi","executableCode":{"code":"private-code"}}]`, "model", ""},
		{`[{"text":null}]`, "model", "text"},
		{`[{}]`, "model", "text"},
	} {
		t.Run(scenario.part, func(t *testing.T) {
			_, err := Normalize(historyRequest(scenario.part, scenario.role), "gemini", "gemini-4-argon")
			if err == nil {
				t.Fatal("unsupported content was silently dropped")
			}
			path := "contents[0].parts[0]"
			if scenario.field != "" {
				path += "." + scenario.field
			}
			message, code, field := PublicErrorDetail(err, 400)
			if field != path || code != "invalid_field" || !strings.Contains(message, path) || strings.Contains(err.Error(), "private-") {
				t.Fatalf("unsafe or inaccurate field diagnostic: %v", err)
			}
		})
	}
}

func TestGeminiCodeHistoryRoundTrip(t *testing.T) {
	output := NewOutput()
	for _, event := range []Event{
		{Type: "code", Code: map[string]any{"language": "PYTHON", "code": "print(7)"}},
		{Type: "execution", Execution: map[string]any{"outcome": "OUTCOME_OK", "output": "7\n"}},
		{Type: "text", Text: "The answer is seven."},
	} {
		if _, _, err := output.Add(event); err != nil {
			t.Fatal(err)
		}
	}
	parts := gjson.GetBytes(output.Gemini(true, nil), "candidates.0.content.parts").Raw
	input, err := Normalize(historyRequest(parts, "model"), "gemini", "gemini-3.1-pro-preview")
	if err != nil {
		t.Fatal(err)
	}
	query := gjson.GetBytes(input.Body, "query.text").String()
	for _, value := range []string{"print(7)", "OUTCOME_OK", "The answer is seven.", "Previous model code"} {
		if !strings.Contains(query, value) {
			t.Fatalf("lost %q: %s", value, query)
		}
	}
	for _, keys := range [][2]string{{"executable_code", "code_execution_result"}, {"executableCode", "codeExecutionResult"}} {
		parts := fmt.Sprintf(`[{%q:{"language":"PYTHON","code":"print(7)"}},{%q:{"outcome":"OUTCOME_OK","output":"7"}}]`, keys[0], keys[1])
		if _, err := Normalize(historyRequest(parts, "model"), "gemini", "gemini-4-argon"); err != nil {
			t.Fatal(err)
		}
	}
}
