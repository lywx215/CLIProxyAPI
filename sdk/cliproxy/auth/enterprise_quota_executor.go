package auth

import (
	"context"
	"net/http"

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
	if err = e.validateModel(ctx, a, r); err != nil {
		return response, err
	}
	defer func() {
		observeEnterpriseModelAttempt(ctx, a, r.Model, err)
		err = stopEnterpriseProbeRetry(ctx, err)
	}()
	ticket, err := a.enterpriseQuota.begin()
	if err != nil {
		return response, err
	}
	defer func() { ticket.finish(err) }()
	return e.ProviderExecutor.Execute(ctx, a, r, o)
}
func (e *enterpriseQuotaExecutor) ExecuteStream(ctx context.Context, a *Auth, r exec.Request, o exec.Options) (*exec.StreamResult, error) {
	if err := e.validateModel(ctx, a, r); err != nil {
		return nil, err
	}
	ticket, err := a.enterpriseQuota.begin()
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
		defer func() { ticket.finish(finalErr) }()
		for {
			select {
			case <-ctx.Done():
				finalErr = ctx.Err()
				discardStreamChunks(source.Chunks)
				return
			case chunk, ok := <-source.Chunks:
				if !ok {
					return
				}
				if chunk.Err != nil {
					finalErr = chunk.Err
					observeEnterpriseModelAttempt(ctx, a, r.Model, finalErr)
					chunk.Err = stopEnterpriseProbeRetry(ctx, finalErr)
					ticket.finish(finalErr)
					ticket = nil
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					finalErr = ctx.Err()
					discardStreamChunks(source.Chunks)
					return
				}
			}
		}
	}()
	return &exec.StreamResult{Chunks: out, Headers: source.Headers}, nil
}
func (e *enterpriseQuotaExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	return e.ProviderExecutor.HttpRequest(ctx, a, r)
}
