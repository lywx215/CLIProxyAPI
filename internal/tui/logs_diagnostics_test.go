package tui

import "testing"

func TestDiagnosticBasicLogDisplayAndFilters(t *testing.T) {
	const line = `@diag {"diagnosticSchema":"ai-proxy-diagnostics/1","level":"INFO"}`
	for _, filter := range []string{"", "info", "warn", "error"} {
		m := logsTabModel{filter: filter}
		if got, want := m.matchLevel(line), filter == "" || filter == "info"; got != want {
			t.Fatalf("filter %q = %v", filter, got)
		}
		if m.styleLine(line) != line {
			t.Fatal("diagnostic bytes changed by styling")
		}
	}
}
