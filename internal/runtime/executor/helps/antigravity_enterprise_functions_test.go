package helps

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBusinessAICodeFunctionResponsesSeparateInstructionsPreservingHistory(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"Read both sensors"}]},{"role":"model","parts":[{"functionCall":{"name":"read_sensor","id":"call-a","args":{"sensor":"a"}},"thoughtSignature":"native-signature-a"},{"functionCall":{"name":"read_sensor","id":"call-b","args":{"sensor":"b"}}},{"text":""}]},{"role":"user","futureContent":{"keep":true},"parts":[{"functionResponse":{"id":"call-b","name":"read_sensor","response":{"reading":82}}},{"functionResponse":{"id":"call-a","name":"read_sensor","response":{"reading":17}}},{"text":"Compare the two readings"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}],"tools":[{"functionDeclarations":[{"name":"read_sensor"}]}],"toolConfig":{"functionCallingConfig":{"mode":"NONE","allowedFunctionNames":["read_sensor"]}},"generationConfig":{"temperature":0.2},"futureRoot":{"keep":true}}`)
	output, err := NormalizeBusinessAICodeFunctionResponses(input)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(output, "contents.#").Int() != 4 || gjson.GetBytes(output, "contents.2.parts.#").Int() != 2 || gjson.GetBytes(output, "contents.3.parts.#").Int() != 2 {
		t.Fatalf("results or instruction parts were lost: %s", output)
	}
	for _, path := range []string{"contents.0", "contents.1", "tools", "toolConfig", "generationConfig", "futureRoot"} {
		if gjson.GetBytes(output, path).Raw != gjson.GetBytes(input, path).Raw {
			t.Errorf("unrelated field changed: %s", path)
		}
	}
	if gjson.GetBytes(output, "contents.2.parts.0.functionResponse.id").String() != "call-b" || gjson.GetBytes(output, "contents.2.parts.1.functionResponse.id").String() != "call-a" {
		t.Fatal("parallel response order or IDs changed")
	}
	if gjson.GetBytes(output, "contents.3.parts.0.text").String() != "Compare the two readings" || gjson.GetBytes(output, "contents.3.parts.1.inlineData.data").String() != "aGVsbG8=" {
		t.Fatal("ordinary user parts changed")
	}
	for _, path := range []string{"contents.2.futureContent.keep", "contents.3.futureContent.keep"} {
		if !gjson.GetBytes(output, path).Bool() {
			t.Fatal("content extension was lost")
		}
	}
	second, errAgain := NormalizeBusinessAICodeFunctionResponses(output)
	if errAgain != nil || !bytes.Equal(second, output) {
		t.Fatal("normalization is not idempotent")
	}
}

func TestBusinessAICodeFunctionResponsesPreserveInterleavedPartOrder(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"text":"before"},{"functionResponse":{"name":"one","response":{"x":1}}},{"functionResponse":{"name":"two","response":{"x":2}}},{"text":"between"},{"function_response":{"name":"three","response":{"x":3}}},{"text":"after"}]}]}`)
	output, err := NormalizeBusinessAICodeFunctionResponses(input)
	if err != nil {
		t.Fatal(err)
	}
	var original, normalized map[string]any
	if err := json.Unmarshal(input, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output, &normalized); err != nil {
		t.Fatal(err)
	}
	flatten := func(value map[string]any) []any {
		var parts []any
		for _, content := range value["contents"].([]any) {
			parts = append(parts, content.(map[string]any)["parts"].([]any)...)
		}
		return parts
	}
	if !reflect.DeepEqual(flatten(original), flatten(normalized)) || gjson.GetBytes(output, "contents.#").Int() != 5 {
		t.Fatalf("interleaved history reordered: %s", output)
	}
}

func TestBusinessAICodeFunctionResponsesLeaveUnrelatedPayloadsUnchanged(t *testing.T) {
	for _, input := range []string{
		`{"contents":[{"role":"user","parts":[{"text":""},{"text":"plain user text"}]}],"tools":[{"googleSearch":{}}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f"}},{"text":""}]}]}`,
		`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"x":1}}}]},{"role":"user","parts":[{"text":"continue"}]}]}`,
		`{"contents":[{"role":"model","parts":[{"functionResponse":{"name":"f"}},{"text":"invalid role handled elsewhere"}]}]}`,
		`{"contents":[{"role":"user","parts":[]}],"generationConfig":{"responseMimeType":"application/json"}}`,
		`{"contents":"invalid contents are validated elsewhere"}`,
		`{"systemInstruction":{"parts":[{"text":"system"}]}}`,
	} {
		output, err := NormalizeBusinessAICodeFunctionResponses([]byte(input))
		if err != nil || !bytes.Equal(output, []byte(input)) {
			t.Fatalf("unrelated payload changed: %s -> %s, err=%v", input, output, err)
		}
	}
}
