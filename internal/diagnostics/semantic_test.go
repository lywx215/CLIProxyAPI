package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestSemanticLifecycle(t *testing.T) {
	for _, mode := range []string{"off", "mid-enable", "on", "interrupted", "sink-error", "sink-panic", "unknown-sink"} {
		t.Run(mode, func(t *testing.T) {
			var enabled atomic.Bool
			enabled.Store(mode != "off" && mode != "mid-enable")
			sink := &recordSink{}
			e := NewEngine(ResourceConfig{}, "", nil, enabled.Load, func(b []byte) error {
				if bytes.Contains(b, []byte(`"recordKind":"debug"`)) {
					if mode == "sink-error" {
						return errors.New("private failure")
					}
					if mode == "sink-panic" {
						panic("private failure")
					}
				}
				return sink.write(b)
			})
			if mode == "unknown-sink" {
				e.MarkSinkLossUnknown()
			}
			ctx, s := e.StartServer(context.Background(), nil, "fixture")
			if mode == "mid-enable" {
				enabled.Store(true)
			}
			if mode == "interrupted" {
				NotifyDebugDisabled()
				enabled.Store(false)
				enabled.Store(true)
			}
			ObserveNormalized(ctx, []byte(`{"contents":[]}`), []byte(`{"contents":[]}`))
			s.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
			ObserveNormalized(ctx, []byte(`{"contents":[]}`), []byte(`{"contents":[]}`))
			r := sink.records(t, "diag.server")[0]
			d := r.Data.(map[string]any)["coverage"].(map[string]any)
			wantCapture := "enabled_throughout"
			wantSeq := float64(2)
			switch mode {
			case "off", "mid-enable":
				wantCapture = "none"
				wantSeq = 1
			case "interrupted":
				wantCapture = "interrupted"
				wantSeq = 1
			}
			if d["debugCapture"] != wantCapture || d["expectedLastLogSeq"] != wantSeq {
				t.Fatalf("coverage %v", d)
			}
			if mode == "sink-error" || mode == "sink-panic" {
				if d["droppedForSpan"] != float64(1) {
					t.Fatalf("loss missing %v", d)
				}
			}
			if mode == "unknown-sink" && d["droppedForSpan"] != nil {
				t.Fatal("unacknowledged sink invented zero")
			}
			if float64(r.LogSeq) != wantSeq {
				t.Fatal("late event or split sequence")
			}
		})
	}
}

func TestSemanticTruncationAndSealing(t *testing.T) {
	sink := &recordSink{}
	e := NewEngine(ResourceConfig{}, "", nil, func() bool { return true }, sink.write)
	ctx, s := e.StartServer(context.Background(), nil, "fixture")
	// Private emission test: oversize is replaced, never passed through.
	semantic(ctx, "request.normalized", struct {
		Unused string `json:"unused"`
	}{strings.Repeat("x", 6000)})
	s.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
	if len(sink.records(t, "diag.truncated")) != 1 {
		t.Fatal("stub missing")
	}
	d := sink.records(t, "diag.server")[0].Data.(map[string]any)["coverage"].(map[string]any)
	if d["truncatedEvents"] != float64(1) || d["expectedLastLogSeq"] != float64(2) {
		t.Fatalf("coverage %v", d)
	}
}

func TestSemanticBoundedParsingAndCumulativeUsage(t *testing.T) {
	sink := &recordSink{}
	e := NewEngine(ResourceConfig{}, "", nil, func() bool { return true }, sink.write)
	ctx, s := e.StartServer(context.Background(), nil, "fixture")
	x := NewExchange(ctx, "openai", true, true)
	x.Status(200)
	x.Upstream([]byte(`{"candidates":[{"content":{"parts":[{"text":"text"},{"text":"thought","thought":true}]},"finishReason":"STOP"}]}`))
	for range 3 {
		x.Upstream([]byte(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7,"thoughtsTokenCount":80,"totalTokenCount":92}}`))
	}
	x.Delivered([]byte(`data: {"choices":[{"delta":{"content":"text"},"finish_reason":"stop"}],"usage":{"completion_tokens":87,"completion_tokens_details":{"reasoning_tokens":80}}}`))
	x.ReadFinished(nil)
	x.Finish(nil)
	s.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
	a := sink.records(t, "upstream.attempt_finished")[0].Data.(map[string]any)
	u := a["usage"].(map[string]any)
	if u["outputTotal"].(map[string]any)["value"] != float64(87) {
		t.Fatal("cumulative usage summed")
	}
	d := sink.records(t, "response.converted")[0].Data.(map[string]any)["deliveredUsage"].(map[string]any)
	if d["outputTotal"].(map[string]any)["value"] != float64(87) || d["reasoningIncludedInOutput"] != true {
		t.Fatal("reasoning double count")
	}
	for _, bad := range []string{`{"x":`, strings.Repeat("[", 10000) + strings.Repeat("]", 10000), strings.Repeat(" ", maxObservationBytes+1)} {
		if boundedJSON([]byte(bad)) {
			t.Fatal("unbounded/malformed accepted")
		}
	}
}

func TestSemanticTimingUsesServerOriginAndNewExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := &recordSink{}
		e := NewEngine(ResourceConfig{}, "", nil, func() bool { return true }, sink.write)
		ctx, s := e.StartServer(context.Background(), nil, "fixture")
		<-time.After(5 * time.Second)
		x := NewExchange(ctx, "gemini", false, false)
		<-time.After(2 * time.Second)
		x.Upstream([]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`))
		x.ReadFinished(nil)
		x.Finish(nil)
		y := NewExchange(ctx, "gemini", false, false)
		y.ReadFinished(nil)
		y.Finish(nil)
		s.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
		r := sink.records(t, "upstream.attempt_finished")
		a := r[0].Data.(map[string]any)
		b := r[1].Data.(map[string]any)
		if a["totalMs"] != float64(2000) || a["timing"].(map[string]any)["firstEffectiveOutputMs"] != float64(7000) {
			t.Fatalf("origin %v", a)
		}
		if b["timing"].(map[string]any)["firstEffectiveOutputMs"] != nil || a["timing"].(map[string]any)["firstUpstreamByteMs"] != nil {
			t.Fatal("borrowed unobserved timing")
		}
	})
}

func TestSemanticMalformedSSEIsNotRecursive(t *testing.T) {
	o := newObservation("gemini")
	o.observe([]byte(strings.Repeat("data:", 100000)+"{}"), "upstream", true)
	if o.parsed {
		t.Fatal("nested SSE prefixes accepted")
	}
	scalar := newObservation("gemini")
	scalar.observe([]byte("1"), "upstream", false)
	if scalar.parsed {
		t.Fatal("scalar response accepted")
	}
}
