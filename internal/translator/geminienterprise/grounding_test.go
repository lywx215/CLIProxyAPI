package geminienterprise

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseDetachedGroundingUsesCompleteVisibleText(t *testing.T) {
	for _, content := range []string{"", `"content":{},`} {
		body := `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"hidden","thought":true}},{"content":{"text":"前🙂"}},{"content":{"text":"引用 tail"}}]}},{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{` + content + `"textGroundingMetadata":{"references":[{"documentMetadata":{"uri":"https://example.com/source","title":"Source"}}],"segments":[{"startIndex":"7","endIndex":"13","text":"引用","referenceIndices":[0]}]}}}]}}]`
		output := NewOutput()
		if err := (&Parser{}).Parse(strings.NewReader(body), func(event Event) error {
			_, _, err := output.Add(event)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		gemini := output.Gemini(true, nil)
		if gjson.GetBytes(gemini, "candidates.0.content.parts.0.text").String() != "前🙂引用 tail" ||
			gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.0.segment.startIndex").Int() != 7 ||
			gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.0.segment.endIndex").Int() != 13 {
			t.Fatalf("detached grounding lost full-text byte offsets: %s", gemini)
		}
		chat := output.Chat("gemini-4-argon")
		if gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.start_index").Int() != 2 ||
			gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.end_index").Int() != 4 ||
			strings.Contains(string(chat), "hidden") {
			t.Fatalf("detached grounding lost character offsets or leaked thought: %s", chat)
		}
	}
}

func TestEnterpriseDetachedGroundingMapsAcrossCodeParts(t *testing.T) {
	output := NewOutput()
	for _, event := range []Event{
		{Type: "text", Text: "前🙂"},
		{Type: "code", Code: map[string]any{"language": "PYTHON", "code": "print(7)"}},
		{Type: "text", Text: "甲"},
		{Type: "text", Text: "乙🙂引用"},
		{Type: "grounding", WholeText: true, Sources: []Source{{URI: "https://example.com"}}, Supports: []Support{
			{Start: 17, End: 23, Text: "引用", Indices: []int{0}},
			{Start: 0, End: 10, Text: "前🙂甲", Indices: []int{0}},
		}},
	} {
		if _, _, err := output.Add(event); err != nil {
			t.Fatal(err)
		}
	}
	gemini := output.Gemini(true, nil)
	segment := gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.0.segment")
	if segment.Get("partIndex").Int() != 2 || segment.Get("startIndex").Int() != 10 || segment.Get("endIndex").Int() != 16 ||
		gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.#").Int() != 3 {
		t.Fatalf("detached citation did not map or split across typed parts: %s", gemini)
	}
	chat := output.Chat("gemini-4-argon")
	text := []rune(gjson.GetBytes(chat, "choices.0.message.content").String())
	annotation := gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation")
	if string(text[annotation.Get("start_index").Int():annotation.Get("end_index").Int()]) != "引用" {
		t.Fatalf("code rendering shifted OpenAI citation: %s", chat)
	}
}

func TestEnterpriseDetachedGroundingSourcesBeforeVisibleText(t *testing.T) {
	metadata := map[string]any{"references": []map[string]any{{"documentMetadata": map[string]string{"uri": "https://example.com/source"}}}}
	first, _ := json.Marshal(map[string]any{"answer": map[string]any{"state": "IN_PROGRESS", "replies": []map[string]any{{"groundedContent": map[string]any{"textGroundingMetadata": metadata}}}}})
	body := string(first) + `{"answer":{"state":"SUCCEEDED","replies":[{"content":{"text":"visible"}}]}}`
	output := NewOutput()
	if err := (&Parser{}).Parse(strings.NewReader(body), func(event Event) error {
		_, _, err := output.Add(event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(output.Sources) != 1 || len(output.Annotations) != 0 || output.Text != "visible" {
		t.Fatal("source-only metadata was lost or fabricated a text span")
	}
}
