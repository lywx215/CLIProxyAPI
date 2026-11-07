package logging

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsWebLimitAndStageWithoutPayloads(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Level = log.WarnLevel
	entry.Message = "Web request rejected before session creation"
	entry.Data = log.Fields{
		"request_id": "12345678", "provider": "gemini-enterprise-web", "stage": "preflight",
		"status": 413, "reason": "history_limit_exceeded", "field": "contents[4].parts[0].text",
		"limit_name": "history_text_bytes", "observed": int64(1048577), "maximum": int64(1048576), "observed_at_least": true,
		"contents": 5, "logical_contents": 3, "attachments": 2, "attachment_bytes": 300,
		"filtered_thoughts": 1, "filtered_signatures": 2,
		"body": "PRIVATE-TEXT", "source": "PRIVATE-SOURCE", "cookies": "PRIVATE-COOKIE", "payload": "PRIVATE-BASE64",
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	line := string(formatted)
	for _, want := range []string{`[12345678]`, `stage="preflight"`, `status=413`, `field="contents[4].parts[0].text"`, `limit_name="history_text_bytes"`, `observed=1048577`, `maximum=1048576`, `observed_at_least=true`, `logical_contents=3`, `attachment_bytes=300`, `filtered_signatures=2`} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %s", want, line)
		}
	}
	if strings.Contains(line, "PRIVATE-") {
		t.Fatal("payload escaped log field projection")
	}
}
