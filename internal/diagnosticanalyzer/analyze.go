package diagnosticanalyzer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
var instancePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var bootPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var tracePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func regexpTrace(s string) bool  { return tracePattern.MatchString(s) && s != strings.Repeat("0", 32) }
func validService(s string) bool { return s == "cliproxyapi" || s == "gcli2api" || s == "aitoapi" }

// Query aliases select candidate traces, never merge requests. An unknown
// principal/deployment keeps all matching aliases visible as widened candidates.
type Query struct {
	Trace            string `json:"trace,omitempty"`
	CallerRequestID  string `json:"callerRequestId,omitempty"`
	Environment      string `json:"environment,omitempty"`
	DeploymentID     string `json:"deploymentId,omitempty"`
	Service          string `json:"service,omitempty"`
	CallerAliasScope string `json:"callerAliasScope,omitempty"`
	CallerAlias      string `json:"callerAlias,omitempty"`
	InstanceID       string `json:"instanceId,omitempty"`
	BootID           string `json:"bootId,omitempty"`
}

type Options struct {
	Query  Query
	Limits Limits
	// No producer currently needs a numeric fallback. Each opt-in is an
	// operator attestation about a specific service/retryScope, never log data.
	UniqueAttemptScopes []string
}

type ScopeComparison struct {
	SameLookupScope     bool `json:"sameLookupScope"`
	CandidateAliasMatch bool `json:"candidateAliasMatch"`
	Merge               bool `json:"merge"`
	ScopeUnknown        bool `json:"scopeUnknown"`
}

func compareScope(a, b object) ScopeComparison {
	unknown := func(r object) bool {
		return r["callerAlias"] == nil || r["callerAliasScope"] == "unknown" || r["deploymentId"] == "unassigned"
	}
	u := unknown(a) || unknown(b)
	same := !u
	for _, k := range []string{"environment", "deploymentId", "service", "callerAliasScope", "callerAlias"} {
		same = same && a[k] == b[k]
	}
	if a["callerAliasScope"] == "boot" || b["callerAliasScope"] == "boot" {
		same = same && a["instanceId"] == b["instanceId"] && a["bootId"] == b["bootId"]
	}
	return ScopeComparison{same, a["callerRequestId"] != nil && a["callerRequestId"] == b["callerRequestId"], false, u}
}

type Node struct {
	ID                 int                            `json:"id"`
	Resource           Resource                       `json:"resource"`
	Kind               string                         `json:"kind"`
	TraceID            string                         `json:"traceId"`
	SpanID             string                         `json:"spanId"`
	ParentSpanID       string                         `json:"parentSpanId"`
	ServerSpanID       string                         `json:"serverSpanId"`
	RequestID          string                         `json:"requestId"`
	Events             []int                          `json:"events"`
	Trusted            bool                           `json:"trusted"`
	Findings           []string                       `json:"findings"`
	Coverage           diagnostics.CoverageAssessment `json:"coverage"`
	TerminalCount      uint64                         `json:"terminalCount"`
	TerminalStubCount  uint64                         `json:"terminalStubCount"`
	UsageQualification string                         `json:"usageQualification"`
	ContextOnly        bool                           `json:"contextOnly"`
	ObservedLocalCalls *int                           `json:"observedLocalCalls"`
	records            []*Evidence
}

type Edge struct {
	Kind      string   `json:"kind"`
	Senders   []int    `json:"senders"`
	Receivers []int    `json:"receivers"`
	Findings  []string `json:"findings"`
	Verified  bool     `json:"verified"`
}

type Counts struct {
	Requests             int  `json:"requests"`
	ObservedServerOwners int  `json:"observedServerOwners"`
	Calls                int  `json:"callCount"`
	Attempts             int  `json:"knownAttemptCount"`
	UnknownAttemptCalls  int  `json:"unknownAttemptCallCount"`
	RequestFailures      int  `json:"requestFailures"`
	CallFailures         int  `json:"callFailures"`
	AttemptFailures      int  `json:"attemptFailures"`
	Conflicted           bool `json:"conflicted"`
}

type ResourceCounts struct {
	Resource Resource `json:"resource"`
	Counts   Counts   `json:"counts"`
}
type Report struct {
	ReportVersion    string           `json:"reportVersion"`
	DiagnosticSchema string           `json:"diagnosticSchema"`
	ContractSHA256   string           `json:"contractSha256"`
	Query            Query            `json:"query"`
	CandidateTraces  []string         `json:"candidateTraces"`
	AliasCandidates  []int            `json:"aliasCandidates"`
	Sources          []Source         `json:"sources"`
	Issues           []Issue          `json:"quarantined"`
	IssuesOmitted    int              `json:"quarantineDetailsOmitted"`
	Limited          bool             `json:"limited"`
	Limits           Limits           `json:"limits"`
	Nodes            []*Node          `json:"nodes"`
	Edges            []*Edge          `json:"edges"`
	Evidence         []*Evidence      `json:"evidence"`
	ProcessEvidence  []*Evidence      `json:"processEvidence"`
	Counts           Counts           `json:"counts"`
	ByResource       []ResourceCounts `json:"byResource"`
	Notes            []string         `json:"notes"`
}

func add(xs *[]string, s string) {
	for _, x := range *xs {
		if x == s {
			return
		}
	}
	*xs = append(*xs, s)
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func nodeKey(r object) string {
	return resource(r).key() + "|" + str(r["spanKind"]) + "|" + str(r["traceId"]) + "|" + str(r["spanId"]) + "|" + str(r["parentSpanId"]) + "|" + str(r["serverSpanId"]) + "|" + str(r["requestId"])
}
func terminal(n *Node) object {
	if n.TerminalCount != 1 || n.TerminalStubCount != 0 {
		return nil
	}
	for _, e := range n.records {
		if e.Record["event"] == "diag."+n.Kind {
			return e.Record
		}
	}
	return nil
}

func validateOptions(o Options) error {
	q := o.Query
	if q.Trace != "" && (q.CallerRequestID != "" || !regexpTrace(q.Trace)) {
		return errors.New("invalid_query")
	}
	if q.CallerRequestID != "" {
		if len(q.CallerRequestID) > 128 || !idPattern.MatchString(q.CallerRequestID) || !aliasPattern.MatchString(q.Environment) || !aliasPattern.MatchString(q.DeploymentID) || !validService(q.Service) {
			return errors.New("invalid_query_scope")
		}
		if q.CallerAliasScope != "unknown" && q.CallerAliasScope != "boot" && q.CallerAliasScope != "deployment" {
			return errors.New("invalid_query_scope")
		}
		if q.CallerAliasScope != "unknown" && !aliasPattern.MatchString(q.CallerAlias) {
			return errors.New("invalid_query_scope")
		}
		if q.CallerAliasScope == "boot" && (!instancePattern.MatchString(q.InstanceID) || !bootPattern.MatchString(q.BootID)) {
			return errors.New("invalid_query_scope")
		}
	} else if q.Environment != "" || q.DeploymentID != "" || q.Service != "" || q.CallerAlias != "" || q.CallerAliasScope != "" || q.InstanceID != "" || q.BootID != "" {
		return errors.New("scope_requires_caller")
	}
	for _, scope := range o.UniqueAttemptScopes {
		parts := strings.Split(scope, "/")
		if len(parts) != 2 || !validService(parts[0]) || !aliasPattern.MatchString(parts[1]) {
			return errors.New("invalid_attempt_scope")
		}
	}
	return nil
}

func queryMatch(r object, q Query) (bool, bool) {
	if r["spanKind"] != "server" || r["callerRequestId"] != q.CallerRequestID || r["service"] != q.Service {
		return false, false
	}
	widened := q.CallerAliasScope == "unknown" || q.DeploymentID == "unassigned" || r["callerAliasScope"] == "unknown" || r["deploymentId"] == "unassigned" || r["callerAlias"] == nil
	if q.Environment != "unassigned" && r["environment"] != "unassigned" && q.Environment != r["environment"] {
		return false, false
	}
	if q.DeploymentID != "unassigned" && r["deploymentId"] != "unassigned" && q.DeploymentID != r["deploymentId"] {
		return false, false
	}
	if !widened {
		s := object{"environment": q.Environment, "deploymentId": q.DeploymentID, "service": q.Service, "callerAliasScope": q.CallerAliasScope, "callerAlias": q.CallerAlias, "instanceId": q.InstanceID, "bootId": q.BootID, "callerRequestId": q.CallerRequestID}
		return compareScope(r, s).SameLookupScope, false
	}
	return true, true
}

// Analyze validates the entire bounded import before querying. A trace filter
// cannot hide identity collisions carried by records on another trace.
func Analyze(inputs []Input, opts Options) (*Report, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	d, err := readInputs(inputs, opts.Limits)
	if err != nil {
		return nil, err
	}
	r := &Report{ReportVersion: "diag-analyze/1", DiagnosticSchema: SchemaVersion, ContractSHA256: ContractDigest, Query: opts.Query, Limits: d.limits, Sources: d.sources, Issues: d.issues, IssuesOmitted: d.issuesOmitted, Limited: d.limited, Nodes: []*Node{}, Edges: []*Edge{}, Evidence: []*Evidence{}, ProcessEvidence: []*Evidence{}, CandidateTraces: []string{}, AliasCandidates: []int{}, ByResource: []ResourceCounts{}, Notes: []string{
		"Trace IDs and caller aliases select candidates; they do not prove a relationship.",
		"Verified means unique bilateral evidence from operator-controlled sources, not cryptographic proof.",
		"Coverage is relative to observed exports and instrumented paths; process capabilities do not prove route coverage or a complete export tail.",
		"Usage snapshots are last-observed, not proven final. Upstream and delivered observations remain separate; no token totals are recomputed, and 87 is not an error class.",
		"Missing DEBUG events do not prove that a business stage did not execute. Wall clocks never establish edges or ordering across spans.",
		"Counts describe observed identities, not inferred retries or resolved business totals. Conflicts remain explicit; request/call/attempt failures are separate observed counts.",
	}}
	all := []*Node{}
	byKey := map[string]*Node{}
	bySpan := map[string][]*Node{}
	processConflicts := map[string]bool{}
	bootKey := func(rec object) string {
		return str(rec["service"]) + "|" + str(rec["instanceId"]) + "|" + str(rec["bootId"])
	}
	for _, e := range d.events {
		rec := e.Record
		if rec["spanKind"] == "process" {
			if e.Conflict {
				processConflicts[bootKey(rec)] = true
			}
			continue
		}
		key := nodeKey(rec)
		n := byKey[key]
		if n == nil {
			n = &Node{ID: len(all) + 1, Resource: resource(rec), Kind: str(rec["spanKind"]), TraceID: str(rec["traceId"]), SpanID: str(rec["spanId"]), ParentSpanID: str(rec["parentSpanId"]), ServerSpanID: str(rec["serverSpanId"]), RequestID: str(rec["requestId"]), Events: []int{}, Trusted: true, Findings: []string{}, UsageQualification: "last_observed_not_proven_final"}
			byKey[key] = n
			all = append(all, n)
			bySpan[n.SpanID] = append(bySpan[n.SpanID], n)
		}
		n.records = append(n.records, e)
		n.Events = append(n.Events, e.ID)
		n.Trusted = n.Trusted && e.Trusted
		if e.Conflict {
			add(&n.Findings, "event_conflict")
		}
	}
	for _, group := range bySpan {
		if len(group) > 1 {
			for _, n := range group {
				add(&n.Findings, "span_conflict")
			}
		}
	}
	for _, n := range all {
		if processConflicts[bootKey(n.records[0].Record)] {
			add(&n.Findings, "event_conflict")
			add(&n.Findings, "process_identity_conflict")
		}
		if !n.Trusted {
			add(&n.Findings, "untrusted_source")
		}
		e := diagnostics.CoverageEvidence{DebugCapture: "unknown", AccessCapture: "unknown"}
		e.Conflict = contains(n.Findings, "event_conflict") || contains(n.Findings, "span_conflict")
		for _, event := range n.records {
			rec := event.Record
			e.Sequences = append(e.Sequences, *num(rec["logSeq"]))
			data := obj(rec["data"])
			if rec["event"] == "diag."+n.Kind {
				e.TerminalCount++
				e.TerminalLogSeq = num(rec["logSeq"])
				c := obj(data["coverage"])
				e.ExpectedLastLogSeq = num(c["expectedLastLogSeq"])
				e.DebugCapture = str(c["debugCapture"])
				e.AccessCapture = str(c["accessCapture"])
				e.DroppedForSpan = num(c["droppedForSpan"])
				e.TruncatedEvents = num(c["truncatedEvents"])
			}
			if rec["event"] == "diag.truncated" {
				add(&n.Findings, "truncated_event")
				e.ExportKnownLoss = true
				if data["originalEvent"] == "diag."+n.Kind {
					e.TerminalStubCount++
				}
			}
			for _, p := range event.Provenance {
				s := d.sources[p.Source-1]
				if s.KnownLoss || !s.CompleteScan || s.Quarantined > 0 {
					e.ExportKnownLoss = true
					add(&n.Findings, "source_evidence_gap")
				}
			}
		}
		n.TerminalCount = e.TerminalCount
		n.TerminalStubCount = e.TerminalStubCount
		seqs := append([]uint64{}, e.Sequences...)
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		var previous uint64
		for _, seq := range seqs {
			if seq > previous+1 {
				e.ExportKnownLoss = true
				add(&n.Findings, "sequence_gap")
			}
			if e.TerminalLogSeq != nil && seq > *e.TerminalLogSeq {
				e.Conflict = true
				add(&n.Findings, "span_conflict")
				add(&n.Findings, "post_terminal_record")
			}
			previous = seq
		}
		n.Coverage = diagnostics.AssessCoverage(e)
		if n.Coverage.TerminalMissing {
			add(&n.Findings, "terminal_missing")
		}
		if e.TerminalCount > 1 {
			add(&n.Findings, "contradictory_terminals")
		}
		sort.Slice(n.records, func(i, j int) bool {
			a, b := *num(n.records[i].Record["logSeq"]), *num(n.records[j].Record["logSeq"])
			if a != b {
				return a < b
			}
			return n.records[i].ID < n.records[j].ID
		})
		n.Events = n.Events[:0]
		for _, event := range n.records {
			n.Events = append(n.Events, event.ID)
		}
	}
	edges := buildEdges(all)
	markCycles(all, edges)
	// Local owner checks can discover conflicts after initial sequence coverage.
	// Apply the final graph evidence before publishing any coverage assessment.
	for _, n := range all {
		if contains(n.Findings, "span_conflict") || contains(n.Findings, "event_conflict") {
			n.Coverage.DebugCoverage = "partial"
		}
	}
	// Unread or explicitly lost records anywhere in the import may contain a
	// competing candidate, even when no observed endpoint came from that source.
	sourceScanIncomplete, exportKnownLoss := false, false
	for _, source := range d.sources {
		sourceScanIncomplete = sourceScanIncomplete || !source.CompleteScan
		exportKnownLoss = exportKnownLoss || source.KnownLoss
	}
	for _, edge := range edges {
		if sourceScanIncomplete {
			add(&edge.Findings, "source_scan_incomplete")
		}
		if exportKnownLoss {
			add(&edge.Findings, "export_known_loss")
		}
		if d.limited {
			add(&edge.Findings, "analysis_limited")
		}
		for _, id := range append(append([]int{}, edge.Senders...), edge.Receivers...) {
			n := all[id-1]
			for _, f := range n.Findings {
				if f == "event_conflict" || f == "span_conflict" || f == "cycle" || f == "untrusted_source" || f == "contradictory_terminals" || f == "source_evidence_gap" || f == "ambiguous_parent" || f == "call_count_conflict" {
					add(&edge.Findings, f)
				}
			}
		}
		if edge.Kind == "remote" && len(edge.Senders) == 1 && len(edge.Receivers) == 1 && len(edge.Findings) == 0 {
			edge.Verified = true
			add(&edge.Findings, "verified")
		}
		sort.Strings(edge.Findings)
	}
	traces := map[string]bool{}
	matched := map[int]bool{}
	for _, n := range all {
		if opts.Query.Trace != "" {
			if n.TraceID == opts.Query.Trace {
				traces[n.TraceID] = true
			}
		} else if opts.Query.CallerRequestID != "" {
			for _, event := range n.records {
				match, wide := queryMatch(event.Record, opts.Query)
				if match {
					matched[n.ID] = true
					traces[n.TraceID] = true
					if wide {
						add(&n.Findings, "caller_scope_unknown")
					}
				}
			}
		} else {
			traces[n.TraceID] = true
		}
	}
	for trace := range traces {
		r.CandidateTraces = append(r.CandidateTraces, trace)
	}
	sort.Strings(r.CandidateTraces)
	selected := map[int]bool{}
	selectedEvents := map[int]bool{}
	boots := map[string]bool{}
	for _, n := range all {
		if traces[n.TraceID] {
			r.Nodes = append(r.Nodes, n)
			selected[n.ID] = true
			boots[bootKey(n.records[0].Record)] = true
			for _, event := range n.records {
				selectedEvents[event.ID] = true
			}
			sort.Strings(n.Findings)
			if matched[n.ID] {
				r.AliasCandidates = append(r.AliasCandidates, n.ID)
			}
		}
	}
	for _, edge := range edges {
		visible := false
		for _, id := range append(append([]int{}, edge.Senders...), edge.Receivers...) {
			visible = visible || selected[id]
		}
		if visible {
			r.Edges = append(r.Edges, edge)
		}
	}
	// A mismatching local owner or a reused span can live outside the selected
	// trace. Keep its node and evidence as explicit context, not query totals.
	contextIDs := map[int]bool{}
	for _, edge := range r.Edges {
		for _, id := range append(append([]int{}, edge.Senders...), edge.Receivers...) {
			if !selected[id] {
				contextIDs[id] = true
			}
		}
	}
	checkedSpans := map[string]bool{}
	for _, n := range r.Nodes {
		if contains(n.Findings, "span_conflict") {
			if checkedSpans[n.SpanID] {
				continue
			}
			checkedSpans[n.SpanID] = true
			for _, other := range bySpan[n.SpanID] {
				if !selected[other.ID] {
					contextIDs[other.ID] = true
				}
			}
		}
	}
	queryNodes := append([]*Node{}, r.Nodes...)
	for _, n := range all {
		if contextIDs[n.ID] {
			n.ContextOnly = true
			add(&n.Findings, "outside_query_conflict_context")
			sort.Strings(n.Findings)
			r.Nodes = append(r.Nodes, n)
			for _, event := range n.records {
				selectedEvents[event.ID] = true
			}
			boots[bootKey(n.records[0].Record)] = true
		}
	}
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].ID < r.Nodes[j].ID })
	// Retain conflicting payloads on other traces too, with their source locations.
	conflictedIdentities := map[string]bool{}
	for _, event := range d.events {
		if selectedEvents[event.ID] && event.Conflict {
			conflictedIdentities[event.Identity] = true
		}
	}
	for _, event := range d.events {
		if event.Record["spanKind"] == "process" {
			if boots[bootKey(event.Record)] || opts.Query.Trace == "" && opts.Query.CallerRequestID == "" {
				r.ProcessEvidence = append(r.ProcessEvidence, event)
			}
		} else if selectedEvents[event.ID] || conflictedIdentities[event.Identity] {
			r.Evidence = append(r.Evidence, event)
		}
	}
	r.Counts = countNodes(queryNodes, opts.UniqueAttemptScopes)
	groups := map[string][]*Node{}
	for _, n := range queryNodes {
		groups[n.Resource.key()] = append(groups[n.Resource.key()], n)
	}
	keys := []string{}
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r.ByResource = append(r.ByResource, ResourceCounts{groups[key][0].Resource, countNodes(groups[key], opts.UniqueAttemptScopes)})
	}
	return r, nil
}

func buildEdges(nodes []*Node) []*Edge {
	edges := []*Edge{}
	owners := map[string][]int{}
	calls := map[string][]int{}
	receivers := map[string][]int{}
	localCalls := map[string]map[string]bool{}
	for _, n := range nodes {
		if n.Kind == "server" {
			owners[n.Resource.key()+"|"+n.SpanID] = append(owners[n.Resource.key()+"|"+n.SpanID], n.ID)
			if n.ParentSpanID == "" {
				add(&n.Findings, "root")
			} else {
				key := n.TraceID + "|" + n.ParentSpanID
				receivers[key] = append(receivers[key], n.ID)
			}
		} else {
			ownerKey := n.Resource.key() + "|" + n.ServerSpanID
			if localCalls[ownerKey] == nil {
				localCalls[ownerKey] = map[string]bool{}
			}
			localCalls[ownerKey][n.SpanID] = true
			key := n.TraceID + "|" + n.SpanID
			calls[key] = append(calls[key], n.ID)
		}
	}
	for _, n := range nodes {
		if n.Kind == "server" {
			count := len(localCalls[n.Resource.key()+"|"+n.SpanID])
			n.ObservedLocalCalls = &count
			if rec := terminal(n); rec != nil {
				declared := num(obj(rec["data"])["callCount"])
				if declared != nil && *declared != uint64(count) {
					if *declared > uint64(count) {
						add(&n.Findings, "missing_call_terminals")
					} else {
						add(&n.Findings, "call_count_conflict")
					}
					n.Coverage.DebugCoverage = "partial"
				}
			}
		}
	}
	ambiguousOwners := map[string]*Edge{}
	for _, n := range nodes {
		if n.Kind != "call" {
			continue
		}
		ownerKey := n.Resource.key() + "|" + n.ServerSpanID
		ids := owners[ownerKey]
		// Represent an ambiguous owner group once. Repeating its candidates for
		// every call would turn a bounded import into quadratic memory/output.
		if len(ids) > 1 {
			e := ambiguousOwners[ownerKey]
			if e == nil {
				e = &Edge{Kind: "local", Senders: ids, Receivers: []int{}, Findings: []string{"ambiguous_parent"}}
				ambiguousOwners[ownerKey] = e
				edges = append(edges, e)
			}
			e.Receivers = append(e.Receivers, n.ID)
			add(&n.Findings, "ambiguous_parent")
			continue
		}
		e := &Edge{Kind: "local", Senders: append([]int{}, ids...), Receivers: []int{n.ID}, Findings: []string{}}
		if len(ids) == 0 {
			add(&e.Findings, "missing_local_parent")
			add(&n.Findings, "missing_local_parent")
		} else if len(ids) > 1 {
			add(&e.Findings, "ambiguous_parent")
		} else {
			p := nodes[ids[0]-1]
			if p.TraceID != n.TraceID || p.RequestID != n.RequestID {
				add(&e.Findings, "span_conflict")
				add(&n.Findings, "span_conflict")
				add(&p.Findings, "span_conflict")
			}
		}
		edges = append(edges, e)
	}
	keys := map[string]bool{}
	for key := range calls {
		keys[key] = true
	}
	for key := range receivers {
		keys[key] = true
	}
	ordered := []string{}
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		send, recv := calls[key], receivers[key]
		e := &Edge{Kind: "remote", Senders: append([]int{}, send...), Receivers: append([]int{}, recv...), Findings: []string{}}
		if len(send) == 0 {
			add(&e.Findings, "external_parent_unverified")
		}
		if len(send) > 1 || len(recv) > 1 {
			add(&e.Findings, "ambiguous_parent")
		}
		for _, id := range send {
			n := nodes[id-1]
			rec := terminal(n)
			if rec == nil {
				add(&e.Findings, "terminal_evidence_incomplete")
				continue
			}
			data := obj(rec["data"])
			if data["peerTraceId"] != nil && data["peerTraceId"] != rec["traceId"] {
				add(&e.Findings, "context_mismatch")
			}
			if data["peerConfigured"] != true {
				add(&e.Findings, "not_participating")
				if len(recv) > 0 {
					add(&e.Findings, "peer_conflict")
				}
				continue
			}
			if len(recv) == 0 {
				add(&e.Findings, "missing_peer")
			}
			if len(send) == 1 && len(recv) == 1 {
				other := terminal(nodes[recv[0]-1])
				if other == nil {
					add(&e.Findings, "terminal_evidence_incomplete")
					continue
				}
				caller := obj(other["callerIdSource"])
				if data["peerService"] != other["service"] || data["peerDeploymentId"] != nil && data["peerDeploymentId"] != other["deploymentId"] || data["peerRequestId"] != nil && data["peerRequestId"] != other["requestId"] || caller["header"] == "x-diag-request-id" && caller["trust"] == "configured_peer" && other["callerRequestId"] != rec["requestId"] {
					add(&e.Findings, "peer_conflict")
				}
			}
		}
		edges = append(edges, e)
	}
	return edges
}

// Tarjan SCC over unique declared relationships, before verification. Ambiguous
// groups are already disqualified and are not expanded into a quadratic graph.
func markCycles(nodes []*Node, edges []*Edge) {
	adj := make([][]int, len(nodes))
	for _, e := range edges {
		if len(e.Senders) == 1 && len(e.Receivers) == 1 {
			a, b := e.Senders[0]-1, e.Receivers[0]-1
			adj[a] = append(adj[a], b)
		}
	}
	index, low := make([]int, len(nodes)), make([]int, len(nodes))
	active := make([]bool, len(nodes))
	stack := []int{}
	next := 0
	var visit func(int)
	visit = func(v int) {
		next++
		index[v] = next
		low[v] = next
		stack = append(stack, v)
		active[v] = true
		for _, w := range adj[v] {
			if index[w] == 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if active[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			group := []int{}
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				active[w] = false
				group = append(group, w)
				if w == v {
					break
				}
			}
			cycle := len(group) > 1
			for _, w := range adj[v] {
				cycle = cycle || w == v
			}
			if cycle {
				for _, w := range group {
					add(&nodes[w].Findings, "cycle")
					add(&nodes[w].Findings, "span_conflict")
					nodes[w].Coverage.DebugCoverage = "partial"
				}
			}
		}
	}
	for v := range nodes {
		if index[v] == 0 {
			visit(v)
		}
	}
}

func attemptKey(r object, unique bool) string {
	if r["retryScope"] == nil {
		return ""
	}
	suffix := ""
	if r["attemptId"] != nil {
		suffix = "id:" + str(r["attemptId"])
	} else if unique && r["attemptNo"] != nil {
		suffix = "no:" + canonical(r["attemptNo"])
	}
	if suffix == "" {
		return ""
	}
	return resource(r).key() + "|" + str(r["serverSpanId"]) + "|" + str(r["retryScope"]) + "|" + suffix
}

func countNodes(nodes []*Node, scopes []string) Counts {
	requests, owners, calls, attempts, unknown := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	reqFail, callFail, attemptFail := map[string]bool{}, map[string]bool{}, map[string]bool{}
	result := Counts{}
	for _, n := range nodes {
		key := n.Resource.key() + "|" + n.SpanID
		owners[n.Resource.key()+"|"+n.ServerSpanID] = true
		if n.Kind == "server" {
			requests[key] = true
		} else {
			calls[key] = true
		}
		conflict := contains(n.Findings, "span_conflict") || contains(n.Findings, "event_conflict") || contains(n.Findings, "contradictory_terminals")
		result.Conflicted = result.Conflicted || conflict
		for _, e := range n.records {
			r := e.Record
			data := obj(r["data"])
			a := attemptKey(r, contains(scopes, str(r["service"])+"/"+str(r["retryScope"])))
			if a != "" {
				attempts[a] = true
			} else if n.Kind == "call" {
				unknown[key] = true
			}
			if conflict {
				continue
			}
			if r["event"] == "diag.server" && (data["deliveryState"] == "failed" || data["endReason"] == "error" || num(data["wireStatus"]) != nil && *num(data["wireStatus"]) >= 400) {
				reqFail[key] = true
			}
			if r["event"] == "diag.call" && (data["endReason"] == "transport_error" || data["endReason"] == "read_error" || num(data["upstreamStatus"]) != nil && *num(data["upstreamStatus"]) >= 400) {
				callFail[key] = true
			}
			if r["event"] == "upstream.attempt_finished" && a != "" && (data["resultClass"] == "error" || data["resultClass"] == "blocked" || data["resultClass"] == "empty" || data["resultClass"] == "incomplete") {
				attemptFail[a] = true
			}
		}
	}
	result.Requests = len(requests)
	result.ObservedServerOwners = len(owners)
	result.Calls = len(calls)
	result.Attempts = len(attempts)
	result.UnknownAttemptCalls = len(unknown)
	result.RequestFailures = len(reqFail)
	result.CallFailures = len(callFail)
	result.AttemptFailures = len(attemptFail)
	return result
}

func (r *Report) WriteJSON(w io.Writer) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(r)
}

// WriteText prints a forest using verified remote edges and conflict-free local
// ownership. Other relationships remain explicit findings below the forest.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Observed requests=%d calls=%d attempts=%d unknown-attempt-calls=%d conflicted=%t limited=%t\n", r.Counts.Requests, r.Counts.Calls, r.Counts.Attempts, r.Counts.UnknownAttemptCalls, r.Counts.Conflicted, r.Limited)
	nodes := map[int]*Node{}
	for _, n := range r.Nodes {
		nodes[n.ID] = n
	}
	children := map[int][]int{}
	parent := map[int]bool{}
	for _, e := range r.Edges {
		if (e.Verified || e.Kind == "local" && len(e.Findings) == 0) && len(e.Senders) == 1 && len(e.Receivers) == 1 && nodes[e.Senders[0]] != nil && nodes[e.Receivers[0]] != nil {
			children[e.Senders[0]] = append(children[e.Senders[0]], e.Receivers[0])
			parent[e.Receivers[0]] = true
		}
	}
	for _, e := range r.Evidence {
		fmt.Fprintf(&b, "evidence %d event=%s seq=%v sources=%v conflict=%t\n", e.ID, str(e.Record["event"]), e.Record["logSeq"], e.Provenance, e.Conflict)
		data := obj(e.Record["data"])
		for _, key := range []string{"resultClass", "usage", "upstreamUsage", "deliveredUsage", "output", "tokenCount", "tokenSource"} {
			if value, ok := data[key]; ok {
				encoded, _ := json.Marshal(value)
				fmt.Fprintf(&b, "  %s=%s\n", key, encoded)
			}
		}
	}
	visited := map[int]bool{}
	var write func(int, int)
	write = func(id, depth int) {
		if visited[id] {
			return
		}
		visited[id] = true
		n := nodes[id]
		fmt.Fprintf(&b, "%s[%d] %s %s/%s/%s/%s/%s request=%s span=%s trace=%s DEBUG=%s terminalMissing=%t %s\n", strings.Repeat("  ", min(depth, 32)), id, n.Kind, n.Resource.Environment, n.Resource.DeploymentID, n.Resource.Service, n.Resource.InstanceID, n.Resource.BootID, n.RequestID, n.SpanID, n.TraceID, n.Coverage.DebugCoverage, n.Coverage.TerminalMissing, strings.Join(n.Findings, ","))
		if depth >= 32 {
			fmt.Fprintln(&b, "  tree_depth_limit: remaining nodes listed separately")
			return
		}
		for _, child := range children[id] {
			write(child, depth+1)
		}
	}
	for _, n := range r.Nodes {
		if !parent[n.ID] {
			write(n.ID, 0)
		}
	}
	for _, n := range r.Nodes {
		write(n.ID, 0)
	}
	for _, e := range r.Edges {
		fmt.Fprintf(&b, "edge %s %v -> %v: %s\n", e.Kind, e.Senders, e.Receivers, strings.Join(e.Findings, ","))
	}
	for _, s := range r.Sources {
		fmt.Fprintf(&b, "source %d (%s) trusted=%t bytes=%d lines=%d accepted=%d quarantined=%d completeScan=%t %s\n", s.ID, s.Alias, s.Trusted, s.ScannedBytes, s.Lines, s.Accepted, s.Quarantined, s.CompleteScan, strings.Join(s.Findings, ","))
	}
	for _, i := range r.Issues {
		fmt.Fprintf(&b, "quarantine source=%d line=%d bytes=%d lengthComplete=%t reason=%s\n", i.Source, i.Line, i.Bytes, i.LengthComplete, i.Reason)
	}
	if r.IssuesOmitted > 0 {
		fmt.Fprintf(&b, "quarantine_details_omitted=%d\n", r.IssuesOmitted)
	}
	for _, note := range r.Notes {
		fmt.Fprintln(&b, note)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
