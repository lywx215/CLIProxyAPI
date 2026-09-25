# DIAG-05 R2 complete source evidence

Base reviewed HEAD: `bb410f92433ada96614caee23d5fb385510e43c5`.
These are complete files from the R2 commit containing this companion, not diff
excerpts. SHA-256 values use UTF-8 source normalized to LF (the Git blob form).
No secrets or runtime configuration are included. Source paths and line numbers
refer to the corresponding repository files. This is evidence, not a review.

## `internal/diagnostics/protocol.go`

SHA-256 (LF): `0e6e4e23864316ffd8ff794749c9bf0646284055e911ed9a01e988c3b48acef2`

```go
package diagnostics

import (
	"bytes"
	"context"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tidwall/gjson"
)

const maxObservationBytes = 4 << 20
const maxSafeCount = int64(9007199254740991)

func ptr[T any](v T) *T { return &v }

// boundedJSON avoids recursion on hostile input before invoking the JSON parser.
// Limits affect observation only; payloads are never rewritten or retained.
func boundedJSON(b []byte) bool {
	valid, _ := inspectJSON(b)
	return valid
}

// A local observation limit is not evidence that upstream JSON is malformed.
func inspectJSON(b []byte) (valid, limited bool) {
	if len(b) > maxObservationBytes {
		return false, true
	}
	depth, quoted, escape := 0, false, false
	for _, c := range b {
		if quoted {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '[', '{':
			depth++
			if depth > 64 {
				return false, true
			}
		case ']', '}':
			depth--
		}
	}
	return gjson.ValidBytes(b), false
}

func protocol(p string) string {
	switch p {
	case "gemini", "antigravity":
		return "gemini"
	case "openai", "openai_chat":
		return "openai_chat"
	case "openai-response", "openai-responses", "openai_responses":
		return "openai_responses"
	case "claude":
		return "claude"
	}
	return "unknown"
}

func unknownUsage(p string) Usage {
	m := Metric{Source: "unknown"}
	return Usage{Protocol: protocol(p), Input: m, Candidate: m, Reasoning: m, OutputTotal: m, Basis: "unknown", Raw: map[string]*int64{}}
}

func number(r gjson.Result) *int64 {
	if r.Type != gjson.Number {
		return nil
	}
	v := r.Float()
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > float64(maxSafeCount) || math.Trunc(v) != v {
		return nil
	}
	return ptr(int64(v))
}
func metric(n *int64, source string) Metric {
	if n == nil {
		return Metric{Source: "unknown"}
	}
	return Metric{n, true, source}
}

// usageSnapshot replaces cumulative frames; it never adds repeated usage.
func usageSnapshot(root gjson.Result, p, source string) (Usage, bool) {
	u := unknownUsage(p)
	var raw gjson.Result
	switch u.Protocol {
	case "gemini":
		raw = root.Get("usageMetadata")
	case "claude":
		raw = root.Get("usage")
		if !raw.IsObject() {
			raw = root.Get("message.usage")
		}
	default:
		raw = root.Get("usage")
	}
	if !raw.IsObject() {
		return u, false
	}
	u.Basis = "provider_cumulative"
	if source == "converted" {
		u.Basis = "converted"
	}
	get := func(name, path string) *int64 {
		n := number(raw.Get(path))
		if n != nil {
			u.Raw[name] = n
		}
		return n
	}
	switch u.Protocol {
	case "gemini":
		u.Input = metric(get("promptTokenCount", "promptTokenCount"), source)
		u.Candidate = metric(get("candidatesTokenCount", "candidatesTokenCount"), source)
		u.Reasoning = metric(get("thoughtsTokenCount", "thoughtsTokenCount"), source)
		get("totalTokenCount", "totalTokenCount")
		get("cachedContentTokenCount", "cachedContentTokenCount")
		// Absent components are unknown, not an invented zero. The delivered
		// protocol's explicit output field is observed independently below.
		if u.Candidate.Present && u.Reasoning.Present && *u.Candidate.Value <= maxSafeCount-*u.Reasoning.Value {
			u.OutputTotal = metric(ptr(*u.Candidate.Value+*u.Reasoning.Value), source)
			u.ReasoningIncluded = ptr(true)
		}
	case "openai_chat":
		u.Input = metric(get("prompt_tokens", "prompt_tokens"), source)
		u.OutputTotal = metric(get("completion_tokens", "completion_tokens"), source)
		u.Reasoning = metric(get("reasoning_tokens", "completion_tokens_details.reasoning_tokens"), source)
		get("total_tokens", "total_tokens")
		u.ReasoningIncluded = ptr(true)
	case "openai_responses":
		u.Input = metric(get("input_tokens", "input_tokens"), source)
		u.OutputTotal = metric(get("output_tokens", "output_tokens"), source)
		u.Reasoning = metric(get("reasoning_tokens", "output_tokens_details.reasoning_tokens"), source)
		get("total_tokens", "total_tokens")
		u.ReasoningIncluded = ptr(true)
	case "claude":
		u.Input = metric(get("input_tokens", "input_tokens"), source)
		u.OutputTotal = metric(get("output_tokens", "output_tokens"), source)
		u.ReasoningIncluded = ptr(true)
	}
	return u, true
}

type tailMessage struct {
	Index int      `json:"index"`
	Role  string   `json:"role"`
	Kinds []string `json:"partKinds"`
	Text  *int64   `json:"textUtf8Bytes"`
}
type structure struct {
	Messages  *int64        `json:"messageCount"`
	Empty     *int64        `json:"emptyMessageCount"`
	Tools     *int64        `json:"toolCallCount"`
	Responses *int64        `json:"toolResponseCount"`
	Tail      []tailMessage `json:"tail"`
	Complete  bool          `json:"complete"`
}
type transformation struct {
	Operation string `json:"operation"`
	Index     int    `json:"index"`
	Reason    string `json:"reason"`
}
type normalizedData struct {
	Before    structure        `json:"before"`
	After     structure        `json:"after"`
	Changes   []transformation `json:"transformations"`
	Requested *string          `json:"requestedModelAlias"`
	Effective *string          `json:"effectiveModelAlias"`
}

func summarizeStructure(b []byte) structure {
	s := structure{Tail: []tailMessage{}}
	if !boundedJSON(b) {
		return s
	}
	r := gjson.ParseBytes(b)
	if r.Get("request").IsObject() {
		r = r.Get("request")
	}
	contents := r.Get("contents")
	if !contents.IsArray() {
		return s
	}
	n, empty, calls, responses := int64(0), int64(0), int64(0), int64(0)
	contents.ForEach(func(_, message gjson.Result) bool {
		role := message.Get("role").String()
		switch role {
		case "user", "model", "assistant", "system", "tool":
		default:
			role = "unknown"
		}
		d := tailMessage{Index: int(n), Role: role, Kinds: []string{}, Text: ptr(int64(0))}
		kinds := map[string]bool{}
		effective := false
		message.Get("parts").ForEach(func(_, part gjson.Result) bool {
			kind := "other"
			switch {
			case part.Get("functionCall").IsObject():
				kind = "tool_call"
				calls++
				effective = true
			case part.Get("functionResponse").IsObject():
				kind = "tool_response"
				responses++
				effective = true
			case part.Get("inlineData").IsObject() || part.Get("fileData").IsObject():
				kind = "media"
				effective = true
			case part.Get("text").Type == gjson.String:
				kind = "text"
				text := part.Get("text").String()
				*d.Text += int64(len(text))
				effective = effective || strings.TrimSpace(text) != ""
				if part.Get("thought").Bool() {
					kind = "thought"
				}
			default:
				effective = true
			}
			if !kinds[kind] {
				d.Kinds = append(d.Kinds, kind)
				kinds[kind] = true
			}
			return true
		})
		if !effective {
			empty++
		}
		n++
		s.Tail = append(s.Tail, d)
		if len(s.Tail) > 4 {
			s.Tail = s.Tail[1:]
		}
		return true
	})
	s.Messages, s.Empty, s.Tools, s.Responses, s.Complete = &n, &empty, &calls, &responses, true
	return s
}

// ObserveNormalized compares bounded message structures and contents only.
// Positional differences do not identify a cleanup operation or its reason.
func ObserveNormalized(ctx context.Context, before, after []byte) {
	Guard(func() {
		if !DebugActive(ctx) {
			return
		}
		d := normalizedData{Before: summarizeStructure(before), After: summarizeStructure(after), Changes: []transformation{}}
		if d.Before.Complete && d.After.Complete {
			contents := func(b []byte) []gjson.Result {
				r := gjson.ParseBytes(b)
				if r.Get("request").IsObject() {
					r = r.Get("request")
				}
				return r.Get("contents").Array()
			}
			left, right := contents(before), contents(after)
			for i := 0; i < max(len(left), len(right)) && len(d.Changes) < 16; i++ {
				if i >= len(left) || i >= len(right) || left[i].Raw != right[i].Raw {
					d.Changes = append(d.Changes, transformation{"other", i, "other"})
				}
			}
		}
		semantic(ctx, "request.normalized", d)
	})
}

// Exchange is owned by one executor invocation, not by HTTP send counts. It
// retains only bounded numeric evidence. Unknown business attempt IDs stay null.
type Exchange struct {
	owner                *ServerSpan
	finishOnce           sync.Once
	ctx                  context.Context
	started              time.Time
	up, down             responseObservation
	stream, clientStream bool
	ended, converted     bool
	readErr              bool
	status               int
	timing               Timing
}
type responseObservation struct {
	usage                                                            Usage
	output                                                           Output
	malformed, limited, unsupported, terminal, failed, blocked, seen bool
	candidates                                                       map[int64]bool
}

func newObservation(p string) responseObservation {
	return responseObservation{candidates: make(map[int64]bool), usage: unknownUsage(p), output: Output{ptr(int64(0)), ptr(int64(0)), ptr(int64(0)), ptr(int64(0)), ptr(int64(0)), ptr(int64(0))}}
}
func NewExchange(ctx context.Context, outputProtocol string, upstreamStream, clientStream bool) (exchange *Exchange) {
	defer func() {
		if recover() != nil {
			exchange = nil
		}
	}()
	s := ServerFromContext(ctx)
	if s == nil {
		return nil
	}
	started := time.Now()
	if a, ok := ctx.Value(attemptKey{}).(attemptIdentity); ok {
		started = a.started
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.debugActiveLocked() {
		return nil
	}
	x := &Exchange{owner: s, ctx: ctx, started: started, up: newObservation("gemini"), down: newObservation(outputProtocol), stream: upstreamStream, clientStream: clientStream, timing: Timing{Source: "server_monotonic"}}
	// Registration and sealing share one lock, so no late exchange can escape
	// the terminal's pending-lifecycle check. No response bytes are retained.
	s.pendingExchanges++
	return x
}
func (x *Exchange) active() bool { return x != nil && DebugActive(x.ctx) }
func (x *Exchange) Status(status int) {
	if x != nil {
		x.status = status
	}
}
func (x *Exchange) Upstream(b []byte) {
	Guard(func() {
		if !x.active() {
			return
		}
		x.up.observe(b, "upstream", x.stream)
		if x.timing.FirstEffective == nil && effective(x.up.output) {
			if s := ServerFromContext(x.ctx); s != nil {
				x.timing.FirstEffective = ptr(elapsed(s.started))
			}
		}
	})
}
func (x *Exchange) Delivered(b []byte) {
	Guard(func() {
		if !x.active() {
			return
		}
		x.converted = true
		x.down.observe(b, "converted", x.clientStream)
	})
}
func (x *Exchange) ReadFinished(err error) {
	if x != nil {
		x.ended = true
		x.readErr = err != nil
	}
}
func effective(o Output) bool {
	return (o.NonWhitespace != nil && *o.NonWhitespace > 0) || (o.Tools != nil && *o.Tools > 0) || (o.Media != nil && *o.Media > 0)
}
func (o *responseObservation) text(text string, thought bool) {
	if thought {
		*o.output.Thought += int64(len(text))
		return
	}
	*o.output.Text += int64(len(text))
	for _, r := range text {
		if !unicode.IsSpace(r) {
			*o.output.NonWhitespace++
		}
	}
}
func (o *responseObservation) observe(b []byte, source string, stream bool) {
	b = bytes.TrimSpace(b)
	// Translators may return one or several complete SSE events. No frame is
	// buffered or reconstructed across calls, and no raw bytes survive this call.
	if bytes.HasPrefix(b, []byte("event:")) || bytes.HasPrefix(b, []byte("data:")) {
		for line := range bytes.SplitSeq(b, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				o.observePayload(bytes.TrimSpace(line[5:]), source, stream)
			}
		}
		return
	}
	o.observePayload(b, source, stream)
}

// SSE decoding is deliberately one level only. Nested data: prefixes are invalid
// JSON rather than recursively decoded input controlled by an upstream.
func (o *responseObservation) observePayload(b []byte, source string, stream bool) {
	if bytes.Equal(b, []byte("[DONE]")) {
		return
	}
	valid, limited := inspectJSON(b)
	if !valid {
		o.limited = o.limited || limited
		o.malformed = o.malformed || !limited
		return
	}
	r := gjson.ParseBytes(b)
	if !r.IsObject() {
		o.malformed = true
		return
	}
	if r.Get("response").IsObject() {
		r = r.Get("response")
	}
	o.seen = true
	if u, ok := usageSnapshot(r, o.usage.Protocol, source); ok {
		if o.usage.Protocol == "claude" && !u.Input.Present {
			u.Input = o.usage.Input
			if n := o.usage.Raw["input_tokens"]; n != nil {
				u.Raw["input_tokens"] = n
			}
		}
		o.usage = u
	}
	if r.Get("error").Exists() {
		o.failed = true
	}
	if r.Get("promptFeedback.blockReason").String() != "" {
		o.blocked = true
	}
	switch o.usage.Protocol {
	case "gemini":
		candidates := r.Get("candidates").Array()
		if int64(len(candidates)) > *o.output.Candidates {
			*o.output.Candidates = int64(len(candidates))
		}
		for ordinal, c := range candidates {
			index := int64(ordinal)
			if n := number(c.Get("index")); n != nil {
				index = *n
			}
			if _, known := o.candidates[index]; !known {
				if len(o.candidates) >= 64 {
					o.limited = true
					continue
				}
				o.candidates[index] = false
			}
			finish := c.Get("finishReason")
			if finish.Exists() && finish.Type != gjson.String {
				o.unsupported = true
			}
			switch finish.Str {
			case "STOP", "MAX_TOKENS":
				o.candidates[index] = true
			case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
				o.candidates[index] = true
				o.blocked = true
			case "":
			default:
				// A provider string outside our vocabulary is terminal evidence,
				// not invalid JSON and not proof of successful generation.
				o.candidates[index] = true
				o.unsupported = true
			}
			for _, part := range c.Get("content.parts").Array() {
				if text := part.Get("text"); text.Type == gjson.String {
					o.text(text.String(), part.Get("thought").Bool())
				}
				if call := part.Get("functionCall"); call.IsObject() && call.Get("name").Type == gjson.String && call.Get("name").String() != "" && call.Get("args").IsObject() {
					*o.output.Tools++
				}
				if (part.Get("inlineData.mimeType").String() != "" && part.Get("inlineData.data").String() != "") || (part.Get("fileData.mimeType").String() != "" && part.Get("fileData.fileUri").String() != "") {
					*o.output.Media++
				}
			}
		}
		o.terminal = len(o.candidates) > 0
		for _, terminal := range o.candidates {
			o.terminal = o.terminal && terminal
		}
		*o.output.Candidates = int64(len(o.candidates))
	case "openai_chat":
		choices := r.Get("choices").Array()
		if int64(len(choices)) > *o.output.Candidates {
			*o.output.Candidates = int64(len(choices))
		}
		for _, c := range choices {
			m := c.Get("message")
			if stream {
				m = c.Get("delta")
			}
			o.text(m.Get("content").String(), false)
			o.text(m.Get("reasoning_content").String(), true)
			switch c.Get("finish_reason").String() {
			case "stop", "length", "tool_calls":
				o.terminal = true
			case "content_filter":
				o.terminal = true
				o.blocked = true
			}
			if !stream {
				for _, t := range m.Get("tool_calls").Array() {
					if t.Get("function.name").String() != "" && gjson.Valid(t.Get("function.arguments").String()) {
						*o.output.Tools++
					}
				}
			} else if m.Get("tool_calls").Exists() {
				o.output.Tools = nil
			}
		}
	case "claude", "openai_responses":
		// Usage remains observable. Detailed output parsing for these protocols
		// is outside this Gemini-focused projection; do not assert success.
		o.output = Output{}
	}
}

func (x *Exchange) Finish(err error) {
	if x == nil {
		return
	}
	x.finishOnce.Do(func() {
		defer func() {
			x.owner.mu.Lock()
			x.owner.pendingExchanges--
			x.owner.mu.Unlock()
		}()
		x.finish(err)
	})
}

func (x *Exchange) finish(err error) {
	Guard(func() {
		if !x.active() {
			return
		}
		result, origin, stage, class := "unknown", "unknown", "unknown", "unknown"
		if x.ctx.Err() != nil {
			result, origin, stage, class = "cancelled", "unknown", "read", "cancelled"
		} else if err != nil && x.status == 0 {
			result, origin, stage, class = "error", "upstream", "dispatch", "transport_error"
		} else if err != nil || x.readErr || x.status >= 400 || x.up.failed {
			result, origin, stage, class = "error", "upstream", "read", "other"
		} else if x.up.malformed {
			result, origin, stage, class = "incomplete", "upstream", "parse", "parse_error"
		} else if x.up.blocked {
			result, origin, stage, class = "blocked", "upstream", "read", "blocked"
		} else if x.up.limited || x.up.unsupported {
			// Keep the default unknown classification; limits are local evidence.
		} else if x.ended && x.up.seen {
			if !x.up.terminal {
				result, origin, stage, class = "incomplete", "upstream", "read", "incomplete"
			} else if effective(x.up.output) {
				result, origin, stage, class = "success", "none", "none", "none"
			} else {
				result, origin, stage, class = "empty", "upstream", "read", "empty"
			}
		}
		parsed := ptr(!x.up.malformed && x.up.seen && x.ended && !x.readErr)
		terminal := ptr(x.up.terminal)
		if x.up.limited {
			parsed, terminal = nil, nil
		}
		semantic(x.ctx, "upstream.attempt_finished", attemptData{Result: result, Origin: origin, Stage: stage, Error: class, Usage: x.up.usage, Output: x.up.observedOutput(), Terminal: terminal, EOF: ptr(x.ended && !x.readErr), Parsed: parsed, Total: ptr(elapsed(x.started)), CredentialScope: "unknown", Timing: x.timing})
		if x.converted {
			mode := "nonstream"
			if x.clientStream {
				mode = "stream"
			} else if x.stream {
				mode = "collected"
			}
			deliveredResult := result
			if x.down.malformed || x.down.limited || x.down.unsupported || !effective(x.down.output) {
				deliveredResult = "unknown"
			}
			semantic(x.ctx, "response.converted", convertedData{"gemini", x.down.usage.Protocol, x.clientStream, x.stream, mode, x.up.usage, x.down.usage, x.down.observedOutput(), deliveredResult})
		}
	})
}

// Partial counts cannot be advertised as complete output aggregates.
func (o *responseObservation) observedOutput() Output {
	if !o.seen || o.malformed || o.limited {
		return Output{}
	}
	return o.output
}
```

## `internal/diagnostics/attempt.go`

SHA-256 (LF): `4ff077c0a0b2ca94f57c6613b7612471343c801aefabd940d789e294bf8dedcc`

```go
package diagnostics

import (
	"context"
	"time"
)

type attemptKey struct{}
type attemptIdentity struct {
	id, scope string
	number    uint64
	started   time.Time
}

// ExecutorAttempt is invoked by the conductor at each actual model executor
// dispatch, including its explicit retries. HTTP sends never allocate attempts.
// Only the instrumented Gemini family participates in this semantic scope.
func ExecutorAttempt(ctx context.Context, provider string) context.Context {
	if provider != "gemini" && provider != "antigravity" {
		return ctx
	}
	s := ServerFromContext(ctx)
	if s == nil {
		return ctx
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return ctx
	}
	s.attempts++
	return context.WithValue(ctx, attemptKey{}, attemptIdentity{randomHex(8), "conductor_gemini_family", s.attempts, time.Now()})
}

func applyAttempt(ctx context.Context, r *Record) {
	if a, ok := ctx.Value(attemptKey{}).(attemptIdentity); ok {
		r.AttemptID = &a.id
		r.AttemptNo = &a.number
		r.RetryScope = &a.scope
	}
}
```

## `internal/diagnostics/semantic.go`

SHA-256 (LF): `6f0e2c8337cd29d440fc38ba398d4b41a267601ed31907d8f809e21e6598829e`

```go
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
```

## `internal/diagnostics/records.go`

SHA-256 (LF): `0ebad2b8cffb33b5bfb2fca5323353489bb209e96d6fb7964ac92369d1a541bc`

```go
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
	pendingExchanges             uint64
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
	// Never wait for an executor or read its mutable response observation. A
	// registered exchange still outstanding at sealing proves capture ended
	// before its lifecycle settled, even if its later Finish is suppressed.
	if s.pendingExchanges > 0 {
		s.debugInterrupted = true
	}
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
```

## `internal/runtime/executor/gemini_executor.go`

SHA-256 (LF): `01e406d8fc3c7757d74db7c63330972108a719df2849f722765ccebc065f469b`

```go
// Package executor provides runtime execution capabilities for various AI service providers.
// It includes stateless executors that handle API requests, streaming responses,
// token counting, and authentication refresh for different AI service providers.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	internalsignature "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// glEndpoint is the base URL for the Google Generative Language API.
	glEndpoint = "https://generativelanguage.googleapis.com"

	// glAPIVersion is the API version used for Gemini requests.
	glAPIVersion = "v1beta"

	// streamScannerBuffer is the buffer size for SSE stream scanning.
	streamScannerBuffer = 52_428_800

	// geminiInteractionsAPIRevision is the default API revision for native Interactions requests.
	geminiInteractionsAPIRevision = "2026-05-20"
)

// GeminiExecutor is a stateless executor for the official Gemini API using API keys.
// It supports regular and streaming requests to the Google Generative Language API.
type GeminiExecutor struct {
	// cfg holds the application configuration.
	cfg        *config.Config
	identifier string
}

// NewGeminiExecutor creates a new Gemini executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *GeminiExecutor: A new Gemini executor instance
func NewGeminiExecutor(cfg *config.Config) *GeminiExecutor {
	return &GeminiExecutor{cfg: cfg, identifier: "gemini"}
}

// NewGeminiInteractionsExecutor creates a Gemini executor bound to the native Interactions provider.
func NewGeminiInteractionsExecutor(cfg *config.Config) *GeminiExecutor {
	return &GeminiExecutor{cfg: cfg, identifier: "gemini-interactions"}
}

// Identifier returns the executor identifier.
func (e *GeminiExecutor) Identifier() string {
	if e == nil || strings.TrimSpace(e.identifier) == "" {
		return "gemini"
	}
	return e.identifier
}

// RequestToFormat reports the upstream request format used after auth selection.
func (e *GeminiExecutor) RequestToFormat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	if strings.EqualFold(strings.TrimSpace(e.Identifier()), "gemini-interactions") && nativeInteractionsSourceFormat(opts.SourceFormat) {
		return sdktranslator.FormatInteractions
	}
	return sdktranslator.FormatGemini
}

// PrepareRequest injects Gemini credentials into the outgoing HTTP request.
func (e *GeminiExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey := geminiAPIKey(auth)
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
		req.Header.Del("Authorization")
	} else {
		req.Header.Del("x-goog-api-key")
		req.Header.Del("Authorization")
	}
	applyGeminiHeaders(req, auth)
	return nil
}

// HttpRequest injects Gemini credentials into the request and executes it.
func (e *GeminiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("gemini executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming request to the Gemini API.
// It translates the request to Gemini format, sends it to the API, and translates
// the response back to the requested format.
//
// Parameters:
//   - ctx: The context for the request
//   - auth: The authentication information
//   - req: The request to execute
//   - opts: Additional execution options
//
// Returns:
//   - cliproxyexecutor.Response: The response from the API
//   - error: An error if the request fails
func (e *GeminiExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if shouldExecuteNativeInteractions(auth, opts) {
		return e.executeInteractions(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey := geminiAPIKey(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	// Official Gemini API via API key.
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("gemini")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, body := helps.TranslateRequestPairWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, false, isCompat)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	body = fixGeminiImageAspectRatio(baseModel, body)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = capGeminiMaxOutputTokens(body, baseModel)
	body = internalsignature.SanitizeGeminiRequestThoughtSignatures(body, "contents")

	action := "generateContent"
	if req.Metadata != nil {
		if a, _ := req.Metadata["action"].(string); a == "countTokens" {
			action = "countTokens"
		}
	}
	body = helps.EnsureGeminiLeadingUserContent(body, "contents")
	if action != "countTokens" {
		body = helps.EnsureGeminiTrailingUserContent(body, "contents")
	}
	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, action)
	if opts.Alt != "" && action != "countTokens" {
		url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
	}

	body, _ = sjson.DeleteBytes(body, "session_id")
	reporter.SetTranslatedReasoningEffort(body, to.String())
	diagnostics.ObserveNormalized(ctx, originalPayloadSource, body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	diag := diagnostics.NewExchange(ctx, responseFormat.String(), false, false)
	defer func() { diag.Finish(err) }()
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close response body error: %v", errClose)
		}
	}()
	diag.Status(httpResp.StatusCode)
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, errReadDiagnostic := io.ReadAll(httpResp.Body)
		diag.Upstream(b)
		diag.ReadFinished(errReadDiagnostic)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	diag.Upstream(data)
	diag.ReadFinished(err)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if action == "generateContent" {
		helps.LogGeminiNonStreamingResponse(ctx, requestedModel, baseModel, body, data)
	}
	reporter.ObserveResponseModel(data)
	reporter.Publish(ctx, helps.ParseGeminiUsage(data))
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, data, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	outBytes := helps.RewriteResponseModelVersion([]byte(out), requestedModel, baseModel)
	diag.Delivered(outBytes)
	resp = cliproxyexecutor.Response{Payload: outBytes, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming request to the Gemini API.
func (e *GeminiExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if shouldExecuteNativeInteractions(auth, opts) {
		return e.executeInteractionsStream(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey := geminiAPIKey(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("gemini")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, body := helps.TranslateRequestPairWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, true, isCompat)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body = fixGeminiImageAspectRatio(baseModel, body)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = capGeminiMaxOutputTokens(body, baseModel)
	body = internalsignature.SanitizeGeminiRequestThoughtSignatures(body, "contents")
	body = helps.EnsureGeminiBoundaryUserContent(body, "contents")

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, "streamGenerateContent")
	if opts.Alt == "" {
		url = url + "?alt=sse"
	} else {
		url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
	}

	body, _ = sjson.DeleteBytes(body, "session_id")
	reporter.SetTranslatedReasoningEffort(body, to.String())
	diagnostics.ObserveNormalized(ctx, originalPayloadSource, body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	diag := diagnostics.NewExchange(ctx, responseFormat.String(), true, true)
	defer func() {
		if err != nil {
			diag.Finish(err)
		}
	}()
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	diag.Status(httpResp.StatusCode)
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, errReadDiagnostic := io.ReadAll(httpResp.Body)
		diag.Upstream(b)
		diag.ReadFinished(errReadDiagnostic)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() { diag.Finish(nil) }()
		defer reporter.EnsurePublished(ctx)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("gemini executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			if raw := helps.JSONPayload(line); len(raw) > 0 {
				diag.Upstream(raw)
			}
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			reporter.ObserveResponseModel(line)
			filtered := helps.FilterSSEUsageMetadata(line)
			payload := helps.JSONPayload(filtered)
			if len(payload) == 0 {
				continue
			}
			if detail, ok := helps.ParseGeminiStreamUsage(payload); ok {
				reporter.Publish(ctx, detail)
			}
			lines := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, bytes.Clone(payload), &param, claudeInputTokens)
			for i := range lines {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: helps.RewriteSSEModelVersion([]byte(lines[i]), requestedModel, baseModel)}:
					diag.Delivered([]byte(lines[i]))
				case <-ctx.Done():
					return
				}
			}
		}
		diag.ReadFinished(scanner.Err())
		lines := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param, claudeInputTokens)
		for i := range lines {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: helps.RewriteSSEModelVersion([]byte(lines[i]), requestedModel, baseModel)}:
				diag.Delivered([]byte(lines[i]))
			case <-ctx.Done():
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			// Settle live upstream failures before Err lets the handler seal the
			// span. Cancellation retains the existing interrupted cleanup path.
			if ctx.Err() == nil {
				diag.Finish(errScan)
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *GeminiExecutor) executeInteractions(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	targetName := thinking.ParseSuffix(req.Model).ModelName
	apiKey := geminiAPIKey(auth)
	reporter := helps.NewExecutorUsageReporter(ctx, e, targetName, auth)
	defer reporter.TrackFailure(ctx, &err)

	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, body := translateGeminiInteractionsRequestPair(ctx, e.cfg, targetName, req.Payload, opts, false, isCompat)
	if gjson.GetBytes(body, "model").Exists() && targetName != "" {
		body = helps.SetStringIfDifferent(body, "model", targetName)
	}
	body, err = applyGeminiInteractionsThinking(body, req, opts)
	if err != nil {
		return resp, err
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	fromProtocol := opts.SourceFormat.String()
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, targetName, "interactions", fromProtocol, "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = sanitizeGeminiInteractionsUnsupportedInputIDs(body)

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/interactions", baseURL, glAPIVersion)
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errRequest != nil {
		return resp, errRequest
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
	applyGeminiInteractionsRequestHeaders(httpReq, opts.Headers)
	applyGeminiInteractionsRevisionHeader(httpReq)

	authID, authLabel, authType, authValue := geminiAuthLogFields(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close interactions response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		return resp, err
	}
	reporter.ObserveResponseModel(data)
	reporter.Publish(ctx, helps.ParseInteractionsUsage(data))
	targetFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatInteractions, targetFormat, req.Model, opts.OriginalRequest, body, data, &param)
	if targetFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *GeminiExecutor) executeInteractionsStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	targetName := thinking.ParseSuffix(req.Model).ModelName
	apiKey := geminiAPIKey(auth)
	reporter := helps.NewExecutorUsageReporter(ctx, e, targetName, auth)
	defer reporter.TrackFailure(ctx, &err)

	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, body := translateGeminiInteractionsRequestPair(ctx, e.cfg, targetName, req.Payload, opts, true, isCompat)
	if gjson.GetBytes(body, "model").Exists() && targetName != "" {
		body = helps.SetStringIfDifferent(body, "model", targetName)
	}
	body, err = applyGeminiInteractionsThinking(body, req, opts)
	if err != nil {
		return nil, err
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	fromProtocol := opts.SourceFormat.String()
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, targetName, "interactions", fromProtocol, "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = sanitizeGeminiInteractionsUnsupportedInputIDs(body)
	body = helps.SetBoolIfDifferent(body, "stream", true)
	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/interactions", baseURL, glAPIVersion)
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errRequest != nil {
		return nil, errRequest
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
	applyGeminiInteractionsRequestHeaders(httpReq, opts.Headers)
	applyGeminiInteractionsRevisionHeader(httpReq)

	authID, authLabel, authType, authValue := geminiAuthLogFields(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close interactions error response body error: %v", errClose)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		return nil, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	go func() {
		defer close(out)
		defer reporter.EnsurePublished(ctx)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("gemini executor: close interactions stream body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		originalRequest := opts.OriginalRequest
		if len(originalRequest) == 0 {
			originalRequest = req.Payload
		}
		claudeInputTokens := helps.NewClaudeInputTokenState(opts.SourceFormat, sdktranslator.FormatInteractions, responseFormat, originalRequest)
		var param any
		var frame []byte
		emitFrame := func() bool {
			rawFrame := bytes.Clone(frame)
			trimmed := bytes.TrimSpace(rawFrame)
			frame = frame[:0]
			if len(trimmed) == 0 {
				return true
			}
			payload := geminiInteractionsSSEPayload(rawFrame)
			if len(payload) == 0 && geminiInteractionsSSEDone(rawFrame) {
				payload = []byte("[DONE]")
			}
			if len(payload) == 0 && len(trimmed) > 0 && trimmed[0] == '{' {
				payload = trimmed
			}
			if len(payload) > 0 {
				reporter.ObserveResponseModel(payload)
				if detail, ok := helps.ParseInteractionsStreamUsage(payload); ok {
					reporter.Publish(ctx, detail)
				}
			}
			if responseFormat == sdktranslator.FormatInteractions {
				visibleFrame := append(bytes.TrimRight(rawFrame, "\r\n"), '\n', '\n')
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: visibleFrame}:
				case <-ctx.Done():
					return false
				}
				return true
			}
			if len(payload) == 0 {
				return true
			}
			var lines [][]byte
			lines = helps.TranslateStreamWithClaudeInputTokens(ctx, sdktranslator.FormatInteractions, responseFormat, req.Model, opts.OriginalRequest, body, payload, &param, claudeInputTokens)
			for i := range lines {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: lines[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				if !emitFrame() {
					return
				}
				continue
			}
			if len(frame) > 0 {
				frame = append(frame, '\n')
			}
			frame = append(frame, line...)
		}
		if !emitFrame() {
			return
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens counts tokens for the given request using the Gemini API.
func (e *GeminiExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey := geminiAPIKey(auth)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("gemini")
	translatedReq := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, false, helps.APIKeyModelIsCompat(req))

	translatedReq, err := helps.ApplyRequestThinking(translatedReq, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translatedReq = fixGeminiImageAspectRatio(baseModel, translatedReq)
	respCtx := context.WithValue(ctx, "alt", opts.Alt)
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "tools")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "generationConfig")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "safetySettings")
	translatedReq = helps.SetStringIfDifferent(translatedReq, "model", baseModel)
	translatedReq = internalsignature.SanitizeGeminiRequestThoughtSignatures(translatedReq, "contents")
	translatedReq = helps.EnsureGeminiLeadingUserContent(translatedReq, "contents")

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, "countTokens")

	requestBody := bytes.NewReader(translatedReq)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, requestBody)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	}
	applyGeminiHeaders(httpReq, auth, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translatedReq,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			helps.LogWithRequestID(ctx).Errorf("response body close error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", resp.StatusCode, helps.SummarizeErrorBody(resp.Header.Get("Content-Type"), data))
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(data)}
	}

	count := gjson.GetBytes(data, "totalTokens").Int()
	translated := sdktranslator.TranslateTokenCount(respCtx, to, responseFormat, count, data)
	return cliproxyexecutor.Response{Payload: translated, Headers: resp.Header.Clone()}, nil
}

// Refresh refreshes the authentication credentials (no-op for Gemini API key).
func (e *GeminiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func geminiAPIKey(a *cliproxyauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			return v
		}
	}
	return ""
}

func resolveGeminiBaseURL(auth *cliproxyauth.Auth) string {
	base := glEndpoint
	if auth != nil && auth.Attributes != nil {
		if custom := strings.TrimSpace(auth.Attributes["base_url"]); custom != "" {
			base = strings.TrimRight(custom, "/")
		}
	}
	if base == "" {
		return glEndpoint
	}
	return base
}

func (e *GeminiExecutor) resolveGeminiConfig(auth *cliproxyauth.Auth) *config.GeminiKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.GeminiKey {
		entry := &e.cfg.GeminiKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range e.cfg.GeminiKey {
			entry := &e.cfg.GeminiKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func shouldExecuteNativeInteractions(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) bool {
	return nativeInteractionsSourceFormat(opts.SourceFormat) && isNativeInteractionsAuth(auth)
}

func nativeInteractionsSourceFormat(format sdktranslator.Format) bool {
	switch format {
	case sdktranslator.FormatInteractions, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, sdktranslator.FormatGemini:
		return true
	default:
		return false
	}
}

// sanitizeGeminiInteractionsUnsupportedInputIDs aligns input step IDs with the
// official Gemini Interactions API schema:
// - `function_call` (FunctionCallStep) requires `id` and rejects `call_id`
// - `function_result` (FunctionResultStep) requires `call_id` and rejects `id`
// - other steps and content parts do not support `id`
func sanitizeGeminiInteractionsUnsupportedInputIDs(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for i, item := range input.Array() {
		stepType := item.Get("type").String()
		if stepType == "function_call" {
			if !item.Get("id").Exists() && item.Get("call_id").Exists() {
				body, _ = sjson.SetBytes(body, fmt.Sprintf("input.%d.id", i), item.Get("call_id").String())
			}
			if item.Get("call_id").Exists() {
				body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.call_id", i))
			}
		} else {
			if item.Get("id").Exists() {
				body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.id", i))
			}
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for j, part := range content.Array() {
			if part.Get("id").Exists() {
				body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.content.%d.id", i, j))
			}
		}
	}
	return body
}

func translateGeminiInteractionsRequestBody(ctx context.Context, cfg *config.Config, model string, payload []byte, opts cliproxyexecutor.Options, stream, isCompat bool) []byte {
	if opts.SourceFormat == "" || opts.SourceFormat == sdktranslator.FormatInteractions {
		return bytes.Clone(payload)
	}
	return helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, cfg, opts.SourceFormat, sdktranslator.FormatInteractions, model, payload, stream, isCompat)
}

// translateGeminiInteractionsRequestPair translates the working payload and the
// payload-config baseline. Identical inputs are translated once when no plugin
// hooks are installed. The baseline is captured before model and thinking
// mutations, and the working buffer is a separate copy so those mutations cannot
// change it. Distinct inputs and plugin hooks keep the existing order: working
// payload first, then the payload-config source.
func translateGeminiInteractionsRequestPair(ctx context.Context, cfg *config.Config, model string, payload []byte, opts cliproxyexecutor.Options, stream, isCompat bool) (original, working []byte) {
	source := geminiInteractionsPayloadConfigInput(opts, payload)
	if geminiInteractionsSameByteSlice(payload, source) && !sdktranslator.HasPluginHooks() {
		original = translateGeminiInteractionsRequestBody(ctx, cfg, model, payload, opts, stream, isCompat)
		return original, bytes.Clone(original)
	}
	working = translateGeminiInteractionsRequestBody(ctx, cfg, model, payload, opts, stream, isCompat)
	original = geminiInteractionsPayloadConfigSource(ctx, cfg, model, payload, opts, stream, isCompat)
	return original, working
}

func geminiInteractionsPayloadConfigSource(ctx context.Context, cfg *config.Config, model string, payload []byte, opts cliproxyexecutor.Options, stream, isCompat bool) []byte {
	return translateGeminiInteractionsRequestBody(ctx, cfg, model, geminiInteractionsPayloadConfigInput(opts, payload), opts, stream, isCompat)
}

func geminiInteractionsPayloadConfigInput(opts cliproxyexecutor.Options, payload []byte) []byte {
	if len(opts.OriginalRequest) == 0 {
		return payload
	}
	return opts.OriginalRequest
}

// geminiInteractionsSameByteSlice reports whether both slices describe the same
// bytes of the same backing array. It compares identity rather than content so
// the check stays constant time on large payloads.
func geminiInteractionsSameByteSlice(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

func isNativeInteractionsAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "gemini-interactions")
}

func applyGeminiInteractionsThinking(body []byte, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]byte, error) {
	fromFormat := opts.SourceFormat.String()
	if strings.TrimSpace(fromFormat) == "" {
		fromFormat = sdktranslator.FormatInteractions.String()
	}
	return helps.ApplyRequestThinking(body, req, opts, fromFormat, sdktranslator.FormatInteractions.String(), "gemini")
}

func applyGeminiInteractionsRevisionHeader(req *http.Request) {
	if req == nil {
		return
	}
	if req.Header.Get("Api-Revision") == "" {
		req.Header.Set("Api-Revision", geminiInteractionsAPIRevision)
	}
}

func applyGeminiInteractionsRequestHeaders(req *http.Request, headers http.Header) {
	if req == nil || headers == nil || req.Header.Get("Api-Revision") != "" {
		return
	}
	if revision := headers.Get("Api-Revision"); revision != "" {
		req.Header.Set("Api-Revision", revision)
	}
}

func geminiInteractionsSSEPayload(frame []byte) []byte {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("{")) {
		return trimmed
	}
	lines := bytes.Split(frame, []byte{'\n'})
	var payload []byte
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[bytes.Index(line, []byte("data:"))+len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if len(payload) > 0 {
			payload = append(payload, '\n')
		}
		payload = append(payload, data...)
	}
	if len(payload) == 0 {
		return nil
	}
	return payload
}

func geminiInteractionsSSEDone(frame []byte) bool {
	trimmed := bytes.TrimSpace(frame)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return true
	}
	lines := bytes.Split(frame, []byte{'\n'})
	sawDoneEvent := false
	for _, line := range lines {
		line = bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if bytes.EqualFold(line, []byte("event: done")) {
			sawDoneEvent = true
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(line[len("data:"):])
			if bytes.Equal(data, []byte("[DONE]")) {
				return true
			}
		}
	}
	return sawDoneEvent
}

func geminiAuthLogFields(auth *cliproxyauth.Auth) (string, string, string, string) {
	if auth == nil {
		return "", "", "", ""
	}
	authType, authValue := auth.AccountInfo()
	return auth.ID, auth.Label, authType, authValue
}

func applyGeminiHeaders(req *http.Request, auth *cliproxyauth.Auth, clientHeaders ...http.Header) {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs, clientHeaders...)
}

func capGeminiMaxOutputTokens(body []byte, modelName string) []byte {
	maxOut := gjson.GetBytes(body, "generationConfig.maxOutputTokens")
	if !maxOut.Exists() || maxOut.Type != gjson.Number {
		return body
	}
	modelInfo := registry.LookupModelInfo(modelName, "gemini")
	if modelInfo == nil {
		return body
	}
	limit := modelInfo.OutputTokenLimit
	if limit <= 0 {
		limit = modelInfo.MaxCompletionTokens
	}
	if limit <= 0 || maxOut.Int() <= int64(limit) {
		return body
	}
	body, _ = sjson.SetBytes(body, "generationConfig.maxOutputTokens", limit)
	return body
}

func fixGeminiImageAspectRatio(modelName string, rawJSON []byte) []byte {
	if modelName == "gemini-2.5-flash-image-preview" {
		aspectRatioResult := gjson.GetBytes(rawJSON, "generationConfig.imageConfig.aspectRatio")
		if aspectRatioResult.Exists() {
			contents := gjson.GetBytes(rawJSON, "contents")
			contentArray := contents.Array()
			if len(contentArray) > 0 {
				hasInlineData := false
			loopContent:
				for i := 0; i < len(contentArray); i++ {
					parts := contentArray[i].Get("parts").Array()
					for j := 0; j < len(parts); j++ {
						if parts[j].Get("inlineData").Exists() {
							hasInlineData = true
							break loopContent
						}
					}
				}

				if !hasInlineData {
					emptyImageBase64ed, _ := util.CreateWhiteImageBase64(aspectRatioResult.String())
					emptyImagePart := []byte(`{"inlineData":{"mime_type":"image/png","data":""}}`)
					emptyImagePart, _ = sjson.SetBytes(emptyImagePart, "inlineData.data", emptyImageBase64ed)
					newPartsJson := []byte(`[]`)
					newPartsJson, _ = sjson.SetRawBytes(newPartsJson, "-1", []byte(`{"text": "Based on the following requirements, create an image within the uploaded picture. The new content *MUST* completely cover the entire area of the original picture, maintaining its exact proportions, and *NO* blank areas should appear."}`))
					newPartsJson, _ = sjson.SetRawBytes(newPartsJson, "-1", emptyImagePart)

					parts := contentArray[0].Get("parts").Array()
					for j := 0; j < len(parts); j++ {
						newPartsJson, _ = sjson.SetRawBytes(newPartsJson, "-1", []byte(parts[j].Raw))
					}

					rawJSON, _ = sjson.SetRawBytes(rawJSON, "contents.0.parts", newPartsJson)
					rawJSON, _ = sjson.SetRawBytes(rawJSON, "generationConfig.responseModalities", []byte(`["IMAGE", "TEXT"]`))
				}
			}
			rawJSON, _ = sjson.DeleteBytes(rawJSON, "generationConfig.imageConfig")
		}
	}
	return rawJSON
}
```

## `internal/runtime/executor/antigravity_executor.go`

SHA-256 (LF): `bac2ca4fcec08f50b80392d6ff16da6658fe25276376b5bac7c8c8ebf5dcf2f4`

```go
// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Antigravity executor that proxies requests to the antigravity
// upstream using OAuth credentials.
package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	internalsignature "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	antigravityclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/antigravity/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	antigravityBaseURLDaily                = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily         = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityBaseURLProd                 = "https://cloudcode-pa.googleapis.com"
	antigravityCountTokensPath             = "/v1internal:countTokens"
	antigravityStreamPath                  = "/v1internal:streamGenerateContent"
	antigravityGeneratePath                = "/v1internal:generateContent"
	antigravityClientID                    = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret                = "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"
	antigravityAuthType                    = "antigravity"
	antigravityRequestTokenSafetyWindow    = 5 * time.Minute
	antigravityCreditsHintRefreshInterval  = 10 * time.Minute
	antigravityCreditsHintRefreshTimeout   = 5 * time.Second
	antigravityShortQuotaCooldownThreshold = 5 * time.Minute
	antigravityInstantRetryThreshold       = 3 * time.Second
	// systemInstruction              = "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.**Absolute paths only****Proactiveness**"
)

// AntigravityExecutor proxies requests to the antigravity upstream.
type AntigravityExecutor struct {
	cfg *config.Config
}

// NewAntigravityExecutor creates a new Antigravity executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *AntigravityExecutor: A new Antigravity executor instance
func NewAntigravityExecutor(cfg *config.Config) *AntigravityExecutor {
	return &AntigravityExecutor{cfg: cfg}
}

func (e *AntigravityExecutor) obfuscateSensitiveWords(payload []byte) []byte {
	if e == nil || e.cfg == nil || len(e.cfg.Antigravity.SensitiveWords) == 0 {
		return payload
	}
	matcher := helps.BuildSensitiveWordMatcher(e.cfg.Antigravity.SensitiveWords)
	return helps.ObfuscateSensitiveWordsInSystemInstruction(payload, matcher)
}

// Each Antigravity credential gets its own HTTP/1.1 connection pool. Sessions routed
// to the same auth reuse that pool, while different OAuth identities never share a
// TCP/TLS connection, matching the native client's one-credential process model.
// The cache is bounded so pools cannot accumulate when keys churn.
var (
	antigravityBaseTransport = defaultAntigravityBaseTransport()
	antigravityTransports    = helps.NewTransportCache[antigravityTransportKey](antigravityTransportCacheCapacity)
)

const (
	// antigravityTransportCacheCapacity caps how many Antigravity connection pools stay
	// alive. The bound exists only to stop entries from accumulating when keys churn, for
	// example when a credential's proxy is rotated through the management API or when an
	// SDK embedder supplies a freshly built base transport per request.
	//
	// It is sized for large deployments on purpose. An unused cache entry costs under 1 KB
	// and no goroutines, so capacity is close to free, whereas evicting a pool that is
	// still in active use forces the next request on that credential to redo the TCP + TLS
	// handshake and defeats the point of caching. Credential counts in the low thousands
	// are expected once Home-managed pools are included.
	//
	// Capacity is therefore NOT the lever for bounding memory: an idle pooled connection
	// costs roughly 38 KB plus three goroutines, and that total is driven by live traffic
	// and reclaimed by IdleConnTimeout. Shrinking this number does not save that memory,
	// it only causes pool thrashing.
	antigravityTransportCacheCapacity = 8192

	// antigravityDefaultMaxIdleConnsPerHost sets the default number of idle connections
	// to retain per host per credential when connection pooling is enabled.
	// Matches Go's DefaultMaxIdleConnsPerHost (2) and the native Antigravity binary.
	antigravityDefaultMaxIdleConnsPerHost = 2

	// antigravityMaxAllowedMaxIdleConnsPerHost is the hard upper bound on MaxIdleConnsPerHost (100).
	// Prevents unbounded connection pool expansion in multi-credential environments.
	antigravityMaxAllowedMaxIdleConnsPerHost = 100

	// antigravityDefaultIdleConnTimeout is the default idle connection timeout (30 seconds).
	// Kept strictly far below Google Frontend (GFE / ESF) 240-second cutoff to prevent
	// client-side reuse of half-closed connections that cause connection resets.
	antigravityDefaultIdleConnTimeout = 30 * time.Second

	// antigravityMaxAllowedIdleConnTimeout is the hard upper bound on IdleConnTimeout (210 seconds).
	// Kept strictly below Google Frontend (GFE / ESF) 240.0-second HTTP/1.1 idle keep-alive cutoff
	// with a 30-second safety margin to eliminate timer race conditions.
	antigravityMaxAllowedIdleConnTimeout = 210 * time.Second

	// antigravityAnonymousTransportScope is the pool scope for auth objects that carry
	// no identity at all. Reaching it means the auth has no ID, no source path and no
	// token of any kind, so there is no credential to keep isolated and a single shared
	// pool is safe. Allocating a private pool per request instead would leak a
	// connection pool, and the goroutines managing it, on every call.
	antigravityAnonymousTransportScope = "anonymous"
)

type antigravityPoolSettings struct {
	shortMode           bool
	idleConnTimeout     time.Duration
	maxIdleConnsPerHost int
}

func resolveAntigravityPoolSettings(cfg *config.Config) antigravityPoolSettings {
	// By default, upstream connection pooling is disabled (shortMode = true, maxIdleConnsPerHost = -1)
	// to prevent socket buildup and stale connection errors across rotating credentials.
	settings := antigravityPoolSettings{
		shortMode:           true,
		maxIdleConnsPerHost: -1,
		idleConnTimeout:     0,
	}
	if cfg == nil {
		return settings
	}

	// Pooling is active ONLY when explicitly enabled: true
	if cfg.Antigravity.ConnectionPool.Enabled == nil || !*cfg.Antigravity.ConnectionPool.Enabled {
		return settings
	}

	// Enabled is true: initialize default pool settings
	settings.shortMode = false
	settings.idleConnTimeout = antigravityDefaultIdleConnTimeout
	settings.maxIdleConnsPerHost = antigravityDefaultMaxIdleConnsPerHost

	rawTimeout := strings.TrimSpace(cfg.Antigravity.ConnectionPool.IdleConnTimeout)
	if rawTimeout != "" {
		d, err := time.ParseDuration(rawTimeout)
		if err != nil {
			log.Warnf("antigravity executor: invalid idle-conn-timeout %q: %v, using default %v", rawTimeout, err, antigravityDefaultIdleConnTimeout)
		} else {
			if d <= 0 {
				settings.shortMode = true
				settings.maxIdleConnsPerHost = -1
				return settings
			}
			if d > antigravityMaxAllowedIdleConnTimeout {
				d = antigravityMaxAllowedIdleConnTimeout
			}
			settings.idleConnTimeout = d
		}
	}

	if cfg.Antigravity.ConnectionPool.MaxIdleConnsPerHost != nil {
		val := *cfg.Antigravity.ConnectionPool.MaxIdleConnsPerHost
		if val < 0 {
			settings.shortMode = true
			settings.maxIdleConnsPerHost = -1
			return settings
		}
		if val > antigravityMaxAllowedMaxIdleConnsPerHost {
			val = antigravityMaxAllowedMaxIdleConnsPerHost
		}
		settings.maxIdleConnsPerHost = val
	}

	if settings.maxIdleConnsPerHost < 0 {
		settings.shortMode = true
	}

	return settings
}

// ResetAntigravityTransports purges all cached Antigravity connection pools and closes their idle connections.
// Used during configuration hot-reloads to ensure updated pool parameters apply immediately.
func ResetAntigravityTransports() {
	antigravityTransports.Purge()
}

// AntigravityTransportsLen reports the current number of cached Antigravity transports.
func AntigravityTransportsLen() int {
	return antigravityTransports.Len()
}

// closeAntigravityAuthIdleTransports closes and removes all idle connections for the given auth.
func closeAntigravityAuthIdleTransports(auth *cliproxyauth.Auth) {
	if auth == nil {
		return
	}
	scope := antigravityTransportScope(auth)
	if scope == "" || scope == antigravityAnonymousTransportScope {
		return
	}
	antigravityTransports.CloseMatching(func(key antigravityTransportKey) bool {
		return key.credential == scope
	})
}

// antigravityTransportKey identifies one connection pool. At most one of proxy and
// base is set: proxy for a credential-scoped proxy pool, base for a transport handed
// in through the request context, and neither for a direct pool.
// Resolved pool settings (shortMode, idleConnTimeout, maxIdleConnsPerHost) are included
// in the key to prevent stale transport reuse across configuration hot-reloads under load.
type antigravityTransportKey struct {
	credential          string
	proxy               string
	base                *http.Transport
	shortMode           bool
	idleConnTimeout     time.Duration
	maxIdleConnsPerHost int
}

func defaultAntigravityBaseTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport
	}
	return &http.Transport{}
}

func cloneTransportWithHTTP11(base *http.Transport, cfgs ...*config.Config) *http.Transport {
	if base == nil {
		return nil
	}

	clone := base.Clone()
	clone.ForceAttemptHTTP2 = false
	// Wipe TLSNextProto to prevent implicit HTTP/2 upgrade.
	clone.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	// Native Antigravity sends no ALPN extension. With HTTP/2 disabled above,
	// an empty NextProtos keeps the wire shape aligned while using HTTP/1.1.
	clone.TLSClientConfig.NextProtos = nil
	applyAntigravityPoolLimits(clone, cfgs...)
	return clone
}

// applyAntigravityPoolLimits configures connection pool parameters for Antigravity.
// Default: IdleConnTimeout=30s, MaxIdleConnsPerHost=2.
// IdleConnTimeout is strictly capped at 240s (antigravityMaxAllowedIdleConnTimeout)
// matching the Google Frontend (GFE / ESF) HTTP/1.1 idle cutoff.
// If short-lived connection mode is configured, MaxIdleConnsPerHost is set to -1
// so idle connections are closed immediately upon request completion.
func applyAntigravityPoolLimits(transport *http.Transport, cfgs ...*config.Config) {
	if transport == nil {
		return
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	if settings.shortMode {
		transport.MaxIdleConnsPerHost = -1
		transport.DisableKeepAlives = false
		transport.IdleConnTimeout = 0
		return
	}

	// If the operator base transport already disabled pooling (< 0), honor it.
	if transport.MaxIdleConnsPerHost < 0 {
		return
	}

	// Go treats 0 as DefaultMaxIdleConnsPerHost (2). Ensure at least configured/default limit.
	if transport.MaxIdleConnsPerHost < settings.maxIdleConnsPerHost {
		transport.MaxIdleConnsPerHost = settings.maxIdleConnsPerHost
	}

	// MaxIdleConns caps the pool across all hosts.
	if transport.MaxIdleConns > 0 && transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		transport.MaxIdleConns = transport.MaxIdleConnsPerHost
	}

	// Apply IdleConnTimeout:
	// If the config explicitly specified a timeout, apply settings.idleConnTimeout.
	// If config did not specify a timeout:
	// - if base transport already has a timeout > 0, preserve it (capped at 240s).
	// - otherwise, apply default 30s.
	rawTimeout := ""
	if cfg != nil {
		rawTimeout = strings.TrimSpace(cfg.Antigravity.ConnectionPool.IdleConnTimeout)
	}
	if rawTimeout != "" {
		transport.IdleConnTimeout = settings.idleConnTimeout
	} else if transport.IdleConnTimeout == 0 || transport.IdleConnTimeout == 90*time.Second {
		transport.IdleConnTimeout = settings.idleConnTimeout
	} else if transport.IdleConnTimeout > antigravityMaxAllowedIdleConnTimeout {
		transport.IdleConnTimeout = antigravityMaxAllowedIdleConnTimeout
	}
}

// antigravityHTTP11Transport returns the HTTP/1.1 pool shared by every request that
// uses the same credential and the same base transport. The base is either the
// process default or a transport provided through the request context.
func antigravityHTTP11Transport(auth *cliproxyauth.Auth, base *http.Transport, cfgs ...*config.Config) *http.Transport {
	if base == nil {
		return nil
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	key := antigravityTransportKey{
		credential:          antigravityTransportScope(auth),
		base:                base,
		shortMode:           settings.shortMode,
		idleConnTimeout:     settings.idleConnTimeout,
		maxIdleConnsPerHost: settings.maxIdleConnsPerHost,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		return cloneTransportWithHTTP11(base, cfgs...), nil
	})
	if errGet != nil {
		// Defensive only: the builder above cannot fail. Never return nil here, because a
		// nil Transport makes http.Client fall back to http.DefaultTransport, which
		// advertises h2 over ALPN and would break the Antigravity wire fingerprint.
		log.Debugf("antigravity executor: cache HTTP/1.1 transport failed: %v", errGet)
		return cloneTransportWithHTTP11(base, cfgs...)
	}
	return transport
}

// antigravityProxiedHTTP11Transport returns the credential-scoped HTTP/1.1 pool for
// one proxy setting, or nil when the proxy setting cannot be turned into a
// transport. Keying on the normalized proxy string rather than on a prebuilt
// transport keeps one pool per credential and proxy instead of one per request.
func antigravityProxiedHTTP11Transport(auth *cliproxyauth.Auth, proxyURL string, cfgs ...*config.Config) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	key := antigravityTransportKey{
		credential:          antigravityTransportScope(auth),
		proxy:               proxyURL,
		shortMode:           settings.shortMode,
		idleConnTimeout:     settings.idleConnTimeout,
		maxIdleConnsPerHost: settings.maxIdleConnsPerHost,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		base, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
		if errBuild != nil {
			return nil, errBuild
		}
		if base == nil {
			return nil, fmt.Errorf("antigravity executor: proxy setting produced no transport")
		}
		return cloneTransportWithHTTP11(base, cfgs...), nil
	})
	if errGet != nil {
		// The caller falls back to NewProxyAwareHTTPClient, which reports the failure
		// and applies the context transport fallback.
		return nil
	}
	return transport
}

// antigravityTransportScope returns the connection-pool scope for one credential.
// Runtime auths always carry an ID. Incomplete auth objects, such as those built by
// tests, plugins or SDK embedders, fall back to another stable credential marker so
// they neither share a pool with an unrelated OAuth identity nor allocate a fresh
// pool, and with it a fresh set of pool goroutines, on every single request.
func antigravityTransportScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return antigravityAnonymousTransportScope
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "id:" + id
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributePath]); path != "" {
			return "path:" + path
		}
		if source := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeSource]); source != "" {
			return "source:" + source
		}
	}
	// Fall back to the credential material itself. Auth.Label is deliberately not used:
	// it is documented as an optional human readable label for logging and carries no
	// uniqueness guarantee, so two different OAuth identities sharing one label would
	// wrongly share a TCP/TLS pool.
	//
	// The refresh token is preferred over the access token because it stays stable
	// across token rotation. Keying on the access token would move a credential to a new
	// pool on every refresh, and would also strand refresh requests themselves, which
	// run before any access token exists.
	if refresh := strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token")); refresh != "" {
		return antigravityCredentialScope("refresh:", refresh)
	}
	if access := strings.TrimSpace(metaStringValue(auth.Metadata, "access_token")); access != "" {
		return antigravityCredentialScope("token:", access)
	}
	return antigravityAnonymousTransportScope
}

// antigravityCredentialScope derives a pool scope from secret credential material.
// Only a short digest is retained, and it is never logged, so a pool key cannot be
// used to recover the credential it came from.
func antigravityCredentialScope(prefix, secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return prefix + hex.EncodeToString(digest[:8])
}

// newAntigravityHTTPClient creates an HTTP client specifically for Antigravity,
// enforcing HTTP/1.1 by disabling HTTP/2 to match the native Antigravity client, which
// negotiates TLS 1.3 without advertising an ALPN protocol and therefore never uses h2.
// The underlying Transport is always shared so keep-alive connections survive across
// requests instead of forcing a fresh TCP + TLS handshake every time.
func newAntigravityHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) (client *http.Client) {
	defer func() { client = diagnostics.FinalizeClient(ctx, client) }()
	// Native Antigravity reuses one transport across requests. Opt into a
	// credential-scoped proxy transport only here so other providers keep their
	// existing lifecycle and different OAuth identities remain isolated.
	if proxyURL := antigravityProxyURL(ctx, cfg, auth); proxyURL != "" {
		if transport := antigravityProxiedHTTP11Transport(auth, proxyURL, cfg); transport != nil {
			return &http.Client{Transport: transport, Timeout: timeout}
		}
		// Fall through so NewProxyAwareHTTPClient reports the failure and applies the
		// context transport fallback, preserving the previous behavior.
	}

	client = helps.NewProxyAwareHTTPClientBase(ctx, cfg, auth, timeout)
	// Direct requests share an HTTP/1.1 pool only within the selected credential.
	if client.Transport == nil {
		client.Transport = antigravityHTTP11Transport(auth, antigravityBaseTransport, cfg)
		return client
	}

	// Preserve a context-provided transport while forcing HTTP/1.1. The cache key
	// includes credential identity, so sharing the base does not share TLS pools.
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		// A RoundTripper that is not an *http.Transport owns its own protocol behavior.
		return client
	}
	if transport == nil {
		// A typed-nil *http.Transport still satisfies the interface nil check in
		// NewProxyAwareHTTPClient. Leaving it in place would make http.Client fall back
		// to http.DefaultTransport, which advertises h2 over ALPN and breaks the
		// Antigravity fingerprint, so substitute the process base transport.
		transport = antigravityBaseTransport
	}
	client.Transport = antigravityHTTP11Transport(auth, transport, cfg)
	return client
}

func antigravityProxyURL(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) string {
	if proxyURL := cliproxyexecutor.RequestProxyURL(ctx); proxyURL != "" {
		return proxyURL
	}
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func sanitizeAntigravityGeminiRequestSignatures(modelName string, rawJSON []byte) []byte {
	if !antigravityUsesReasoningReplayCache(modelName) {
		return rawJSON
	}
	rawJSON = internalsignature.SanitizeGeminiRequestThoughtSignatures(rawJSON, "request.contents")
	return normalizeAntigravityGeminiFunctionResponseRoles(rawJSON)
}

// ensureAntigravityGeminiLeadingUserContent prepends a synthetic empty user turn
// after every contents rewrite, including reasoning replay. Claude targets are
// left unchanged because the adapter rejects empty text parts.
func ensureAntigravityGeminiLeadingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	return helps.EnsureGeminiLeadingUserContent(payload, "request.contents")
}

// ensureAntigravityGeminiTrailingUserContent appends a synthetic empty user turn
// if the final turn is a model turn. Claude targets are left unchanged because
// the adapter rejects empty text parts.
func ensureAntigravityGeminiTrailingUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	return helps.EnsureGeminiTrailingUserContent(payload, "request.contents")
}

// ensureAntigravityGeminiBoundaryUserContent normalizes both leading and trailing
// turns for Gemini targets. Claude targets are left unchanged.
func ensureAntigravityGeminiBoundaryUserContent(modelName string, payload []byte) []byte {
	if strings.Contains(strings.ToLower(modelName), "claude") {
		return payload
	}
	return helps.EnsureGeminiBoundaryUserContent(payload, "request.contents")
}

type antigravityContentEdit struct {
	index       int64
	path        string
	start       int
	end         int
	replacement []byte
}

// normalizeAntigravityGeminiFunctionResponseRoles edits each response turn in
// isolation, then splices all changed turns into the request with one body copy.
// Applying SJSON once per field made large histories scale with history size
// multiplied by the number of tool turns.
func normalizeAntigravityGeminiFunctionResponseRoles(rawJSON []byte) []byte {
	rawJSON = repairAntigravityGeminiFunctionResponseNames(rawJSON)
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	type functionRef struct {
		id   string
		name string
	}

	edits := make([]antigravityContentEdit, 0)
	var pending []functionRef
	validOffsets := true
	contents.ForEach(func(contentIndex, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			pending = nil
			return true
		}

		var calls, responses []functionRef
		var responseParts, otherParts []json.RawMessage
		partCount := 0
		hasOtherPart := false
		parts.ForEach(func(_, part gjson.Result) bool {
			partCount++
			switch {
			case part.Get("functionCall").Exists():
				calls = append(calls, functionRef{id: part.Get("functionCall.id").String(), name: part.Get("functionCall.name").String()})
			case part.Get("functionResponse").Exists():
				responses = append(responses, functionRef{id: part.Get("functionResponse.id").String(), name: part.Get("functionResponse.name").String()})
				responseParts = append(responseParts, json.RawMessage(part.Raw))
			default:
				hasOtherPart = true
				otherParts = append(otherParts, json.RawMessage(part.Raw))
			}
			return true
		})
		if partCount == 0 {
			pending = nil
			return true
		}
		if len(calls) > 0 && len(responses) == 0 {
			pending = calls
			return true
		}
		if len(responses) == 0 {
			if hasOtherPart {
				pending = nil
			}
			return true
		}
		if len(calls) > 0 {
			pending = nil
			return true
		}

		var contentJSON []byte
		contentChanged := false
		if len(pending) > 0 && len(responses) > 0 {
			ordered := make([]json.RawMessage, 0, partCount)
			used := make([]bool, len(responses))
			for _, call := range pending {
				for responseIndex, response := range responses {
					if used[responseIndex] {
						continue
					}
					if (call.id != "" && response.id == call.id) || (call.id == "" && call.name != "" && response.name == call.name) {
						used[responseIndex] = true
						ordered = append(ordered, responseParts[responseIndex])
						break
					}
				}
			}
			for responseIndex := range responses {
				if !used[responseIndex] {
					ordered = append(ordered, responseParts[responseIndex])
				}
			}
			if len(ordered) == len(responseParts) {
				ordered = append(ordered, otherParts...)
				encoded, errMarshal := json.Marshal(ordered)
				if errMarshal == nil && !bytes.Equal(encoded, []byte(parts.Raw)) {
					contentJSON = []byte(content.Raw)
					if updated, errSet := sjson.SetRawBytes(contentJSON, "parts", encoded); errSet == nil {
						contentJSON = updated
						contentChanged = true
					}
				}
			}
		}
		pending = nil
		if !hasOtherPart && content.Get("role").String() != "model" {
			if contentJSON == nil {
				contentJSON = []byte(content.Raw)
			}
			if updated, errSet := sjson.SetBytes(contentJSON, "role", "model"); errSet == nil {
				contentJSON = updated
				contentChanged = true
			}
		}
		if !contentChanged {
			return true
		}

		start := content.Index
		end := start + len(content.Raw)
		if start < 0 || end < start || end > len(rawJSON) || !bytes.Equal(rawJSON[start:end], []byte(content.Raw)) {
			validOffsets = false
		}
		edits = append(edits, antigravityContentEdit{
			index:       contentIndex.Int(),
			start:       start,
			end:         end,
			replacement: contentJSON,
		})
		return true
	})
	return applyAntigravityIndexedEdits(rawJSON, edits, validOffsets)
}

// applyAntigravityIndexedEdits splices collected JSON fragments into the original
// request with one body copy. Applying SJSON once per field made large histories
// scale with history size multiplied by the number of edits.
func applyAntigravityIndexedEdits(rawJSON []byte, edits []antigravityContentEdit, validOffsets bool) []byte {
	if len(edits) == 0 {
		return rawJSON
	}
	if !validOffsets {
		return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
	}

	finalSize := len(rawJSON)
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		finalSize += len(edit.replacement) - (edit.end - edit.start)
		if finalSize < 0 {
			return applyAntigravityContentEditsWithSJSON(rawJSON, edits)
		}
		cursor = edit.end
	}
	out := make([]byte, 0, finalSize)
	cursor = 0
	for _, edit := range edits {
		out = append(out, rawJSON[cursor:edit.start]...)
		out = append(out, edit.replacement...)
		cursor = edit.end
	}
	return append(out, rawJSON[cursor:]...)
}

// applyAntigravityContentEditsWithSJSON preserves the legacy path semantics if
// a GJSON result cannot be proven to point into the original request bytes.
func applyAntigravityContentEditsWithSJSON(rawJSON []byte, edits []antigravityContentEdit) []byte {
	out := rawJSON
	for _, edit := range edits {
		path := edit.path
		if path == "" {
			path = fmt.Sprintf("request.contents.%d", edit.index)
		}
		if updated, errSet := sjson.SetRawBytes(out, path, edit.replacement); errSet == nil {
			out = updated
		}
	}
	return out
}

// repairAntigravityGeminiFunctionResponseNames copies missing or placeholder
// functionResponse names from the matching functionCall. Edits are applied to
// each part in isolation, then spliced into the request with one body copy.
func repairAntigravityGeminiFunctionResponseNames(rawJSON []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	callIDToName := make(map[string]string)
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(_, part gjson.Result) bool {
			fc := part.Get("functionCall")
			if fc.Exists() {
				id := strings.TrimSpace(fc.Get("id").String())
				name := strings.TrimSpace(fc.Get("name").String())
				if id != "" && name != "" && name != "unknown" {
					callIDToName[id] = name
				}
			}
			return true
		})
		return true
	})
	if len(callIDToName) == 0 {
		return rawJSON
	}

	edits := make([]antigravityContentEdit, 0)
	validOffsets := true
	contents.ForEach(func(contentIdx, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(partIdx, part gjson.Result) bool {
			fr := part.Get("functionResponse")
			if !fr.Exists() {
				return true
			}
			id := strings.TrimSpace(fr.Get("id").String())
			name := strings.TrimSpace(fr.Get("name").String())
			if id == "" || (name != "" && name != "unknown") {
				return true
			}
			realName, ok := callIDToName[id]
			if !ok {
				return true
			}
			updatedPart, errSet := sjson.SetBytes([]byte(part.Raw), "functionResponse.name", realName)
			if errSet != nil {
				return true
			}
			start := part.Index
			end := start + len(part.Raw)
			if start < 0 || end < start || end > len(rawJSON) || !bytes.Equal(rawJSON[start:end], []byte(part.Raw)) {
				validOffsets = false
			}
			edits = append(edits, antigravityContentEdit{
				index:       contentIdx.Int(),
				path:        fmt.Sprintf("request.contents.%d.parts.%d", contentIdx.Int(), partIdx.Int()),
				start:       start,
				end:         end,
				replacement: updatedPart,
			})
			return true
		})
		return true
	})
	return applyAntigravityIndexedEdits(rawJSON, edits, validOffsets)
}

func validateAntigravityRequestSignatures(ctx context.Context, modelName string, from sdktranslator.Format, rawJSON []byte) ([]byte, error) {
	if from.String() != "claude" {
		return rawJSON, nil
	}
	before := countClaudeThinkingBlocks(rawJSON)
	if antigravityUsesReasoningReplayCache(modelName) {
		rawJSON = antigravityclaude.StripInvalidGeminiSignatureThinkingBlocks(rawJSON)
		logAntigravitySignatureStrip(before, countClaudeThinkingBlocks(rawJSON), "provider_cleanup", "empty_or_non_gemini_signature")
		return rawJSON, nil
	}
	// Claude models accept only Claude-format thinking signatures.
	rawJSON = antigravityclaude.StripEmptySignatureThinkingBlocks(rawJSON)
	logAntigravitySignatureStrip(before, countClaudeThinkingBlocks(rawJSON), "prefix_cleanup", "empty_or_non_claude_signature")
	if cache.SignatureCacheEnabled() {
		return rawJSON, nil
	}
	if !cache.SignatureBypassStrictMode() {
		// Non-strict bypass: let the translator handle invalid signatures
		// by dropping unsigned thinking blocks silently (no 400).
		return rawJSON, nil
	}
	before = countClaudeThinkingBlocks(rawJSON)
	rawJSON = antigravityclaude.StripInvalidBypassSignatureThinkingBlocks(rawJSON)
	logAntigravitySignatureStrip(before, countClaudeThinkingBlocks(rawJSON), "strict_bypass", "invalid_antigravity_claude_signature")
	return rawJSON, nil
}

func hasAntigravityClaudeTypedWebSearchTool(payload []byte) bool {
	tools := util.GetGJSONBytesNoCopy(payload, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "web_search_20250305", "web_search_20260209":
			return true
		}
	}
	return false
}

func hasAntigravityGoogleSearchTool(payload []byte) bool {
	tools := util.GetGJSONBytesNoCopy(payload, "request.tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if tool.Get("googleSearch").Exists() {
			return true
		}
	}
	return false
}

func hasAntigravityResponsesWebSearchTool(rawJSON []byte) bool {
	tools := util.GetGJSONBytesNoCopy(rawJSON, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
			return true
		}
	}
	return false
}

func shouldResolveAntigravityWebSearchGroundingURLs(from sdktranslator.Format, originalRequestRawJSON, requestRawJSON []byte) bool {
	if !hasAntigravityGoogleSearchTool(requestRawJSON) {
		return false
	}
	switch from {
	case sdktranslator.FormatClaude:
		return hasAntigravityClaudeTypedWebSearchTool(originalRequestRawJSON)
	case sdktranslator.FormatOpenAIResponse:
		return hasAntigravityResponsesWebSearchTool(originalRequestRawJSON)
	default:
		return false
	}
}

func (e *AntigravityExecutor) resolveWebSearchGroundingURLs(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, originalRequestRawJSON, requestRawJSON, responseRawJSON []byte) []byte {
	if !shouldResolveAntigravityWebSearchGroundingURLs(from, originalRequestRawJSON, requestRawJSON) {
		return responseRawJSON
	}
	return helps.ResolveAntigravityGroundingURLs(ctx, e.cfg, auth, responseRawJSON)
}

func countClaudeThinkingBlocks(rawJSON []byte) int {
	messages := util.GetGJSONBytesNoCopy(rawJSON, "messages")
	if !messages.IsArray() {
		return 0
	}

	count := 0
	messages.ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "thinking" {
				count++
			}
			return true
		})
		return true
	})
	return count
}

func logAntigravitySignatureStrip(before, after int, stage, reason string) {
	removed := before - after
	if removed <= 0 {
		return
	}
	log.WithFields(log.Fields{
		"component":       "signature_sanitizer",
		"executor":        "antigravity",
		"target_provider": "claude",
		"action":          "drop_thinking_blocks",
		"stage":           stage,
		"reason":          reason,
		"count":           removed,
	}).Debug("antigravity executor: dropped Claude thinking blocks with invalid signatures")
}

// Identifier returns the executor identifier.
func (e *AntigravityExecutor) Identifier() string { return antigravityAuthType }

// PrepareRequest injects Antigravity credentials into the outgoing HTTP request.
func (e *AntigravityExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _, errToken := e.ensureAccessToken(req.Context(), auth)
	if errToken != nil {
		return errToken
	}
	if strings.TrimSpace(token) == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// HttpRequest injects Antigravity credentials into the request and executes it.
// It uses a whitelist approach: all incoming headers are stripped and only
// the minimum set required by the Antigravity protocol is explicitly set.
func (e *AntigravityExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("antigravity executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)

	// Connection management is a Request field, not a header, so the header
	// whitelist below cannot strip it. An inbound "Connection: close" makes Go's
	// server set Request.Close, and WithContext copies that field verbatim, which
	// would both leak the downstream header upstream and drain the shared pool.
	httpReq.Close = false

	// --- Whitelist: save only the headers we need from the original request ---
	contentType := httpReq.Header.Get("Content-Type")

	// Wipe ALL incoming headers
	for k := range httpReq.Header {
		delete(httpReq.Header, k)
	}

	// --- Set only the headers Antigravity actually sends ---
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	// Content-Length is managed automatically by Go's http.Client from the Body
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))

	// Inject Authorization: Bearer <token>
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}
```

## `internal/runtime/executor/antigravity_executor_stream.go`

SHA-256 (LF): `70604838ba654040e93b5db57be143330725649064c9ab4420cd5195711da501`

```go
package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ExecuteStream performs a streaming request to the Antigravity API.
func (e *AntigravityExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if helps.HasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.ExpandAntigravityCompactionCapsules(req.Payload)
		if errExpand != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		req.Payload = expanded
		if len(opts.OriginalRequest) > 0 {
			expandedOrig, errOrig := helps.ExpandAntigravityCompactionCapsules(opts.OriginalRequest)
			if errOrig == nil {
				opts.OriginalRequest = expandedOrig
			} else {
				opts.OriginalRequest = expanded
			}
		}
	}
	if helps.HasResponsesCompactionTrigger(req.Payload) || helps.HasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompactionStream(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	ctx = context.WithValue(ctx, "alt", "")
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining, errCooldown := antigravityIsInShortCooldownRequired(ctx, auth, baseModel, time.Now()); errCooldown != nil {
			return nil, homeKVUnavailableStatusErr(errCooldown)
		} else if inCooldown && !antigravityShouldBypassShortCooldown(ctx, e.cfg) {
			log.Debugf("antigravity executor: auth %s in short cooldown for model %s (%s remaining), returning 429 to switch auth", auth.ID, baseModel, remaining)
			d := remaining
			return nil, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
		}
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(ctx, baseModel, from, originalPayload)
	if errValidate != nil {
		return nil, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
		reporter.UpdateAccessTokenFingerprint(auth)
	}

	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Stream: true, ModelInfo: modelInfo}
	originalTranslated, translated := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "antigravity", from.String(), "request", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	translated = e.obfuscateSensitiveWords(translated)
	translated = sanitizeAntigravityGeminiRequestSignatures(baseModel, translated)
	translated, _ = sjson.DeleteBytes(translated, "request.stream")
	reporter.SetTranslatedReasoningEffort(translated, to.String())

	useCredits := antigravityShouldUseCredits(ctx, e.cfg, baseModel)

	baseURL := resolveAntigravityRequestBaseURL(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)

	// Credential retry rounds are owned by the conductor. Perform one upstream
	// request per credential so request-retry is not consumed twice.
	requestPayload := translated
	if useCredits {
		if cp := injectEnabledCreditTypes(translated); len(cp) > 0 {
			requestPayload = cp
			helps.MarkCreditsUsed(ctx)
		}
	}
	replayScope := antigravityReasoningReplayScope{}
	if antigravityUsesReasoningReplayCache(baseModel) {
		var errReplay error
		requestPayload, replayScope, errReplay = prepareAntigravityGeminiReasoningReplayPayload(ctx, baseModel, req, opts, requestPayload)
		if errReplay != nil {
			err = errReplay
			return nil, err
		}
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)
	httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, requestPayload, true, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata))
	if errReq != nil {
		err = errReq
		return nil, err
	}
	diagnostics.ObserveNormalized(ctx, originalPayloadSource, requestPayload)
	diag := diagnostics.NewExchange(ctx, responseFormat.String(), true, true)
	defer func() {
		if err != nil {
			diag.Finish(err)
		}
	}()
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
			return nil, errDo
		}
		err = errDo
		return nil, err
	}
	diag.Status(httpResp.StatusCode)
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		diag.Upstream(bodyBytes)
		diag.ReadFinished(errRead)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
				err = errRead
				return nil, err
			}
			if errCtx := ctx.Err(); errCtx != nil {
				err = errCtx
				return nil, err
			}
			err = errRead
			return nil, err
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
		if httpResp.StatusCode == http.StatusTooManyRequests {
			decision := decideAntigravity429(bodyBytes)

			switch decision.kind {
			case antigravity429DecisionShortCooldownSwitchAuth:
				closeAntigravityAuthIdleTransports(auth)
				if decision.retryAfter != nil && *decision.retryAfter > 0 && !antigravityCoolingDisabled(auth, e.cfg) {
					if errMarkCooldown := markAntigravityShortCooldownRequired(ctx, auth, baseModel, time.Now(), *decision.retryAfter); errMarkCooldown != nil {
						err = homeKVUnavailableStatusErr(errMarkCooldown)
						return nil, err
					}
					log.Debugf("antigravity executor: short quota cooldown (%s) for model %s recorded", *decision.retryAfter, baseModel)
				}
			case antigravity429DecisionFullQuotaExhausted:
				closeAntigravityAuthIdleTransports(auth)
				if useCredits && antigravityHasExplicitCreditsBalanceExhaustedReason(bodyBytes) && !antigravityCoolingDisabled(auth, e.cfg) {
					markAntigravityCreditsPermanentlyDisabled(auth)
				}
				// No credits logic - just fall through to error return below
			}
		}

		if errClear := clearAntigravityReasoningReplayOnInvalidSignature(ctx, replayScope, httpResp.StatusCode, bodyBytes); errClear != nil {
			// Report the upstream failure rather than the cleanup failure.
			logAntigravityReasoningReplayDegraded(replayScope, "invalidate", errClear)
		}
		err = newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
		return nil, err
	}

	// Stream success
	if useCredits {
		clearAntigravityCreditsFailureState(auth)
	}
	replayAccumulator := newAntigravityReasoningReplayAccumulator(replayScope, requestPayload)
	out := make(chan cliproxyexecutor.StreamChunk)
	go func(resp *http.Response) {
		defer close(out)
		defer func() { diag.Finish(nil) }()
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response line error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			if raw := helps.JSONPayload(line); len(raw) > 0 {
				diag.Upstream(raw)
			}
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if replayAccumulator != nil {
				replayAccumulator.ObserveSSELine(line)
			}

			// Filter usage metadata for all models
			// Only retain usage statistics in the terminal chunk
			line = helps.FilterSSEUsageMetadata(line)

			payload := helps.JSONPayload(line)
			if payload == nil {
				continue
			}
			reporter.ObserveResponseModel(payload)

			if detail, ok := helps.ParseAntigravityStreamUsage(payload); ok {
				reporter.Publish(ctx, detail)
			}

			payload = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, payload)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, bytes.Clone(payload), &param, claudeInputTokens)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: helps.RewriteSSEModelVersion(chunks[i], requestedModel, baseModel)}:
					diag.Delivered(chunks[i])
				case <-ctx.Done():
					return
				}
			}
		}
		diag.ReadFinished(scanner.Err())
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			// Settle live upstream failures before Err lets the handler seal the
			// span. Cancellation retains the existing interrupted cleanup path.
			if ctx.Err() == nil {
				diag.Finish(errScan)
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		} else {
			// Only a clean end of stream may produce a synthetic terminal event.
			// Translating [DONE] after a read error would report a truncated
			// stream as a successful completion.
			tail := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, []byte("[DONE]"), &param, claudeInputTokens)
			for i := range tail {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: helps.RewriteSSEModelVersion(tail[i], requestedModel, baseModel)}:
					diag.Delivered(tail[i])
				case <-ctx.Done():
					return
				}
			}
			if replayAccumulator != nil {
				replayAccumulator.Commit(ctx)
			}
			reporter.EnsurePublished(ctx)
		}
	}(httpResp)
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *AntigravityExecutor) executeCompactionStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(payload, baseModel)

	summaryReq := cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  summaryPayload,
		Metadata: req.Metadata,
	}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatOpenAIResponse

	summaryResp, errSummary := e.Execute(ctx, auth, summaryReq, summaryOpts)
	if errSummary != nil {
		return nil, errSummary
	}

	summaryText, errExtract := helps.ExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return nil, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.SealAntigravityCompaction(summaryText, baseModel)
	if errSeal != nil {
		return nil, fmt.Errorf("seal compaction capsule: %w", errSeal)
	}

	inputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.input_tokens").Int())
	outputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.output_tokens").Int())
	totalTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.total_tokens").Int())
	if totalTokens == 0 && inputTokens == 0 {
		usage := helps.ParseOpenAIUsage(summaryResp.Payload)
		inputTokens = int(usage.InputTokens)
		outputTokens = int(usage.OutputTokens)
		totalTokens = int(usage.TotalTokens)
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	chunks := helps.BuildAntigravityCompactionStreamChunks(requestedModel, capsule, inputTokens, outputTokens, totalTokens)
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)

	headers := summaryResp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")

	return &cliproxyexecutor.StreamResult{
		Headers: headers,
		Chunks:  out,
	}, nil
}
```

## `internal/runtime/executor/antigravity_executor_execute.go`

SHA-256 (LF): `a7fa7c1b0e94ae2ccfb5141c734e62d3755e3a6d9a92989301e04462a7dcde1f`

```go
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Execute performs a non-streaming request to the Antigravity API.
func (e *AntigravityExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if helps.HasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.ExpandAntigravityCompactionCapsules(req.Payload)
		if errExpand != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		req.Payload = expanded
		if len(opts.OriginalRequest) > 0 {
			expandedOrig, errOrig := helps.ExpandAntigravityCompactionCapsules(opts.OriginalRequest)
			if errOrig == nil {
				opts.OriginalRequest = expandedOrig
			} else {
				opts.OriginalRequest = expanded
			}
		}
	}
	if opts.Alt == "responses/compact" || helps.HasResponsesCompactionTrigger(req.Payload) || helps.HasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompaction(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining, errCooldown := antigravityIsInShortCooldownRequired(ctx, auth, baseModel, time.Now()); errCooldown != nil {
			return resp, homeKVUnavailableStatusErr(errCooldown)
		} else if inCooldown && !antigravityShouldBypassShortCooldown(ctx, e.cfg) {
			log.Debugf("antigravity executor: auth %s in short cooldown for model %s (%s remaining), returning 429 to switch auth", auth.ID, baseModel, remaining)
			d := remaining
			return resp, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
		}
	}

	isClaude := strings.Contains(strings.ToLower(baseModel), "claude")
	if isClaude || strings.Contains(baseModel, "gemini-3-pro") || strings.Contains(baseModel, "gemini-3.1-flash-image") {
		return e.executeClaudeNonStream(ctx, auth, req, opts)
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(ctx, baseModel, from, originalPayload)
	if errValidate != nil {
		return resp, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
		reporter.UpdateAccessTokenFingerprint(auth)
	}
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, ModelInfo: modelInfo}
	originalTranslated, translated := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "antigravity", from.String(), "request", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	translated = e.obfuscateSensitiveWords(translated)
	translated = sanitizeAntigravityGeminiRequestSignatures(baseModel, translated)
	reporter.SetTranslatedReasoningEffort(translated, to.String())

	useCredits := antigravityShouldUseCredits(ctx, e.cfg, baseModel)

	baseURL := resolveAntigravityRequestBaseURL(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	// Credential retry rounds are owned by the conductor. Perform one upstream
	// request per credential so request-retry is not consumed twice.
	requestPayload := translated
	if useCredits {
		if cp := injectEnabledCreditTypes(translated); len(cp) > 0 {
			requestPayload = cp
			helps.MarkCreditsUsed(ctx)
		}
	}
	replayScope := antigravityReasoningReplayScope{}
	if antigravityUsesReasoningReplayCache(baseModel) {
		var errReplay error
		requestPayload, replayScope, errReplay = prepareAntigravityGeminiReasoningReplayPayload(ctx, baseModel, req, opts, requestPayload)
		if errReplay != nil {
			err = errReplay
			return resp, err
		}
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)

	httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, requestPayload, false, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata))
	if errReq != nil {
		err = errReq
		return resp, err
	}

	diagnostics.ObserveNormalized(ctx, originalPayloadSource, requestPayload)
	diag := diagnostics.NewExchange(ctx, responseFormat.String(), false, false)
	defer func() { diag.Finish(err) }()
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
			return resp, errDo
		}
		err = errDo
		return resp, err
	}

	diag.Status(httpResp.StatusCode)
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	diag.Upstream(bodyBytes)
	diag.ReadFinished(errRead)
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("antigravity executor: close response body error: %v", errClose)
	}
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		err = errRead
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)

	if httpResp.StatusCode == http.StatusTooManyRequests {
		decision := decideAntigravity429(bodyBytes)
		switch decision.kind {
		case antigravity429DecisionShortCooldownSwitchAuth:
			closeAntigravityAuthIdleTransports(auth)
			if decision.retryAfter != nil && *decision.retryAfter > 0 && !antigravityCoolingDisabled(auth, e.cfg) {
				if errMarkCooldown := markAntigravityShortCooldownRequired(ctx, auth, baseModel, time.Now(), *decision.retryAfter); errMarkCooldown != nil {
					err = homeKVUnavailableStatusErr(errMarkCooldown)
					return resp, err
				}
				log.Debugf("antigravity executor: short quota cooldown (%s) for model %s, recorded cooldown", *decision.retryAfter, baseModel)
			}
		case antigravity429DecisionFullQuotaExhausted:
			closeAntigravityAuthIdleTransports(auth)
			if useCredits && antigravityHasExplicitCreditsBalanceExhaustedReason(bodyBytes) && !antigravityCoolingDisabled(auth, e.cfg) {
				markAntigravityCreditsPermanentlyDisabled(auth)
			}
			// No credits logic - just fall through to error return below
		}
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("antigravity executor: upstream error status: %d, body: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), bodyBytes))
		if errClear := clearAntigravityReasoningReplayOnInvalidSignature(ctx, replayScope, httpResp.StatusCode, bodyBytes); errClear != nil {
			// Report the upstream failure rather than the cleanup failure.
			logAntigravityReasoningReplayDegraded(replayScope, "invalidate", errClear)
		}
		err = newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
		return resp, err
	}

	// Success
	if useCredits {
		clearAntigravityCreditsFailureState(auth)
	}
	cacheAntigravityReasoningReplayFromResponse(ctx, replayScope, requestPayload, bodyBytes)
	bodyBytes = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, bodyBytes)
	reporter.ObserveResponseModel(bodyBytes)
	reporter.Publish(ctx, helps.ParseAntigravityUsage(bodyBytes))
	var param any
	converted := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, bodyBytes, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		converted = helps.EnsureResponsesUsageDetails(converted)
	}
	resp = cliproxyexecutor.Response{Payload: helps.RewriteResponseModelVersion(converted, requestedModel, baseModel), Headers: httpResp.Header.Clone()}
	diag.Delivered(resp.Payload)
	reporter.EnsurePublished(ctx)
	return resp, nil
}

func (e *AntigravityExecutor) executeCompaction(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(payload, baseModel)

	summaryReq := cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  summaryPayload,
		Metadata: req.Metadata,
	}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatOpenAIResponse

	summaryResp, errSummary := e.Execute(ctx, auth, summaryReq, summaryOpts)
	if errSummary != nil {
		return resp, errSummary
	}

	summaryText, errExtract := helps.ExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return resp, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.SealAntigravityCompaction(summaryText, baseModel)
	if errSeal != nil {
		return resp, fmt.Errorf("seal compaction capsule: %w", errSeal)
	}

	inputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.input_tokens").Int())
	outputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.output_tokens").Int())
	totalTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.total_tokens").Int())
	if totalTokens == 0 && inputTokens == 0 {
		usage := helps.ParseOpenAIUsage(summaryResp.Payload)
		inputTokens = int(usage.InputTokens)
		outputTokens = int(usage.OutputTokens)
		totalTokens = int(usage.TotalTokens)
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	respBytes := helps.BuildAntigravityCompactionResponse(requestedModel, capsule, inputTokens, outputTokens, totalTokens)
	return cliproxyexecutor.Response{
		Payload: respBytes,
		Headers: summaryResp.Headers,
	}, nil
}

// executeClaudeNonStream performs a claude non-streaming request to the Antigravity API.
func (e *AntigravityExecutor) executeClaudeNonStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining, errCooldown := antigravityIsInShortCooldownRequired(ctx, auth, baseModel, time.Now()); errCooldown != nil {
			return resp, homeKVUnavailableStatusErr(errCooldown)
		} else if inCooldown && !antigravityShouldBypassShortCooldown(ctx, e.cfg) {
			log.Debugf("antigravity executor: auth %s in short cooldown for model %s (%s remaining), returning 429 to switch auth", auth.ID, baseModel, remaining)
			d := remaining
			return resp, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
		}
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(ctx, baseModel, from, originalPayload)
	if errValidate != nil {
		return resp, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
		reporter.UpdateAccessTokenFingerprint(auth)
	}
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Stream: true, ModelInfo: modelInfo}
	originalTranslated, translated := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)

	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "antigravity", from.String(), "request", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	translated = e.obfuscateSensitiveWords(translated)
	translated = sanitizeAntigravityGeminiRequestSignatures(baseModel, translated)
	reporter.SetTranslatedReasoningEffort(translated, to.String())

	useCredits := antigravityShouldUseCredits(ctx, e.cfg, baseModel)

	baseURL := resolveAntigravityRequestBaseURL(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)

	// Credential retry rounds are owned by the conductor. Perform one upstream
	// request per credential so request-retry is not consumed twice.
	requestPayload := translated
	if useCredits {
		if cp := injectEnabledCreditTypes(translated); len(cp) > 0 {
			requestPayload = cp
			helps.MarkCreditsUsed(ctx)
		}
	}
	replayScope := antigravityReasoningReplayScope{}
	if antigravityUsesReasoningReplayCache(baseModel) {
		var errReplay error
		requestPayload, replayScope, errReplay = prepareAntigravityGeminiReasoningReplayPayload(ctx, baseModel, req, opts, requestPayload)
		if errReplay != nil {
			err = errReplay
			return resp, err
		}
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)
	httpReq, errReq := e.buildRequest(ctx, auth, token, baseModel, requestPayload, true, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata))
	if errReq != nil {
		err = errReq
		return resp, err
	}

	diagnostics.ObserveNormalized(ctx, originalPayloadSource, requestPayload)
	diag := diagnostics.NewExchange(ctx, responseFormat.String(), true, false)
	defer func() { diag.Finish(err) }()
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
			return resp, errDo
		}
		err = errDo
		return resp, err
	}
	diag.Status(httpResp.StatusCode)
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		diag.Upstream(bodyBytes)
		diag.ReadFinished(errRead)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
				err = errRead
				return resp, err
			}
			if errCtx := ctx.Err(); errCtx != nil {
				err = errCtx
				return resp, err
			}
			err = errRead
			return resp, err
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
		if httpResp.StatusCode == http.StatusTooManyRequests {
			decision := decideAntigravity429(bodyBytes)

			switch decision.kind {
			case antigravity429DecisionShortCooldownSwitchAuth:
				closeAntigravityAuthIdleTransports(auth)
				if decision.retryAfter != nil && *decision.retryAfter > 0 && !antigravityCoolingDisabled(auth, e.cfg) {
					if errMarkCooldown := markAntigravityShortCooldownRequired(ctx, auth, baseModel, time.Now(), *decision.retryAfter); errMarkCooldown != nil {
						err = homeKVUnavailableStatusErr(errMarkCooldown)
						return resp, err
					}
					log.Debugf("antigravity executor: short quota cooldown (%s) for model %s, recorded cooldown", *decision.retryAfter, baseModel)
				}
			case antigravity429DecisionFullQuotaExhausted:
				closeAntigravityAuthIdleTransports(auth)
				if useCredits && antigravityHasExplicitCreditsBalanceExhaustedReason(bodyBytes) && !antigravityCoolingDisabled(auth, e.cfg) {
					markAntigravityCreditsPermanentlyDisabled(auth)
				}
				// No credits logic - just fall through to error return below
			}
		}

		if errClear := clearAntigravityReasoningReplayOnInvalidSignature(ctx, replayScope, httpResp.StatusCode, bodyBytes); errClear != nil {
			// Report the upstream failure rather than the cleanup failure.
			logAntigravityReasoningReplayDegraded(replayScope, "invalidate", errClear)
		}
		err = newAntigravityStatusErr(httpResp.StatusCode, bodyBytes)
		return resp, err
	}

	// Stream success
	if useCredits {
		clearAntigravityCreditsFailureState(auth)
	}
	replayAccumulator := newAntigravityReasoningReplayAccumulator(replayScope, requestPayload)
	out := make(chan cliproxyexecutor.StreamChunk)
	go func(resp *http.Response) {
		defer close(out)
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		for scanner.Scan() {
			line := scanner.Bytes()
			if raw := helps.JSONPayload(line); len(raw) > 0 {
				diag.Upstream(raw)
			}
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if replayAccumulator != nil {
				replayAccumulator.ObserveSSELine(line)
			}

			// Filter usage metadata for all models
			// Only retain usage statistics in the terminal chunk
			line = helps.FilterSSEUsageMetadata(line)

			payload := helps.JSONPayload(line)
			if payload == nil {
				continue
			}
			reporter.ObserveResponseModel(payload)

			if detail, ok := helps.ParseAntigravityStreamUsage(payload); ok {
				reporter.Publish(ctx, detail)
			}

			out <- cliproxyexecutor.StreamChunk{Payload: payload}
		}
		diag.ReadFinished(scanner.Err())
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else {
			if replayAccumulator != nil {
				replayAccumulator.Commit(ctx)
			}
			reporter.EnsurePublished(ctx)
		}
	}(httpResp)

	var buffer bytes.Buffer
	for chunk := range out {
		if chunk.Err != nil {
			return resp, chunk.Err
		}
		if len(chunk.Payload) > 0 {
			_, _ = buffer.Write(chunk.Payload)
			_, _ = buffer.Write([]byte("\n"))
		}
	}
	resp = cliproxyexecutor.Response{Payload: e.convertStreamToNonStream(buffer.Bytes())}

	resp.Payload = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, resp.Payload)
	reporter.ObserveResponseModel(resp.Payload)
	reporter.Publish(ctx, helps.ParseAntigravityUsage(resp.Payload))
	var param any
	converted := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, resp.Payload, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		converted = helps.EnsureResponsesUsageDetails(converted)
	}
	resp = cliproxyexecutor.Response{Payload: helps.RewriteResponseModelVersion(converted, requestedModel, baseModel), Headers: httpResp.Header.Clone()}
	diag.Delivered(resp.Payload)
	reporter.EnsurePublished(ctx)

	return resp, nil
}

func (e *AntigravityExecutor) convertStreamToNonStream(stream []byte) []byte {
	responseTemplate := ""
	var traceID string
	var finishReason string
	var modelVersion string
	var responseID string
	var role string
	var usageRaw string
	parts := make([]map[string]interface{}, 0)
	var pendingKind string
	var pendingText strings.Builder
	var pendingThoughtSig string

	flushPending := func() {
		if pendingKind == "" {
			return
		}
		text := pendingText.String()
		switch pendingKind {
		case "text":
			if strings.TrimSpace(text) == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			parts = append(parts, map[string]interface{}{"text": text})
		case "thought":
			if strings.TrimSpace(text) == "" && pendingThoughtSig == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			part := map[string]interface{}{"thought": true}
			part["text"] = text
			if pendingThoughtSig != "" {
				part["thoughtSignature"] = pendingThoughtSig
			}
			parts = append(parts, part)
		}
		pendingKind = ""
		pendingText.Reset()
		pendingThoughtSig = ""
	}

	normalizePart := func(partResult gjson.Result) map[string]interface{} {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(partResult.Raw), &m)
		if m == nil {
			m = map[string]interface{}{}
		}
		sig := partResult.Get("thoughtSignature").String()
		if sig == "" {
			sig = partResult.Get("thought_signature").String()
		}
		if sig != "" {
			m["thoughtSignature"] = sig
			delete(m, "thought_signature")
		}
		if inlineData, ok := m["inline_data"]; ok {
			m["inlineData"] = inlineData
			delete(m, "inline_data")
		}
		return m
	}

	for _, line := range bytes.Split(stream, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !gjson.ValidBytes(trimmed) {
			continue
		}

		root := gjson.ParseBytes(trimmed)
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			if root.Get("candidates").Exists() {
				responseNode = root
			} else {
				continue
			}
		}
		responseTemplate = responseNode.Raw

		if traceResult := root.Get("traceId"); traceResult.Exists() && traceResult.String() != "" {
			traceID = traceResult.String()
		}

		if roleResult := responseNode.Get("candidates.0.content.role"); roleResult.Exists() {
			role = roleResult.String()
		}

		if finishResult := responseNode.Get("candidates.0.finishReason"); finishResult.Exists() && finishResult.String() != "" {
			finishReason = finishResult.String()
		}

		if modelResult := responseNode.Get("modelVersion"); modelResult.Exists() && modelResult.String() != "" {
			modelVersion = modelResult.String()
		}
		if responseIDResult := responseNode.Get("responseId"); responseIDResult.Exists() && responseIDResult.String() != "" {
			responseID = responseIDResult.String()
		}
		if usageResult := responseNode.Get("usageMetadata"); usageResult.Exists() {
			usageRaw = usageResult.Raw
		} else if usageMetadataResult := root.Get("usageMetadata"); usageMetadataResult.Exists() {
			usageRaw = usageMetadataResult.Raw
		}

		if partsResult := responseNode.Get("candidates.0.content.parts"); partsResult.IsArray() {
			for _, part := range partsResult.Array() {
				hasFunctionCall := part.Get("functionCall").Exists()
				hasInlineData := part.Get("inlineData").Exists() || part.Get("inline_data").Exists()
				sig := part.Get("thoughtSignature").String()
				if sig == "" {
					sig = part.Get("thought_signature").String()
				}
				text := part.Get("text").String()
				thought := part.Get("thought").Bool()

				if hasFunctionCall || hasInlineData {
					flushPending()
					parts = append(parts, normalizePart(part))
					continue
				}

				if thought || part.Get("text").Exists() {
					kind := "text"
					if thought {
						kind = "thought"
					}
					if pendingKind != "" && pendingKind != kind {
						flushPending()
					}
					pendingKind = kind
					pendingText.WriteString(text)
					if kind == "thought" && sig != "" {
						pendingThoughtSig = sig
					}
					continue
				}

				flushPending()
				parts = append(parts, normalizePart(part))
			}
		}
	}
	flushPending()

	if responseTemplate == "" {
		responseTemplate = `{"candidates":[{"content":{"role":"model","parts":[]}}]}`
	}

	partsJSON, _ := json.Marshal(parts)
	updatedTemplate, _ := sjson.SetRawBytes([]byte(responseTemplate), "candidates.0.content.parts", partsJSON)
	responseTemplate = string(updatedTemplate)
	if role != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "candidates.0.content.role", role)
		responseTemplate = string(updatedTemplate)
	}
	if finishReason != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "candidates.0.finishReason", finishReason)
		responseTemplate = string(updatedTemplate)
	}
	if modelVersion != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "modelVersion", modelVersion)
		responseTemplate = string(updatedTemplate)
	}
	if responseID != "" {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "responseId", responseID)
		responseTemplate = string(updatedTemplate)
	}
	if usageRaw != "" {
		updatedTemplate, _ = sjson.SetRawBytes([]byte(responseTemplate), "usageMetadata", []byte(usageRaw))
		responseTemplate = string(updatedTemplate)
	} else if !gjson.Get(responseTemplate, "usageMetadata").Exists() {
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "usageMetadata.promptTokenCount", 0)
		responseTemplate = string(updatedTemplate)
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "usageMetadata.candidatesTokenCount", 0)
		responseTemplate = string(updatedTemplate)
		updatedTemplate, _ = sjson.SetBytes([]byte(responseTemplate), "usageMetadata.totalTokenCount", 0)
		responseTemplate = string(updatedTemplate)
	}

	output := `{"response":{},"traceId":""}`
	updatedOutput, _ := sjson.SetRawBytes([]byte(output), "response", []byte(responseTemplate))
	output = string(updatedOutput)
	if traceID != "" {
		updatedOutput, _ = sjson.SetBytes([]byte(output), "traceId", traceID)
		output = string(updatedOutput)
	}
	return []byte(output)
}
```

## `sdk/api/handlers/stream_forwarder.go`

SHA-256 (LF): `4a8cb10b500471643a7507c0210c12700436fe5cd684253ee390e639f242aecb`

```go
package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

// PendingStreamError returns an immediately available non-nil stream error.
func PendingStreamError(errs <-chan *interfaces.ErrorMessage) (*interfaces.ErrorMessage, bool) {
	if errs == nil {
		return nil, false
	}
	select {
	case errMsg, ok := <-errs:
		if ok && errMsg != nil {
			return errMsg, true
		}
	default:
	}
	return nil, false
}

type StreamForwardOptions struct {
	// KeepAliveInterval overrides the configured streaming keep-alive interval.
	// If nil, the configured default is used. If set to <= 0, keep-alives are disabled.
	KeepAliveInterval *time.Duration

	// WriteChunk writes a single data chunk to the response body. It should not flush.
	WriteChunk func(chunk []byte)

	// ChunkError optionally reports that WriteChunk emitted a terminal failure.
	// The failure is passed to cancel without writing another terminal payload.
	ChunkError func() *interfaces.ErrorMessage

	// NormalizeTerminalError optionally replaces an upstream error before it is
	// written or passed to cancel.
	NormalizeTerminalError func(errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage

	// WriteTerminalError writes an error payload to the response body when streaming fails
	// after headers have already been committed. It should not flush.
	WriteTerminalError func(errMsg *interfaces.ErrorMessage)

	// CloseError optionally validates a clean upstream channel close before WriteDone.
	// Returning an error surfaces it through WriteTerminalError instead of completing the stream.
	CloseError func() *interfaces.ErrorMessage

	// WriteDone optionally writes a terminal marker when the upstream data channel closes
	// without an error (e.g. OpenAI's `[DONE]`). It should not flush.
	WriteDone func()

	// WriteKeepAlive optionally writes a keep-alive heartbeat. It should not flush.
	// When nil, a standard SSE comment heartbeat is used.
	WriteKeepAlive func()

	// ThrottleDelay is called before each chunk is written (after the first).
	// It may sleep to enforce a target token emission rate.
	// When nil, no throttling is applied.
	ThrottleDelay func(chunk []byte)
}

func (h *BaseAPIHandler) ForwardStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, opts StreamForwardOptions) {
	if c == nil {
		return
	}
	if cancel == nil {
		return
	}

	writeChunk := opts.WriteChunk
	if writeChunk == nil {
		writeChunk = func([]byte) {}
	}

	writeKeepAlive := opts.WriteKeepAlive
	if writeKeepAlive == nil {
		writeKeepAlive = func() {
			_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
		}
	}

	keepAliveInterval := StreamingKeepAliveInterval(h.Cfg)
	if opts.KeepAliveInterval != nil {
		keepAliveInterval = *opts.KeepAliveInterval
	}
	var keepAlive *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAlive = time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}

	var terminalErr *interfaces.ErrorMessage
	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case chunk, ok := <-data:
			if !ok {
				// Prefer surfacing a terminal error if one is pending.
				if terminalErr == nil {
					if errMsg, ok := PendingStreamError(errs); ok {
						terminalErr = errMsg
						if opts.NormalizeTerminalError != nil {
							terminalErr = opts.NormalizeTerminalError(terminalErr)
						}
					}
				}
				if terminalErr == nil && opts.CloseError != nil {
					terminalErr = opts.CloseError()
				}
				if terminalErr != nil {
					if opts.WriteTerminalError != nil {
						opts.WriteTerminalError(terminalErr)
					}
					flusher.Flush()
					cancel(terminalErr.Error)
					return
				}
				if opts.WriteDone != nil {
					opts.WriteDone()
				}
				flusher.Flush()
				cancel(nil)
				return
			}
			if opts.ThrottleDelay != nil {
				opts.ThrottleDelay(chunk)
			}
			writeChunk(chunk)
			flusher.Flush()
			if opts.ChunkError != nil {
				chunkErr := opts.ChunkError()
				if chunkErr != nil {
					if opts.NormalizeTerminalError != nil {
						chunkErr = opts.NormalizeTerminalError(chunkErr)
					}
					if chunkErr != nil {
						cancel(chunkErr.Error)
					} else {
						cancel(nil)
					}
					return
				}
			}
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				terminalErr = errMsg
				if opts.NormalizeTerminalError != nil {
					terminalErr = opts.NormalizeTerminalError(terminalErr)
				}
				if opts.WriteTerminalError != nil {
					opts.WriteTerminalError(terminalErr)
					flusher.Flush()
				}
			}
			var execErr error
			if terminalErr != nil {
				execErr = terminalErr.Error
			}
			cancel(execErr)
			return
		case <-keepAliveC:
			writeKeepAlive()
			flusher.Flush()
		}
	}
}
```

## `sdk/api/handlers/handlers_stream.go`

SHA-256 (LF): `1560838371646e9b4442ab0c2127f54d25daf41a76aee6e37f61538c13babe6f`

```go
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"golang.org/x/net/context"
)

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
// The returned http.Header carries upstream response headers captured before streaming begins.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, false)
}

// ExecuteImageStreamWithAuthManager executes a streaming OpenAI-compatible image endpoint request.
func (h *BaseAPIHandler) ExecuteImageStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManager(ctx, handlerType, modelName, rawJSON, alt, true)
}

func (h *BaseAPIHandler) streamWithPluginExecutor(ctx context.Context, entryProtocol, responseProtocol, modelName, originalRequestedModel string, rawJSON []byte, alt, executorPluginID string, execOptions modelExecutionOptions) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: fmt.Errorf("plugin executor routing is unavailable while Home is enabled")}
		close(errChan)
		return nil, nil, errChan
	}
	host := h.pluginExecutorHost()
	if host == nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor host is unavailable")}
		close(errChan)
		return nil, nil, errChan
	}
	execCtx, nestedTracker := withNestedExecutionTracker(coreusage.WithStream(ctx, true))
	req, opts := h.pluginExecutorRequest(execCtx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, true, execOptions)
	lifecycle := h.newRequestLifecycleTracker(execCtx, entryProtocol, modelName, originalRequestedModel, true, opts.Metadata, execOptions.SkipInterceptorPluginID)
	var interceptErr *interfaces.ErrorMessage
	req, opts, interceptErr = h.applyRequestInterceptorsBeforeAuth(execCtx, entryProtocol, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		lifecycle.completeError(execCtx, interceptErr)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- interceptErr
		close(errChan)
		return nil, nil, errChan
	}
	req, opts, interceptErr = h.applyRequestInterceptorsAfterPluginExecutorRoute(execCtx, host, executorPluginID, entryProtocol, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		lifecycle.completeError(execCtx, interceptErr)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- interceptErr
		close(errChan)
		return nil, nil, errChan
	}
	execCtx = enrichContextWithSessionHierarchy(execCtx, opts.Headers, req.Payload, opts.Metadata)
	var reporter *helps.UsageReporter
	if !execOptions.InternalSource {
		reporter = helps.NewUsageReporter(execCtx, executorPluginID, modelName, nil)
		reporter.SetTranslatedReasoningEffort(req.Payload, entryProtocol)
	}
	streamResult, errStream := host.ExecutePluginExecutorStream(execCtx, executorPluginID, req, opts)
	if errStream != nil {
		if reporter != nil && !nestedTracker.hasNestedExecution() {
			reporter.PublishFailure(execCtx, errStream)
		}
		errMsg := executionErrorMessage(errStream)
		lifecycle.completeError(execCtx, errMsg)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	if streamResult == nil {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("plugin executor returned nil stream")}
		if reporter != nil && !nestedTracker.hasNestedExecution() {
			reporter.PublishFailure(execCtx, errMsg.Error)
		}
		lifecycle.completeError(execCtx, errMsg)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}

	passthroughHeadersEnabled := executionPassthroughHeaders(h.Cfg, execOptions.InternalSource)
	interceptorHost := h.interceptorHost()
	streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)
	rawStreamHeaders := cloneHeader(streamResult.Headers)
	baseStreamHeaders := cloneHeader(streamResult.Headers)
	// Request headers and request bodies are stream-invariant. Keep a private snapshot
	// and clone into each interceptor call so plugins cannot mutate shared storage.
	// Schema v3+ payload chunks omit these bodies (host also strips per plugin).
	var streamRequestHeaders http.Header
	var streamOriginalRequest []byte
	var streamRequestBody []byte
	applyStreamHeaders := func(headers http.Header) {
		rawStreamHeaders = finalInterceptorHeaders(rawStreamHeaders, headers)
	}
	if streamInterceptorsActive {
		streamRequestHeaders = cloneHeader(opts.Headers)
		streamOriginalRequest = cloneBytes(opts.OriginalRequest)
		streamRequestBody = cloneBytes(req.Payload)
		intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
			RequestID:       lifecycle.requestID(),
			SourceFormat:    responseProtocol,
			Model:           modelName,
			RequestedModel:  originalRequestedModel,
			RequestHeaders:  cloneHeader(streamRequestHeaders),
			ResponseHeaders: cloneHeader(rawStreamHeaders),
			OriginalRequest: cloneBytes(streamOriginalRequest),
			RequestBody:     cloneBytes(streamRequestBody),
			ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
			Metadata:        opts.Metadata,
		}, execOptions.SkipInterceptorPluginID)
		applyStreamHeaders(intercepted.Headers)
	}
	upstreamHeaders := downstreamHeadersAfterInterceptors(baseStreamHeaders, rawStreamHeaders, passthroughHeadersEnabled)
	if upstreamHeaders == nil && (passthroughHeadersEnabled || streamInterceptorsActive) {
		upstreamHeaders = make(http.Header)
	}

	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	chunks := streamResult.Chunks
	if chunks == nil {
		closed := make(chan coreexecutor.StreamChunk)
		close(closed)
		chunks = closed
	}
	var responseSSEValidator *sseJSONValidationState
	if responseProtocol == "openai-response" {
		responseSSEValidator = &sseJSONValidationState{}
	}
	go func() {
		completionOutcome := pluginapi.RequestCompletionSucceeded
		completionStatus := http.StatusOK
		var completionErr error
		var streamUsage helps.StreamUsageBuffer
		defer func() {
			lifecycle.complete(completionOutcome, completionStatus, completionErr)
			if reporter != nil && !nestedTracker.hasNestedExecution() {
				if completionOutcome != pluginapi.RequestCompletionSucceeded && completionErr != nil {
					if !streamUsage.PublishFailure(execCtx, reporter, completionErr) {
						reporter.PublishFailure(execCtx, completionErr)
					}
				} else {
					streamUsage.Publish(execCtx, reporter)
					reporter.EnsurePublished(execCtx)
				}
			}
		}()
		defer close(dataChan)
		defer close(errChan)
		chunkIndex := 0
		var historyChunks [][]byte
		for {
			chunk, ok, canceled := nextStreamChunk(ctx, nil, nil, chunks)
			if canceled {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				if ctx != nil {
					completionErr = ctx.Err()
				}
				return
			}
			if !ok {
				if responseSSEValidator != nil {
					if errValidate := responseSSEValidator.Finish(); errValidate != nil {
						completionOutcome = pluginapi.RequestCompletionFailed
						completionStatus = http.StatusBadGateway
						completionErr = errValidate
						select {
						case errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errValidate}:
						case <-done:
							completionOutcome = pluginapi.RequestCompletionCanceled
							completionStatus = 0
							if ctx != nil {
								completionErr = ctx.Err()
							}
						}
					}
				}
				return
			}
			if chunk.Err != nil {
				errMsg := executionErrorMessage(chunk.Err)
				completionOutcome = pluginapi.RequestCompletionFailed
				completionStatus = errMsg.StatusCode
				completionErr = chunk.Err
				select {
				case errChan <- errMsg:
				case <-done:
					completionOutcome = pluginapi.RequestCompletionCanceled
					completionStatus = 0
					if ctx != nil {
						completionErr = ctx.Err()
					}
				}
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			observePluginExecutorStreamUsage(responseProtocol, chunk.Payload, &streamUsage)
			payload := cloneBytes(chunk.Payload)
			if streamInterceptorsActive {
				chunkReq := pluginapi.StreamChunkInterceptRequest{
					RequestID:       lifecycle.requestID(),
					SourceFormat:    responseProtocol,
					Model:           modelName,
					RequestedModel:  originalRequestedModel,
					RequestHeaders:  cloneHeader(streamRequestHeaders),
					ResponseHeaders: cloneHeader(rawStreamHeaders),
					Body:            payload,
					ChunkIndex:      chunkIndex,
					Metadata:        opts.Metadata,
				}
				// Re-evaluate each chunk so mid-stream plugin reloads stay correct.
				// Schema v5+ omits history here.
				if streamChunkPayloadIncludesHistory(interceptorHost) {
					chunkReq.HistoryChunks = cloneByteSlices(historyChunks)
				}
				// Schema v3+ omits bodies here (one header-init clone only).
				if streamChunkPayloadIncludesRequestBody(interceptorHost) {
					chunkReq.OriginalRequest = cloneBytes(streamOriginalRequest)
					chunkReq.RequestBody = cloneBytes(streamRequestBody)
				}
				intercepted := interceptStreamChunk(ctx, interceptorHost, chunkReq, execOptions.SkipInterceptorPluginID)
				applyStreamHeaders(intercepted.Headers)
				if len(intercepted.Body) > 0 {
					payload = cloneBytes(intercepted.Body)
				}
				chunkIndex++
				if intercepted.DropChunk {
					continue
				}
			} else {
				chunkIndex++
			}
			if responseSSEValidator != nil {
				validatedPayload, errValidate := responseSSEValidator.AddChunk(payload)
				if errValidate != nil {
					completionOutcome = pluginapi.RequestCompletionFailed
					completionStatus = http.StatusBadGateway
					completionErr = errValidate
					select {
					case errChan <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errValidate}:
					case <-done:
						completionOutcome = pluginapi.RequestCompletionCanceled
						completionStatus = 0
						if ctx != nil {
							completionErr = ctx.Err()
						}
					}
					return
				}
				payload = validatedPayload
				if len(payload) == 0 {
					continue
				}
			}
			select {
			case dataChan <- payload:
				if streamInterceptorsActive && streamChunkPayloadIncludesHistory(interceptorHost) {
					historyChunks = appendStreamInterceptorHistory(historyChunks, payload)
				}
			case <-done:
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				if ctx != nil {
					completionErr = ctx.Err()
				}
				return
			}
		}
	}()
	return dataChan, upstreamHeaders, errChan
}

func (h *BaseAPIHandler) executeStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, allowImageModel bool) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	return h.executeStreamWithAuthManagerFormats(ctx, handlerType, handlerType, modelName, rawJSON, alt, allowImageModel, modelExecutionOptions{})
}

func (h *BaseAPIHandler) executeStreamWithAuthManagerFormats(ctx context.Context, entryProtocol, exitProtocol, modelName string, rawJSON []byte, alt string, allowImageModel bool, execOptions modelExecutionOptions) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage) {
	originalRequestedModel := modelName
	routeDecision, preparedRoute := preparedModelRouteFromContext(ctx, execOptions.SkipRouterPluginID)
	if !preparedRoute {
		routeDecision = h.applyModelRouter(ctx, entryProtocol, modelName, rawJSON, true, execOptions)
	}
	responseProtocol := modelExecutionResponseProtocol(entryProtocol, exitProtocol)
	if errMsg := validateNativeInteractionsExecution(entryProtocol, execOptions, routeDecision); errMsg != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	if routeDecision.ExecutorPluginID != "" {
		return h.streamWithPluginExecutor(ctx, entryProtocol, responseProtocol, modelName, originalRequestedModel, rawJSON, alt, routeDecision.ExecutorPluginID, execOptions)
	}
	providers, normalizedModel, errMsg := h.providersForExecution(modelName, originalRequestedModel, allowImageModel, routeDecision, execOptions)
	if errMsg != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	providers = adjustExecutionProvidersForEntryProtocol(entryProtocol, providers)
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = originalRequestedModel
	addAuthSelectionModelMetadata(reqMeta, execOptions.AuthSelectionModel)
	addModelExecutionSourceMetadata(reqMeta, execOptions.InternalSource)
	setReasoningEffortMetadata(reqMeta, entryProtocol, normalizedModel, rawJSON)
	setServiceTierMetadata(reqMeta, rawJSON)
	setGenerateMetadata(reqMeta, rawJSON)
	if limit := parseModelTokenLimit(normalizedModel); limit > 0 {
		if estimated := len(rawJSON) / 4; estimated > limit {
			errChan := make(chan *interfaces.ErrorMessage, 1)
			errChan <- &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("estimated input size (%d tokens) exceeds limit (%d) for model %q", estimated, limit, normalizedModel),
			}
			close(errChan)
			return nil, nil, errChan
		}
	}
	payload := rawJSON
	if len(payload) == 0 {
		payload = nil
	}
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: payload,
	}
	afterAuthCapture := &requestAfterAuthCapture{}
	lifecycle := h.newRequestLifecycleTracker(ctx, entryProtocol, normalizedModel, originalRequestedModel, true, reqMeta, execOptions.SkipInterceptorPluginID)
	opts := coreexecutor.Options{
		Stream:                      true,
		Alt:                         alt,
		OriginalRequest:             rawJSON,
		SourceFormat:                sdktranslator.FromString(entryProtocol),
		ResponseFormat:              sdktranslator.FromString(responseProtocol),
		Headers:                     modelExecutionHeaders(ctx, execOptions.Headers),
		Query:                       modelExecutionQuery(ctx, execOptions.Query),
		RequestAfterAuthInterceptor: h.requestAfterAuthInterceptor(afterAuthCapture, lifecycle.requestID(), execOptions.SkipInterceptorPluginID),
		WebSocketResponseObserver:   h.webSocketResponseObserver(lifecycle.requestID(), execOptions.SkipInterceptorPluginID),
		ProxyURL:                    execOptions.ProxyURL,
	}
	opts.Metadata = reqMeta
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	var interceptErr *interfaces.ErrorMessage
	req, opts, interceptErr = h.applyRequestInterceptorsBeforeAuth(ctx, entryProtocol, originalRequestedModel, lifecycle.requestID(), req, opts, execOptions.SkipInterceptorPluginID)
	if interceptErr != nil {
		lifecycle.completeError(ctx, interceptErr)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- interceptErr
		close(errChan)
		return nil, nil, errChan
	}
	ctx = enrichContextWithSessionHierarchy(ctx, opts.Headers, req.Payload, opts.Metadata)
	streamResult, err := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		err = enrichAuthSelectionError(err, providers, normalizedModel)
		errMsg := executionErrorMessage(err)
		lifecycle.completeError(ctx, errMsg)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	if streamResult == nil {
		errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("auth manager returned nil stream")}
		lifecycle.completeError(ctx, errMsg)
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, nil, errChan
	}
	executedRequest := func() (coreexecutor.Request, coreexecutor.Options) {
		return afterAuthCapture.apply(req, opts)
	}
	if executedReq, executedOpts := executedRequest(); len(executedOpts.Headers) > 0 || len(executedReq.Payload) > 0 || len(executedOpts.Metadata) > 0 {
		ctx = enrichContextWithSessionHierarchy(ctx, executedOpts.Headers, executedReq.Payload, executedOpts.Metadata)
	}
	passthroughHeadersEnabled := executionPassthroughHeaders(h.Cfg, execOptions.InternalSource)
	interceptorHost := h.interceptorHost()
	streamInterceptorsActive := streamInterceptorsEnabled(interceptorHost)
	// Resolve bootstrap retries and header initialization before returning so the
	// returned header snapshot is never modified by the stream goroutine.
	rawStreamHeaders := cloneHeader(streamResult.Headers)
	baseStreamHeaders := cloneHeader(streamResult.Headers)
	chunks := streamResult.Chunks
	if chunks == nil {
		closed := make(chan coreexecutor.StreamChunk)
		close(closed)
		chunks = closed
	}
	streamClosedBeforeRead := false
	streamCanceledBeforeRead := false
	streamHeaderInitialized := false
	// Request headers/bodies are stream-invariant after after-auth capture. Keep a private
	// snapshot and clone into each interceptor call so plugins cannot mutate shared storage.
	// Schema v3+ payload chunks omit these bodies (host also strips per plugin).
	var streamRequestHeaders http.Header
	var streamOriginalRequest []byte
	var streamRequestBody []byte

	applyStreamHeaders := func(headers http.Header) {
		rawStreamHeaders = finalInterceptorHeaders(rawStreamHeaders, headers)
	}

	applyStreamHeaderInit := func() {
		if !streamInterceptorsActive || streamHeaderInitialized {
			return
		}
		executedReq, executedOpts := executedRequest()
		streamRequestHeaders = cloneHeader(executedOpts.Headers)
		streamOriginalRequest = cloneBytes(executedOpts.OriginalRequest)
		streamRequestBody = cloneBytes(executedReq.Payload)
		intercepted := interceptStreamChunk(ctx, interceptorHost, pluginapi.StreamChunkInterceptRequest{
			RequestID:       lifecycle.requestID(),
			SourceFormat:    responseProtocol,
			Model:           normalizedModel,
			RequestedModel:  originalRequestedModel,
			RequestHeaders:  cloneHeader(streamRequestHeaders),
			ResponseHeaders: cloneHeader(rawStreamHeaders),
			OriginalRequest: cloneBytes(streamOriginalRequest),
			RequestBody:     cloneBytes(streamRequestBody),
			ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
			Metadata:        executedOpts.Metadata,
		}, execOptions.SkipInterceptorPluginID)
		applyStreamHeaders(intercepted.Headers)
		streamHeaderInitialized = true
	}

	var responseSSEValidator *sseJSONValidationState
	if responseProtocol == "openai-response" {
		responseSSEValidator = &sseJSONValidationState{}
	}

	transformStreamPayload := func(payload []byte, chunkIndex *int, historyChunks [][]byte) ([]byte, bool, *interfaces.ErrorMessage) {
		applyStreamHeaderInit()
		payload = cloneBytes(payload)
		if streamInterceptorsActive {
			chunkReq := pluginapi.StreamChunkInterceptRequest{
				RequestID:       lifecycle.requestID(),
				SourceFormat:    responseProtocol,
				Model:           normalizedModel,
				RequestedModel:  originalRequestedModel,
				RequestHeaders:  cloneHeader(streamRequestHeaders),
				ResponseHeaders: cloneHeader(rawStreamHeaders),
				Body:            payload,
				ChunkIndex:      *chunkIndex,
				Metadata:        opts.Metadata,
			}
			// Re-evaluate each chunk so mid-stream plugin reloads stay correct.
			// Schema v5+ omits history here.
			if streamChunkPayloadIncludesHistory(interceptorHost) {
				chunkReq.HistoryChunks = cloneByteSlices(historyChunks)
			}
			// Schema v3+ omits bodies here (one header-init clone only).
			if streamChunkPayloadIncludesRequestBody(interceptorHost) {
				chunkReq.OriginalRequest = cloneBytes(streamOriginalRequest)
				chunkReq.RequestBody = cloneBytes(streamRequestBody)
			}
			intercepted := interceptStreamChunk(ctx, interceptorHost, chunkReq, execOptions.SkipInterceptorPluginID)
			applyStreamHeaders(intercepted.Headers)
			if len(intercepted.Body) > 0 {
				payload = cloneBytes(intercepted.Body)
			}
			(*chunkIndex)++
			if intercepted.DropChunk {
				return nil, false, nil
			}
		} else {
			(*chunkIndex)++
		}
		if responseSSEValidator != nil {
			validatedPayload, errValidate := responseSSEValidator.AddChunk(payload)
			if errValidate != nil {
				return nil, false, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errValidate}
			}
			payload = validatedPayload
			if len(payload) == 0 {
				return nil, false, nil
			}
		}
		return payload, true, nil
	}

	var bootstrapPayload []byte
	bootstrapChunkIndex := 0
	var bootstrapHistoryChunks [][]byte
	var bootstrapStreamErr error
	var bootstrapErr *interfaces.ErrorMessage
	readInitialStreamChunks := func() {
		for {
			var chunk coreexecutor.StreamChunk
			var ok bool
			if ctx != nil {
				select {
				case <-ctx.Done():
					streamCanceledBeforeRead = true
					return
				case chunk, ok = <-chunks:
				}
			} else {
				chunk, ok = <-chunks
			}
			if !ok {
				streamClosedBeforeRead = true
				applyStreamHeaderInit()
				return
			}
			if chunk.Err != nil {
				bootstrapStreamErr = chunk.Err
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			payload, deliverable, errMsg := transformStreamPayload(chunk.Payload, &bootstrapChunkIndex, bootstrapHistoryChunks)
			if errMsg != nil {
				bootstrapErr = errMsg
				return
			}
			if !deliverable {
				continue
			}
			bootstrapPayload = payload
			return
		}
	}

	bootstrapEligible := func(err error) bool {
		status := statusFromError(err)
		if status == 0 {
			return true
		}
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
			http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		default:
			return status >= http.StatusInternalServerError
		}
	}

	maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)
	if h.AuthManager.HomeEnabled() {
		maxBootstrapRetries = 0
	}
	for bootstrapRetries := 0; !streamCanceledBeforeRead; {
		readInitialStreamChunks()
		if streamCanceledBeforeRead || bootstrapErr != nil || bootstrapStreamErr == nil {
			break
		}
		if bootstrapRetries >= maxBootstrapRetries || !bootstrapEligible(bootstrapStreamErr) {
			bootstrapErr = executionErrorMessage(bootstrapStreamErr)
			break
		}
		bootstrapRetries++
		retryResult, retryErr := h.AuthManager.ExecuteStream(ctx, providers, req, opts)
		if retryErr != nil {
			originalBootstrapErr := executionErrorMessage(bootstrapStreamErr)
			if isAuthSelectionUnavailable(retryErr) && originalBootstrapErr.StatusCode >= http.StatusInternalServerError {
				bootstrapErr = originalBootstrapErr
			} else {
				bootstrapErr = executionErrorMessage(enrichAuthSelectionError(retryErr, providers, normalizedModel))
			}
			break
		}
		if retryResult == nil {
			bootstrapErr = executionErrorMessage(fmt.Errorf("auth manager returned nil stream"))
			break
		}
		rawStreamHeaders = cloneHeader(retryResult.Headers)
		baseStreamHeaders = cloneHeader(retryResult.Headers)
		streamHeaderInitialized = false
		streamClosedBeforeRead = false
		bootstrapStreamErr = nil
		bootstrapPayload = nil
		bootstrapChunkIndex = 0
		bootstrapHistoryChunks = nil
		if responseSSEValidator != nil {
			responseSSEValidator = &sseJSONValidationState{}
		}
		chunks = retryResult.Chunks
		if chunks == nil {
			closed := make(chan coreexecutor.StreamChunk)
			close(closed)
			chunks = closed
		}
	}

	upstreamHeaders := downstreamHeadersAfterInterceptors(baseStreamHeaders, rawStreamHeaders, passthroughHeadersEnabled)
	if upstreamHeaders == nil && (passthroughHeadersEnabled || streamInterceptorsActive) {
		upstreamHeaders = make(http.Header)
	}
	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)

	go func() {
		completionOutcome := pluginapi.RequestCompletionSucceeded
		completionStatus := http.StatusOK
		var completionErr error
		defer func() {
			lifecycle.complete(completionOutcome, completionStatus, completionErr)
		}()
		defer close(dataChan)
		defer close(errChan)
		if streamCanceledBeforeRead {
			completionOutcome = pluginapi.RequestCompletionCanceled
			completionStatus = 0
			if ctx != nil {
				completionErr = ctx.Err()
			}
			return
		}

		sendErr := func(msg *interfaces.ErrorMessage) bool {
			if ctx == nil {
				errChan <- msg
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case errChan <- msg:
				return true
			}
		}

		sendData := func(chunk []byte) bool {
			if ctx == nil {
				dataChan <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case dataChan <- chunk:
				return true
			}
		}

		if bootstrapErr != nil {
			completionOutcome = pluginapi.RequestCompletionFailed
			if bootstrapErr.DirectResponse {
				completionOutcome = pluginapi.RequestCompletionRejected
			}
			completionStatus = bootstrapErr.StatusCode
			completionErr = bootstrapErr.Error
			if !sendErr(bootstrapErr) && ctx != nil && ctx.Err() != nil {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				completionErr = ctx.Err()
			}
			return
		}

		chunkIndex := bootstrapChunkIndex
		historyChunks := bootstrapHistoryChunks
		if bootstrapPayload != nil {
			if okSendData := sendData(bootstrapPayload); !okSendData {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				if ctx != nil {
					completionErr = ctx.Err()
				}
				return
			}
			if streamInterceptorsActive && streamChunkPayloadIncludesHistory(interceptorHost) {
				historyChunks = appendStreamInterceptorHistory(historyChunks, bootstrapPayload)
			}
		}
		for {
			chunk, ok, canceled := nextStreamChunk(ctx, nil, &streamClosedBeforeRead, chunks)
			if canceled {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				if ctx != nil {
					completionErr = ctx.Err()
				}
				return
			}
			if !ok {
				if responseSSEValidator != nil {
					if errValidate := responseSSEValidator.Finish(); errValidate != nil {
						errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errValidate}
						completionOutcome = pluginapi.RequestCompletionFailed
						completionStatus = errMsg.StatusCode
						completionErr = errMsg.Error
						_ = sendErr(errMsg)
					}
				}
				return
			}
			if chunk.Err != nil {
				errMsg := executionErrorMessage(chunk.Err)
				completionOutcome = pluginapi.RequestCompletionFailed
				completionStatus = errMsg.StatusCode
				completionErr = chunk.Err
				if !sendErr(errMsg) && ctx != nil && ctx.Err() != nil {
					completionOutcome = pluginapi.RequestCompletionCanceled
					completionStatus = 0
					completionErr = ctx.Err()
				}
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			payload, deliverable, errMsg := transformStreamPayload(chunk.Payload, &chunkIndex, historyChunks)
			if errMsg != nil {
				completionOutcome = pluginapi.RequestCompletionFailed
				completionStatus = errMsg.StatusCode
				completionErr = errMsg.Error
				if !sendErr(errMsg) && ctx != nil && ctx.Err() != nil {
					completionOutcome = pluginapi.RequestCompletionCanceled
					completionStatus = 0
					completionErr = ctx.Err()
				}
				return
			}
			if !deliverable {
				continue
			}
			if okSendData := sendData(payload); !okSendData {
				completionOutcome = pluginapi.RequestCompletionCanceled
				completionStatus = 0
				if ctx != nil {
					completionErr = ctx.Err()
				}
				return
			}
			if streamInterceptorsActive && streamChunkPayloadIncludesHistory(interceptorHost) {
				historyChunks = appendStreamInterceptorHistory(historyChunks, payload)
			}
		}
	}()
	return dataChan, upstreamHeaders, errChan
}

type sseJSONValidationState struct {
	pending        []byte
	pendingErr     error
	prevEndsWithCR bool
}

func (s *sseJSONValidationState) AddChunk(chunk []byte) ([]byte, error) {
	if s.pendingErr != nil {
		errPending := s.pendingErr
		s.pendingErr = nil
		return nil, errPending
	}
	if len(chunk) == 0 {
		return nil, nil
	}
	if s.prevEndsWithCR {
		if chunk[0] == '\n' {
			chunk = chunk[1:]
		}
		s.prevEndsWithCR = false
	}
	if len(chunk) == 0 {
		return nil, nil
	}
	endsWithCR := chunk[len(chunk)-1] == '\r'
	chunk = bytes.ReplaceAll(chunk, []byte("\r\n"), []byte("\n"))
	chunk = bytes.ReplaceAll(chunk, []byte("\r"), []byte("\n"))
	s.prevEndsWithCR = endsWithCR
	if len(s.pending) > 0 && !bytes.HasSuffix(s.pending, []byte("\n")) && !bytes.HasPrefix(chunk, []byte("\n")) {
		first := bytes.TrimSpace(bytes.SplitN(chunk, []byte("\n"), 2)[0])
		if bytes.HasPrefix(first, []byte("data:")) || bytes.HasPrefix(first, []byte("event:")) {
			s.pending = append(s.pending, '\n')
		}
	}
	s.pending = append(s.pending, chunk...)

	var output []byte
	for {
		frameEnd := bytes.Index(s.pending, []byte("\n\n"))
		if frameEnd < 0 {
			break
		}
		frameEnd += 2
		frame := s.pending[:frameEnd]
		if errValidate := validateSSEFrameDataJSON(frame); errValidate != nil {
			if len(output) > 0 {
				s.pending = s.pending[:0]
				s.pendingErr = errValidate
				return output, nil
			}
			return nil, errValidate
		}
		output = append(output, frame...)
		copy(s.pending, s.pending[frameEnd:])
		s.pending = s.pending[:len(s.pending)-frameEnd]
	}

	if len(bytes.TrimSpace(s.pending)) == 0 {
		s.pending = s.pending[:0]
		return output, nil
	}
	payload, found := sseJSONValidationDataPayload(s.pending)
	payload = bytes.TrimSpace(payload)
	if !found || len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || json.Valid(payload) {
		output = append(output, s.pending...)
		s.pending = s.pending[:0]
	}
	return output, nil
}

func (s *sseJSONValidationState) Finish() error {
	s.prevEndsWithCR = false
	if s.pendingErr != nil {
		errPending := s.pendingErr
		s.pendingErr = nil
		s.pending = nil
		return errPending
	}
	if len(bytes.TrimSpace(s.pending)) == 0 {
		s.pending = nil
		return nil
	}
	errValidate := validateSSEFrameDataJSON(s.pending)
	s.pending = nil
	return errValidate
}

func sseJSONValidationDataPayload(frame []byte) ([]byte, bool) {
	var payload []byte
	found := false
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if found {
			payload = append(payload, '\n')
		}
		payload = append(payload, bytes.TrimSpace(line[len("data:"):])...)
		found = true
	}
	return payload, found
}

func validateSSEFrameDataJSON(frame []byte) error {
	payload, found := sseJSONValidationDataPayload(frame)
	payload = bytes.TrimSpace(payload)
	if !found || len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || json.Valid(payload) {
		return nil
	}
	const max = 512
	preview := payload
	if len(preview) > max {
		preview = preview[:max]
	}
	return fmt.Errorf("invalid SSE data JSON (len=%d): %q", len(payload), preview)
}

func validateSSEDataJSON(chunk []byte) error {
	state := &sseJSONValidationState{}
	if _, errAdd := state.AddChunk(chunk); errAdd != nil {
		return errAdd
	}
	return state.Finish()
}
```

## `sdk/api/handlers/gemini/gemini_handlers.go`

SHA-256 (LF): `2077d9cf1c2b0147225d8e5f0428512cf8897676ddd2e2b6e003873b6a4f492a`

```go
// Package gemini provides HTTP handlers for Gemini API endpoints.
// This package implements handlers for managing Gemini model operations including
// model listing, content generation, streaming content generation, and token counting.
// It serves as a proxy layer between clients and the Gemini backend service,
// handling request translation, client management, and response processing.
package gemini

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

// GeminiAPIHandler contains the handlers for Gemini API endpoints.
// It holds a pool of clients to interact with the backend service.
type GeminiAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewGeminiAPIHandler creates a new Gemini API handlers instance.
// It takes an BaseAPIHandler instance as input and returns a GeminiAPIHandler.
func NewGeminiAPIHandler(apiHandlers *handlers.BaseAPIHandler) *GeminiAPIHandler {
	return &GeminiAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *GeminiAPIHandler) HandlerType() string {
	return Gemini
}

// Models returns the Gemini-compatible model metadata supported by this handler.
func (h *GeminiAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("gemini")
}

// GeminiModels handles the Gemini models listing endpoint.
// It returns a JSON response containing available Gemini models and their specifications.
func (h *GeminiAPIHandler) GeminiModels(c *gin.Context) {
	rawModels := h.Models()
	normalizedModels := make([]map[string]any, 0, len(rawModels))
	defaultMethods := []string{"generateContent"}
	for _, model := range rawModels {
		normalizedModel := make(map[string]any, len(model))
		for k, v := range model {
			normalizedModel[k] = v
		}
		if name, ok := normalizedModel["name"].(string); ok && name != "" {
			if !strings.HasPrefix(name, "models/") {
				normalizedModel["name"] = "models/" + name
			}
			if displayName, _ := normalizedModel["displayName"].(string); displayName == "" {
				normalizedModel["displayName"] = name
			}
			if description, _ := normalizedModel["description"].(string); description == "" {
				normalizedModel["description"] = name
			}
		}
		if _, ok := normalizedModel["supportedGenerationMethods"]; !ok {
			normalizedModel["supportedGenerationMethods"] = defaultMethods
		}
		normalizedModels = append(normalizedModels, normalizedModel)
	}
	h.WriteModelListResponse(c, h.HandlerType(), gin.H{
		"models": normalizedModels,
	})
}

// GeminiGetHandler handles GET requests for specific Gemini model information.
// It returns detailed information about a specific Gemini model based on the action parameter.
func (h *GeminiAPIHandler) GeminiGetHandler(c *gin.Context) {
	var request struct {
		Action string `uri:"action" binding:"required"`
	}
	if err := c.ShouldBindUri(&request); err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	action := strings.TrimPrefix(request.Action, "/")

	// Get dynamic models from the global registry and find the matching one
	availableModels := h.Models()
	var targetModel map[string]any

	for _, model := range availableModels {
		name, _ := model["name"].(string)
		// Match name with or without 'models/' prefix
		if name == action || name == "models/"+action {
			targetModel = model
			break
		}
	}

	if targetModel != nil {
		// Ensure the name has 'models/' prefix in the output if it's a Gemini model
		if name, ok := targetModel["name"].(string); ok && name != "" && !strings.HasPrefix(name, "models/") {
			targetModel["name"] = "models/" + name
		}
		c.JSON(http.StatusOK, targetModel)
		return
	}

	c.JSON(http.StatusNotFound, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: "Not Found",
			Type:    "not_found",
		},
	})
}

// GeminiHandler handles POST requests for Gemini API operations.
// It routes requests to appropriate handlers based on the action parameter (model:method format).
func (h *GeminiAPIHandler) GeminiHandler(c *gin.Context) {
	var request struct {
		Action string `uri:"action" binding:"required"`
	}
	if err := c.ShouldBindUri(&request); err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	action := strings.Split(strings.TrimPrefix(request.Action, "/"), ":")
	if len(action) != 2 {
		c.JSON(http.StatusNotFound, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("%s not found.", c.Request.URL.Path),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	method := action[1]
	rawJSON, _ := c.GetRawData()

	switch method {
	case "generateContent":
		h.handleGenerateContent(c, action[0], rawJSON)
	case "streamGenerateContent":
		h.handleStreamGenerateContent(c, action[0], rawJSON)
	case "countTokens":
		h.handleCountTokens(c, action[0], rawJSON)
	}
}

// handleStreamGenerateContent handles streaming content generation requests for Gemini models.
// This function establishes a Server-Sent Events connection and streams the generated content
// back to the client in real-time. It supports both SSE format and direct streaming based
// on the 'alt' query parameter.
//
// Parameters:
//   - c: The Gin context for the request
//   - modelName: The name of the Gemini model to use for content generation
//   - rawJSON: The raw JSON request body containing generation parameters
func (h *GeminiAPIHandler) handleStreamGenerateContent(c *gin.Context, modelName string, rawJSON []byte) {
	alt := h.GetAlt(c)
	requestStart := time.Now()

	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	throttler := handlers.NewRequestThrottler(h.Cfg)
	defer handlers.ObserveRequestThrottle(c.Request.Context(), throttler)()

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, alt)

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}

	// Peek at the first chunk
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				// Err channel closed cleanly; wait for data channel.
				errChan = nil
				continue
			}
			// Upstream failed immediately. Return proper error status and JSON.
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				if errMsg, hasPendingError := handlers.PendingStreamError(errChan); hasPendingError {
					h.WriteErrorResponse(c, errMsg)
					if errMsg != nil {
						cliCancel(errMsg.Error)
					} else {
						cliCancel(nil)
					}
					return
				}
				// Closed without data
				if alt == "" {
					setSSEHeaders()
				}
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				flusher.Flush()
				cliCancel(nil)
				return
			}

			// TTFT and first payload delay: original Gemini streams can put a
			// large amount of generated text in the first chunk, so account for
			// that payload before emitting it.
			if !throttler.ThrottleFirstChunkWithPayload(cliCtx, requestStart, chunk) {
				cliCancel(cliCtx.Err())
				return
			}

			// Success! Set headers.
			if alt == "" {
				setSSEHeaders()
			}
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

			// Write first chunk
			if alt == "" {
				_, _ = c.Writer.Write([]byte("data: "))
				_, _ = c.Writer.Write(chunk)
				_, _ = c.Writer.Write([]byte("\n\n"))
			} else {
				_, _ = c.Writer.Write(chunk)
			}
			flusher.Flush()

			// Continue with throttled stream
			h.forwardGeminiStream(c, flusher, alt, func(err error) { cliCancel(err) }, dataChan, errChan, throttler)
			return
		}
	}
}

// handleCountTokens handles token counting requests for Gemini models.
// This function counts the number of tokens in the provided content without
// generating a response. It's useful for quota management and content validation.
//
// Parameters:
//   - c: The Gin context for the request
//   - modelName: The name of the Gemini model to use for token counting
//   - rawJSON: The raw JSON request body containing the content to count
func (h *GeminiAPIHandler) handleCountTokens(c *gin.Context, modelName string, rawJSON []byte) {
	c.Header("Content-Type", "application/json")
	alt := h.GetAlt(c)
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	resp, upstreamHeaders, errMsg := h.ExecuteCountWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, alt)
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleGenerateContent handles non-streaming content generation requests for Gemini models.
// This function processes the request synchronously and returns the complete generated
// response in a single API call. It supports various generation parameters and
// response formats.
//
// Parameters:
//   - c: The Gin context for the request
//   - modelName: The name of the Gemini model to use for content generation
//   - rawJSON: The raw JSON request body containing generation parameters and content
func (h *GeminiAPIHandler) handleGenerateContent(c *gin.Context, modelName string, rawJSON []byte) {
	c.Header("Content-Type", "application/json")
	alt := h.GetAlt(c)
	requestStart := time.Now()
	throttler := handlers.NewRequestThrottler(h.Cfg)
	defer handlers.ObserveRequestThrottle(c.Request.Context(), throttler)()

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, alt)
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}

	// Non-streaming speed throttle: estimate tokens and delay if needed
	tokenCount := handlers.EstimateObservedNonStreamingTokens(resp, throttler)
	if !throttler.ThrottleNonStreaming(cliCtx, requestStart, tokenCount) {
		cliCancel(cliCtx.Err())
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

func (h *GeminiAPIHandler) forwardGeminiStream(c *gin.Context, flusher http.Flusher, alt string, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, throttler *handlers.RequestThrottler) {
	var keepAliveInterval *time.Duration
	if alt != "" {
		keepAliveInterval = new(time.Duration(0))
	}

	// Build throttle delay callback for subsequent chunks
	var throttleDelay func(chunk []byte)
	if throttler != nil {
		cliCtx := c.Request.Context()
		throttleDelay = func(chunk []byte) {
			throttler.ThrottleChunk(cliCtx, chunk)
		}
	}

	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		KeepAliveInterval: keepAliveInterval,
		ThrottleDelay:     throttleDelay,
		WriteChunk: func(chunk []byte) {
			if alt == "" {
				_, _ = c.Writer.Write([]byte("data: "))
				_, _ = c.Writer.Write(chunk)
				_, _ = c.Writer.Write([]byte("\n\n"))
			} else {
				_, _ = c.Writer.Write(chunk)
			}
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			if errMsg == nil {
				return
			}
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			errText := http.StatusText(status)
			if errMsg.Error != nil && errMsg.Error.Error() != "" {
				errText = errMsg.Error.Error()
			}
			body := handlers.BuildErrorResponseBody(status, errText)
			if alt == "" {
				_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", string(body))
			} else {
				_, _ = c.Writer.Write(body)
			}
		},
	})
}
```

## `sdk/api/handlers/openai/openai_handlers.go`

SHA-256 (LF): `78a4de1efc484e890579cbd1a1d8b10a8a6eebca510f1728af44696e3efdb14b`

```go
// Package openai provides HTTP handlers for OpenAI API endpoints.
// This package implements the OpenAI-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAI API requests to the appropriate backend format and
// convert responses back to OpenAI-compatible format.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	responsesconverter "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAIAPIHandler contains the handlers for OpenAI API endpoints.
// It holds a pool of clients to interact with the backend service.
type OpenAIAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewOpenAIAPIHandler creates a new OpenAI API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an OpenAIAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *OpenAIAPIHandler: A new OpenAI API handlers instance
func NewOpenAIAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIAPIHandler {
	return &OpenAIAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *OpenAIAPIHandler) HandlerType() string {
	return OpenAI
}

// Models returns the OpenAI-compatible model metadata supported by this handler.
func (h *OpenAIAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("openai")
}

// OpenAIModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAI-compatible format.
func (h *OpenAIAPIHandler) OpenAIModels(c *gin.Context) {
	if _, ok := c.Request.URL.Query()["client_version"]; ok {
		clientVersion := c.Query("client_version")
		h.WriteModelListResponse(c, h.HandlerType(), h.codexClientModelsResponse(clientVersion))
		return
	}

	// Get all available models
	allModels := h.Models()

	// Filter to only include the 4 required fields: id, object, created, owned_by
	filteredModels := make([]map[string]any, len(allModels))
	for i, model := range allModels {
		filteredModel := map[string]any{
			"id":     model["id"],
			"object": model["object"],
		}

		// Add created field if it exists
		if created, exists := model["created"]; exists {
			filteredModel["created"] = created
		}

		// Add owned_by field if it exists
		if ownedBy, exists := model["owned_by"]; exists {
			filteredModel["owned_by"] = ownedBy
		}

		filteredModels[i] = filteredModel
	}

	h.WriteModelListResponse(c, h.HandlerType(), gin.H{
		"object": "list",
		"data":   filteredModels,
	})
}

// ChatCompletions handles the /v1/chat/completions endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIAPIHandler) ChatCompletions(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	stream := streamResult.Type == gjson.True

	// Some clients send OpenAI Responses-format payloads to /v1/chat/completions.
	// Convert them to Chat Completions so downstream translators preserve tool metadata.
	if shouldTreatAsResponsesFormat(rawJSON) {
		modelName := gjson.GetBytes(rawJSON, "model").String()
		rawJSON = responsesconverter.ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName, rawJSON, stream)
		stream = gjson.GetBytes(rawJSON, "stream").Bool()
	}

	if stream {
		h.handleStreamingResponse(c, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, rawJSON)
	}

}

// shouldTreatAsResponsesFormat detects OpenAI Responses-style payloads that are
// accidentally sent to the Chat Completions endpoint.
func shouldTreatAsResponsesFormat(rawJSON []byte) bool {
	if gjson.GetBytes(rawJSON, "messages").Exists() {
		return false
	}
	if gjson.GetBytes(rawJSON, "input").Exists() {
		return true
	}
	if gjson.GetBytes(rawJSON, "instructions").Exists() {
		return true
	}
	return false
}

// Completions handles the /v1/completions endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
// This endpoint follows the OpenAI completions API specification.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIAPIHandler) Completions(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		h.handleCompletionsStreamingResponse(c, rawJSON)
	} else {
		h.handleCompletionsNonStreamingResponse(c, rawJSON)
	}

}

// convertCompletionsRequestToChatCompletions converts OpenAI completions API request to chat completions format.
// This allows the completions endpoint to use the existing chat completions infrastructure.
//
// Parameters:
//   - rawJSON: The raw JSON bytes of the completions request
//
// Returns:
//   - []byte: The converted chat completions request
func convertCompletionsRequestToChatCompletions(rawJSON []byte) []byte {
	root := gjson.ParseBytes(rawJSON)

	// Extract prompt from completions request
	prompt := root.Get("prompt").String()
	if prompt == "" {
		prompt = "Complete this:"
	}

	// Create chat completions structure
	out := []byte(`{"model":"","messages":[{"role":"user","content":""}]}`)

	// Set model
	if model := root.Get("model"); model.Exists() {
		out, _ = sjson.SetBytes(out, "model", model.String())
	}

	// Set the prompt as user message content
	out, _ = sjson.SetBytes(out, "messages.0.content", prompt)

	// Copy other parameters from completions to chat completions
	if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	if temperature := root.Get("temperature"); temperature.Exists() {
		out, _ = sjson.SetBytes(out, "temperature", temperature.Float())
	}

	if topP := root.Get("top_p"); topP.Exists() {
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}

	if frequencyPenalty := root.Get("frequency_penalty"); frequencyPenalty.Exists() {
		out, _ = sjson.SetBytes(out, "frequency_penalty", frequencyPenalty.Float())
	}

	if presencePenalty := root.Get("presence_penalty"); presencePenalty.Exists() {
		out, _ = sjson.SetBytes(out, "presence_penalty", presencePenalty.Float())
	}

	if stop := root.Get("stop"); stop.Exists() {
		out, _ = sjson.SetRawBytes(out, "stop", []byte(stop.Raw))
	}

	if stream := root.Get("stream"); stream.Exists() {
		out, _ = sjson.SetBytes(out, "stream", stream.Bool())
	}

	if logprobs := root.Get("logprobs"); logprobs.Exists() {
		out, _ = sjson.SetBytes(out, "logprobs", logprobs.Bool())
	}

	if topLogprobs := root.Get("top_logprobs"); topLogprobs.Exists() {
		out, _ = sjson.SetBytes(out, "top_logprobs", topLogprobs.Int())
	}

	if echo := root.Get("echo"); echo.Exists() {
		out, _ = sjson.SetBytes(out, "echo", echo.Bool())
	}

	return out
}

// convertChatCompletionsResponseToCompletions converts chat completions API response back to completions format.
// This ensures the completions endpoint returns data in the expected format.
//
// Parameters:
//   - rawJSON: The raw JSON bytes of the chat completions response
//
// Returns:
//   - []byte: The converted completions response
func convertChatCompletionsResponseToCompletions(rawJSON []byte) []byte {
	root := gjson.ParseBytes(rawJSON)

	// Base completions response structure
	out := []byte(`{"id":"","object":"text_completion","created":0,"model":"","choices":[]}`)

	// Copy basic fields
	if id := root.Get("id"); id.Exists() {
		out, _ = sjson.SetBytes(out, "id", id.String())
	}

	if created := root.Get("created"); created.Exists() {
		out, _ = sjson.SetBytes(out, "created", created.Int())
	}

	if model := root.Get("model"); model.Exists() {
		out, _ = sjson.SetBytes(out, "model", model.String())
	}

	if usage := root.Get("usage"); usage.Exists() {
		out, _ = sjson.SetRawBytes(out, "usage", []byte(usage.Raw))
	}

	// Convert choices from chat completions to completions format
	var choices []interface{}
	if chatChoices := root.Get("choices"); chatChoices.Exists() && chatChoices.IsArray() {
		chatChoices.ForEach(func(_, choice gjson.Result) bool {
			completionsChoice := map[string]interface{}{
				"index": choice.Get("index").Int(),
			}

			// Extract text content from message.content
			if message := choice.Get("message"); message.Exists() {
				if content := message.Get("content"); content.Exists() {
					completionsChoice["text"] = content.String()
				}
			} else if delta := choice.Get("delta"); delta.Exists() {
				// For streaming responses, use delta.content
				if content := delta.Get("content"); content.Exists() {
					completionsChoice["text"] = content.String()
				}
			}

			// Copy finish_reason
			if finishReason := choice.Get("finish_reason"); finishReason.Exists() {
				completionsChoice["finish_reason"] = finishReason.String()
			}

			// Copy logprobs if present
			if logprobs := choice.Get("logprobs"); logprobs.Exists() {
				completionsChoice["logprobs"] = logprobs.Value()
			}

			choices = append(choices, completionsChoice)
			return true
		})
	}

	if len(choices) > 0 {
		choicesJSON, _ := json.Marshal(choices)
		out, _ = sjson.SetRawBytes(out, "choices", choicesJSON)
	}

	return out
}

// convertChatCompletionsStreamChunkToCompletions converts a streaming chat completions chunk to completions format.
// This handles the real-time conversion of streaming response chunks and filters out empty text responses.
//
// Parameters:
//   - chunkData: The raw JSON bytes of a single chat completions stream chunk
//
// Returns:
//   - []byte: The converted completions stream chunk, or nil if should be filtered out
func convertChatCompletionsStreamChunkToCompletions(chunkData []byte) []byte {
	root := gjson.ParseBytes(chunkData)

	// Check if this chunk has any meaningful content
	hasContent := false
	hasUsage := root.Get("usage").Exists()
	if chatChoices := root.Get("choices"); chatChoices.Exists() && chatChoices.IsArray() {
		chatChoices.ForEach(func(_, choice gjson.Result) bool {
			// Check if delta has content or finish_reason
			if delta := choice.Get("delta"); delta.Exists() {
				if content := delta.Get("content"); content.Exists() && content.String() != "" {
					hasContent = true
					return false // Break out of forEach
				}
			}
			// Also check for finish_reason to ensure we don't skip final chunks
			if finishReason := choice.Get("finish_reason"); finishReason.Exists() && finishReason.String() != "" && finishReason.String() != "null" {
				hasContent = true
				return false // Break out of forEach
			}
			return true
		})
	}

	// If no meaningful content and no usage, return nil to indicate this chunk should be skipped
	if !hasContent && !hasUsage {
		return nil
	}

	// Base completions stream response structure
	out := []byte(`{"id":"","object":"text_completion","created":0,"model":"","choices":[]}`)

	// Copy basic fields
	if id := root.Get("id"); id.Exists() {
		out, _ = sjson.SetBytes(out, "id", id.String())
	}

	if created := root.Get("created"); created.Exists() {
		out, _ = sjson.SetBytes(out, "created", created.Int())
	}

	if model := root.Get("model"); model.Exists() {
		out, _ = sjson.SetBytes(out, "model", model.String())
	}

	// Convert choices from chat completions delta to completions format
	var choices []interface{}
	if chatChoices := root.Get("choices"); chatChoices.Exists() && chatChoices.IsArray() {
		chatChoices.ForEach(func(_, choice gjson.Result) bool {
			completionsChoice := map[string]interface{}{
				"index": choice.Get("index").Int(),
			}

			// Extract text content from delta.content
			if delta := choice.Get("delta"); delta.Exists() {
				if content := delta.Get("content"); content.Exists() && content.String() != "" {
					completionsChoice["text"] = content.String()
				} else {
					completionsChoice["text"] = ""
				}
			} else {
				completionsChoice["text"] = ""
			}

			// Copy finish_reason
			if finishReason := choice.Get("finish_reason"); finishReason.Exists() && finishReason.String() != "null" {
				completionsChoice["finish_reason"] = finishReason.String()
			}

			// Copy logprobs if present
			if logprobs := choice.Get("logprobs"); logprobs.Exists() {
				completionsChoice["logprobs"] = logprobs.Value()
			}

			choices = append(choices, completionsChoice)
			return true
		})
	}

	if len(choices) > 0 {
		choicesJSON, _ := json.Marshal(choices)
		out, _ = sjson.SetRawBytes(out, "choices", choicesJSON)
	}

	// Copy usage if present
	if usage := root.Get("usage"); usage.Exists() {
		out, _ = sjson.SetRawBytes(out, "usage", []byte(usage.Raw))
	}

	return out
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for Gemini models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAI format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAI-compatible request
func (h *OpenAIAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	requestStart := time.Now()
	throttler := handlers.NewRequestThrottler(h.Cfg)

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, h.GetAlt(c))
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}

	// Non-streaming speed throttle
	tokenCount := handlers.EstimateNonStreamingTokens(resp)
	if !throttler.ThrottleNonStreaming(cliCtx, requestStart, tokenCount) {
		cliCancel(cliCtx.Err())
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleStreamingResponse handles streaming responses for Gemini models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAI-compatible request
func (h *OpenAIAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	modelName := gjson.GetBytes(rawJSON, "model").String()
	requestStart := time.Now()
	throttler := handlers.NewRequestThrottler(h.Cfg)

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, h.GetAlt(c))

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}

	// Peek at the first chunk to determine success or failure before setting headers
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				// Err channel closed cleanly; wait for data channel.
				errChan = nil
				continue
			}
			// Upstream failed immediately. Return proper error status and JSON.
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				if errMsg, hasPendingError := handlers.PendingStreamError(errChan); hasPendingError {
					h.WriteErrorResponse(c, errMsg)
					if errMsg != nil {
						cliCancel(errMsg.Error)
					} else {
						cliCancel(nil)
					}
					return
				}
				// Stream closed without data? Send DONE or just headers.
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
				flusher.Flush()
				cliCancel(nil)
				return
			}

			// TTFT and first payload delay.
			if !throttler.ThrottleFirstChunkWithPayload(cliCtx, requestStart, chunk) {
				cliCancel(cliCtx.Err())
				return
			}

			// Success! Commit to streaming headers.
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(chunk))
			flusher.Flush()

			// Continue streaming the rest with throttling
			h.handleStreamResult(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, throttler)
			return
		}
	}
}

// handleCompletionsNonStreamingResponse handles non-streaming completions responses.
// It converts completions request to chat completions format, sends to backend,
// then converts the response back to completions format before sending to client.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAI-compatible completions request
func (h *OpenAIAPIHandler) handleCompletionsNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	// Convert completions request to chat completions format
	chatCompletionsJSON := convertCompletionsRequestToChatCompletions(rawJSON)

	modelName := gjson.GetBytes(chatCompletionsJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, chatCompletionsJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	completionsResp := convertChatCompletionsResponseToCompletions(resp)
	_, _ = c.Writer.Write(completionsResp)
	cliCancel()
}

// handleCompletionsStreamingResponse handles streaming completions responses.
// It converts completions request to chat completions format, streams from backend,
// then converts each response chunk back to completions format before sending to client.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAI-compatible completions request
func (h *OpenAIAPIHandler) handleCompletionsStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	// Convert completions request to chat completions format
	chatCompletionsJSON := convertCompletionsRequestToChatCompletions(rawJSON)

	modelName := gjson.GetBytes(chatCompletionsJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, chatCompletionsJSON, "")

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}

	// Peek at the first chunk
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				// Err channel closed cleanly; wait for data channel.
				errChan = nil
				continue
			}
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				if errMsg, hasPendingError := handlers.PendingStreamError(errChan); hasPendingError {
					h.WriteErrorResponse(c, errMsg)
					if errMsg != nil {
						cliCancel(errMsg.Error)
					} else {
						cliCancel(nil)
					}
					return
				}
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
				flusher.Flush()
				cliCancel(nil)
				return
			}

			// Success! Set headers.
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

			// Write the first chunk
			converted := convertChatCompletionsStreamChunkToCompletions(chunk)
			if converted != nil {
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(converted))
				flusher.Flush()
			}

			done := make(chan struct{})
			var doneOnce sync.Once
			stop := func() { doneOnce.Do(func() { close(done) }) }

			convertedChan := make(chan []byte)
			go func() {
				defer close(convertedChan)
				for {
					select {
					case <-done:
						return
					case chunk, ok := <-dataChan:
						if !ok {
							return
						}
						converted := convertChatCompletionsStreamChunkToCompletions(chunk)
						if converted == nil {
							continue
						}
						select {
						case <-done:
							return
						case convertedChan <- converted:
						}
					}
				}
			}()

			h.handleStreamResult(c, flusher, func(err error) {
				stop()
				cliCancel(err)
			}, convertedChan, errChan, nil)
			return
		}
	}
}
func (h *OpenAIAPIHandler) handleStreamResult(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, throttler *handlers.RequestThrottler) {
	// Build throttle delay callback
	var throttleDelay func(chunk []byte)
	if throttler != nil {
		cliCtx := c.Request.Context()
		throttleDelay = func(chunk []byte) {
			throttler.ThrottleChunk(cliCtx, chunk)
		}
	}

	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		ThrottleDelay: throttleDelay,
		WriteChunk: func(chunk []byte) {
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(chunk))
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			if errMsg == nil {
				return
			}
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			errText := http.StatusText(status)
			if errMsg.Error != nil && errMsg.Error.Error() != "" {
				errText = errMsg.Error.Error()
			}
			body := handlers.BuildErrorResponseBody(status, errText)
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(body))
		},
		WriteDone: func() {
			_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
		},
	})
}
```

## `sdk/api/handlers/openai/openai_responses_handlers.go`

SHA-256 (LF): `03e788b9aeb107d754ca1016aa4b6a226defbf74ed033a17ca374273bf41594c`

```go
// Package openai provides HTTP handlers for OpenAIResponses API endpoints.
// This package implements the OpenAIResponses-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAIResponses API requests to the appropriate backend format and
// convert responses back to OpenAIResponses-compatible format.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/optimize-multi-agent-v2"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func writeResponsesSSEChunk(w io.Writer, chunk []byte) {
	if w == nil || len(chunk) == 0 {
		return
	}
	if _, err := w.Write(chunk); err != nil {
		return
	}
	if bytes.HasSuffix(chunk, []byte("\n\n")) || bytes.HasSuffix(chunk, []byte("\r\n\r\n")) {
		return
	}
	suffix := []byte("\n\n")
	if bytes.HasSuffix(chunk, []byte("\r\n")) {
		suffix = []byte("\r\n")
	} else if bytes.HasSuffix(chunk, []byte("\n")) {
		suffix = []byte("\n")
	}
	if _, err := w.Write(suffix); err != nil {
		return
	}
}

type responsesSSEFramer struct {
	pending              []byte
	outputItems          map[int][]byte
	outputOrder          []int
	unindexedOutputItems [][]byte
	lastEvent            string
	terminalEvent        string
	terminalError        *interfaces.ErrorMessage
	failureEvent         string
	isCodexClient        bool
	dataFrames           int
}

func (f *responsesSSEFramer) WriteChunk(w io.Writer, chunk []byte) {
	if len(chunk) == 0 || f.terminalEvent != "" {
		return
	}
	if responsesSSEStartsNewDataFrame(f.pending, chunk) {
		f.writeFrame(w, f.pending)
		f.pending = f.pending[:0]
		if f.terminalEvent != "" {
			return
		}
	}
	if responsesSSENeedsLineBreak(f.pending, chunk) {
		f.pending = append(f.pending, '\n')
	}
	f.pending = append(f.pending, chunk...)
	for {
		frameLen := responsesSSEFrameLen(f.pending)
		if frameLen == 0 {
			break
		}
		f.writeFrame(w, f.pending[:frameLen])
		copy(f.pending, f.pending[frameLen:])
		f.pending = f.pending[:len(f.pending)-frameLen]
		if f.terminalEvent != "" {
			f.pending = f.pending[:0]
			return
		}
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return
	}
	if len(f.pending) == 0 || !responsesSSECanEmitWithoutDelimiter(f.pending) {
		return
	}
	f.writeFrame(w, f.pending)
	f.pending = f.pending[:0]
}

func (f *responsesSSEFramer) Flush(w io.Writer) {
	if len(f.pending) == 0 || f.terminalEvent != "" {
		return
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return
	}
	if !responsesSSECanFlushWithoutDelimiter(f.pending) {
		f.pending = f.pending[:0]
		return
	}
	f.writeFrame(w, f.pending)
	f.pending = f.pending[:0]
}

func (f *responsesSSEFramer) writeFrame(w io.Writer, frame []byte) {
	writeResponsesSSEChunk(w, f.repairFrame(frame))
}

func (f *responsesSSEFramer) shouldFilterPrivateEvent(streamEvent, payloadType string) bool {
	check := func(name string) bool {
		name = strings.TrimSpace(name)
		if name == "" {
			return false
		}
		if responsesSSEErrorEvent(name) {
			return false
		}

		// Always filter internal WebSocket timing telemetry from SSE streams.
		if strings.HasPrefix(name, "responsesapi.") {
			return true
		}

		// If official Codex client, preserve codex.response.metadata but filter rate limits.
		if f != nil && f.isCodexClient {
			if name == "codex.rate_limits" {
				return true
			}
			return false
		}

		// For standard Responses API clients: filter any codex.* private events.
		if strings.HasPrefix(name, "codex.") {
			return true
		}

		return false
	}

	return check(streamEvent) || check(payloadType)
}

func (f *responsesSSEFramer) repairFrame(frame []byte) []byte {
	payload, ok := responsesSSEDataPayload(frame)
	streamEvent := responsesSSEEventName(frame)
	if streamEvent != "" && f.shouldFilterPrivateEvent(streamEvent, "") {
		return nil
	}
	if !ok || len(payload) == 0 {
		return frame
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		f.dataFrames++
		return frame
	}
	if !json.Valid(payload) {
		return frame
	}

	payloadType := gjson.GetBytes(payload, "type").String()
	if f.shouldFilterPrivateEvent(streamEvent, payloadType) {
		return nil
	}

	f.dataFrames++

	if responsesSSEErrorEvent(payloadType) || responsesSSEPayloadHasError(payload) {
		if payloadType != "" {
			f.lastEvent = sanitizeResponsesStreamEventName(payloadType)
		}
		return f.repairErrorPayload(payload)
	}
	eventType := payloadType
	if responsesSSETerminalEvent(streamEvent) {
		eventType = streamEvent
	} else if eventType == "" {
		eventType = streamEvent
	}
	if eventType != "" {
		f.lastEvent = sanitizeResponsesStreamEventName(eventType)
	}
	if responsesSSEErrorEvent(eventType) {
		return f.repairErrorPayload(payload)
	}
	if responsesSSETerminalEvent(eventType) {
		f.terminalEvent = eventType
	}

	switch eventType {
	case "response.output_item.done":
		f.recordOutputItem(payload)
	case "response.completed":
		repaired := f.repairCompletedPayload(payload)
		if !bytes.Equal(repaired, payload) {
			return responsesSSEFrameWithData(frame, repaired)
		}
	}
	return frame
}

func responsesSSEPayloadErrorMessage(payload []byte) *interfaces.ErrorMessage {
	status := http.StatusBadGateway
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code", "response.error.status", "response.error.status_code"} {
		candidate := int(gjson.GetBytes(payload, path).Int())
		if candidate >= http.StatusBadRequest && candidate <= 599 {
			status = candidate
			break
		}
	}
	return sanitizeResponsesStreamErrorMessage(&interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", payload)})
}

func (f *responsesSSEFramer) repairErrorPayload(payload []byte) []byte {
	errMsg := responsesSSEPayloadErrorMessage(payload)
	status := errMsg.StatusCode
	f.terminalError = errMsg
	failureEvent := f.failureEvent
	if failureEvent != "response.failed" {
		failureEvent = "error"
	}
	f.terminalEvent = failureEvent
	errText := responsesStreamErrorText(errMsg, status)
	seq := 0
	if s := gjson.GetBytes(payload, "sequence_number"); s.Exists() {
		seq = int(s.Int())
	} else if origSeq := gjson.Get(errText, "sequence_number"); origSeq.Exists() {
		seq = int(origSeq.Int())
	} else if f != nil && f.dataFrames > 0 {
		seq = f.dataFrames - 1
	}
	if failureEvent == "response.failed" {
		chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, seq)
		return []byte(fmt.Sprintf("event: response.failed\ndata: %s\n\n", chunk))
	}
	chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, seq)
	return []byte(fmt.Sprintf("event: error\ndata: %s\n\n", chunk))
}

func responsesSSEErrorEvent(eventType string) bool {
	switch eventType {
	case "response.failed", "response.error", "error":
		return true
	default:
		return false
	}
}

func responsesSSETerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.done", "response.error", "error":
		return true
	default:
		return false
	}
}

func responsesSSEPayloadHasError(payload []byte) bool {
	for _, path := range []string{"error", "response.error"} {
		result := gjson.GetBytes(payload, path)
		if result.Exists() && result.Type != gjson.Null {
			return true
		}
	}
	return gjson.GetBytes(payload, "code").Exists() && gjson.GetBytes(payload, "message").Exists()
}

func responsesSSEDataPayload(frame []byte) ([]byte, bool) {
	var payload []byte
	found := false
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if found {
			payload = append(payload, '\n')
		}
		payload = append(payload, data...)
		found = true
	}
	return payload, found
}

func responsesSSEFrameWithData(frame, payload []byte) []byte {
	var out bytes.Buffer
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		out.WriteString("data: ")
		out.Write(line)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func (f *responsesSSEFramer) recordOutputItem(payload []byte) {
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() == "" {
		return
	}

	if outputIndex := gjson.GetBytes(payload, "output_index"); outputIndex.Exists() {
		index := int(outputIndex.Int())
		if f.outputItems == nil {
			f.outputItems = make(map[int][]byte)
		}
		if _, exists := f.outputItems[index]; !exists {
			f.outputOrder = append(f.outputOrder, index)
		}
		f.outputItems[index] = append([]byte(nil), item.Raw...)
		return
	}

	f.unindexedOutputItems = append(f.unindexedOutputItems, append([]byte(nil), item.Raw...))
}

func (f *responsesSSEFramer) repairCompletedPayload(payload []byte) []byte {
	if len(f.outputOrder) == 0 && len(f.unindexedOutputItems) == 0 {
		return payload
	}
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && (!output.IsArray() || len(output.Array()) > 0) {
		return payload
	}

	var outputJSON bytes.Buffer
	outputJSON.WriteByte('[')
	indexes := append([]int(nil), f.outputOrder...)
	sort.Ints(indexes)
	written := 0
	for _, index := range indexes {
		item, ok := f.outputItems[index]
		if !ok {
			continue
		}
		if written > 0 {
			outputJSON.WriteByte(',')
		}
		outputJSON.Write(item)
		written++
	}
	for _, item := range f.unindexedOutputItems {
		if written > 0 {
			outputJSON.WriteByte(',')
		}
		outputJSON.Write(item)
		written++
	}
	outputJSON.WriteByte(']')

	repaired, err := sjson.SetRawBytes(payload, "response.output", outputJSON.Bytes())
	if err != nil {
		return payload
	}
	return repaired
}

func responsesSSEFrameLen(chunk []byte) int {
	if len(chunk) == 0 {
		return 0
	}
	lf := bytes.Index(chunk, []byte("\n\n"))
	crlf := bytes.Index(chunk, []byte("\r\n\r\n"))
	switch {
	case lf < 0:
		if crlf < 0 {
			return 0
		}
		return crlf + 4
	case crlf < 0:
		return lf + 2
	case lf < crlf:
		return lf + 2
	default:
		return crlf + 4
	}
}

func responsesSSENeedsMoreData(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return false
	}
	return responsesSSEHasField(trimmed, []byte("event:")) && !responsesSSEHasField(trimmed, []byte("data:"))
}

func responsesSSEHasField(chunk []byte, prefix []byte) bool {
	s := chunk
	for len(s) > 0 {
		line := s
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line = s[:i]
			s = s[i+1:]
		} else {
			s = nil
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func responsesSSECanEmitWithoutDelimiter(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 || responsesSSENeedsMoreData(trimmed) ||
		!responsesSSEHasField(trimmed, []byte("event:")) || !responsesSSEHasField(trimmed, []byte("data:")) {
		return false
	}
	return responsesSSEDataLinesValid(trimmed)
}

func responsesSSECanFlushWithoutDelimiter(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	return len(trimmed) > 0 && responsesSSEHasField(trimmed, []byte("data:")) && responsesSSEDataLinesValid(trimmed)
}

func responsesSSEStartsNewDataFrame(pending, chunk []byte) bool {
	trimmedPending := bytes.TrimSpace(pending)
	if len(trimmedPending) == 0 || responsesSSEHasField(trimmedPending, []byte("event:")) ||
		!responsesSSEHasField(trimmedPending, []byte("data:")) || !responsesSSEDataLinesValid(trimmedPending) {
		return false
	}
	trimmedChunk := bytes.TrimLeft(chunk, " \t\r\n")
	return bytes.HasPrefix(trimmedChunk, []byte("data:"))
}

func responsesSSEEventName(frame []byte) string {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			return strings.TrimSpace(string(trimmed[len("event:"):]))
		}
	}
	return ""
}

func responsesSSEDataLinesValid(chunk []byte) bool {
	payload, found := responsesSSEDataPayload(chunk)
	if !found {
		return true
	}
	payload = bytes.TrimSpace(payload)
	return len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || json.Valid(payload)
}

func responsesSSENeedsLineBreak(pending, chunk []byte) bool {
	if len(pending) == 0 || len(chunk) == 0 {
		return false
	}
	if bytes.HasSuffix(pending, []byte("\n")) || bytes.HasSuffix(pending, []byte("\r")) {
		return false
	}
	if chunk[0] == '\n' || chunk[0] == '\r' {
		return false
	}
	trimmed := bytes.TrimLeft(chunk, " \t")
	if len(trimmed) == 0 {
		return false
	}
	for _, prefix := range [][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")} {
		if bytes.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// OpenAIResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type OpenAIResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
}

// NewOpenAIResponsesAPIHandler creates a new OpenAIResponses API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an OpenAIResponsesAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *OpenAIResponsesAPIHandler: A new OpenAIResponses API handlers instance
func NewOpenAIResponsesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler {
	return &OpenAIResponsesAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *OpenAIResponsesAPIHandler) HandlerType() string {
	return OpenaiResponse
}

// Models returns the OpenAIResponses-compatible model metadata supported by this handler.
func (h *OpenAIResponsesAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("openai")
}

// OpenAIResponsesModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAIResponses-compatible format.
func (h *OpenAIResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   h.Models(),
	})
}

func (h *OpenAIResponsesAPIHandler) prepareCodexMultiAgentV2Tools(c *gin.Context, payload []byte) []byte {
	if h == nil || h.Cfg == nil {
		return payload
	}

	requestCtx := context.Background()
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}
	requestCtx = context.WithValue(requestCtx, "gin", c)

	var requestHeaders http.Header
	if c != nil && c.Request != nil {
		requestHeaders = c.Request.Header
	}
	homeEnabled := h.AuthManager != nil && h.AuthManager.HomeEnabled()
	updated, prepared := multiagentv2.PrepareCodexMultiAgentV2Tools(
		requestCtx,
		requestHeaders,
		payload,
		h.Cfg.CodexOptimizeMultiAgentV2,
		homeEnabled,
	)
	if prepared && c != nil {
		c.Set(multiagentv2.CodexMultiAgentV2ToolsPreparedContextKey, true)
	}
	return updated
}

func (h *OpenAIResponsesAPIHandler) prepareCodexOrphanDelegation(c *gin.Context, payload []byte) []byte {
	if h == nil || h.Cfg == nil || !h.Cfg.CodexOrphanDelegationCompatibility {
		return payload
	}
	requestCtx := context.Background()
	var requestHeaders http.Header
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
		requestHeaders = c.Request.Header
	}
	requestCtx = context.WithValue(requestCtx, "gin", c)
	return multiagentv2.RewriteCodexOrphanDelegationInput(requestCtx, requestHeaders, payload, true)
}

// Responses handles the /v1/responses endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIResponsesAPIHandler) Responses(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	rawJSON = h.prepareCodexMultiAgentV2Tools(c, rawJSON)
	rawJSON = h.prepareCodexOrphanDelegation(c, rawJSON)

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		h.handleStreamingResponse(c, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, rawJSON)
	}

}

func (h *OpenAIResponsesAPIHandler) Compact(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	rawJSON = h.prepareCodexOrphanDelegation(c, rawJSON)

	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported for compact responses",
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if streamResult.Exists() {
		if updated, err := sjson.DeleteBytes(rawJSON, "stream"); err == nil {
			rawJSON = updated
		}
	}

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "responses/compact")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for Gemini models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAIResponses format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	requestStart := time.Now()
	throttler := handlers.NewRequestThrottler(h.Cfg)
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	if !throttler.ThrottleNonStreaming(cliCtx, requestStart, handlers.EstimateNonStreamingTokens(resp)) {
		cliCancel(cliCtx.Err())
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleStreamingResponse handles streaming responses for Gemini models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	// New core execution path
	modelName := gjson.GetBytes(rawJSON, "model").String()
	requestStart := time.Now()
	throttler := handlers.NewRequestThrottler(h.Cfg)
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	isCodexClient := isCodexResponsesClientRequest(c)
	failureEvent := "error"
	if isCodexClient {
		failureEvent = "response.failed"
	}
	framer := &responsesSSEFramer{failureEvent: failureEvent, isCodexClient: isCodexClient}
	var initialOutput bytes.Buffer

	// Peek at the first complete SSE data frame.
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				// Err channel closed cleanly; wait for data channel.
				errChan = nil
				continue
			}
			framer.Flush(&initialOutput)
			safeErrMsg := sanitizeResponsesStreamErrorMessage(errMsg)
			if framer.dataFrames == 0 {
				safeErrMsg = sanitizeResponsesInitialErrorMessage(errMsg)
			}
			if safeErrMsg != nil && framer.dataFrames > 0 {
				if !throttler.ThrottleFirstChunkWithPayload(cliCtx, requestStart, initialOutput.Bytes()) {
					cliCancel(cliCtx.Err())
					return
				}
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write(initialOutput.Bytes())
				flusher.Flush()
				pendingErrors := make(chan *interfaces.ErrorMessage, 1)
				pendingErrors <- safeErrMsg
				close(pendingErrors)
				h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, make(chan []byte), pendingErrors, framer, throttler)
				return
			}
			// Upstream failed before a complete SSE data frame. Return JSON.
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), safeErrMsg)
			h.WriteErrorResponse(c, safeErrMsg)
			if safeErrMsg != nil {
				cliCancel(safeErrMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				framer.Flush(&initialOutput)
				errMsg, hasPendingError := handlers.PendingStreamError(errChan)
				if !hasPendingError && framer.terminalEvent == "" {
					message := "upstream stream closed before first payload"
					if framer.dataFrames > 0 {
						message = "upstream stream closed before a terminal event"
					}
					errMsg = &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: fmt.Errorf("%s", message)}
				}
				if framer.dataFrames > 0 {
					errMsg = sanitizeResponsesStreamErrorMessage(errMsg)
				} else {
					errMsg = sanitizeResponsesInitialErrorMessage(errMsg)
				}

				if framer.dataFrames > 0 {
					if !throttler.ThrottleFirstChunkWithPayload(cliCtx, requestStart, initialOutput.Bytes()) {
						cliCancel(cliCtx.Err())
						return
					}
					setSSEHeaders()
					handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
					_, _ = c.Writer.Write(initialOutput.Bytes())
					flusher.Flush()
					if framer.terminalError != nil {
						h.logResponsesStreamError(c, framer, framer.terminalError)
						cliCancel(framer.terminalError.Error)
						return
					}
					if errMsg == nil {
						cliCancel(nil)
						return
					}
					pendingErrors := make(chan *interfaces.ErrorMessage, 1)
					pendingErrors <- errMsg
					close(pendingErrors)
					h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, make(chan []byte), pendingErrors, framer, throttler)
					return
				}

				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
				h.WriteErrorResponse(c, errMsg)
				if errMsg != nil {
					cliCancel(errMsg.Error)
				} else {
					cliCancel(nil)
				}
				return
			}

			framer.WriteChunk(&initialOutput, chunk)
			if framer.dataFrames == 0 {
				continue
			}

			if !throttler.ThrottleFirstChunkWithPayload(cliCtx, requestStart, initialOutput.Bytes()) {
				cliCancel(cliCtx.Err())
				return
			}
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			_, _ = c.Writer.Write(initialOutput.Bytes())
			flusher.Flush()
			if framer.terminalError != nil {
				h.logResponsesStreamError(c, framer, framer.terminalError)
				cliCancel(framer.terminalError.Error)
				return
			}

			h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, framer, throttler)
			return
		}
	}
}

// isCodexResponsesClientRequest limits the alternate terminal event to official Codex clients.
func isCodexResponsesClientRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if multiagentv2.IsCodexClientUserAgent(c.GetHeader("User-Agent")) {
		return true
	}

	switch originator := strings.ToLower(strings.TrimSpace(c.GetHeader("Originator"))); originator {
	case "codex desktop", "codex-tui", "codex_cli_rs":
		return true
	default:
		return strings.HasPrefix(originator, "codex desktop/") || strings.HasPrefix(originator, "codex-tui/") || strings.HasPrefix(originator, "codex_cli_rs/")
	}
}

const (
	responsesStreamErrorMessageLimit = 2048
	responsesStreamErrorFieldLimit   = 256
)

var (
	responsesStreamSensitiveValuePattern = regexp.MustCompile(`(?i)((?:"?(?:api[_-]?key|access[_-]?token|token|authorization|secret)"?)\s*[=:]\s*"?)([^\s"&,;}]+)`)
	responsesStreamBearerPattern         = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

func truncateResponsesStreamErrorText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func redactResponsesStreamErrorText(text string) string {
	text = responsesStreamSensitiveValuePattern.ReplaceAllString(text, `${1}[REDACTED]`)
	return responsesStreamBearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
}

func sanitizeResponsesStreamEventName(eventName string) string {
	return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(strings.TrimSpace(eventName)), responsesStreamErrorFieldLimit)
}

func isResponsesStreamSensitiveKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.ReplaceAll(k, "-", "_")
	if strings.Contains(k, "tokens") || strings.Contains(k, "token_count") || strings.Contains(k, "token_limit") || strings.Contains(k, "token_usage") {
		return false
	}
	switch k {
	case "authorization", "secret", "password", "passwd", "api_key", "apikey", "token", "access_token", "refresh_token", "id_token", "auth_token", "session_token", "api_token", "client_secret", "client_key":
		return true
	}
	return strings.HasSuffix(k, "_secret") ||
		strings.HasSuffix(k, "_password") ||
		strings.HasSuffix(k, "_api_key") ||
		strings.HasSuffix(k, "_token")
}

func sanitizeResponsesStreamErrorNode(val any) any {
	switch v := val.(type) {
	case string:
		return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(v), responsesStreamErrorMessageLimit)
	case map[string]any:
		cleaned := make(map[string]any, len(v))
		for k, item := range v {
			if isResponsesStreamSensitiveKey(k) {
				cleaned[k] = "[REDACTED]"
				continue
			}
			cleaned[k] = sanitizeResponsesStreamErrorNode(item)
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(v))
		for i, item := range v {
			cleaned[i] = sanitizeResponsesStreamErrorNode(item)
		}
		return cleaned
	default:
		return val
	}
}

func responsesStreamErrorText(errMsg *interfaces.ErrorMessage, status int) string {
	text := http.StatusText(status)
	if errMsg != nil && errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
		text = strings.TrimSpace(errMsg.Error.Error())
	}
	trimmed := strings.TrimSpace(text)
	if !json.Valid([]byte(trimmed)) {
		return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(trimmed), responsesStreamErrorMessageLimit)
	}

	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	dec.UseNumber()
	if errUnmarshal := dec.Decode(&root); errUnmarshal != nil {
		return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(trimmed), responsesStreamErrorMessageLimit)
	}

	errorNode, hasError := root["error"].(map[string]any)
	if !hasError {
		if resp, ok := root["response"].(map[string]any); ok {
			errorNode, hasError = resp["error"].(map[string]any)
		}
	}

	if hasError {
		cleanedError := sanitizeResponsesStreamErrorNode(errorNode)
		out := map[string]any{
			"error": cleanedError,
		}
		if seq, ok := root["sequence_number"]; ok {
			out["sequence_number"] = seq
		}
		data, errMarshal := json.Marshal(out)
		if errMarshal == nil {
			return string(data)
		}
	}

	cleanedRoot := sanitizeResponsesStreamErrorNode(root)
	data, errMarshal := json.Marshal(cleanedRoot)
	if errMarshal == nil {
		return string(data)
	}
	return http.StatusText(status)
}

type responsesStreamSanitizedError struct {
	message string
	cause   error
}

func (e *responsesStreamSanitizedError) Error() string { return e.message }
func (e *responsesStreamSanitizedError) Unwrap() error { return e.cause }

func sanitizeResponsesInitialErrorMessage(errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if errMsg != nil && errMsg.DirectResponse {
		return errMsg
	}
	return sanitizeResponsesStreamErrorMessage(errMsg)
}

func sanitizeResponsesStreamErrorMessage(errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if errMsg == nil {
		return nil
	}
	status := errMsg.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusInternalServerError
	}
	safe := *errMsg
	safe.StatusCode = status
	safe.Error = &responsesStreamSanitizedError{message: responsesStreamErrorText(errMsg, status), cause: errMsg.Error}
	safe.DirectResponse = false
	safe.Body = nil
	return &safe
}

func (h *OpenAIResponsesAPIHandler) logResponsesStreamError(c *gin.Context, framer *responsesSSEFramer, errMsg *interfaces.ErrorMessage) {
	if errMsg == nil {
		return
	}
	status := errMsg.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusInternalServerError
	}
	lastEvent := "none"
	if framer != nil && framer.lastEvent != "" {
		lastEvent = framer.lastEvent
	}
	errText := responsesStreamErrorText(errMsg, status)
	h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), &interfaces.ErrorMessage{
		StatusCode: status,
		Error:      fmt.Errorf("responses stream terminated after %s: %s", lastEvent, errText),
	})
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, framer *responsesSSEFramer, throttler *handlers.RequestThrottler) {
	if framer == nil {
		framer = &responsesSSEFramer{}
	}
	if isCodexResponsesClientRequest(c) {
		framer.failureEvent = "response.failed"
		framer.isCodexClient = true
	} else {
		framer.failureEvent = "error"
		framer.isCodexClient = false
	}
	writeTerminalError := func(errMsg *interfaces.ErrorMessage) {
		framer.Flush(c.Writer)
		if errMsg == nil {
			return
		}
		status := http.StatusInternalServerError
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
		}
		errText := responsesStreamErrorText(errMsg, status)
		h.logResponsesStreamError(c, framer, errMsg)
		if framer.terminalEvent != "" {
			return
		}
		seq := 0
		if framer != nil {
			seq = framer.dataFrames
		}
		if origSeq := gjson.Get(errText, "sequence_number"); origSeq.Exists() {
			seq = int(origSeq.Int())
		}
		if isCodexResponsesClientRequest(c) {
			chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, seq)
			_, _ = fmt.Fprintf(c.Writer, "\nevent: response.failed\ndata: %s\n\n", string(chunk))
			return
		}
		chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, seq)
		_, _ = fmt.Fprintf(c.Writer, "\nevent: error\ndata: %s\n\n", string(chunk))
	}

	var throttleDelay func([]byte)
	if throttler != nil {
		throttleDelay = func(chunk []byte) {
			_ = throttler.ThrottleChunk(c.Request.Context(), chunk)
		}
	}

	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		ThrottleDelay:          throttleDelay,
		NormalizeTerminalError: sanitizeResponsesStreamErrorMessage,
		WriteChunk: func(chunk []byte) {
			framer.WriteChunk(c.Writer, chunk)
		},
		ChunkError: func() *interfaces.ErrorMessage {
			if framer.terminalError != nil {
				h.logResponsesStreamError(c, framer, framer.terminalError)
			}
			return framer.terminalError
		},
		WriteTerminalError: writeTerminalError,
		CloseError: func() *interfaces.ErrorMessage {
			framer.Flush(c.Writer)
			if framer.terminalError != nil {
				return framer.terminalError
			}
			if framer.terminalEvent != "" {
				return nil
			}
			lastEvent := framer.lastEvent
			if lastEvent == "" {
				lastEvent = "none"
			}
			return &interfaces.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error:      fmt.Errorf("upstream stream closed before a terminal event (last event: %s)", lastEvent),
			}
		},
		WriteDone: func() {
			framer.Flush(c.Writer)
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}
```

## `sdk/cliproxy/auth/conductor_stream.go`

SHA-256 (LF): `e198fd411f68a362a774a1a6b01c9c28f5da7dffec9937818acc6321380c550f`

```go
package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	upstreamAttempt := hasUpstreamExecutionAttempt(err)
	err = unwrapUpstreamExecutionAttempt(err)
	bootstrapErr := &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
	if upstreamAttempt {
		return markUpstreamExecutionAttempt(bootstrapErr)
	}
	return bootstrapErr
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func validateStreamResult(result *cliproxyexecutor.StreamResult, err error) (*cliproxyexecutor.StreamResult, error) {
	if err != nil {
		return result, err
	}
	if result == nil || result.Chunks == nil {
		return result, &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
	}
	return result, nil
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, aliasResult OAuthModelAliasResult, ephemeralResult bool, opts cliproxyexecutor.Options) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	streamStart := time.Now()
	go func() {
		defer close(out)
		var failed bool
		forward := true
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				entry := logEntryWithRequestID(ctx)
				warnLogUpstreamFailure(ctx, entry, provider, resultModel, auth, time.Since(streamStart), chunk.Err)
				rerr := resultErrorFromError(chunk.Err)
				action, okAction := matchRequestScopedErrorAction(auth, chunk.Err, m.runtimeConfigSnapshot())
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: opts}
				result.RetryAfter = retryAfterFromError(chunk.Err)
				result.CredentialScope = isCredentialScopedError(chunk.Err)
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			}
			if !forward {
				return false
			}
			if chunk.Err != nil {
				if ctx == nil {
					out <- chunk
					return true
				}
				select {
				case <-ctx.Done():
					forward = false
					return false
				case out <- chunk:
					return true
				}
			}
			if len(chunk.Payload) == 0 {
				return true
			}
			payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
			if len(payload) == 0 {
				return true
			}
			chunk.Payload = payload
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			tailChunk := cliproxyexecutor.StreamChunk{Payload: tail}
			if !emit(tailChunk) {
				return
			}
		}
		if !failed && (ephemeralResult || claudeOAuthRequestCancellation(ctx, auth, nil) == nil) {
			m.recordExecutionResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: true, Options: opts}, auth, ephemeralResult)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool, aliasResult OAuthModelAliasResult, routing *apiKeyModelRoutingSnapshot, allowRetry bool, ephemeralResult bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	var lastErr error
	var upstreamErr error
	didRefreshOnUnauthorized := false
	for idx, execModel := range execModels {
		ctx = newUpstreamAttemptContext(ctx)
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execOpts := opts
		var errIntercept error
		execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(ctx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		if errIntercept != nil {
			return nil, errIntercept
		}
		if executionModel == "" {
			execReq = attachResolvedAPIKeyModelInfo(routing, execReq, auth, routeModel, execModel)
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		entry := logEntryWithRequestID(ctx)
		payload := execOpts.OriginalRequest
		if len(payload) == 0 {
			payload = execReq.Payload
		}
		execOpts.Metadata = ensureCanonicalSessionMetadata(execOpts.Metadata, execOpts.Headers, payload)
		ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
		startStream := time.Now()
		streamResult, errStream := executor.ExecuteStream(diagnostics.ExecutorAttempt(ctx, executor.Identifier()), auth, execReq, execOpts)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if hasUpstreamExecutionAttempt(errStream) {
			upstreamErr = errStream
		}
		durationStream := time.Since(startStream)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if allowRetry && !ephemeralResult {
				alreadyTried := didRefreshOnUnauthorized
				refreshed, okRefresh := m.tryRefreshAfterUnauthorized(newUpstreamAttemptContext(ctx), auth, errStream, alreadyTried)
				if okRefresh {
					auth = refreshed
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					ctx = newUpstreamAttemptContext(ctx)
					ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
					startRetry := time.Now()
					streamResult, errStream = executor.ExecuteStream(diagnostics.ExecutorAttempt(ctx, executor.Identifier()), auth, execReq, execOpts)
					errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
					if hasUpstreamExecutionAttempt(errStream) {
						upstreamErr = errStream
					}
					durationRetry := time.Since(startRetry)
					if errStream != nil {
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationRetry, errStream)
						if errCtx := ctx.Err(); errCtx != nil {
							return nil, errCtx
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, errStream); errCancel != nil {
				return nil, errCancel
			}
		}
		streamResult, errStream = validateStreamResult(streamResult, errStream)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if errStream != nil {
			rerr := resultErrorFromError(errStream)
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
			result.RetryAfter = retryAfterFromError(errStream)
			if isCredentialScopedError(errStream) {
				result.CredentialScope = true
			}
			applyRequestScopedActionToResult(action, okAction, &result)
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream)
				}
				lastErr = errStream
				if result.CredentialScope {
					return nil, preferredExecutionAttemptError(errStream, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			if result.CredentialScope {
				return nil, preferredExecutionAttemptError(errStream, upstreamErr)
			}
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
		if hasUpstreamExecutionAttempt(bootstrapErr) {
			upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCtx
			}
			if allowRetry && !ephemeralResult {
				alreadyTried := didRefreshOnUnauthorized
				refreshed, okRefresh := m.tryRefreshAfterUnauthorized(newUpstreamAttemptContext(ctx), auth, bootstrapErr, alreadyTried)
				if okRefresh {
					discardStreamChunks(streamResult.Chunks)
					auth = refreshed
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					ctx = newUpstreamAttemptContext(ctx)
					startRetry := time.Now()
					retryStream, retryErr := executor.ExecuteStream(diagnostics.ExecutorAttempt(ctx, executor.Identifier()), auth, execReq, execOpts)
					retryErr = markUpstreamExecutionAttemptFromContext(ctx, retryErr)
					retryStream, retryErr = validateStreamResult(retryStream, retryErr)
					retryErr = markUpstreamExecutionAttemptFromContext(ctx, retryErr)
					if retryErr != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							return nil, errCtx
						}
						bootstrapErr = retryErr
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						streamResult = &cliproxyexecutor.StreamResult{}
					} else {
						streamResult = retryStream
						buffered, closed, bootstrapErr = readStreamBootstrap(ctx, streamResult.Chunks)
						bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
						if bootstrapErr != nil {
							warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
			}
			if hasUpstreamExecutionAttempt(bootstrapErr) {
				upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, bootstrapErr); errCancel != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCancel
			}
		}
		if bootstrapErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, bootstrapErr, m.runtimeConfigSnapshot())
			if okAction {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				discardStreamChunks(streamResult.Chunks)
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(bootstrapErr)
				}
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			if isCredentialScopedError(bootstrapErr) {
				result.CredentialScope = true
			}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			discardStreamChunks(streamResult.Chunks)
			currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}

		if closed && len(buffered) == 0 {
			emptyErr := markUpstreamExecutionAttemptFromContext(ctx, &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true})
			currentErr := newStreamBootstrapError(emptyErr, streamResult.Headers)
			if hasUpstreamExecutionAttempt(emptyErr) {
				upstreamErr = currentErr
			}
			warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), emptyErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(emptyErr), Options: execOpts}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, execModel, aliasResult)
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, routeModel, streamResult.Headers, buffered, remaining, attemptAliasResult, ephemeralResult, execOpts), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, preferredExecutionAttemptError(lastErr, upstreamErr)
}
```

## `sdk/cliproxy/auth/conductor_execution.go`

SHA-256 (LF): `ef0b6a6809914229306aa21f7e5686544f6d53d5bb5ad92813e97ffbb1717670`

```go
package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

func newUpstreamAttemptContext(ctx context.Context) context.Context {
	ctx = logging.WithFreshResponseHeadersHolder(ctx)
	return cliproxyexecutor.WithUpstreamAttemptTracker(ctx)
}

func claudeOAuthRequestCancellation(ctx context.Context, auth *Auth, err error) error {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || !strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "oauth") {
		return nil
	}
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

type upstreamExecutionAttemptError struct {
	cause error
}

func (e *upstreamExecutionAttemptError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *upstreamExecutionAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func markUpstreamExecutionAttempt(err error) error {
	if err == nil {
		return nil
	}
	if hasUpstreamExecutionAttempt(err) {
		return err
	}
	return &upstreamExecutionAttemptError{cause: err}
}

func markUpstreamExecutionAttemptFromContext(ctx context.Context, err error) error {
	if err == nil || !cliproxyexecutor.UpstreamAttempted(ctx) {
		return err
	}
	return markUpstreamExecutionAttempt(err)
}

func hasUpstreamExecutionAttempt(err error) bool {
	var marked *upstreamExecutionAttemptError
	return errors.As(err, &marked) && marked != nil
}

func unwrapUpstreamExecutionAttempt(err error) error {
	marked, ok := err.(*upstreamExecutionAttemptError)
	if !ok || marked == nil || marked.cause == nil {
		return err
	}
	return marked.cause
}

func unwrapExecutionBoundaryError(err error) error {
	err = unwrapRequestStopError(err)
	return unwrapUpstreamExecutionAttempt(err)
}

func preferredExecutionAttemptError(fallback, upstream error) error {
	if errors.Is(fallback, context.Canceled) || errors.Is(fallback, context.DeadlineExceeded) {
		return fallback
	}
	if upstream == nil {
		return fallback
	}
	var exhausted *homeRetryRoundExhaustedError
	if errors.As(fallback, &exhausted) && exhausted != nil {
		upstream = unwrapUpstreamExecutionAttempt(upstream)
		var previousRound *homeRetryRoundExhaustedError
		if errors.As(upstream, &previousRound) && previousRound != nil && previousRound.cause != nil {
			upstream = previousRound.cause
		}
		// Keep the current Home round marker because it owns authoritative retry timing.
		preferred := *exhausted
		preferred.cause = upstream
		return markUpstreamExecutionAttempt(&preferred)
	}
	return markUpstreamExecutionAttempt(upstream)
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	req, opts = cliproxysession.Enrich(req, opts)
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if m.HomeEnabled() {
		resp, errHome := m.executeHome(ctx, normalized, req, opts, false)
		return resp, unwrapExecutionBoundaryError(errHome)
	}

	defaultRequestRetry, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	var preferredUpstreamErr error
	retryModel := authSelectionModelFromOptions(opts, req.Model)
	for attempt := 0; ; attempt++ {
		roundAttempted := make(map[string]struct{})
		roundOpts := withAttemptedAuthTracker(opts, roundAttempted)
		resp, errExec := m.executeMixedOnce(ctx, normalized, req, roundOpts, maxRetryCredentials, attempt, defaultRequestRetry)
		if errExec == nil {
			return resp, nil
		}
		if isRequestTerminatedError(errExec) || isRequestStopError(errExec) {
			return cliproxyexecutor.Response{}, unwrapExecutionBoundaryError(errExec)
		}
		if hasUpstreamExecutionAttempt(errExec) {
			preferredUpstreamErr = errExec
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterErrorWithAttempted(ctx, opts, errExec, attempt, normalized, retryModel, maxWait, -1, defaultRequestRetry, roundAttempted)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		if ctx != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return cliproxyexecutor.Response{}, errCtx
			}
		}
		lastErr = preferredExecutionAttemptError(lastErr, preferredUpstreamErr)
		lastErr = unwrapExecutionBoundaryError(lastErr)
		if hasAntigravityProvider(normalized) && shouldAttemptAntigravityCreditsFallback(m, lastErr, normalized) {
			if resp, ok, errCredits := m.tryAntigravityCreditsExecute(ctx, req, opts); errCredits != nil {
				return cliproxyexecutor.Response{}, errCredits
			} else if ok {
				return resp, nil
			}
		}
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	req, opts = cliproxysession.Enrich(req, opts)
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if m.HomeEnabled() {
		resp, errHome := m.executeHome(ctx, normalized, req, opts, true)
		return resp, unwrapExecutionBoundaryError(errHome)
	}

	defaultRequestRetry, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	var preferredUpstreamErr error
	retryModel := authSelectionModelFromOptions(opts, req.Model)
	for attempt := 0; ; attempt++ {
		roundAttempted := make(map[string]struct{})
		roundOpts := withAttemptedAuthTracker(opts, roundAttempted)
		resp, errExec := m.executeCountMixedOnce(ctx, normalized, req, roundOpts, maxRetryCredentials, attempt, defaultRequestRetry)
		if errExec == nil {
			return resp, nil
		}
		if isRequestTerminatedError(errExec) || isRequestStopError(errExec) {
			return cliproxyexecutor.Response{}, unwrapExecutionBoundaryError(errExec)
		}
		if hasUpstreamExecutionAttempt(errExec) {
			preferredUpstreamErr = errExec
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterErrorWithAttempted(ctx, opts, errExec, attempt, normalized, retryModel, maxWait, -1, defaultRequestRetry, roundAttempted)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		if ctx != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return cliproxyexecutor.Response{}, errCtx
			}
		}
		lastErr = preferredExecutionAttemptError(lastErr, preferredUpstreamErr)
		return cliproxyexecutor.Response{}, unwrapExecutionBoundaryError(lastErr)
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	req, opts = cliproxysession.Enrich(req, opts)
	if m.HomeEnabled() {
		if unlockSession := m.lockHomeWebsocketSession(ctx, opts); unlockSession != nil {
			defer unlockSession()
		}
	}
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	defaultRequestRetry, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	var preferredUpstreamErr error
	homeRetryLimit := -1
	retryModel := authSelectionModelFromOptions(opts, req.Model)
	attempt := 0
	retryRoundPending := false
	retryRoundWaited := false
	for {
		roundAttempted := make(map[string]struct{})
		roundOpts := withAttemptedAuthTracker(opts, roundAttempted)
		result, errStream := m.executeStreamMixedOnce(ctx, normalized, req, roundOpts, maxRetryCredentials, &homeRetryLimit, attempt, defaultRequestRetry)
		if errStream == nil {
			return result, nil
		}
		if hasUpstreamExecutionAttempt(errStream) {
			preferredUpstreamErr = errStream
		}
		if m.HomeEnabled() && retryRoundPending {
			if wait, okWait := pendingHomeRetryRoundDelay(errStream, maxWait, &homeRetryLimit, pinnedAuthIDFromMetadata(opts.Metadata) == ""); okWait && m.homeRetryAllowed(attempt-1, homeRetryLimit) {
				if retryRoundWaited {
					return nil, unwrapExecutionBoundaryError(errStream)
				}
				if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
					return nil, errWait
				}
				retryRoundWaited = true
				continue
			}
		}
		retryRoundPending = false
		retryRoundWaited = false
		if isRequestTerminatedError(errStream) || isRequestStopError(errStream) {
			return nil, unwrapExecutionBoundaryError(errStream)
		}
		lastErr = errStream
		wait, shouldRetry := m.shouldRetryAfterErrorWithAttempted(ctx, opts, errStream, attempt, normalized, retryModel, maxWait, homeRetryLimit, defaultRequestRetry, roundAttempted)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return nil, errWait
		}
		attempt++
		retryRoundPending = m.HomeEnabled()
		retryRoundWaited = false
	}
	if lastErr != nil {
		if ctx != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
		}
		if preferredUpstreamErr != nil && (!m.HomeEnabled() || isHomeRetryRoundExhausted(lastErr)) {
			lastErr = preferredExecutionAttemptError(lastErr, preferredUpstreamErr)
		}
		lastErr = unwrapExecutionBoundaryError(lastErr)
		if hasAntigravityProvider(normalized) && shouldAttemptAntigravityCreditsFallback(m, lastErr, normalized) {
			if result, ok, errCredits := m.tryAntigravityCreditsExecuteStream(ctx, req, opts); errCredits != nil {
				return nil, errCredits
			} else if ok {
				return result, nil
			}
		}
		var bootstrapErr *streamBootstrapError
		if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
			return streamErrorResult(bootstrapErr.Headers(), lastErr), nil
		}
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
}

type requestToFormatResolver interface {
	RequestToFormat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format
}

func isRequestTerminatedError(err error) bool {
	var terminated *cliproxyexecutor.RequestTerminatedError
	return errors.As(err, &terminated) && terminated != nil
}

func applyRequestAfterAuthInterceptor(ctx context.Context, executor ProviderExecutor, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, requestedModel string) (cliproxyexecutor.Request, cliproxyexecutor.Options, error) {
	if opts.RequestAfterAuthInterceptor == nil {
		return req, opts, nil
	}
	toFormat := requestToFormat(provider, executor, req, opts)
	resp := opts.RequestAfterAuthInterceptor(ctx, cliproxyexecutor.RequestAfterAuthInterceptRequest{
		SourceFormat:   opts.SourceFormat,
		ToFormat:       toFormat,
		Model:          req.Model,
		RequestedModel: requestedModel,
		Stream:         opts.Stream,
		Headers:        cloneRequestHeaders(opts.Headers),
		Body:           bytes.Clone(req.Payload),
		Metadata:       opts.Metadata,
	})
	opts.Headers = mergeRequestHeaders(opts.Headers, resp.Headers, resp.ClearHeaders)
	if len(resp.Body) > 0 {
		req.Payload = bytes.Clone(resp.Body)
		opts.OriginalRequest = bytes.Clone(resp.Body)
	}
	if resp.Terminate {
		return req, opts, &cliproxyexecutor.RequestTerminatedError{
			HTTPStatus: resp.StatusCode,
			Header:     cloneRequestHeaders(resp.ResponseHeaders),
			Body:       bytes.Clone(resp.ResponseBody),
		}
	}
	if len(resp.ClearHeaders) > 0 || len(resp.Body) > 0 {
		evalPayload := opts.OriginalRequest
		if len(evalPayload) == 0 {
			evalPayload = req.Payload
		}
		info, ok := cliproxysession.ExtractSessionInfo(opts.Headers, evalPayload, opts.Metadata)
		if ok && info.SessionID != "" {
			if opts.Metadata == nil {
				opts.Metadata = make(map[string]any, 2)
			}
			opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = cliproxysession.BoundSessionIdentity(info.SessionID)
			if info.ParentSessionID != "" && info.ParentSessionID != info.SessionID {
				opts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = cliproxysession.BoundSessionIdentity(info.ParentSessionID)
			} else {
				delete(opts.Metadata, cliproxyexecutor.ParentSessionIDMetadataKey)
			}
		} else {
			delete(opts.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey)
			delete(opts.Metadata, cliproxyexecutor.ParentSessionIDMetadataKey)
			delete(opts.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey)
		}
	} else if len(resp.Headers) > 0 {
		if info, ok := cliproxysession.ExtractSessionInfo(opts.Headers, nil, opts.Metadata); ok && info.SessionID != "" {
			if opts.Metadata == nil {
				opts.Metadata = make(map[string]any, 2)
			}
			opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] = cliproxysession.BoundSessionIdentity(info.SessionID)
			if info.ParentSessionID != "" && info.ParentSessionID != info.SessionID {
				opts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey] = cliproxysession.BoundSessionIdentity(info.ParentSessionID)
			}
		}
	}
	return req, opts, nil
}

func requestToFormat(provider string, executor ProviderExecutor, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	resolver, ok := executor.(requestToFormatResolver)
	if ok && resolver != nil {
		formatRequestTo := resolver.RequestToFormat(req, opts)
		if formatRequestTo != "" {
			return formatRequestTo
		}
	}
	source := opts.SourceFormat.String()
	if source == "openai-image" || source == "openai-video" {
		return opts.SourceFormat
	}
	if opts.Alt == "responses/compact" && !opts.Stream {
		return sdktranslator.FormatOpenAIResponse
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return sdktranslator.FormatCodex
	case "xai":
		return sdktranslator.FormatCodex
	case "claude":
		return sdktranslator.FormatClaude
	case "gemini", "vertex", "aistudio":
		return sdktranslator.FormatGemini
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		return sdktranslator.FormatOpenAI
	case "meta":
		return sdktranslator.FormatCodex
	case "antigravity":
		return sdktranslator.FormatAntigravity
	case "devin":
		return sdktranslator.FormatInteractions
	default:
		return sdktranslator.FormatOpenAI
	}
}

func cloneRequestHeaders(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func mergeRequestHeaders(current, updates http.Header, clear []string) http.Header {
	if updates == nil && len(clear) == 0 {
		return current
	}
	out := cloneRequestHeaders(current)
	if out == nil && (len(updates) > 0 || len(clear) > 0) {
		out = make(http.Header)
	}
	for _, key := range clear {
		out.Del(key)
		for existingKey := range out {
			if strings.EqualFold(existingKey, key) {
				delete(out, existingKey)
			}
		}
	}
	for key, values := range updates {
		out.Del(key)
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, retryRound int, defaultRequestRetry int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := authSelectionModelFromOptions(opts, req.Model)
	executionModel, restoreExecutionModel := executionModelForAuthSelection(opts, req.Model)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{})
	if !homeMode {
		for authID := range m.requestRetryRoundExclusions(retryRound, defaultRequestRetry) {
			tried[authID] = struct{}{}
		}
	}
	attempted := make(map[string]struct{})
	var lastErr error
	var upstreamErr error
	for {
		if maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, preferredExecutionAttemptError(lastErr, upstreamErr)
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeRetryRound(pickOpts, retryRound)
			pickOpts = withHomeAuthCount(pickOpts, homeAuthCount)
			pickOpts = withHomeExcludedAuthIDs(pickOpts, tried)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return cliproxyexecutor.Response{}, preferredExecutionAttemptError(lastErr, upstreamErr)
			}
			return cliproxyexecutor.Response{}, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, routeModel)
		publishSelectedAuthMetadata(opts.Metadata, auth)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)
		execCtx = newUpstreamAttemptContext(execCtx)

		models, pooled, aliasResult, routing := m.preparedExecutionModelsWithAlias(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			if errCancel := claudeOAuthRequestCancellation(execCtx, auth, errPrepare); errCancel != nil {
				return cliproxyexecutor.Response{}, errCancel
			}
			stateModel := m.selectionModelKeyForAuth(auth, routeModel)
			if stateModel == "" {
				stateModel = canonicalModelKey(routeModel)
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: stateModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(errPrepare), Options: pickOpts}
			m.MarkResult(execCtx, result)
			lastErr = errPrepare
			continue
		}
		var authErr error
		didRefreshOnUnauthorized := false
		for _, upstreamModel := range models {
			execCtx = newUpstreamAttemptContext(execCtx)
			resultModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			if restoreExecutionModel {
				execReq.Model = executionModel
			}
			execOpts := opts
			if pickOpts.Metadata != nil {
				if canonicalID, ok := pickOpts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; ok {
					meta := make(map[string]any, len(execOpts.Metadata)+2)
					for k, v := range execOpts.Metadata {
						meta[k] = v
					}
					meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = canonicalID
					if parentID, okParent := pickOpts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; okParent && parentID != canonicalID {
						meta[cliproxyexecutor.ParentSessionIDMetadataKey] = parentID
					} else {
						delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
					}
					execOpts.Metadata = meta
				}
			}
			payload := execOpts.OriginalRequest
			if len(payload) == 0 {
				payload = execReq.Payload
			}
			execOpts.Metadata = ensureCanonicalSessionMetadata(execOpts.Metadata, execOpts.Headers, payload)
			var errIntercept error
			execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(execCtx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
			if errIntercept != nil {
				return cliproxyexecutor.Response{}, errIntercept
			}
			if !restoreExecutionModel {
				execReq = attachResolvedAPIKeyModelInfo(routing, execReq, auth, routeModel, upstreamModel)
			}
			execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
			startExec := time.Now()
			resp, errExec := executor.Execute(diagnostics.ExecutorAttempt(execCtx, executor.Identifier()), auth, execReq, execOpts)
			errExec = markUpstreamExecutionAttemptFromContext(execCtx, errExec)
			durationExec := time.Since(startExec)
			if errExec != nil {
				if hasUpstreamExecutionAttempt(errExec) {
					upstreamErr = errExec
				}
				if errCtx := execCtx.Err(); errCtx != nil {
					return cliproxyexecutor.Response{}, errCtx
				}
				refreshCtx := newUpstreamAttemptContext(execCtx)
				if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(refreshCtx, auth, errExec, didRefreshOnUnauthorized); okRefresh {
					auth = refreshed
					didRefreshOnUnauthorized = true
					execCtx = newUpstreamAttemptContext(execCtx)
					execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
					startRetry := time.Now()
					resp, errExec = executor.Execute(diagnostics.ExecutorAttempt(execCtx, executor.Identifier()), auth, execReq, execOpts)
					errExec = markUpstreamExecutionAttemptFromContext(execCtx, errExec)
					durationRetry := time.Since(startRetry)
					if errExec != nil {
						if hasUpstreamExecutionAttempt(errExec) {
							upstreamErr = errExec
						}
						warnLogUpstreamFailure(execCtx, entry, provider, upstreamModel, auth, durationRetry, errExec)
						if errCtx := execCtx.Err(); errCtx != nil {
							return cliproxyexecutor.Response{}, errCtx
						}
					}
				} else {
					warnLogUpstreamFailure(execCtx, entry, provider, upstreamModel, auth, durationExec, errExec)
				}
			}
			if errCancel := claudeOAuthRequestCancellation(execCtx, auth, errExec); errCancel != nil {
				return cliproxyexecutor.Response{}, errCancel
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: errExec == nil, Options: execOpts}
			if errExec != nil {
				result.Error = resultErrorFromError(errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				if isCredentialScopedError(errExec) {
					result.CredentialScope = true
				}
				action, okAction := matchRequestScopedErrorAction(auth, errExec, m.runtimeConfigSnapshot())
				applyRequestScopedActionToResult(action, okAction, &result)
				if isResponsesCompactAvailabilityNeutralError(execOpts, errExec, result.Error) {
					m.recordAvailabilityNeutralResult(execCtx, result)
				} else {
					m.MarkResult(execCtx, result)
				}
				if okAction {
					if isRequestScopedStop(action, okAction) {
						return cliproxyexecutor.Response{}, wrapRequestStopError(errExec)
					}
					authErr = errExec
					if result.CredentialScope {
						break
					}
					continue
				}
				if isResponsesCompactRequestFaultError(execOpts, errExec) || isRequestInvalidError(errExec) {
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				if result.CredentialScope {
					break
				}
				continue
			}
			m.MarkResult(execCtx, result)
			attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, upstreamModel, aliasResult)
			rewriteForceMappedResponse(&resp, attemptAliasResult)
			return resp, nil
		}
		if authErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, authErr, m.runtimeConfigSnapshot())
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return cliproxyexecutor.Response{}, wrapRequestStopError(authErr)
				}
				lastErr = authErr
				if homeMode {
					homeAuthCount++
				}
				continue
			}
			if isResponsesCompactRequestFaultError(opts, authErr) || isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			lastErr = authErr
			if homeMode {
				homeAuthCount++
			}
			continue
		}
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, retryRound int, defaultRequestRetry int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := authSelectionModelFromOptions(opts, req.Model)
	executionModel, restoreExecutionModel := executionModelForAuthSelection(opts, req.Model)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{})
	if !homeMode {
		for authID := range m.requestRetryRoundExclusions(retryRound, defaultRequestRetry) {
			tried[authID] = struct{}{}
		}
	}
	attempted := make(map[string]struct{})
	var lastErr error
	var upstreamErr error
	for {
		if maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, preferredExecutionAttemptError(lastErr, upstreamErr)
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeRetryRound(pickOpts, retryRound)
			pickOpts = withHomeAuthCount(pickOpts, homeAuthCount)
			pickOpts = withHomeExcludedAuthIDs(pickOpts, tried)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return cliproxyexecutor.Response{}, preferredExecutionAttemptError(lastErr, upstreamErr)
			}
			return cliproxyexecutor.Response{}, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, routeModel)
		publishSelectedAuthMetadata(opts.Metadata, auth)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)
		execCtx = newUpstreamAttemptContext(execCtx)

		models, pooled, aliasResult, routing := m.preparedExecutionModelsWithAlias(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			if errCancel := claudeOAuthRequestCancellation(execCtx, auth, errPrepare); errCancel != nil {
				return cliproxyexecutor.Response{}, errCancel
			}
			stateModel := m.selectionModelKeyForAuth(auth, routeModel)
			if stateModel == "" {
				stateModel = canonicalModelKey(routeModel)
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: stateModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(errPrepare), Options: pickOpts, SkipQuotaObservation: true}
			m.MarkResult(execCtx, result)
			lastErr = errPrepare
			continue
		}
		var authErr error
		didRefreshOnUnauthorized := false
		for _, upstreamModel := range models {
			execCtx = newUpstreamAttemptContext(execCtx)
			resultModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			if restoreExecutionModel {
				execReq.Model = executionModel
			}
			execOpts := opts
			if pickOpts.Metadata != nil {
				if canonicalID, ok := pickOpts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; ok {
					meta := make(map[string]any, len(execOpts.Metadata)+2)
					for k, v := range execOpts.Metadata {
						meta[k] = v
					}
					meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = canonicalID
					if parentID, okParent := pickOpts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; okParent && parentID != canonicalID {
						meta[cliproxyexecutor.ParentSessionIDMetadataKey] = parentID
					} else {
						delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
					}
					execOpts.Metadata = meta
				}
			}
			payload := execOpts.OriginalRequest
			if len(payload) == 0 {
				payload = execReq.Payload
			}
			execOpts.Metadata = ensureCanonicalSessionMetadata(execOpts.Metadata, execOpts.Headers, payload)
			var errIntercept error
			execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(execCtx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
			if errIntercept != nil {
				return cliproxyexecutor.Response{}, errIntercept
			}
			if !restoreExecutionModel {
				execReq = attachResolvedAPIKeyModelInfo(routing, execReq, auth, routeModel, upstreamModel)
			}
			execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
			startExec := time.Now()
			resp, errExec := executor.CountTokens(execCtx, auth, execReq, execOpts)
			errExec = markUpstreamExecutionAttemptFromContext(execCtx, errExec)
			durationExec := time.Since(startExec)
			if errExec != nil {
				if hasUpstreamExecutionAttempt(errExec) {
					upstreamErr = errExec
				}
				if errCtx := execCtx.Err(); errCtx != nil {
					return cliproxyexecutor.Response{}, errCtx
				}
				refreshCtx := newUpstreamAttemptContext(execCtx)
				if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(refreshCtx, auth, errExec, didRefreshOnUnauthorized); okRefresh {
					auth = refreshed
					didRefreshOnUnauthorized = true
					execCtx = newUpstreamAttemptContext(execCtx)
					execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
					startRetry := time.Now()
					resp, errExec = executor.CountTokens(execCtx, auth, execReq, execOpts)
					errExec = markUpstreamExecutionAttemptFromContext(execCtx, errExec)
					durationRetry := time.Since(startRetry)
					if errExec != nil {
						if hasUpstreamExecutionAttempt(errExec) {
							upstreamErr = errExec
						}
						warnLogUpstreamFailure(execCtx, entry, provider, upstreamModel, auth, durationRetry, errExec)
						if errCtx := execCtx.Err(); errCtx != nil {
							return cliproxyexecutor.Response{}, errCtx
						}
					}
				} else {
					warnLogUpstreamFailure(execCtx, entry, provider, upstreamModel, auth, durationExec, errExec)
				}
			}
			if errCancel := claudeOAuthRequestCancellation(execCtx, auth, errExec); errCancel != nil {
				return cliproxyexecutor.Response{}, errCancel
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: errExec == nil, Options: execOpts, SkipQuotaObservation: true}
			if errExec != nil {
				result.Error = resultErrorFromError(errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				action, okAction := matchRequestScopedErrorAction(auth, errExec, m.runtimeConfigSnapshot())
				applyRequestScopedActionToResult(action, okAction, &result)
				// Some Anthropic-compatible upstreams do not implement the
				// count_tokens route and return a generic endpoint 404. Record
				// the failure for hooks and metrics without suspending a model
				// that remains usable through the messages endpoint.
				if isCountTokensEndpointNotFoundError(errExec, execReq.Model) && (result.Error == nil || result.Error.Code != ErrorCodeForceCooldown) {
					m.recordAvailabilityNeutralResult(execCtx, result)
				} else {
					if isCredentialScopedError(errExec) {
						result.CredentialScope = true
					}
					m.MarkResult(execCtx, result)
				}
				if okAction {
					if isRequestScopedStop(action, okAction) {
						return cliproxyexecutor.Response{}, wrapRequestStopError(errExec)
					}
					authErr = errExec
					if result.CredentialScope {
						break
					}
					continue
				}
				if isRequestInvalidError(errExec) {
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				if result.CredentialScope {
					break
				}
				continue
			}
			m.MarkResult(execCtx, result)
			attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, upstreamModel, aliasResult)
			rewriteForceMappedResponse(&resp, attemptAliasResult)
			return resp, nil
		}
		if authErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, authErr, m.runtimeConfigSnapshot())
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return cliproxyexecutor.Response{}, wrapRequestStopError(authErr)
				}
				lastErr = authErr
				if homeMode {
					homeAuthCount++
				}
				continue
			}
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			lastErr = authErr
			if homeMode {
				homeAuthCount++
			}
			continue
		}
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, homeRetryLimit *int, retryRound int, defaultRequestRetry int) (*cliproxyexecutor.StreamResult, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := authSelectionModelFromOptions(opts, req.Model)
	responseAlias := requestedModelAliasFromOptions(opts, routeModel)
	executionModel, restoreExecutionModel := executionModelForAuthSelection(opts, req.Model)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{})
	if !homeMode {
		for authID := range m.requestRetryRoundExclusions(retryRound, defaultRequestRetry) {
			tried[authID] = struct{}{}
		}
	}
	homeExcludedAuthIDs := make(map[string]struct{})
	homeSameAuthRetries := make(map[string]int)
	lastHomeAuthID := ""
	homeSameAuthRetryPending := false
	attempted := make(map[string]struct{})
	var lastErr error
	var upstreamErr error
	var roundTiming homeRetryRoundTiming
	for {
		allowSameAuthRetry := homeMode && homeSameAuthRetryPending && lastHomeAuthID != "" && homeSameAuthRetries[lastHomeAuthID] == 0
		if maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials && !allowSameAuthRetry {
			if lastErr != nil {
				preferredErr := preferredExecutionAttemptError(lastErr, upstreamErr)
				if homeMode {
					return nil, markHomeRetryRoundExhausted(preferredErr, roundTiming.RetryAfter(), true)
				}
				return nil, preferredErr
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeRetryRound(pickOpts, retryRound)
			pickOpts = withHomeAuthCount(pickOpts, homeAuthCount)
			pickOpts = withHomeExcludedAuthIDs(pickOpts, homeExcludedAuthIDs)
		}

		var selection *HomeDispatchSelection
		var auth *Auth
		var executor ProviderExecutor
		var provider string
		var errPick error
		if homeMode {
			selection, errPick = m.pickHomeDispatchSelection(ctx, routeModel, pickOpts)
			if selection != nil {
				auth = selection.CloneAuthForRoute(routeModel)
				executor = selection.Executor
				provider = selection.Provider
			}
		} else {
			auth, executor, provider, errPick = m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		}
		if errPick != nil {
			preferredErr := preferredExecutionAttemptError(lastErr, upstreamErr)
			var homeCooldown *homeDispatchRetryAfterError
			if homeMode && lastErr != nil && errors.As(errPick, &homeCooldown) && homeCooldown != nil {
				observeHomeCooldownRetryLimit(homeCooldown, homeRetryLimit, pinnedAuthIDFromMetadata(opts.Metadata) == "")
				return nil, markHomeRetryRoundExhausted(preferredErr, homeCooldown.RetryAfter(), false)
			}
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				if homeMode {
					return nil, markHomeRetryRoundExhausted(preferredErr, roundTiming.RetryAfter(), isHomeNextRoundImmediatelyAvailable(errPick))
				}
				return nil, preferredErr
			}
			return nil, errPick
		}
		if auth == nil || executor == nil {
			if selection != nil {
				selection.End("missing_execution_target")
			}
			return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		if homeMode {
			m.observeHomeRetryLimit(auth, selection, homeRetryLimit)
		}
		if selection != nil && allowSameAuthRetry && maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials && auth.ID != lastHomeAuthID {
			if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "max_retry_credentials"); errEnd != nil {
				return nil, errEnd
			}
			if lastErr != nil {
				return nil, markHomeRetryRoundExhausted(preferredExecutionAttemptError(lastErr, upstreamErr), roundTiming.RetryAfter(), true)
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		if homeMode && lastHomeAuthID != "" && auth.ID != lastHomeAuthID {
			homeSameAuthRetryPending = false
		}
		if selection != nil {
			// A legacy Home may ignore excluded_auth_ids and return the same
			// credential again. Reject credentials explicitly excluded from this
			// round while retaining the explicit same-auth retry path, which
			// intentionally leaves the credential out of homeExcludedAuthIDs.
			if _, alreadyTried := tried[auth.ID]; alreadyTried {
				if _, excluded := homeExcludedAuthIDs[auth.ID]; excluded {
					if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "repeated_excluded_auth"); errEnd != nil {
						return nil, errEnd
					}
					if lastErr != nil {
						return nil, markHomeRetryRoundExhausted(preferredExecutionAttemptError(lastErr, upstreamErr), roundTiming.RetryAfter(), false)
					}
					return nil, repeatedHomeAuthError()
				} else {
					homeSameAuthRetries[auth.ID]++
					if homeSameAuthRetries[auth.ID] > 1 {
						// A fresh Home selection may retry the same auth once for
						// connection lifecycle or authorization recovery. Repeated
						// failures must still rotate away from this credential.
						homeExcludedAuthIDs[auth.ID] = struct{}{}
						if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "repeated_same_auth"); errEnd != nil {
							return nil, errEnd
						}
						continue
					}
				}
			}
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, routeModel)
		if selection != nil {
			if errRuntimeAuth := m.bindHomeSelectionRuntimeAuth(ctx, opts, selection); errRuntimeAuth != nil {
				selection.End("runtime_auth_bind_failed")
				return nil, errRuntimeAuth
			}
		}
		publishSelectedAuthMetadata(opts.Metadata, auth)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		releaseAttempt := func() {}
		if selection != nil {
			var errBind error
			execCtx, releaseAttempt, errBind = homeExecutionAttemptContext(ctx, selection)
			if errBind != nil {
				selection.End("attempt_bind_failed")
				return nil, errBind
			}
		}
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		// Enrich before auth preparation so prepare-stage usage records observe the client request.
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)
		execCtx = newUpstreamAttemptContext(execCtx)
		models, pooled, aliasResult, routing := m.preparedExecutionModelsWithAlias(auth, routeModel)
		if selection != nil && aliasResult.ForceMapping && responseAlias != "" {
			aliasResult.OriginalAlias = responseAlias
		}
		if len(models) == 0 {
			if selection != nil {
				homeExcludedAuthIDs[auth.ID] = struct{}{}
				lastHomeAuthID = auth.ID
				homeSameAuthRetryPending = false
				releaseAttempt()
				if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "no_execution_models"); errEnd != nil {
					return nil, errEnd
				}
			}
			continue
		}
		attempted[auth.ID] = struct{}{}
		var errPrepare error
		if selection != nil {
			auth, errPrepare = m.prepareHomeRequestAuth(execCtx, executor, selection)
		} else {
			auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		}
		if errPrepare != nil {
			if selection != nil {
				excludeAuth := shouldExcludeHomeAuthAfterStreamError(execCtx, auth, errPrepare)
				if homeSameAuthRetries[auth.ID] > 0 {
					excludeAuth = true
				}
				if excludeAuth {
					homeExcludedAuthIDs[auth.ID] = struct{}{}
				}
				lastHomeAuthID = auth.ID
				homeSameAuthRetryPending = !excludeAuth
			}
			if selection == nil {
				if errCancel := claudeOAuthRequestCancellation(execCtx, auth, errPrepare); errCancel != nil {
					return nil, errCancel
				}
			}
			stateModel := m.selectionModelKeyForAuth(auth, routeModel)
			if stateModel == "" {
				stateModel = canonicalModelKey(routeModel)
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: stateModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(errPrepare), Options: pickOpts}
			if selection != nil {
				m.reportHomeResult(execCtx, result, auth)
				releaseAttempt()
			} else {
				m.MarkResult(execCtx, result)
			}
			lastErr = errPrepare
			if homeMode {
				roundTiming.Observe(lastErr)
			}
			if selection != nil {
				if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "prepare_failed"); errEnd != nil {
					return nil, errEnd
				}
			}
			continue
		}
		execReq := sanitizeDownstreamWebsocketFallbackRequest(execCtx, auth, req)
		if selection != nil && !restoreExecutionModel {
			execReq = attachResolvedHomeModelInfo(execReq, selection.modelInfo)
		}
		streamExecutionModel := ""
		if restoreExecutionModel {
			streamExecutionModel = executionModel
		}
		execOpts := opts
		if selection != nil {
			execOpts.ExecutionLifecycle = selection
			if selection.CanonicalSessionID != "" {
				meta := make(map[string]any, len(execOpts.Metadata)+2)
				for k, v := range execOpts.Metadata {
					meta[k] = v
				}
				meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = selection.CanonicalSessionID
				if selection.ParentSessionID != "" && selection.ParentSessionID != selection.CanonicalSessionID {
					meta[cliproxyexecutor.ParentSessionIDMetadataKey] = selection.ParentSessionID
				} else {
					delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
				}
				execOpts.Metadata = meta
			}
		} else if pickOpts.Metadata != nil {
			if canonicalID, ok := pickOpts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; ok {
				meta := make(map[string]any, len(execOpts.Metadata)+2)
				for k, v := range execOpts.Metadata {
					meta[k] = v
				}
				meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = canonicalID
				if parentID, okParent := pickOpts.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; okParent && parentID != canonicalID {
					meta[cliproxyexecutor.ParentSessionIDMetadataKey] = parentID
				} else {
					delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
				}
				execOpts.Metadata = meta
			}
		}
		payload := execOpts.OriginalRequest
		if len(payload) == 0 {
			payload = execReq.Payload
		}
		execOpts.Metadata = ensureCanonicalSessionMetadata(execOpts.Metadata, execOpts.Headers, payload)
		execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
		if homeMode && len(models) > 1 {
			models = models[:1]
			pooled = false
		}
		streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, execReq, execOpts, routeModel, streamExecutionModel, models, pooled, aliasResult, routing, !homeMode || selection != nil, selection != nil)
		if errStream != nil {
			if hasUpstreamExecutionAttempt(errStream) {
				upstreamErr = errStream
			}
			if selection != nil {
				excludeAuth := shouldExcludeHomeAuthAfterStreamError(execCtx, auth, errStream)
				if homeSameAuthRetries[auth.ID] > 0 {
					excludeAuth = true
				}
				if excludeAuth {
					homeExcludedAuthIDs[auth.ID] = struct{}{}
				}
				lastHomeAuthID = auth.ID
				homeSameAuthRetryPending = !excludeAuth
			}
			if selection != nil {
				releaseAttempt()
				if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "stream_start_failed"); errEnd != nil {
					return nil, errEnd
				}
			}
			if errCtx := execCtx.Err(); errCtx != nil && ctx != nil && ctx.Err() != nil {
				return nil, errCtx
			}
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream)
				}
				lastErr = errStream
				if homeMode {
					roundTiming.Observe(lastErr)
				}
				if homeMode {
					homeAuthCount++
				}
				continue
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			if homeMode {
				roundTiming.Observe(lastErr)
			}
			if homeMode {
				homeAuthCount++
			}
			continue
		}
		if selection != nil {
			if m.retainHomeWebsocketSelection(ctx, opts, routeModel, selection) {
				return wrapHomeStream(ctx, streamResult, nil, releaseAttempt), nil
			}
			return wrapHomeStream(ctx, streamResult, selection, releaseAttempt), nil
		}
		return streamResult, nil
	}
}

func shouldExcludeHomeAuthAfterStreamError(ctx context.Context, _ *Auth, err error) bool {
	if err == nil || isConnectionLifecycleError(err) {
		return false
	}
	// A 426 during a downstream websocket attempt is a transport fallback
	// signal and may retry the same credential once.
	if cliproxyexecutor.DownstreamWebsocket(ctx) && statusCodeFromError(err) == http.StatusUpgradeRequired {
		return false
	}
	return true
}

func withAttemptedAuthTracker(opts cliproxyexecutor.Options, attempted map[string]struct{}) cliproxyexecutor.Options {
	if attempted == nil {
		return opts
	}
	meta := cloneRequestMetadata(opts.Metadata)
	prevCallback, _ := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string))
	meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey] = func(authID string) {
		if strings.TrimSpace(authID) != "" {
			attempted[authID] = struct{}{}
		}
		if prevCallback != nil {
			prevCallback(authID)
		}
	}
	opts.Metadata = meta
	return opts
}

func cloneRequestMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return make(map[string]any, 4)
	}
	dst := make(map[string]any, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	opts.Metadata = cloneRequestMetadata(opts.Metadata)
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	return opts
}

func authSelectionModelFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.AuthSelectionModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	case []byte:
		if strings.TrimSpace(string(value)) != "" {
			return strings.TrimSpace(string(value))
		}
	}
	return fallback
}

func executionModelForAuthSelection(opts cliproxyexecutor.Options, model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}
	selectionModel := authSelectionModelFromOptions(opts, model)
	if selectionModel == model {
		return "", false
	}
	return model, true
}

func withHomeAuthCount(opts cliproxyexecutor.Options, count int) cliproxyexecutor.Options {
	if count <= 0 {
		count = 1
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[homeAuthCountMetadataKey] = count
	opts.Metadata = meta
	return opts
}

func withHomeRetryRound(opts cliproxyexecutor.Options, retryRound int) cliproxyexecutor.Options {
	meta := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	if retryRound > 0 {
		meta[homeRetryRoundMetadataKey] = retryRound
	} else {
		delete(meta, homeRetryRoundMetadataKey)
	}
	opts.Metadata = meta
	return opts
}

func withHomeExcludedAuthIDs(opts cliproxyexecutor.Options, tried map[string]struct{}) cliproxyexecutor.Options {
	meta := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	excluded := make(map[string]struct{})
	for _, authID := range homeExcludedAuthIDsFromMetadata(meta) {
		excluded[authID] = struct{}{}
	}
	for authID := range tried {
		if authID = strings.TrimSpace(authID); authID != "" {
			excluded[authID] = struct{}{}
		}
	}
	if len(excluded) == 0 {
		delete(meta, ExcludedAuthIDsMetadataKey)
	} else {
		ids := make([]string, 0, len(excluded))
		for authID := range excluded {
			ids = append(ids, authID)
		}
		sort.Strings(ids)
		meta[ExcludedAuthIDsMetadataKey] = ids
	}
	opts.Metadata = meta
	return opts
}

func homeAuthCountFromMetadata(meta map[string]any) int {
	if len(meta) == 0 {
		return 1
	}
	switch value := meta[homeAuthCountMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	}
	return 1
}

func homeExcludedAuthIDsFromMetadata(meta map[string]any) []string {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta[ExcludedAuthIDsMetadataKey]
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	appendID := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
	}
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			appendID(value)
		}
	case []any:
		for _, value := range values {
			if text, okText := value.(string); okText {
				appendID(text)
			}
		}
	case map[string]struct{}:
		for value := range values {
			appendID(value)
		}
	case map[string]bool:
		for value, enabled := range values {
			if enabled {
				appendID(value)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	return ids
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

type requestAuthPrepareLock struct {
	mu sync.Mutex
}

// prepareHomeRequestAuth prepares a dispatch auth without reading or updating local auth state.
func (m *Manager) prepareHomeRequestAuth(ctx context.Context, executor ProviderExecutor, selection *HomeDispatchSelection) (*Auth, error) {
	if selection == nil {
		return nil, nil
	}
	auth := selection.CloneAuth()
	prepared, errPrepare := m.prepareHomeAuthSnapshot(ctx, executor, auth)
	if errPrepare != nil {
		warnLogHomeCredentialFailure(ctx, "request_auth_preparation", selection.Provider, auth, errPrepare)
	}
	return prepared, errPrepare
}

func (m *Manager) prepareHomeAuthSnapshot(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok || preparer == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	prepare := func() (*Auth, error) {
		target := auth.Clone()
		if !preparer.ShouldPrepareRequestAuth(target) {
			return target, nil
		}
		updated, errPrepare := preparer.PrepareRequestAuth(ctx, target)
		if errPrepare != nil {
			return auth, errPrepare
		}
		if updated == nil {
			return target, nil
		}
		return updated, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return prepare()
	}
	lockValue, _ := m.requestPrepareLocks.LoadOrStore(id, &requestAuthPrepareLock{})
	lock, ok := lockValue.(*requestAuthPrepareLock)
	if !ok || lock == nil {
		return prepare()
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	return prepare()
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok {
		return auth, nil
	}

	return m.PrepareRequestAuth(ctx, preparer, auth)
}

// PrepareRequestAuth prepares a registered credential using the same serialization
// and lifecycle checks as normal request execution. Management tools use this path too.
func (m *Manager) PrepareRequestAuth(ctx context.Context, preparer RequestAuthPreparer, auth *Auth) (*Auth, error) {
	if m == nil || preparer == nil || auth == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	var prepareMu *sync.Mutex
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") {
		// Meta also mints on 401 recovery. Serialize both paths per credential.
		lockValue, _ := m.refreshLocks.LoadOrStore(id, &authRefreshLock{})
		prepareMu = &lockValue.(*authRefreshLock).mu
	} else {
		lockValue, _ := m.requestPrepareLocks.LoadOrStore(id, &requestAuthPrepareLock{})
		prepareMu = &lockValue.(*requestAuthPrepareLock).mu
	}
	prepareMu.Lock()
	defer prepareMu.Unlock()

	target := auth.Clone()
	m.mu.RLock()
	current := m.auths[id]
	if current != nil {
		target = current.Clone()
	}
	m.mu.RUnlock()
	if current == nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") {
		return nil, fmt.Errorf("prepare meta auth: credential no longer registered")
	}

	if !preparer.ShouldPrepareRequestAuth(target) {
		return target, nil
	}

	base := target.Clone()
	updated, errPrepare := preparer.PrepareRequestAuth(ctx, base.Clone())
	if errPrepare != nil {
		return auth, errPrepare
	}
	if updated == nil {
		return target, nil
	}

	saved, errUpdate := m.UpdatePreparedAuth(ctx, base, updated)
	if errUpdate != nil {
		return nil, errUpdate
	}
	if saved != nil {
		return saved, nil
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") {
		return nil, fmt.Errorf("prepare meta auth: credential removed during mint")
	}
	return target, nil
}

func contextWithRequestedModelAlias(ctx context.Context, opts cliproxyexecutor.Options, fallback string) context.Context {
	alias := requestedModelAliasFromOptions(opts, fallback)
	ctx = coreusage.WithRequestedModelAlias(ctx, alias)
	effort := reasoningEffortFromOptions(opts)
	if effort != "" {
		ctx = coreusage.WithReasoningEffort(ctx, effort)
	}
	serviceTier := serviceTierFromOptions(opts)
	if serviceTier != "" {
		ctx = coreusage.WithServiceTier(ctx, serviceTier)
	}
	if generate, ok := generateFromOptions(opts); ok {
		ctx = coreusage.WithGenerate(ctx, generate)
	}
	ctx = coreusage.WithStream(ctx, opts.Stream)
	return ctx
}

func requestedModelAliasFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		return strings.TrimSpace(value)
	case []byte:
		if len(value) == 0 {
			return fallback
		}
		return strings.TrimSpace(string(value))
	default:
		return fallback
	}
}

func reasoningEffortFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ReasoningEffortMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func serviceTierFromOptions(opts cliproxyexecutor.Options) string {
	return stringMetadataValue(opts.Metadata, cliproxyexecutor.ServiceTierMetadataKey)
}

func generateFromOptions(opts cliproxyexecutor.Options) (bool, bool) {
	if len(opts.Metadata) == 0 {
		return false, false
	}
	raw, ok := opts.Metadata[cliproxyexecutor.GenerateMetadataKey]
	if !ok || raw == nil {
		return false, false
	}
	switch value := raw.(type) {
	case bool:
		return value, true
	default:
		return false, false
	}
}

func stringMetadataValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func disallowFreeAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowFreeAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && parsed
	case []byte:
		parsed, err := strconv.ParseBool(strings.TrimSpace(string(val)))
		return err == nil && parsed
	default:
		return false
	}
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func publishSelectedAuthMetadata(meta map[string]any, auth *Auth) {
	if len(meta) == 0 || auth == nil {
		return
	}
	if authID := strings.TrimSpace(auth.ID); authID != "" {
		meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
		if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
			callback(authID)
		}
	}
	if authIndex := strings.TrimSpace(auth.EnsureIndex()); authIndex != "" {
		meta[cliproxyexecutor.SelectedAuthIndexMetadataKey] = authIndex
		if callback, ok := meta[cliproxyexecutor.SelectedAuthIndexCallbackMetadataKey].(func(string)); ok && callback != nil {
			callback(authIndex)
		}
	}
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	exec, _ := m.executorLocked(provider)
	return exec
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return util.OpenAICompatibleProviderKey(providerKey)
		}
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		providerKey := strings.TrimSpace(auth.Label)
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		return util.OpenAICompatibleProviderKey(providerKey)
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "kimi.com":
		return "kimi"
	case "kimi.ai":
		return "kimi-ai"
	default:
		return provider
	}
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

func formatAuthIdentity(auth *Auth, provider string) string {
	if auth == nil {
		return "auth=nil"
	}
	accountType, accountInfo := auth.AccountInfo()
	switch accountType {
	case "api_key":
		return fmt.Sprintf("api_key=%s", util.HideAPIKey(accountInfo))
	case "oauth":
		return formatOauthIdentity(auth, provider, accountInfo)
	default:
		if auth.FileName != "" {
			return fmt.Sprintf("auth_file=%s", filepath.Base(auth.FileName))
		}
		if auth.ID != "" {
			return fmt.Sprintf("auth_id=%s", auth.ID)
		}
		if accountInfo != "" {
			return accountInfo
		}
		return "unknown"
	}
}

func warnLogHomeCredentialFailure(ctx context.Context, operation, provider string, auth *Auth, err error) {
	if err == nil {
		return
	}
	provider = strings.TrimSpace(provider)
	if provider == "" && auth != nil {
		provider = strings.TrimSpace(auth.Provider)
	}
	fields := log.Fields{
		"auth":      formatAuthIdentity(auth, provider),
		"operation": operation,
		"provider":  provider,
	}
	if statusCode := statusCodeFromError(err); statusCode != 0 {
		fields["status"] = statusCode
	}
	logEntryWithRequestID(ctx).WithFields(fields).Warnf("Home credential operation failed: err=%s", safeErrorDiagnosticForLog(err))
}

func safeErrorDiagnosticForLog(err error) string {
	if err == nil {
		return ""
	}
	diagnostic := err.Error()
	type logDiagnosticError interface {
		LogDiagnostic() string
	}
	var diagnosticErr logDiagnosticError
	if errors.As(err, &diagnosticErr) && diagnosticErr != nil {
		if markedDiagnostic := strings.TrimSpace(diagnosticErr.LogDiagnostic()); markedDiagnostic != "" {
			diagnostic = markedDiagnostic
		}
	}
	return logging.SafeDiagnosticForLog(diagnostic)
}

func warnLogUpstreamFailure(ctx context.Context, entry *log.Entry, provider, model string, auth *Auth, duration time.Duration, err error) {
	if err == nil {
		return
	}
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if isRequestInvalidError(err) {
		return
	}
	if entry == nil {
		if ctx != nil {
			entry = logEntryWithRequestID(ctx)
		} else {
			entry = log.NewEntry(log.StandardLogger())
		}
	}
	authIdent := formatAuthIdentity(auth, provider)
	errSummary := safeErrorDiagnosticForLog(err)
	duration = duration.Round(time.Millisecond)
	if statusCode := statusCodeFromError(err); statusCode != 0 {
		entry.Warnf("%3d | %13v | upstream execution failed: provider=%s model=%s auth=%s err=%s", statusCode, duration, provider, model, authIdent, errSummary)
		return
	}
	entry.Warnf("upstream execution failed: provider=%s model=%s auth=%s duration=%s err=%s", provider, model, authIdent, duration, errSummary)
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec, _ = m.executorLocked(executorKeyFromAuth(a))
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}

func ensureCanonicalSessionMetadata(metadata map[string]any, headers http.Header, payload []byte) map[string]any {
	if metadata != nil {
		if canonicalID, ok := metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string); ok && strings.TrimSpace(canonicalID) != "" {
			return metadata
		}
	}
	canonicalID := CanonicalSessionID(headers, payload, metadata)
	if canonicalID == "" {
		return metadata
	}
	out := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	out[cliproxyexecutor.CanonicalSessionIDMetadataKey] = canonicalID
	return out
}

func syncMetadataSessionToContext(ctx context.Context, metadata map[string]any) context.Context {
	if ctx == nil {
		return nil
	}
	canonicalID := ""
	if len(metadata) > 0 {
		canonicalID, _ = metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
		if canonicalID == "" {
			canonicalID, _ = metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey].(string)
		}
		if canonicalID == "" {
			if execID, _ := metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string); execID != "" {
				execID = strings.TrimSpace(execID)
				if !strings.HasPrefix(execID, "execution:") {
					canonicalID = "execution:" + execID
				} else {
					canonicalID = execID
				}
			}
		}
		if canonicalID == "" {
			if derivedID, _ := metadata[cliproxyexecutor.DerivedSessionIDMetadataKey].(string); derivedID != "" {
				derivedID = strings.TrimSpace(derivedID)
				if !strings.HasPrefix(derivedID, "derived:") {
					canonicalID = "derived:" + derivedID
				} else {
					canonicalID = derivedID
				}
			}
		}
	}
	canonicalID = strings.TrimSpace(canonicalID)
	if canonicalID == "" {
		clientMeta := logging.GetClientRequestMetadata(ctx)
		if clientMeta.SessionID != "" || clientMeta.ParentSessionID != "" || clientMeta.NodeKind != "" || clientMeta.IsFork || clientMeta.IsCompaction {
			clientMeta.SessionID = ""
			clientMeta.ParentSessionID = ""
			clientMeta.NodeKind = ""
			clientMeta.IsFork = false
			clientMeta.IsCompaction = false
			ctx = logging.WithClientRequestMetadata(ctx, clientMeta)
		}
		return util.WithSessionID(ctx, "")
	}
	clientMeta := logging.GetClientRequestMetadata(ctx)
	clientMeta.SessionID = cliproxysession.BoundSessionIdentity(canonicalID)
	if parentID, ok := metadata[cliproxyexecutor.ParentSessionIDMetadataKey].(string); ok && strings.TrimSpace(parentID) != "" {
		clientMeta.ParentSessionID = cliproxysession.BoundSessionIdentity(strings.TrimSpace(parentID))
	} else {
		clientMeta.ParentSessionID = ""
	}
	if clientMeta.SessionID == clientMeta.ParentSessionID {
		clientMeta.ParentSessionID = ""
	}
	if nodeKind, ok := metadata[cliproxyexecutor.NodeKindMetadataKey].(string); ok && strings.TrimSpace(nodeKind) != "" {
		clientMeta.NodeKind = strings.TrimSpace(nodeKind)
	} else {
		clientMeta.NodeKind = ""
	}
	if isFork, ok := metadata[cliproxyexecutor.IsForkMetadataKey].(bool); ok {
		clientMeta.IsFork = isFork
	} else {
		clientMeta.IsFork = false
	}
	if isCompaction, ok := metadata[cliproxyexecutor.IsCompactionMetadataKey].(bool); ok {
		clientMeta.IsCompaction = isCompaction
	} else {
		clientMeta.IsCompaction = false
	}
	ctx = logging.WithClientRequestMetadata(ctx, clientMeta)
	return util.WithSessionID(ctx, clientMeta.SessionID)
}
```

## `sdk/cliproxy/auth/conductor_home_execution.go`

SHA-256 (LF): `25a02df6fef71420ae042f1cfa9a245b2bbc66b908c53b7ab4215e2681582dda`

```go
package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/sjson"
)

func (m *Manager) executeHome(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, countTokens bool) (cliproxyexecutor.Response, error) {
	if unlockSession := m.lockHomeWebsocketSession(ctx, opts); unlockSession != nil {
		defer unlockSession()
	}
	defaultRequestRetry, maxRetryCredentials, maxWait := m.retrySettings()
	retryModel := authSelectionModelFromOptions(opts, req.Model)
	homeRetryLimit := -1
	attempt := 0
	retryRoundPending := false
	retryRoundWaited := false
	var preferredUpstreamErr error
	for {
		response, errExecute := m.executeHomeOnce(ctx, providers, req, opts, countTokens, maxRetryCredentials, &homeRetryLimit, attempt)
		if errExecute == nil {
			return response, nil
		}
		if hasUpstreamExecutionAttempt(errExecute) {
			preferredUpstreamErr = errExecute
		}
		if retryRoundPending {
			if wait, okWait := pendingHomeRetryRoundDelay(errExecute, maxWait, &homeRetryLimit, pinnedAuthIDFromMetadata(opts.Metadata) == ""); okWait && m.homeRetryAllowed(attempt-1, homeRetryLimit) {
				if retryRoundWaited {
					return cliproxyexecutor.Response{}, errExecute
				}
				if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
					return cliproxyexecutor.Response{}, errWait
				}
				retryRoundWaited = true
				continue
			}
		}
		retryRoundPending = false
		retryRoundWaited = false
		if isRequestTerminatedError(errExecute) || isRequestStopError(errExecute) {
			return cliproxyexecutor.Response{}, unwrapExecutionBoundaryError(errExecute)
		}
		wait, shouldRetry := m.shouldRetryAfterErrorWithHomeRetryLimit(ctx, opts, errExecute, attempt, providers, retryModel, maxWait, homeRetryLimit, defaultRequestRetry)
		if !shouldRetry {
			if preferredUpstreamErr != nil && isHomeRetryRoundExhausted(errExecute) {
				errExecute = preferredExecutionAttemptError(errExecute, preferredUpstreamErr)
			}
			return cliproxyexecutor.Response{}, unwrapExecutionBoundaryError(errExecute)
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
		attempt++
		retryRoundPending = true
		retryRoundWaited = false
	}
}

func (m *Manager) executeHomeOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, countTokens bool, maxRetryCredentials int, homeRetryLimit *int, retryRounds ...int) (cliproxyexecutor.Response, error) {
	retryRound := 0
	if len(retryRounds) > 0 {
		retryRound = retryRounds[0]
	}
	routeModel := authSelectionModelFromOptions(opts, req.Model)
	responseAlias := requestedModelAliasFromOptions(opts, routeModel)
	executionModel, restoreExecutionModel := executionModelForAuthSelection(opts, req.Model)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	var lastErr error
	var upstreamErr error
	var roundTiming homeRetryRoundTiming
	for homeAuthCount := 1; ; homeAuthCount++ {
		if maxRetryCredentials > 0 && len(attempted) >= maxRetryCredentials {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, markHomeRetryRoundExhausted(preferredExecutionAttemptError(lastErr, upstreamErr), roundTiming.RetryAfter(), true)
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		pickOpts := withHomeRetryRound(opts, retryRound)
		pickOpts = withHomeAuthCount(pickOpts, homeAuthCount)
		pickOpts = withHomeExcludedAuthIDs(pickOpts, tried)
		selection, errSelection := m.pickHomeDispatchSelection(ctx, routeModel, pickOpts)
		if errSelection != nil {
			preferredErr := preferredExecutionAttemptError(lastErr, upstreamErr)
			var homeCooldown *homeDispatchRetryAfterError
			if lastErr != nil && errors.As(errSelection, &homeCooldown) && homeCooldown != nil {
				observeHomeCooldownRetryLimit(homeCooldown, homeRetryLimit, pinnedAuthIDFromMetadata(opts.Metadata) == "")
				return cliproxyexecutor.Response{}, markHomeRetryRoundExhausted(preferredErr, homeCooldown.RetryAfter(), false)
			}
			if shouldReturnLastErrorOnPickFailure(true, lastErr, errSelection) {
				return cliproxyexecutor.Response{}, markHomeRetryRoundExhausted(preferredErr, roundTiming.RetryAfter(), isHomeNextRoundImmediatelyAvailable(errSelection))
			}
			return cliproxyexecutor.Response{}, errSelection
		}
		auth := selection.CloneAuthForRoute(routeModel)
		if auth == nil || selection.Executor == nil {
			selection.End("missing_execution_target")
			return cliproxyexecutor.Response{}, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		m.observeHomeRetryLimit(auth, selection, homeRetryLimit)
		if _, seen := tried[auth.ID]; seen {
			if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "repeated_auth"); errEnd != nil {
				return cliproxyexecutor.Response{}, errEnd
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, markHomeRetryRoundExhausted(preferredExecutionAttemptError(lastErr, upstreamErr), roundTiming.RetryAfter(), false)
			}
			return cliproxyexecutor.Response{}, repeatedHomeAuthError()
		}
		tried[auth.ID] = struct{}{}
		attempted[auth.ID] = struct{}{}
		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, selection.Provider, routeModel)
		if errRuntimeAuth := m.bindHomeSelectionRuntimeAuth(ctx, opts, selection); errRuntimeAuth != nil {
			selection.End("runtime_auth_bind_failed")
			return cliproxyexecutor.Response{}, errRuntimeAuth
		}
		publishSelectedAuthMetadata(opts.Metadata, auth)
		execCtx, releaseAttempt, errBind := homeExecutionAttemptContext(ctx, selection)
		if errBind != nil {
			selection.End("attempt_bind_failed")
			return cliproxyexecutor.Response{}, errBind
		}
		// Enrich before auth preparation so prepare-stage usage records observe the client request.
		execCtx = contextWithRequestedModelAlias(execCtx, opts, routeModel)
		execCtx = newUpstreamAttemptContext(execCtx)
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		models, pooled, aliasResult, routing := m.preparedExecutionModelsWithAlias(auth, routeModel)
		if aliasResult.ForceMapping && responseAlias != "" {
			aliasResult.OriginalAlias = responseAlias
		}
		if len(models) > 1 {
			models = models[:1]
			pooled = false
		}
		if len(models) == 0 {
			releaseAttempt()
			if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "no_execution_models"); errEnd != nil {
				return cliproxyexecutor.Response{}, errEnd
			}
			lastErr = &Error{Code: "auth_not_found", Message: "no execution models available"}
			roundTiming.Observe(lastErr)
			continue
		}
		preparedAuth, errPrepare := m.prepareHomeRequestAuth(execCtx, selection.Executor, selection)
		if errPrepare != nil {
			stateModel := m.selectionModelKeyForAuth(auth, routeModel)
			if stateModel == "" {
				stateModel = canonicalModelKey(routeModel)
			}
			m.reportHomeResult(execCtx, Result{AuthID: auth.ID, Provider: selection.Provider, Model: stateModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(errPrepare), Options: opts}, auth)
			releaseAttempt()
			if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "prepare_failed"); errEnd != nil {
				return cliproxyexecutor.Response{}, errEnd
			}
			lastErr = errPrepare
			roundTiming.Observe(lastErr)
			continue
		}
		for _, upstreamModel := range models {
			execCtx = newUpstreamAttemptContext(execCtx)
			resultModel := m.stateModelForExecution(preparedAuth, routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			if restoreExecutionModel {
				execReq.Model = executionModel
			}
			execOpts := opts
			execOpts.ExecutionLifecycle = selection
			if selection != nil && selection.CanonicalSessionID != "" {
				meta := make(map[string]any, len(execOpts.Metadata)+2)
				for k, v := range execOpts.Metadata {
					meta[k] = v
				}
				meta[cliproxyexecutor.CanonicalSessionIDMetadataKey] = selection.CanonicalSessionID
				if selection.ParentSessionID != "" && selection.ParentSessionID != selection.CanonicalSessionID {
					meta[cliproxyexecutor.ParentSessionIDMetadataKey] = selection.ParentSessionID
				} else {
					delete(meta, cliproxyexecutor.ParentSessionIDMetadataKey)
				}
				execOpts.Metadata = meta
			}
			var errIntercept error
			execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(execCtx, selection.Executor, selection.Provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
			if errIntercept != nil {
				releaseAttempt()
				selection.End("request_intercepted")
				return cliproxyexecutor.Response{}, errIntercept
			}
			if !restoreExecutionModel {
				execReq = attachResolvedAPIKeyModelInfo(routing, execReq, preparedAuth, routeModel, upstreamModel)
				execReq = attachResolvedHomeModelInfo(execReq, selection.modelInfo)
			}
			if errCtx := execCtx.Err(); errCtx != nil {
				releaseAttempt()
				selection.End("attempt_canceled")
				return cliproxyexecutor.Response{}, errCtx
			}
			var response cliproxyexecutor.Response
			var errExecute error
			var effectiveAuthMu sync.RWMutex
			effectiveAuth := preparedAuth.Clone()
			setEffectiveAuth := func(auth *Auth) {
				if auth == nil || AccessTokenSHA256(auth) == "" {
					return
				}
				effectiveAuthMu.Lock()
				effectiveAuth = auth.Clone()
				effectiveAuthMu.Unlock()
			}
			getEffectiveAuth := func() (*Auth, string) {
				effectiveAuthMu.RLock()
				defer effectiveAuthMu.RUnlock()
				if effectiveAuth == nil {
					return nil, ""
				}
				return effectiveAuth.Clone(), AccessTokenSHA256(effectiveAuth)
			}
			execCtx = syncMetadataSessionToContext(execCtx, execOpts.Metadata)
			executorCtx := execCtx
			if countTokens {
				executorCtx = withAccessTokenFingerprintObserver(execCtx, setEffectiveAuth)
			}
			execute := func() (cliproxyexecutor.Response, error) {
				if countTokens {
					return selection.Executor.CountTokens(executorCtx, preparedAuth, execReq, execOpts)
				}
				return selection.Executor.Execute(diagnostics.ExecutorAttempt(execCtx, selection.Executor.Identifier()), preparedAuth, execReq, execOpts)
			}
			startHomeExec := time.Now()
			response, errExecute = execute()
			errExecute = markUpstreamExecutionAttemptFromContext(execCtx, errExecute)
			durationHomeExec := time.Since(startHomeExec)
			if countTokens {
				if _, fingerprint := getEffectiveAuth(); isUnauthorizedError(errExecute) {
					m.reportHomeUnauthorized(execCtx, preparedAuth, selection.Provider, resultModel, fingerprint, extractErrorBody(errExecute))
				}
			}
			if errExecute != nil {
				if hasUpstreamExecutionAttempt(errExecute) {
					upstreamErr = errExecute
				}
				warnLogUpstreamFailure(execCtx, entry, selection.Provider, upstreamModel, preparedAuth, durationHomeExec, errExecute)
			}
			result := Result{AuthID: preparedAuth.ID, Provider: selection.Provider, Model: resultModel, RouteModel: routeModel, Success: errExecute == nil, Options: execOpts}
			if errExecute == nil {
				m.reportHomeResult(execCtx, result, preparedAuth)
				releaseAttempt()
				attemptAliasResult := resolveAttemptAliasResult(routing, preparedAuth, routeModel, upstreamModel, aliasResult)
				rewriteForceMappedResponse(&response, attemptAliasResult)
				if !m.retainHomeWebsocketSelection(ctx, opts, routeModel, selection) {
					selection.End("completed")
				}
				return response, nil
			}
			result.Error = resultErrorFromError(errExecute)
			result.RetryAfter = retryAfterFromError(errExecute)
			if isCredentialScopedError(errExecute) {
				result.CredentialScope = true
			}
			action, okAction := matchRequestScopedErrorAction(preparedAuth, errExecute, m.runtimeConfigSnapshot())
			applyRequestScopedActionToResult(action, okAction, &result)
			m.reportHomeResult(execCtx, result, preparedAuth)
			lastErr = errExecute
			if okAction {
				if isRequestScopedStop(action, okAction) {
					releaseAttempt()
					selection.End("request_stopped")
					return cliproxyexecutor.Response{}, wrapRequestStopError(errExecute)
				}
				if result.CredentialScope {
					break
				}
				continue
			}
			if isRequestInvalidError(errExecute) {
				releaseAttempt()
				selection.End("request_invalid")
				return cliproxyexecutor.Response{}, errExecute
			}
			if result.CredentialScope {
				break
			}
		}
		roundTiming.Observe(lastErr)
		releaseAttempt()
		if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, "execution_failed"); errEnd != nil {
			return cliproxyexecutor.Response{}, errEnd
		}
		if errCtx := execCtx.Err(); errCtx != nil && ctx != nil && ctx.Err() != nil {
			return cliproxyexecutor.Response{}, errCtx
		}
	}
}

func homeExecutionAttemptContext(ctx context.Context, selection *HomeDispatchSelection) (context.Context, func(), error) {
	if selection == nil {
		return nil, func() {}, fmt.Errorf("Home dispatch selection is nil")
	}
	return selection.AttemptContext(ctx)
}

func wrapHomeStream(ctx context.Context, result *cliproxyexecutor.StreamResult, selection *HomeDispatchSelection, releaseAttempt func()) *cliproxyexecutor.StreamResult {
	if result == nil || result.Chunks == nil {
		if releaseAttempt != nil {
			releaseAttempt()
		}
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		if releaseAttempt != nil {
			defer releaseAttempt()
		}
		if selection != nil {
			defer selection.End("stream_closed")
		}
		forward := true
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				if !forward {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- chunk:
				}
				if chunk.Err != nil && selection != nil {
					forward = false
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

func sanitizeDownstreamWebsocketFallbackRequest(ctx context.Context, auth *Auth, req cliproxyexecutor.Request) cliproxyexecutor.Request {
	if !cliproxyexecutor.DownstreamWebsocket(ctx) || authWebsocketsEnabled(auth) || len(req.Payload) == 0 {
		return req
	}
	updated, errDelete := sjson.DeleteBytes(req.Payload, "generate")
	if errDelete != nil {
		return req
	}
	req.Payload = updated
	return req
}
```

## `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go`

SHA-256 (LF): `649f7f6ac682215ce14b07e69c0c07fa9d5117184baf42ece1a830e21407fa30`

```go
// Package openai provides response translation functionality for Gemini to OpenAI API compatibility.
// This package handles the conversion of Gemini API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertGeminiResponseToOpenAIChatParams holds parameters for response conversion.
type convertGeminiResponseToOpenAIChatParams struct {
	UnixTimestamp int64
	// FunctionIndex tracks tool call indices per candidate index to support multiple candidates.
	FunctionIndex        map[int]int
	SawToolCall          map[int]bool
	UpstreamFinishReason map[int]string
	SanitizedNameMap     map[string]string
}

// functionCallIDCounter provides a process-wide unique counter for function call identifiers.
var functionCallIDCounter uint64

// ConvertGeminiResponseToOpenAI translates a single chunk of a streaming response from the
// Gemini API format to the OpenAI Chat Completions streaming format.
// It processes various Gemini event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func ConvertGeminiResponseToOpenAI(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	// Initialize parameters if nil.
	if *param == nil {
		*param = &convertGeminiResponseToOpenAIChatParams{
			UnixTimestamp:        0,
			FunctionIndex:        make(map[int]int),
			SawToolCall:          make(map[int]bool),
			UpstreamFinishReason: make(map[int]string),
			SanitizedNameMap:     util.SanitizedToolNameMap(originalRequestRawJSON),
		}
	}

	// Ensure the Map is initialized (handling cases where param might be reused from older context).
	p := (*param).(*convertGeminiResponseToOpenAIChatParams)
	if p.FunctionIndex == nil {
		p.FunctionIndex = make(map[int]int)
	}
	if p.SawToolCall == nil {
		p.SawToolCall = make(map[int]bool)
	}
	if p.UpstreamFinishReason == nil {
		p.UpstreamFinishReason = make(map[int]string)
	}
	if p.SanitizedNameMap == nil {
		p.SanitizedNameMap = util.SanitizedToolNameMap(originalRequestRawJSON)
	}

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return [][]byte{}
	}

	// Initialize the OpenAI SSE base template.
	// We use a base template and clone it for each candidate to support multiple candidates.
	baseTemplate := []byte(`{"id":"","object":"chat.completion.chunk","created":12345,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)

	// Extract and set the model version.
	if modelVersionResult := gjson.GetBytes(rawJSON, "modelVersion"); modelVersionResult.Exists() {
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "model", modelVersionResult.String())
	}

	// Extract and set the creation timestamp.
	if createTimeResult := gjson.GetBytes(rawJSON, "createTime"); createTimeResult.Exists() {
		t, err := time.Parse(time.RFC3339Nano, createTimeResult.String())
		if err == nil {
			p.UnixTimestamp = t.Unix()
		}
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "created", p.UnixTimestamp)
	} else {
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "created", p.UnixTimestamp)
	}

	// Extract and set the response ID.
	if responseIDResult := gjson.GetBytes(rawJSON, "responseId"); responseIDResult.Exists() {
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "id", responseIDResult.String())
	}

	// Extract and set usage metadata (token counts).
	// Usage is applied to the base template so it appears in the chunks.
	if usageResult := gjson.GetBytes(rawJSON, "usageMetadata"); usageResult.Exists() {
		cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "usage.completion_tokens", usageResult.Get("candidatesTokenCount").Int()+usageResult.Get("thoughtsTokenCount").Int())
		if totalTokenCountResult := usageResult.Get("totalTokenCount"); totalTokenCountResult.Exists() {
			baseTemplate, _ = sjson.SetBytes(baseTemplate, "usage.total_tokens", totalTokenCountResult.Int())
		}
		promptTokenCount := usageResult.Get("promptTokenCount").Int()
		thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "usage.prompt_tokens", promptTokenCount)
		if thoughtsTokenCount > 0 {
			baseTemplate, _ = sjson.SetBytes(baseTemplate, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
		}
		// Include cached token count if present (indicates prompt caching is working)
		if cachedTokenCount > 0 {
			var err error
			baseTemplate, err = sjson.SetBytes(baseTemplate, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
			if err != nil {
				log.Warnf("gemini openai response: failed to set cached_tokens in streaming: %v", err)
			}
		}
	}

	var responseStrings [][]byte
	candidates := gjson.GetBytes(rawJSON, "candidates")

	// Iterate over all candidates to support candidate_count > 1.
	if candidates.IsArray() {
		candidates.ForEach(func(_, candidate gjson.Result) bool {
			// Clone the template for the current candidate.
			template := append([]byte(nil), baseTemplate...)

			// Set the specific index for this candidate.
			candidateIndex := int(candidate.Get("index").Int())
			template, _ = sjson.SetBytes(template, "choices.0.index", candidateIndex)

			if finishReasonResult := candidate.Get("finishReason"); finishReasonResult.Exists() {
				p.UpstreamFinishReason[candidateIndex] = strings.ToUpper(finishReasonResult.String())
			}

			partsResult := candidate.Get("content.parts")
			assistantRoleSet := false
			setAssistantRole := func() {
				if assistantRoleSet {
					return
				}
				template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
				assistantRoleSet = true
			}

			if partsResult.IsArray() {
				partResults := partsResult.Array()
				for i := 0; i < len(partResults); i++ {
					partResult := partResults[i]
					partTextResult := partResult.Get("text")
					functionCallResult := partResult.Get("functionCall")
					inlineDataResult := partResult.Get("inlineData")
					if !inlineDataResult.Exists() {
						inlineDataResult = partResult.Get("inline_data")
					}
					thoughtSignatureResult := partResult.Get("thoughtSignature")
					if !thoughtSignatureResult.Exists() {
						thoughtSignatureResult = partResult.Get("thought_signature")
					}

					// Speech-to-text models (gemini-3.5-transcribe) deliver the
					// transcript in an audioTranscription part instead of text.
					audioTranscriptionResult := partResult.Get("audioTranscription")
					if audioTranscriptionResult.Exists() && !partTextResult.Exists() {
						partTextResult = audioTranscriptionResult.Get("text")
					}

					hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
					hasContentPayload := partTextResult.Exists() || functionCallResult.Exists() || inlineDataResult.Exists()

					// Skip pure thoughtSignature parts but keep any actual payload in the same part.
					if hasThoughtSignature && !hasContentPayload {
						continue
					}

					if partTextResult.Exists() {
						text := partTextResult.String()
						setAssistantRole()
						// Handle text content, distinguishing between regular content and reasoning/thoughts.
						if partResult.Get("thought").Bool() {
							template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", text)
						} else {
							template, _ = sjson.SetBytes(template, "choices.0.delta.content", text)
						}
					} else if functionCallResult.Exists() {
						// Handle function call content.
						p.SawToolCall[candidateIndex] = true
						toolCallsResult := gjson.GetBytes(template, "choices.0.delta.tool_calls")

						// Retrieve the function index for this specific candidate.
						functionCallIndex := p.FunctionIndex[candidateIndex]
						p.FunctionIndex[candidateIndex]++

						if toolCallsResult.Exists() && toolCallsResult.IsArray() {
							functionCallIndex = len(toolCallsResult.Array())
						} else {
							template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
						}

						functionCallTemplate := []byte(`{"id":"","index":0,"type":"function","function":{"name":"","arguments":""}}`)
						fcName := util.RestoreSanitizedToolName(p.SanitizedNameMap, functionCallResult.Get("name").String())
						functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "id", fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1)))
						functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "index", functionCallIndex)
						functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.name", fcName)
						if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.arguments", fcArgsResult.Raw)
						}
						setAssistantRole()
						template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallTemplate)
					} else if inlineDataResult.Exists() {
						data := inlineDataResult.Get("data").String()
						if data == "" {
							continue
						}
						mimeType := inlineDataResult.Get("mimeType").String()
						if mimeType == "" {
							mimeType = inlineDataResult.Get("mime_type").String()
						}
						if mimeType == "" {
							mimeType = "image/png"
						}
						imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
						imagesResult := gjson.GetBytes(template, "choices.0.delta.images")
						if !imagesResult.Exists() || !imagesResult.IsArray() {
							template, _ = sjson.SetRawBytes(template, "choices.0.delta.images", []byte(`[]`))
						}
						imageIndex := len(gjson.GetBytes(template, "choices.0.delta.images").Array())
						imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
						imagePayload, _ = sjson.SetBytes(imagePayload, "index", imageIndex)
						imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
						setAssistantRole()
						template, _ = sjson.SetRawBytes(template, "choices.0.delta.images.-1", imagePayload)
					}
				}
			}

			upstreamFinishReason := p.UpstreamFinishReason[candidateIndex]
			sawToolCall := p.SawToolCall[candidateIndex]
			usageExists := gjson.GetBytes(rawJSON, "usageMetadata").Exists()
			isFinalChunk := upstreamFinishReason != "" && usageExists

			if isFinalChunk {
				var finishReason string
				if sawToolCall {
					finishReason = "tool_calls"
				} else if upstreamFinishReason == "MAX_TOKENS" {
					finishReason = "max_tokens"
				} else {
					finishReason = "stop"
				}
				template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)
				template, _ = sjson.SetBytes(template, "choices.0.native_finish_reason", strings.ToLower(upstreamFinishReason))
			}

			responseStrings = append(responseStrings, template)
			return true // continue loop
		})
	} else {
		// If there are no candidates (e.g., a pure usageMetadata chunk), return the usage chunk if present.
		if gjson.GetBytes(rawJSON, "usageMetadata").Exists() && len(responseStrings) == 0 {
			responseStrings = append(responseStrings, append([]byte(nil), baseTemplate...))
		}
	}

	return responseStrings
}

// ConvertGeminiResponseToOpenAINonStream converts a non-streaming Gemini response to a non-streaming OpenAI response.
// This function processes the complete Gemini response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertGeminiResponseToOpenAINonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	sanitizedNameMap := util.SanitizedToolNameMap(originalRequestRawJSON)
	var unixTimestamp int64
	// Initialize template with an empty choices array to support multiple candidates.
	template := []byte(`{"id":"","object":"chat.completion","created":123456,"model":"model","choices":[]}`)

	if modelVersionResult := gjson.GetBytes(rawJSON, "modelVersion"); modelVersionResult.Exists() {
		template, _ = sjson.SetBytes(template, "model", modelVersionResult.String())
	}

	if createTimeResult := gjson.GetBytes(rawJSON, "createTime"); createTimeResult.Exists() {
		t, err := time.Parse(time.RFC3339Nano, createTimeResult.String())
		if err == nil {
			unixTimestamp = t.Unix()
		}
		template, _ = sjson.SetBytes(template, "created", unixTimestamp)
	} else {
		template, _ = sjson.SetBytes(template, "created", unixTimestamp)
	}

	if responseIDResult := gjson.GetBytes(rawJSON, "responseId"); responseIDResult.Exists() {
		template, _ = sjson.SetBytes(template, "id", responseIDResult.String())
	}

	if usageResult := gjson.GetBytes(rawJSON, "usageMetadata"); usageResult.Exists() {
		template, _ = sjson.SetBytes(template, "usage.completion_tokens", usageResult.Get("candidatesTokenCount").Int()+usageResult.Get("thoughtsTokenCount").Int())
		if totalTokenCountResult := usageResult.Get("totalTokenCount"); totalTokenCountResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokenCountResult.Int())
		}
		promptTokenCount := usageResult.Get("promptTokenCount").Int()
		thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
		cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
		template, _ = sjson.SetBytes(template, "usage.prompt_tokens", promptTokenCount)
		if thoughtsTokenCount > 0 {
			template, _ = sjson.SetBytes(template, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
		}
		// Include cached token count if present (indicates prompt caching is working)
		if cachedTokenCount > 0 {
			var err error
			template, err = sjson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
			if err != nil {
				log.Warnf("gemini openai response: failed to set cached_tokens in non-streaming: %v", err)
			}
		}
	}

	// Process the main content part of the response for all candidates.
	candidates := gjson.GetBytes(rawJSON, "candidates")
	if candidates.IsArray() {
		var choicesList [][]byte
		candidates.ForEach(func(_, candidate gjson.Result) bool {
			// Construct a single Choice object.
			choiceTemplate := []byte(`{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}`)

			// Set the index for this choice.
			choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "index", candidate.Get("index").Int())

			// Set finish reason.
			if finishReasonResult := candidate.Get("finishReason"); finishReasonResult.Exists() {
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "finish_reason", strings.ToLower(finishReasonResult.String()))
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "native_finish_reason", strings.ToLower(finishReasonResult.String()))
			}

			partsResult := candidate.Get("content.parts")
			hasFunctionCall := false
			if partsResult.IsArray() {
				partsResults := partsResult.Array()
				var toolCalls [][]byte
				var images [][]byte
				var textContent strings.Builder
				var reasoningContent strings.Builder
				hasTextContent := false
				hasReasoningContent := false

				for i := 0; i < len(partsResults); i++ {
					partResult := partsResults[i]
					partTextResult := partResult.Get("text")
					functionCallResult := partResult.Get("functionCall")
					inlineDataResult := partResult.Get("inlineData")
					if !inlineDataResult.Exists() {
						inlineDataResult = partResult.Get("inline_data")
					}

					// Speech-to-text models (gemini-3.5-transcribe) deliver the
					// transcript in an audioTranscription part instead of text.
					audioTranscriptionResult := partResult.Get("audioTranscription")
					if audioTranscriptionResult.Exists() && !partTextResult.Exists() {
						partTextResult = audioTranscriptionResult.Get("text")
					}

					if partTextResult.Exists() {
						// Append text content, distinguishing between regular content and reasoning.
						if partResult.Get("thought").Bool() {
							hasReasoningContent = true
							reasoningContent.WriteString(partTextResult.String())
						} else {
							hasTextContent = true
							textContent.WriteString(partTextResult.String())
						}
					} else if functionCallResult.Exists() {
						// Append function call content to the tool_calls array.
						hasFunctionCall = true
						functionCallItemTemplate := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
						fcName := util.RestoreSanitizedToolName(sanitizedNameMap, functionCallResult.Get("name").String())
						functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "id", fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1)))
						functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.name", fcName)
						if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
							functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.arguments", fcArgsResult.Raw)
						}
						toolCalls = append(toolCalls, functionCallItemTemplate)
					} else if inlineDataResult.Exists() {
						data := inlineDataResult.Get("data").String()
						if data != "" {
							mimeType := inlineDataResult.Get("mimeType").String()
							if mimeType == "" {
								mimeType = inlineDataResult.Get("mime_type").String()
							}
							if mimeType == "" {
								mimeType = "image/png"
							}
							imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
							imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
							imagePayload, _ = sjson.SetBytes(imagePayload, "index", len(images))
							imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
							images = append(images, imagePayload)
						}
					}
				}

				if hasTextContent {
					choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "message.content", textContent.String())
				}
				if hasReasoningContent {
					choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "message.reasoning_content", reasoningContent.String())
				}
				if len(toolCalls) > 0 {
					choiceTemplate, _ = sjson.SetRawBytes(choiceTemplate, "message.tool_calls", translatorcommon.JoinRawArray(toolCalls))
				}
				if len(images) > 0 {
					choiceTemplate, _ = sjson.SetRawBytes(choiceTemplate, "message.images", translatorcommon.JoinRawArray(images))
				}
			}

			if hasFunctionCall {
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "finish_reason", "tool_calls")
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "native_finish_reason", "tool_calls")
			}

			// Append the constructed choice to the main choices array.
			choicesList = append(choicesList, choiceTemplate)
			return true
		})
		if len(choicesList) > 0 {
			template = translatorcommon.SetRawArrayItems(template, "choices", choicesList)
		}
	}

	return template
}
```

## `internal/translator/gemini/openai/responses/gemini_openai-responses_response.go`

SHA-256 (LF): `4e0413a3c90ccf6350dff00a5f88dbb95aa26168a1415de41f0fbc80d02c9905`

```go
package responses

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type geminiDetachedReasoningItem struct {
	Index     int
	ID        string
	Signature string
}

type geminiCompletedMessageItem struct {
	ID          string
	Text        string
	Annotations [][]byte
}

type geminiCompletedReasoningItem struct {
	ID        string
	Signature string
	Text      string
}

type geminiToResponsesState struct {
	Seq        int
	ResponseID string
	CreatedAt  int64
	Started    bool
	Completed  bool

	// message aggregation
	MsgOpened    bool
	MsgClosed    bool
	MsgIndex     int
	CurrentMsgID string
	ItemTextBuf  strings.Builder

	// reasoning aggregation
	ReasoningOpened           bool
	ReasoningIndex            int
	ReasoningItemID           string
	ReasoningEnc              string
	ReasoningDirection        string
	ReasoningTargetKind       string
	ReasoningBuf              strings.Builder
	ReasoningPendingDeltas    []string
	ReasoningClosed           bool
	PendingReasoningSignature string
	DetachedReasoning         map[int]geminiDetachedReasoningItem
	CompletedMessages         map[int]geminiCompletedMessageItem
	CompletedReasoning        map[int]geminiCompletedReasoningItem
	SeenReasoningSignatures   map[string]bool
	LastSemanticKind          string
	HiddenTextSignatures      map[string][]string

	// function call aggregation (keyed by output_index)
	NextIndex        int
	FuncArgsBuf      map[int]*strings.Builder
	FuncInputBuf     map[int]string
	FuncCustom       map[int]bool
	FuncNames        map[int]string
	FuncNamespaces   map[int]string
	FuncCallIDs      map[int]string
	FuncDone         map[int]bool
	SanitizedNameMap map[string]string
	ToolIdentityMap  map[string]util.ResponsesToolIdentity

	// web search aggregation
	WebSearchStreamMode          bool
	WebSearchOpened              bool
	WebSearchDone                bool
	WebSearchIndex               int
	WebSearchItemID              string
	WebSearchDoneItem            []byte
	WebSearchQuery               string
	WebSearchQueries             []string
	WebSearchSources             [][]byte
	WebSearchAnnotations         [][]byte
	WebSearchAnnotationsAttached bool
	WebSearchBufferedDeltas      []string
	WebSearchBufferedParts       []geminiStreamBufferedPart
	RawGroundingMetadata         gjson.Result
	PartMappings                 []GeminiPartMapping
	StreamPartIndex              int
	CurrentLogicalPartIndex      int
	CurrentPartKind              string
	HasSeenFirstPart             bool
	TextPartRunActive            bool
	CurrentMsgRuneOffset         int64
	EmittedAnnotationCount       map[int]int
}

type geminiStreamBufferedPart struct {
	PartIndex int
	Text      string
}

// responseIDCounter provides a process-wide unique counter for synthesized response identifiers.
var responseIDCounter uint64

// funcCallIDCounter provides a process-wide unique counter for function call identifiers.
var funcCallIDCounter uint64

func pickRequestJSON(originalRequestRawJSON, requestRawJSON []byte) []byte {
	if len(originalRequestRawJSON) > 0 && gjson.ValidBytes(originalRequestRawJSON) {
		return originalRequestRawJSON
	}
	if len(requestRawJSON) > 0 && gjson.ValidBytes(requestRawJSON) {
		return requestRawJSON
	}
	return nil
}

func unwrapRequestRoot(root gjson.Result) gjson.Result {
	req := root.Get("request")
	if !req.Exists() {
		return root
	}
	if req.Get("model").Exists() || req.Get("input").Exists() || req.Get("instructions").Exists() {
		return req
	}
	return root
}

func unwrapGeminiResponseRoot(root gjson.Result) gjson.Result {
	resp := root.Get("response")
	if !resp.Exists() {
		return root
	}
	// Vertex-style Gemini responses wrap the actual payload in a "response" object.
	if resp.Get("candidates").Exists() || resp.Get("responseId").Exists() || resp.Get("usageMetadata").Exists() {
		return resp
	}
	return root
}

func emitEvent(event string, payload []byte) []byte {
	return translatorcommon.SSEEventData(event, payload)
}

func hasEffectiveGoogleSearchTool(rawJSON []byte) bool {
	if len(rawJSON) == 0 {
		return false
	}
	if gjson.GetBytes(rawJSON, "requestType").String() == "web_search" {
		return true
	}
	for _, path := range []string{"request.tools", "tools"} {
		tools := gjson.GetBytes(rawJSON, path)
		if !tools.IsArray() {
			continue
		}
		for _, tool := range tools.Array() {
			if tool.Get("googleSearch").Exists() {
				return true
			}
		}
	}
	return false
}

func isUpstreamGeminiRequest(rawJSON []byte) bool {
	if len(rawJSON) == 0 {
		return false
	}
	if gjson.GetBytes(rawJSON, "requestType").Exists() {
		return true
	}
	for _, path := range []string{"contents", "request.contents"} {
		if gjson.GetBytes(rawJSON, path).Exists() {
			return true
		}
	}
	return false
}

func determineWebSearchStreamMode(modelName, requestModelName string, originalRequestRawJSON, requestRawJSON []byte) bool {
	if len(originalRequestRawJSON) > 0 {
		origRoot := unwrapRequestRoot(gjson.ParseBytes(originalRequestRawJSON))
		if !AllowsResponsesWebSearchToolChoice(origRoot) {
			return false
		}
	}
	if len(requestRawJSON) > 0 {
		reqRoot := unwrapRequestRoot(gjson.ParseBytes(requestRawJSON))
		if reqRoot.Get("tool_choice").Exists() && !AllowsResponsesWebSearchToolChoice(reqRoot) {
			return false
		}
		if isUpstreamGeminiRequest(requestRawJSON) || hasEffectiveGoogleSearchTool(requestRawJSON) {
			return hasEffectiveGoogleSearchTool(requestRawJSON)
		}
	}
	reqJSON := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
	if len(reqJSON) > 0 {
		reqRoot := unwrapRequestRoot(gjson.ParseBytes(reqJSON))
		return HasResponsesWebSearchTool(reqRoot) &&
			AllowsResponsesWebSearchToolChoice(reqRoot) &&
			(ModelSupportsWebSearch(modelName) || ModelSupportsWebSearch(requestModelName))
	}
	return false
}

// ConvertGeminiResponseToOpenAIResponses converts Gemini SSE chunks into OpenAI Responses SSE events.
func ConvertGeminiResponseToOpenAIResponses(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	reqJSON := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
	if *param == nil {
		*param = &geminiToResponsesState{
			FuncArgsBuf:             make(map[int]*strings.Builder),
			FuncInputBuf:            make(map[int]string),
			FuncCustom:              make(map[int]bool),
			FuncNames:               make(map[int]string),
			FuncNamespaces:          make(map[int]string),
			FuncCallIDs:             make(map[int]string),
			FuncDone:                make(map[int]bool),
			DetachedReasoning:       make(map[int]geminiDetachedReasoningItem),
			CompletedMessages:       make(map[int]geminiCompletedMessageItem),
			CompletedReasoning:      make(map[int]geminiCompletedReasoningItem),
			SeenReasoningSignatures: make(map[string]bool),
			SanitizedNameMap:        util.SanitizedToolNameMap(originalRequestRawJSON),
			ToolIdentityMap:         util.ResponsesToolReverseIdentityMap(reqJSON),
			EmittedAnnotationCount:  make(map[int]int),
		}
	}
	st := (*param).(*geminiToResponsesState)
	if st.FuncArgsBuf == nil {
		st.FuncArgsBuf = make(map[int]*strings.Builder)
	}
	if st.FuncInputBuf == nil {
		st.FuncInputBuf = make(map[int]string)
	}
	if st.FuncCustom == nil {
		st.FuncCustom = make(map[int]bool)
	}
	if st.FuncNames == nil {
		st.FuncNames = make(map[int]string)
	}
	if st.FuncNamespaces == nil {
		st.FuncNamespaces = make(map[int]string)
	}
	if st.FuncCallIDs == nil {
		st.FuncCallIDs = make(map[int]string)
	}
	if st.FuncDone == nil {
		st.FuncDone = make(map[int]bool)
	}
	if st.DetachedReasoning == nil {
		st.DetachedReasoning = make(map[int]geminiDetachedReasoningItem)
	}
	if st.CompletedMessages == nil {
		st.CompletedMessages = make(map[int]geminiCompletedMessageItem)
	}
	if st.CompletedReasoning == nil {
		st.CompletedReasoning = make(map[int]geminiCompletedReasoningItem)
	}
	if st.SeenReasoningSignatures == nil {
		st.SeenReasoningSignatures = make(map[string]bool)
	}
	if st.SanitizedNameMap == nil {
		st.SanitizedNameMap = util.SanitizedToolNameMap(originalRequestRawJSON)
	}
	if st.ToolIdentityMap == nil {
		st.ToolIdentityMap = util.ResponsesToolReverseIdentityMap(reqJSON)
	}
	if st.EmittedAnnotationCount == nil {
		st.EmittedAnnotationCount = make(map[int]int)
	}

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	rawJSON = bytes.TrimSpace(rawJSON)
	if len(rawJSON) == 0 || st.Completed {
		return [][]byte{}
	}
	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		if !st.Started {
			return [][]byte{}
		}
		rawJSON = []byte(`{"candidates":[{"finishReason":"STOP"}]}`)
	}

	root := gjson.ParseBytes(rawJSON)
	if !root.Exists() {
		return [][]byte{}
	}
	root = unwrapGeminiResponseRoot(root)

	var out [][]byte
	nextSeq := func() int { st.Seq++; return st.Seq }

	reasoningEncryptedContent := func() string {
		if st.ReasoningEnc == "" || st.ReasoningDirection == "" {
			return st.ReasoningEnc
		}
		return encodeGeminiResponsesCarrier(st.ReasoningEnc, st.ReasoningDirection, st.ReasoningTargetKind)
	}
	finalizeWebSearch := func() {
		if !st.WebSearchOpened || st.WebSearchDone {
			return
		}
		if st.WebSearchQuery == "" && len(st.WebSearchQueries) > 0 {
			st.WebSearchQuery = st.WebSearchQueries[0]
		}
		if st.WebSearchQuery == "" && len(reqJSON) > 0 {
			reqRoot := unwrapRequestRoot(gjson.ParseBytes(reqJSON))
			st.WebSearchQuery = ExtractResponsesWebSearchQuery(reqRoot)
		}

		completed := []byte(`{"type":"response.web_search_call.completed","sequence_number":0,"output_index":0,"item_id":""}`)
		completed, _ = sjson.SetBytes(completed, "sequence_number", nextSeq())
		completed, _ = sjson.SetBytes(completed, "output_index", st.WebSearchIndex)
		completed, _ = sjson.SetBytes(completed, "item_id", st.WebSearchItemID)
		out = append(out, emitEvent("response.web_search_call.completed", completed))

		doneItem := BuildResponsesWebSearchCallItem(st.WebSearchItemID, st.WebSearchQuery, st.WebSearchQueries, st.WebSearchSources)
		st.WebSearchDoneItem = doneItem
		doneEvent := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0}`)
		doneEvent, _ = sjson.SetBytes(doneEvent, "sequence_number", nextSeq())
		doneEvent, _ = sjson.SetBytes(doneEvent, "output_index", st.WebSearchIndex)
		doneEvent, _ = sjson.SetRawBytes(doneEvent, "item", doneItem)
		out = append(out, emitEvent("response.output_item.done", doneEvent))
		st.WebSearchDone = true
	}
	openReasoning := func() {
		if st.ReasoningOpened || st.ReasoningClosed || (st.ReasoningBuf.Len() == 0 && st.ReasoningEnc == "") {
			return
		}
		finalizeWebSearch()
		st.ReasoningOpened = true
		st.ReasoningIndex = st.NextIndex
		st.NextIndex++
		st.ReasoningItemID = fmt.Sprintf("rs_%s_%d", st.ResponseID, st.ReasoningIndex)
		item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","encrypted_content":"","summary":[]}}`)
		item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
		item, _ = sjson.SetBytes(item, "output_index", st.ReasoningIndex)
		item, _ = sjson.SetBytes(item, "item.id", st.ReasoningItemID)
		item, _ = sjson.SetBytes(item, "item.encrypted_content", reasoningEncryptedContent())
		out = append(out, emitEvent("response.output_item.added", item))
		partAdded := []byte(`{"type":"response.reasoning_summary_part.added","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
		partAdded, _ = sjson.SetBytes(partAdded, "sequence_number", nextSeq())
		partAdded, _ = sjson.SetBytes(partAdded, "item_id", st.ReasoningItemID)
		partAdded, _ = sjson.SetBytes(partAdded, "output_index", st.ReasoningIndex)
		out = append(out, emitEvent("response.reasoning_summary_part.added", partAdded))
		for _, delta := range st.ReasoningPendingDeltas {
			msg := []byte(`{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"delta":""}`)
			msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
			msg, _ = sjson.SetBytes(msg, "item_id", st.ReasoningItemID)
			msg, _ = sjson.SetBytes(msg, "output_index", st.ReasoningIndex)
			msg, _ = sjson.SetBytes(msg, "delta", delta)
			out = append(out, emitEvent("response.reasoning_summary_text.delta", msg))
		}
		st.ReasoningPendingDeltas = nil
	}

	// Helper to finalize reasoning summary events in correct order.
	// It emits response.reasoning_summary_text.done followed by
	// response.reasoning_summary_part.done exactly once.
	finalizeReasoning := func() {
		openReasoning()
		if !st.ReasoningOpened || st.ReasoningClosed {
			return
		}
		full := st.ReasoningBuf.String()
		textDone := []byte(`{"type":"response.reasoning_summary_text.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`)
		textDone, _ = sjson.SetBytes(textDone, "sequence_number", nextSeq())
		textDone, _ = sjson.SetBytes(textDone, "item_id", st.ReasoningItemID)
		textDone, _ = sjson.SetBytes(textDone, "output_index", st.ReasoningIndex)
		textDone, _ = sjson.SetBytes(textDone, "text", full)
		out = append(out, emitEvent("response.reasoning_summary_text.done", textDone))

		partDone := []byte(`{"type":"response.reasoning_summary_part.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
		partDone, _ = sjson.SetBytes(partDone, "sequence_number", nextSeq())
		partDone, _ = sjson.SetBytes(partDone, "item_id", st.ReasoningItemID)
		partDone, _ = sjson.SetBytes(partDone, "output_index", st.ReasoningIndex)
		partDone, _ = sjson.SetBytes(partDone, "part.text", full)
		out = append(out, emitEvent("response.reasoning_summary_part.done", partDone))

		itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":""}]}}`)
		itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
		itemDone, _ = sjson.SetBytes(itemDone, "item.id", st.ReasoningItemID)
		itemDone, _ = sjson.SetBytes(itemDone, "output_index", st.ReasoningIndex)
		itemDone, _ = sjson.SetBytes(itemDone, "item.encrypted_content", reasoningEncryptedContent())
		itemDone, _ = sjson.SetBytes(itemDone, "item.summary.0.text", full)
		out = append(out, emitEvent("response.output_item.done", itemDone))

		st.CompletedReasoning[st.ReasoningIndex] = geminiCompletedReasoningItem{
			ID:        st.ReasoningItemID,
			Signature: reasoningEncryptedContent(),
			Text:      full,
		}
		st.ReasoningClosed = true
	}

	resetReasoning := func() {
		st.ReasoningOpened = false
		st.ReasoningClosed = false
		st.ReasoningIndex = 0
		st.ReasoningItemID = ""
		st.ReasoningEnc = ""
		st.ReasoningDirection = ""
		st.ReasoningTargetKind = ""
		st.ReasoningBuf.Reset()
		st.ReasoningPendingDeltas = nil
	}

	openWebSearch := func() {
		if st.WebSearchOpened {
			return
		}
		finalizeReasoning()
		st.WebSearchOpened = true
		st.WebSearchIndex = st.NextIndex
		st.NextIndex++
		st.WebSearchItemID = fmt.Sprintf("ws_%s", strings.TrimPrefix(st.ResponseID, "resp_"))
		if st.WebSearchQuery == "" && len(st.WebSearchQueries) > 0 {
			st.WebSearchQuery = st.WebSearchQueries[0]
		}
		if st.WebSearchQuery == "" && len(reqJSON) > 0 {
			reqRoot := unwrapRequestRoot(gjson.ParseBytes(reqJSON))
			st.WebSearchQuery = ExtractResponsesWebSearchQuery(reqRoot)
		}

		added := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"web_search_call","status":"in_progress","action":{"type":"search","query":""}}}`)
		added, _ = sjson.SetBytes(added, "sequence_number", nextSeq())
		added, _ = sjson.SetBytes(added, "output_index", st.WebSearchIndex)
		added, _ = sjson.SetBytes(added, "item.id", st.WebSearchItemID)
		added, _ = sjson.SetBytes(added, "item.action.query", st.WebSearchQuery)
		out = append(out, emitEvent("response.output_item.added", added))

		searching := []byte(`{"type":"response.web_search_call.searching","sequence_number":0,"output_index":0,"item_id":""}`)
		searching, _ = sjson.SetBytes(searching, "sequence_number", nextSeq())
		searching, _ = sjson.SetBytes(searching, "output_index", st.WebSearchIndex)
		searching, _ = sjson.SetBytes(searching, "item_id", st.WebSearchItemID)
		out = append(out, emitEvent("response.web_search_call.searching", searching))
	}

	flushWebSearchBufferedText := func() {
		finalizeWebSearch()
		if len(st.WebSearchBufferedDeltas) == 0 {
			return
		}
		if st.MsgClosed {
			st.MsgOpened = false
			st.MsgClosed = false
			st.ItemTextBuf.Reset()
			st.CurrentMsgRuneOffset = 0
		}
		if !st.MsgOpened {
			st.MsgOpened = true
			st.MsgIndex = st.NextIndex
			st.NextIndex++
			st.CurrentMsgID = fmt.Sprintf("msg_%s_%d", st.ResponseID, st.MsgIndex)
			item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`)
			item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
			item, _ = sjson.SetBytes(item, "output_index", st.MsgIndex)
			item, _ = sjson.SetBytes(item, "item.id", st.CurrentMsgID)
			out = append(out, emitEvent("response.output_item.added", item))
			partAdded := []byte(`{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
			partAdded, _ = sjson.SetBytes(partAdded, "sequence_number", nextSeq())
			partAdded, _ = sjson.SetBytes(partAdded, "item_id", st.CurrentMsgID)
			partAdded, _ = sjson.SetBytes(partAdded, "output_index", st.MsgIndex)
			out = append(out, emitEvent("response.content_part.added", partAdded))
			st.ItemTextBuf.Reset()
			st.CurrentMsgRuneOffset = 0
		}
		for _, delta := range st.WebSearchBufferedDeltas {
			st.ItemTextBuf.WriteString(delta)
			msg := []byte(`{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`)
			msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
			msg, _ = sjson.SetBytes(msg, "item_id", st.CurrentMsgID)
			msg, _ = sjson.SetBytes(msg, "output_index", st.MsgIndex)
			msg, _ = sjson.SetBytes(msg, "delta", delta)
			out = append(out, emitEvent("response.output_text.delta", msg))
		}
		for _, bp := range st.WebSearchBufferedParts {
			n := len(st.PartMappings)
			if n > 0 && st.PartMappings[n-1].PartIndex == bp.PartIndex && st.PartMappings[n-1].MessageIndex == st.MsgIndex {
				st.PartMappings[n-1].PartText += bp.Text
			} else {
				st.PartMappings = append(st.PartMappings, GeminiPartMapping{
					PartIndex:      bp.PartIndex,
					MessageIndex:   st.MsgIndex,
					StartRuneInMsg: st.CurrentMsgRuneOffset,
					PartText:       bp.Text,
				})
			}
			st.CurrentMsgRuneOffset += int64(utf8.RuneCountInString(bp.Text))
		}
		st.WebSearchBufferedDeltas = nil
		st.WebSearchBufferedParts = nil
	}

	emitNewCitationAnnotations := func(msgIndex int, itemID string, annotations [][]byte) {
		emitted := st.EmittedAnnotationCount[msgIndex]
		for annIdx := emitted; annIdx < len(annotations); annIdx++ {
			annEvent := []byte(`{"type":"response.output_text.annotation.added","sequence_number":0,"response_id":"","item_id":"","output_index":0,"content_index":0,"annotation_index":0}`)
			annEvent, _ = sjson.SetBytes(annEvent, "sequence_number", nextSeq())
			annEvent, _ = sjson.SetBytes(annEvent, "response_id", st.ResponseID)
			annEvent, _ = sjson.SetBytes(annEvent, "item_id", itemID)
			annEvent, _ = sjson.SetBytes(annEvent, "output_index", msgIndex)
			annEvent, _ = sjson.SetBytes(annEvent, "content_index", 0)
			annEvent, _ = sjson.SetBytes(annEvent, "annotation_index", annIdx)
			annEvent, _ = sjson.SetRawBytes(annEvent, "annotation", annotations[annIdx])
			out = append(out, emitEvent("response.output_text.annotation.added", annEvent))
		}
		if len(annotations) > emitted {
			st.EmittedAnnotationCount[msgIndex] = len(annotations)
		}
	}

	// Helper to finalize the assistant message in correct order.
	// It emits response.output_text.annotation.added for any new citations,
	// then response.output_text.done, response.content_part.done,
	// and response.output_item.done exactly once.
	finalizeMessage := func() {
		finalizeWebSearch()
		if len(st.WebSearchBufferedDeltas) > 0 {
			flushWebSearchBufferedText()
		}
		if !st.MsgOpened || st.MsgClosed {
			return
		}
		fullText := st.ItemTextBuf.String()
		var msgCitations [][]byte
		if st.RawGroundingMetadata.Exists() {
			cMap := BuildResponsesURLCitationsForMessages(st.RawGroundingMetadata, st.PartMappings, []string{fullText})
			msgCitations = cMap[st.MsgIndex]
			if len(msgCitations) == 0 && len(st.CompletedMessages) == 0 && len(cMap[0]) > 0 {
				msgCitations = cMap[0]
			}
			st.WebSearchAnnotations = msgCitations
		}
		emitNewCitationAnnotations(st.MsgIndex, st.CurrentMsgID, msgCitations)
		done := []byte(`{"type":"response.output_text.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"text":"","logprobs":[]}`)
		done, _ = sjson.SetBytes(done, "sequence_number", nextSeq())
		done, _ = sjson.SetBytes(done, "item_id", st.CurrentMsgID)
		done, _ = sjson.SetBytes(done, "output_index", st.MsgIndex)
		done, _ = sjson.SetBytes(done, "text", fullText)
		out = append(out, emitEvent("response.output_text.done", done))
		partDone := []byte(`{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
		partDone, _ = sjson.SetBytes(partDone, "sequence_number", nextSeq())
		partDone, _ = sjson.SetBytes(partDone, "item_id", st.CurrentMsgID)
		partDone, _ = sjson.SetBytes(partDone, "output_index", st.MsgIndex)
		partDone, _ = sjson.SetBytes(partDone, "part.text", fullText)
		if len(msgCitations) > 0 {
			partDone, _ = sjson.SetRawBytes(partDone, "part.annotations", translatorcommon.JoinRawArray(msgCitations))
		}
		out = append(out, emitEvent("response.content_part.done", partDone))
		final := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}}`)
		final, _ = sjson.SetBytes(final, "sequence_number", nextSeq())
		final, _ = sjson.SetBytes(final, "output_index", st.MsgIndex)
		final, _ = sjson.SetBytes(final, "item.id", st.CurrentMsgID)
		final, _ = sjson.SetBytes(final, "item.content.0.text", fullText)
		if len(msgCitations) > 0 {
			final, _ = sjson.SetRawBytes(final, "item.content.0.annotations", translatorcommon.JoinRawArray(msgCitations))
			st.WebSearchAnnotationsAttached = true
		}
		out = append(out, emitEvent("response.output_item.done", final))

		st.CompletedMessages[st.MsgIndex] = geminiCompletedMessageItem{
			ID:          st.CurrentMsgID,
			Text:        fullText,
			Annotations: msgCitations,
		}
		st.MsgClosed = true
		st.CurrentMsgRuneOffset = 0
	}

	emitLateCitations := func() {
		if !st.RawGroundingMetadata.Exists() || len(st.CompletedMessages) == 0 {
			return
		}
		msgTexts := make([]string, 0, len(st.CompletedMessages))
		for idx := 0; idx < st.NextIndex; idx++ {
			if msg, ok := st.CompletedMessages[idx]; ok {
				msgTexts = append(msgTexts, msg.Text)
			}
		}
		lateCitationsMap := BuildResponsesURLCitationsForMessages(st.RawGroundingMetadata, st.PartMappings, msgTexts)
		if lateCitationsMap == nil {
			return
		}
		for idx := 0; idx < st.NextIndex; idx++ {
			completedMessage, ok := st.CompletedMessages[idx]
			if !ok {
				continue
			}
			lateCites := lateCitationsMap[idx]
			if len(lateCites) == 0 && len(st.CompletedMessages) == 1 && len(lateCitationsMap[0]) > 0 {
				lateCites = lateCitationsMap[0]
			}
			annotations := MergeCitationAnnotations(completedMessage.Annotations, lateCites)
			emitNewCitationAnnotations(idx, completedMessage.ID, annotations)
			if len(annotations) > 0 {
				completedMessage.Annotations = annotations
				st.CompletedMessages[idx] = completedMessage
			}
		}
	}

	emitDetachedReasoning := func(signature, direction, targetKind string) {
		signature = strings.TrimSpace(signature)
		if signature == "" || st.SeenReasoningSignatures[signature] {
			return
		}
		finalizeReasoning()
		finalizeMessage()
		idx := st.NextIndex
		st.NextIndex++
		placement := "before"
		if direction == geminiResponsesCarrierPrevious {
			placement = "after"
		}
		itemID := fmt.Sprintf("rs_%s_detached_%s_%d", st.ResponseID, placement, idx)
		carrierSignature := encodeGeminiResponsesCarrier(signature, direction, targetKind)

		added := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","encrypted_content":"","summary":[]}}`)
		added, _ = sjson.SetBytes(added, "sequence_number", nextSeq())
		added, _ = sjson.SetBytes(added, "output_index", idx)
		added, _ = sjson.SetBytes(added, "item.id", itemID)
		added, _ = sjson.SetBytes(added, "item.encrypted_content", carrierSignature)
		out = append(out, emitEvent("response.output_item.added", added))

		done := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","encrypted_content":"","summary":[]}}`)
		done, _ = sjson.SetBytes(done, "sequence_number", nextSeq())
		done, _ = sjson.SetBytes(done, "output_index", idx)
		done, _ = sjson.SetBytes(done, "item.id", itemID)
		done, _ = sjson.SetBytes(done, "item.encrypted_content", carrierSignature)
		out = append(out, emitEvent("response.output_item.done", done))

		st.DetachedReasoning[idx] = geminiDetachedReasoningItem{Index: idx, ID: itemID, Signature: carrierSignature}
		st.SeenReasoningSignatures[signature] = true
	}
	emitTrailingDetachedReasoning := func(signature string) {
		switch st.LastSemanticKind {
		case geminiResponsesCarrierText:
			signature = strings.TrimSpace(signature)
			if signature == "" || st.SeenReasoningSignatures[signature] {
				return
			}
			finalizeReasoning()
			finalizeMessage()
			// LastSemanticKind also includes thought text. Never bind a later
			// thought signature to a visible message from before that thought.
			if !st.MsgOpened || (st.ReasoningOpened && st.ReasoningIndex > st.MsgIndex) {
				emitDetachedReasoning(signature, geminiResponsesCarrierPrevious, geminiResponsesCarrierText)
				return
			}
			if st.HiddenTextSignatures == nil {
				st.HiddenTextSignatures = make(map[string][]string)
			}
			signatures := append(st.HiddenTextSignatures[st.CurrentMsgID], signature)
			// Keep failed writes in the prefix so a later successful write cannot
			// move a newer signature ahead of an earlier fallback carrier.
			st.HiddenTextSignatures[st.CurrentMsgID] = signatures
			if cacheGeminiResponsesTextSignatures(modelName, st.CurrentMsgID, st.ItemTextBuf.String(), signatures) {
				st.SeenReasoningSignatures[signature] = true
				return
			}
			// Preserve replay continuity if the cache cannot accept the signature.
			emitDetachedReasoning(signature, geminiResponsesCarrierPrevious, geminiResponsesCarrierText)
		case geminiResponsesCarrierFunction:
			emitDetachedReasoning(signature, geminiResponsesCarrierPrevious, geminiResponsesCarrierFunction)
		default:
			emitDetachedReasoning(signature, geminiResponsesCarrierStandalone, geminiResponsesCarrierAny)
		}
	}

	// Initialize per-response fields and emit created/in_progress once
	if !st.Started {
		st.ResponseID = root.Get("responseId").String()
		if st.ResponseID == "" {
			st.ResponseID = fmt.Sprintf("resp_%x_%d", time.Now().UnixNano(), atomic.AddUint64(&responseIDCounter, 1))
		}
		if !strings.HasPrefix(st.ResponseID, "resp_") {
			st.ResponseID = fmt.Sprintf("resp_%s", st.ResponseID)
		}
		if v := root.Get("createTime"); v.Exists() {
			if t, errParseCreateTime := time.Parse(time.RFC3339Nano, v.String()); errParseCreateTime == nil {
				st.CreatedAt = t.Unix()
			}
		}
		if st.CreatedAt == 0 {
			st.CreatedAt = time.Now().Unix()
		}

		created := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
		created, _ = sjson.SetBytes(created, "sequence_number", nextSeq())
		created, _ = sjson.SetBytes(created, "response.id", st.ResponseID)
		created, _ = sjson.SetBytes(created, "response.created_at", st.CreatedAt)
		requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
		if requestModelName == "" {
			requestModelName = modelName
		}
		if requestModelName != "" {
			created, _ = sjson.SetBytes(created, "response.model", requestModelName)
		}
		out = append(out, emitEvent("response.created", created))

		inprog := []byte(`{"type":"response.in_progress","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","output":[]}}`)
		inprog, _ = sjson.SetBytes(inprog, "sequence_number", nextSeq())
		inprog, _ = sjson.SetBytes(inprog, "response.id", st.ResponseID)
		inprog, _ = sjson.SetBytes(inprog, "response.created_at", st.CreatedAt)
		if requestModelName != "" {
			inprog, _ = sjson.SetBytes(inprog, "response.model", requestModelName)
		}
		out = append(out, emitEvent("response.in_progress", inprog))

		st.Started = true
		st.NextIndex = 0
		st.WebSearchStreamMode = determineWebSearchStreamMode(modelName, requestModelName, originalRequestRawJSON, requestRawJSON)
	}

	// Handle groundingMetadata for web search
	if gm := ExtractGroundingMetadata(root); gm.Exists() {
		if st.RawGroundingMetadata.Exists() {
			st.RawGroundingMetadata = MergeGroundingMetadata(st.RawGroundingMetadata, gm)
		} else {
			st.RawGroundingMetadata = MergeGroundingMetadata(gjson.Result{}, gm)
		}
		mergedGM := st.RawGroundingMetadata
		queries := ExtractGroundingQueries(mergedGM)
		if len(queries) > 0 {
			st.WebSearchQueries = queries
			if st.WebSearchQuery == "" {
				st.WebSearchQuery = queries[0]
			}
		}
		if sources := ExtractGroundingSources(mergedGM); len(sources) > 0 {
			st.WebSearchSources = sources
		}
		// Function calls, thoughts, or signature boundaries may finalize the search
		// item before later grounding frames arrive. Keep the cached completed item
		// aligned with the latest queries and sources so response.completed is complete.
		if st.WebSearchDone {
			st.WebSearchDoneItem = BuildResponsesWebSearchCallItem(st.WebSearchItemID, st.WebSearchQuery, st.WebSearchQueries, st.WebSearchSources)
		}

		if !st.WebSearchOpened && HasValidWebGrounding(mergedGM) {
			openWebSearch()
		}

		emitLateCitations()
	}

	// Handle parts (text/thought/functionCall)
	if parts := root.Get("candidates.0.content.parts"); parts.Exists() && parts.IsArray() {
		parts.ForEach(func(partIdxInChunk, part gjson.Result) bool {
			explicitPartIndex := -1
			if p := part.Get("partIndex"); p.Exists() {
				explicitPartIndex = int(p.Int())
			} else if p := part.Get("index"); p.Exists() {
				explicitPartIndex = int(p.Int())
			}

			signature := strings.TrimSpace(part.Get("thoughtSignature").String())
			if signature == "" {
				signature = strings.TrimSpace(part.Get("thought_signature").String())
			}
			functionCall := part.Get("functionCall")
			text := part.Get("text")
			isThought := part.Get("thought").Bool()

			var partKind string
			switch {
			case isThought:
				partKind = "thought"
			case functionCall.Exists():
				partKind = "function"
			case text.Exists():
				partKind = "text"
			default:
				partKind = "unknown"
			}

			var currentPartIndex int
			if explicitPartIndex >= 0 {
				currentPartIndex = explicitPartIndex
				st.CurrentLogicalPartIndex = explicitPartIndex
				st.CurrentPartKind = partKind
				st.HasSeenFirstPart = true
				st.TextPartRunActive = (partKind == "text")
			} else {
				if !st.HasSeenFirstPart {
					st.HasSeenFirstPart = true
					st.CurrentLogicalPartIndex = 0
					st.CurrentPartKind = partKind
					currentPartIndex = 0
					if partKind == "text" {
						st.TextPartRunActive = true
					}
				} else {
					if partIdxInChunk.Int() > 0 {
						st.CurrentLogicalPartIndex++
						st.CurrentPartKind = partKind
						st.TextPartRunActive = (partKind == "text")
					} else if partKind != st.CurrentPartKind {
						st.CurrentLogicalPartIndex++
						st.CurrentPartKind = partKind
						st.TextPartRunActive = (partKind == "text")
					} else if partKind == "function" {
						st.CurrentLogicalPartIndex++
						st.CurrentPartKind = partKind
						st.TextPartRunActive = false
					} else if partKind == "text" && !st.TextPartRunActive {
						st.CurrentLogicalPartIndex++
						st.CurrentPartKind = partKind
						st.TextPartRunActive = true
					}
					currentPartIndex = st.CurrentLogicalPartIndex
				}
			}
			st.StreamPartIndex = currentPartIndex
			if functionCall.Exists() && st.PendingReasoningSignature != "" {
				if signature == "" {
					emitDetachedReasoning(st.PendingReasoningSignature, geminiResponsesCarrierNext, geminiResponsesCarrierFunction)
				} else {
					emitTrailingDetachedReasoning(st.PendingReasoningSignature)
				}
				st.PendingReasoningSignature = ""
			}
			reasoningActive := (st.ReasoningOpened && !st.ReasoningClosed) || (!st.ReasoningOpened && (st.ReasoningBuf.Len() > 0 || st.ReasoningEnc != ""))
			if signature != "" && !isThought {
				if reasoningActive {
					switch {
					case st.ReasoningEnc == "" || st.ReasoningEnc == signature:
						st.ReasoningEnc = signature
						switch {
						case functionCall.Exists():
							st.ReasoningDirection = geminiResponsesCarrierNext
							st.ReasoningTargetKind = geminiResponsesCarrierFunction
						case text.Exists() && text.String() != "":
							st.ReasoningDirection = geminiResponsesCarrierNext
							st.ReasoningTargetKind = geminiResponsesCarrierText
						default:
							st.ReasoningDirection = geminiResponsesCarrierStandalone
							st.ReasoningTargetKind = geminiResponsesCarrierText
						}
						st.SeenReasoningSignatures[signature] = true
					default:
						finalizeReasoning()
						if functionCall.Exists() {
							emitDetachedReasoning(signature, geminiResponsesCarrierNext, geminiResponsesCarrierFunction)
						} else if !st.SeenReasoningSignatures[signature] {
							st.PendingReasoningSignature = signature
						}
					}
					if text.Exists() && text.String() == "" && !functionCall.Exists() {
						finalizeReasoning()
						return true
					}
				} else {
					switch {
					case functionCall.Exists():
						emitDetachedReasoning(signature, geminiResponsesCarrierNext, geminiResponsesCarrierFunction)
					case text.Exists() && text.String() != "":
						if st.PendingReasoningSignature != "" && st.PendingReasoningSignature != signature {
							emitTrailingDetachedReasoning(st.PendingReasoningSignature)
							st.PendingReasoningSignature = ""
						}
						if !st.SeenReasoningSignatures[signature] {
							st.PendingReasoningSignature = signature
						}
					case text.Exists() && text.String() == "":
						if st.PendingReasoningSignature != "" {
							pendingSignature := st.PendingReasoningSignature
							st.PendingReasoningSignature = ""
							if pendingSignature != signature {
								emitTrailingDetachedReasoning(pendingSignature)
							}
						}
						if st.MsgOpened || len(st.FuncDone) > 0 || len(st.WebSearchBufferedDeltas) > 0 {
							emitTrailingDetachedReasoning(signature)
						} else if !st.SeenReasoningSignatures[signature] {
							st.PendingReasoningSignature = signature
						}
						return true
					}
				}
			}

			// Reasoning text
			if isThought {
				if len(st.WebSearchBufferedDeltas) > 0 {
					finalizeMessage()
				}
				if st.PendingReasoningSignature != "" && st.MsgOpened && !st.MsgClosed {
					emitTrailingDetachedReasoning(st.PendingReasoningSignature)
					st.PendingReasoningSignature = ""
				}
				incomingSignature := ""
				if signature != "" && signature != geminiResponsesThoughtSignature {
					if st.PendingReasoningSignature != "" {
						if st.PendingReasoningSignature != signature {
							emitDetachedReasoning(st.PendingReasoningSignature, geminiResponsesCarrierStandalone, geminiResponsesCarrierAny)
						}
						st.PendingReasoningSignature = ""
					}
					incomingSignature = signature
				} else if st.PendingReasoningSignature != "" {
					incomingSignature = st.PendingReasoningSignature
					st.PendingReasoningSignature = ""
				}
				if st.ReasoningOpened && !st.ReasoningClosed && incomingSignature != "" && st.ReasoningEnc != "" && incomingSignature != st.ReasoningEnc {
					finalizeReasoning()
					resetReasoning()
				}
				if st.ReasoningClosed {
					finalizeMessage()
					resetReasoning()
				} else if !st.ReasoningOpened && st.ReasoningBuf.Len() == 0 && st.MsgOpened && !st.MsgClosed {
					finalizeMessage()
				}
				if incomingSignature != "" {
					st.ReasoningEnc = incomingSignature
					st.ReasoningDirection = geminiResponsesCarrierStandalone
					st.ReasoningTargetKind = geminiResponsesCarrierText
					st.SeenReasoningSignatures[incomingSignature] = true
				}
				if t := part.Get("text"); t.Exists() && t.String() != "" {
					st.LastSemanticKind = geminiResponsesCarrierText
					st.ReasoningBuf.WriteString(t.String())
					if st.ReasoningOpened {
						msg := []byte(`{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"delta":""}`)
						msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
						msg, _ = sjson.SetBytes(msg, "item_id", st.ReasoningItemID)
						msg, _ = sjson.SetBytes(msg, "output_index", st.ReasoningIndex)
						msg, _ = sjson.SetBytes(msg, "delta", t.String())
						out = append(out, emitEvent("response.reasoning_summary_text.delta", msg))
					} else {
						st.ReasoningPendingDeltas = append(st.ReasoningPendingDeltas, t.String())
					}
				}
				if !st.ReasoningOpened && st.ReasoningEnc != "" {
					openReasoning()
				}
				return true
			}

			// Assistant visible text
			if t := part.Get("text"); t.Exists() && t.String() != "" {
				if signature == "" && st.PendingReasoningSignature != "" && ((st.MsgOpened && !st.MsgClosed) || len(st.WebSearchBufferedDeltas) > 0) {
					emitTrailingDetachedReasoning(st.PendingReasoningSignature)
					st.PendingReasoningSignature = ""
				}
				// Responses output items are sequential: finish reasoning before
				// opening the visible message. A signature that arrives later is
				// cached with the message and recombined on replay.
				finalizeReasoning()

				if st.MsgClosed {
					st.MsgOpened = false
					st.MsgClosed = false
					st.ItemTextBuf.Reset()
					st.CurrentMsgRuneOffset = 0
				}

				// In web search stream mode, buffer deltas until web_search_call is finalized
				// (stream end / a later output item) so the completed search item includes
				// incremental sources and strictly precedes the message.
				if st.WebSearchStreamMode && !st.WebSearchDone {
					st.LastSemanticKind = geminiResponsesCarrierText
					st.WebSearchBufferedDeltas = append(st.WebSearchBufferedDeltas, t.String())
					n := len(st.WebSearchBufferedParts)
					if n > 0 && st.WebSearchBufferedParts[n-1].PartIndex == currentPartIndex {
						st.WebSearchBufferedParts[n-1].Text += t.String()
					} else {
						st.WebSearchBufferedParts = append(st.WebSearchBufferedParts, geminiStreamBufferedPart{
							PartIndex: currentPartIndex,
							Text:      t.String(),
						})
					}
					st.TextPartRunActive = true
					return true
				}

				if !st.MsgOpened {
					st.MsgOpened = true
					st.MsgIndex = st.NextIndex
					st.NextIndex++
					st.CurrentMsgID = fmt.Sprintf("msg_%s_%d", st.ResponseID, st.MsgIndex)
					item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`)
					item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
					item, _ = sjson.SetBytes(item, "output_index", st.MsgIndex)
					item, _ = sjson.SetBytes(item, "item.id", st.CurrentMsgID)
					out = append(out, emitEvent("response.output_item.added", item))
					partAdded := []byte(`{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
					partAdded, _ = sjson.SetBytes(partAdded, "sequence_number", nextSeq())
					partAdded, _ = sjson.SetBytes(partAdded, "item_id", st.CurrentMsgID)
					partAdded, _ = sjson.SetBytes(partAdded, "output_index", st.MsgIndex)
					out = append(out, emitEvent("response.content_part.added", partAdded))
					st.ItemTextBuf.Reset()
					st.CurrentMsgRuneOffset = 0
				}
				st.LastSemanticKind = geminiResponsesCarrierText
				st.ItemTextBuf.WriteString(t.String())
				n := len(st.PartMappings)
				if n > 0 && st.PartMappings[n-1].PartIndex == currentPartIndex && st.PartMappings[n-1].MessageIndex == st.MsgIndex {
					st.PartMappings[n-1].PartText += t.String()
				} else {
					st.PartMappings = append(st.PartMappings, GeminiPartMapping{
						PartIndex:      currentPartIndex,
						MessageIndex:   st.MsgIndex,
						StartRuneInMsg: st.CurrentMsgRuneOffset,
						PartText:       t.String(),
					})
				}
				st.CurrentMsgRuneOffset += int64(utf8.RuneCountInString(t.String()))
				msg := []byte(`{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`)
				msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
				msg, _ = sjson.SetBytes(msg, "item_id", st.CurrentMsgID)
				msg, _ = sjson.SetBytes(msg, "output_index", st.MsgIndex)
				msg, _ = sjson.SetBytes(msg, "delta", t.String())
				out = append(out, emitEvent("response.output_text.delta", msg))
				st.TextPartRunActive = true
				return true
			}

			// Function call
			if fc := part.Get("functionCall"); fc.Exists() {
				// Before emitting function-call outputs, finalize reasoning, web search, and the message (if open).
				// Responses streaming requires message done events before the next output_item.added.
				finalizeReasoning()
				finalizeWebSearch()
				if len(st.WebSearchBufferedDeltas) > 0 {
					flushWebSearchBufferedText()
				}
				finalizeMessage()
				st.LastSemanticKind = geminiResponsesCarrierFunction

				rawName := fc.Get("name").String()
				identity, hasIdentity := st.ToolIdentityMap[rawName]
				if !hasIdentity {
					restored := util.RestoreSanitizedToolName(st.SanitizedNameMap, rawName)
					identity = util.ResponsesToolIdentity{Name: restored}
				}
				name := identity.Name
				namespace := identity.Namespace
				isCustom := identity.Custom

				idx := st.NextIndex
				st.NextIndex++
				// Ensure buffers
				if st.FuncArgsBuf[idx] == nil {
					st.FuncArgsBuf[idx] = &strings.Builder{}
				}
				if st.FuncCallIDs[idx] == "" {
					st.FuncCallIDs[idx] = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&funcCallIDCounter, 1))
				}
				st.FuncNames[idx] = name
				st.FuncNamespaces[idx] = namespace
				st.FuncCustom[idx] = isCustom

				argsJSON := "{}"
				if args := fc.Get("args"); args.Exists() {
					argsJSON = args.Raw
				}
				if st.FuncArgsBuf[idx].Len() == 0 && argsJSON != "" {
					st.FuncArgsBuf[idx].WriteString(argsJSON)
				}

				if isCustom {
					inputStr := util.UnwrapResponsesCustomToolInput(argsJSON)
					st.FuncInputBuf[idx] = inputStr

					// Emit item.added for custom tool call
					item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"in_progress","input":"","call_id":"","name":""}}`)
					item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
					item, _ = sjson.SetBytes(item, "output_index", idx)
					item, _ = sjson.SetBytes(item, "item.id", fmt.Sprintf("ctc_%s", st.FuncCallIDs[idx]))
					item, _ = sjson.SetBytes(item, "item.call_id", st.FuncCallIDs[idx])
					item = translatorcommon.SetResponsesToolCallIdentity(item, name, namespace, "item")
					out = append(out, emitEvent("response.output_item.added", item))

					// Emit custom tool call input.done
					if !st.FuncDone[idx] {
						inputDone := []byte(`{"type":"response.custom_tool_call_input.done","sequence_number":0,"item_id":"","output_index":0,"input":""}`)
						inputDone, _ = sjson.SetBytes(inputDone, "sequence_number", nextSeq())
						inputDone, _ = sjson.SetBytes(inputDone, "item_id", fmt.Sprintf("ctc_%s", st.FuncCallIDs[idx]))
						inputDone, _ = sjson.SetBytes(inputDone, "output_index", idx)
						inputDone, _ = sjson.SetBytes(inputDone, "input", inputStr)
						out = append(out, emitEvent("response.custom_tool_call_input.done", inputDone))

						itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}}`)
						itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
						itemDone, _ = sjson.SetBytes(itemDone, "output_index", idx)
						itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("ctc_%s", st.FuncCallIDs[idx]))
						itemDone, _ = sjson.SetBytes(itemDone, "item.input", inputStr)
						itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", st.FuncCallIDs[idx])
						itemDone = translatorcommon.SetResponsesToolCallIdentity(itemDone, name, namespace, "item")
						out = append(out, emitEvent("response.output_item.done", itemDone))

						st.FuncDone[idx] = true
					}
				} else {
					// Emit item.added for function call
					item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"in_progress","arguments":"","call_id":"","name":""}}`)
					item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
					item, _ = sjson.SetBytes(item, "output_index", idx)
					item, _ = sjson.SetBytes(item, "item.id", fmt.Sprintf("fc_%s", st.FuncCallIDs[idx]))
					item, _ = sjson.SetBytes(item, "item.call_id", st.FuncCallIDs[idx])
					item = translatorcommon.SetResponsesToolCallIdentity(item, name, namespace, "item")
					out = append(out, emitEvent("response.output_item.added", item))

					// Emit arguments delta (full args in one chunk).
					// When Gemini omits args, emit "{}" to keep Responses streaming event order consistent.
					if argsJSON != "" {
						ad := []byte(`{"type":"response.function_call_arguments.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`)
						ad, _ = sjson.SetBytes(ad, "sequence_number", nextSeq())
						ad, _ = sjson.SetBytes(ad, "item_id", fmt.Sprintf("fc_%s", st.FuncCallIDs[idx]))
						ad, _ = sjson.SetBytes(ad, "output_index", idx)
						ad, _ = translatorcommon.SetStringWithoutHTMLEscape(ad, "delta", argsJSON)
						out = append(out, emitEvent("response.function_call_arguments.delta", ad))
					}

					// Gemini emits the full function call payload at once, so we can finalize it immediately.
					if !st.FuncDone[idx] {
						fcDone := []byte(`{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`)
						fcDone, _ = sjson.SetBytes(fcDone, "sequence_number", nextSeq())
						fcDone, _ = sjson.SetBytes(fcDone, "item_id", fmt.Sprintf("fc_%s", st.FuncCallIDs[idx]))
						fcDone, _ = sjson.SetBytes(fcDone, "output_index", idx)
						fcDone, _ = translatorcommon.SetStringWithoutHTMLEscape(fcDone, "arguments", argsJSON)
						out = append(out, emitEvent("response.function_call_arguments.done", fcDone))

						itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`)
						itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
						itemDone, _ = sjson.SetBytes(itemDone, "output_index", idx)
						itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("fc_%s", st.FuncCallIDs[idx]))
						itemDone, _ = translatorcommon.SetStringWithoutHTMLEscape(itemDone, "item.arguments", argsJSON)
						itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", st.FuncCallIDs[idx])
						itemDone = translatorcommon.SetResponsesToolCallIdentity(itemDone, name, namespace, "item")
						out = append(out, emitEvent("response.output_item.done", itemDone))

						st.FuncDone[idx] = true
					}
				}

				return true
			}

			return true
		})
	}

	// Finalization on finishReason
	if fr := root.Get("candidates.0.finishReason"); fr.Exists() && fr.String() != "" {
		if st.PendingReasoningSignature != "" {
			emitTrailingDetachedReasoning(st.PendingReasoningSignature)
			st.PendingReasoningSignature = ""
		}
		// Finalize web search with the complete incremental sources, then reasoning,
		// then the message so web_search_call precedes later output items.
		finalizeWebSearch()
		finalizeReasoning()
		finalizeMessage()

		// Close function calls
		if len(st.FuncArgsBuf) > 0 {
			// sort indices (small N); avoid extra imports
			idxs := make([]int, 0, len(st.FuncArgsBuf))
			for idx := range st.FuncArgsBuf {
				idxs = append(idxs, idx)
			}
			for i := 0; i < len(idxs); i++ {
				for j := i + 1; j < len(idxs); j++ {
					if idxs[j] < idxs[i] {
						idxs[i], idxs[j] = idxs[j], idxs[i]
					}
				}
			}
			for _, idx := range idxs {
				if st.FuncDone[idx] {
					continue
				}
				if st.FuncCustom[idx] {
					inputStr := st.FuncInputBuf[idx]
					inputDone := []byte(`{"type":"response.custom_tool_call_input.done","sequence_number":0,"item_id":"","output_index":0,"input":""}`)
					inputDone, _ = sjson.SetBytes(inputDone, "sequence_number", nextSeq())
					inputDone, _ = sjson.SetBytes(inputDone, "item_id", fmt.Sprintf("ctc_%s", st.FuncCallIDs[idx]))
					inputDone, _ = sjson.SetBytes(inputDone, "output_index", idx)
					inputDone, _ = sjson.SetBytes(inputDone, "input", inputStr)
					out = append(out, emitEvent("response.custom_tool_call_input.done", inputDone))

					itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}}`)
					itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
					itemDone, _ = sjson.SetBytes(itemDone, "output_index", idx)
					itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("ctc_%s", st.FuncCallIDs[idx]))
					itemDone, _ = sjson.SetBytes(itemDone, "item.input", inputStr)
					itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", st.FuncCallIDs[idx])
					itemDone = translatorcommon.SetResponsesToolCallIdentity(itemDone, st.FuncNames[idx], st.FuncNamespaces[idx], "item")
					out = append(out, emitEvent("response.output_item.done", itemDone))
				} else {
					args := "{}"
					if b := st.FuncArgsBuf[idx]; b != nil && b.Len() > 0 {
						args = b.String()
					}
					fcDone := []byte(`{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`)
					fcDone, _ = sjson.SetBytes(fcDone, "sequence_number", nextSeq())
					fcDone, _ = sjson.SetBytes(fcDone, "item_id", fmt.Sprintf("fc_%s", st.FuncCallIDs[idx]))
					fcDone, _ = sjson.SetBytes(fcDone, "output_index", idx)
					fcDone, _ = translatorcommon.SetStringWithoutHTMLEscape(fcDone, "arguments", args)
					out = append(out, emitEvent("response.function_call_arguments.done", fcDone))

					itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`)
					itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
					itemDone, _ = sjson.SetBytes(itemDone, "output_index", idx)
					itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("fc_%s", st.FuncCallIDs[idx]))
					itemDone, _ = translatorcommon.SetStringWithoutHTMLEscape(itemDone, "item.arguments", args)
					itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", st.FuncCallIDs[idx])
					itemDone = translatorcommon.SetResponsesToolCallIdentity(itemDone, st.FuncNames[idx], st.FuncNamespaces[idx], "item")
					out = append(out, emitEvent("response.output_item.done", itemDone))
				}
				st.FuncDone[idx] = true
			}
		}

		// Reasoning already finalized above if present

		// Build response.completed with aggregated outputs and request echo fields
		completed := []byte(`{"type":"response.completed","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null}}`)
		completed, _ = sjson.SetBytes(completed, "sequence_number", nextSeq())
		completed, _ = sjson.SetBytes(completed, "response.id", st.ResponseID)
		completed, _ = sjson.SetBytes(completed, "response.created_at", st.CreatedAt)

		if reqJSON := pickRequestJSON(originalRequestRawJSON, requestRawJSON); len(reqJSON) > 0 {
			req := unwrapRequestRoot(gjson.ParseBytes(reqJSON))
			if v := req.Get("instructions"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.instructions", v.String())
			}
			if v := req.Get("max_output_tokens"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.max_output_tokens", v.Int())
			}
			if v := req.Get("max_tool_calls"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.max_tool_calls", v.Int())
			}
			if v := req.Get("model"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.model", v.String())
			}
			if v := req.Get("parallel_tool_calls"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.parallel_tool_calls", v.Bool())
			}
			if v := req.Get("previous_response_id"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.previous_response_id", v.String())
			}
			if v := req.Get("prompt_cache_key"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.prompt_cache_key", v.String())
			}
			if v := req.Get("reasoning"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.reasoning", v.Value())
			}
			if v := req.Get("safety_identifier"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.safety_identifier", v.String())
			}
			if v := req.Get("service_tier"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.service_tier", v.String())
			}
			if v := req.Get("store"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.store", v.Bool())
			}
			if v := req.Get("temperature"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.temperature", v.Float())
			}
			if v := req.Get("text"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.text", v.Value())
			}
			if v := req.Get("tool_choice"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.tool_choice", v.Value())
			}
			if v := req.Get("tools"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.tools", v.Value())
			}
			if v := req.Get("top_logprobs"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.top_logprobs", v.Int())
			}
			if v := req.Get("top_p"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.top_p", v.Float())
			}
			if v := req.Get("truncation"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.truncation", v.String())
			}
			if v := req.Get("user"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.user", v.Value())
			}
			if v := req.Get("metadata"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.metadata", v.Value())
			}
		}

		emitLateCitations()

		// Compose outputs in output_index order.
		outputs := make([][]byte, 0, st.NextIndex)
		for idx := 0; idx < st.NextIndex; idx++ {
			if st.WebSearchDone && idx == st.WebSearchIndex {
				outputs = append(outputs, BuildResponsesWebSearchCallItem(st.WebSearchItemID, st.WebSearchQuery, st.WebSearchQueries, st.WebSearchSources))
				continue
			}
			if completedReasoning, ok := st.CompletedReasoning[idx]; ok {
				item := []byte(`{"id":"","type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":""}]}`)
				item, _ = sjson.SetBytes(item, "id", completedReasoning.ID)
				item, _ = sjson.SetBytes(item, "encrypted_content", completedReasoning.Signature)
				item, _ = sjson.SetBytes(item, "summary.0.text", completedReasoning.Text)
				outputs = append(outputs, item)
				continue
			}
			if completedMessage, ok := st.CompletedMessages[idx]; ok {
				item := []byte(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
				item, _ = sjson.SetBytes(item, "id", completedMessage.ID)
				item, _ = sjson.SetBytes(item, "content.0.text", completedMessage.Text)
				if len(completedMessage.Annotations) > 0 {
					item, _ = sjson.SetRawBytes(item, "content.0.annotations", translatorcommon.JoinRawArray(completedMessage.Annotations))
				}
				outputs = append(outputs, item)
				continue
			}
			if detached, ok := st.DetachedReasoning[idx]; ok {
				item := []byte(`{"id":"","type":"reasoning","encrypted_content":"","summary":[]}`)
				item, _ = sjson.SetBytes(item, "id", detached.ID)
				item, _ = sjson.SetBytes(item, "encrypted_content", detached.Signature)
				outputs = append(outputs, item)
				continue
			}

			if callID, ok := st.FuncCallIDs[idx]; ok && callID != "" {
				if st.FuncCustom[idx] {
					inputStr := st.FuncInputBuf[idx]
					item := []byte(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
					item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("ctc_%s", callID))
					item, _ = sjson.SetBytes(item, "input", inputStr)
					item, _ = sjson.SetBytes(item, "call_id", callID)
					item = translatorcommon.SetResponsesToolCallIdentity(item, st.FuncNames[idx], st.FuncNamespaces[idx], "")
					outputs = append(outputs, item)
				} else {
					args := "{}"
					if b := st.FuncArgsBuf[idx]; b != nil && b.Len() > 0 {
						args = b.String()
					}
					item := []byte(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
					item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("fc_%s", callID))
					item, _ = translatorcommon.SetStringWithoutHTMLEscape(item, "arguments", args)
					item, _ = sjson.SetBytes(item, "call_id", callID)
					item = translatorcommon.SetResponsesToolCallIdentity(item, st.FuncNames[idx], st.FuncNamespaces[idx], "")
					outputs = append(outputs, item)
				}
			}
		}
		if len(outputs) > 0 {
			completed, _ = sjson.SetRawBytes(completed, "response.output", translatorcommon.JoinRawArray(outputs))
		}
		if st.WebSearchDone {
			completed, _ = sjson.SetBytes(completed, "response.tool_usage.web_search.num_requests", 1)
		}

		// usage mapping
		if um := root.Get("usageMetadata"); um.Exists() {
			// input tokens = prompt only (thoughts go to output)
			input := um.Get("promptTokenCount").Int()
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens", input)
			// cached token details: align with OpenAI "cached_tokens" semantics.
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens_details.cached_tokens", um.Get("cachedContentTokenCount").Int())
			// output tokens
			completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens", um.Get("candidatesTokenCount").Int()+um.Get("thoughtsTokenCount").Int())
			if v := um.Get("thoughtsTokenCount"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens_details.reasoning_tokens", v.Int())
			} else {
				completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens_details.reasoning_tokens", 0)
			}
			if v := um.Get("totalTokenCount"); v.Exists() {
				completed, _ = sjson.SetBytes(completed, "response.usage.total_tokens", v.Int())
			} else {
				completed, _ = sjson.SetBytes(completed, "response.usage.total_tokens", 0)
			}
		}

		out = append(out, emitEvent("response.completed", completed))
		st.Completed = true
	}

	return out
}

// ConvertGeminiResponseToOpenAIResponsesNonStream aggregates Gemini response JSON into a single OpenAI Responses JSON object.
func ConvertGeminiResponseToOpenAIResponsesNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	root := gjson.ParseBytes(rawJSON)
	root = unwrapGeminiResponseRoot(root)
	reqJSON := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
	sanitizedNameMap := util.SanitizedToolNameMap(originalRequestRawJSON)
	toolIdentityMap := util.ResponsesToolReverseIdentityMap(reqJSON)

	// Base response scaffold
	resp := []byte(`{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"incomplete_details":null}`)

	// id: prefer provider responseId, otherwise synthesize
	id := root.Get("responseId").String()
	if id == "" {
		id = fmt.Sprintf("resp_%x_%d", time.Now().UnixNano(), atomic.AddUint64(&responseIDCounter, 1))
	}
	// Normalize to response-style id (prefix resp_ if missing)
	if !strings.HasPrefix(id, "resp_") {
		id = fmt.Sprintf("resp_%s", id)
	}
	resp, _ = sjson.SetBytes(resp, "id", id)

	// created_at: map from createTime if available
	createdAt := time.Now().Unix()
	if v := root.Get("createTime"); v.Exists() {
		if t, errParseCreateTime := time.Parse(time.RFC3339Nano, v.String()); errParseCreateTime == nil {
			createdAt = t.Unix()
		}
	}
	resp, _ = sjson.SetBytes(resp, "created_at", createdAt)

	// Echo request fields when present; fallback model from response modelVersion
	if reqJSON := pickRequestJSON(originalRequestRawJSON, requestRawJSON); len(reqJSON) > 0 {
		req := unwrapRequestRoot(gjson.ParseBytes(reqJSON))
		if v := req.Get("instructions"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "instructions", v.String())
		}
		if v := req.Get("max_output_tokens"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "max_output_tokens", v.Int())
		}
		if v := req.Get("max_tool_calls"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "max_tool_calls", v.Int())
		}
		if v := req.Get("model"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "model", v.String())
		} else if v = root.Get("modelVersion"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "model", v.String())
		}
		if v := req.Get("parallel_tool_calls"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "parallel_tool_calls", v.Bool())
		}
		if v := req.Get("previous_response_id"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "previous_response_id", v.String())
		}
		if v := req.Get("prompt_cache_key"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "prompt_cache_key", v.String())
		}
		if v := req.Get("reasoning"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "reasoning", v.Value())
		}
		if v := req.Get("safety_identifier"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "safety_identifier", v.String())
		}
		if v := req.Get("service_tier"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "service_tier", v.String())
		}
		if v := req.Get("store"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "store", v.Bool())
		}
		if v := req.Get("temperature"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "temperature", v.Float())
		}
		if v := req.Get("text"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "text", v.Value())
		}
		if v := req.Get("tool_choice"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "tool_choice", v.Value())
		}
		if v := req.Get("tools"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "tools", v.Value())
		}
		if v := req.Get("top_logprobs"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "top_logprobs", v.Int())
		}
		if v := req.Get("top_p"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "top_p", v.Float())
		}
		if v := req.Get("truncation"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "truncation", v.String())
		}
		if v := req.Get("user"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "user", v.Value())
		}
		if v := req.Get("metadata"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "metadata", v.Value())
		}
	} else if v := root.Get("modelVersion"); v.Exists() {
		resp, _ = sjson.SetBytes(resp, "model", v.String())
	}

	// Build outputs from candidates[0].content.parts
	var reasoningText strings.Builder
	var reasoningEncrypted string
	var reasoningDirection string
	var reasoningTargetKind string
	type nonStreamReasoningOutput struct {
		text       string
		signature  string
		direction  string
		targetKind string
	}
	type nonStreamFunctionOutput struct {
		item      []byte
		signature string
	}
	type nonStreamOutputOrder struct {
		kind  string
		index int
	}
	type nonStreamDetachedOutput struct {
		signature  string
		direction  string
		targetKind string
	}
	type nonStreamMessageOutput struct {
		text       string
		signatures []string
	}
	var reasoningOutputs []nonStreamReasoningOutput
	var functionOutputs []nonStreamFunctionOutput
	var messageOutputs []nonStreamMessageOutput
	var outputOrder []nonStreamOutputOrder
	reasoningOutputSignatures := make(map[string]bool)
	flushReasoningOutput := func() {
		if reasoningText.Len() == 0 && reasoningEncrypted == "" {
			return
		}
		reasoningIndex := len(reasoningOutputs)
		reasoningOutputs = append(reasoningOutputs, nonStreamReasoningOutput{text: reasoningText.String(), signature: reasoningEncrypted, direction: reasoningDirection, targetKind: reasoningTargetKind})
		outputOrder = append(outputOrder, nonStreamOutputOrder{kind: "reasoning", index: reasoningIndex})
		if reasoningEncrypted != "" {
			reasoningOutputSignatures[reasoningEncrypted] = true
		}
		reasoningText.Reset()
		reasoningEncrypted = ""
		reasoningDirection = ""
		reasoningTargetKind = ""
	}
	var detachedReasoningOutputs []nonStreamDetachedOutput
	var currentMessageText strings.Builder
	var currentMessageSignatures []string
	var partMappings []GeminiPartMapping
	var currentMsgRuneOffset int64
	flushMessageOutput := func() {
		if currentMessageText.Len() == 0 {
			return
		}
		messageIndex := len(messageOutputs)
		messageOutputs = append(messageOutputs, nonStreamMessageOutput{text: currentMessageText.String(), signatures: append([]string(nil), currentMessageSignatures...)})
		outputOrder = append(outputOrder, nonStreamOutputOrder{kind: "message", index: messageIndex})
		currentMessageText.Reset()
		currentMessageSignatures = nil
		currentMsgRuneOffset = 0
	}

	var outputs [][]byte
	appendOutput := func(itemJSON []byte) {
		outputs = append(outputs, itemJSON)
	}
	detachedOutputIndex := 0
	seenDetachedOutputs := make(map[string]bool)
	appendDetachedOutput := func(signature, direction, targetKind string) {
		if signature == "" || seenDetachedOutputs[signature] {
			return
		}
		seenDetachedOutputs[signature] = true
		placement := "before"
		if direction == geminiResponsesCarrierPrevious {
			placement = "after"
		}
		itemJSON := []byte(`{"id":"","type":"reasoning","encrypted_content":"","summary":[]}`)
		itemJSON, _ = sjson.SetBytes(itemJSON, "id", fmt.Sprintf("rs_%s_detached_%s_%d", strings.TrimPrefix(id, "resp_"), placement, detachedOutputIndex))
		itemJSON, _ = sjson.SetBytes(itemJSON, "encrypted_content", encodeGeminiResponsesCarrier(signature, direction, targetKind))
		detachedOutputIndex++
		appendOutput(itemJSON)
	}

	if parts := root.Get("candidates.0.content.parts"); parts.Exists() && parts.IsArray() {
		parts.ForEach(func(key, p gjson.Result) bool {
			partIdx := int(key.Int())
			if pIdx := p.Get("partIndex"); pIdx.Exists() {
				partIdx = int(pIdx.Int())
			} else if pIdx := p.Get("index"); pIdx.Exists() {
				partIdx = int(pIdx.Int())
			}
			signature := strings.TrimSpace(p.Get("thoughtSignature").String())
			if signature == "" {
				signature = strings.TrimSpace(p.Get("thought_signature").String())
			}
			if p.Get("thought").Bool() {
				flushMessageOutput()
				currentMsgRuneOffset = 0
				if signature != "" && reasoningEncrypted != "" && signature != reasoningEncrypted {
					flushReasoningOutput()
				}
				if t := p.Get("text"); t.Exists() {
					reasoningText.WriteString(t.String())
				}
				if signature != "" {
					reasoningEncrypted = signature
					reasoningDirection = geminiResponsesCarrierStandalone
					reasoningTargetKind = geminiResponsesCarrierText
				}
				return true
			}
			if t := p.Get("text"); t.Exists() && t.String() != "" {
				messageSignature := ""
				if signature != "" {
					if reasoningText.Len() > 0 && reasoningEncrypted == "" {
						reasoningEncrypted = signature
						reasoningDirection = geminiResponsesCarrierNext
						reasoningTargetKind = geminiResponsesCarrierText
					} else {
						messageSignature = signature
					}
				}
				flushReasoningOutput()
				if len(currentMessageSignatures) > 0 && (messageSignature == "" || currentMessageSignatures[len(currentMessageSignatures)-1] != messageSignature) {
					flushMessageOutput()
					currentMsgRuneOffset = 0
				}
				partText := t.String()
				partMappings = append(partMappings, GeminiPartMapping{
					PartIndex:      partIdx,
					MessageIndex:   len(messageOutputs),
					StartRuneInMsg: currentMsgRuneOffset,
					PartText:       partText,
				})
				currentMsgRuneOffset += int64(utf8.RuneCountInString(partText))
				currentMessageText.WriteString(partText)
				if messageSignature != "" && (len(currentMessageSignatures) == 0 || currentMessageSignatures[len(currentMessageSignatures)-1] != messageSignature) {
					currentMessageSignatures = append(currentMessageSignatures, messageSignature)
				}
				return true
			}
			if fc := p.Get("functionCall"); fc.Exists() {
				if reasoningText.Len() > 0 && reasoningEncrypted == "" && signature != "" {
					reasoningEncrypted = signature
					reasoningDirection = geminiResponsesCarrierNext
					reasoningTargetKind = geminiResponsesCarrierFunction
					signature = ""
				}
				flushReasoningOutput()
				flushMessageOutput()
				currentMsgRuneOffset = 0

				rawName := fc.Get("name").String()
				identity, hasIdentity := toolIdentityMap[rawName]
				if !hasIdentity {
					restored := util.RestoreSanitizedToolName(sanitizedNameMap, rawName)
					identity = util.ResponsesToolIdentity{Name: restored}
				}
				name := identity.Name
				namespace := identity.Namespace
				isCustom := identity.Custom

				args := fc.Get("args")
				argsStr := ""
				if args.Exists() {
					argsStr = args.Raw
				}
				callID := fmt.Sprintf("call_%x_%d", time.Now().UnixNano(), atomic.AddUint64(&funcCallIDCounter, 1))
				var itemJSON []byte
				if isCustom {
					inputStr := util.UnwrapResponsesCustomToolInput(argsStr)
					itemJSON = []byte(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
					itemJSON, _ = sjson.SetBytes(itemJSON, "id", fmt.Sprintf("ctc_%s", callID))
					itemJSON, _ = sjson.SetBytes(itemJSON, "call_id", callID)
					itemJSON, _ = sjson.SetBytes(itemJSON, "input", inputStr)
					itemJSON = translatorcommon.SetResponsesToolCallIdentity(itemJSON, name, namespace, "")
				} else {
					itemJSON = []byte(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
					itemJSON, _ = sjson.SetBytes(itemJSON, "id", fmt.Sprintf("fc_%s", callID))
					itemJSON, _ = sjson.SetBytes(itemJSON, "call_id", callID)
					itemJSON, _ = translatorcommon.SetStringWithoutHTMLEscape(itemJSON, "arguments", argsStr)
					itemJSON = translatorcommon.SetResponsesToolCallIdentity(itemJSON, name, namespace, "")
				}
				functionIndex := len(functionOutputs)
				functionOutputs = append(functionOutputs, nonStreamFunctionOutput{item: itemJSON, signature: signature})
				outputOrder = append(outputOrder, nonStreamOutputOrder{kind: "function", index: functionIndex})
				return true
			}
			if signature != "" {
				if reasoningText.Len() > 0 {
					switch {
					case reasoningEncrypted == "":
						reasoningEncrypted = signature
						reasoningDirection = geminiResponsesCarrierStandalone
						reasoningTargetKind = geminiResponsesCarrierText
					case reasoningEncrypted != signature:
						flushReasoningOutput()
						detachedIndex := len(detachedReasoningOutputs)
						detachedReasoningOutputs = append(detachedReasoningOutputs, nonStreamDetachedOutput{signature: signature, direction: geminiResponsesCarrierPrevious, targetKind: geminiResponsesCarrierText})
						outputOrder = append(outputOrder, nonStreamOutputOrder{kind: "detached", index: detachedIndex})
					}
				} else if currentMessageText.Len() > 0 {
					if len(currentMessageSignatures) == 0 {
						currentMessageSignatures = append(currentMessageSignatures, signature)
					} else if currentMessageSignatures[len(currentMessageSignatures)-1] != signature {
						flushMessageOutput()
						currentMsgRuneOffset = 0
						detachedIndex := len(detachedReasoningOutputs)
						detachedReasoningOutputs = append(detachedReasoningOutputs, nonStreamDetachedOutput{signature: signature, direction: geminiResponsesCarrierPrevious, targetKind: geminiResponsesCarrierText})
						outputOrder = append(outputOrder, nonStreamOutputOrder{kind: "detached", index: detachedIndex})
					}
				} else if len(functionOutputs) > 0 {
					detachedIndex := len(detachedReasoningOutputs)
					detachedReasoningOutputs = append(detachedReasoningOutputs, nonStreamDetachedOutput{signature: signature, direction: geminiResponsesCarrierPrevious, targetKind: geminiResponsesCarrierFunction})
					outputOrder = append(outputOrder, nonStreamOutputOrder{kind: "detached", index: detachedIndex})
				} else {
					detachedIndex := len(detachedReasoningOutputs)
					detachedReasoningOutputs = append(detachedReasoningOutputs, nonStreamDetachedOutput{signature: signature, direction: geminiResponsesCarrierNext, targetKind: geminiResponsesCarrierAny})
					outputOrder = append(outputOrder, nonStreamOutputOrder{kind: "detached", index: detachedIndex})
				}
			}
			return true
		})
	}

	flushReasoningOutput()
	flushMessageOutput()

	// Web search handling from groundingMetadata
	groundingMetadata := ExtractGroundingMetadata(root)
	hasGrounding := HasValidWebGrounding(groundingMetadata)
	var wsItem []byte
	var messageCitations map[int][][]byte
	if hasGrounding {
		queries := ExtractGroundingQueries(groundingMetadata)
		query := ""
		if len(queries) > 0 {
			query = queries[0]
		}
		if query == "" && len(reqJSON) > 0 {
			query = ExtractResponsesWebSearchQuery(unwrapRequestRoot(gjson.ParseBytes(reqJSON)))
		}
		sources := ExtractGroundingSources(groundingMetadata)
		wsID := fmt.Sprintf("ws_%s", strings.TrimPrefix(id, "resp_"))
		wsItem = BuildResponsesWebSearchCallItem(wsID, query, queries, sources)

		messageTexts := make([]string, len(messageOutputs))
		for i, mo := range messageOutputs {
			messageTexts[i] = mo.text
		}
		messageCitations = BuildResponsesURLCitationsForMessages(groundingMetadata, partMappings, messageTexts)
	}

	wsAppended := false
	for _, outputItem := range outputOrder {
		switch outputItem.kind {
		case "detached":
			if outputItem.index < 0 || outputItem.index >= len(detachedReasoningOutputs) {
				continue
			}
			detached := detachedReasoningOutputs[outputItem.index]
			if !reasoningOutputSignatures[detached.signature] {
				appendDetachedOutput(detached.signature, detached.direction, detached.targetKind)
			}
		case "reasoning":
			if outputItem.index < 0 || outputItem.index >= len(reasoningOutputs) {
				continue
			}
			reasoningOutput := reasoningOutputs[outputItem.index]
			rid := strings.TrimPrefix(id, "resp_")
			reasoningID := fmt.Sprintf("rs_%s", rid)
			if len(reasoningOutputs) > 1 {
				reasoningID = fmt.Sprintf("rs_%s_%d", rid, outputItem.index)
			}
			itemJSON := []byte(`{"id":"","type":"reasoning","encrypted_content":""}`)
			itemJSON, _ = sjson.SetBytes(itemJSON, "id", reasoningID)
			encryptedContent := reasoningOutput.signature
			if encryptedContent != "" && reasoningOutput.direction != "" {
				encryptedContent = encodeGeminiResponsesCarrier(encryptedContent, reasoningOutput.direction, reasoningOutput.targetKind)
			}
			itemJSON, _ = sjson.SetBytes(itemJSON, "encrypted_content", encryptedContent)
			if reasoningOutput.text != "" {
				summaryJSON := []byte(`{"type":"summary_text","text":""}`)
				summaryJSON, _ = sjson.SetBytes(summaryJSON, "text", reasoningOutput.text)
				itemJSON, _ = sjson.SetRawBytes(itemJSON, "summary", translatorcommon.JoinRawArray([][]byte{summaryJSON}))
			}
			appendOutput(itemJSON)
		case "message":
			if hasGrounding && !wsAppended {
				appendOutput(wsItem)
				wsAppended = true
			}
			if outputItem.index < 0 || outputItem.index >= len(messageOutputs) {
				continue
			}
			messageOutput := messageOutputs[outputItem.index]
			for _, signature := range messageOutput.signatures {
				if !reasoningOutputSignatures[signature] {
					appendDetachedOutput(signature, geminiResponsesCarrierNext, geminiResponsesCarrierText)
				}
			}
			itemJSON := []byte(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
			itemJSON, _ = sjson.SetBytes(itemJSON, "id", fmt.Sprintf("msg_%s_%d", strings.TrimPrefix(id, "resp_"), outputItem.index))
			itemJSON, _ = sjson.SetBytes(itemJSON, "content.0.text", messageOutput.text)
			if c := messageCitations[outputItem.index]; len(c) > 0 {
				itemJSON, _ = sjson.SetRawBytes(itemJSON, "content.0.annotations", translatorcommon.JoinRawArray(c))
			}
			appendOutput(itemJSON)
		case "function":
			if outputItem.index < 0 || outputItem.index >= len(functionOutputs) {
				continue
			}
			functionOutput := functionOutputs[outputItem.index]
			appendDetachedOutput(functionOutput.signature, geminiResponsesCarrierNext, geminiResponsesCarrierFunction)
			appendOutput(functionOutput.item)
		}
	}

	if hasGrounding && !wsAppended {
		appendOutput(wsItem)
		wsAppended = true
	}

	if len(outputs) > 0 {
		resp, _ = sjson.SetRawBytes(resp, "output", translatorcommon.JoinRawArray(outputs))
	}

	if hasGrounding {
		resp, _ = sjson.SetBytes(resp, "tool_usage.web_search.num_requests", 1)
	}

	// usage mapping
	if um := root.Get("usageMetadata"); um.Exists() {
		// input tokens = prompt only (thoughts go to output)
		input := um.Get("promptTokenCount").Int()
		resp, _ = sjson.SetBytes(resp, "usage.input_tokens", input)
		// cached token details: align with OpenAI "cached_tokens" semantics.
		resp, _ = sjson.SetBytes(resp, "usage.input_tokens_details.cached_tokens", um.Get("cachedContentTokenCount").Int())
		// output tokens
		resp, _ = sjson.SetBytes(resp, "usage.output_tokens", um.Get("candidatesTokenCount").Int()+um.Get("thoughtsTokenCount").Int())
		if v := um.Get("thoughtsTokenCount"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "usage.output_tokens_details.reasoning_tokens", v.Int())
		}
		if v := um.Get("totalTokenCount"); v.Exists() {
			resp, _ = sjson.SetBytes(resp, "usage.total_tokens", v.Int())
		}
	}

	return resp
}
```

## `internal/runtime/executor/diagnostics_read_error_test.go`

SHA-256 (LF): `4eba6eaa0b1189507bf3ecd08dc8ae0afe57efaefa09a76b964e03b53546d507`

```go
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type diagnosticReadErrorBody struct {
	*strings.Reader
	closing, release chan struct{}
}

func (b *diagnosticReadErrorBody) Read(p []byte) (int, error) {
	if b.Reader.Len() > 0 {
		return b.Reader.Read(p)
	}
	return 0, errors.New("synthetic stream read failure")
}

func (b *diagnosticReadErrorBody) Close() error {
	close(b.closing)
	<-b.release
	return nil
}

func TestDIAG05GinReadErrorBeforeExchangeCleanup(t *testing.T) {
	coreusage.StartDefault(context.Background())
	cache.CacheSignature("diag05-fixture", "diagnostics fixture", strings.Repeat("x", cache.MinValidSignatureLen))
	defer cache.ClearSignatureCache("diag05-fixture")
	var artifact bytes.Buffer
	for _, provider := range []string{"gemini", "antigravity"} {
		for _, protocol := range []string{"gemini", "openai", "responses"} {
			for _, initial := range []string{"", `{"candidates":[{"content":{"parts":[{"text":"fixture output"}]}}]}`} {
				name := provider + "/" + protocol + "/bootstrap"
				if initial != "" {
					name = provider + "/" + protocol + "/partial"
				}
				t.Run(name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						gin.SetMode(gin.TestMode)
						var mu sync.Mutex
						var records []diagnostics.Record
						engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
							var r diagnostics.Record
							if err := json.Unmarshal(b[6:], &r); err != nil {
								return err
							}
							mu.Lock()
							records = append(records, r)
							artifact.Write(b)
							mu.Unlock()
							return nil
						})
						frame := initial
						if frame != "" {
							if provider == "antigravity" {
								frame = `{"response":` + frame + `}`
							}
							frame = "data: " + frame + "\n\n"
						}
						body := &diagnosticReadErrorBody{strings.NewReader(frame), make(chan struct{}), make(chan struct{})}
						manager := cliproxyauth.NewManager(nil, nil, nil)
						if provider == "gemini" {
							manager.RegisterExecutor(NewGeminiExecutor(&config.Config{}))
						} else {
							manager.RegisterExecutor(NewAntigravityExecutor(&config.Config{}))
						}
						manager.SetRoundTripperProvider(cancellationTransportProvider{semanticRT(func(*http.Request) (*http.Response, error) {
							return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
						})})
						authID, model := "diag05-read-auth", "gemini-2.5-flash"
						_, err := manager.Register(context.Background(), &cliproxyauth.Auth{ID: authID, Provider: provider, Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": "http://fixture.invalid", "api_key": "fixture-key"}, Metadata: map[string]any{"access_token": "fixture-token", "expired": time.Now().Add(24 * time.Hour).Format(time.RFC3339), "project_id": "fixture-project", "disable_cooling": true}})
						if err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
						defer registry.GetGlobalRegistry().UnregisterClient(authID)
						base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager)
						router := gin.New()
						router.Use(engine.GinMiddleware(func(*gin.Context) string { return "read-error-fixture" }))
						path, request := "/v1beta/models/"+model+":streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`
						if protocol == "gemini" {
							router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(base).GeminiHandler)
						} else if protocol == "responses" {
							path, request = "/v1/responses", `{"model":"`+model+`","stream":true,"input":"fixture"}`
							router.POST(path, openai.NewOpenAIResponsesAPIHandler(base).Responses)
						} else {
							path, request = "/v1/chat/completions", `{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"fixture"}]}`
							router.POST(path, openai.NewOpenAIAPIHandler(base).ChatCompletions)
						}
						writer := httptest.NewRecorder()
						handlerDone := make(chan struct{})
						go func() {
							defer close(handlerDone)
							router.ServeHTTP(writer, httptest.NewRequest("POST", path, strings.NewReader(request)))
						}()
						<-body.closing
						<-handlerDone // Real handlers return on Err without draining or releasing Close.
						mu.Lock()
						attempts, servers := 0, 0
						for _, r := range records {
							d, _ := r.Data.(map[string]any)
							if r.Event == "upstream.attempt_finished" {
								attempts++
								if servers != 0 || d["resultClass"] != "error" || d["failureStage"] != "read" || d["eofSeen"] != false {
									t.Errorf("read evidence lost: %v", d)
								}
							}
							if r.Event == "diag.server" {
								servers++
								if d["coverage"].(map[string]any)["debugCapture"] != "enabled_throughout" {
									t.Errorf("error delivery preceded settlement: %v", d)
								}
							}
						}
						before := len(records)
						mu.Unlock()
						completedTail := provider == "gemini" && protocol == "responses" && initial != ""
						if attempts != 1 || servers != 1 || (!completedTail && !strings.Contains(writer.Body.String(), `"error"`)) {
							t.Errorf("attempt=%d server=%d business error=%s", attempts, servers, writer.Body.String())
						}
						// Gemini's Responses converter emits a completed tail before Err; the forwarder
						// treats it as payload and still receives the error afterwards.
						if provider == "gemini" && protocol == "responses" && initial != "" && !strings.Contains(writer.Body.String(), "response.completed") {
							t.Error("existing Gemini DONE tail changed")
						}
						if provider == "antigravity" && (strings.Contains(writer.Body.String(), "[DONE]") || strings.Contains(writer.Body.String(), "response.completed")) {
							t.Error("Antigravity read error synthesized a clean tail")
						}
						close(body.release)
						synctest.Wait()
						mu.Lock()
						if len(records) != before {
							t.Error("late duplicate terminal")
						}
						mu.Unlock()
					})
				})
			}
		}
	}
	if path := os.Getenv("DIAG05_R2_TEST_RECORDS"); path != "" {
		if err := os.WriteFile(path, artifact.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
```

## `internal/runtime/executor/diagnostics_cancel_lifecycle_test.go`

SHA-256 (LF): `0d3b4f64c09eecd5d62057be7c3331ad027619f8c6d9599c85c766b84672fde1`

```go
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type cancellationTransportProvider struct{ http.RoundTripper }

func (p cancellationTransportProvider) RoundTripperFor(*cliproxyauth.Auth) http.RoundTripper {
	return p.RoundTripper
}

// The read ends on cancellation, but the existing executor Body.Close is held
// behind a channel. This forces Gin's server terminal before Exchange.Finish.
type cancellationCleanupBody struct {
	ctx                       context.Context
	first                     []byte
	reading, closing, release chan struct{}
}

func (b *cancellationCleanupBody) Read(p []byte) (int, error) {
	if len(b.first) > 0 {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	close(b.reading)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancellationCleanupBody) Close() error {
	close(b.closing)
	<-b.release
	return errors.New("synthetic delayed cleanup failure")
}

func TestDIAG05GinCancellationBeforeExchangeCleanup(t *testing.T) {
	// Keep the process-wide usage dispatcher outside the synctest bubble.
	coreusage.StartDefault(context.Background())
	var artifact bytes.Buffer
	for _, throttle := range []bool{false, true} {
		name := "waiting-upstream"
		if throttle {
			name = "waiting-throttle"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				var mu sync.Mutex
				var records []diagnostics.Record
				engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
					var r diagnostics.Record
					if err := json.Unmarshal(b[6:], &r); err != nil {
						return err
					}
					mu.Lock()
					records = append(records, r)
					artifact.Write(b)
					mu.Unlock()
					return nil
				})
				body := &cancellationCleanupBody{reading: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{})}
				if throttle {
					body.first = []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"fixture output\"}]}}]}\n\n")
				}
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(NewGeminiExecutor(&config.Config{}))
				manager.SetRoundTripperProvider(cancellationTransportProvider{semanticRT(func(r *http.Request) (*http.Response, error) {
					body.ctx = r.Context()
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
				})})
				authID, model := "diag05-cancel-auth", "gemini-2.5-flash"
				if _, err := manager.Register(context.Background(), &cliproxyauth.Auth{ID: authID, Provider: "gemini", Status: cliproxyauth.StatusActive, Attributes: map[string]string{"base_url": "http://fixture.invalid"}}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(authID, "gemini", []*registry.ModelInfo{{ID: model}})
				defer registry.GetGlobalRegistry().UnregisterClient(authID)
				cfg := &config.SDKConfig{SpeedThrottle: config.SpeedThrottleConfig{Enabled: throttle, MinTokensPerSecond: 1, MaxTokensPerSecond: 1, MinFirstTokenDelayMs: 1000, MaxFirstTokenDelayMs: 1000}}
				router := gin.New()
				router.Use(engine.GinMiddleware(func(*gin.Context) string { return "cancel-fixture" }))
				router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager)).GeminiHandler)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req := httptest.NewRequest("POST", "/v1beta/models/"+model+":streamGenerateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)).WithContext(ctx)
				writer := httptest.NewRecorder()
				handlerDone := make(chan struct{})
				go func() { defer close(handlerDone); router.ServeHTTP(writer, req) }()
				<-body.reading
				synctest.Wait()
				cancel()
				<-body.closing
				<-handlerDone // No stream drain and no cleanup release are needed to return.
				mu.Lock()
				serverCount, attempts, converted := 0, 0, 0
				for _, r := range records {
					switch r.Event {
					case "upstream.attempt_finished":
						attempts++
					case "response.converted":
						converted++
					case "diag.server":
						serverCount++
						d := r.Data.(map[string]any)
						coverage := d["coverage"].(map[string]any)
						if coverage["debugCapture"] != "interrupted" {
							t.Errorf("pending exchange silently omitted: coverage=%v", coverage)
						}
						if d["endReason"] != "client_cancel" || coverage["expectedLastLogSeq"] != float64(r.LogSeq) {
							t.Errorf("server terminal changed: %v", d)
						}
					}
				}
				mu.Unlock()
				if serverCount != 1 || attempts != 0 || converted != 0 || strings.Contains(writer.Body.String(), "fixture output") {
					t.Errorf("unexpected pre-cleanup result: server=%d attempt=%d converted=%d body=%d", serverCount, attempts, converted, writer.Body.Len())
				}
				close(body.release)
				synctest.Wait()
				mu.Lock()
				defer mu.Unlock()
				for _, r := range records {
					if r.Event == "upstream.attempt_finished" || r.Event == "response.converted" {
						t.Error("late observation appended after sealed server")
					}
				}
			})
		})
	}
	if path := os.Getenv("DIAG05_R1_TEST_RECORDS"); path != "" {
		if err := os.WriteFile(path, artifact.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
```

## `internal/diagnostics/protocol_limits_test.go`

SHA-256 (LF): `915966d897d6ee90cea3409177ad986e625de7c1a462b774158df0a898abe8d8`

```go
package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSemanticObservationLimitsAndUnknownFinish(t *testing.T) {
	var artifact bytes.Buffer
	good := `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
	media := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + strings.Repeat("A", maxObservationBytes) + `"}}]},"finishReason":"STOP"}]}`
	depth := `{"candidates":[{"finishReason":"STOP"}],"extra":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`
	many := `{"candidates":[` + strings.Repeat(`{"finishReason":"STOP"},`, 64) + `{"finishReason":"STOP"}]}`
	for _, tc := range []struct {
		name, payload, result, origin, stage, class string
		parsed                                      any
		count                                       any
	}{
		{"media-limit", media, "unknown", "unknown", "unknown", "unknown", nil, nil},
		{"depth-limit", depth, "unknown", "unknown", "unknown", "unknown", nil, nil},
		{"candidate-limit", many, "unknown", "unknown", "unknown", "unknown", nil, nil},
		{"unknown-finish", strings.Replace(good, "STOP", "MALFORMED_FUNCTION_CALL", 1), "unknown", "unknown", "unknown", "unknown", true, float64(4)},
		{"future-finish", strings.Replace(good, "STOP", "FUTURE_PROVIDER_VALUE", 1), "unknown", "unknown", "unknown", "unknown", true, float64(4)},
		{"bad-json", `{"candidates":`, "incomplete", "upstream", "parse", "parse_error", false, nil},
		{"non-object", `[]`, "incomplete", "upstream", "parse", "parse_error", false, nil},
	} {
		for _, stream := range []bool{false, true} {
			for _, ending := range []string{"clean", "read-error", "cancelled", "error-frame"} {
				mode := "nonstream"
				if stream {
					mode = "stream"
				}
				t.Run(tc.name+"/"+mode+"/"+ending, func(t *testing.T) {
					sink := &recordSink{}
					e := NewEngine(ResourceConfig{}, "", nil, func() bool { return true }, sink.write)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					ctx, span := e.StartServer(ctx, nil, "fixture")
					if tc.parsed != false && !json.Valid([]byte(tc.payload)) {
						t.Fatal("limit/unsupported fixture is not legal JSON")
					}
					x := NewExchange(ctx, "gemini", stream, stream)
					x.Status(200)
					// A later limit must invalidate earlier partial aggregate counts.
					if stream {
						x.Upstream([]byte(good))
					}
					x.Upstream([]byte(tc.payload))
					if stream {
						x.Delivered([]byte(good))
					}
					x.Delivered([]byte(tc.payload))
					var readErr error
					wantResult := tc.result
					wantCount := tc.count
					if !stream && wantCount != nil {
						wantCount = float64(2)
					}
					if ending == "cancelled" {
						cancel()
						wantResult = "cancelled"
					}
					if ending == "read-error" {
						readErr = errors.New("fixture")
						wantResult = "error"
					}
					if ending == "error-frame" {
						x.Upstream([]byte(`{"error":{"code":500}}`))
						wantResult = "error"
					}
					x.ReadFinished(readErr)
					x.Finish(readErr)
					span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
					d := sink.records(t, "upstream.attempt_finished")[0].Data.(map[string]any)
					if d["resultClass"] != wantResult {
						t.Fatalf("result %v", d)
					}
					if ending == "clean" && (d["failureOrigin"] != tc.origin || d["failureStage"] != tc.stage || d["errorClass"] != tc.class || d["parserFinishOk"] != tc.parsed) {
						t.Errorf("classification %v", d)
					}
					if got := d["output"].(map[string]any)["ordinaryTextUtf8Bytes"]; got != wantCount {
						t.Errorf("aggregate=%v want=%v", got, wantCount)
					}
					if tc.parsed == nil && d["parserFinishOk"] != nil {
						t.Error("local limit attributed to parser")
					}
					if tc.parsed == true && (d["terminalSeen"] != true || d["errorClass"] == "parse_error") {
						t.Error("unknown string finish lost terminal evidence")
					}
					converted := sink.records(t, "response.converted")[0].Data.(map[string]any)
					if converted["resultClass"] == "success" || converted["output"].(map[string]any)["ordinaryTextUtf8Bytes"] != wantCount {
						t.Errorf("converted partial/unsupported observation %v", converted)
					}
					for _, line := range sink.lines {
						artifact.Write(line)
					}
				})
			}
		}
	}
	if path := os.Getenv("DIAG05_R2_LIMIT_RECORDS"); path != "" {
		if err := os.WriteFile(path, artifact.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSemanticNormalizationContentsAndAttemptScope(t *testing.T) {
	sink := &recordSink{}
	e := NewEngine(ResourceConfig{}, "", nil, func() bool { return true }, sink.write)
	ctx, span := e.StartServer(context.Background(), nil, "fixture")
	before := []byte(`{"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[]}],"model":"a"}`)
	after := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[]}],"model":"b","generationConfig":{}}}`)
	ObserveNormalized(ctx, before, after)
	ObserveNormalized(ctx, before, []byte(strings.Replace(string(before), `"parts":[]`, `"parts":[{"text":"b"}]`, 1)))
	first := ExecutorAttempt(ctx, "gemini")
	other := ExecutorAttempt(ctx, "claude")
	second := ExecutorAttempt(other, "antigravity")
	for i, c := range []context.Context{first, second} {
		a := c.Value(attemptKey{}).(attemptIdentity)
		if a.number != uint64(i+1) || a.scope != "conductor_gemini_family" {
			t.Errorf("ambiguous attempt scope: %v", a)
		}
	}
	if other.Value(attemptKey{}) != nil {
		t.Error("unobserved provider acquired attempt identity")
	}
	span.Finish(ServerData{EndReason: "finished", DeliveryState: "unknown"})
	records := sink.records(t, "request.normalized")
	if len(records[0].Data.(map[string]any)["transformations"].([]any)) != 0 {
		t.Error("model/config change attributed to message zero")
	}
	changes := records[1].Data.(map[string]any)["transformations"].([]any)
	if len(changes) != 1 || changes[0].(map[string]any)["index"] != float64(1) {
		t.Errorf("wrong changed position: %v", changes)
	}
}
```
