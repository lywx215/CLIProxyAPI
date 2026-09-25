// Command diag-analyze analyzes offline diagnostic exports without contacting
// services. Trust is a command-line declaration, never a property of a record.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnosticanalyzer"
)

type listFlag []string

func (v *listFlag) String() string     { return "" }
func (v *listFlag) Set(s string) error { *v = append(*v, s); return nil }

const usage = `diag-analyze -input ALIAS=PATH [-input ALIAS=PATH ...] [-trust ALIAS] [-format text|json]
  -trace TRACE                         Select candidate trace (not a trusted tree).
  -caller-request-id ID                Select caller aliases; requires scope below.
  -environment TOKEN -deployment TOKEN -service cliproxyapi|gcli2api|aitoapi
  -caller-scope deployment|boot|unknown [-caller-alias TOKEN]
  -instance TOKEN -boot UUID           Required for boot scope.
  -known-loss ALIAS                     Operator evidence of missing export data.
  -unique-attempt-scope SERVICE/SCOPE    Explicit numeric attempt uniqueness attestation.
  -max-bytes N -max-lines N -max-records N -max-issues N   Lower hard limits only.
All aliases must be nonsensitive tokens. Paths and raw input errors are never printed.
Exit 0: scan finished; 1: report contains quarantine or a scan limit; 2: configuration/I/O error.
`

func run(args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("diag-analyze", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var inputs, trust, loss, scopes listFlag
	fs.Var(&inputs, "input", "")
	fs.Var(&trust, "trust", "")
	fs.Var(&loss, "known-loss", "")
	fs.Var(&scopes, "unique-attempt-scope", "")
	format := fs.String("format", "text", "")
	help := fs.Bool("help", false, "")
	var opts diagnosticanalyzer.Options
	opts.Limits = diagnosticanalyzer.DefaultLimits()
	q := &opts.Query
	fs.StringVar(&q.Trace, "trace", "", "")
	fs.StringVar(&q.CallerRequestID, "caller-request-id", "", "")
	fs.StringVar(&q.Environment, "environment", "", "")
	fs.StringVar(&q.DeploymentID, "deployment", "", "")
	fs.StringVar(&q.Service, "service", "", "")
	fs.StringVar(&q.CallerAliasScope, "caller-scope", "", "")
	fs.StringVar(&q.CallerAlias, "caller-alias", "", "")
	fs.StringVar(&q.InstanceID, "instance", "", "")
	fs.StringVar(&q.BootID, "boot", "", "")
	fs.Int64Var(&opts.Limits.Bytes, "max-bytes", opts.Limits.Bytes, "")
	fs.IntVar(&opts.Limits.Lines, "max-lines", opts.Limits.Lines, "")
	fs.IntVar(&opts.Limits.Records, "max-records", opts.Limits.Records, "")
	fs.IntVar(&opts.Limits.Issues, "max-issues", opts.Limits.Issues, "")
	fail := func(code string) int { _, _ = fmt.Fprintln(stderr, code); return 2 }
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			_, _ = io.WriteString(out, usage)
			return 0
		}
		return fail("invalid_arguments")
	}
	if *help {
		_, _ = io.WriteString(out, usage)
		return 0
	}
	if fs.NArg() != 0 || len(inputs) == 0 || len(inputs) > diagnosticanalyzer.MaxSources || (*format != "text" && *format != "json") {
		return fail("invalid_arguments")
	}
	paths := map[string]string{}
	order := []string{}
	for _, input := range inputs {
		alias, path, ok := strings.Cut(input, "=")
		if !ok || alias == "" || path == "" || paths[alias] != "" {
			return fail("invalid_source_config")
		}
		paths[alias] = path
		order = append(order, alias)
	}
	trusted, knownLoss := map[string]bool{}, map[string]bool{}
	for _, alias := range trust {
		if paths[alias] == "" {
			return fail("unknown_trust_source")
		}
		trusted[alias] = true
	}
	for _, alias := range loss {
		if paths[alias] == "" {
			return fail("unknown_loss_source")
		}
		knownLoss[alias] = true
	}
	files := []*os.File{}
	defer func() {
		for _, f := range files {
			if errClose := f.Close(); errClose != nil {
				_, _ = fmt.Fprintln(stderr, "input_close_error")
			}
		}
	}()
	sources := []diagnosticanalyzer.Input{}
	for i, alias := range order {
		// Reject ordinary nonregular paths before Open can block on a FIFO.
		// Keep the post-open check too; this preflight does not eliminate TOCTOU.
		st, err := os.Stat(paths[alias])
		if err != nil || !st.Mode().IsRegular() {
			return fail(fmt.Sprintf("input_%d: not_readable_regular_file", i+1))
		}
		f, err := os.Open(paths[alias])
		if err != nil {
			return fail(fmt.Sprintf("input_%d: open_error", i+1))
		}
		files = append(files, f)
		st, err = f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			return fail(fmt.Sprintf("input_%d: not_readable_regular_file", i+1))
		}
		size := st.Size()
		sources = append(sources, diagnosticanalyzer.Input{Alias: alias, Reader: f, Size: &size, Trusted: trusted[alias], KnownLoss: knownLoss[alias]})
	}
	opts.UniqueAttemptScopes = scopes
	report, err := diagnosticanalyzer.Analyze(sources, opts)
	if err != nil {
		return fail("analysis_configuration_error")
	}
	if *format == "json" {
		err = report.WriteJSON(out)
	} else {
		err = report.WriteText(out)
	}
	if err != nil {
		return fail("report_write_error")
	}
	if report.Limited || len(report.Issues) > 0 || report.IssuesOmitted > 0 {
		return 1
	}
	for _, s := range report.Sources {
		if !s.CompleteScan {
			return 1
		}
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
