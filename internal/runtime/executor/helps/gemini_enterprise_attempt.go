package helps

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type enterpriseAttemptKey struct{}

// EnterpriseAttempt observes one execution without retaining payloads or credentials.
// Trace callbacks can run on transport goroutines; all observation state is locked.
type EnterpriseAttempt struct {
	model                                                          string
	host, contentType                                              string
	outputSeen                                                     bool
	mu                                                             sync.Mutex
	now                                                            func() time.Time
	start, request, headers, firstByte, firstOutput, bodyDone, end time.Time
	connectStart, tlsStart                                         time.Time
	connectMS, tlsMS                                               int64
	connectSeen, tlsSeen, sent, finished                           bool
	status                                                         int
	requestID, phase                                               string
	stream                                                         bool
}

func NewEnterpriseAttempt(ctx context.Context, stream bool) (context.Context, *EnterpriseAttempt) {
	if ctx == nil {
		ctx = context.Background()
	}
	a := &EnterpriseAttempt{now: time.Now, start: time.Now(), phase: "request_build", stream: stream}
	return context.WithValue(ctx, enterpriseAttemptKey{}, a), a
}

func enterpriseAttemptFrom(ctx context.Context) *EnterpriseAttempt {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(enterpriseAttemptKey{}).(*EnterpriseAttempt)
	return a
}

func ObserveEnterpriseRequest(ctx context.Context, req *http.Request, model ...string) *http.Request {
	if a := enterpriseAttemptFrom(ctx); a != nil {
		if len(model) > 0 {
			a.mu.Lock()
			a.model = model[0]
			if !enterpriseModelID.MatchString(a.model) {
				a.model = enterpriseFingerprint(a.model)
			}
			a.mu.Unlock()
		}
		return a.Request(req)
	}
	return req
}

func ObserveEnterpriseResponse(ctx context.Context, resp *http.Response) {
	if a := enterpriseAttemptFrom(ctx); a != nil {
		a.Response(resp)
	}
}

// Request observes transport events without installing any deadlines or retries.
func (a *EnterpriseAttempt) Request(req *http.Request) *http.Request {
	a.mu.Lock()
	a.request, a.phase = a.now(), "upstream_headers"
	if enterpriseEndpointHost.MatchString(req.URL.Hostname()) {
		a.host = req.URL.Hostname()
	}
	a.mu.Unlock()
	trace := &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			a.mu.Lock()
			if a.connectStart.IsZero() {
				a.connectStart = a.now()
			}
			a.mu.Unlock()
		},
		ConnectDone: func(_, _ string, _ error) {
			a.mu.Lock()
			a.connectMS = elapsedMS(a.connectStart, a.now())
			a.connectSeen = true
			a.mu.Unlock()
		},
		TLSHandshakeStart: func() { a.mu.Lock(); a.tlsStart = a.now(); a.mu.Unlock() },
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			a.mu.Lock()
			a.tlsMS += elapsedMS(a.tlsStart, a.now())
			a.tlsSeen = true
			a.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) { a.mu.Lock(); a.sent = info.Err == nil; a.mu.Unlock() },
		GotFirstResponseByte: func() {
			a.mu.Lock()
			if a.firstByte.IsZero() {
				a.firstByte = a.now()
			}
			a.mu.Unlock()
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

func (a *EnterpriseAttempt) Response(resp *http.Response) {
	a.mu.Lock()
	a.headers, a.status, a.phase = a.now(), resp.StatusCode, "upstream_body"
	a.requestID = safeEnterpriseRequestID(resp.Header)
	a.contentType = strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	switch a.contentType {
	case "", "application/json", "text/event-stream", "text/html", "text/plain":
	default:
		a.contentType = "other"
	}
	a.mu.Unlock()
	resp.Body = &enterpriseObservedBody{ReadCloser: resp.Body, attempt: a}
}

type enterpriseObservedBody struct {
	io.ReadCloser
	attempt *EnterpriseAttempt
}

func (r *enterpriseObservedBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.attempt.mu.Lock()
		if r.attempt.firstByte.IsZero() {
			r.attempt.firstByte = r.attempt.now()
		}
		r.attempt.mu.Unlock()
	}
	return n, err
}

func (a *EnterpriseAttempt) Output(kind string) {
	switch kind {
	case "text", "thought", "code", "execution", "file":
	default:
		return
	}
	a.mu.Lock()
	if a.firstOutput.IsZero() {
		a.firstOutput = a.now()
	}
	a.outputSeen = true
	a.mu.Unlock()
}

func (a *EnterpriseAttempt) BodyComplete() {
	a.mu.Lock()
	a.bodyDone, a.phase = a.now(), "response_format"
	a.mu.Unlock()
}

func (a *EnterpriseAttempt) MediaDownload() { a.mu.Lock(); a.phase = "media_download"; a.mu.Unlock() }

func (a *EnterpriseAttempt) Finish(ctx context.Context, cfg *config.Config, err error) {
	a.mu.Lock()
	if a.finished {
		a.mu.Unlock()
		return
	}
	a.end, a.finished = a.now(), true
	a.mu.Unlock()
	if err != nil {
		RecordEnterpriseFailure(ctx, cfg, err)
	} else {
		LogWithRequestID(ctx).WithFields(a.fields(nil)).Debug("Enterprise upstream request completed")
	}
}

func elapsedMS(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func (a *EnterpriseAttempt) fields(err error) log.Fields {
	a.mu.Lock()
	defer a.mu.Unlock()
	end := a.end
	if end.IsZero() {
		end = a.now()
	}
	f := log.Fields{"stage": a.phase, "stream": a.stream, "upstream_http_status": a.status,
		"request_sent": a.sent, "generation_started": a.outputSeen || coreexec.GenerationHasStarted(err),
		"total_ms": elapsedMS(a.start, end), "error_class": enterpriseFailureClass(err)}
	if !a.request.IsZero() {
		f["build_ms"] = elapsedMS(a.start, a.request)
	}
	if !a.headers.IsZero() {
		f["headers_ms"] = elapsedMS(a.request, a.headers)
		f["body_ms"] = elapsedMS(a.headers, end)
	}
	if !a.bodyDone.IsZero() {
		f["body_ms"] = elapsedMS(a.headers, a.bodyDone)
		f["response_ms"] = elapsedMS(a.bodyDone, end)
	}
	if !a.firstByte.IsZero() {
		f["first_byte_ms"] = elapsedMS(a.request, a.firstByte)
	}
	if !a.firstOutput.IsZero() {
		f["first_output_ms"] = elapsedMS(a.request, a.firstOutput)
	}
	if a.connectSeen {
		f["connect_ms"] = a.connectMS
	}
	if a.tlsSeen {
		f["tls_ms"] = a.tlsMS
	}
	if a.requestID != "" {
		f["upstream_request_id"] = a.requestID
	}
	if a.host != "" {
		f["upstream_host"] = a.host
	}
	if a.model != "" {
		f["upstream_model"] = a.model
	}
	if a.contentType != "" {
		f["upstream_content_type"] = a.contentType
	}
	var generation *ge.GenerationError
	if errors.As(err, &generation) {
		f["upstream_http_status"], f["upstream_code"] = generation.UpstreamHTTPStatus, generation.UpstreamCode
		if generation.RPCStatus != "" {
			f["upstream_rpc_status"] = generation.RPCStatus
		}
		f["generation_started"] = generation.Started || coreexec.GenerationHasStarted(err)
	}
	return f
}

func enterpriseFailureClass(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "client_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "caller_deadline"
	}
	var transport *ge.TransportError
	if errors.As(err, &transport) {
		var network net.Error
		if errors.As(transport.Cause, &network) && network.Timeout() {
			return "transport_timeout"
		}
		return "transport_error"
	}
	var generation *ge.GenerationError
	if errors.As(err, &generation) {
		if generation.UpstreamHTTPStatus >= 200 && generation.UpstreamHTTPStatus < 300 {
			return "stream_error"
		}
		return "http_error"
	}
	var protocol *enterprise.Error
	if errors.As(err, &protocol) {
		switch protocol.Kind {
		case "incomplete_upstream", "invalid_upstream_stream", "upstream_too_large", "upstream_read_failed", "upstream_failed", "unsupported_upstream_content", "invalid_grounding", "invalid_media", "media_download_failed", "empty_upstream", "invalid_json_output", "output_too_large":
			return protocol.Kind
		}
	}
	return "generation_error"
}

func enterpriseFailureStatus(err error) int {
	if errors.Is(err, context.Canceled) {
		return 499
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504
	}
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() >= 400 && status.StatusCode() <= 599 {
		return status.StatusCode()
	}
	return 502
}

var enterpriseRequestID = regexp.MustCompile(`^(?:[a-fA-F0-9]{32}|[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12})$`)
var enterpriseEndpointHost = regexp.MustCompile(`^(?:(?:us|eu|ca|au|in)-)?discoveryengine\.googleapis\.com$`)
var enterpriseModelID = regexp.MustCompile(`^(?:gemini|veo)-[a-zA-Z0-9._-]{1,100}$`)

func enterpriseFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func safeEnterpriseRequestID(headers http.Header) string {
	for _, name := range []string{"X-Goog-Request-Id", "X-Request-Id"} {
		if value := headers.Get(name); value != "" {
			if enterpriseRequestID.MatchString(value) {
				return value
			}
			return enterpriseFingerprint(value)
		}
	}
	return ""
}
