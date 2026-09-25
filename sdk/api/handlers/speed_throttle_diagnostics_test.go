package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestDIAG05ThrottleStreamWaits(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancelled"}[cancelled], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var events []map[string]any
				e := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
					var r map[string]any
					if err := json.Unmarshal(b[6:], &r); err != nil {
						return err
					}
					if r["event"] == "throttle.finished" {
						events = append(events, r["data"].(map[string]any))
					}
					return nil
				})
				ctx, span := e.StartServer(context.Background(), nil, "fixture")
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				throttler := NewRequestThrottler(&config.SDKConfig{SpeedThrottle: config.SpeedThrottleConfig{Enabled: true, MinTokensPerSecond: 10, MaxTokensPerSecond: 10, MinFirstTokenDelayMs: 100, MaxFirstTokenDelayMs: 100}})
				finish := ObserveRequestThrottle(ctx, throttler)
				start := time.Now()
				done := make(chan bool)
				go func() {
					done <- throttler.ThrottleFirstChunkWithPayload(ctx, start, []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop"}]}}]}`))
				}()
				synctest.Wait()
				if cancelled {
					cancel()
				}
				ok := <-done
				if ok == cancelled {
					t.Fatal("business cancellation changed")
				}
				if ok {
					if !throttler.ThrottleChunk(ctx, []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefgh"}]}}]}`)) {
						t.Fatal("chunk failed")
					}
					if !throttler.ThrottleChunk(ctx, []byte(`{"usageMetadata":{"candidatesTokenCount":7,"thoughtsTokenCount":80}}`)) {
						t.Fatal("usage tail failed")
					}
				}
				finish()
				span.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
				if len(events) != 1 {
					t.Fatal("event missing")
				}
				d := events[0]
				wantWait, wantTokens := float64(600), float64(6)
				if cancelled {
					wantWait = 0
					wantTokens = 4
				}
				if d["tokenSource"] != "estimated" || d["tokenCount"] != wantTokens || d["actualWaitMs"] != wantWait || d["cancelled"] != cancelled {
					t.Fatalf("actual throttle evidence %v", d)
				}
				if time.Since(start) != time.Duration(wantWait)*time.Millisecond {
					t.Fatal("business wait changed")
				}
			})
		})
	}
}

func TestDIAG05ThrottleTokenProvenance(t *testing.T) {
	for _, tc := range []struct {
		body, source string
		want         int
	}{
		{`{"usageMetadata":{"candidatesTokenCount":7,"thoughtsTokenCount":80}}`, "provider_output", 87},
		{`{"usageMetadata":{"candidatesTokenCount":7}}`, "provider_candidate", 7},
		{`{"usage":{"completion_tokens":87,"completion_tokens_details":{"reasoning_tokens":80}}}`, "provider_output", 87},
		{`{"usage":{"output_tokens":0},"candidates":[{"content":{"parts":[{"text":"abcdefgh"}]}}]}`, "estimated", 2},
		{`{"candidates":[{"content":{"parts":[{"text":"abcdefgh"}]}}]}`, "estimated", 2},
	} {
		n, source := estimateNonStreamingTokensWithSource([]byte(tc.body))
		if n != tc.want || source != tc.source || EstimateNonStreamingTokens([]byte(tc.body)) != n {
			t.Fatalf("provenance %d/%s", n, source)
		}
	}
}
