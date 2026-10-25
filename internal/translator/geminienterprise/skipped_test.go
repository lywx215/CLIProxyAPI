package geminienterprise

import (
	"errors"
	"io"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func TestEnterpriseSkippedReasonsAcrossStreamEncodings(t *testing.T) {
	terminal := `{"answer":{"state":"SKIPPED","assistSkippedReasons":["CUSTOMER_POLICY_VIOLATION"],"customerPolicyEnforcementResult":{"bannedPhrases":["synthetic-private-policy"]}},"assistToken":"synthetic-private-token"}`
	for _, prefix := range []string{"", `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"partial"}}]}}`} {
		for _, encoding := range []string{"array", "ndjson", "sse"} {
			t.Run(encoding+map[bool]string{false: "/before", true: "/after"}[prefix != ""], func(t *testing.T) {
				body := terminal
				switch encoding {
				case "array":
					if prefix != "" {
						body = prefix + "," + body
					}
					body = "[" + body + "]"
				case "ndjson":
					body = prefix + "\n" + body
				case "sse":
					body = "data: " + body + "\n\n"
					if prefix != "" {
						body = "data: " + prefix + "\n\n" + body
					}
				}
				for _, size := range []int{1, 7, 4096} {
					parser := &Parser{}
					done := false
					err := parser.Parse(&fragmentReader{data: []byte(body), size: size}, func(event Event) error { done = done || event.Type == "done"; return nil })
					var skipped *ge.SkippedError
					if !errors.As(err, &skipped) || skipped.SkipReasons()[0] != "CUSTOMER_POLICY_VIOLATION" || skipped.GenerationStarted() != (prefix != "") || done {
						t.Fatalf("skip state was misclassified: %v done=%v", err, done)
					}
					if strings.Contains(err.Error(), "synthetic-private") {
						t.Fatal("skip diagnostics leaked policy or transport data")
					}
				}
			})
		}
	}
}

type skippedReadError struct{ data []byte }

func (r *skippedReadError) Read(dst []byte) (int, error) {
	n := copy(dst, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func TestEnterpriseSkippedTerminalSurvivesReadError(t *testing.T) {
	parser := &Parser{ReadError: func(error, bool) error { return errors.New("transport error must not replace a parsed rejection") }}
	err := parser.Parse(&skippedReadError{data: []byte(`{"answer":{"state":"SKIPPED","assistSkippedReasons":["NON_ASSIST_SEEKING_QUERY_IGNORED"]}}`)}, func(Event) error { return nil })
	var skipped *ge.SkippedError
	if !errors.As(err, &skipped) || skipped.SkipReasons()[0] != "NON_ASSIST_SEEKING_QUERY_IGNORED" {
		t.Fatalf("parsed skip rejection was lost: %v", err)
	}
}
