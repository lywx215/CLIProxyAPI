package diagnostics

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

type ownershipKey struct{}

// ownership is bound to this exact transport clone, never to ireq or a mutable
// holder inherited by Go's redirect request. http.Client rebuilds redirect
// headers from pristine ireq; its context therefore must remain unmarked.
type ownership struct{ request *http.Request }

type transport struct{ base http.RoundTripper }
type idleTransport struct {
	*transport
	idle interface{ CloseIdleConnections() }
}

func (t *idleTransport) CloseIdleConnections() { t.idle.CloseIdleConnections() }

// FinalizeClient is called after all proxy/fingerprint/cache configuration. It
// decorates a client copy, never a cached/context transport. Detached work with
// no server owner remains explicitly outside this request tracing scope.
func FinalizeClient(ctx context.Context, client *http.Client) *http.Client {
	if client == nil || ServerFromContext(ctx) == nil {
		return client
	}
	switch client.Transport.(type) {
	case *transport, *idleTransport:
		return client
	}
	out := *client
	base := out.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	t := &transport{base: base}
	if idle, ok := base.(interface{ CloseIdleConnections() }); ok {
		out.Transport = &idleTransport{t, idle}
	} else {
		out.Transport = t
	}
	return &out
}

func requestOwned(req *http.Request) bool {
	owner, _ := req.Context().Value(ownershipKey{}).(ownership)
	return owner.request == req
}

func prepareRequest(req *http.Request, x Incoming, requestID, spanID string, peer bool) *http.Request {
	owned := requestOwned(req)
	cloned := req.Clone(req.Context())
	if cloned.Header == nil {
		cloned.Header = make(http.Header)
	}
	injected := inject(cloned.Header, x, requestID, spanID, peer, owned)
	// Set the marker only on the concrete outgoing clone, after WithContext has
	// produced its final address. Never write it back to req.Context().
	cloned = cloned.WithContext(context.WithValue(cloned.Context(), ownershipKey{}, ownership{}))
	if injected {
		*cloned = *cloned.WithContext(context.WithValue(cloned.Context(), ownershipKey{}, ownership{cloned}))
	}
	return cloned
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	s := ServerFromContext(req.Context())
	if s == nil {
		cloned := req.Clone(req.Context())
		cleanHeaders(cloned.Header, requestOwned(req))
		return t.base.RoundTrip(cloned)
	}
	s.mu.Lock()
	if s.sealed {
		s.mu.Unlock()
		cloned := req.Clone(req.Context())
		cleanHeaders(cloned.Header, requestOwned(req))
		return t.base.RoundTrip(cloned)
	}
	s.calls++
	callNo := s.calls
	s.mu.Unlock()
	spanID := randomHex(8)
	peer := s.engine.peers.Load().Match(req.URL)
	if req.Host != "" && req.Host != req.URL.Host {
		virtual := *req.URL
		virtual.Host = req.Host
		a, validA := parseOrigin(req.URL)
		b, validB := parseOrigin(&virtual)
		if !validA || !validB || a != b {
			peer = nil
		}
	}
	kind, _ := req.Context().Value(callKindKey{}).(string)
	if kind == "" {
		kind = "other"
	}
	if req.Response != nil {
		kind = "redirect"
	}
	data := CallData{CallKind: kind, PeerIDs: PeerIDs{Rejected: "none"}}
	if peer != nil {
		data.PeerConfigured = true
		data.TargetAlias = &peer.Alias
		data.PeerService = &peer.Service
		data.PeerDeploymentID = peer.DeploymentID
	}
	r := s.record("call", "diag.call", spanID, &s.id)
	r.CallNo = &callNo
	applyAttempt(req.Context(), &r)
	c := &callCompletion{server: s, record: r, data: data, started: time.Now(), ctx: req.Context()}
	outgoing := prepareRequest(req, s.incoming, s.requestID, spanID, peer != nil)
	resp, err := t.base.RoundTrip(outgoing)
	if err != nil {
		reason := "transport_error"
		if req.Context().Err() != nil {
			reason = "cancelled"
		}
		c.finish(reason)
		return resp, err
	}
	if resp == nil {
		c.finish("transport_error")
		return resp, err
	}
	if resp.StatusCode >= 100 && resp.StatusCode <= 599 {
		status := resp.StatusCode
		c.data.UpstreamStatus = &status
	}
	c.data.PeerIDs = ReadPeerIDs(resp.Header, peer != nil)
	// Provider-specific aliases and attempt ownership are intentionally left to
	// the protocol owners. Arbitrary response headers are never projected.
	if resp.Body == nil {
		c.finish("eof")
		return resp, nil
	}
	c.mu.Lock()
	c.stop = context.AfterFunc(req.Context(), func() { c.finish("cancelled") })
	c.mu.Unlock()
	resp.Body = wrapBody(resp.Body, c)
	return resp, nil
}

type callCompletion struct {
	ctx     context.Context
	mu      sync.Mutex
	done    bool
	stop    func() bool
	server  *ServerSpan
	record  Record
	data    CallData
	started time.Time
}

func (c *callCompletion) finish(reason string) {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	if c.ctx != nil && c.ctx.Err() != nil {
		reason = "cancelled"
	}
	stop := c.stop
	c.record.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	c.record.LogSeq = 1
	c.data.EndReason, c.data.TotalMS, c.data.Coverage = reason, elapsed(c.started), Coverage{ExpectedLastLogSeq: 1, DroppedForSpan: c.server.engine.knownDrops(0), DebugCapture: "none", AccessCapture: "unknown"}
	c.record.Data = c.data
	r := c.record
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	if c.server.engine.enabled() {
		c.server.engine.emit(r)
	}
}

type observedBody struct {
	base       io.ReadCloser
	completion *callCompletion
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.base.Read(p)
	if err == io.EOF {
		b.completion.finish("eof")
	} else if err != nil {
		b.completion.finish("read_error")
	}
	return n, err
}
func (b *observedBody) Close() error {
	err := b.base.Close()
	if err != nil {
		b.completion.finish("read_error")
	} else {
		b.completion.finish("closed_early")
	}
	return err
}

type writerBody struct {
	*observedBody
	writer io.Writer
}

func (b *writerBody) Write(p []byte) (int, error) { return b.writer.Write(p) }

type writerToBody struct {
	*observedBody
	writerTo io.WriterTo
}

func (b *writerToBody) WriteTo(w io.Writer) (int64, error) {
	n, err := b.writerTo.WriteTo(w)
	if err != nil {
		b.completion.finish("read_error")
	} else {
		b.completion.finish("eof")
	}
	return n, err
}

type duplexWriterToBody struct {
	*writerToBody
	writer io.Writer
}

func (b *duplexWriterToBody) Write(p []byte) (int, error) { return b.writer.Write(p) }

func wrapBody(body io.ReadCloser, c *callCompletion) io.ReadCloser {
	b := &observedBody{body, c}
	w, writable := body.(io.Writer)
	wt, copies := body.(io.WriterTo)
	if writable && copies {
		return &duplexWriterToBody{&writerToBody{b, wt}, w}
	}
	if writable {
		return &writerBody{b, w}
	}
	if copies {
		return &writerToBody{b, wt}
	}
	return b
}
