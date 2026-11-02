package executor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

func TestEnterprisePreflightDiagnosticsAreCorrelatedOnce(t *testing.T) {
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
	for _, stream := range []bool{false, true} {
		var buf bytes.Buffer
		logger.SetOutput(&buf)
		cfg := &config.Config{}
		cfg.Home.Enabled = true
		executor := NewGeminiEnterpriseExecutor(cfg)
		opts := enterpriseOptions(translator.FormatOpenAI)
		before := executor.validate(opts)
		ctx := logging.WithRequestID(t.Context(), "00000000-1111-2222-3333-444444444444")
		req := coreexec.Request{Model: "gemini-3.8-flash", Payload: []byte(`{"messages":[{"role":"user","content":"private-prompt"}]}`)}
		var err error
		if stream {
			_, err = executor.ExecuteStream(ctx, nil, req, opts)
		} else {
			_, err = executor.Execute(ctx, nil, req, opts)
		}
		if before == nil || err == nil || before.Error() != err.Error() {
			t.Fatal("diagnostics changed the preflight error")
		}
		text := buf.String()
		if strings.Count(text, "Enterprise request failed") != 1 || !strings.Contains(text, `diagnostic_source="local_validation"`) || !strings.Contains(text, `diagnostic_request_id="00000000-1111-2222-3333-444444444444"`) || strings.Contains(text, "private-prompt") {
			t.Fatalf("preflight diagnostic was duplicated, uncorrelated or unsafe: %s", text)
		}
	}
}
