package enterpriseweb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/core"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

type observationKey struct{}

// ExecutionObservation follows one Web attempt. Only the owner of that
// attempt updates it; the delivery acknowledgment is synchronized separately.
type ExecutionObservation struct {
	value                       usage.Observation
	start, phaseStart, submitAt time.Time
	phase                       string
	phaseStatus                 int
	trace, authIndex, model     string
	stream                      bool
	summary                     contract.ContextSummary
}

func NewExecutionObservation(model, trace, authIndex string, stream bool) *ExecutionObservation {
	model = observationModel(model)
	return &ExecutionObservation{
		start: time.Now(), trace: trace, authIndex: authIndex, stream: stream,
		value: usage.Observation{
			Version: 1, RequestModel: model, ModelMatch: "not_reported",
			ResponseModelSequence: []string{}, CandidateFinishes: []usage.CandidateFinish{},
			UpstreamResult: "not_sent", CompletionBasis: "none", TailVerification: "unverified",
			DeliveryResult: "not_tracked", UsageResult: "not_reported",
			Web: &usage.WebObservation{StagesMS: make(map[string]float64, 7)},
		},
	}
}

func observationModel(model string) string {
	if len(model) == 0 || len(model) > 128 {
		return "unknown"
	}
	for i := range len(model) {
		c := model[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '/' || c == '(' || c == ')') {
			return "unknown"
		}
	}
	return model
}

func (o *ExecutionObservation) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, observationKey{}, o)
}

func (o *ExecutionObservation) SetModel(model string) {
	o.model = observationModel(model)
	if o.model != "unknown" {
		o.value.UpstreamRequestModel = &o.model
		o.value.UpstreamCanonicalModel = &o.model
	}
}

func (o *ExecutionObservation) SetSummary(summary contract.ContextSummary) {
	o.summary = summary
	o.value.Web.Contents = summary.Contents
	o.value.Web.Attachments = summary.Attachments
	o.value.Web.AttachmentBytes = summary.AttachmentBytes
}

func webMillis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (o *ExecutionObservation) endPhase(now time.Time) {
	if o.phase != "" {
		o.value.Web.StagesMS[o.phase] += webMillis(now.Sub(o.phaseStart))
	}
}

func (o *ExecutionObservation) Stage(ctx context.Context, stage string) {
	now := time.Now()
	o.endPhase(now)
	o.phase, o.phaseStart, o.phaseStatus = stage, now, 0
	ObserveStage(ctx, stage, o.model, o.summary)
}

func observeWebHTTPSend(ctx context.Context) {
	o, ok := ctx.Value(observationKey{}).(*ExecutionObservation)
	if !ok {
		return
	}
	o.phaseStatus = 0
	if o.phase != "generation" {
		return
	}
	o.submitAt = time.Now()
	o.value.AttemptNo = usage.NextModelAttempt(ctx)
	o.value.UpstreamResult = "unknown"
	o.value.Web.Submitted = true
}

func (o *ExecutionObservation) Complete() bool  { return o.value.UpstreamResult == "complete" }
func (o *ExecutionObservation) Submitted() bool { return o.value.Web.Submitted }

func observeWebHTTPStatus(ctx context.Context, status int) {
	if o, ok := ctx.Value(observationKey{}).(*ExecutionObservation); ok {
		o.phaseStatus = status
		if o.phase == "generation" {
			o.value.Web.UpstreamHTTPStatus = status
		}
	}
}

type firstWebByte struct {
	io.Reader
	observation *ExecutionObservation
}

func (r firstWebByte) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	o := r.observation
	if n > 0 && o.value.UpstreamTTFBMS == nil {
		ms := webMillis(time.Since(o.submitAt))
		o.value.UpstreamTTFBMS = &ms
	}
	return n, err
}

func (o *ExecutionObservation) Parse(ctx context.Context, body io.Reader, emit func(contract.Event) error) error {
	o.Stage(ctx, "parse")
	err := core.ParseObservedStream(firstWebByte{body, o}, func(event contract.Event) error {
		for _, part := range event.Parts {
			o.value.Web.OutputStarted = true
			if part.Text != "" {
				first := &o.value.FirstAnswerMS
				if part.Thought {
					first = &o.value.FirstSummaryMS
				}
				if *first == nil {
					ms := webMillis(time.Since(o.submitAt))
					*first = &ms
				}
			}
		}
		if event.Finished {
			o.value.UpstreamResult = "complete"
			o.value.TailVerification = "verified"
		}
		return emit(event)
	}, func(state string) {
		o.value.Web.AnswerState = state
		if state != "IN_PROGRESS" {
			o.value.CompletionBasis = "web_answer_state"
		}
	})
	if err == nil {
		return nil
	}
	f := core.DescribeStreamFailure(err)
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		f.Code == "upstream_stream_read_failed" || f.Code == "stream_truncated_json") {
		return o.operationFailure(ctx.Err())
	}
	o.value.Web.GoogleStatus, o.value.Web.GoogleReason = f.GoogleStatus, f.GoogleReason
	if f.Rejected {
		o.value.UpstreamResult = "rejected"
	} else {
		o.value.UpstreamResult, o.value.TailVerification = "incomplete", "failed"
	}
	message := "Gemini Enterprise Web returned an incomplete or invalid response"
	if f.Rejected {
		message = "Gemini Enterprise Web upstream did not complete the answer"
	}
	return o.fail(core.Status(err), f.Code, message)
}

// OperationFailure preserves the effective status and records failures before
// parsing begins. The message is projected from safe local error data only.
func (o *ExecutionObservation) OperationFailure(ctx context.Context, err error) error {
	if !o.value.Web.Submitted {
		o.value.Web.UpstreamHTTPStatus = o.phaseStatus
	}
	if o.value.Web.UpstreamHTTPStatus == 0 && o.phaseStatus == 0 && ctx.Err() != nil {
		err = ctx.Err()
	}
	return o.operationFailure(err)
}

func (o *ExecutionObservation) operationFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return o.fail(499, "web_request_cancelled", "Media request was cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return o.fail(504, "web_request_deadline", "Media request deadline exceeded")
	}
	status := failureStatus(err)
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	message, code := PublicErrorDetail(err, status)
	if message == "" {
		message = "Gemini Enterprise Web request failed"
	}
	if code == "" || code == "media_input_error" {
		code = "web_" + o.phase + "_failed"
		if o.phase == "generation" {
			code = "upstream_connection_failed"
			if o.value.Web.UpstreamHTTPStatus != 0 {
				code = "upstream_http_error"
				o.value.UpstreamResult = "rejected"
			}
		}
	}
	return o.fail(status, code, message)
}

func (o *ExecutionObservation) fail(status int, code, message string) error {
	o.setFailure(o.phase, status, code)
	return InputFailure(status, code, "", message)
}

func (o *ExecutionObservation) setFailure(stage string, status int, code string) {
	send, generation := "not_sent", "not_started"
	if o.value.Web.Submitted {
		send, generation = "possibly_sent", "unknown"
		if o.value.Web.UpstreamHTTPStatus != 0 {
			send = "response_received"
		}
		if o.value.Web.AnswerState != "" || o.value.Web.OutputStarted {
			generation = "started"
		}
	}
	o.value.Error = &exec.FailureFacts{
		Code: code, Stage: stage, Scope: "request", HTTPStatus: status,
		SendState: send, Generation: generation, Replay: "unsafe",
	}
}

// Finish freezes one result after the writer has acknowledged delivery. It
// never changes a verified upstream answer into an upstream failure.
func (o *ExecutionObservation) Finish(ctx context.Context, err, deliveryErr error, tracked, estimated bool) (*usage.Observation, error) {
	now := time.Now()
	o.endPhase(now)
	o.value.Web.TotalMS = webMillis(now.Sub(o.start))
	if tracked {
		o.value.Web.ClientHTTPStatus = usage.ObservedDeliveryHTTPStatus(ctx)
		o.value.DeliveryResult = "written"
		if deliveryErr != nil {
			o.value.DeliveryResult = "write_failed"
			code, message := "client_write_failed", "Could not write the Web response to the client"
			if errors.Is(deliveryErr, context.Canceled) || errors.Is(deliveryErr, context.DeadlineExceeded) {
				o.value.DeliveryResult = "cancelled"
				code, message = "client_cancelled", "The Web response was cancelled by the client"
			}
			if o.value.Error == nil || o.value.Error.Code == "web_request_cancelled" {
				o.setFailure("delivery", 499, code)
				err = InputFailure(499, code, "", message)
			}
		}
	}
	if estimated {
		o.value.UsageResult = "estimated"
	}
	if o.value.Error != nil {
		o.logFailure(ctx)
	}
	return &o.value, err
}

func (o *ExecutionObservation) logFailure(ctx context.Context) {
	w, f := o.value.Web, o.value.Error
	fields := log.Fields{
		"request_id": logging.GetRequestID(ctx), "trace_id": o.trace, "provider": Provider,
		"model": o.model, "auth_index": o.authIndex, "stream": o.stream, "stage": f.Stage, "reason": f.Code,
		"status": f.HTTPStatus, "upstream_result": o.value.UpstreamResult, "delivery_result": o.value.DeliveryResult,
		"tail_verification": o.value.TailVerification, "submitted": w.Submitted, "output_started": w.OutputStarted,
		"contents": w.Contents, "attachments": w.Attachments, "attachment_bytes": w.AttachmentBytes,
		"total_ms": int64(w.TotalMS),
	}
	if w.UpstreamHTTPStatus != 0 {
		fields["upstream_http_status"] = w.UpstreamHTTPStatus
	}
	if w.ClientHTTPStatus != 0 {
		fields["client_http_status"] = w.ClientHTTPStatus
	}
	for key, value := range map[string]string{"answer_state": w.AnswerState, "google_status": w.GoogleStatus, "google_reason": w.GoogleReason} {
		if value != "" {
			fields[key] = value
		}
	}
	for stage, ms := range w.StagesMS {
		fields[stage+"_ms"] = int64(ms)
	}
	for key, value := range map[string]*float64{"first_byte_ms": o.value.UpstreamTTFBMS, "first_summary_ms": o.value.FirstSummaryMS, "first_answer_ms": o.value.FirstAnswerMS} {
		if value != nil {
			fields[key] = int64(*value)
		}
	}
	log.WithFields(fields).Warn("Web request failed")
}
