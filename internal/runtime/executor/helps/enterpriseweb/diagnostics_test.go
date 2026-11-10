package enterpriseweb

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	webgemini "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/gemini"
	log "github.com/sirupsen/logrus"
)

func captureWebLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logger := log.StandardLogger()
	out, formatter, level := logger.Out, logger.Formatter, logger.GetLevel()
	var buffer bytes.Buffer
	logger.SetOutput(&buffer)
	logger.SetFormatter(&logging.LogFormatter{})
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.SetOutput(out); logger.SetFormatter(formatter); logger.SetLevel(level) })
	return &buffer
}

func TestWeb413LogsMeasurementsAtInfoLevelOnceWithoutRawError(t *testing.T) {
	buffer := captureWebLogs(t)
	body := []byte(`{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 1<<20) + `PRIVATE-TEXT"}]}]}`)
	_, err := webgemini.ParseSerializedBytes(context.Background(), body, config.Binding{})
	var failure *contract.Failure
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	r := enabledRuntime()
	ctx, lease, err := r.Acquire(logging.WithRequestID(context.Background(), "12345678"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	wrapped := InputFailureFrom(failure)
	r.RecordPreflight(ctx, wrapped)
	r.RecordPreflight(ctx, wrapped)
	line := buffer.String()
	for _, want := range []string{`[12345678]`, `stage="preflight"`, `status=413`, `reason="history_limit_exceeded"`, `field="contents[0].parts[0].text"`, `limit_name="history_text_bytes"`, `maximum=1048576`, `observed_at_least=true`} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %s", want, line)
		}
	}
	if strings.Contains(line, "PRIVATE-TEXT") || strings.Count(line, "Web request rejected") != 1 || !strings.Contains(line, "[warn ") {
		t.Fatal("limit log leaked content, was duplicated, or requires debug", line)
	}
}

func TestWebLimitLogRejectsUntrustedDiagnostics(t *testing.T) {
	buffer := captureWebLogs(t)
	failure := &Failure{Code: 413, Message: "PRIVATE-RAW-ERROR", Param: "contents.PRIVATE-FIELD", Limit: &contract.InputLimit{Name: "PRIVATE-LIMIT", Observed: 2, Maximum: 1}}
	observePreflight(context.Background(), "PRIVATE-REASON", failure)
	if strings.Contains(buffer.String(), "PRIVATE-") || !strings.Contains(buffer.String(), `reason="unavailable"`) {
		t.Fatal("untrusted strings escaped diagnostic projection", buffer.String())
	}
}

func TestWebAttachment400LogsOnlySafeSubreasonAndPath(t *testing.T) {
	buffer := captureWebLogs(t)
	log.SetLevel(log.DebugLevel)
	rt := enabledRuntime()
	ctx, lease, err := rt.Acquire(logging.WithRequestID(t.Context(), "12345678"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	body := []byte(`{"contents":[{"parts":[{"inlineData":{"mimeType":"application/private-mime","data":"PRIVATE-ATTACHMENT"}}]}]}`)
	_, err = webgemini.ParseSerializedBytes(ctx, body, config.Binding{})
	var failure *contract.Failure
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	rt.RecordPreflight(ctx, InputFailureFrom(failure))
	observePreflight(ctx, "invalid_history", &Failure{
		Code: 400, Param: "contents[0].parts[0].inlineData.PRIVATE-FIELD",
		Message: "PRIVATE-ERROR", AttachmentReason: "PRIVATE-REASON",
	})
	line := buffer.String()
	for _, want := range []string{`[12345678]`, `status=400`, `reason="invalid_history"`, `attachment_reason="unsupported_mime"`, `field="contents[0].parts[0].inlineData.mimeType"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %s", want, line)
		}
	}
	if strings.Contains(line, "PRIVATE-") || strings.Contains(line, "private-mime") || strings.Count(line, "attachment_reason=") != 1 {
		t.Fatal("untrusted attachment diagnostic leaked", line)
	}
}

func TestWebDeclaredBodyLimitLogsBeforeReading(t *testing.T) {
	buffer := captureWebLogs(t)
	_, _, lease, err := ReadBody(logging.WithRequestID(context.Background(), "87654321"), strings.NewReader("PRIVATE-BODY"), MaxBodyBytes+1, enabledRuntime())
	if code(err) != 413 || lease != nil {
		t.Fatal("oversized declared body accepted", err)
	}
	line := buffer.String()
	if !strings.Contains(line, `limit_name="declared_request_bytes"`) || !strings.Contains(line, `observed=71303169`) || !strings.Contains(line, `[87654321]`) || strings.Contains(line, "PRIVATE-BODY") {
		t.Fatal("missing safe admission diagnostic", line)
	}
}
