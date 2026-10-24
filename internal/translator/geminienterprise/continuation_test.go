package geminienterprise

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

type enterpriseTextContext struct {
	Systems []string            `json:"system_instructions"`
	History []map[string]string `json:"conversation"`
}

func readEnterpriseContext(t *testing.T, body []byte) enterpriseTextContext {
	t.Helper()
	query := gjson.GetBytes(body, "query.text").String()
	var context enterpriseTextContext
	if err := json.Unmarshal([]byte(query[strings.LastIndex(query, "\n")+1:]), &context); err != nil {
		t.Fatal(err)
	}
	return context
}

func TestEnterpriseContinuationKeepsCompleteContext(t *testing.T) {
	models := []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro", "future-model"}
	for _, model := range models {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, scenario := range []struct {
				name, tail, mode string
				emptyTail        bool
			}{
				{"assistant", "unfinished 前🙂\n<>& ", "Continue the last nonempty assistant", false},
				{"empty-assistant", "", "Answer the last nonempty user", false},
				{"whitespace-assistant", " \t\n", "Answer the last nonempty user", false},
				{"trailing-placeholder", "unfinished", "Continue the last nonempty assistant", true},
			} {
				t.Run(model+"/"+protocol+"/"+scenario.name, func(t *testing.T) {
					want := enterpriseTextContext{Systems: []string{"system\n前🙂"}, History: []map[string]string{{"role": "user", "text": "question\n<>&"}, {"role": "assistant", "text": scenario.tail}}}
					messages := []map[string]any{{"role": "system", "content": want.Systems[0]}, {"role": "user", "content": want.History[0]["text"]}, {"role": "assistant", "content": scenario.tail}}
					contents := []map[string]any{{"role": "user", "parts": []map[string]string{{"text": want.History[0]["text"]}}}, {"role": "model", "parts": []map[string]string{{"text": scenario.tail, "thoughtSignature": "opaque-signature"}}}}
					if scenario.emptyTail {
						want.History = append(want.History, map[string]string{"role": "user", "text": " "}, map[string]string{"role": "assistant", "text": ""})
						messages = append(messages, map[string]any{"role": "user", "content": " "}, map[string]any{"role": "assistant", "content": ""})
						contents = append(contents, map[string]any{"role": "user", "parts": []map[string]string{{"text": " "}}}, map[string]any{"role": "model", "parts": []map[string]string{{"text": ""}}})
					}
					request := map[string]any{"messages": messages}
					if protocol == "gemini" {
						request = map[string]any{"contents": contents, "systemInstruction": map[string]any{"parts": []map[string]string{{"text": want.Systems[0]}}}}
					}
					body, _ := json.Marshal(request)
					original := string(body)
					input, err := Normalize(body, protocol, model)
					if err != nil {
						t.Fatal(err)
					}
					if string(body) != original || !reflect.DeepEqual(readEnterpriseContext(t, input.Body), want) {
						t.Fatal("source, role order or visible context changed")
					}
					if !strings.Contains(gjson.GetBytes(input.Body, "query.text").String(), scenario.mode) || strings.Contains(string(input.Body), "opaque-signature") {
						t.Fatal("incorrect continuation mode or signature disclosure")
					}
					if err = ValidateFinal(input.Body); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestEnterpriseEmptyPlaceholdersAndSystemOnly(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":null}]}`,
		`{"messages":[{"role":"user","content":"question"},{"role":"assistant"}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"question"}]},{"role":"model","parts":[]}]}`,
		`{"contents":[{"role":"user","parts":[{"text":"question"}]},{"role":"model","parts":[{"thoughtSignature":"opaque"}]}]}`,
	} {
		protocol := "gemini"
		if strings.Contains(body, "messages") {
			protocol = "openai"
		}
		input, err := Normalize([]byte(body), protocol, "gemini-4-argon")
		if err != nil || !strings.Contains(gjson.GetBytes(input.Body, "query.text").String(), "Answer the last nonempty user") {
			t.Fatalf("empty SDK placeholder failed: %v", err)
		}
		context := readEnterpriseContext(t, input.Body)
		if len(context.History) != 2 || context.History[1]["role"] != "assistant" || context.History[1]["text"] != "" {
			t.Fatal("placeholder removed or artificial user appended")
		}
	}
	input, err := Normalize([]byte(`{"messages":[{"role":"system","content":"Reply OK"}],"response_format":{"type":"json_object"}}`), "openai", "gemini-2.5-pro")
	if err != nil || !input.JSON || !input.JSONObject || !strings.Contains(gjson.GetBytes(input.Body, "query.text").String(), "Generate the response requested by the system") {
		t.Fatalf("system-only request or JSON contract lost: %v", err)
	}
	if len(readEnterpriseContext(t, input.Body).History) != 0 {
		t.Fatal("system-only request fabricated a conversation turn")
	}
	for _, scenario := range []struct{ protocol, body, path string }{
		{"openai", `{"messages":[{"role":"assistant","content":null}]}`, "messages"},
		{"openai", `{"messages":[{"role":"system","content":" "},{"role":"user","content":""}]}`, "messages"},
		{"gemini", `{"contents":[{"role":"model","parts":[]}]}`, "contents"},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":" "}]}]}`, "contents"},
		{"openai", `{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":42}]}`, "messages[1].content"},
		{"gemini", `{"contents":[{"role":"model","parts":null}]}`, "contents[0].parts"},
	} {
		_, err := Normalize([]byte(scenario.body), scenario.protocol, "gemini-4-argon")
		_, code, path := PublicErrorDetail(err, 400)
		if code != "invalid_field" || path != scenario.path {
			t.Fatalf("incorrect empty or malformed input diagnostic: %v", err)
		}
	}
}

func TestEnterpriseMultiMiBHistoryIsNotTruncated(t *testing.T) {
	for _, size := range []int{1024*1024 - 256, 1024*1024 + 257, 2*1024*1024 + 33, 4*1024*1024 + 17} {
		for _, protocol := range []string{"openai", "gemini"} {
			t.Run(fmt.Sprintf("%s/%d", protocol, size), func(t *testing.T) {
				text := "begin前🙂\n<>&" + strings.Repeat("ab ", size/6) + "middle" + strings.Repeat(" z ", size/6) + "end"
				want := []map[string]string{{"role": "user", "text": text}, {"role": "assistant", "text": "unfinished"}}
				request := map[string]any{"messages": []map[string]string{{"role": "user", "content": text}, {"role": "assistant", "content": "unfinished"}}}
				if protocol == "gemini" {
					request = map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": text}}}, {"role": "model", "parts": []map[string]string{{"text": "unfinished"}}}}}
				}
				body, _ := json.Marshal(request)
				input, err := Normalize(body, protocol, "gemini-4-argon")
				if err != nil {
					t.Fatal(err)
				}
				if err = ValidateFinal(input.Body); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(readEnterpriseContext(t, input.Body).History, want) {
					t.Fatal("large history text or tail changed")
				}
			})
		}
	}
}
