package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sample = "../../contracts/diagnostics/v1/examples/bilateral-basic.jsonl"

func TestCLI(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run([]string{"-input", "sample=" + sample, "-trust", "sample", "-format", format}, &out, &errOut)
			if code != 0 || errOut.Len() != 0 {
				t.Fatalf("code=%d stderr=%s", code, errOut.String())
			}
			if format == "json" {
				var r map[string]any
				if err := json.Unmarshal(out.Bytes(), &r); err != nil {
					t.Fatal(err)
				}
				if r["contractSha256"] == nil {
					t.Fatal("report missing")
				}
			} else if !strings.Contains(out.String(), "verified") {
				t.Fatal(out.String())
			}
		})
	}
	for _, args := range [][]string{
		{"-input", "sample=" + sample, "-trust", "FORBIDDEN_SECRET"},
		{"-input", "safe=FORBIDDEN_SECRET_PATH"},
		{"-input", "unsafe\nFORBIDDEN_SECRET=" + sample},
		{"-unexpected-FORBIDDEN_SECRET"},
		{"-max-records", "FORBIDDEN_SECRET"},
		{"-input", "safe=" + sample, "-caller-request-id", "FORBIDDEN_SECRET"},
	} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != 2 || strings.Contains(out.String()+errOut.String(), "FORBIDDEN") {
			t.Fatalf("unsafe error code=%d out=%s err=%s", code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"-input", "sample=" + sample, "-format", "json", "-max-records", "1"}, &out, &errOut); code != 1 || !strings.Contains(out.String(), `"limited": true`) {
		t.Fatal("limit exit/status")
	}
}

func TestActualCommand(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "diag-analyze.exe")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, trusted := range []bool{false, true} {
		args := []string{"-input", "sample=" + sample, "-format", "json"}
		if trusted {
			args = append(args, "-trust", "sample")
		}
		cmd := exec.Command(binary, args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command: %v\n%s", err, output)
		}
		var report struct {
			Edges []struct {
				Verified bool `json:"verified"`
			} `json:"edges"`
		}
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, edge := range report.Edges {
			if edge.Verified {
				count++
			}
		}
		if (count > 0) != trusted {
			t.Fatal("operator trust not enforced")
		}
	}
	bad := filepath.Join(t.TempDir(), "FORBIDDEN_SECRET_PATH.jsonl")
	if err := os.WriteFile(bad, []byte("@diag {FORBIDDEN_SECRET_BODY}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-input", "local="+bad, "-format", "json")
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 || bytes.Contains(output, []byte("FORBIDDEN")) {
		t.Fatalf("quarantine command: %v %s", err, output)
	}
	cmd = exec.Command(binary, "-input", "local="+t.TempDir())
	output, err = cmd.CombinedOutput()
	exit, ok = err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || string(output) != "input_1: not_readable_regular_file\n" {
		t.Fatalf("nonregular directory: %v %s", err, output)
	}
}
