package geminienterprise

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseThoughtVisibilityDoesNotShiftGrounding(t *testing.T) {
	response := `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"requested summary"}},{"content":{"text":"前🙂引用 tail"}}]}},{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"textGroundingMetadata":{"references":[{"documentMetadata":{"uri":"https://example.com/source","title":"Source"}}],"segments":[{"startIndex":"7","endIndex":"13","text":"引用","referenceIndices":[0]}]}}}]}}]`
	for _, include := range []bool{false, true} {
		output := NewOutput()
		err := (&Parser{IncludeThoughts: include}).Parse(strings.NewReader(response), func(event Event) error {
			_, _, err := output.Add(event)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if output.Text != "前🙂引用 tail" || (output.Reasoning != "") != include {
			t.Fatal("thought summaries polluted visible text")
		}
		partIndex := int64(0)
		if include {
			partIndex = 1
		}
		native := output.Gemini(true, nil)
		if got := gjson.GetBytes(native, "candidates.0.groundingMetadata.groundingSupports.0.segment.partIndex").Int(); got != partIndex {
			t.Fatalf("native grounding part index = %d, want %d", got, partIndex)
		}
		chat := output.Chat("gemini-3.1-pro-preview")
		if gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.start_index").Int() != 2 || gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.end_index").Int() != 4 {
			t.Fatal("reasoning changed visible Unicode citation indices")
		}
	}
}

func TestEnterpriseThoughtsAndJSONRemainSeparate(t *testing.T) {
	output := NewOutput()
	for _, event := range []Event{{Type: "thought", Text: "summary "}, {Type: "thought", Text: "continued"}, {Type: "text", Text: "```json\n{\"ok\":true}\n```"}} {
		if _, _, err := output.Add(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := output.Finish(Input{JSON: true, JSONObject: true}); err != nil {
		t.Fatal(err)
	}
	if output.Text != `{"ok":true}` || len(output.Parts) != 2 || output.Parts[0]["thought"] != true || output.Parts[1]["text"] != `{"ok":true}` {
		t.Fatalf("JSON cleanup included or lost thinking: %+v", output.Parts)
	}
	if gjson.GetBytes(output.Chat("gemini-3.1-pro-preview"), "choices.0.message.reasoning_content").String() != "summary continued" {
		t.Fatal("chat thought output was lost")
	}
}

func TestEnterpriseThinkingInputValidation(t *testing.T) {
	for _, config := range []string{`{"thinkingLevel":7}`, `{"includeThoughts":"true"}`, `{"thinkingBudget":128}`, `{"thinkingLevel":"HIGH","thinking_level":"LOW"}`} {
		_, err := Normalize([]byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":`+config+`}}`), "gemini", "gemini-3.1-pro-preview")
		if err == nil {
			t.Fatalf("invalid source thinking controls accepted: %s", config)
		}
	}
	if err := ValidateFinal([]byte(`{"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-3.1-pro-preview","thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true}}}`)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"MEDIUM"`, `"invalid"`, `7`} {
		if err := ValidateFinal([]byte(`{"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-3.1-pro-preview","thinkingConfig":{"thinkingLevel":` + value + `}}}`)); err == nil {
			t.Fatal("unverified final level accepted")
		}
	}
}
