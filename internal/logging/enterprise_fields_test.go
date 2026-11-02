package logging

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestEnterpriseFormatterProjectsOnlyPrimitiveDiagnostics(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Message = "Enterprise request failed"
	entry.Data = log.Fields{
		"provider": "gemini-enterprise", "stage": "upstream_body\ninjected", "status": 400,
		"google_reason": map[string]any{"secret": "private-value"}, "upstream": map[string]any{"secret": "private-value"},
		"reason_class": "invalid_type", "upstream_http_status": 0,
		"configured_fields": strings.Repeat("private-value", 1000),
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	text := string(formatted)
	if !strings.Contains(text, `stage="upstream_body\ninjected"`) || !strings.Contains(text, `upstream_http_status=unknown`) || !strings.Contains(text, `configured_fields="truncated"`) {
		t.Fatalf("formatter dropped safe observations: %s", text)
	}
	if strings.Contains(text, "private-value") || strings.Count(text, "\n") != 1 {
		t.Fatalf("formatter printed an object or unescaped data: %s", text)
	}
	entry.Data["provider"] = "other"
	formatted, _ = (&LogFormatter{}).Format(entry)
	if strings.Contains(string(formatted), "reason_class=") {
		t.Fatal("Enterprise projection changed another provider's log format")
	}
}
