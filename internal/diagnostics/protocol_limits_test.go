package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSemanticObservationLimitsAndUnknownFinish(t *testing.T) {
	var artifact bytes.Buffer
	good := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
	media := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + strings.Repeat("A", maxObservationBytes) + `"}}]},"finishReason":"STOP"}]}`
	depth := `{"candidates":[{"finishReason":"STOP"}],"extra":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`
	many := `{"candidates":[` + strings.Repeat(`{"finishReason":"STOP"},`, 64) + `{"finishReason":"STOP"}]}`
	for _, tc := range []struct {
		name, payload, result, origin, stage, class string
		parsed                                      any
		count                                       any
	}{
		{"media-limit", media, "unknown", "unknown", "unknown", "unknown", nil, nil},
		{"depth-limit", depth, "unknown", "unknown", "unknown", "unknown", nil, nil},
		{"candidate-limit", many, "unknown", "unknown", "unknown", "unknown", nil, nil},
		{"unknown-finish", strings.Replace(good, "STOP", "MALFORMED_FUNCTION_CALL", 1), "unknown", "unknown", "unknown", "unknown", true, float64(4)},
		{"future-finish", strings.Replace(good, "STOP", "FUTURE_PROVIDER_VALUE", 1), "unknown", "unknown", "unknown", "unknown", true, float64(4)},
		{"bad-json", `{"candidates":`, "incomplete", "upstream", "parse", "parse_error", false, nil},
		{"non-object", `[]`, "incomplete", "upstream", "parse", "parse_error", false, nil},
	} {
		for _, stream := range []bool{false, true} {
			for _, ending := range []string{"clean", "read-error", "cancelled", "error-frame"} {
				mode := "nonstream"
				if stream {
					mode = "stream"
				}
				t.Run(tc.name+"/"+mode+"/"+ending, func(t *testing.T) {
					sink := &recordSink{}
					e := NewEngine(ResourceConfig{}, "", nil, func() bool { return true }, sink.write)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					ctx, span := e.StartServer(ctx, nil, "fixture")
					if tc.parsed != false && !json.Valid([]byte(tc.payload)) {
						t.Fatal("limit/unsupported fixture is not legal JSON")
					}
					x := NewExchange(ctx, "gemini", stream, stream)
					x.Status(200)
					// A later limit must invalidate earlier partial aggregate counts.
					if stream {
						x.Upstream([]byte(good))
					}
					x.Upstream([]byte(tc.payload))
					if stream {
						x.Delivered([]byte(good))
					}
					x.Delivered([]byte(tc.payload))
					var readErr error
					wantResult := tc.result
					wantCount := tc.count
					if !stream && wantCount != nil {
						wantCount = float64(2)
					}
					if ending == "cancelled" {
						cancel()
						wantResult = "cancelled"
					}
					if ending == "read-error" {
						readErr = errors.New("fixture")
						wantResult = "error"
					}
					if ending == "error-frame" {
						x.Upstream([]byte(`{"error":{"code":500}}`))
						wantResult = "error"
					}
					x.ReadFinished(readErr)
					x.Finish(readErr)
					span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
					d := sink.records(t, "upstream.attempt_finished")[0].Data.(map[string]any)
					if d["resultClass"] != wantResult {
						t.Fatalf("result %v", d)
					}
					if ending == "clean" && (d["failureOrigin"] != tc.origin || d["failureStage"] != tc.stage || d["errorClass"] != tc.class || d["parserFinishOk"] != tc.parsed) {
						t.Errorf("classification %v", d)
					}
					if got := d["output"].(map[string]any)["ordinaryTextUtf8Bytes"]; got != wantCount {
						t.Errorf("aggregate=%v want=%v", got, wantCount)
					}
					if tc.parsed == nil && d["parserFinishOk"] != nil {
						t.Error("local limit attributed to parser")
					}
					if tc.parsed == true && (d["terminalSeen"] != true || d["errorClass"] == "parse_error") {
						t.Error("unknown string finish lost terminal evidence")
					}
					converted := sink.records(t, "response.converted")[0].Data.(map[string]any)
					if converted["resultClass"] == "success" || converted["output"].(map[string]any)["ordinaryTextUtf8Bytes"] != wantCount {
						t.Errorf("converted partial/unsupported observation %v", converted)
					}
					for _, line := range sink.lines {
						artifact.Write(line)
					}
				})
			}
		}
	}
	if path := os.Getenv("DIAG05_R2_LIMIT_RECORDS"); path != "" {
		if err := os.WriteFile(path, artifact.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSemanticNormalizationContentsAndAttemptScope(t *testing.T) {
	sink := &recordSink{}
	e := NewEngine(ResourceConfig{}, "", nil, func() bool { return true }, sink.write)
	ctx, span := e.StartServer(context.Background(), nil, "fixture")
	before := []byte(`{"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[]}],"model":"a"}`)
	after := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[]}],"model":"b","generationConfig":{}}}`)
	ObserveNormalized(ctx, before, after)
	ObserveNormalized(ctx, before, []byte(strings.Replace(string(before), `"parts":[]`, `"parts":[{"text":"b"}]`, 1)))
	first := ExecutorAttempt(ctx, "gemini")
	other := ExecutorAttempt(ctx, "claude")
	second := ExecutorAttempt(other, "antigravity")
	for i, c := range []context.Context{first, second} {
		a := c.Value(attemptKey{}).(attemptIdentity)
		if a.number != uint64(i+1) || a.scope != "conductor_gemini_family" {
			t.Errorf("ambiguous attempt scope: %v", a)
		}
	}
	if other.Value(attemptKey{}) != nil {
		t.Error("unobserved provider acquired attempt identity")
	}
	span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
	records := sink.records(t, "request.normalized")
	if len(records[0].Data.(map[string]any)["transformations"].([]any)) != 0 {
		t.Error("model/config change attributed to message zero")
	}
	changes := records[1].Data.(map[string]any)["transformations"].([]any)
	if len(changes) != 1 || changes[0].(map[string]any)["index"] != float64(1) {
		t.Errorf("wrong changed position: %v", changes)
	}
}
