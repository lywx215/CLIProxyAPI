package logging

import (
	"bytes"
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
