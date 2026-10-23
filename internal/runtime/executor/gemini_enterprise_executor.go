package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// GeminiEnterpriseExecutor executes text requests using each account's saved binding.
type GeminiEnterpriseExecutor struct{ cfg *config.Config }

func NewGeminiEnterpriseExecutor(cfg *config.Config) *GeminiEnterpriseExecutor {
	return &GeminiEnterpriseExecutor{cfg: cfg}
}
func (*GeminiEnterpriseExecutor) Identifier() string { return ge.Provider }
func (*GeminiEnterpriseExecutor) RequestToFormat(coreexec.Request, coreexec.Options) translator.Format {
	return translator.FormatGeminiEnterprise
}

func (e *GeminiEnterpriseExecutor) validate(opts coreexec.Options) error {
	if e.cfg != nil && e.cfg.Home.Enabled {
		return enterprise.Invalid("Gemini Enterprise is not supported in CLIProxyAPIHome mode")
	}
	if opts.Alt == "responses/compact" || !coreexec.GeminiEnterpriseOperationAllowed(opts.SourceFormat.String(), coreexec.ResponseFormatOrSource(opts).String(), coreexec.Operation(opts)) {
		return enterprise.Invalid("operation_not_supported: explicit native Chat Completions or Gemini Generate Content operation is required")
	}
	return nil
}

func (e *GeminiEnterpriseExecutor) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if e.cfg != nil && e.cfg.Home.Enabled {
		return enterprise.Invalid("Gemini Enterprise is not supported in CLIProxyAPIHome mode")
	}
	if !ge.Official(req) {
		return enterprise.Invalid("Gemini Enterprise credentials may only be sent to the official HTTPS discoveryengine.googleapis.com origin")
	}
	if auth == nil || auth.Provider != ge.Provider {
		return enterprise.Invalid("Gemini Enterprise authentication is required")
	}
	binding := ge.FromMetadata(auth.Metadata)
	if err := binding.Validate(); err != nil {
		return err
	}
	token := ge.String(auth.Metadata, "access_token")
	if token == "" {
		return statusErr{code: 401, msg: "gemini-enterprise: missing access token; authorize with --gemini-enterprise-login"}
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Goog-User-Project", binding.ProjectID)
	return nil
}

func (e *GeminiEnterpriseExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, enterprise.Invalid("request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	req = req.Clone(ctx)
	if err := e.PrepareRequest(req, auth); err != nil {
		return nil, err
	}
	client := newHTTPClient(ctx, e.cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(req)
}

func (e *GeminiEnterpriseExecutor) open(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options, reporter *helps.UsageReporter) (*http.Response, enterprise.Input, error) {
	if err := e.validate(opts); err != nil {
		return nil, enterprise.Input{}, err
	}
	input, err := enterprise.Normalize(req.Payload, opts.SourceFormat.String(), req.Model)
	if err != nil {
		return nil, input, err
	}
	if len(input.Ignored) > 0 {
		log.WithField("fields", input.Ignored).Debug("gemini-enterprise ignored optional parameters")
	}
	finalize := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), req.Model, translator.FormatGeminiEnterprise.String(), "", input.Body, req, opts)
	body := finalize(input.Body)
	if err := enterprise.ValidateFinal(body); err != nil {
		return nil, input, err
	}
	if auth == nil {
		return nil, input, enterprise.Invalid("missing Gemini Enterprise authentication")
	}
	binding := ge.FromMetadata(auth.Metadata)
	if err := binding.Validate(); err != nil {
		return nil, input, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ge.Origin+"/v1alpha/"+ge.Engine(binding)+"/assistants/"+binding.AssistantID+":streamAssist", bytes.NewReader(body))
	if err != nil {
		return nil, input, enterprise.Invalid("cannot construct streamAssist request")
	}
	request.Header.Set("Content-Type", "application/json")
	if err := e.PrepareRequest(request, auth); err != nil {
		return nil, input, err
	}
	// Never record bearer credentials or raw upstream assistToken/sessionInfo in request logs.
	headers := request.Header.Clone()
	headers.Del("Authorization")
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: request.URL.String(), Method: request.Method, Headers: headers, Body: body, Provider: e.Identifier(), AuthID: auth.ID, AuthLabel: auth.Label})
	client := reporter.TrackHTTPClientRoundTripOnly(newHTTPClient(ctx, e.cfg, auth, 0))
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, input, ctx.Err()
		}
		return nil, input, enterprise.Failure("upstream_unavailable", "cannot reach the Google Enterprise endpoint", false)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, nil)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, input, statusErr{code: response.StatusCode, msg: fmt.Sprintf("gemini-enterprise: Google returned HTTP %d", response.StatusCode)}
	}
	return response, input, nil
}

func (e *GeminiEnterpriseExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (result coreexec.Response, err error) {
	if err = e.validate(opts); err != nil {
		return result, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	response, input, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		return result, err
	}
	defer func() { _ = response.Body.Close() }()
	output := enterprise.NewOutput()
	parser := &enterprise.Parser{}
	err = parser.Parse(response.Body, func(event enterprise.Event) error {
		reporter.ObserveTokenEvent(event.Type == "text" || event.Type == "code" || event.Type == "execution")
		_, _, errAdd := output.Add(event)
		return errAdd
	})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, err
	}
	if err = output.Finish(input); err != nil {
		return result, err
	}
	if opts.SourceFormat == translator.FormatGemini {
		result.Payload = output.Gemini(true, nil)
	} else {
		result.Payload = output.Chat(req.Model)
	}
	result.Headers = helps.EnterpriseResponseHeaders(input.Ignored)
	helps.AppendAPIResponseChunk(ctx, e.cfg, result.Payload)
	reporter.EnsurePublished(ctx)
	return result, nil
}

func (e *GeminiEnterpriseExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (result *coreexec.StreamResult, err error) {
	if err = e.validate(opts); err != nil {
		return nil, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	reporter.SetStream(true)
	response, input, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		reporter.TrackFailure(ctx, &err)
		return nil, err
	}
	chunks := make(chan coreexec.StreamChunk)
	go func() {
		defer close(chunks)
		defer func() { _ = response.Body.Close() }()
		var streamErr error
		defer reporter.TrackFailure(ctx, &streamErr)
		send := func(payload []byte) error {
			select {
			case chunks <- coreexec.StreamChunk{Payload: payload}:
				helps.AppendAPIResponseChunk(ctx, e.cfg, payload)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		output := enterprise.NewOutput()
		parser := &enterprise.Parser{}
		streamErr = parser.Parse(response.Body, func(event enterprise.Event) error {
			reporter.ObserveTokenEvent(event.Type == "text" || event.Type == "code" || event.Type == "execution")
			previousAnnotations := len(output.Annotations)
			text, part, errAdd := output.Add(event)
			if errAdd != nil {
				return errAdd
			}
			if input.JSON || event.Type == "done" {
				return nil
			}
			if opts.SourceFormat == translator.FormatGemini {
				if part == nil {
					return nil
				}
				return send(output.Gemini(false, []map[string]any{part}))
			}
			if text == "" && len(output.Annotations) == previousAnnotations {
				return nil
			}
			return send(output.ChatDelta(req.Model, text, output.Annotations[previousAnnotations:], false))
		})
		if ctx.Err() != nil {
			streamErr = ctx.Err()
		}
		if streamErr == nil {
			streamErr = output.Finish(input)
		}
		if streamErr != nil {
			select {
			case chunks <- coreexec.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
			return
		}
		if opts.SourceFormat == translator.FormatGemini {
			parts := []map[string]any{}
			if input.JSON {
				parts = output.Parts
			}
			streamErr = send(output.Gemini(true, parts))
		} else {
			if input.JSON {
				streamErr = send(output.ChatDelta(req.Model, output.Text, nil, false))
			}
			if streamErr == nil {
				streamErr = send(output.ChatDelta(req.Model, "", nil, true))
			}
		}
		if streamErr == nil {
			reporter.EnsurePublished(ctx)
		}
	}()
	return &coreexec.StreamResult{Chunks: chunks, Headers: helps.EnterpriseResponseHeaders(input.Ignored)}, nil
}

func (e *GeminiEnterpriseExecutor) CountTokens(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, enterprise.Invalid("operation_not_supported: Gemini Enterprise does not support countTokens")
}

func (e *GeminiEnterpriseExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if e.cfg != nil && e.cfg.Home.Enabled {
		return nil, enterprise.Invalid("Gemini Enterprise local refresh is disabled in CLIProxyAPIHome mode")
	}
	if auth == nil || auth.Provider != ge.Provider {
		return nil, enterprise.Invalid("invalid Gemini Enterprise authentication")
	}
	proxy := auth.ProxyURL
	if proxy == "" && e.cfg != nil {
		proxy = e.cfg.ProxyURL
	}
	token, err := ge.Refresh(ctx, ge.FromMetadata(auth.Metadata), ge.String(auth.Metadata, "refresh_token"), proxy)
	if err != nil {
		if strings.Contains(err.Error(), "invalid_grant") {
			return nil, coreauth.NewTerminalAuthError(&coreauth.Error{Code: "invalid_grant", Message: "Gemini Enterprise requires new Google authorization", HTTPStatus: 401}, err)
		}
		return nil, err
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = map[string]any{}
	}
	ge.PutToken(updated.Metadata, token)
	updated.LastRefreshedAt = time.Now().UTC()
	updated.UpdatedAt = updated.LastRefreshedAt
	return updated, nil
}
