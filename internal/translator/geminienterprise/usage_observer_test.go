package geminienterprise

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnterpriseUsageObservationPrecedesVisibility(t *testing.T) {
	raw := `[{"usageMetadata":{"thoughtsTokenCount":10},"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"思考🙂"}},{"content":{"text":"answer"}},{"content":{"thought":true,"text":" more"}}]}},{"answer":{"state":"SUCCEEDED"}},{"usageMetadata":{"thoughtsTokenCount":50}}]`
	for _, include := range []bool{false, true} {
		var thoughts strings.Builder
		var snapshots []string
		var events []Event
		p := &Parser{IncludeThoughts: include, Thought: func(s string) { thoughts.WriteString(s) }, Usage: func(raw json.RawMessage) { snapshots = append(snapshots, string(raw)) }}
		if err := p.Parse(strings.NewReader(raw), func(e Event) error { events = append(events, e); return nil }); err != nil {
			t.Fatal(err)
		}
		if thoughts.String() != "思考🙂 more" || len(snapshots) != 2 {
			t.Fatal("observation was tied to visibility or terminal framing")
		}
		visible := 0
		for _, e := range events {
			if e.Type == "thought" {
				visible++
			}
		}
		if include && visible != 2 || !include && visible != 0 {
			t.Fatal("observation changed visibility")
		}
	}
}

func TestEnterpriseUsageObservationDoesNotSearchReplyPayloads(t *testing.T) {
	count := 0
	p := &Parser{Usage: func(json.RawMessage) { count++ }}
	raw := `[{"answer":{"state":"IN_PROGRESS","usageMetadata":{"thoughtsTokenCount":100},"replies":[{"content":{"text":"{\"usageMetadata\":{\"thoughtsTokenCount\":200}}"}}]}},{"answer":{"state":"SUCCEEDED"}}]`
	if err := p.Parse(strings.NewReader(raw), func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("user-controlled reply content became usage")
	}
}
