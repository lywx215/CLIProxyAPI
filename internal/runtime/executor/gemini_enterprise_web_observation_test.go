package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	"github.com/tidwall/gjson"
)

const webPartialAnswer = `{"streamAssistResponse":{"answer":{"state":"IN_PROGRESS","replies":[{"groundedContent":{"content":{"text":"first answer"}}}]}}}`

func webObservationHTTP(t *testing.T, stream bool) (*gin.Engine, *http.Request, *GeminiEnterpriseWebExecutor, *phaseOneFixture, *webUsageCapture) {
	t.Helper()
	m, _, e, ctx, upstream := phaseOneManager(t)
	capture := webCaptureUsage(t, "web-phase-one-cookie")
	router := gin.New()
	router.Use(middleware.EnterpriseWebBodyAdmission(m))
	h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&e.cfg.SDKConfig, m))
	router.POST("/v1beta/models/*action", h.GeminiHandler)
	action := "generateContent"
	if stream {
		action = "streamGenerateContent?alt=sse"
	}
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash:"+action,
		bytes.NewReader(phaseOneRequest("image/png").Payload))
	return router, request.WithContext(logging.WithRequestID(ctx, "12345678")), e, upstream, capture
}

func checkWebObservationFinished(t *testing.T, e *GeminiEnterpriseWebExecutor, upstream *phaseOneFixture, capture *webUsageCapture) {
	t.Helper()
	if upstream.calls["widgetStreamAssist"] != 1 || upstream.calls["widgetCreateSession"] != 1 ||
		e.runtime.Status()["active_requests"] != 0 || e.runtime.Status()["inflight_bytes"] != int64(0) {
		t.Fatalf("unexpected replay or retained resources: %v / %v", upstream.calls, e.runtime.Status())
	}
	select {
	case <-capture.records:
		t.Fatal("duplicate Web usage record")
	default:
	}
}

func TestEnterpriseWebHTTPGenerationObservations(t *testing.T) {
	rpc := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"PRIVATE-RAW-ERROR","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXCEEDED","metadata":{"secret":"PRIVATE-METADATA"}}]}}`
	for _, tc := range []struct {
		name, body, result, code, state string
		stream, output                  bool
		status                          int
	}{
		{"before output", "[" + rpc + "]", "rejected", "upstream_rpc_error", "", true, false, 429},
		{"partial RPC error", "[" + webPartialAnswer + "," + rpc + "]", "rejected", "upstream_rpc_error", "IN_PROGRESS", true, true, 200},
		{"truncated stream", "[" + webPartialAnswer + ",", "incomplete", "stream_truncated_json", "IN_PROGRESS", true, true, 200},
		{"no terminal state", "[" + webPartialAnswer + "]", "incomplete", "stream_missing_terminal_success", "IN_PROGRESS", true, true, 200},
		{"failed answer", `[{"streamAssistResponse":{"answer":{"state":"FAILED"}}}]`, "rejected", "upstream_answer_rejected", "FAILED", true, false, 502},
		{"skipped answer", `[{"streamAssistResponse":{"answer":{"state":"SKIPPED"}}}]`, "rejected", "upstream_answer_rejected", "SKIPPED", true, false, 502},
		{"cancelled upstream answer", `[{"streamAssistResponse":{"answer":{"state":"CANCELLED"}}}]`, "rejected", "upstream_answer_rejected", "CANCELLED", true, false, 502},
		{"corrupt successful tail", webUsageFrames(true) + `{}`, "incomplete", "stream_trailing_data", "SUCCEEDED", true, true, 200},
		{"complete stream", webUsageFrames(true), "complete", "", "SUCCEEDED", true, true, 200},
		{"aggregate truncation", "[" + webPartialAnswer + ",", "incomplete", "stream_truncated_json", "IN_PROGRESS", false, true, 502},
		{"complete aggregate", webUsageFrames(true), "complete", "", "SUCCEEDED", false, true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, req, executor, upstream, capture := webObservationHTTP(t, tc.stream)
			upstream.response = tc.body
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			record := capture.next(t)
			o := record.Observation
			if o == nil || o.Web == nil || !o.Valid() || record.Failed != (tc.code != "") || record.TraceID != "12345678" ||
				o.UpstreamResult != tc.result || o.DeliveryResult != "written" || o.Web.ClientHTTPStatus != tc.status ||
				o.Web.UpstreamHTTPStatus != 200 || o.Web.AnswerState != tc.state || o.Web.OutputStarted != tc.output ||
				!o.Web.Submitted || recorder.Code != tc.status || o.UpstreamTTFBMS == nil ||
				o.Web.Attachments != 1 || len(o.Web.StagesMS) != 7 {
				t.Fatalf("inconsistent execution facts: status=%d failed=%t observation=%+v", recorder.Code, record.Failed, o)
			}
			body := recorder.Body.String()
			if strings.Contains(body, "PRIVATE-") || strings.Contains(record.Fail.Body, "PRIVATE-") {
				t.Fatal("raw upstream error escaped projection")
			}
			if tc.code == "" {
				if o.Error != nil || o.TailVerification != "verified" || o.FirstSummaryMS == nil || o.FirstAnswerMS == nil ||
					strings.Count(body, `"finishReason":"STOP"`) != 1 || strings.Contains(body, "event: error") {
					t.Fatal("successful terminal or first output observations lost", o)
				}
			} else {
				if o.Error == nil || o.Error.Code != tc.code || o.Error.Stage != "parse" || o.Error.Replay != "unsafe" ||
					o.TailVerification == "verified" || strings.Contains(body, `"finishReason":"STOP"`) {
					t.Fatal("failure lost or became success", o)
				}
				if tc.code == "upstream_rpc_error" && (o.Web.GoogleStatus != "RESOURCE_EXHAUSTED" || o.Web.GoogleReason != "QUOTA_EXCEEDED") {
					t.Fatal("safe RPC status or reason lost", o.Web)
				}
				if tc.stream && tc.status == 200 {
					if strings.Count(body, "event: error\n") != 1 || !strings.Contains(body, `"code":"`+tc.code+`"`) {
						t.Fatalf("expected one safe SSE error: %s", body)
					}
				} else if gjson.Get(body, "error.code").String() != tc.code {
					t.Fatalf("failure before commit lost its reason: %s", body)
				}
			}
			checkWebObservationFinished(t, executor, upstream, capture)
		})
	}
}

type webObservedResponseWriter struct {
	*httptest.ResponseRecorder
	failOn []byte
	wrote  chan struct{}
	once   sync.Once
}

func (w *webObservedResponseWriter) Write(p []byte) (int, error) {
	if len(w.failOn) > 0 && bytes.Contains(p, w.failOn) {
		return 0, errors.New("PRIVATE-WRITE-ERROR")
	}
	n, err := w.ResponseRecorder.Write(p)
	if w.wrote != nil && bytes.Contains(p, []byte("first answer")) {
		w.once.Do(func() { close(w.wrote) })
	}
	return n, err
}

func TestEnterpriseWebWriteFailureDoesNotChangeUpstreamSuccess(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "stream"
		if !stream {
			name = "aggregate"
		}
		t.Run(name, func(t *testing.T) {
			router, req, executor, upstream, capture := webObservationHTTP(t, stream)
			writer := &webObservedResponseWriter{ResponseRecorder: httptest.NewRecorder(), failOn: []byte(`"finishReason":"STOP"`)}
			router.ServeHTTP(writer, req)
			r := capture.next(t)
			o := r.Observation
			if !r.Failed || o == nil || !o.Valid() || o.UpstreamResult != "complete" || o.TailVerification != "verified" ||
				o.DeliveryResult != "write_failed" || o.Web.ClientHTTPStatus != 200 || o.Error == nil ||
				o.Error.Stage != "delivery" || o.Error.Code != "client_write_failed" || strings.Contains(r.Fail.Body, "PRIVATE-") {
				t.Fatalf("write failure corrupted the upstream outcome: %+v / %+v", r, o)
			}
			checkWebObservationFinished(t, executor, upstream, capture)
		})
	}
}

type webBlockedBody struct {
	*strings.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *webBlockedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err != io.EOF {
		return n, err
	}
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *webBlockedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestEnterpriseWebStreamCancellationReleasesRequest(t *testing.T) {
	router, req, executor, upstream, capture := webObservationHTTP(t, true)
	body := &webBlockedBody{Reader: strings.NewReader("[" + webPartialAnswer + ","), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(context.WithValue(ctx, "cliproxy.roundtripper", phaseOneTransport(func(r *http.Request) (*http.Response, error) {
		res, err := upstream.roundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/widgetStreamAssist") {
			_ = res.Body.Close()
			res.Body = body
		}
		return res, err
	})))
	writer := &webObservedResponseWriter{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{})}
	finished := make(chan struct{})
	go func() {
		router.ServeHTTP(writer, req)
		close(finished)
	}()
	select {
	case <-writer.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("stream buffered the partial answer")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled stream did not settle")
	}
	r := capture.next(t)
	o := r.Observation
	if !r.Failed || o == nil || !o.Valid() || o.UpstreamResult == "complete" || o.Web.ClientHTTPStatus != 200 ||
		o.DeliveryResult != "cancelled" || o.Error == nil || o.Error.Code != "client_cancelled" ||
		strings.Contains(writer.Body.String(), `"finishReason":"STOP"`) {
		t.Fatalf("cancelled delivery misclassified: %+v / %+v", r, o)
	}
	checkWebObservationFinished(t, executor, upstream, capture)
}

func TestEnterpriseWebRecordsGenerationCallInDiagnostics(t *testing.T) {
	router, req, _, _, capture := webObservationHTTP(t, true)
	var mu sync.Mutex
	var lines [][]byte
	engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, nil, func(line []byte) error {
		mu.Lock()
		lines = append(lines, bytes.Clone(line))
		mu.Unlock()
		return nil
	})
	ctx, span := engine.StartServer(req.Context(), nil, "12345678")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req.WithContext(ctx))
	_ = capture.next(t)
	span.Finish(diagnostics.ServerData{WireStatus: &recorder.Code, HeadersCommitted: true, EndReason: "finished", DeliveryState: "local_finished"})
	mu.Lock()
	defer mu.Unlock()
	calls := map[string]int{}
	for _, line := range lines {
		body := bytes.TrimPrefix(line, []byte("@diag "))
		if gjson.GetBytes(body, "event").String() == "diag.call" {
			calls[gjson.GetBytes(body, "data.callKind").String()]++
		}
	}
	if calls["model"] != 1 || calls["auth"] != 2 || calls["other"] != 3 {
		t.Fatalf("missing actual model and preparation calls: %v", calls)
	}
}
