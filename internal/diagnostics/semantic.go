package diagnostics

import (
	"context"
	"sync/atomic"
)

// NotifyDebugDisabled is called before the application's logger disables DEBUG.
// An epoch records even an off/on transition between two observations. Requests
// never opt in halfway through their lifetime or resume an interrupted capture.
var debugDisabledEpoch atomic.Uint64

func NotifyDebugDisabled() { debugDisabledEpoch.Add(1) }

func (s *ServerSpan) debugActiveLocked() bool {
	if s.debugOpted && (s.debugEpoch != debugDisabledEpoch.Load() || s.engine.debug == nil || !s.engine.debug()) {
		s.debugInterrupted = true
	}
	return !s.sealed && s.debugOpted && !s.debugInterrupted
}

func DebugActive(ctx context.Context) bool {
	s := ServerFromContext(ctx)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.debugActiveLocked()
}

// semantic emits only internal, closed projections. Callers cannot provide an
// arbitrary event name, nested map, error string, model name or credential.
func semantic(ctx context.Context, event string, data any) {
	s := ServerFromContext(ctx)
	if s == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	s.mu.Lock()
	if !s.debugActiveLocked() {
		s.mu.Unlock()
		return
	}
	s.seq++
	r := s.record("server", event, s.id, s.incoming.ParentSpanID)
	applyAttempt(ctx, &r)
	r.Level, r.RecordKind, r.LogSeq, r.Data = "DEBUG", "debug", s.seq, data
	s.mu.Unlock()
	dropped, truncated := s.engine.emit(r)
	s.mu.Lock()
	s.dropped += dropped
	s.truncated += truncated
	s.mu.Unlock()
}

// Guard isolates observer failures from business execution, including sinks.
func Guard(observe func()) { defer func() { _ = recover() }(); observe() }

type Metric struct {
	Value   *int64 `json:"value"`
	Present bool   `json:"present"`
	Source  string `json:"source"`
}
type Usage struct {
	Protocol          string            `json:"protocol"`
	Input             Metric            `json:"input"`
	Candidate         Metric            `json:"candidate"`
	Reasoning         Metric            `json:"reasoning"`
	OutputTotal       Metric            `json:"outputTotal"`
	Basis             string            `json:"basis"`
	ReasoningIncluded *bool             `json:"reasoningIncludedInOutput"`
	Raw               map[string]*int64 `json:"raw"`
}
type Output struct {
	Candidates    *int64 `json:"candidateCount"`
	Text          *int64 `json:"ordinaryTextUtf8Bytes"`
	Thought       *int64 `json:"thoughtUtf8Bytes"`
	Tools         *int64 `json:"validToolCalls"`
	Media         *int64 `json:"mediaParts"`
	NonWhitespace *int64 `json:"ordinaryTextNonWhitespaceChars"`
}
type Timing struct {
	FirstByte      *float64 `json:"firstUpstreamByteMs"`
	FirstEffective *float64 `json:"firstEffectiveOutputMs"`
	Commit         *float64 `json:"responseCommitMs"`
	Downstream     *float64 `json:"firstDownstreamEffectiveOutputMs"`
	Source         string   `json:"timingSource"`
}
type attemptData struct {
	Result          string   `json:"resultClass"`
	Origin          string   `json:"failureOrigin"`
	Stage           string   `json:"failureStage"`
	Error           string   `json:"errorClass"`
	Usage           Usage    `json:"usage"`
	Output          Output   `json:"output"`
	Terminal        *bool    `json:"terminalSeen"`
	EOF             *bool    `json:"eofSeen"`
	Parsed          *bool    `json:"parserFinishOk"`
	Total           *float64 `json:"totalMs"`
	Credential      *string  `json:"credentialRef"`
	CredentialScope string   `json:"credentialRefScope"`
	Timing          Timing   `json:"timing"`
}
type convertedData struct {
	Input          string `json:"inputProtocol"`
	OutputProtocol string `json:"outputProtocol"`
	ClientStream   bool   `json:"clientStreaming"`
	UpstreamStream bool   `json:"upstreamStreaming"`
	Mode           string `json:"deliveryMode"`
	Upstream       Usage  `json:"upstreamUsage"`
	Delivered      Usage  `json:"deliveredUsage"`
	Output         Output `json:"output"`
	Result         string `json:"resultClass"`
}

type ThrottleData struct {
	Enabled    bool     `json:"enabled"`
	Revision   string   `json:"configRevision"`
	Rate       *float64 `json:"targetTokensPerSecond"`
	FirstDelay *float64 `json:"selectedFirstTokenDelayMs"`
	Tokens     *int64   `json:"tokenCount"`
	Source     string   `json:"tokenSource"`
	Planned    *float64 `json:"plannedWaitMs"`
	Actual     *float64 `json:"actualWaitMs"`
	Elapsed    *float64 `json:"elapsedMs"`
	Cancelled  bool     `json:"cancelled"`
}

func ObserveThrottle(ctx context.Context, d ThrottleData) {
	Guard(func() {
		if !DebugActive(ctx) {
			return
		}
		// Revision is a local snapshot identity, never arbitrary configuration.
		if !validToken(d.Revision, 64) {
			d.Revision = "unknown"
		}
		switch d.Source {
		case "provider_output", "provider_candidate", "estimated":
		default:
			d.Source = "unknown"
		}
		semantic(ctx, "throttle.finished", d)
	})
}
