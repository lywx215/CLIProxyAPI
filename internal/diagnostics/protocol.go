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
