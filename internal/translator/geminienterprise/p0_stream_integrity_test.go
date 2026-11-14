package geminienterprise

import (
	"errors"
	"strings"
	"testing"
)

func TestEnterpriseSSERejectsCorruptIgnoredFields(t *testing.T) {
	prefix := "data: " + enterpriseVisible + "\n\ndata: " + enterpriseDone + "\n\n"
	for _, field := range []string{":", "id:", "retry:", "event:"} {
		t.Run(field, func(t *testing.T) {
			body := []byte(prefix + field + " ")
			body = append(body, 0xff, '\n', '\n')
			parser := &Parser{}
			done := false
			err := parser.Parse(&fragmentReader{data: body, size: 1}, func(event Event) error {
				done = done || event.Type == "done"
				return nil
			})
			var failure *Error
			if !errors.As(err, &failure) || failure.Kind != "invalid_upstream_stream" || !failure.Scoped || done {
				t.Fatalf("corrupt SSE field was treated as successful metadata: err=%v done=%v", err, done)
			}
		})
	}
}

func TestEnterpriseSSEPreservesValidIgnoredFieldsAcrossUTF8Reads(t *testing.T) {
	metadata := ": 中文🙂\nid: request-前🙂\nretry: 1000\nevent: message\n"
	body := metadata + "data: " + enterpriseVisible + "\n\ndata: " + enterpriseDone + "\n\n: 完成🙂\nid: final-前🙂\n\n"
	parser := &Parser{}
	var text strings.Builder
	doneCount := 0
	err := parser.Parse(&fragmentReader{data: []byte(body), size: 1}, func(event Event) error {
		text.WriteString(event.Text)
		if event.Type == "done" {
			doneCount++
		}
		return nil
	})
	if err != nil || text.String() != "前🙂引用" || doneCount != 1 {
		t.Fatalf("valid SSE metadata or fragmented UTF-8 changed: err=%v text=%q done=%d", err, text.String(), doneCount)
	}
}
