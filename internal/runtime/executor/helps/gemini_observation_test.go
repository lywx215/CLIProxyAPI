package helps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestGeminiObservationSparseUsageAndTimings(t *testing.T) {
	o := NewGeminiObservation("antigravity-enterprise", "alias")
	start := time.Unix(1, 0)
	o.Start(start, 2)
	o.Request([]byte(`{"model":"native-low"}`), "native")
	frames := []string{
		`{"modelVersion":"native","usageMetadata":{"promptTokenCount":5},"candidates":[{"content":{"parts":[{"text":" ","thought":true}]}}]}`,
		`{"modelVersion":"native","candidates":[{"content":{"parts":[{"text":"summary","thought":true},{"text":"answer"}]}}]}`,
		`{"modelVersion":"native","candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"candidatesTokenCount":0,"totalTokenCount":5}}`,
	}
	for i, f := range frames {
		if err := o.Observe([]byte(f), start.Add(time.Duration(i+1)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if !o.Ready() {
		t.Fatal("legal token-limit ending was not ready")
	}
	if err := o.Finish(false, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	v, d := o.Snapshot()
	if v.ModelMatch != "mapped" || v.FirstSummaryMS == nil || *v.FirstSummaryMS != 2 || v.FirstAnswerMS == nil || *v.FirstAnswerMS != 2 {
		t.Fatalf("observation = %+v", v)
	}
	if v.TailVerification != "unverified" || v.UsageResult != "official_partial" || v.Failed() {
		t.Fatalf("outcome = %+v", v)
	}
	if d.InputTokens != 5 || !d.TokenPresence.InputTokens || !d.TokenPresence.OutputTokens || d.OutputTokens != 0 || d.TokenPresence.ReasoningTokens {
		t.Fatalf("usage = %+v / %+v", d, d.TokenPresence)
	}
}

func TestGeminiObservationAssociatesOrdinaryTerminalUsageAndRetainsUnknownReason(t *testing.T) {
	o := NewGeminiObservation("antigravity", "m")
	frames := []string{
		`{"traceId":"a","response":{"candidates":[{"finishReason":"STOP"}]}}`,
		`{"traceId":"b","response":{"usageMetadata":{"totalTokenCount":999}}}`,
		`{"traceId":"a","response":{"usageMetadata":{}}}`,
		`{"response":{"usageMetadata":{"totalTokenCount":0}}}`,
	}
	for i, f := range frames {
		if err := o.Observe([]byte(f), time.Now()); err != nil {
			t.Fatal(err)
		}
		if o.Ready() != (i == 3) {
			t.Fatalf("premature completion after frame %d", i)
		}
	}
	if !bytes.Contains(o.TranslationPayload([]byte(frames[3])), []byte(`"usageMetadata"`)) {
		t.Fatal("correlated usage tail was hidden from native clients")
	}
	_, detail := o.Snapshot()
	if detail.TotalTokens != 0 || !detail.TokenPresence.TotalTokens {
		t.Fatal("foreign usage contaminated accounting")
	}
	o = NewGeminiObservation("antigravity-enterprise", "m")
	if e := o.Observe([]byte(`{"candidates":[{"finishReason":"NEW_REASON"}]}`), time.Now()); e == nil || o.Ready() {
		t.Fatal("unknown finish completed the response")
	}
	v, _ := o.Snapshot()
	if len(v.CandidateFinishes) != 1 || v.CandidateFinishes[0].Reason != "NEW_REASON" {
		t.Fatalf("original reason lost: %+v", v.CandidateFinishes)
	}
}

type geminiFixtureStatus int

func (e geminiFixtureStatus) Error() string   { return "rejected by fixture" }
func (e geminiFixtureStatus) StatusCode() int { return int(e) }

func TestObservedGeminiPreOutputRejectionDoesNotShareLaterDelivery(t *testing.T) {
	ctx := usage.WithObservedHTTPDelivery(t.Context())
	makeResponse := func(data string) *http.Response {
		return &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}
	}
	translate := func(raw []byte) ([][]byte, error) { return [][]byte{raw}, nil }
	first := NewUsageReporter(ctx, "antigravity-enterprise", "m", nil)
	stream := ForwardObservedGeminiStream(ctx, makeResponse("data: {\"error\":{\"code\":401}}\n\n"), first, ObservedGeminiStreamOptions{Translate: translate, DecodeError: func([]byte) error { return geminiFixtureStatus(401) }})
	var rejection error
	for c := range stream.Chunks {
		rejection = c.Err
	}
	v, _ := first.geminiObservation.Snapshot()
	if rejection == nil || coreexec.ReplayForbidden(rejection) || v.DeliveryResult != "not_tracked" || v.Error.Scope != "credential" || usage.ObservedDeliverySupported(ctx) {
		t.Fatalf("retryable attempt retained delivery: %+v %v", v, rejection)
	}
	second := NewUsageReporter(ctx, "antigravity-enterprise", "m", nil)
	stream = ForwardObservedGeminiStream(ctx, makeResponse("data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n"), second, ObservedGeminiStreamOptions{Translate: translate})
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
	}
	usage.FinishObservedDelivery(ctx, nil)
	usage.WaitObservedSettlement(ctx)
	v, _ = second.geminiObservation.Snapshot()
	if v.DeliveryResult != "written" || v.Failed() {
		t.Fatalf("delivery of the next attempt was not recorded: %+v", v)
	}
}

func TestGeminiRejectionRequiresEvidenceToClaimGeneration(t *testing.T) {
	for _, test := range []struct {
		raw     string
		started bool
	}{
		{`{"error":{"code":401},"candidates":[{}],"usageMetadata":{"totalTokenCount":12}}`, false},
		{`{"error":{"code":401},"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, true},
	} {
		r := NewUsageReporter(context.Background(), "antigravity-enterprise", "m", nil)
		r.geminiObservation.Start(time.Now(), 1)
		err := r.GeminiResponseFailure([]byte(test.raw), geminiFixtureStatus(401))
		if coreexec.ReplayForbidden(err) != test.started || coreexec.GenerationHasStarted(err) != test.started {
			t.Fatalf("wrong generation or retry fact: %+v", coreexec.FactsFromError(err))
		}
	}
}

func TestGeminiObservationTerminalAndCompatibility(t *testing.T) {
	for _, test := range []struct {
		name, provider, raw string
		pass                bool
	}{
		{"empty", "antigravity", `{}`, false},
		{"text EOF", "antigravity", `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, false},
		{"tool compatibility", "antigravity", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}],"usageMetadata":{"totalTokenCount":0}}`, true},
		{"enterprise has no inference", "antigravity-enterprise", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}],"usageMetadata":{"totalTokenCount":0}}`, false},
		{"multiple complete tools may infer EOF", "antigravity", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}},{"functionCall":{"name":"g","args":{}}}]}}],"usageMetadata":{"totalTokenCount":0}}`, true},
		{"prompt block", "antigravity-enterprise", `{"promptFeedback":{"blockReason":"SAFETY"}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := NewGeminiObservation(test.provider, "m")
			if e := o.Observe([]byte(test.raw), time.Now()); e != nil {
				t.Fatal(e)
			}
			e := o.Finish(true, false, nil, nil)
			if (e == nil) != test.pass {
				t.Fatalf("Finish = %v", e)
			}
			if e != nil && !coreexec.ReplayForbidden(e) {
				t.Fatal("truncated response may be replayed")
			}
		})
	}
	o := NewGeminiObservation("antigravity-enterprise", "m")
	o.Request([]byte(`{"generationConfig":{"candidateCount":2}}`), "m")
	_ = o.Observe([]byte(`{"candidates":[{"index":0,"finishReason":"STOP"}]}`), time.Now())
	if o.Ready() {
		t.Fatal("one candidate completed the whole response")
	}
	_ = o.Observe([]byte(`{"candidates":[{"index":1,"finishReason":"STOP"}]}`), time.Now())
	if !o.Ready() {
		t.Fatal("both candidates did not complete")
	}
	if e := o.Finish(true, false, http.Header{"Grpc-Status": []string{"13"}}, nil); e == nil {
		t.Fatal("nonzero trailer accepted")
	}
}

type geminiSegmentReader struct {
	segments [][]byte
	calls    int
	err      error
}

func (r *geminiSegmentReader) Read(p []byte) (int, error) {
	r.calls++
	if len(r.segments) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	v := r.segments[0]
	r.segments = r.segments[1:]
	return copy(p, v), nil
}

func TestGeminiEventStreamSplitTailAndBatchErrors(t *testing.T) {
	finish := []byte("data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n")
	usage := []byte("data: {\"usageMetadata\":{\"totalTokenCount\":0}}\n\n")
	r := &geminiSegmentReader{segments: [][]byte{finish, usage}, err: errors.New("must not read beyond completion")}
	o := NewGeminiObservation("antigravity", "m")
	result, err := ReadGeminiEvents(r, "text/event-stream", time.Now, o.Observe, o.Ready)
	if err != nil || !result.Stopped || result.EOF || r.calls != 2 {
		t.Fatalf("read = %+v, %v, calls %d", result, err, r.calls)
	}
	if err = o.Finish(result.EOF, result.Residual, nil, err); err != nil {
		t.Fatal(err)
	}
	_, d := o.Snapshot()
	if d.TokenPresence == nil || !d.TokenPresence.TotalTokens || d.TotalTokens != 0 {
		t.Fatal("official zero was lost")
	}
	bad := append(bytes.Clone(finish), []byte("data: {\"error\":{\"code\":503}}\n\n")...)
	o = NewGeminiObservation("antigravity-enterprise", "m")
	_, err = ReadGeminiEvents(strings.NewReader(string(bad)), "text/event-stream", time.Now, o.Observe, o.Ready)
	if err == nil {
		t.Fatal("tail error already read was ignored")
	}
}

func TestGeminiEventStreamMultilineUTF8AndResidual(t *testing.T) {
	// A valid multi-line JSON event is reconstructed before every observer.
	r := &geminiSegmentReader{segments: [][]byte{[]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你\"}]},\r\n"), []byte("data: \"finishReason\":\"STOP\"}]}\r\n\r\n")}}
	o := NewGeminiObservation("antigravity-enterprise", "m")
	result, err := ReadGeminiEvents(r, "text/event-stream", time.Now, o.Observe, o.Ready)
	if err != nil || !result.Stopped {
		t.Fatalf("multiline read = %+v %v", result, err)
	}
	r = &geminiSegmentReader{segments: [][]byte{[]byte("data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\ndata: {\"half\":")}}
	o = NewGeminiObservation("antigravity-enterprise", "m")
	result, err = ReadGeminiEvents(r, "text/event-stream", time.Now, o.Observe, o.Ready)
	if err != nil || !result.Residual || result.EOF {
		t.Fatalf("local stop flushed a fragment: %+v %v", result, err)
	}
}

func TestGeminiJSONAlreadyReadErrorAndPartialArray(t *testing.T) {
	o := NewGeminiObservation("antigravity-enterprise", "m")
	r := &geminiSegmentReader{segments: [][]byte{[]byte(`[{"candidates":[{"finishReason":"STOP"}]},{"error":{"code":503}}]`)}}
	if _, err := ReadGeminiEvents(r, "application/json", time.Now, o.Observe, o.Ready); err == nil {
		t.Fatal("buffered array error was ignored after a terminal candidate")
	}
	o = NewGeminiObservation("antigravity-enterprise", "m")
	r = &geminiSegmentReader{segments: [][]byte{[]byte(`[{"candidates":[{"finishReason":"STOP"}]}, {"half":`)}, err: errors.New("must not read")}
	result, err := ReadGeminiEvents(r, "application/json", time.Now, o.Observe, o.Ready)
	if err != nil || !result.Stopped || !result.Residual || result.EOF || r.calls != 1 {
		t.Fatalf("partial array stop: %+v %v calls=%d", result, err, r.calls)
	}
}

func TestGeminiObservationFinalPayloadAndIndependentTranslationFailure(t *testing.T) {
	o := NewGeminiObservation("antigravity-enterprise", "alias")
	o.Request([]byte(`{"model":"native-high","generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":false}}}`), "native")
	if e := o.Observe([]byte(`{"modelVersion":"native","candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}`), time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := o.Finish(false, false, nil, nil); e != nil {
		t.Fatal(e)
	}
	cause := errors.New("conversion fixture")
	err := o.LocalFailure("response_translation_failed", cause)
	err = o.Finish(false, false, nil, err)
	v, _ := o.Snapshot()
	if v.UpstreamResult != "terminal_observed" || v.Error.Stage != "translation" || !errors.Is(err, cause) || !coreexec.ReplayForbidden(err) {
		t.Fatalf("independent outcomes: %+v %v", v, err)
	}
	if v.ThinkingLevel == nil || *v.ThinkingLevel != "HIGH" || v.IncludeThoughts == nil || *v.IncludeThoughts || v.UpstreamRequestModel == nil || *v.UpstreamRequestModel != "native-high" {
		t.Fatalf("final request: %+v", v)
	}
}

func TestGeminiObservationRejectsMissingCandidateIndicesAndKeepsSparseUsage(t *testing.T) {
	o := NewGeminiObservation("antigravity-enterprise", "m")
	o.Request([]byte(`{"generationConfig":{"candidateCount":2}}`), "m")
	_ = o.Observe([]byte(`{"candidates":[{"index":5,"finishReason":"STOP"},{"index":6,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"toolUsePromptTokenCount":2,"totalTokenCount":11}}`), time.Now())
	if o.Ready() || o.Finish(true, false, nil, nil) == nil {
		t.Fatal("wrong candidate indices completed the response")
	}
	_, d := o.Snapshot()
	if d.InputTokens != 7 || !d.TokenPresence.InputTokens || d.TokenPresence.OutputTokens || d.TokenPresence.ReasoningTokens {
		t.Fatalf("sparse usage: %+v / %+v", d, d.TokenPresence)
	}
}
