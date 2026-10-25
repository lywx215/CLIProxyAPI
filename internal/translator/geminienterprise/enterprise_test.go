package geminienterprise

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

type fragmentReader struct {
	data []byte
	size int
}

func (r *fragmentReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.size, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestEnterpriseNormalizeProtocols(t *testing.T) {
	for _, test := range []struct{ protocol, body string }{
		{"openai", `{"messages":[{"role":"system","content":"policy"},{"role":"user","content":"old"},{"role":"assistant","content":"previous"},{"role":"user","content":[{"type":"text","text":"read https://example.com"}]}],"tools":[{"google_search":{}},{"url_context":{}},{"code_execution":{}}],"gemini_tools":[{"googleSearch":{}}],"response_format":{"type":"json_object"},"temperature":0.3}`},
		{"gemini", `{"systemInstruction":{"parts":[{"text":"policy"}]},"contents":[{"role":"user","parts":[{"text":"old"}]},{"role":"model","parts":[{"text":"previous"}]},{"parts":[{"text":"read https://example.com"}]}],"tools":[{"googleSearch":{},"urlContext":{},"codeExecution":{}}],"generationConfig":{"responseMimeType":"application/json","temperature":0.3}}`},
	} {
		t.Run(test.protocol, func(t *testing.T) {
			input, err := Normalize([]byte(test.body), test.protocol, "gemini-4-argon")
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateFinal(input.Body); err != nil {
				t.Fatal(err)
			}
			query := gjson.GetBytes(input.Body, "query.text").String()
			if query != "policy\nold\nprevious\nread https://example.com" {
				t.Fatalf("content changed or proxy instructions added: %q", query)
			}
			if !gjson.GetBytes(input.Body, "toolsSpec.webGroundingSpec").Exists() || !input.JSON || len(input.Ignored) == 0 {
				t.Fatalf("missing adaptations: %+v", input)
			}
			if gjson.GetBytes(input.Body, "session").Exists() || gjson.GetBytes(input.Body, "generationSpec.modelId").String() != "gemini-4-argon" {
				t.Fatal("unexpected model/session")
			}
		})
	}
}

func TestEnterpriseRejectLossyInputs(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		`{"messages":[{"role":"user","content":"hi","audio":{}}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"googleSearch":{},"google_search":{}}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"tools":[{"google_search":true}]}`,
		`{"messages":[{"role":"user","content":"hi"}],"web_search_options":[]}`,
		`{"messages":[{"role":"user","content":"hi"}],"stream":"yes"}`,
	} {
		if _, err := Normalize([]byte(body), "openai", "gemini-4-argon"); err == nil {
			t.Fatalf("accepted lossy request %s", body)
		}
	}
	for _, body := range []string{
		`{"contents":[{"parts":[{"inlineData":{"data":"image"}}]}]}`,
		`{"contents":[{"parts":[{"functionResponse":{"name":"f"}}]}]}`,
		`{"contents":[{"parts":[{"text":"hi"}]}],"systemInstruction":{},"system_instruction":{}}`,
		`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","response_mime_type":"text/plain"}}`,
	} {
		if _, err := Normalize([]byte(body), "gemini", "gemini-4-argon"); err == nil {
			t.Fatalf("accepted lossy Gemini request %s", body)
		}
	}
	if _, err := Normalize([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai", ""); err == nil {
		t.Fatal("invented a default model")
	}
}

const enterpriseVisible = `{"answer":{"state":"IN_PROGRESS","replies":[{"groundedContent":{"content":{"text":"前🙂引用"},"textGroundingMetadata":{"references":[{"documentMetadata":{"uri":"https://example.com","title":"Example"}}],"segments":[{"startIndex":7,"endIndex":13,"text":"引用","referenceIndices":[0]}]}}}]}}`
const enterpriseDone = `{"answer":{"state":"SUCCEEDED"}}`

func TestEnterpriseFragmentedStreamFormatsAndGrounding(t *testing.T) {
	for _, body := range []string{"[" + enterpriseVisible + "," + enterpriseDone + "]", enterpriseVisible + "\n" + enterpriseDone, "data: " + enterpriseVisible + "\n\ndata: " + enterpriseDone + "\n\n"} {
		for _, size := range []int{1, 2, 7, 4096} {
			output := NewOutput()
			p := &Parser{}
			if err := p.Parse(&fragmentReader{data: []byte(body), size: size}, func(e Event) error { _, _, err := output.Add(e); return err }); err != nil {
				t.Fatal(err)
			}
			if err := output.Finish(Input{}); err != nil {
				t.Fatal(err)
			}
			chat := output.Chat("gemini-4-argon")
			if got := gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.start_index").Int(); got != 2 {
				t.Fatalf("OpenAI rune start=%d, want 2", got)
			}
			if got := gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.end_index").Int(); got != 4 {
				t.Fatalf("OpenAI rune end=%d, want 4", got)
			}
			gemini := output.Gemini(true, nil)
			if got := gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.0.segment.startIndex").Int(); got != 7 {
				t.Fatalf("Gemini byte start=%d, want 7", got)
			}
			for _, data := range [][]byte{chat, gemini} {
				for _, absent := range []string{"usage", "usageMetadata", "webSearchQueries", "urlContextMetadata", "assistToken", "sessionInfo"} {
					if gjson.GetBytes(data, absent).Exists() {
						t.Fatalf("invented/leaked %s", absent)
					}
				}
			}
		}
	}
}

func TestEnterpriseTerminalErrorsAndThoughtFencing(t *testing.T) {
	thought := `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"secret thought","thought":true}}]},"assistToken":"secret","sessionInfo":{"id":"private"}}`
	for _, test := range []struct {
		body   string
		scoped bool
	}{
		{`{"error":{"message":"bad"}}`, false},
		{`{"answer":{"state":"IN_PROGRESS"},"error":{"message":"bad"}}`, true},
		{`{"answer":{"state":"IN_PROGRESS","replies":{}}}`, true},
		{thought + `{"error":{"message":"bad"}}`, true},
		{enterpriseVisible, true},
		{"[" + enterpriseVisible + "," + enterpriseDone + "]junk", true},
		{"[" + enterpriseVisible + "," + enterpriseDone + ",]", true},
		{enterpriseVisible + `{"answer":{"state":"FAILED"}}`, true},
		{thought + `{"answer":{"state":"SKIPPED"}}`, true},
		{enterpriseVisible + enterpriseDone + `{"answer":{"state":"IN_PROGRESS"}}`, true},
		{"data: " + enterpriseVisible + "\n\ndata: broken\n\n", true},
		{"event: error\ndata: {\"message\":\"failure\"}\n\n", false},
		{"event: error\ndata: {\"answer\":{\"state\":\"IN_PROGRESS\"}}\n\n", true},
		{"data: " + enterpriseVisible + "\n\ndata: " + enterpriseDone + "\n\nevent: error\ndata: {\"message\":\"failure\"}\n\n", true},
		{"data: " + enterpriseVisible + "\n\ndata: " + enterpriseDone + "\n\nevent: error\n", true},
	} {
		p := &Parser{}
		var text strings.Builder
		err := p.Parse(strings.NewReader(test.body), func(e Event) error { text.WriteString(e.Text); return nil })
		var failure interface{ IsRequestScoped() bool }
		if !errors.As(err, &failure) || failure.IsRequestScoped() != test.scoped {
			t.Fatalf("error=%v, want scoped=%v for %s", err, test.scoped, test.body)
		}
		if strings.Contains(text.String(), "secret") {
			t.Fatal("thought leaked")
		}
	}
}

func TestEnterpriseCodeJSONAndStreamingPackets(t *testing.T) {
	body := `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"executableCode":{"language":"PYTHON","code":"print(7)"}}},{"content":{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"7\n"}}},{"content":{"text":"Answer: 7"}}]}},` + enterpriseDone + `]`
	o := NewOutput()
	p := &Parser{}
	if err := p.Parse(strings.NewReader(body), func(e Event) error { _, _, err := o.Add(e); return err }); err != nil {
		t.Fatal(err)
	}
	if len(o.Parts) != 3 || !strings.Contains(o.Text, "print(7)") || !strings.Contains(o.Text, "OUTCOME_OK") {
		t.Fatal("lost actual code/execution")
	}
	if !gjson.GetBytes(o.Gemini(true, nil), "candidates.0.content.parts.0.executableCode").Exists() {
		t.Fatal("lost Gemini code part")
	}
	for _, test := range []struct {
		text          string
		object, valid bool
	}{{`{"ok":true}`, true, true}, {`[1,2]`, true, false}, {`[1,2]`, false, true}, {`null`, true, false}, {`broken`, false, false}, {`{"number":1e1000}`, true, true}, {`1e1000`, false, true}} {
		output := NewOutput()
		_, _, _ = output.Add(Event{Type: "text", Text: test.text})
		err := output.Finish(Input{JSON: true, JSONObject: test.object})
		if (err == nil) != test.valid {
			t.Fatalf("JSON %s: %v", test.text, err)
		}
	}
	packet := string(o.ChatDelta("gemini-4-argon", "visible", nil, false))
	if !strings.Contains(packet, `"content":"visible"`) || !strings.Contains(packet, `"role":"assistant"`) {
		t.Fatal("first packet must contain visible content")
	}
}

func TestEnterpriseUpstreamPartsDoNotLeakPrivateFields(t *testing.T) {
	body := `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"executableCode":{"language":"PYTHON","code":"print(7)","assistToken":"private-token"}}},{"content":{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"7","sessionInfo":{"id":"private-session"}}}}]}},` + enterpriseDone + `]`
	output := NewOutput()
	if err := (&Parser{}).Parse(strings.NewReader(body), func(e Event) error { _, _, err := output.Add(e); return err }); err != nil {
		t.Fatal(err)
	}
	gemini := string(output.Gemini(true, nil))
	for _, private := range []string{"private-token", "private-session", "assistToken", "sessionInfo"} {
		if strings.Contains(gemini, private) {
			t.Fatalf("private upstream field leaked: %s", private)
		}
	}
	if !strings.Contains(gemini, "print(7)") || !strings.Contains(gemini, "OUTCOME_OK") {
		t.Fatal("lost actual code or execution output")
	}
	for _, content := range []string{`{"file":{"name":"audio"}}`, `{"inlineData":{"data":"media"}}`, `{"executableCode":{"code":"print(7)","language":7}}`} {
		body := `{"answer":{"state":"IN_PROGRESS","replies":[{"content":` + content + `}]}}` + enterpriseDone
		if err := (&Parser{}).Parse(strings.NewReader(body), func(Event) error { return nil }); err == nil {
			t.Fatalf("silently discarded unsupported content: %s", content)
		}
	}
}

func TestEnterpriseFinalToolsPayloadValidation(t *testing.T) {
	for _, tool := range []string{`null`, `7`, `"search"`, `[]`} {
		body := `{"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-4-argon"},"toolsSpec":{"webGroundingSpec":` + tool + `}}`
		if err := ValidateFinal([]byte(body)); err == nil {
			t.Fatalf("invalid web grounding override was accepted: %s", tool)
		}
	}
}

func TestEnterpriseGroundingOffsetsAcrossTextAndCodeChunks(t *testing.T) {
	output := NewOutput()
	for _, event := range []Event{{Type: "text", Text: "前🙂"}, {Type: "code", Code: map[string]any{"language": "PYTHON", "code": "🙂"}}, {Type: "text", Text: "甲"}, {Type: "text", Text: "乙🙂引用"}, {Type: "grounding", Sources: []Source{{URI: "https://example.com"}}, Supports: []Support{{Start: 7, End: 13, Text: "引用", Indices: []int{0}}}}} {
		if _, _, err := output.Add(event); err != nil {
			t.Fatal(err)
		}
	}
	chat := output.Chat("gemini-4-argon")
	if gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.start_index").Int() != 22 || gjson.GetBytes(chat, "choices.0.message.annotations.0.url_citation.end_index").Int() != 24 {
		t.Fatalf("incorrect rune indices after code and multibyte chunks: %s", chat)
	}
	gemini := output.Gemini(true, nil)
	if gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.0.segment.partIndex").Int() != 2 || gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.0.segment.startIndex").Int() != 10 || gjson.GetBytes(gemini, "candidates.0.groundingMetadata.groundingSupports.0.segment.endIndex").Int() != 16 {
		t.Fatalf("incorrect Gemini part byte indices: %s", gemini)
	}
}
