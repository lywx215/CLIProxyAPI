package geminienterprise

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseP0HistoricalOpenAISignaturesStayOutOfPrompt(t *testing.T) {
	// Google documents this signature on the tool-call envelope, not in arguments.
	// https://ai.google.dev/gemini-api/docs/generate-content/thought-signatures#signatures-for-openai-compatibility
	for _, signature := range []any{"opaque-protocol-state", nil, map[string]any{"opaque": "opaque-protocol-state"}} {
		for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro"} {
			arguments := ` {"thought_signature":"business-value","extra_content":{"google":{"thought_signature":"business-nested-value"}},"items":[1,9007199254740993],"text":"前🙂"} `
			call := map[string]any{
				"id": "call-17", "type": "function",
				"function": map[string]any{"name": "historical_echo", "arguments": arguments, "custom": "function-extension"},
				"extra_content": map[string]any{
					"google": map[string]any{"thought_signature": signature, "custom": "google-extension"},
					"custom": "provider-extension",
				},
				"custom": "call-extension",
			}
			request := map[string]any{"messages": []map[string]any{
				{"role": "assistant", "content": nil, "tool_calls": []any{call}},
				{"role": "tool", "tool_call_id": "call-17", "content": `{"thought_signature":"business-result","result":"前🙂"}`},
			}}
			body, errMarshal := json.Marshal(request)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			original := bytes.Clone(body)
			input, err := Normalize(body, "openai", model)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("normalization changed source bytes")
			}
			if err := ValidateFinal(input.Body); err != nil {
				t.Fatal(err)
			}
			query := gjson.GetBytes(input.Body, "query.text").String()
			if strings.Contains(query, "opaque-protocol-state") {
				t.Fatal("protocol signature reached the upstream prompt")
			}
			if !input.ClientFunctionsIgnored || !slices.Contains(input.Ignored, "messages[0].tool_calls[0].extra_content.google.thought_signature") {
				t.Fatal("protocol signature was not reported as ignored")
			}
			packets := p0HistoricalPackets(t, query)
			if len(packets) != 2 {
				t.Fatal("client call or result lost")
			}
			projected := packets[0]
			function := projected["function"].(map[string]any)
			if function["arguments"] != arguments {
				t.Fatal("arguments were parsed, reformatted or stripped as protocol metadata")
			}
			extra := projected["extra_content"].(map[string]any)
			google := extra["google"].(map[string]any)
			if _, exists := google["thought_signature"]; exists {
				t.Fatal("protocol signature key remains in historical context")
			}
			if projected["id"] != "call-17" || projected["custom"] != "call-extension" || function["custom"] != "function-extension" || extra["custom"] != "provider-extension" || google["custom"] != "google-extension" {
				t.Fatal("removing protocol metadata lost unrelated history fields")
			}
			result := packets[1]
			if result["tool_call_id"] != "call-17" || result["content"] != request["messages"].([]map[string]any)[1]["content"] {
				t.Fatal("result association or business content changed")
			}
		}
	}
}

func TestEnterpriseP0SignedAndUnsignedHistoricalCallsKeepSameBusinessContext(t *testing.T) {
	unsigned := `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"echo","arguments":"{\"message\":\"one\"}"},"extra_content":{"google":{"custom":"keep"}}},{"id":"b","type":"function","function":{"name":"echo","arguments":"{\"message\":\"two\"}"}}]},{"role":"tool","tool_call_id":"b","content":"second-result"},{"role":"tool","tool_call_id":"a","content":"first-result"}]}`
	signed := strings.Replace(unsigned, `"google":{"custom":"keep"}`, `"google":{"custom":"keep","thought_signature":"opaque-protocol-state"}`, 1)
	plain, errPlain := Normalize([]byte(unsigned), "openai", "gemini-2.5-pro")
	safe, errSigned := Normalize([]byte(signed), "openai", "gemini-2.5-pro")
	if errPlain != nil || errSigned != nil {
		t.Fatalf("valid history rejected: unsigned=%v signed=%v", errPlain, errSigned)
	}
	if !bytes.Equal(plain.Body, safe.Body) {
		t.Fatal("opaque signature changed business context or historical result ordering")
	}
}

func TestEnterpriseP0UnknownFunctionMetadataIsNotRecursivelyStripped(t *testing.T) {
	for _, extra := range []string{`null`, `"opaque-client-extension"`, `{"google":null,"custom":"keep"}`, `{"google":{"thoughtSignature":"unconfirmed-client-extension"}}`} {
		body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"echo","arguments":"{\"value\":1}"},"extra_content":` + extra + `}]},{"role":"tool","tool_call_id":"a","content":"done"}]}`)
		input, err := Normalize(body, "openai", "gemini-2.5-pro")
		if err != nil {
			t.Fatal(err)
		}
		projected := p0HistoricalPackets(t, gjson.GetBytes(input.Body, "query.text").String())[0]
		var expected any
		if err := json.Unmarshal([]byte(extra), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(projected["extra_content"], expected) {
			t.Fatal("unknown client metadata was guessed to be a protocol signature")
		}
	}
}

func p0HistoricalPackets(t *testing.T, text string) []map[string]any {
	t.Helper()
	var packets []map[string]any
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "{") {
			var value map[string]any
			if err := json.Unmarshal([]byte(line), &value); err != nil {
				t.Fatal(err)
			}
			packets = append(packets, value)
		}
	}
	if len(packets) == 0 {
		t.Fatal("client function data missing")
	}
	return packets
}
