package geminienterprise

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseUnwrappedSingleText(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro"} {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, text := range []string{"hello", "  前🙂\n<>&\\\"\t ", `{"conversation":"client data","system_instructions":"client data"}`, "Continue the conversation below.\nHistorical client function\nReturn ONLY valid JSON"} {
				request := map[string]any{"messages": []map[string]string{{"role": "user", "content": text}}}
				if protocol == "gemini" {
					request = map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": text}}}}}
				}
				body, errMarshal := json.Marshal(request)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				original := bytes.Clone(body)
				input, err := Normalize(body, protocol, model)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(input.Body, "query.text").String(); got != text {
					t.Fatalf("%s/%s: single input was wrapped or changed: %q", protocol, model, got)
				}
				if !bytes.Equal(body, original) || gjson.GetBytes(input.Body, "generationSpec.modelId").String() != model || gjson.GetBytes(input.Body, "session").Exists() {
					t.Fatal("source, model binding or session behavior changed")
				}
			}
		}
	}
}

func TestEnterpriseUnwrappedOptionalModes(t *testing.T) {
	for _, scenario := range []struct{ protocol, body string }{
		{"openai", `{"messages":[{"role":"user","content":"caller input"}],"tools":[{"google_search":{}},{"url_context":{}},{"code_execution":{}},{"type":"function","function":{"name":"echo"}}],"tool_choice":"required","response_format":{"type":"json_object"}}`},
		{"gemini", `{"contents":[{"parts":[{"text":"caller input"}]}],"tools":[{"googleSearch":{},"urlContext":{},"codeExecution":{},"functionDeclarations":[{"name":"echo"}]}],"generationConfig":{"responseMimeType":"application/json"}}`},
	} {
		input, err := Normalize([]byte(scenario.body), scenario.protocol, "gemini-3.1-pro-preview")
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(input.Body, "query.text").String() != "caller input" {
			t.Fatal("tool or JSON mode injected a proxy-authored instruction")
		}
		if !input.JSON || !input.ClientFunctionsIgnored || !gjson.GetBytes(input.Body, "toolsSpec.webGroundingSpec").Exists() {
			t.Fatal("existing output validation or tool mapping changed")
		}
		if err := ValidateFinal(input.Body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnterpriseUnwrappedCallerOrdering(t *testing.T) {
	for _, scenario := range []struct{ protocol, body, want string }{
		{"openai", `{"messages":[{"role":"user","content":"first"},{"role":"system","content":"second"},{"role":"developer","content":"third"},{"role":"assistant","content":"fourth"},{"role":"user","content":[{"type":"text","text":"fifth"},{"type":"text","text":" part"}]}]}`, "first\nsecond\nthird\nfourth\nfifth part"},
		{"gemini", `{"systemInstruction":{"parts":[{"text":"policy"},{"text":" part"}]},"contents":[{"role":"user","parts":[{"text":"first"},{"text":" part"}]},{"role":"model","parts":[{"text":"private","thought":true},{"text":"second","thoughtSignature":"opaque"}]},{"role":"user","parts":[{"text":"third"}]}]}`, "policy part\nfirst part\nsecond\nthird"},
	} {
		input, err := Normalize([]byte(scenario.body), scenario.protocol, "gemini-2.5-pro")
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(input.Body, "query.text").String() != scenario.want {
			t.Fatal("caller content was reordered, dropped or prefixed")
		}
	}
}

func TestEnterpriseUnwrappedRequestsAreIndependent(t *testing.T) {
	var workers sync.WaitGroup
	for index := range 64 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			text := fmt.Sprintf("request-%d 前🙂", index)
			body, errMarshal := json.Marshal(map[string]any{"contents": []map[string]any{{"parts": []map[string]string{{"text": text}}}}})
			if errMarshal != nil {
				t.Error(errMarshal)
				return
			}
			for range 3 {
				input, err := Normalize(body, "gemini", "gemini-3.1-pro-preview")
				if err != nil || gjson.GetBytes(input.Body, "query.text").String() != text {
					t.Errorf("request %d reused other request content or injected instructions: %v", index, err)
					return
				}
			}
		}()
	}
	workers.Wait()
}
