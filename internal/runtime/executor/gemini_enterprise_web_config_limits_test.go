package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestEnterpriseWebConfiguredDefaultsThroughExecutor(t *testing.T) {
	history := func(count int) []byte {
		contents := make([]map[string]any, count)
		for i := range contents {
			role := "model"
			if i%2 != count%2 {
				role = "user"
			}
			contents[i] = map[string]any{"role": role, "parts": []any{map[string]string{"text": "x"}}}
		}
		contents[count-1]["parts"] = []any{map[string]any{"inlineData": map[string]string{"mimeType": "text/plain", "data": "YQ=="}}}
		body, _ := json.Marshal(map[string]any{"contents": contents})
		return body
	}
	for _, tc := range []struct {
		name        string
		payload     func() []byte
		wantLimit   contract.InputLimitName
		wantMaximum int64
		wantUploads int
	}{
		{"200-contents", func() []byte { return history(200) }, "", 0, 1},
		{"201-contents", func() []byte { return history(201) }, contract.LimitHistoryContents, 200, 0},
		{"4-MiB-text", func() []byte {
			return bytes.Replace(phaseOneRequest("text/plain").Payload, []byte("read these attachments"), []byte(strings.Repeat("x", 4<<20)), 1)
		}, "", 0, 1},
		{"text-over-limit", func() []byte {
			return bytes.Replace(phaseOneRequest("text/plain").Payload, []byte("read these attachments"), []byte(strings.Repeat("x", (4<<20)+1)), 1)
		}, contract.LimitHistoryTextBytes, 4 << 20, 0},
		{"12-attachments", func() []byte { return phaseOneRequest(strings.Fields(strings.Repeat("text/plain ", 12))...).Payload }, "", 0, 12},
		{"13-attachments", func() []byte { return phaseOneRequest(strings.Fields(strings.Repeat("text/plain ", 13))...).Payload }, contract.LimitAttachments, 12, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, a, ctx, upstream := phaseOneExecutor(t, historyTestConfig(true))
			request := exec.Request{Model: "gemini-3.8-flash", Payload: tc.payload()}
			_, err := e.Execute(ctx, a, request, phaseOneOptions())
			if tc.wantLimit != "" {
				var failure *web.Failure
				if !errors.As(err, &failure) || failure.Code != 413 || failure.Limit == nil || failure.Limit.Name != tc.wantLimit || failure.Limit.Maximum != tc.wantMaximum {
					t.Fatalf("configured limit missing: %#v (%v)", failure, err)
				}
				if len(upstream.calls) != 0 {
					t.Fatal("invalid input reached upstream", upstream.calls)
				}
			} else if err != nil || upstream.calls["widgetStreamAssist"] != 1 || upstream.calls["uploadFile"] != tc.wantUploads {
				t.Fatal("default boundary failed", err, upstream.calls)
			}
			if e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
				t.Fatal("input limit or history reservation leaked")
			}
		})
	}
}
