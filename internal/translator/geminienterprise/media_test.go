package geminienterprise

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func enterpriseSuggestionsFixture() (string, string) {
	data := base64.StdEncoding.EncodeToString([]byte(`{"suggestions":["private UI follow-up"]}`))
	part, _ := json.Marshal(map[string]any{"inlineData": map[string]string{"mimeType": "application/json+suggestions", "data": data}})
	return `{"answer":{"state":"IN_PROGRESS","replies":[{"content":` + string(part) + `}]}}`, data
}

func TestEnterpriseSuggestionsUIIsNotVisibleOrLeaked(t *testing.T) {
	suggestions, encoded := enterpriseSuggestionsFixture()
	visible := `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"visible answer"}}]}}`
	for _, body := range []string{
		"[" + suggestions + "," + visible + "," + enterpriseDone + "]",
		suggestions + "\n" + visible + "\n" + enterpriseDone,
		"data: " + suggestions + "\n\ndata: " + visible + "\n\ndata: " + enterpriseDone + "\n\n",
	} {
		output := NewOutput()
		var events []string
		err := (&Parser{}).Parse(&fragmentReader{data: []byte(body), size: 3}, func(event Event) error {
			events = append(events, event.Type)
			_, _, errAdd := output.Add(event)
			return errAdd
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(events, ",") != "text,done" || output.Text != "visible answer" {
			t.Fatalf("UI suggestions produced visible events: %v", events)
		}
		if err := output.Finish(Input{}); err != nil {
			t.Fatal(err)
		}
		for _, response := range [][]byte{output.Chat("gemini-4-argon"), output.Gemini(true, nil)} {
			for _, private := range []string{encoded, "private UI follow-up", "application/json+suggestions", "inlineData"} {
				if strings.Contains(string(response), private) {
					t.Fatalf("UI suggestion metadata leaked: %s", private)
				}
			}
		}
	}
}

func TestEnterpriseSuggestionsDoNotMaskTerminalFailures(t *testing.T) {
	suggestions, encoded := enterpriseSuggestionsFixture()
	for _, body := range []string{
		suggestions,
		suggestions + `{"answer":{"state":"FAILED"}}`,
		"[" + suggestions + "," + enterpriseDone + "]broken-tail",
		"data: " + suggestions + "\n\nevent: error\ndata: {}\n\n",
	} {
		var visible bool
		err := (&Parser{}).Parse(strings.NewReader(body), func(event Event) error {
			visible = visible || event.Type != "done"
			return nil
		})
		var failure *Error
		if !errors.As(err, &failure) || !failure.Scoped || visible {
			t.Fatalf("UI activity failed to fence replay: err=%v visible=%v", err, visible)
		}
		if strings.Contains(err.Error(), encoded) {
			t.Fatal("upstream UI payload leaked through an error")
		}
	}
}

func TestEnterpriseSuggestionsPreserveSiblingTextAndGrounding(t *testing.T) {
	_, encoded := enterpriseSuggestionsFixture()
	item, _ := json.Marshal(map[string]any{"answer": map[string]any{"state": "IN_PROGRESS", "replies": []map[string]any{{"groundedContent": map[string]any{
		"content":               map[string]any{"text": "visible answer", "inlineData": map[string]string{"mimeType": "application/json+suggestions", "data": encoded}},
		"textGroundingMetadata": map[string]any{"references": []map[string]any{{"documentMetadata": map[string]string{"uri": "https://example.com", "title": "Source"}}}},
	}}}}})
	output := NewOutput()
	err := (&Parser{}).Parse(strings.NewReader("["+string(item)+","+enterpriseDone+"]"), func(event Event) error {
		_, _, errAdd := output.Add(event)
		return errAdd
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "visible answer" || len(output.Sources) != 1 || output.Sources[0].URI != "https://example.com" {
		t.Fatalf("UI field discarded sibling content: text=%q sources=%v", output.Text, output.Sources)
	}
	if strings.Contains(string(output.Gemini(true, nil)), encoded) {
		t.Fatal("UI payload leaked beside sibling content")
	}
}

func TestEnterpriseActualAndUnknownMediaRemainRejected(t *testing.T) {
	for _, content := range []string{
		`{"file":{}}`,
		`{"file":{"mimeType":"application/pdf","fileId":"private-file-id"}}`,
		`{"inlineData":{}}`,
		`{"inlineData":{"mimeType":"image/png","data":"private-image-data"}}`,
		`{"inlineData":{"mimeType":"application/json","data":"private-json-data"}}`,
		`{"inlineData":{"mimeType":"application/json+suggestions","data":7}}`,
		`{"inlineData":{"mimeType":"application/json+suggestions","data":null}}`,
		`{"inlineData":{"mimeType":"application/json+suggestions"}}`,
		`{"inlineData":{"mimeType":"application/json+suggestions","data":"text","extra":"unknown"}}`,
		`{"file":{"fileId":"private-file-id"},"inlineData":{"mimeType":"application/json+suggestions","data":"text"}}`,
	} {
		body := `{"answer":{"state":"IN_PROGRESS","replies":[{"content":` + content + `}]}}` + enterpriseDone
		err := (&Parser{}).Parse(strings.NewReader(body), func(Event) error { return nil })
		var failure *Error
		if !errors.As(err, &failure) || failure.Kind != "unsupported_upstream_content" || !failure.Scoped {
			t.Fatalf("unsupported media accepted: content=%s err=%v", content, err)
		}
		if strings.Contains(err.Error(), "private-") {
			t.Fatal("private media value leaked through an error")
		}
	}
}
