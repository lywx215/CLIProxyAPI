package helps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type ObservedGeminiStreamOptions struct {
	Finalize       func() ([][]byte, error)
	OnEvent        func([]byte)
	Translate      func([]byte) ([][]byte, error)
	BeforeTerminal func()
	Delivered      func([]byte)
	Finished       func(error, bool)
	DecodeError    func([]byte) error
	WrapError      func(error) error
	Native         bool
}

// ForwardObservedGeminiStream is the single owner of stream usage settlement.
// HTTP delivery is acknowledged independently after the output channel closes.
func ForwardObservedGeminiStream(ctx context.Context, response *http.Response, reporter *UsageReporter, opts ObservedGeminiStreamOptions) *coreexec.StreamResult {
	out := make(chan coreexec.StreamChunk)
	stopSupport := usage.SupportObservedStreamDelivery(ctx)
	go func() {
		obs := reporter.geminiObservation
		var streamErr error
		var emitted, bypassDelivery bool
		var result GeminiReadResult
		var closeOnce, publishOnce sync.Once
		closeBody := func() { closeOnce.Do(func() { _ = response.Body.Close() }) }
		stopClose := context.AfterFunc(ctx, closeBody)
		defer stopClose()
		publish := func(deliveryErr error, tracked bool) {
			publishOnce.Do(func() {
				if deliveryErr != nil && (streamErr == nil || errors.Is(streamErr, context.Canceled)) {
					streamErr = deliveryErr
				}
				if errors.Is(streamErr, context.Canceled) && obs.Ready() && (!tracked || deliveryErr == nil) {
					streamErr = nil
				}
				if !tracked && ctx.Err() != nil && !obs.HasFinish() {
					deliveryErr = ctx.Err()
					tracked = true
				}
				obs.Delivery(deliveryErr, tracked)
				_, detail := obs.Snapshot()
				if streamErr != nil {
					reporter.PublishFailureWithDetail(ctx, detail, streamErr)
				} else {
					reporter.Publish(ctx, detail)
				}
			})
		}
		finishDiagnostics := func() {
			if opts.Finished != nil {
				opts.Finished(streamErr, result.EOF)
				opts.Finished = nil
			}
		}
		defer func() {
			finishDiagnostics()
			if !bypassDelivery && usage.StreamDeliveryTracked(ctx) {
				close(out)
				e, tracked := usage.WaitStreamDelivery(ctx)
				publish(e, tracked)
			} else {
				publish(nil, false)
				close(out)
			}
			stopSupport()
			closeBody()
		}()
		var failureFrames [][]byte
		send := func(chunks [][]byte) error {
			for _, chunk := range chunks {
				if len(chunk) == 0 {
					continue
				}
				select {
				case out <- coreexec.StreamChunk{Payload: chunk}:
					emitted = true
					if opts.Delivered != nil {
						opts.Delivered(chunk)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		emit := func(raw []byte) error {
			chunks, e := opts.Translate(raw)
			if e != nil {
				failureFrames = append(failureFrames, chunks...)
				return obs.LocalFailure("response_translation_failed", e)
			}
			return send(chunks)
		}
		var terminalFrames [][]byte
		terminalBytes := 0
		contentType := response.Header.Get("Content-Type")
		if obs.ordinary {
			contentType = "text/event-stream"
		}
		result, streamErr = ReadGeminiEvents(response.Body, contentType, obs.now, func(raw []byte, at time.Time) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if opts.OnEvent != nil {
				opts.OnEvent(raw)
			}
			if geminiBusinessRoot(raw).Get("error").Exists() && opts.DecodeError != nil {
				obs.ObserveErrorFrame(raw)
				return opts.DecodeError(raw)
			}
			if e := obs.Observe(raw, at); e != nil {
				return e
			}
			reporter.ObserveResponseModel(raw)
			raw = obs.TranslationPayload(raw)
			if obs.HasFinish() {
				terminalFrames = append(terminalFrames, bytes.Clone(raw))
				terminalBytes += len(raw)
				if terminalBytes > StreamScannerBuffer {
					return fmt.Errorf("Gemini terminal buffer exceeds size limit")
				}
				return nil
			}
			return emit(raw)
		}, obs.Ready)
		streamErr = obs.Finish(result.EOF, result.Residual, response.Trailer, streamErr)
		finalize := func() {
			if opts.Finalize != nil && ctx.Err() == nil {
				frames, e := opts.Finalize()
				failureFrames = append(failureFrames, frames...)
				if e != nil {
					streamErr = obs.LocalFailure("response_translation_failed", streamValidationError{e, streamErr})
				}
				opts.Finalize = nil
			}
		}
		if streamErr != nil {
			finalize()
		}
		if streamErr == nil {
			var prepared [][]byte
			v, _ := obs.Snapshot()
			if !opts.Native && v.UpstreamResult == "continuation_required" {
				streamErr = obs.LocalFailure("unsupported_upstream_finish_reason", fmt.Errorf("unsupported_upstream_finish_reason: CONTINUATION"))
			}
			for _, raw := range terminalFrames {
				if streamErr != nil {
					break
				}
				chunks, e := opts.Translate(raw)
				if e != nil {
					failureFrames = append(failureFrames, chunks...)
					streamErr = obs.LocalFailure("response_translation_failed", e)
					break
				}
				prepared = append(prepared, chunks...)
			}
			finalize()
			if streamErr == nil {
				chunks, e := opts.Translate([]byte("[DONE]"))
				if e != nil {
					failureFrames = append(failureFrames, chunks...)
					streamErr = obs.LocalFailure("response_translation_failed", e)
				} else {
					prepared = append(prepared, chunks...)
				}
			}
			if streamErr == nil {
				if opts.BeforeTerminal != nil && !result.Residual {
					opts.BeforeTerminal()
				}
				streamErr = send(prepared)
			}
		}
		if streamErr != nil {
			if ctx.Err() != nil {
				return
			}
			streamErr = obs.Finish(result.EOF, result.Residual, response.Trailer, streamErr)
			if emitted && !coreexec.ReplayForbidden(streamErr) {
				streamErr = obs.ForbidReplay(streamErr)
			}
			if opts.WrapError != nil {
				streamErr = opts.WrapError(streamErr)
			}
			finishDiagnostics()
			if !emitted && !coreexec.ReplayForbidden(streamErr) {
				// A pre-output rejection may be retried. Its bytes cannot share
				// the later attempt's downstream acknowledgment.
				bypassDelivery = true
				publish(nil, false)
				stopSupport()
			} else if !usage.StreamDeliveryTracked(ctx) {
				publish(nil, false)
			}
			if len(failureFrames) > 0 {
				if e := send(failureFrames); e != nil {
					return
				}
			}
			select {
			case out <- coreexec.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
		}
	}()
	return &coreexec.StreamResult{Headers: response.Header.Clone(), Chunks: out}
}

type streamValidationError struct{ display, cause error }

func (e streamValidationError) Error() string { return e.display.Error() }
func (e streamValidationError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.display}
	}
	return []error{e.display, e.cause}
}
func (e streamValidationError) StatusCode() int { return 502 }

// ObserveGeminiNonStream validates the raw JSON before client-facing translation.
func (r *UsageReporter) ObserveGeminiNonStream(raw []byte, trailer http.Header) error {
	if r == nil || r.geminiObservation == nil {
		return nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("data:")) {
		o := r.geminiObservation
		result, err := ReadGeminiEvents(bytes.NewReader(raw), "text/event-stream", o.now, o.Observe, nil)
		return o.Finish(result.EOF, result.Residual, trailer, err)
	}
	if e := r.geminiObservation.Observe(raw, r.geminiObservation.now()); e != nil {
		return e
	}
	return r.geminiObservation.Finish(true, false, trailer, nil)
}

// ReadObservedGeminiAggregate applies the same integrity rules to an internal
// SSE consumer, without waiting for the outer non-streaming HTTP delivery.
func ReadObservedGeminiAggregate(ctx context.Context, response *http.Response, reporter *UsageReporter, onEvent func([]byte)) ([]byte, GeminiReadResult, error) {
	o := reporter.geminiObservation
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { _ = response.Body.Close() }) }
	stop := context.AfterFunc(ctx, closeBody)
	defer stop()
	defer closeBody()
	var output bytes.Buffer
	contentType := response.Header.Get("Content-Type")
	if o.ordinary {
		contentType = "text/event-stream"
	}
	result, err := ReadGeminiEvents(response.Body, contentType, o.now, func(raw []byte, at time.Time) error {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
		if onEvent != nil {
			onEvent(raw)
		}
		if e := o.Observe(raw, at); e != nil {
			return e
		}
		reporter.ObserveResponseModel(raw)
		output.Write(raw)
		output.WriteByte('\n')
		return nil
	}, o.Ready)
	if err = o.Finish(result.EOF, result.Residual, response.Trailer, err); err != nil {
		return nil, result, err
	}
	return output.Bytes(), result, nil
}
