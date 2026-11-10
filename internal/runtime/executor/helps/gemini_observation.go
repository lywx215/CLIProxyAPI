package helps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// GeminiObservation owns one model HTTP attempt. It never retains prompt or
// thought text and never changes a request or starts an upstream operation.
type GeminiObservation struct {
	mu                                      sync.Mutex
	now                                     func() time.Time
	started                                 time.Time
	ordinary                                bool
	expected                                int
	candidates                              map[int]string
	blocked, generated, ordinaryText, media bool
	terminalUsage                           bool
	terminalTrace, candidateTrace           string
	toolCalls                               int
	invalidTool                             bool
	usageSeen                               bool
	promptTokens, toolTokens                int64
	promptKnown                             bool
	detail                                  usage.Detail
	value                                   usage.Observation
}

func NewGeminiObservation(provider, requested string) *GeminiObservation {
	return &GeminiObservation{now: time.Now, ordinary: provider == "antigravity", expected: 1, candidates: map[int]string{},
		value: usage.Observation{Version: 1, RequestModel: requested, ResponseModelSequence: []string{}, CandidateFinishes: []usage.CandidateFinish{}, ModelMatch: "not_reported", UpstreamResult: "unknown", CompletionBasis: "none", TailVerification: "unverified", DeliveryResult: "not_tracked", UsageResult: "not_reported"}}
}

func geminiBusinessRoot(payload []byte) gjson.Result {
	r := gjson.ParseBytes(payload)
	if v := r.Get("response"); v.IsObject() {
		return v
	}
	if v := r.Get("request"); v.IsObject() {
		return v
	}
	return r
}

func observedString(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxResponseModelLength {
		return nil
	}
	return &s
}

func (o *GeminiObservation) Request(payload []byte, canonical string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	r := gjson.ParseBytes(payload)
	o.value.UpstreamRequestModel = observedString(r.Get("model").String())
	o.value.UpstreamCanonicalModel = observedString(canonical)
	if o.value.UpstreamCanonicalModel == nil {
		o.value.UpstreamCanonicalModel = observedString(r.Get("model").String())
	}
	count := geminiBusinessRoot(payload).Get("generationConfig.candidateCount").Int()
	thinking := geminiBusinessRoot(payload).Get("generationConfig.thinkingConfig")
	o.value.ThinkingLevel = observedString(thinking.Get("thinkingLevel").String())
	if v := thinking.Get("thinkingBudget"); v.Type == gjson.Number {
		b := v.Int()
		o.value.ThinkingBudget = &b
	}
	if v := thinking.Get("includeThoughts"); v.Type == gjson.True || v.Type == gjson.False {
		b := v.Bool()
		o.value.IncludeThoughts = &b
	}
	if count > 0 && count <= 128 {
		o.expected = int(count)
	}
}

func (o *GeminiObservation) Start(at time.Time, attempt uint64) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started.IsZero() {
		o.started = at
		o.value.AttemptNo = attempt
	}
}

func (o *GeminiObservation) Attempted() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.started.IsZero()
}

func (o *GeminiObservation) mark(dst **float64, at time.Time) {
	if *dst != nil || o.started.IsZero() {
		return
	}
	v := float64(at.Sub(o.started)) / float64(time.Millisecond)
	if v < 0 {
		v = 0
	}
	*dst = &v
}

func (o *GeminiObservation) FirstByte(at time.Time) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.mark(&o.value.UpstreamTTFBMS, at)
}

func (o *GeminiObservation) Observe(payload []byte, at time.Time) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !gjson.ValidBytes(payload) || !gjson.ParseBytes(payload).IsObject() {
		return o.failureLocked("invalid_generation_response", nil)
	}
	r := geminiBusinessRoot(payload)
	if r.Get("error").Exists() {
		return o.failureLocked("upstream_error", nil)
	}
	if model := observedString(r.Get("modelVersion").String()); model != nil {
		o.value.ResponseModel = model
		found := false
		for _, m := range o.value.ResponseModelSequence {
			if m == *model {
				found = true
				break
			}
		}
		if !found {
			if len(o.value.ResponseModelSequence) < 8 {
				o.value.ResponseModelSequence = append(o.value.ResponseModelSequence, *model)
			} else {
				o.value.ModelsTruncated = true
			}
		}
	}
	if reason := r.Get("promptFeedback.blockReason").String(); reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
		o.blocked = true
		o.value.CompletionBasis = "prompt_block"
	}
	candidates := r.Get("candidates").Array()
	if trace := geminiEventTrace(payload); o.ordinary && trace != "" && len(candidates) > 0 && !o.allFinished() {
		o.candidateTrace = trace
	}
	for _, c := range candidates {
		idx := int(c.Get("index").Int())
		if idx < 0 || idx >= 128 {
			return o.failureLocked("invalid_candidate_index", nil)
		}
		if _, ok := o.candidates[idx]; !ok {
			o.candidates[idx] = ""
		}
		previousFinish := o.candidates[idx]
		for _, p := range c.Get("content.parts").Array() {
			if previousFinish != "" && ((p.Get("text").Type == gjson.String && strings.TrimSpace(p.Get("text").String()) != "") || p.Get("functionCall").Exists() || p.Get("inlineData").Exists() || p.Get("executableCode").Exists()) {
				return o.failureLocked("content_after_terminal", nil)
			}
			if t := p.Get("text"); t.Type == gjson.String && strings.TrimSpace(t.String()) != "" {
				o.generated = true
				if p.Get("thought").Bool() {
					o.mark(&o.value.FirstSummaryMS, at)
				} else {
					o.ordinaryText = true
					o.mark(&o.value.FirstAnswerMS, at)
				}
			}
			if f := p.Get("functionCall"); validGeminiCall(f) {
				o.generated = true
				o.toolCalls++
				o.mark(&o.value.FirstToolCallMS, at)
			} else if f.Exists() {
				o.invalidTool = true
			}
			for _, field := range []string{"inlineData", "fileData", "executableCode", "codeExecutionResult"} {
				if p.Get(field).Exists() {
					o.media, o.generated = true, true
				}
			}
		}
		if reason := c.Get("finishReason").String(); reason != "" && reason != "FINISH_REASON_UNSPECIFIED" {
			if previousFinish != "" && previousFinish != reason {
				return o.failureLocked("conflicting_candidate_finish", nil)
			}
			if len(reason) <= 128 {
				o.candidates[idx] = reason
			}
			if !knownGeminiFinish(reason) {
				return o.failureLocked("unsupported_upstream_finish_reason", nil)
			}
		}
	}
	if o.allFinished() && o.terminalTrace == "" {
		o.terminalTrace = o.candidateTrace
	}
	if u := r.Get("usageMetadata"); u.IsObject() && o.relatedUsage(payload) {
		if o.mergeUsage(u) && o.allFinished() {
			o.terminalUsage = true
		}
	}
	return nil
}

func validGeminiCall(f gjson.Result) bool {
	return f.IsObject() && strings.TrimSpace(f.Get("name").String()) != "" && (!f.Get("args").Exists() || f.Get("args").IsObject())
}

// ObserveErrorFrame keeps partial accounting evidence without creating semantic
// first-output timings from a failure envelope.
func (o *GeminiObservation) ObserveErrorFrame(raw []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := geminiBusinessRoot(raw)
	if u := r.Get("usageMetadata"); u.IsObject() && o.relatedUsage(raw) {
		o.mergeUsage(u)
	}
	for _, c := range r.Get("candidates").Array() {
		for _, p := range c.Get("content.parts").Array() {
			if strings.TrimSpace(p.Get("text").String()) != "" || validGeminiCall(p.Get("functionCall")) || p.Get("inlineData").Exists() || p.Get("fileData").Exists() || p.Get("executableCode").Exists() || p.Get("codeExecutionResult").Exists() {
				o.generated = true
			}
		}
	}
}

func geminiEventTrace(payload []byte) string {
	trace := gjson.GetBytes(payload, "traceId").String()
	if len(trace) > 256 {
		return ""
	}
	return trace
}

func (o *GeminiObservation) relatedUsage(payload []byte) bool {
	trace := geminiEventTrace(payload)
	return !o.ordinary || o.terminalTrace == "" || trace == "" || trace == o.terminalTrace
}

// TranslationPayload preserves a correlated usage tail even without a trace ID.
// Interim usage retains the existing client-facing filter.
func (o *GeminiObservation) TranslationPayload(payload []byte) []byte {
	if !o.ordinary {
		return payload
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.terminalUsage && o.relatedUsage(payload) {
		return payload
	}
	filtered, _ := StripUsageMetadataFromJSON(payload)
	return filtered
}

func knownGeminiFinish(s string) bool {
	switch s {
	case "STOP", "MAX_TOKENS", "SAFETY", "RECITATION", "LANGUAGE", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MALFORMED_FUNCTION_CALL", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_OTHER", "NO_IMAGE", "IMAGE_RECITATION", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "CONTINUATION":
		return true
	}
	return false
}

func (o *GeminiObservation) allFinished() bool {
	if len(o.candidates) != o.expected {
		return false
	}
	for idx := 0; idx < o.expected; idx++ {
		if !knownGeminiFinish(o.candidates[idx]) {
			return false
		}
	}
	return true
}

// GeminiDiagnosticError leaves parse and missing-terminal classification to the
// existing diagnostics parser while keeping the execution error unchanged.
func GeminiDiagnosticError(err error) error {
	f := coreexec.FactsFromError(err)
	if f != nil && (f.Code == "incomplete_generation_response" || f.Code == "invalid_generation_response") {
		return nil
	}
	return err
}

func (o *GeminiObservation) Ready() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.blocked || (o.allFinished() && (!o.ordinary || o.terminalUsage))
}

func (o *GeminiObservation) HasFinish() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hasFinish()
}

func (o *GeminiObservation) hasFinish() bool {
	if o.blocked {
		return true
	}
	for _, r := range o.candidates {
		if r != "" {
			return true
		}
	}
	return false
}

func (o *GeminiObservation) mergeUsage(node gjson.Result) bool {
	if o.detail.TokenPresence == nil {
		o.detail.TokenPresence = &usage.TokenPresence{}
	}
	p := o.detail.TokenPresence
	valid := false
	fields := []struct {
		key     string
		value   *int64
		present *bool
	}{
		{"promptTokenCount", &o.promptTokens, &o.promptKnown}, {"candidatesTokenCount", &o.detail.OutputTokens, &p.OutputTokens},
		{"thoughtsTokenCount", &o.detail.ReasoningTokens, &p.ReasoningTokens}, {"cachedContentTokenCount", &o.detail.CachedTokens, &p.CachedTokens},
		{"totalTokenCount", &o.detail.TotalTokens, &p.TotalTokens},
	}
	for _, f := range fields {
		v := node.Get(f.key)
		if v.Type == gjson.Number && v.Int() >= 0 && float64(v.Int()) == v.Float() {
			*f.value, *f.present, valid = v.Int(), true, true
		}
	}
	if v := firstExistingUsageNode(node, "toolUsePromptTokenCount", "tool_use_prompt_token_count"); v.Type == gjson.Number && v.Int() >= 0 && float64(v.Int()) == v.Float() {
		o.toolTokens = v.Int()
	}
	if o.promptKnown {
		o.detail.InputTokens, p.InputTokens = safeUsageTokenSum(o.promptTokens, o.toolTokens)
	}
	if p.CachedTokens {
		p.CacheReadTokens = true
		o.detail.CacheReadTokens = o.detail.CachedTokens
	}
	if p.OutputTokens && o.detail.OutputTokens > 0 || p.ReasoningTokens && o.detail.ReasoningTokens > 0 {
		o.generated = true
	}
	o.usageSeen = o.usageSeen || valid
	return valid
}

func (o *GeminiObservation) failureLocked(code string, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("%s", code)
	}
	generation := "unknown"
	if o.generated || coreexec.GenerationHasStarted(cause) {
		generation = "started"
	}
	facts := coreexec.FailureFacts{Code: code, Stage: "stream", Scope: "request", SendState: "response_received", Generation: generation, Replay: "unsafe"}
	var status coreexec.StatusError
	if errors.As(cause, &status) {
		facts.HTTPStatus = status.StatusCode()
		if !o.generated && !o.hasFinish() && !coreexec.GenerationHasStarted(cause) && facts.HTTPStatus >= 400 && facts.HTTPStatus < 500 {
			facts.Generation, facts.Replay = "not_started", "safe"
		}
	}
	if code == "upstream_stream" {
		applyGeminiFailureScope(&facts, cause)
	}
	wrapped := coreexec.WithFailureFacts(cause, facts)
	o.value.Error = coreexec.FactsFromError(wrapped)
	o.value.UpstreamResult = "incomplete"
	return wrapped
}

func applyGeminiFailureScope(f *coreexec.FailureFacts, err error) {
	switch f.HTTPStatus {
	case 401, 403:
		f.Scope = "credential"
	case 429:
		f.Scope = "model"
	}
	var scoped interface {
		FailureScope() string
		FailureCode() string
	}
	if errors.As(err, &scoped) {
		if v := scoped.FailureScope(); v != "" && len(v) <= 64 {
			f.Scope = v
		}
		if v := scoped.FailureCode(); v != "" && len(v) <= 128 {
			f.Code = v
		}
	}
}

// LocalFailure records conversion failure without changing the Google outcome.
func (o *GeminiObservation) LocalFailure(code string, cause error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cause == nil {
		cause = fmt.Errorf("%s", code)
	}
	generation := "unknown"
	if o.generated {
		generation = "started"
	}
	err := coreexec.WithFailureFacts(cause, coreexec.FailureFacts{Code: code, Stage: "translation", Scope: "request", SendState: "response_received", Generation: generation, Replay: "unsafe"})
	o.value.Error = coreexec.FactsFromError(err)
	return err
}

func (o *GeminiObservation) ForbidReplay(err error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	f := coreexec.FactsFromError(err)
	if f == nil {
		return err
	}
	f.Replay = "unsafe"
	o.value.Error = f
	return coreexec.WithFailureFacts(err, *f)
}

func (r *UsageReporter) GeminiTranslationFailure(cause error) error {
	if r == nil || r.geminiObservation == nil {
		return cause
	}
	return r.geminiObservation.LocalFailure("response_translation_failed", cause)
}

// Finish never treats a locally stopped read as an upstream EOF.
func (o *GeminiObservation) Finish(eof, residual bool, trailer http.Header, err error) error {
	if o == nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if eof {
		o.value.TailVerification = "verified"
		if s := trailer.Get("Grpc-Status"); s != "" && s != "0" {
			o.value.TailVerification = "failed"
			return o.failureLocked("upstream_trailer_error", err)
		}
	}
	if err != nil {
		if facts := coreexec.FactsFromError(err); facts != nil {
			o.value.Error = facts
			if facts.Stage == "translation" || facts.Stage == "delivery" {
				return err
			}
		} else {
			err = o.failureLocked("upstream_stream", err)
		}
		o.value.UpstreamResult = "incomplete"
		if o.value.Error != nil && o.value.Error.Generation == "not_started" && o.value.Error.Replay == "safe" {
			o.value.UpstreamResult = "rejected"
		}
		return err
	}
	if o.blocked {
		o.value.UpstreamResult, o.value.CompletionBasis = "terminal_observed", "prompt_block"
	} else if o.allFinished() {
		o.value.UpstreamResult, o.value.CompletionBasis = "terminal_observed", "candidate_finish"
		for _, reason := range o.candidates {
			if reason == "CONTINUATION" {
				o.value.UpstreamResult = "continuation_required"
			}
		}
	} else if eof && !residual && o.ordinary && len(o.candidates) == 1 && o.expected == 1 && o.toolCalls > 0 && !o.invalidTool && !o.ordinaryText && !o.media && o.usageSeen {
		o.value.UpstreamResult, o.value.CompletionBasis = "compatibility_end", "compat_tool_eof"
	} else {
		return o.failureLocked("incomplete_generation_response", nil)
	}
	if eof && o.value.UpstreamResult == "terminal_observed" {
		o.value.UpstreamResult = "complete"
	}
	return nil
}

func (o *GeminiObservation) Delivery(err error, tracked bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !tracked {
		return
	}
	o.value.DeliveryResult = "written"
	if err != nil {
		if errors.Is(err, context.Canceled) {
			o.value.DeliveryResult = "cancelled"
		} else {
			o.value.DeliveryResult = "write_failed"
		}
		if o.value.Error == nil {
			code := "downstream_write_failed"
			if errors.Is(err, context.Canceled) {
				code = "client_cancelled"
			}
			generation := "unknown"
			if o.generated {
				generation = "started"
			}
			o.value.Error = &coreexec.FailureFacts{Code: code, Stage: "delivery", Scope: "request", SendState: "response_received", Generation: generation, Replay: "unsafe"}
		}
	}
}

func (o *GeminiObservation) Snapshot() (*usage.Observation, usage.Detail) {
	if o == nil {
		return nil, usage.Detail{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	v := o.value.Clone()
	v.CandidateFinishes = []usage.CandidateFinish{}
	for idx, reason := range o.candidates {
		if reason != "" {
			v.CandidateFinishes = append(v.CandidateFinishes, usage.CandidateFinish{Index: idx, Reason: reason})
		}
	}
	sort.Slice(v.CandidateFinishes, func(i, j int) bool { return v.CandidateFinishes[i].Index < v.CandidateFinishes[j].Index })
	if v.ResponseModel != nil {
		v.ModelMatch = "exact"
		for _, model := range v.ResponseModelSequence {
			if v.UpstreamRequestModel != nil && model == *v.UpstreamRequestModel {
				continue
			}
			if v.UpstreamCanonicalModel != nil && model == *v.UpstreamCanonicalModel {
				if v.ModelMatch != "different" {
					v.ModelMatch = "mapped"
				}
			} else {
				v.ModelMatch = "different"
			}
		}
		if v.ModelsTruncated {
			v.ModelMatch = "different"
		}
	}
	d := o.detail
	if d.TokenPresence != nil {
		p := *d.TokenPresence
		d.TokenPresence = &p
	}
	if o.usageSeen {
		v.UsageResult = "official_partial"
		if v.TailVerification == "verified" && d.TokenPresence != nil && d.TokenPresence.InputTokens && d.TokenPresence.OutputTokens && d.TokenPresence.ReasoningTokens && d.TokenPresence.TotalTokens {
			v.UsageResult = "official_complete"
		}
	} else {
		d.TokenPresence = &usage.TokenPresence{}
	}
	return v, d
}

type geminiReporterKey struct{}

func WithGeminiReporter(ctx context.Context, r *UsageReporter) context.Context {
	return context.WithValue(ctx, geminiReporterKey{}, r)
}
func ObserveFinalGeminiPayload(ctx context.Context, body []byte) {
	if r, ok := ctx.Value(geminiReporterKey{}).(*UsageReporter); ok {
		r.ObserveGeminiRequest(body)
	}
}
func (r *UsageReporter) ObserveGeminiRequest(body []byte) {
	if r == nil || r.geminiObservation == nil {
		return
	}
	r.geminiObservation.Request(body, r.UpstreamModel())
	format := "gemini"
	if gjson.GetBytes(body, "request").IsObject() {
		format = "antigravity"
	}
	r.SetTranslatedReasoningEffort(body, format)
}

func (r *UsageReporter) observeGeminiFailure(errs ...error) {
	if r == nil || r.geminiObservation == nil {
		return
	}
	o := r.geminiObservation
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.value.Error != nil {
		return
	}
	for _, err := range errs {
		if err == nil {
			continue
		}
		f := coreexec.FactsFromError(err)
		if f == nil {
			f = &coreexec.FailureFacts{Code: "upstream_rejection", Stage: "response", Scope: "request", SendState: "response_received", Generation: "not_started", Replay: "safe"}
			var status coreexec.StatusError
			if errors.As(err, &status) {
				f.HTTPStatus = status.StatusCode()
			}
			applyGeminiFailureScope(f, err)
			if o.started.IsZero() {
				f.Stage = "admission"
				f.SendState = "not_sent"
				o.value.UpstreamResult = "not_sent"
			} else {
				o.value.UpstreamResult = "rejected"
			}
			if f.HTTPStatus >= 500 {
				f.Generation = "unknown"
				f.Replay = "unsafe"
			}
			if o.generated || coreexec.GenerationHasStarted(err) {
				f.Generation, f.Replay = "started", "unsafe"
				o.value.UpstreamResult = "incomplete"
			}
			var retry interface{ RetryAfter() *time.Duration }
			if errors.As(err, &retry) {
				if d := retry.RetryAfter(); d != nil {
					ms := float64(*d) / float64(time.Millisecond)
					f.RetryMS = &ms
				}
			}
		}
		if f.SendState == "not_sent" {
			o.value.UpstreamResult = "not_sent"
		}
		o.value.Error = f.Clone()
		return
	}
}

// GeminiResponseFailure shares the same facts with retry policy and accounting.
func (r *UsageReporter) GeminiResponseFailure(raw []byte, err error) error {
	if r == nil || r.geminiObservation == nil {
		return err
	}
	r.geminiObservation.ObserveErrorFrame(raw)
	r.observeGeminiFailure(err)
	v, _ := r.geminiObservation.Snapshot()
	if v.Error == nil {
		return err
	}
	return coreexec.WithFailureFacts(err, *v.Error)
}

// GeminiReadFailure preserves both the read cause and any complete raw facts
// available before a truncated non-streaming body, without permitting replay.
func (r *UsageReporter) GeminiReadFailure(raw []byte, cause error) error {
	if r == nil || r.geminiObservation == nil {
		return cause
	}
	o := r.geminiObservation
	if gjson.ValidBytes(raw) {
		_ = o.Observe(raw, o.now())
	}
	return o.Finish(false, true, nil, cause)
}
