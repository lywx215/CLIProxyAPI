package helps

import (
	"strings"
	"testing"
)

func TestBusinessAICodeStreamFraming(t *testing.T) {
	for _, test := range []struct{ name, contentType, body string }{
		{"sse", "text/event-stream", ": heartbeat\n\nevent: message\ndata: {\ndata: \"candidates\": []}\n\ndata: [DONE]\n\n"},
		{"array", "application/json", `[{"candidates":[]}]`},
		{"concatenated", "application/json", `{"candidates":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			count := 0
			err := ReadBusinessAICodeStream(strings.NewReader(test.body), test.contentType, func(body []byte) error {
				count++
				if !strings.Contains(string(body), "candidates") {
					t.Fatalf("framing leaked into payload: %s", body)
				}
				return nil
			})
			if err != nil || count != 1 {
				t.Fatalf("count=%d err=%v", count, err)
			}
		})
	}
	if err := ReadBusinessAICodeStream(strings.NewReader(`[{"candidates":[]}`), "application/json", func([]byte) error { return nil }); err == nil {
		t.Fatal("truncated array was accepted")
	}
}

func TestBusinessAICodePayloadRejectsDuplicateJSONKeys(t *testing.T) {
	for _, body := range []string{`{"model":"first","model":"second"}`, `{"entitlement":{"userTier":"first","userTier":"second"}}`, `{"contents":[{"parts":[],"parts":[]}]}`, `{} {}`} {
		if BusinessAICodeJSONIsUnambiguous([]byte(body)) {
			t.Fatalf("ambiguous JSON accepted: %s", body)
		}
	}
	if !BusinessAICodeJSONIsUnambiguous([]byte(`{"model":"model","contents":[{"parts":[{"text":"model model"}]}]}`)) {
		t.Fatal("valid request was rejected")
	}
}
