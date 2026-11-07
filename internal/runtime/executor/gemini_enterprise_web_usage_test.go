package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

type webUsageCapture struct {
	id      string
	records chan usage.Record
}

func (c *webUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.AuthID == c.id {
		c.records <- record
	}
}

func webCaptureUsage(t *testing.T, id string) *webUsageCapture {
	t.Helper()
	c := &webUsageCapture{id: id, records: make(chan usage.Record, 16)}
	usage.RegisterNamedPlugin(t.Name(), c)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	return c
}

func (c *webUsageCapture) next(t *testing.T) usage.Record {
	t.Helper()
	select {
	case r := <-c.records:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("missing Web usage record")
		return usage.Record{}
	}
}

func webUsageFrames(complete bool) string {
	thought := `{"groundedContent":{"content":{"text":"reason summary","thought":true,"thoughtSignature":"private-not-tokenized"}}}`
	answer := `{"groundedContent":{"content":{"text":"hello 世界"}}}`
	first := fmt.Sprintf(`{"streamAssistResponse":{"answer":{"state":"IN_PROGRESS","replies":[%s,{"groundedContent":{"content":{"text":"hel"}}}]}}}`, thought)
	state := "IN_PROGRESS"
	if complete {
		state = "SUCCEEDED"
	}
	last := fmt.Sprintf(`{"streamAssistResponse":{"answer":{"state":%q,"replies":[%s,%s]}}}`, state, thought, answer)
	if !complete {
		last = `{"streamAssistResponse":{"answer":{"state":"IN_PROGRESS","replies":[{"groundedContent":{"content":{"text":"lo 世界"}}}]}}}`
	}
	return "[" + first + "," + last + "]"
}

func TestEnterpriseWebUsageStreamAndNonStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, complete := range []bool{true, false} {
			t.Run(fmt.Sprintf("stream=%t/complete=%t", stream, complete), func(t *testing.T) {
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "gemini-3.8-flash", Protocol: "gemini-enterprise-web"}},
					Params: map[string]any{"streamAssistRequest.query.parts": []any{map[string]any{"text": "final configured 文本"}}},
				}}}}
				e, a, ctx, f := phaseOneExecutor(t, cfg)
				a.ID = t.Name()
				capture := webCaptureUsage(t, a.ID)
				f.response = webUsageFrames(complete)
				req := phaseOneRequest("image/png", "application/pdf")
				var payload []byte
				if stream {
					result, err := e.ExecuteStream(ctx, a, req, phaseOneOptions())
					if err != nil {
						t.Fatal(err)
					}
					finishes, usageChunks, failures := 0, 0, 0
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							failures++
							continue
						}
						if gjson.GetBytes(chunk.Payload, "candidates.0.finishReason").String() == "STOP" {
							finishes++
						}
						if gjson.GetBytes(chunk.Payload, "usageEstimate").Exists() {
							usageChunks++
							payload = chunk.Payload
						}
					}
					if complete && (finishes != 1 || usageChunks != 1 || failures != 0) || !complete && (finishes != 0 || usageChunks != 0 || failures != 1) {
						t.Fatalf("terminal/usage/error = %d/%d/%d", finishes, usageChunks, failures)
					}
				} else {
					result, err := e.Execute(ctx, a, req, phaseOneOptions())
					if (err == nil) != complete {
						t.Fatalf("unexpected result: %v", err)
					}
					payload = result.Payload
				}
				record := capture.next(t)
				d := record.Detail
				counter := helps.NewEnterpriseTokenCounter([]byte(`{"query":{"text":"final configured 文本"}}`))
				counter.ObserveThought("reason summary")
				want := counter.Detail([]map[string]any{{"text": "hello 世界"}})
				if d.Estimate == nil || d.Estimate.InputTextTokens == nil || *d.Estimate.InputTextTokens != want.InputTokens || d.OutputTokens != want.OutputTokens || d.ReasoningTokens != want.ReasoningTokens {
					t.Fatalf("final text or deduplicated output counted incorrectly: %+v / %+v", d, want)
				}
				if d.TokenPresence.InputTokens || d.TokenPresence.TotalTokens || d.TokenPresence.CachedTokens || !d.TokenPresence.OutputTokens || !d.TokenPresence.ReasoningTokens || d.TotalTokens != 0 || d.InputTokens != 0 || !d.Estimate.Incomplete || d.Estimate.ResponseComplete != complete || record.Failed == complete {
					t.Fatalf("unknown media / failure semantics changed: %+v", record)
				}
				if complete && (gjson.GetBytes(payload, "usageMetadata.candidatesTokenCount").Int() != want.OutputTokens || gjson.GetBytes(payload, "usageMetadata.thoughtsTokenCount").Int() != want.ReasoningTokens || gjson.GetBytes(payload, "usageMetadata.promptTokenCount").Exists() || gjson.GetBytes(payload, "usageMetadata.totalTokenCount").Exists() || gjson.GetBytes(payload, "usageEstimate.source").String() != usage.LocalGemma3SentencePiece) {
					t.Fatalf("protocol usage disagrees with record: %s", payload)
				}
				if f.calls["lookupWidgetConfig"] != 1 || f.calls["widgetStreamAssist"] != 1 || f.calls["uploadFile"] != 2 || e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
					t.Fatalf("extra network call or retained lease: %v / %v", f.calls, e.runtime.Status())
				}
			})
		}
	}
}

func TestEnterpriseWebUsageSubmissionFailureAndCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/cancel=%t", stream, cancelRequest), func(t *testing.T) {
				e, a, ctx, f := phaseOneExecutor(t, nil)
				a.ID = t.Name()
				capture := webCaptureUsage(t, a.ID)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				f.entered = make(chan struct{})
				f.generateFailure = true
				if cancelRequest {
					f.block = make(chan struct{})
				}
				finished := make(chan error, 1)
				go func() {
					if stream {
						_, err := e.ExecuteStream(ctx, a, phaseOneRequest("image/png"), phaseOneOptions())
						finished <- err
					} else {
						_, err := e.Execute(ctx, a, phaseOneRequest("image/png"), phaseOneOptions())
						finished <- err
					}
				}()
				<-f.entered
				if cancelRequest {
					cancel()
				}
				var started interface{ GenerationStarted() bool }
				if err := <-finished; err == nil || !errors.As(err, &started) || !started.GenerationStarted() {
					t.Fatalf("missing non-replayable failure: %v", err)
				}
				r := capture.next(t)
				if !r.Failed || r.Detail.Estimate == nil || r.Detail.Estimate.InputTextTokens == nil || r.Detail.Estimate.ResponseComplete || r.Detail.TokenPresence.OutputTokens || r.Detail.TokenPresence.ReasoningTokens || r.Detail.TokenPresence.TotalTokens {
					t.Fatalf("failure fabricated output or completion: %+v", r)
				}
				if f.calls["widgetStreamAssist"] != 1 || !strings.Contains(string(f.final), "read these attachments") || e.runtime.Status()["inflight_bytes"] != int64(0) {
					t.Fatal("submission replayed or retained budget")
				}
			})
		}
	}
}

func TestEnterpriseWebUsageHistorySearchAndConfigSnapshot(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			cfg := historyTestConfig(true)
			e, a, ctx, f := phaseOneExecutor(t, cfg)
			a.ID = t.Name()
			capture := webCaptureUsage(t, a.ID)
			f.lookupConfig = phaseOneWithSearchSettings(phaseOneGoogleSearchSettings)
			f.response = webUsageFrames(true)
			req := phaseOneWithTools(historyTestRequest(phaseOneRequest("image/png", "application/pdf"), true), `"tools":[{"googleSearch":{}}]`)
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, ":uploadFile") {
					// A live configuration update must not replace this admitted
					// request's history mode, input limits or resource ownership.
					e.runtime.Configure(config.GeminiEnterpriseWebConfig{Enabled: false, MaxInflightBytes: 64 << 20,
						InputLimits: config.GeminiEnterpriseWebInputLimits{MaxContents: 1, MaxTextBytes: 1}})
				}
				return f.roundTrip(r)
			}))
			var response []byte
			if stream {
				result, err := e.ExecuteStream(ctx, a, req, phaseOneOptions())
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					if gjson.GetBytes(chunk.Payload, "usageEstimate").Exists() {
						response = chunk.Payload
					}
				}
			} else {
				result, err := e.Execute(ctx, a, req, phaseOneOptions())
				if err != nil {
					t.Fatal(err)
				}
				response = result.Payload
			}
			query := gjson.GetBytes(f.final, "streamAssistRequest.query.parts.0.text").String()
			for _, marker := range []string{"HISTORY-MODEL-CODE", "HISTORY-TOOL-CODE", "Answer using the historical attachment"} {
				if !strings.Contains(query, marker) {
					t.Fatal("history was lost before final token counting", marker)
				}
			}
			want := helps.NewEnterpriseWebTokenCounter(context.Background(), f.final)
			wantDetail := want.Detail(true)
			detail := capture.next(t).Detail
			if wantDetail.Estimate == nil || wantDetail.Estimate.InputTextTokens == nil || detail.Estimate == nil || detail.Estimate.InputTextTokens == nil || *detail.Estimate.InputTextTokens != *wantDetail.Estimate.InputTextTokens || !detail.Estimate.ResponseComplete || detail.TokenPresence.InputTokens || detail.TokenPresence.TotalTokens || !gjson.GetBytes(response, "usageEstimate.response_complete").Bool() {
				t.Fatal("history subset token accounting disagreed with final payload", detail)
			}
			if !gjson.GetBytes(f.final, "streamAssistRequest.toolsSpec.webGroundingSpec").IsObject() || f.calls["uploadFile"] != 2 || f.calls["widgetCreateSession"] != 1 || f.calls["widgetStreamAssist"] != 1 {
				t.Fatal("history/search lost a binding or replayed generation", f.calls)
			}
			if e.runtime.Enabled() || e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
				t.Fatal("shutdown or completed-request accounting changed", e.runtime.Status())
			}
			if _, _, err := e.runtime.Acquire(ctx, 1024); err == nil {
				t.Fatal("shutdown admitted new media")
			}
		})
	}
}
