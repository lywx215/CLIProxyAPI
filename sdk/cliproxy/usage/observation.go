package usage

import (
	"maps"
	"math"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Observation is optional on historical records. Null timings mean unobserved;
// an absent observation means the producer did not record these facts.
type Observation struct {
	Version                int                        `json:"version"`
	AttemptNo              uint64                     `json:"attempt_no"`
	RequestModel           string                     `json:"request_model"`
	ThinkingLevel          *string                    `json:"thinking_level"`
	ThinkingBudget         *int64                     `json:"thinking_budget"`
	IncludeThoughts        *bool                      `json:"include_thoughts"`
	UpstreamRequestModel   *string                    `json:"upstream_request_model"`
	UpstreamCanonicalModel *string                    `json:"upstream_canonical_model"`
	ResponseModel          *string                    `json:"response_model"`
	ResponseModelSequence  []string                   `json:"response_model_sequence"`
	ModelsTruncated        bool                       `json:"models_truncated"`
	ModelMatch             string                     `json:"model_match"`
	UpstreamTTFBMS         *float64                   `json:"upstream_ttfb_ms"`
	FirstSummaryMS         *float64                   `json:"first_summary_ms"`
	FirstAnswerMS          *float64                   `json:"first_answer_ms"`
	FirstToolCallMS        *float64                   `json:"first_tool_call_ms"`
	UpstreamResult         string                     `json:"upstream_result"`
	CompletionBasis        string                     `json:"completion_basis"`
	CandidateFinishes      []CandidateFinish          `json:"candidate_finishes"`
	TailVerification       string                     `json:"tail_verification"`
	DeliveryResult         string                     `json:"delivery_result"`
	UsageResult            string                     `json:"usage_result"`
	Error                  *coreexecutor.FailureFacts `json:"error"`
	Web                    *WebObservation            `json:"web,omitempty"`
}

// WebObservation separates the Plus transport and answer from the client write.
type WebObservation struct {
	UpstreamHTTPStatus int                `json:"upstream_http_status,omitempty"`
	ClientHTTPStatus   int                `json:"client_http_status,omitempty"`
	AnswerState        string             `json:"answer_state,omitempty"`
	GoogleStatus       string             `json:"google_status,omitempty"`
	GoogleReason       string             `json:"google_reason,omitempty"`
	Submitted          bool               `json:"submitted"`
	OutputStarted      bool               `json:"output_started"`
	Contents           int                `json:"contents"`
	Attachments        int                `json:"attachments"`
	AttachmentBytes    int                `json:"attachment_bytes"`
	StagesMS           map[string]float64 `json:"stages_ms"`
	TotalMS            float64            `json:"total_ms"`
}

type CandidateFinish struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

func cloneObservationPtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (o *Observation) Clone() *Observation {
	if o == nil {
		return nil
	}
	c := *o
	c.ThinkingLevel = cloneObservationPtr(o.ThinkingLevel)
	c.ThinkingBudget = cloneObservationPtr(o.ThinkingBudget)
	c.IncludeThoughts = cloneObservationPtr(o.IncludeThoughts)
	c.UpstreamRequestModel = cloneObservationPtr(o.UpstreamRequestModel)
	c.UpstreamCanonicalModel = cloneObservationPtr(o.UpstreamCanonicalModel)
	c.ResponseModel = cloneObservationPtr(o.ResponseModel)
	c.UpstreamTTFBMS = cloneObservationPtr(o.UpstreamTTFBMS)
	c.FirstSummaryMS = cloneObservationPtr(o.FirstSummaryMS)
	c.FirstAnswerMS = cloneObservationPtr(o.FirstAnswerMS)
	c.FirstToolCallMS = cloneObservationPtr(o.FirstToolCallMS)
	c.ResponseModelSequence = append([]string{}, o.ResponseModelSequence...)
	c.CandidateFinishes = append([]CandidateFinish{}, o.CandidateFinishes...)
	c.Error = o.Error.Clone()
	if o.Web != nil {
		web := *o.Web
		web.StagesMS = maps.Clone(o.Web.StagesMS)
		c.Web = &web
	}
	return &c
}

// Failed projects the new independent outcomes onto the historical result flag.
func (o *Observation) Failed() bool {
	return o != nil && (o.Error != nil || o.UpstreamResult == "incomplete" || o.UpstreamResult == "rejected" || o.TailVerification == "failed" || o.DeliveryResult == "write_failed" || o.DeliveryResult == "cancelled")
}

// Valid rejects unknown versions and malformed observation data independently
// of the historical accounting record containing it.
func (o *Observation) Valid() bool {
	if o == nil || o.Version != 1 || len(o.RequestModel) == 0 || len(o.RequestModel) > 128 || len(o.ResponseModelSequence) > 8 || len(o.CandidateFinishes) > 128 {
		return false
	}
	in := func(value string, allowed ...string) bool {
		for _, a := range allowed {
			if a == value {
				return true
			}
		}
		return false
	}
	if !in(o.ModelMatch, "exact", "mapped", "different", "not_reported") || !in(o.UpstreamResult, "complete", "terminal_observed", "compatibility_end", "incomplete", "rejected", "unknown", "not_sent", "continuation_required") || !in(o.TailVerification, "verified", "unverified", "failed") || !in(o.DeliveryResult, "written", "write_failed", "cancelled", "not_tracked") || !in(o.UsageResult, "official_complete", "official_partial", "estimated", "not_reported") || !in(o.CompletionBasis, "candidate_finish", "prompt_block", "compat_tool_eof", "web_answer_state", "none") {
		return false
	}
	if o.CompletionBasis == "web_answer_state" && o.Web == nil {
		return false
	}
	if w := o.Web; w != nil {
		statusOK := func(s int) bool { return s == 0 || s >= 100 && s <= 599 }
		timeOK := func(ms float64) bool { return ms >= 0 && !math.IsNaN(ms) && !math.IsInf(ms, 0) }
		if !statusOK(w.UpstreamHTTPStatus) || !statusOK(w.ClientHTTPStatus) || !timeOK(w.TotalMS) ||
			w.Contents < 0 || w.Attachments < 0 || w.AttachmentBytes < 0 || len(w.StagesMS) > 7 ||
			!in(w.AnswerState, "", "IN_PROGRESS", "SUCCEEDED", "FAILED", "SKIPPED", "CANCELLED") ||
			!validWebGoogleCode(w.GoogleStatus) || !validWebGoogleCode(w.GoogleReason) {
			return false
		}
		for stage, ms := range w.StagesMS {
			if !in(stage, "validation", "authentication", "session", "upload", "generation", "parse", "delivery") || !timeOK(ms) {
				return false
			}
		}
	}
	for _, p := range []*float64{o.UpstreamTTFBMS, o.FirstSummaryMS, o.FirstAnswerMS, o.FirstToolCallMS} {
		if p != nil && (*p < 0 || math.IsNaN(*p) || math.IsInf(*p, 0)) {
			return false
		}
	}
	for _, p := range []*string{o.UpstreamRequestModel, o.UpstreamCanonicalModel, o.ResponseModel, o.ThinkingLevel} {
		if p != nil && (len(*p) == 0 || len(*p) > 128) {
			return false
		}
	}
	for _, m := range o.ResponseModelSequence {
		if len(m) == 0 || len(m) > 128 {
			return false
		}
	}
	for _, f := range o.CandidateFinishes {
		if f.Index < 0 || f.Index >= 128 || len(f.Reason) == 0 || len(f.Reason) > 128 {
			return false
		}
	}
	if e := o.Error; e != nil {
		if len(e.Code) == 0 || len(e.Code) > 128 || len(e.Stage) == 0 || len(e.Stage) > 64 || len(e.Scope) == 0 || len(e.Scope) > 64 || e.HTTPStatus < 0 || e.HTTPStatus > 599 || !in(e.Generation, "started", "not_started", "unknown") || !in(e.SendState, "not_sent", "possibly_sent", "response_received") || !in(e.Replay, "safe", "unsafe") {
			return false
		}
		if e.RetryMS != nil && (*e.RetryMS < 0 || math.IsInf(*e.RetryMS, 0) || math.IsNaN(*e.RetryMS)) {
			return false
		}
	}
	return true
}

func validWebGoogleCode(s string) bool {
	if len(s) > 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'A' || c > 'Z') && (i == 0 || (c < '0' || c > '9') && c != '_') {
			return false
		}
	}
	return true
}
