package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestGeminiExecutorLogsSuccessfulShortResponseWithoutRequestLogging(t *testing.T) {
	logger := log.StandardLogger()
	oldLevel, oldOutput := logger.GetLevel(), logger.Out
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	hook := new(logtest.Hook)
	logger.AddHook(hook)
	logger.SetLevel(log.DebugLevel)
	logger.SetOutput(io.Discard)
	defer func() {
		logger.ReplaceHooks(oldHooks)
		logger.SetLevel(oldLevel)
		logger.SetOutput(oldOutput)
	}()
	response := `{"candidates":[{"content":{"role":"model","parts":[{"text":"short answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":87,"totalTokenCount":187}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer server.Close()
	executor := NewGeminiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	ctx := logging.WithRequestID(context.Background(), "short-response-87")
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-preview",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":4096}}`),
	}
	for _, level := range []log.Level{log.DebugLevel, log.InfoLevel} {
		logger.SetLevel(level)
		hook.Reset()
		resp, errExecute := executor.Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
		if errExecute != nil {
			t.Fatal(errExecute)
		}
		if gjson.GetBytes(resp.Payload, "usageMetadata.candidatesTokenCount").Int() != 87 {
			t.Fatal("diagnostics changed response usage")
		}
		count := 0
		for _, entry := range hook.AllEntries() {
			if entry.Message != "gemini non-streaming response summary" {
				continue
			}
			count++
			if entry.Data["request_id"] != "short-response-87" {
				t.Fatal("missing request correlation")
			}
			summary, ok := entry.Data["response_summary"].(string)
			if !ok || gjson.Get(summary, "candidate_tokens").Int() != 87 || gjson.Get(summary, "candidates.0.finish_reason").String() != "STOP" || gjson.Get(summary, "max_output_tokens").Int() != 4096 {
				t.Fatalf("missing response diagnostics: %v", entry.Data)
			}
		}
		wantCount := 0
		if level == log.DebugLevel {
			wantCount = 1
		}
		if count != wantCount {
			t.Fatalf("summary count at %s = %d, want %d", level, count, wantCount)
		}
	}
}
