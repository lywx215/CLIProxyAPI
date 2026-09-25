package logging

import (
	"bytes"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tui"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestDiagnosticFormatterUsesUnifiedWriterWithoutTextPrefix(t *testing.T) {
	logger := log.New()
	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetFormatter(&LogFormatter{})
	line := diagnosticLine("@diag {\"diagnosticSchema\":\"ai-proxy-diagnostics/1\"}\n")
	logger.WithField("diagnostics_line", line).Info("")
	if !bytes.Equal(output.Bytes(), line) {
		t.Fatalf("extra formatter prefix: %q", output.String())
	}
	output.Reset()
	logger.SetLevel(log.WarnLevel)
	logger.WithField("diagnostics_line", line).Info("")
	if output.Len() != 0 {
		t.Fatal("INFO access gate bypassed")
	}
	output.Reset()
	logger.SetLevel(log.DebugLevel)
	logger.WithField("diagnostics_line", line).Info("")
	if !bytes.Equal(output.Bytes(), line) {
		t.Fatal("DEBUG changes basic framing")
	}
}

func TestDiagnosticProductionHookFormattersPreserveLine(t *testing.T) {
	logger := log.New()
	line := diagnosticLine("@diag {\"diagnosticSchema\":\"ai-proxy-diagnostics/1\",\"level\":\"INFO\"}\n")
	entry := logger.WithField("diagnostics_line", line)
	entry.Level = log.InfoLevel
	forwarder := &HomeAppLogForwarder{formatter: &LogFormatter{}}
	got, err := forwarder.formatEntry(entry)
	if err != nil || got != string(line) {
		t.Fatalf("Home format: %q, %v", got, err)
	}
	hook := tui.NewLogHook(1)
	// Match cmd/server/main.go's production installation.
	hook.SetFormatter(&LogFormatter{})
	if err := hook.Fire(entry); err != nil {
		t.Fatal(err)
	}
	if got := <-hook.Chan(); got != strings.TrimSuffix(string(line), "\n") {
		t.Fatalf("TUI format: %q", got)
	}
}
