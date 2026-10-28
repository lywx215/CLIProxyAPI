package helps

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestEnterpriseAttemptLogsPhasesWithoutRequestLogging(t *testing.T) {
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldOutput, oldLevel := logger.Out, logger.Level
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.DebugLevel)
	hook := logtest.NewGlobal()
	t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetOutput(oldOutput); logger.SetLevel(oldLevel) })
	ctx, a := NewEnterpriseAttempt(t.Context(), false)
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	a.start, a.now = now, func() time.Time { return now }
	now = now.Add(10 * time.Millisecond)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.invalid/private-path?token=private-token", strings.NewReader("private-prompt"))
	req.Header.Set("Authorization", "Bearer private-token")
	req = a.Request(req)
	trace := httptrace.ContextClientTrace(req.Context())
	trace.ConnectStart("tcp", "private-host")
	now = now.Add(5 * time.Millisecond)
	trace.ConnectDone("tcp", "private-host", nil)
	trace.TLSHandshakeStart()
	now = now.Add(7 * time.Millisecond)
	trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	now = now.Add(8 * time.Millisecond)
	trace.GotFirstResponseByte()
	now = now.Add(20 * time.Millisecond)
	a.Response(&http.Response{StatusCode: 200, Header: http.Header{"X-Goog-Request-Id": {"Bearer private-token"}, "Set-Cookie": {"private-cookie"}}, Body: io.NopCloser(strings.NewReader("private-response"))})
	now = now.Add(10 * time.Millisecond)
	a.Output("text")
	now = now.Add(30 * time.Millisecond)
	a.BodyComplete()
	now = now.Add(10 * time.Millisecond)
	err := EnterpriseGenerationError([]byte(`{"error":{"code":14,"status":"UNAVAILABLE","message":"private-prompt private-token"}}`), 200, nil, nil, true)
	err = coreexec.MarkGenerationStarted(err)
	a.Finish(ctx, &config.Config{}, err)
	a.Finish(ctx, &config.Config{}, err)
	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("expected one failure diagnostic, got %d", len(entries))
	}
	f := entries[0].Data
	for key, want := range map[string]int64{"build_ms": 10, "headers_ms": 40, "body_ms": 40, "response_ms": 10, "first_byte_ms": 20, "first_output_ms": 50, "total_ms": 100, "connect_ms": 5, "tls_ms": 7} {
		if got, ok := f[key].(int64); !ok || got != want {
			t.Fatalf("%s = %v, want %d", key, f[key], want)
		}
	}
	if f["upstream_http_status"] != 200 || f["status"] != 503 || f["upstream_rpc_status"] != "UNAVAILABLE" || f["error_class"] != "stream_error" || f["generation_started"] != true || f["request_sent"] != true {
		t.Fatalf("lost original error and safety boundary: %v", f)
	}
	encoded, _ := json.Marshal(f)
	if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "Bearer") {
		t.Fatalf("diagnostic leaked payload or headers: %s", encoded)
	}
	if !strings.HasPrefix(f["upstream_request_id"].(string), "sha256:") {
		t.Fatal("untrusted request ID was not fingerprinted")
	}
}

func TestEnterpriseStreamDiagnosticDoesNotInventHTTP502(t *testing.T) {
	err := EnterpriseGenerationError([]byte(`{"error":{"code":999,"message":"private-prompt"}}`), 200, nil, nil, false)
	if strings.Contains(err.Error(), "returned HTTP 502") || !strings.Contains(err.Error(), "unrecognized error") || strings.Contains(err.Error(), "private-") {
		t.Fatalf("stream status was reported as a transport response: %v", err)
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		_, a := NewEnterpriseAttempt(t.Context(), true)
		f := a.fields(err)
		if f["error_class"] == "http_error" || f["upstream_http_status"] != 0 {
			t.Fatal("caller cancellation was attributed to Google HTTP")
		}
	}
}

func TestEnterpriseAttemptMetadataNeverRetainsArbitraryValues(t *testing.T) {
	for _, model := range []string{"gemini-3.8-flash", "private-model-token"} {
		ctx, a := NewEnterpriseAttempt(t.Context(), false)
		r, _ := http.NewRequestWithContext(ctx, "POST", "https://us-discoveryengine.googleapis.com/private-path?token=private-token", nil)
		ObserveEnterpriseRequest(ctx, r, model)
		a.Response(&http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {"application/json; private-parameter=private-token"}, "X-Request-Id": {"00000000-1111-2222-3333-444444444444"}}, Body: io.NopCloser(strings.NewReader("private-body"))})
		fields := a.fields(nil)
		encoded, _ := json.Marshal(fields)
		if fields["upstream_host"] != "us-discoveryengine.googleapis.com" || fields["upstream_content_type"] != "application/json" || strings.Contains(string(encoded), "private-") {
			t.Fatalf("unsafe or missing routing metadata: %s", encoded)
		}
		if model == "gemini-3.8-flash" && fields["upstream_model"] != model {
			t.Fatal("lost actual upstream model")
		}
	}
}

func TestEnterpriseProtocolFailureDiagnosticsUseOnlyKnownCategories(t *testing.T) {
	for _, kind := range []string{"incomplete_upstream", "invalid_upstream_stream", "upstream_failed", "private-token"} {
		err := &enterprise.Error{Status: 502, Kind: kind, Message: "private-upstream-state"}
		class := enterpriseFailureClass(err)
		if kind == "private-token" {
			if class != "generation_error" {
				t.Fatal("arbitrary error category was retained")
			}
		} else if class != kind {
			t.Fatalf("lost protocol failure category %s", kind)
		}
	}
}
