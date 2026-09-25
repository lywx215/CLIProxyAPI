package diagnostics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const Schema = "ai-proxy-diagnostics/1"
const ArtifactVersion = "1.0.0-rc.1"

type Resource struct {
	Environment            string  `json:"environment"`
	DeploymentID           string  `json:"deploymentId"`
	Service                string  `json:"service"`
	NodeLabel              *string `json:"nodeLabel"`
	InstanceID             string  `json:"instanceId"`
	InstanceIdentitySource string  `json:"instanceIdentitySource"`
	BootID                 string  `json:"bootId"`
	BuildCommit            *string `json:"buildCommit"`
}

// ResourceConfig contains local operator configuration, never request claims.
type ResourceConfig struct{ Environment, DeploymentID, NodeLabel, InstanceID, BuildCommit string }

// NewResource is called once per worker. No platform environment variable is
// assumed to be a replica UID; an embedding host may supply a confirmed UID.
func NewResource(c ResourceConfig, confirmedReplicaUID string) (Resource, bool) {
	r := Resource{Environment: "unassigned", DeploymentID: "unassigned", Service: "cliproxyapi", InstanceID: uuid.NewString(), InstanceIdentitySource: "ephemeral", BootID: uuid.NewString()}
	invalid := false
	for _, item := range []struct {
		value  string
		target *string
	}{{c.Environment, &r.Environment}, {c.DeploymentID, &r.DeploymentID}} {
		if validToken(item.value, 64) {
			*item.target = item.value
		} else if item.value != "" {
			invalid = true
		}
	}
	if validToken(c.NodeLabel, 64) {
		r.NodeLabel = &c.NodeLabel
	} else if c.NodeLabel != "" {
		invalid = true
	}
	if validToken(confirmedReplicaUID, 128) {
		r.InstanceID, r.InstanceIdentitySource = confirmedReplicaUID, "platform"
	} else if validToken(c.InstanceID, 128) {
		r.InstanceID, r.InstanceIdentitySource = c.InstanceID, "configured"
	} else if c.InstanceID != "" {
		invalid = true
	}
	if len(c.BuildCommit) >= 7 && len(c.BuildCommit) <= 64 && hexPattern.MatchString(c.BuildCommit) {
		r.BuildCommit = &c.BuildCommit
	}
	return r, invalid
}

func EnvironmentConfig(buildCommit string) ResourceConfig {
	return ResourceConfig{os.Getenv("DIAG_ENVIRONMENT"), os.Getenv("DIAG_DEPLOYMENT_ID"), os.Getenv("DIAG_NODE_LABEL"), os.Getenv("DIAG_INSTANCE_ID"), buildCommit}
}

func randomHex(bytes int) string {
	b := make([]byte, bytes)
	// crypto/rand.Read is guaranteed to fill the buffer in the supported Go runtime.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type CallerSource struct {
	Header   *string `json:"header"`
	Trust    string  `json:"trust"`
	Rejected string  `json:"rejected"`
}

type Record struct {
	DiagnosticSchema string `json:"diagnosticSchema"`
	TS               string `json:"ts"`
	Level            string `json:"level"`
	RecordKind       string `json:"recordKind"`
	Event            string `json:"event"`
	Resource
	SpanKind         string       `json:"spanKind"`
	TraceID          *string      `json:"traceId"`
	SpanID           *string      `json:"spanId"`
	ParentSpanID     *string      `json:"parentSpanId"`
	ServerSpanID     *string      `json:"serverSpanId"`
	RequestID        *string      `json:"requestId"`
	ContextSource    *string      `json:"contextSource"`
	CallerRequestID  *string      `json:"callerRequestId"`
	CallerAlias      *string      `json:"callerAlias"`
	CallerAliasScope string       `json:"callerAliasScope"`
	CallerIDSource   CallerSource `json:"callerIdSource"`
	AttemptID        *string      `json:"attemptId"`
	AttemptNo        *uint64      `json:"attemptNo"`
	RetryScope       *string      `json:"retryScope"`
	CallNo           *uint64      `json:"callNo"`
	LogSeq           uint64       `json:"logSeq"`
	Data             any          `json:"data"`
}

type Coverage struct {
	ExpectedLastLogSeq uint64  `json:"expectedLastLogSeq"`
	DroppedForSpan     *uint64 `json:"droppedForSpan"`
	SinkDroppedTotal   *uint64 `json:"sinkDroppedTotal"`
	TruncatedEvents    uint64  `json:"truncatedEvents"`
	DebugCapture       string  `json:"debugCapture"`
	AccessCapture      string  `json:"accessCapture"`
}

type ServerData struct {
	RouteTemplate    *string  `json:"routeTemplate"`
	HeadersCommitted bool     `json:"headersCommitted"`
	WireStatus       *int     `json:"wireStatus"`
	EndReason        string   `json:"endReason"`
	DeliveryState    string   `json:"deliveryState"`
	TotalMS          float64  `json:"totalMs"`
	CallCount        uint64   `json:"callCount"`
	Coverage         Coverage `json:"coverage"`
}

type CallData struct {
	TargetAlias      *string `json:"targetAlias"`
	PeerConfigured   bool    `json:"peerConfigured"`
	PeerService      *string `json:"peerService"`
	PeerDeploymentID *string `json:"peerDeploymentId"`
	CallKind         string  `json:"callKind"`
	UpstreamStatus   *int    `json:"upstreamStatus"`
	EndReason        string  `json:"endReason"`
	TotalMS          float64 `json:"totalMs"`
	PeerIDs
	ProviderRequestID *string  `json:"providerRequestId"`
	Coverage          Coverage `json:"coverage"`
}

type processData struct {
	PID              int      `json:"pid"`
	Reason           string   `json:"reason"`
	AccessEnabled    bool     `json:"accessEnabled"`
	DebugEnabled     bool     `json:"debugEnabled"`
	ConfigRevision   string   `json:"configRevision"`
	ContractVersion  string   `json:"contractVersion"`
	Capabilities     []string `json:"capabilities"`
	SinkDroppedTotal *uint64  `json:"sinkDroppedTotal"`
	ConfigStatus     string   `json:"configStatus"`
}

// Engine holds process-local identity and an immutable peer snapshot. Sink must
// synchronously write one complete line through the existing shared log writer.
// Access is the existing basic logging gate, independent of Debug. Because an
// external logger can change its gate without notifying us, access coverage is
// conservatively unknown, never a claim of uninterrupted lifetime capture.
type Engine struct {
	resource        Resource
	invalidResource bool
	peers           atomic.Pointer[Peers]
	access          func() bool
	debug           func() bool
	sink            func([]byte) error
	dropped         atomic.Uint64
	sinkLossUnknown atomic.Bool
	processMu       sync.Mutex
	processSeq      uint64
	revision        uint64
	peerRaw         string
}

func NewEngine(c ResourceConfig, peers string, access, debug func() bool, sink func([]byte) error) *Engine {
	r, invalid := NewResource(c, "")
	e := &Engine{resource: r, invalidResource: invalid, access: access, debug: debug, sink: sink, revision: 1, peerRaw: peers}
	e.peers.Store(ParsePeers(peers))
	e.process("startup")
	return e
}

// ReloadPeers atomically publishes even invalid snapshots (which disable all peers).
func (e *Engine) ReloadPeers(raw string) {
	e.processMu.Lock()
	defer e.processMu.Unlock()
	if raw == e.peerRaw {
		return
	}
	e.peerRaw = raw
	e.peers.Store(ParsePeers(raw))
	e.revision++
	e.processLocked("config_changed")
}

// MarkSinkLossUnknown prevents a nil sink error from claiming acknowledged writes.
func (e *Engine) MarkSinkLossUnknown() { e.sinkLossUnknown.Store(true) }
func (e *Engine) knownDrops(n uint64) *uint64 {
	if n == 0 && e.sinkLossUnknown.Load() {
		return nil
	}
	return &n
}

func (e *Engine) enabled() bool { return e != nil && e.sink != nil && (e.access == nil || e.access()) }

func (e *Engine) base(kind, event string) Record {
	return Record{DiagnosticSchema: Schema, TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Level: "INFO", RecordKind: "basic", Event: event, Resource: e.resource, SpanKind: kind, CallerAliasScope: "unknown", CallerIDSource: CallerSource{Trust: "none", Rejected: "none"}}
}

func (e *Engine) process(reason string) {
	e.processMu.Lock()
	defer e.processMu.Unlock()
	e.processLocked(reason)
}
func (e *Engine) processLocked(reason string) {
	if !e.enabled() {
		return
	}
	e.processSeq++
	r := e.base("process", "diag.process")
	r.LogSeq = e.processSeq
	status := e.peers.Load().Status
	if e.invalidResource {
		status = "config_invalid"
	}
	r.Data = processData{os.Getpid(), reason, true, e.debug != nil && e.debug(), hexCounter(e.revision), ArtifactVersion, []string{"http_inbound", "http_outbound", "normalization", "attempt_result", "conversion", "throttle"}, nil, status}
	e.emit(r)
}

func hexCounter(n uint64) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var out [16]byte
	i := len(out)
	for n > 0 {
		i--
		out[i] = digits[n&15]
		n >>= 4
	}
	return string(out[i:])
}

func (e *Engine) emit(r Record) (dropped, truncated uint64) {
	defer func() {
		if recover() != nil {
			e.dropped.Add(1)
			dropped = 1
		}
	}()
	b, err := json.Marshal(r)
	if err != nil || len(b)+7 > 4096 {
		reason := "line_limit"
		if err != nil {
			reason = "serialization_failure"
		}
		r.Data = struct {
			OriginalEvent      string `json:"originalEvent"`
			OriginalRecordKind string `json:"originalRecordKind"`
			Reason             string `json:"reason"`
		}{r.Event, r.RecordKind, reason}
		r.Event = "diag.truncated"
		truncated = 1
		b, err = json.Marshal(r)
	}
	if err != nil || len(b)+7 > 4096 {
		e.dropped.Add(1)
		return 1, truncated
	}
	line := append([]byte("@diag "), b...)
	line = append(line, '\n')
	if e.sink(line) != nil {
		e.dropped.Add(1)
		dropped = 1
	}
	return
}

type spanKey struct{}
type callKindKey struct{}

// WithCallKind only labels an already-owned synchronous call; no attempt or span
// is created here. Callers without reliable knowledge leave the default 'other'.
func WithCallKind(ctx context.Context, kind string) context.Context {
	switch kind {
	case "model", "auth", "metadata", "other":
		return context.WithValue(ctx, callKindKey{}, kind)
	}
	return ctx
}

type ServerSpan struct {
	engine                       *Engine
	incoming                     Incoming
	id, requestID                string
	started                      time.Time
	mu                           sync.Mutex
	sealed                       bool
	calls                        uint64
	attempts                     uint64
	emitMu                       sync.Mutex
	seq, dropped, truncated      uint64
	debugOpted, debugInterrupted bool
	debugEpoch                   uint64
}

func (e *Engine) StartServer(ctx context.Context, headers map[string][]string, requestID string) (context.Context, *ServerSpan) {
	if !customIDPattern.MatchString(requestID) {
		return ctx, nil
	}
	s := &ServerSpan{engine: e, incoming: Extract(headers, false, false), id: randomHex(8), requestID: requestID, started: time.Now()}
	s.debugEpoch = debugDisabledEpoch.Load()
	s.debugOpted = e.debug != nil && e.debug()
	return context.WithValue(ctx, spanKey{}, s), s
}

func ServerFromContext(ctx context.Context) *ServerSpan {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(spanKey{}).(*ServerSpan)
	return s
}

// CarryContext preserves diagnostics when an existing SDK boundary deliberately
// chooses a different cancellation parent. It does not change that parent.
func CarryContext(dst, src context.Context) context.Context {
	if s := ServerFromContext(src); s != nil {
		return context.WithValue(dst, spanKey{}, s)
	}
	return dst
}

func (s *ServerSpan) record(kind, event, id string, parent *string) Record {
	r := s.engine.base(kind, event)
	r.TraceID, r.SpanID, r.ParentSpanID, r.ServerSpanID, r.RequestID = &s.incoming.TraceID, &id, parent, &s.id, &s.requestID
	r.ContextSource, r.CallerRequestID = &s.incoming.ContextSource, s.incoming.CallerRequestID
	r.CallerIDSource = CallerSource{s.incoming.CallerHeader, s.incoming.CallerTrust, s.incoming.Rejected}
	return r
}

func (s *ServerSpan) coverage() Coverage {
	// The shared logrus writer does not report downstream collector loss. A
	// successful call to its sink is not evidence of zero sink-wide drops.
	capture := "none"
	if s.debugOpted {
		capture = "enabled_throughout"
		if s.debugInterrupted {
			capture = "interrupted"
		}
	}
	return Coverage{ExpectedLastLogSeq: s.seq, DroppedForSpan: s.engine.knownDrops(s.dropped), TruncatedEvents: s.truncated, DebugCapture: capture, AccessCapture: "unknown"}
}

func (s *ServerSpan) Finish(data ServerData) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.sealed {
		s.mu.Unlock()
		return
	}
	s.debugActiveLocked()
	s.sealed = true
	s.mu.Unlock()
	// Seal immediately, then await only already-constructed semantic emissions.
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	s.mu.Lock()
	if !s.engine.enabled() {
		s.mu.Unlock()
		return
	}
	s.seq++
	data.CallCount, data.TotalMS, data.Coverage = s.calls, elapsed(s.started), s.coverage()
	if !data.HeadersCommitted {
		data.WireStatus = nil
	}
	r := s.record("server", "diag.server", s.id, s.incoming.ParentSpanID)
	r.LogSeq = s.seq
	r.Data = data
	s.mu.Unlock()
	s.engine.emit(r)
}

func elapsed(t time.Time) float64 { return float64(time.Since(t).Nanoseconds()) / 1e6 }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SourceScope remains an alias lookup aid. It never merges events or establishes
// a parent; absent local authentication leaves callerAlias null/unknown.
type SourceScope struct {
	Environment      string  `json:"environment"`
	DeploymentID     string  `json:"deploymentId"`
	Service          string  `json:"service"`
	CallerAliasScope string  `json:"callerAliasScope"`
	CallerAlias      *string `json:"callerAlias"`
	InstanceID       string  `json:"instanceId"`
	BootID           string  `json:"bootId"`
	CallerRequestID  *string `json:"callerRequestId"`
}
type ScopeComparison struct {
	SameLookupScope     bool `json:"sameLookupScope"`
	CandidateAliasMatch bool `json:"candidateAliasMatch"`
	Merge               bool `json:"merge"`
	ScopeUnknown        bool `json:"scopeUnknown"`
}

func CompareSourceScope(a, b SourceScope) ScopeComparison {
	unknown := a.CallerAlias == nil || b.CallerAlias == nil || a.CallerAliasScope == "unknown" || b.CallerAliasScope == "unknown" || a.DeploymentID == "unassigned" || b.DeploymentID == "unassigned"
	key := func(s SourceScope) string {
		parts := []string{s.Environment, s.DeploymentID, s.Service, s.CallerAliasScope}
		if s.CallerAlias != nil {
			parts = append(parts, *s.CallerAlias)
		}
		if s.CallerAliasScope == "boot" {
			parts = append(parts, s.InstanceID, s.BootID)
		}
		return strings.Join(parts, "\x00")
	}
	return ScopeComparison{!unknown && key(a) == key(b), a.CallerRequestID != nil && b.CallerRequestID != nil && *a.CallerRequestID == *b.CallerRequestID, false, unknown}
}
