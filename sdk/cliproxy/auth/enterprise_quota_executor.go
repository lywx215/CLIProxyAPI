package auth

import (
	"context"
	"errors"
	"io"
	"net/http"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The final execution boundary gates every selected credential, including plugin
// selections and stale scheduler snapshots. It never mutates request payloads.
type enterpriseQuotaExecutor struct {
	ProviderExecutor
	gate *enterpriseQuotaGate
}

type enterpriseLocalGateError struct{ error }

type enterpriseSharedRateLimitError struct{ error }

func (e *enterpriseSharedRateLimitError) Unwrap() error { return e.error }
func (*enterpriseSharedRateLimitError) StatusCode() int { return http.StatusTooManyRequests }

func (t *enterpriseQuotaTicket) resultError(err error) error {
	if t.sharedRateLimit {
		if upstream := enterpriseGenerationError(err); upstream != nil && upstream.HTTPStatus == 429 {
			return &enterpriseSharedRateLimitError{err}
		}
	}
	return err
}

var errUnrelatedQuotaProbe = errors.New("gemini-enterprise: probe used a different quota feature")

func (t *enterpriseQuotaTicket) verifyProbe(ctx context.Context, model string, hasPayload bool) error {
	if len(t.probes) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !hasPayload {
		return &ge.TransportError{Cause: io.ErrUnexpectedEOF}
	}
	if !t.paidRecovery {
		return nil
	}
	if state := enterpriseAttempts(ctx); state != nil {
		state.mu.Lock()
		if state.generationModel != "" {
			model = state.generationModel
		}
		state.mu.Unlock()
	}
	if !enterpriseQuotaTextModel(model) {
		return errUnrelatedQuotaProbe
	}
	return nil
}

func (e *enterpriseQuotaExecutor) validateModel(ctx context.Context, a *Auth, r exec.Request) error {
	final := true
	if validator, ok := e.ProviderExecutor.(interface{ ValidatesEnterpriseModelPayload() bool }); ok && validator.ValidatesEnterpriseModelPayload() {
		// Built-in payload rules can change the model. Only the final read-only
		// check knows the actual business model; identity is still checked here.
		final = false
	}
	return validateEnterpriseModelRequest(ctx, a, r.Model, final)
}

func (e *enterpriseLocalGateError) Unwrap() error { return e.error }
func (*enterpriseLocalGateError) StatusCode() int { return 429 }

func (e *enterpriseQuotaExecutor) Execute(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (response exec.Response, err error) {
	defer budget.ReleaseAdmissionTurn(ctx)
	if err = e.validateModel(ctx, a, r); err != nil {
		return response, err
	}
	defer func() {
		observeEnterpriseModelAttempt(ctx, a, r.Model, err)
		err = stopEnterpriseProbeRetry(ctx, err)
	}()
	ticket, err := a.enterpriseQuota.beginForModel(r.Model)
	budget.ReleaseAdmissionTurn(ctx)
	if err != nil {
		return response, err
	}
	ticket.request = enterpriseQuotaRequestFromContext(ctx)
	defer func() {
		probeErr := err
		if probeErr == nil {
			probeErr = ticket.verifyProbe(ctx, r.Model, len(response.Payload) > 0)
			if probeErr != errUnrelatedQuotaProbe {
				err = probeErr
			}
		}
		ticket.finish(probeErr)
		err = ticket.resultError(err)
	}()
	return e.ProviderExecutor.Execute(ctx, a, r, o)
}
func (e *enterpriseQuotaExecutor) ExecuteStream(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (*exec.StreamResult, error) {
	defer budget.ReleaseAdmissionTurn(ctx)
	if err := e.validateModel(ctx, a, r); err != nil {
		return nil, err
	}
	ticket, err := a.enterpriseQuota.beginForModel(r.Model)
	budget.ReleaseAdmissionTurn(ctx)
	if err != nil {
		return nil, err
	}
	ticket.request = enterpriseQuotaRequestFromContext(ctx)
	source, err := e.ProviderExecutor.ExecuteStream(ctx, a, r, o)
	if err != nil {
		ticket.finish(err)
		observeEnterpriseModelAttempt(ctx, a, r.Model, err)
		return source, stopEnterpriseProbeRetry(ctx, ticket.resultError(err))
	}
	if source == nil || source.Chunks == nil {
		if err := ticket.verifyProbe(ctx, r.Model, false); err != nil {
			ticket.finish(err)
			return nil, err
		}
		ticket.finish(context.Canceled)
		return source, nil
	}
	out := make(chan exec.StreamChunk)
	go func() {
		defer close(out)
		var finalErr error
		hadPayload := false
		var cancelled bool
		defer func() {
			if !cancelled {
				ticket.finish(finalErr)
				return
			}
			// Close the caller's stream promptly. The upstream request remains
			// in flight until its producer finishes reacting to cancellation.
			ticket.finishQuota(finalErr)
			ticket.finishBalance(finalErr)
			go func() {
				for range source.Chunks {
				}
				ticket.finishRotation()
			}()
		}()
		for {
			select {
			case <-ctx.Done():
				finalErr = ctx.Err()
				cancelled = true
				return
			case chunk, ok := <-source.Chunks:
				if !ok {
					if finalErr == nil {
						finalErr = ticket.verifyProbe(ctx, r.Model, hadPayload)
						if errors.Is(finalErr, io.ErrUnexpectedEOF) {
							ticket.finishQuota(finalErr)
							select {
							case out <- exec.StreamChunk{Err: finalErr}:
							case <-ctx.Done():
							}
						}
					}
					return
				}
				hadPayload = hadPayload || len(chunk.Payload) > 0
				if chunk.Err != nil {
					finalErr = chunk.Err
					observeEnterpriseModelAttempt(ctx, a, r.Model, finalErr)
					ticket.finishQuota(finalErr)
					chunk.Err = stopEnterpriseProbeRetry(ctx, ticket.resultError(finalErr))
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					finalErr = ctx.Err()
					cancelled = true
					return
				}
			}
		}
	}()
	return &exec.StreamResult{Chunks: out, Headers: source.Headers}, nil
}
func (e *enterpriseQuotaExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	finish, err := a.enterpriseQuota.beginRotationRequest()
	if err != nil {
		return nil, err
	}
	response, err := e.ProviderExecutor.HttpRequest(ctx, a, r)
	if err != nil || response == nil || response.Body == nil {
		finish()
		return response, err
	}
	response.Body = &enterpriseCountedBody{ReadCloser: response.Body, finish: finish}
	return response, nil
}

type enterpriseCountedBody struct {
	io.ReadCloser
	finish func()
}

func (body *enterpriseCountedBody) Read(buffer []byte) (int, error) {
	size, err := body.ReadCloser.Read(buffer)
	if err != nil {
		body.finish()
	}
	return size, err
}

func (body *enterpriseCountedBody) Close() error {
	err := body.ReadCloser.Close()
	body.finish()
	return err
}
