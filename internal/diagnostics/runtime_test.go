package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type recordSink struct {
	mu    sync.Mutex
	lines [][]byte
	ready chan struct{}
}

func (s *recordSink) write(line []byte) error {
	s.mu.Lock()
	s.lines = append(s.lines, bytes.Clone(line))
	s.mu.Unlock()
	if s.ready != nil {
		select {
		case s.ready <- struct{}{}:
		default:
		}
	}
	return nil
}
func (s *recordSink) records(t *testing.T, event string) []Record {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []Record
	for _, line := range s.lines {
		if len(line) > 4096 || !bytes.HasPrefix(line, []byte("@diag ")) || bytes.Count(line, []byte{'\n'}) != 1 {
			t.Fatal("invalid diagnostic framing")
		}
		var r Record
		decode(t, bytes.TrimPrefix(line, []byte("@diag ")), &r)
		if r.Event == event {
			records = append(records, r)
		}
	}
	return records
}
func localEngine(sink *recordSink, peers string) *Engine {
	return NewEngine(ResourceConfig{Environment: "test", DeploymentID: "local-fixture", InstanceID: "fixture-instance"}, peers, nil, nil, sink.write)
}
func peerJSON(origin string) string {
	return fmt.Sprintf(`[{"alias":"local-peer","origin":%q,"pathPrefix":"/v1","service":"gcli2api","deploymentId":null}]`, origin)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countedBody struct {
	reader        io.Reader
	reads, closes atomic.Int64
	closeErr      error
}

func (b *countedBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.reader.Read(p) }
func (b *countedBody) Close() error               { b.closes.Add(1); return b.closeErr }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("synthetic secret must not be logged")
}

func TestCallTerminalLifecycle(t *testing.T) {
	for _, mode := range []string{"eof", "close", "read_error", "cancel", "transport_error", "close_error"} {
		t.Run(mode, func(t *testing.T) {
			sink := &recordSink{ready: make(chan struct{}, 10)}
			e := localEngine(sink, peerJSON("https://peer.test"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, span := e.StartServer(ctx, nil, "local-1")
			body := &countedBody{reader: strings.NewReader("payload")}
			if mode == "read_error" {
				body.reader = errorReader{}
			}
			if mode == "close_error" {
				body.closeErr = errors.New("synthetic close failure")
			}
			client := FinalizeClient(ctx, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if requestOwned(r) == false || len(r.Header.Values("Traceparent")) != 1 {
					t.Error("not owned/injected at final boundary")
				}
				if mode == "transport_error" {
					return nil, errors.New("synthetic private transport failure")
				}
				return &http.Response{StatusCode: 200, Header: fields([][2]string{{"X-Diag-Request-Id", "peer-1"}, {"X-Diag-Trace-Id", span.incoming.TraceID}}), Body: body, Request: r}, nil
			})})
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://peer.test/v1", strings.NewReader("unchanged"))
			resp, err := client.Do(req)
			if mode == "transport_error" {
				if err == nil {
					t.Fatal("lost transport error")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if body.reads.Load() != 0 || len(sink.records(t, "diag.call")) != 0 {
					t.Fatal("body pre-read or settled at headers")
				}
				switch mode {
				case "eof", "read_error":
					_, _ = io.ReadAll(resp.Body)
				case "cancel":
					cancel()
					for len(sink.records(t, "diag.call")) == 0 {
						<-sink.ready
					}
				}
				_ = resp.Body.Close()
				_ = resp.Body.Close()
				if body.closes.Load() != 2 {
					t.Fatal("Close calls not delegated unchanged")
				}
			}
			calls := sink.records(t, "diag.call")
			if len(calls) != 1 {
				t.Fatalf("terminal count %d", len(calls))
			}
			data := calls[0].Data.(map[string]any)
			want := map[string]string{"eof": "eof", "close": "closed_early", "cancel": "cancelled", "transport_error": "transport_error", "read_error": "read_error", "close_error": "read_error"}[mode]
			if data["endReason"] != want {
				t.Fatalf("reason %v want %s", data["endReason"], want)
			}
			if requestOwned(req) || req.Header.Get("Traceparent") != "" {
				t.Fatal("mutated original request")
			}
			span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
			span.Finish(ServerData{})
			if len(sink.records(t, "diag.server")) != 1 {
				t.Fatal("server terminal duplicated")
			}
		})
	}
}

func TestGoRedirectsPreservePristineBusinessHeaders(t *testing.T) {
	for _, dest := range []string{"same_origin_private", "other_origin", "peer"} {
		t.Run(dest, func(t *testing.T) {
			const businessTrace = "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"
			var first, last http.Header
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				last = r.Header.Clone()
				w.WriteHeader(200)
				_, _ = w.Write([]byte("body"))
			}))
			defer outside.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/start" {
					first = r.Header.Clone()
					target := "/private"
					if dest == "peer" {
						target = "/v1/next"
					}
					if dest == "other_origin" {
						target = outside.URL + "/v1"
					}
					http.Redirect(w, r, target, http.StatusFound)
					return
				}
				last = r.Header.Clone()
				_, _ = w.Write([]byte("body"))
			}))
			defer server.Close()
			sink := &recordSink{}
			e := localEngine(sink, peerJSON(server.URL))
			ctx, span := e.StartServer(context.Background(), nil, "local-1")
			client := FinalizeClient(ctx, &http.Client{})
			ireq, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/start", nil)
			ireq.Header.Set("Traceparent", businessTrace)
			ireq.Header.Set("Tracestate", "provider=business")
			ireq.Header.Set("X-Request-Id", "provider-id")
			resp, err := client.Do(ireq)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if first.Get("Traceparent") == businessTrace || first.Get("X-Diag-Request-Id") != "local-1" {
				t.Fatal("first peer hop was not injected")
			}
			if dest == "peer" {
				if last.Get("Traceparent") == first.Get("Traceparent") || last.Get("X-Diag-Request-Id") != "local-1" {
					t.Fatal("redirect did not get new call")
				}
			} else {
				if last.Get("Traceparent") != businessTrace || last.Get("Tracestate") != "provider=business" || last.Get("X-Diag-Request-Id") != "" {
					t.Fatalf("Go redirect original business headers lost/leaked: %v", last)
				}
			}
			if ireq.Header.Get("Traceparent") != businessTrace || requestOwned(ireq) {
				t.Fatal("ireq ownership mutated")
			}
			calls := sink.records(t, "diag.call")
			if len(calls) != 2 || calls[1].Data.(map[string]any)["callKind"] != "redirect" {
				t.Fatal("redirect calls miscounted")
			}
			span.Finish(ServerData{EndReason: "finished", DeliveryState: "local_finished"})
		})
	}
}

func TestGinCommitErrorStreamingAndNoEarly200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"error", "stream", "empty", "panic", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			sink := &recordSink{}
			e := localEngine(sink, "")
			r := gin.New()
			r.Use(e.GinMiddleware(func(*gin.Context) string { return "local-1" }))
			r.Use(gin.Recovery())
			r.GET("/v1/:id", func(c *gin.Context) {
				if c.Writer.Written() {
					t.Fatal("early response commit")
				}
				c.Writer.Header()["x-diag-request-id"] = []string{"peer-a", "peer-b"}
				c.Header("X-Diag-Unknown", "must-remove")
				c.Header("X-Request-Id", "old-business-id")
				c.Header("X-Trace-Id", "old-trace")
				switch mode {
				case "error":
					c.String(503, "unchanged-error")
				case "stream":
					c.Status(202)
					c.Writer.Flush()
					_, _ = c.Writer.WriteString("first")
					_, _ = c.Writer.Write([]byte("second"))
				case "empty":
					c.Status(204)
				case "panic":
					panic("synthetic panic")
				case "cancel":
					c.String(499, "cancelled")
				}
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/v1/private-parameter", nil)
			if mode == "cancel" {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			r.ServeHTTP(w, req)
			h := w.Result().Header
			if len(HeaderValues(h, "x-diag-request-id")) != 1 || h.Get("X-Diag-Request-Id") != "local-1" || !nonzeroHex(h.Get("X-Diag-Trace-Id"), 32) || h.Get("X-Diag-Unknown") != "" {
				t.Fatalf("bad commit headers %v", h)
			}
			if h.Get("X-Request-Id") != "old-business-id" || h.Get("X-Trace-Id") != "old-trace" {
				t.Fatal("business IDs changed")
			}
			want := map[string]int{"error": 503, "stream": 202, "empty": 204, "panic": 500, "cancel": 499}[mode]
			if w.Code != want {
				t.Fatalf("status %d want %d", w.Code, want)
			}
			server := sink.records(t, "diag.server")
			if len(server) != 1 {
				t.Fatal("server terminal count")
			}
			data := server[0].Data.(map[string]any)
			if data["routeTemplate"] != "/v1/:id" {
				t.Fatal("raw path or missing template")
			}
			end, delivery, committed := "finished", "local_finished", true
			var wire any = float64(want)
			if mode == "cancel" {
				end, delivery = "client_cancel", "cancelled"
			}
			if mode == "empty" {
				delivery, committed, wire = "unknown", false, nil
			}
			assertServerTerminal(t, data, end, delivery, committed, wire)
		})
	}
}

func TestConcurrentCallsResourcesAndGates(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	sink := &recordSink{}
	e := NewEngine(ResourceConfig{}, "", enabled.Load, func() bool { return false }, sink.write)
	ctx, span := e.StartServer(context.Background(), nil, "same-local-id")
	client := FinalizeClient(ctx, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})})
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://provider.test", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		})
	}
	wg.Wait()
	span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
	seenIDs, seenNo := map[string]bool{}, map[uint64]bool{}
	for _, call := range sink.records(t, "diag.call") {
		if seenIDs[*call.SpanID] || seenNo[*call.CallNo] {
			t.Fatal("call identity reused")
		}
		seenIDs[*call.SpanID], seenNo[*call.CallNo] = true, true
		if *call.ParentSpanID != span.id || *call.ServerSpanID != span.id || call.AttemptID != nil || call.AttemptNo != nil {
			t.Fatal("ownership/attempt fabricated")
		}
	}
	if len(seenIDs) != 64 {
		t.Fatal("call count")
	}
	server := sink.records(t, "diag.server")[0]
	if server.Data.(map[string]any)["callCount"] != float64(64) {
		t.Fatal("server call count")
	}
	other := localEngine(&recordSink{}, "")
	if e.resource.InstanceID == other.resource.InstanceID || e.resource.BootID == other.resource.BootID {
		t.Fatal("instance collision")
	}
	enabled.Store(false)
	_, muted := e.StartServer(context.Background(), nil, "same-local-id")
	muted.Finish(ServerData{})
	if len(sink.records(t, "diag.server")) != 1 {
		t.Fatal("basic gate ignored")
	}
	if muted.incoming.TraceID == span.incoming.TraceID || muted.id == span.id {
		t.Fatal("reused caller/local ID merged traces")
	}
	// No detailed data exists in these records even with a DEBUG-capable sink.
	for _, line := range sink.lines {
		for _, forbidden := range []string{"usage", "credential", "payload", "private-parameter"} {
			if bytes.Contains(line, []byte(forbidden)) {
				t.Fatal("unapproved basic data")
			}
		}
	}
}

func TestReloadDisablesInvalidPeerSnapshot(t *testing.T) {
	sink := &recordSink{}
	e := localEngine(sink, peerJSON("https://peer.test"))
	req := httptest.NewRequest("GET", "https://peer.test/v1", nil)
	e.ReloadPeers(peerJSON("https://peer.test"))
	if len(sink.records(t, "diag.process")) != 1 || e.revision != 1 {
		t.Fatal("unchanged environment produced a config event")
	}
	if e.peers.Load().Match(req.URL) == nil {
		t.Fatal("initial peer")
	}
	e.ReloadPeers(`[invalid`)
	e.ReloadPeers(`[invalid`)
	if e.peers.Load().Match(req.URL) != nil {
		t.Fatal("invalid reload kept old authorization")
	}
	e.ReloadPeers(peerJSON("https://peer.test"))
	if e.peers.Load().Match(req.URL) == nil {
		t.Fatal("valid reload")
	}
	process := sink.records(t, "diag.process")
	if len(process) != 3 || process[1].Data.(map[string]any)["configStatus"] != "config_invalid" {
		t.Fatal("process config records")
	}
}

func TestIPv6AddressEquivalenceKeepsAuthorityFamily(t *testing.T) {
	p := ParsePeers(peerJSON("http://[::ffff:127.0.0.1]"))
	for target, want := range map[string]bool{
		"http://[::ffff:7f00:1]/v1":            true,
		"http://[0:0:0:0:0:ffff:7f00:1]:80/v1": true,
		"http://127.0.0.1/v1":                  false,
		"http://[::1]/v1":                      false,
	} {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		if (p.Match(u) != nil) != want {
			t.Fatalf("authority family or address comparison: %s", target)
		}
	}
	for _, origin := range []string{"http://[127.0.0.1]", "http://peer.test:", "http://[::1%25lo]"} {
		if ParsePeers(peerJSON(origin)).Status != "config_invalid" {
			t.Fatalf("invalid origin accepted: %s", origin)
		}
	}
}

type duplexFixture struct {
	*bytes.Buffer
	closed bool
}

func (b *duplexFixture) Close() error { b.closed = true; return nil }
func TestBodyInterfacesAndFinalizerIdempotence(t *testing.T) {
	sink := &recordSink{}
	e := localEngine(sink, "")
	ctx, span := e.StartServer(context.Background(), nil, "local-1")
	body := &duplexFixture{Buffer: bytes.NewBufferString("stream")}
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 101, Header: make(http.Header), Body: body}, nil
	})
	client := FinalizeClient(ctx, &http.Client{Transport: base})
	if FinalizeClient(ctx, client) != client {
		t.Fatal("double decoration")
	}
	if _, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
		t.Fatal("invented idle capability")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://fixture.test", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	w, ok := resp.Body.(io.Writer)
	if !ok {
		t.Fatal("upgrade writer lost")
	}
	_, _ = w.Write([]byte("-duplex"))
	wt, ok := resp.Body.(io.WriterTo)
	if !ok {
		t.Fatal("WriterTo lost")
	}
	var out bytes.Buffer
	_, err = wt.WriteTo(&out)
	if err != nil || out.String() != "stream-duplex" {
		t.Fatal("duplex bytes changed")
	}
	_ = resp.Body.Close()
	if !body.closed || len(sink.records(t, "diag.call")) != 1 {
		t.Fatal("duplex completion")
	}
	span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
	plain := wrapBody(io.NopCloser(errorReader{}), &callCompletion{server: span, started: time.Now()})
	if _, ok := plain.(io.Writer); ok {
		t.Fatal("invented writer capability")
	}
	if _, ok := plain.(io.WriterTo); ok {
		t.Fatal("invented WriterTo capability")
	}
}

func TestBasicAccessGateIsIndependentOfDebug(t *testing.T) {
	for _, access := range []bool{false, true} {
		for _, debug := range []bool{false, true} {
			t.Run(fmt.Sprintf("access=%t/debug=%t", access, debug), func(t *testing.T) {
				sink := &recordSink{}
				e := NewEngine(ResourceConfig{}, "", func() bool { return access }, func() bool { return debug }, sink.write)
				_, span := e.StartServer(context.Background(), nil, "local-1")
				span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
				want := 0
				if access {
					want = 1
				}
				if len(sink.records(t, "diag.server")) != want || len(sink.records(t, "diag.process")) != want {
					t.Fatal("logging gates coupled")
				}
			})
		}
	}
}

func TestBoundedStubAndSinkFailure(t *testing.T) {
	sink := &recordSink{}
	e := localEngine(sink, "")
	r := e.base("server", "diag.server")
	r.LogSeq = 1
	r.Data = map[string]string{"oversize": strings.Repeat("x", 5000)}
	e.emit(r)
	records := sink.records(t, "diag.truncated")
	if len(records) != 1 || records[0].LogSeq != 1 || records[0].Data.(map[string]any)["reason"] != "line_limit" {
		t.Fatal("bounded replacement")
	}
	failing := NewEngine(ResourceConfig{}, "", nil, nil, func([]byte) error { return errors.New("synthetic sink failure") })
	if failing.dropped.Load() != 1 {
		t.Fatal("sink failure not counted locally")
	}
}

func TestSyntheticRecordArtifact(t *testing.T) {
	sink := &recordSink{}
	e := localEngine(sink, peerJSON("https://peer.test"))
	ctx, span := e.StartServer(context.Background(), fields([][2]string{{"X-Request-Id", "synthetic-caller"}}), "synthetic-local")
	client := FinalizeClient(ctx, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: fields([][2]string{{"X-Diag-Request-Id", "synthetic-peer"}, {"X-Diag-Trace-Id", span.incoming.TraceID}}), Body: io.NopCloser(strings.NewReader("synthetic body never logged"))}, nil
	})})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://peer.test/v1", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	status := 200
	route := "/v1/chat/completions"
	span.Finish(ServerData{RouteTemplate: &route, HeadersCommitted: true, WireStatus: &status, EndReason: "finished", DeliveryState: "local_finished"})
	if output := os.Getenv("DIAG_TEST_RECORDS"); output != "" {
		if err := os.WriteFile(output, bytes.Join(sink.lines, nil), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range sink.records(t, "diag.server") {
		data := r.Data.(map[string]any)
		var coverage Coverage
		b, _ := json.Marshal(data["coverage"])
		decode(t, b, &coverage)
		assessment := AssessCoverage(CoverageEvidence{Sequences: []uint64{r.LogSeq}, ExpectedLastLogSeq: &coverage.ExpectedLastLogSeq, TerminalCount: 1, TerminalLogSeq: &r.LogSeq, DebugCapture: coverage.DebugCapture, AccessCapture: coverage.AccessCapture, DroppedForSpan: &coverage.DroppedForSpan, TruncatedEvents: &coverage.TruncatedEvents})
		if assessment.TerminalMissing || assessment.DebugCoverage != "unknown" {
			t.Fatal("opaque logger coverage overstated")
		}
	}
}

func TestTwoLocalReplicasRetainDistinctServerIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var receivers []*recordSink
	var servers []*httptest.Server
	var peerEntries []string
	for i := range 2 {
		sink := &recordSink{}
		receivers = append(receivers, sink)
		e := NewEngine(ResourceConfig{DeploymentID: "fixture-pool", InstanceID: fmt.Sprintf("replica-%d", i)}, "", nil, nil, sink.write)
		r := gin.New()
		r.Use(e.GinMiddleware(func(*gin.Context) string { return "same-short-id" }))
		r.GET("/v1", func(c *gin.Context) { c.String(200, "local-response") })
		s := httptest.NewServer(r)
		defer s.Close()
		servers = append(servers, s)
		peerEntries = append(peerEntries, fmt.Sprintf(`{"alias":"replica-%d","origin":%q,"pathPrefix":"/v1","service":"cliproxyapi","deploymentId":"fixture-pool"}`, i, s.URL))
	}
	sink := &recordSink{}
	e := localEngine(sink, "["+strings.Join(peerEntries, ",")+"]")
	ctx, span := e.StartServer(context.Background(), nil, "same-short-id")
	client := FinalizeClient(ctx, &http.Client{})
	for _, server := range servers {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	calls := sink.records(t, "diag.call")
	for i, receiver := range receivers {
		r := receiver.records(t, "diag.server")[0]
		if *r.TraceID != *calls[i].TraceID || *r.ParentSpanID != *calls[i].SpanID || *r.RequestID != "same-short-id" {
			t.Fatal("bilateral parent or local request ID")
		}
		data := calls[i].Data.(map[string]any)
		if data["peerTraceId"] != *r.TraceID || data["peerRequestId"] != *r.RequestID {
			t.Fatal("peer response IDs")
		}
	}
	a, b := receivers[0].records(t, "diag.server")[0], receivers[1].records(t, "diag.server")[0]
	if a.InstanceID == b.InstanceID || a.BootID == b.BootID || *a.SpanID == *b.SpanID {
		t.Fatal("replicas merged")
	}
	span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
}

func TestHijackPreservesUpgradeAndDoesNotInventWireStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := &recordSink{ready: make(chan struct{}, 10)}
	e := localEngine(sink, "")
	r := gin.New()
	r.Use(e.GinMiddleware(func(*gin.Context) string { return "local-1" }))
	r.GET("/upgrade", func(c *gin.Context) {
		conn, rw, err := c.Writer.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() {
			if errClose := conn.Close(); errClose != nil {
				t.Error(errClose)
			}
		}()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: fixture\r\n\r\n")
		_ = rw.Flush()
	})
	server := httptest.NewServer(r)
	defer server.Close()
	resp, err := http.Get(server.URL + "/upgrade")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 101 {
		t.Fatal("upgrade changed")
	}
	for len(sink.records(t, "diag.server")) == 0 {
		<-sink.ready
	}
	data := sink.records(t, "diag.server")[0].Data.(map[string]any)
	assertServerTerminal(t, data, "unknown", "unknown", false, nil)
}

func assertServerTerminal(t *testing.T, data map[string]any, end, delivery string, committed bool, wire any) {
	t.Helper()
	if data["endReason"] != end || data["deliveryState"] != delivery || data["headersCommitted"] != committed || data["wireStatus"] != wire {
		t.Fatalf("terminal = %v; want %s/%s/%v/%v", data, end, delivery, committed, wire)
	}
}

type failedResponseWriter struct{ *httptest.ResponseRecorder }

func (w failedResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic write failure")
}

func TestGinWriteFailureTerminal(t *testing.T) {
	sink := &recordSink{}
	r := gin.New()
	r.Use(localEngine(sink, "").GinMiddleware(func(*gin.Context) string { return "local-1" }))
	r.GET("/failure", func(c *gin.Context) {
		c.Status(202)
		if _, err := c.Writer.Write([]byte("body")); err == nil {
			t.Error("write failure swallowed")
		}
	})
	r.ServeHTTP(failedResponseWriter{httptest.NewRecorder()}, httptest.NewRequest("GET", "/failure", nil))
	records := sink.records(t, "diag.server")
	if len(records) != 1 {
		t.Fatalf("terminals = %d", len(records))
	}
	assertServerTerminal(t, records[0].Data.(map[string]any), "error", "failed", true, float64(202))
}

func TestServerFinishDoesNotHoldSpanLockDuringSinkIO(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e := NewEngine(ResourceConfig{}, "", nil, nil, func(line []byte) error {
		if bytes.Contains(line, []byte(`"event":"diag.server"`)) {
			close(entered)
			<-release
		}
		return nil
	})
	ctx, span := e.StartServer(context.Background(), nil, "local-1")
	go func() { span.Finish(ServerData{}); close(finished) }()
	<-entered
	defer func() { close(release); <-finished }()
	if !span.mu.TryLock() {
		t.Fatal("sink holds the span lock")
	}
	sealed := span.sealed
	span.mu.Unlock()
	if !sealed {
		t.Fatal("span not sealed before I/O")
	}
	// A late call delegates without allocating another call or waiting on the sink.
	client := FinalizeClient(ctx, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
	})})
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://fixture.test", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}
