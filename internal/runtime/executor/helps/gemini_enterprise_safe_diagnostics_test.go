package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func TestEnterpriseSafeDiagnosticsReachFormattedLogsWithoutBodyCapture(t *testing.T) {
	logger := log.StandardLogger()
	oldOutput, oldLevel, oldFormatter := logger.Out, logger.GetLevel(), logger.Formatter
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	t.Cleanup(func() {
		logger.SetOutput(oldOutput)
		logger.SetLevel(oldLevel)
		logger.SetFormatter(oldFormatter)
		logger.ReplaceHooks(oldHooks)
	})
	logger.SetLevel(log.WarnLevel)
	logger.SetFormatter(&logging.LogFormatter{})
	for _, transportStatus := range []int{400, 200} {
		var buf bytes.Buffer
		logger.SetOutput(&buf)
		const localID = "00000000-1111-2222-3333-444444444444"
		const callerID = "newapi-request-1234"
		engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "[]", func() bool { return true }, func() bool { return false }, func([]byte) error { return nil })
		ctx, _ := engine.StartServer(t.Context(), http.Header{"X-Request-Id": {callerID}}, localID)
		ctx = logging.WithRequestID(ctx, localID)
		ctx, attempt := NewEnterpriseAttempt(ctx, true)
		query := "中文🙂private-prompt"
		body, _ := json.Marshal(map[string]any{
			"query": map[string]any{"text": query}, "generationSpec": map[string]any{"modelId": "gemini-3.8-flash"},
			"toolsSpec": map[string]any{"webGroundingSpec": map[string]any{}}, "private-secret-key": "private-value",
		})
		auth := &coreauth.Auth{Metadata: map[string]any{
			"project_id": "12345", "location": "eu", "engine_id": "private-engine", "enterprise_collection_revision": uint64(7),
			"access_token": "private-token", "email": "private-account@example.invalid",
		}}
		ObserveEnterprisePayload(ctx, body, auth)
		request, _ := http.NewRequestWithContext(ctx, "POST", "https://eu-discoveryengine.googleapis.com/private-resource", nil)
		request = ObserveEnterpriseRequest(ctx, request, "gemini-3.8-flash")
		httptrace.ContextClientTrace(request.Context()).WroteRequest(httptrace.WroteRequestInfo{})
		response := &http.Response{StatusCode: transportStatus, Header: http.Header{"X-Goog-Request-Id": {localID}}, Body: io.NopCloser(strings.NewReader("private-response"))}
		ObserveEnterpriseResponse(ctx, response)
		err := EnterpriseGenerationError([]byte(`{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"query.text must be a string: private-value"}}`), transportStatus, response.Header, auth, false)
		publicError := err.Error()
		attempt.Finish(ctx, &config.Config{}, err)
		attempt.Finish(ctx, &config.Config{}, err)
		text := buf.String()
		for _, want := range []string{
			`diagnostic_request_id="` + localID + `"`, `caller_request_id="` + callerID + `"`,
			`upstream_request_id="` + localID + `"`, `google_status="INVALID_ARGUMENT"`, `reason_class="invalid_type"`,
			`query.text`, `query_chars=`, `query_bytes=`, `binding_revision=7`, `location="eu"`,
			`request_sent=true`, `output_seen=false`, `search_enabled=true`, `unknown_field_count=1`, `total_ms=`,
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("formatted diagnostic missing %q: %s", want, text)
			}
		}
		if strings.Contains(text, "private") || strings.Count(text, "Enterprise request failed") != 1 || strings.Count(text, "\n") != 1 {
			t.Fatalf("diagnostic leaked content or was duplicated: %s", text)
		}
		if publicError != err.Error() {
			t.Fatal("logging changed the public error")
		}
		fields := attempt.fields(err)
		if fields["query_chars"] != utf8.RuneCountInString(query) || fields["query_bytes"] != len(query) {
			t.Fatal("request counts do not match the finalized UTF-8 text")
		}
		if transportStatus == 200 && (!strings.Contains(text, `upstream_http_status=200`) || !strings.Contains(text, `diagnostic_source="google_stream"`)) {
			t.Fatal("stream RPC error was confused with an HTTP rejection")
		}
		if transportStatus == 200 {
			t.Logf("safe diagnostic fixture: %s", strings.TrimSpace(text))
		}
	}
}

func TestEnterprisePreflightAndUnknownSummaryDiagnostics(t *testing.T) {
	ctx, attempt := NewEnterpriseAttempt(t.Context(), false)
	ObserveEnterpriseStage(ctx, "input")
	ObserveEnterpriseStage(ctx, "private-stage")
	fields := attempt.fields(context.Canceled)
	if fields["stage"] != "input" || fields["request_sent"] != "unknown" {
		t.Fatal("preflight fabricated an upstream observation")
	}
	for _, body := range [][]byte{nil, []byte("private-invalid-json"), []byte(`[]`)} {
		ObserveEnterprisePayload(ctx, body, nil)
		fields = attempt.fields(enterprise.Invalid("private-message"))
		if _, ok := fields["query_chars"]; ok {
			t.Fatal("unknown query length became zero")
		}
		if _, ok := fields["search_enabled"]; ok {
			t.Fatal("unknown search setting became false")
		}
	}
}
