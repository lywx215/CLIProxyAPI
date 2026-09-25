package diagnosticanalyzer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
)

const contract = "../../contracts/diagnostics/v1"

func readJSON(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, why := decodeUnique(b)
	if why != "" {
		t.Fatal(path, why)
	}
	return v
}
func fixture(t *testing.T, name string) object {
	t.Helper()
	return obj(readJSON(t, filepath.Join(contract, "fixtures", "valid", name+".json")))
}
func clone(r object) object { b, _ := json.Marshal(r); v, _ := decodeUnique(b); return obj(v) }
func lines(records ...object) string {
	var b strings.Builder
	for _, r := range records {
		v, _ := json.Marshal(r)
		b.Write(v)
		b.WriteByte('\n')
	}
	return b.String()
}
func analyze(t *testing.T, records ...object) *Report {
	t.Helper()
	r, err := Analyze([]Input{{Alias: "fixture", Reader: strings.NewReader(lines(records...)), Trusted: true}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Issues) > 0 {
		t.Fatalf("unexpected rejection: %+v", r.Issues)
	}
	return r
}
func number(n int) json.Number { return json.Number(fmt.Sprint(n)) }
func cover(r object) {
	r["logSeq"] = number(1)
	obj(obj(r["data"])["coverage"])["expectedLastLogSeq"] = number(1)
	c := obj(obj(r["data"])["coverage"])
	c["debugCapture"] = "enabled_throughout"
	c["accessCapture"] = "enabled_throughout"
	c["droppedForSpan"] = number(0)
	c["truncatedEvents"] = number(0)
}
func basic(t *testing.T, kind, span, owner string) object {
	t.Helper()
	r := fixture(t, kind)
	r["spanId"] = span
	r["serverSpanId"] = owner
	r["traceId"] = strings.Repeat("1", 32)
	r["environment"] = "test"
	r["deploymentId"] = "edge"
	r["service"] = "cliproxyapi"
	r["instanceId"] = "cpa-1"
	r["bootId"] = "10000000-0000-4000-8000-000000000001"
	r["requestId"] = "local-1"
	r["callerRequestId"] = nil
	r["callerAlias"] = nil
	r["callerAliasScope"] = "unknown"
	r["callerIdSource"] = object{"header": nil, "trust": "none", "rejected": "none"}
	r["attemptId"] = nil
	r["attemptNo"] = nil
	r["retryScope"] = nil
	r["contextSource"] = "generated"
	r["parentSpanId"] = nil
	if kind == "call" {
		r["parentSpanId"] = owner
		r["callNo"] = number(1)
		d := obj(r["data"])
		d["peerConfigured"] = false
		d["peerService"] = nil
		d["peerDeploymentId"] = nil
		d["peerRequestId"] = nil
		d["peerTraceId"] = nil
		d["peerIdRejected"] = "none"
	}
	cover(r)
	return r
}
func bilateral(t *testing.T) (object, object, object) {
	t.Helper()
	owner := basic(t, "server", "1111111111111111", "1111111111111111")
	call := basic(t, "call", "2222222222222222", "1111111111111111")
	server := basic(t, "server", "3333333333333333", "3333333333333333")
	server["service"] = "gcli2api"
	server["deploymentId"] = "pool"
	server["instanceId"] = "gcli-1"
	server["bootId"] = "20000000-0000-4000-8000-000000000002"
	server["requestId"] = "remote-1"
	server["contextSource"] = "accepted"
	server["parentSpanId"] = call["spanId"]
	server["callerRequestId"] = "local-1"
	server["callerIdSource"] = object{"header": "x-diag-request-id", "trust": "configured_peer", "rejected": "none"}
	d := obj(call["data"])
	d["targetAlias"] = "gcli-pool"
	d["peerConfigured"] = true
	d["peerService"] = "gcli2api"
	d["peerDeploymentId"] = "pool"
	d["peerRequestId"] = "remote-1"
	d["peerTraceId"] = server["traceId"]
	return owner, call, server
}
func verified(r *Report) int {
	n := 0
	for _, e := range r.Edges {
		if e.Verified {
			n++
		}
	}
	return n
}
func findings(r *Report) []string {
	out := []string{}
	for _, n := range r.Nodes {
		for _, s := range n.Findings {
			add(&out, s)
		}
	}
	for _, e := range r.Edges {
		for _, s := range e.Findings {
			add(&out, s)
		}
	}
	sort.Strings(out)
	return out
}

func TestFrozenSchemaAndFixtures(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(contract, "record.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, recordSchema) {
		t.Fatal("embedded schema differs")
	}
	m, err := os.ReadFile(filepath.Join(contract, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(m)
	if hex.EncodeToString(hash[:]) != ContractDigest {
		t.Fatal("manifest changed")
	}
	s, _ := decodeUnique(recordSchema)
	v, err := compileSchema(s, obj(s))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"valid", "invalid"} {
		paths, _ := filepath.Glob(filepath.Join(contract, "fixtures", group, "*.json"))
		for _, path := range paths {
			t.Run(group+"/"+filepath.Base(path), func(t *testing.T) {
				r := readJSON(t, path)
				if v(r) != (group == "valid") {
					t.Fatal("schema result")
				}
				if group == "valid" && len(semanticIssues(obj(r))) != 0 {
					t.Fatal("semantics")
				}
			})
		}
	}
	for _, raw := range readJSON(t, filepath.Join(contract, "fixtures/schema-cases.json")).([]any) {
		c := obj(raw)
		if c["schema"] == "record.schema.json" {
			t.Run(str(c["id"]), func(t *testing.T) {
				if v(c["input"]) != c["valid"] {
					t.Fatal("schema case")
				}
			})
		}
	}
}

func TestOfflineVectors(t *testing.T) {
	for _, name := range []string{"semantic", "source-scope", "coverage", "counts", "graph"} {
		for _, raw := range readJSON(t, filepath.Join(contract, "vectors", name+".json")).([]any) {
			c := obj(raw)
			t.Run(name+"/"+str(c["id"]), func(t *testing.T) {
				in := obj(c["input"])
				var actual any
				switch name {
				case "semantic":
					actual = semanticIssues(obj(in["record"]))
				case "source-scope":
					actual = compareScope(obj(in["left"]), obj(in["right"]))
				case "coverage":
					b, _ := json.Marshal(in)
					var e diagnostics.CoverageEvidence
					if err := json.Unmarshal(b, &e); err != nil {
						t.Fatal(err)
					}
					actual = diagnostics.AssessCoverage(e)
				case "counts":
					nodes := []*Node{}
					scopes := []string{}
					for i, raw := range in["calls"].([]any) {
						rec := obj(raw)
						if rec["attemptNoUnique"] == true {
							add(&scopes, str(rec["service"])+"/"+str(rec["retryScope"]))
						}
						nodes = append(nodes, &Node{ID: i + 1, Resource: resource(rec), SpanID: str(rec["spanId"]), ServerSpanID: str(rec["serverSpanId"]), Kind: "call", records: []*Evidence{{Record: rec}}})
					}
					out := countNodes(nodes, scopes)
					actual = map[string]int{"observedServerOwners": out.ObservedServerOwners, "callCount": out.Calls, "knownAttemptCount": out.Attempts, "unknownAttemptCallCount": out.UnknownAttemptCalls}
				case "graph":
					testGraphVector(t, in, obj(c["expected"]))
					return
				}
				b, _ := json.Marshal(actual)
				v, _ := decodeUnique(b)
				if canonical(v) != canonical(c["expected"]) {
					t.Fatalf("actual %s expected %v", b, c["expected"])
				}
			})
		}
	}
}

func testGraphVector(t *testing.T, in, expected object) {
	t.Helper()
	sources := []Input{}
	boots := map[string]string{}
	for i, raw := range in["records"].([]any) {
		p := obj(raw)
		r := basic(t, str(p["kind"]), str(p["spanId"]), str(p["spanId"]))
		for _, key := range []string{"traceId", "spanId", "parentSpanId", "service", "instanceId", "logSeq", "deploymentId", "requestId"} {
			r[key] = p[key]
		}
		boot := str(p["bootId"])
		if boots[boot] == "" {
			boots[boot] = fmt.Sprintf("%08d-0000-4000-8000-000000000001", len(boots)+1)
		}
		r["bootId"] = boots[boot]
		r["contextSource"] = "generated"
		if r["parentSpanId"] != nil {
			r["contextSource"] = "accepted"
		}
		if p["kind"] == "call" {
			r["serverSpanId"] = r["parentSpanId"]
			d := obj(r["data"])
			for _, key := range []string{"peerConfigured", "peerService", "peerDeploymentId", "peerRequestId", "peerTraceId"} {
				d[key] = p[key]
			}
			if d["peerConfigured"] == true {
				d["targetAlias"] = "peer"
			}
		} else {
			r["serverSpanId"] = r["spanId"]
			r["callerRequestId"] = p["callerRequestId"]
			r["callerIdSource"] = object{"header": p["callerHeader"], "trust": p["callerTrust"], "rejected": "none"}
		}
		obj(obj(r["data"])["coverage"])["expectedLastLogSeq"] = r["logSeq"]
		sources = append(sources, Input{Alias: fmt.Sprintf("source%d", i), Reader: strings.NewReader(lines(r)), Trusted: p["sourceTrusted"] == true})
	}
	r, err := Analyze(sources, Options{})
	if err != nil || len(r.Issues) > 0 {
		t.Fatalf("rejected %v %+v", err, r.Issues)
	}
	got := findings(r)
	for _, f := range expected["findings"].([]any) {
		if !contains(got, str(f)) {
			t.Fatalf("missing %s in %v", f, got)
		}
	}
	pairs := []any{}
	for _, e := range r.Edges {
		if e.Verified {
			pairs = append(pairs, []any{r.Nodes[e.Senders[0]-1].SpanID, r.Nodes[e.Receivers[0]-1].SpanID})
		}
	}
	if canonical(pairs) != canonical(expected["verifiedEdges"]) {
		t.Fatalf("verified pairs %v expected %v", pairs, expected["verifiedEdges"])
	}
	ids := map[string]bool{}
	for _, e := range r.Evidence {
		ids[e.Identity] = true
	}
	if len(ids) != int(*num(expected["distinctEvents"])) {
		t.Fatal("identity count")
	}
}

func TestBilateralConflictsTrustAndClock(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func(object, object, object)
	}{
		{"valid", "verified", func(o, c, s object) {}},
		{"clock-skew", "verified", func(o, c, s object) { c["ts"] = "2026-09-25T23:00:00.000Z"; s["ts"] = "2026-09-24T01:00:00.000Z" }},
		{"optional-response", "verified", func(o, c, s object) { obj(c["data"])["peerRequestId"] = nil; obj(c["data"])["peerTraceId"] = nil }},
		{"context", "context_mismatch", func(o, c, s object) { obj(c["data"])["peerTraceId"] = strings.Repeat("2", 32) }},
		{"peer", "peer_conflict", func(o, c, s object) { obj(c["data"])["peerService"] = "aitoapi" }},
		{"request", "peer_conflict", func(o, c, s object) { s["callerRequestId"] = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, c, s := bilateral(t)
			tc.mutate(o, c, s)
			r := analyze(t, o, c, s)
			if !contains(findings(r), tc.want) {
				t.Fatal(findings(r))
			}
			if (verified(r) == 1) != (tc.want == "verified") {
				t.Fatal("verification")
			}
		})
	}
	o, c, s := bilateral(t)
	r, err := Analyze([]Input{{Alias: "controlled", Reader: strings.NewReader(lines(o, c, s)), Trusted: true}, {Alias: "unknown", Reader: strings.NewReader(lines(s))}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if verified(r) != 0 || len(r.Evidence) != 3 || len(r.Evidence[2].Provenance) != 2 {
		t.Fatal("duplicate import upgraded trust or lost provenance")
	}
	r = analyze(t, o, c, s, s)
	if verified(r) != 1 || len(r.Evidence) != 3 || r.Counts.Requests != 2 {
		t.Fatal("duplicate import")
	}
	other := clone(s)
	obj(other["data"])["endReason"] = "error"
	r = analyze(t, o, c, s, other)
	if verified(r) != 0 || len(r.Evidence) != 4 || !contains(findings(r), "event_conflict") {
		t.Fatal("conflict evidence")
	}
	s2 := clone(s)
	s2["spanId"] = "4444444444444444"
	s2["serverSpanId"] = s2["spanId"]
	r = analyze(t, o, c, s, s2)
	if verified(r) != 0 || !contains(findings(r), "ambiguous_parent") {
		t.Fatal("ambiguous")
	}
}

func TestLateOwnerConflictCoverage(t *testing.T) {
	for _, field := range []string{"traceId", "requestId"} {
		t.Run(field, func(t *testing.T) {
			o, c, _ := bilateral(t)
			if field == "traceId" {
				c[field] = strings.Repeat("2", 32)
			} else {
				c[field] = "different-local-request"
			}
			obj(c["data"])["peerTraceId"] = nil
			r := analyze(t, o, c)
			for _, n := range r.Nodes {
				if n.Coverage.DebugCoverage != "partial" || !contains(n.Findings, "span_conflict") {
					t.Fatalf("late owner conflict retains full: %+v", n)
				}
			}
		})
	}
}

func TestCyclesAndIndependentEdges(t *testing.T) {
	o, c, s := bilateral(t)
	back := basic(t, "call", "4444444444444444", str(s["spanId"]))
	for _, k := range []string{"service", "deploymentId", "instanceId", "bootId", "requestId"} {
		back[k] = s[k]
	}
	back["contextSource"] = "accepted"
	d := obj(back["data"])
	d["peerConfigured"] = true
	d["targetAlias"] = "edge"
	d["peerService"] = "cliproxyapi"
	d["peerDeploymentId"] = "edge"
	o["parentSpanId"] = back["spanId"]
	o["contextSource"] = "accepted"
	r := analyze(t, o, c, s, back)
	if verified(r) != 0 || !contains(findings(r), "cycle") {
		t.Fatal("cycle verified")
	}
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil || b.Len() > 20000 {
		t.Fatal("cycle rendering")
	}
	o, c, s = bilateral(t)
	bad := basic(t, "call", "5555555555555555", str(o["spanId"]))
	obj(bad["data"])["peerConfigured"] = true
	obj(bad["data"])["peerService"] = "aitoapi"
	obj(bad["data"])["targetAlias"] = "aito"
	r = analyze(t, o, c, s, bad)
	if verified(r) != 1 || !contains(findings(r), "missing_peer") {
		t.Fatal("unrelated gap poisoned edge")
	}
}

func TestQueryCannotHideCrossTraceCollision(t *testing.T) {
	o, c, s := bilateral(t)
	forged := clone(s)
	forged["traceId"] = strings.Repeat("2", 32)
	r, err := Analyze([]Input{{Alias: "controlled", Reader: strings.NewReader(lines(o, c, s, forged)), Trusted: true}}, Options{Query: Query{Trace: str(s["traceId"])}})
	if err != nil {
		t.Fatal(err)
	}
	if verified(r) != 0 || !contains(findings(r), "span_conflict") || len(r.Evidence) != 4 {
		t.Fatal("filter hid conflicting payload")
	}
}

func TestCoverageGapsAndHugeCounters(t *testing.T) {
	o, c, _ := bilateral(t)
	o["logSeq"] = json.Number("9007199254740991")
	obj(obj(o["data"])["coverage"])["expectedLastLogSeq"] = o["logSeq"]
	r := analyze(t, o, c)
	if r.Nodes[0].Coverage.DebugCoverage != "partial" {
		t.Fatal("huge sequence gap")
	}
	o, c, _ = bilateral(t)
	obj(obj(o["data"])["coverage"])["debugCapture"] = "interrupted"
	r = analyze(t, o, c)
	if r.Nodes[0].Coverage.DebugCoverage != "partial" {
		t.Fatal("interruption")
	}
	stub := clone(o)
	stub["event"] = "diag.truncated"
	stub["data"] = object{"originalEvent": "diag.server", "originalRecordKind": "basic", "reason": "line_limit"}
	r = analyze(t, stub, c)
	if r.Nodes[0].Coverage.TerminalMissing || r.Nodes[0].Coverage.DebugCoverage != "partial" {
		t.Fatal("stub")
	}
	debug := fixture(t, "request.normalized")
	debug["logSeq"] = number(1)
	r = analyze(t, debug)
	if !r.Nodes[0].Coverage.TerminalMissing || r.Nodes[0].Coverage.DebugCoverage != "unknown" {
		t.Fatal("missing terminal")
	}
}

func TestCallerQueriesRemainAliases(t *testing.T) {
	a := basic(t, "server", "1111111111111111", "1111111111111111")
	a["callerRequestId"] = "shared"
	a["callerAlias"] = "principal-a"
	a["callerAliasScope"] = "deployment"
	a["callerIdSource"] = object{"header": "x-request-id", "trust": "authenticated", "rejected": "none"}
	b := clone(a)
	b["traceId"] = strings.Repeat("2", 32)
	b["spanId"] = "2222222222222222"
	b["serverSpanId"] = b["spanId"]
	b["callerAlias"] = "principal-b"
	c := clone(b)
	c["traceId"] = strings.Repeat("3", 32)
	c["spanId"] = "3333333333333333"
	c["serverSpanId"] = c["spanId"]
	c["callerAlias"] = nil
	c["callerAliasScope"] = "unknown"
	q := Query{CallerRequestID: "shared", Environment: "test", DeploymentID: "edge", Service: "cliproxyapi", CallerAliasScope: "deployment", CallerAlias: "principal-a"}
	r, err := Analyze([]Input{{Alias: "input", Reader: strings.NewReader(lines(a, b, c)), Trusted: true}}, Options{Query: q})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.CandidateTraces) != 2 || len(r.AliasCandidates) != 2 || verified(r) != 0 {
		t.Fatal("caller scope or merging")
	}
	q.CallerAliasScope = "unknown"
	q.CallerAlias = ""
	r, err = Analyze([]Input{{Alias: "input", Reader: strings.NewReader(lines(a, b, c))}}, Options{Query: q})
	if err != nil || len(r.CandidateTraces) != 3 {
		t.Fatal("unknown must widen")
	}
}

func TestAttemptIDsAndLateCall(t *testing.T) {
	o := basic(t, "server", "1111111111111111", "1111111111111111")
	records := []object{o}
	for i, no := range []int{1, 2, 1, 2} {
		c := basic(t, "call", fmt.Sprintf("%016x", i+2), str(o["spanId"]))
		c["attemptId"] = fmt.Sprintf("attempt-%d", i)
		c["attemptNo"] = number(no)
		c["retryScope"] = "continuation"
		c["ts"] = "2026-09-26T01:00:00.000Z"
		records = append(records, c)
	}
	r := analyze(t, records...)
	if r.Counts.Requests != 1 || r.Counts.Calls != 4 || r.Counts.Attempts != 4 {
		t.Fatal(r.Counts)
	}
	compaction := clone(records[1])
	compaction["spanId"] = "9999999999999999"
	records = append(records, compaction)
	r = analyze(t, records...)
	if r.Counts.Attempts != 4 || r.Counts.Calls != 5 {
		t.Fatal("compaction counted as attempt")
	}
	boot := clone(o)
	boot["bootId"] = "30000000-0000-4000-8000-000000000003"
	boot["spanId"] = "eeeeeeeeeeeeeeee"
	boot["serverSpanId"] = boot["spanId"]
	records = append(records, boot)
	r = analyze(t, records...)
	if r.Counts.Requests != 2 || len(r.ByResource) != 2 {
		t.Fatal("reboot merged")
	}
}

func TestQuarantineAndBoundedScanning(t *testing.T) {
	good := lines(basic(t, "server", "1111111111111111", "1111111111111111"))
	secret := "FORBIDDEN_SECRET_BODY"
	bad := strings.TrimSuffix(good, "}\n") + `,"extra":"` + secret + `"}` + "\n"
	duplicate := strings.Replace(good, `"event":`, `"secret":"`+secret+`","event":"diag.server","event":`, 1)
	unknown := strings.Replace(good, SchemaVersion, "ai-proxy-diagnostics/99", 1)
	input := secret + " legacy text\n" + "@diag {bad:" + secret + "}\n" + bad + duplicate + unknown + "@diag " + string([]byte{0xff}) + "\n" + strings.Repeat("x", 1<<20) + "\n@diag " + good
	r, err := Analyze([]Input{{Alias: "clean-alias", Reader: strings.NewReader(input), Trusted: true}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Evidence) != 1 || len(r.Issues) != 6 || r.Sources[0].LegacyLines != 1 {
		t.Fatalf("bad quarantine %+v", r.Issues)
	}
	var b bytes.Buffer
	_ = r.WriteJSON(&b)
	_ = r.WriteText(&b)
	if strings.Contains(b.String(), secret) || strings.Contains(b.String(), "extra") {
		t.Fatal("raw input leaked")
	}
	for _, limit := range []Limits{{Bytes: 200, Lines: 100, Records: 100, Issues: 2}, {Bytes: MaxBytes, Lines: 2, Records: 100, Issues: 2}, {Bytes: MaxBytes, Lines: 100, Records: 1, Issues: 2}} {
		r, err := Analyze([]Input{{Alias: "input", Reader: strings.NewReader(good + good + good)}}, Options{Limits: limit})
		if err != nil || !r.Limited || r.Sources[0].CompleteScan {
			t.Fatal("unmarked bound")
		}
	}
	r, err = Analyze([]Input{{Alias: "input", Reader: strings.NewReader(strings.Repeat("@diag {bad}\n", 10))}}, Options{Limits: Limits{Bytes: MaxBytes, Lines: 100, Records: 100, Issues: 2}})
	if err != nil || len(r.Issues) != 2 || r.IssuesOmitted != 8 {
		t.Fatal("issue bound")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, fmt.Errorf("FORBIDDEN_READER_ERROR") }
func TestSafeSourceFailures(t *testing.T) {
	r, err := Analyze([]Input{{Alias: "safe", Reader: failingReader{}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	_ = r.WriteJSON(&b)
	if strings.Contains(b.String(), "FORBIDDEN") || r.Sources[0].CompleteScan {
		t.Fatal("reader error leak")
	}
	_, err = Analyze([]Input{{Alias: "FORBIDDEN\nsource", Reader: strings.NewReader("")}}, Options{})
	if err == nil || strings.Contains(err.Error(), "FORBIDDEN") {
		t.Fatal("alias error leak")
	}
}

func TestProducerSamples(t *testing.T) {
	for _, tc := range []struct {
		file    string
		records int
	}{{"gcli-zero.jsonl", 6}, {"gcli-incomplete.jsonl", 6}, {"aito-r1.jsonl", 67}, {"cpa-r2-read.jsonl", 70}, {"cpa-r2-limits.jsonl", 224}} {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			size := int64(len(b))
			r, err := Analyze([]Input{{Alias: "producer", Reader: bytes.NewReader(b), Size: &size, Trusted: true}}, Options{})
			if err != nil || len(r.Issues) > 0 {
				t.Fatalf("producer rejected %v %+v", err, r.Issues)
			}
			if r.Sources[0].Accepted != tc.records {
				t.Fatalf("records %d", r.Sources[0].Accepted)
			}
			if !r.Sources[0].CompleteScan {
				t.Fatal("scan")
			}
			// Every accepted payload survives structurally unchanged, including null
			// versus observed zero, result classifications and separate usage bases.
			roundtrip := []any{}
			for _, e := range append(r.ProcessEvidence, r.Evidence...) {
				roundtrip = append(roundtrip, e.Record)
			}
			if len(roundtrip) != tc.records {
				t.Fatal("producer evidence lost")
			}
		})
	}
}

func TestNearGcliCapIsOnlySuspicion(t *testing.T) {
	raw, err := os.ReadFile("testdata/gcli-zero.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	size := int64(16777216 - 4096)
	padding := size - int64(len(raw))
	pad := strings.Repeat(strings.Repeat(" ", 4095)+"\n", int(padding/4096)) + strings.Repeat(" ", int(padding%4096))
	reader := io.MultiReader(bytes.NewReader(raw), strings.NewReader(pad))
	r, err := Analyze([]Input{{Alias: "gcli", Reader: reader, Size: &size, Trusted: true}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Sources[0]
	if !s.CompleteScan || !contains(s.Findings, "suspected_tail_gap_near_producer_limit") || s.KnownLoss || len(r.Issues) > 0 {
		t.Fatal("cap assertion")
	}
}

func TestObservedGapWithoutTerminal(t *testing.T) {
	a := fixture(t, "request.normalized")
	a["logSeq"] = number(1)
	b := clone(a)
	b["logSeq"] = number(3)
	r := analyze(t, a, b)
	if !r.Nodes[0].Coverage.TerminalMissing || r.Nodes[0].Coverage.DebugCoverage != "partial" {
		t.Fatal("observed gap hidden by absent terminal")
	}
}

func TestProcessConflictBlocksRelatedEdges(t *testing.T) {
	o, c, s := bilateral(t)
	p := fixture(t, "process")
	for _, k := range []string{"environment", "deploymentId", "service", "instanceId", "bootId"} {
		p[k] = o[k]
	}
	other := clone(p)
	other["deploymentId"] = "forged"
	r := analyze(t, p, o, c, s, other)
	if verified(r) != 0 || !contains(findings(r), "process_identity_conflict") {
		t.Fatal("process conflict ignored")
	}
	r, err := Analyze([]Input{{Alias: "controlled", Reader: strings.NewReader(lines(p, o, c, s, other)), Trusted: true}}, Options{Query: Query{Trace: str(o["traceId"])}})
	if err != nil || len(r.ProcessEvidence) != 2 || !r.ProcessEvidence[0].Conflict || !r.ProcessEvidence[1].Conflict || verified(r) != 0 {
		t.Fatal("query concealed process conflict variant")
	}
}

func TestQueryRetainsLateOwnerConflictContext(t *testing.T) {
	o, c, _ := bilateral(t)
	c["traceId"] = strings.Repeat("2", 32)
	obj(c["data"])["peerTraceId"] = nil
	r, err := Analyze([]Input{{Alias: "input", Reader: strings.NewReader(lines(o, c)), Trusted: true}}, Options{Query: Query{Trace: str(c["traceId"])}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 2 || r.Counts.Requests != 0 || r.Counts.Calls != 1 || !r.Nodes[0].ContextOnly {
		t.Fatal("dangling filtered conflict or inflated totals")
	}
	for _, n := range r.Nodes {
		if n.Coverage.DebugCoverage != "partial" {
			t.Fatal("conflict coverage")
		}
	}
}

func TestCanonicalNumbersDoNotHideConflict(t *testing.T) {
	a, _ := decodeUnique([]byte(`{"x":1,"y":0.1234567890123456789}`))
	b, _ := decodeUnique([]byte(`{"x":1.0,"y":0.1234567890123456789}`))
	c, _ := decodeUnique([]byte(`{"x":1,"y":0.1234567890123456788}`))
	if canonical(a) != canonical(b) || canonical(a) == canonical(c) {
		t.Fatal("numeric equality")
	}
	for _, input := range []string{`{"x":1e999999999}`, `{"a":{"x":1,"x":2}}`} {
		if _, why := decodeUnique([]byte(input)); why == "" {
			t.Fatal("unsafe parser")
		}
	}
}

func TestOutputStableAndNoMutation(t *testing.T) {
	o, c, s := bilateral(t)
	r := analyze(t, o, c, s)
	before, _ := json.Marshal(r)
	var b bytes.Buffer
	_ = r.WriteText(&b)
	after, _ := json.Marshal(r)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("render changed evidence")
	}
}

func TestAmbiguousOwnerGroupHasLinearEndpoints(t *testing.T) {
	const size = 300
	records := make([]object, 0, 2*size)
	for i := 0; i < size; i++ {
		o := basic(t, "server", "1111111111111111", "1111111111111111")
		o["requestId"] = fmt.Sprintf("owner-%d", i)
		records = append(records, o)
	}
	for i := 0; i < size; i++ {
		c := basic(t, "call", fmt.Sprintf("%016x", i+2), "1111111111111111")
		records = append(records, c)
	}
	r := analyze(t, records...)
	endpoints := 0
	for _, e := range r.Edges {
		endpoints += len(e.Senders) + len(e.Receivers)
	}
	if endpoints > 4*len(records) || verified(r) != 0 || !contains(findings(r), "ambiguous_parent") {
		t.Fatalf("quadratic expansion: %d endpoints", endpoints)
	}
	if len(r.Evidence) != len(records) || len(r.Nodes) != len(records) {
		t.Fatal("bounded graph lost evidence")
	}
}

func TestAmbiguousLocalOwnerCannotVerifyRelatedRemoteEdge(t *testing.T) {
	o, c, s := bilateral(t)
	other := clone(o)
	other["requestId"] = "different-owner"
	r := analyze(t, o, c, s, other)
	if verified(r) != 0 || !contains(findings(r), "ambiguous_parent") {
		t.Fatal("ambiguous local owner upgraded related remote edge")
	}
}

func TestLimitCannotVerifyIncompleteImport(t *testing.T) {
	o, c, s := bilateral(t)
	r, err := Analyze([]Input{{Alias: "first", Reader: strings.NewReader(lines(o, c, s)), Trusted: true}, {Alias: "unread", Reader: strings.NewReader(lines(s)), Trusted: true}}, Options{Limits: Limits{Bytes: MaxBytes, Lines: MaxLines, Records: 3, Issues: MaxIssues}})
	if err != nil || !r.Limited || verified(r) != 0 {
		t.Fatal("bounded import pretends unique")
	}
}

func TestOtherSourceGapBlocksVerification(t *testing.T) {
	o, c, s := bilateral(t)
	unrelated := basic(t, "server", "4444444444444444", "4444444444444444")
	unrelated["traceId"] = strings.Repeat("4", 32)
	for _, tc := range []struct {
		name, finding, sourceReason string
		prefix                      string
		readError, sizeChanged      bool
		knownLoss                   bool
	}{
		{name: "empty-read-error", finding: "source_scan_incomplete", sourceReason: "read_error", readError: true},
		{name: "later-read-error", finding: "source_scan_incomplete", sourceReason: "read_error", prefix: lines(unrelated), readError: true},
		{name: "empty-size-changed", finding: "source_scan_incomplete", sourceReason: "size_changed", sizeChanged: true},
		{name: "later-size-changed", finding: "source_scan_incomplete", sourceReason: "size_changed", prefix: lines(unrelated), sizeChanged: true},
		{name: "empty-known-loss", finding: "export_known_loss", sourceReason: "export_known_loss", knownLoss: true},
		{name: "later-known-loss", finding: "export_known_loss", sourceReason: "export_known_loss", prefix: lines(unrelated), knownLoss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(tc.prefix)
			if tc.readError {
				reader = io.MultiReader(reader, failingReader{})
			}
			b := Input{Alias: "other", Reader: reader, Trusted: true, KnownLoss: tc.knownLoss}
			if tc.sizeChanged {
				size := int64(len(tc.prefix) + 1)
				b.Size = &size
			}
			r, err := Analyze([]Input{{Alias: "pair", Reader: strings.NewReader(lines(o, c, s)), Trusted: true}, b}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if verified(r) != 0 || !contains(findings(r), tc.finding) {
				t.Errorf("other source gap still permits verification: verified=%d findings=%v", verified(r), findings(r))
			}
			if r.Limited || contains(findings(r), "analysis_limited") || !contains(r.Sources[1].Findings, tc.sourceReason) {
				t.Error("source evidence gap must retain its cause, not become a quota limit")
			}
			if r.Sources[1].CompleteScan != tc.knownLoss {
				t.Error("source scan status changed")
			}
			wantEvents := 3
			if tc.prefix != "" {
				wantEvents++
				if r.Nodes[3].Coverage.DebugCoverage != "partial" {
					t.Error("read evidence lost partial coverage")
				}
			}
			if len(r.Evidence) != wantEvents {
				t.Error("read evidence discarded")
			}
			if tc.readError {
				wantLine := 1
				if tc.prefix != "" {
					wantLine++
				}
				if len(r.Issues) != 1 || r.Issues[0].Line != wantLine || r.Issues[0].Bytes != 0 || r.Issues[0].LengthComplete {
					t.Errorf("zero-byte failure must name next line: %+v", r.Issues)
				}
			}
		})
	}
}
