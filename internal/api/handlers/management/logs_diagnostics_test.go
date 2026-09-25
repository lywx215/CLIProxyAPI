package management

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDiagnosticLogTimestampAndIncrementalConsumers(t *testing.T) {
	const line = `@diag {"diagnosticSchema":"ai-proxy-diagnostics/1","ts":"2026-09-25T08:00:02.000Z","level":"INFO"}`
	want := time.Date(2026, 9, 25, 8, 0, 2, 0, time.UTC).Unix()
	if parseTimestamp(line) != want {
		t.Fatal("diagnostic timestamp not parsed")
	}
	for _, invalid := range []string{`@diag invalid`, `@diag {"diagnosticSchema":"other","ts":"2026-09-25T08:00:02Z"}`, `@diag {"diagnosticSchema":"ai-proxy-diagnostics/1","ts":"bad"}`} {
		if parseTimestamp(invalid) != 0 {
			t.Fatal("malformed/unrecognized record accepted")
		}
	}
	old := "[" + time.Unix(want-2, 0).In(time.Local).Format("2006-01-02 15:04:05") + "] [info ] old"
	acc := newLogAccumulator(want-1, 0)
	acc.addLine(old)
	acc.addLine(line)
	lines, total, latest := acc.result()
	if !reflect.DeepEqual(lines, []string{line}) || total != 2 || latest != want {
		t.Fatalf("legacy cutoff = %v/%d/%d", lines, total, latest)
	}
	path := filepath.Join(t.TempDir(), defaultLogFileName)
	if err := os.WriteFile(path, []byte(old+"\n"+line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Cursor reads retain complete lines verbatim, independently of text prefixes.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := readCompleteLogLines(path, 0, info.Size(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.lines, []string{old, line}) || read.latest != want {
		t.Fatalf("cursor lines = %+v", read)
	}
}
