package auth

import (
	"context"
	"io"
	"net/http"

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
	ticket, err := a.enterpriseQuota.begin()
	budget.ReleaseAdmissionTurn(ctx)
	if err != nil {
		return response, err
	}
	defer func() { ticket.finish(err) }()
	return e.ProviderExecutor.Execute(ctx, a, r, o)
}
func (e *enterpriseQuotaExecutor) ExecuteStream(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (*exec.StreamResult, error) {
	defer budget.ReleaseAdmissionTurn(ctx)
	if err := e.validateModel(ctx, a, r); err != nil {
		return nil, err
	}
	ticket, err := a.enterpriseQuota.begin()
	budget.ReleaseAdmissionTurn(ctx)
	if err != nil {
		return nil, err
	}
	source, err := e.ProviderExecutor.ExecuteStream(ctx, a, r, o)
	if err != nil {
		ticket.finish(err)
		observeEnterpriseModelAttempt(ctx, a, r.Model, err)
		return source, stopEnterpriseProbeRetry(ctx, err)
	}
	if source == nil || source.Chunks == nil {
		ticket.finish(context.Canceled)
		return source, nil
	}
	out := make(chan exec.StreamChunk)
	go func() {
		defer close(out)
		var finalErr error
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
					return
				}
				if chunk.Err != nil {
					finalErr = chunk.Err
					observeEnterpriseModelAttempt(ctx, a, r.Model, finalErr)
					chunk.Err = stopEnterpriseProbeRetry(ctx, finalErr)
					ticket.finishQuota(finalErr)
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
