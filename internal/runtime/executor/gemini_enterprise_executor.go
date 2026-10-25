package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// GeminiEnterpriseExecutor executes text and media tools using each account's saved binding.
type GeminiEnterpriseExecutor struct{ cfg *config.Config }

func NewGeminiEnterpriseExecutor(cfg *config.Config) *GeminiEnterpriseExecutor {
	return &GeminiEnterpriseExecutor{cfg: cfg}
}

func enterpriseRequestMediaKind(model string) string {
	switch model {
	case "gemini-3-pro-image":
		return "image"
	case "veo-3.1":
		return "video"
	}
	return registry.EnterpriseMediaKind(model)
}
func (*GeminiEnterpriseExecutor) Identifier() string { return ge.Provider }
func (*GeminiEnterpriseExecutor) RequestToFormat(coreexec.Request, coreexec.Options) translator.Format {
	return translator.FormatGeminiEnterprise
}

func (e *GeminiEnterpriseExecutor) ShouldPrepareRequestAuth(auth *coreauth.Auth) bool {
	if auth == nil || auth.Provider != ge.Provider || (e.cfg != nil && e.cfg.Home.Enabled) {
		return false
	}
	i := auth.EnterpriseQuotaIdentity
	return i == nil || i.Binding != ge.Binding(ge.FromMetadata(auth.Metadata)) || time.Since(i.CheckedAt) >= time.Hour
}

func (e *GeminiEnterpriseExecutor) PrepareRequestAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	updated := auth.Clone()
	client := newHTTPClient(ctx, e.cfg, auth, 0)
	identity := ge.ResolveQuotaIdentity(ctx, client, ge.FromMetadata(auth.Metadata), ge.String(auth.Metadata, "access_token"), time.Now())
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !identity.Verified && !identity.Invalidated && auth.EnterpriseQuotaIdentity != nil && auth.EnterpriseQuotaIdentity.Verified && auth.EnterpriseQuotaIdentity.Binding == identity.Binding {
		copyIdentity := *auth.EnterpriseQuotaIdentity
		copyIdentity.CheckedAt = identity.CheckedAt
		identity = &copyIdentity
	}
	updated.EnterpriseQuotaIdentity = identity
	return updated, nil
}

func (e *GeminiEnterpriseExecutor) validate(opts coreexec.Options) error {
	if e.cfg != nil && e.cfg.Home.Enabled {
		return enterprise.Invalid("Gemini Enterprise is not supported in CLIProxyAPIHome mode")
	}
	if opts.Alt == "responses/compact" || !coreexec.GeminiEnterpriseOperationAllowed(opts.SourceFormat.String(), coreexec.ResponseFormatOrSource(opts).String(), coreexec.Operation(opts)) {
		return enterprise.Invalid("operation_not_supported: an explicit native content or media generation operation is required")
	}
	return nil
}

func (e *GeminiEnterpriseExecutor) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if e.cfg != nil && e.cfg.Home.Enabled {
		return enterprise.Invalid("Gemini Enterprise is not supported in CLIProxyAPIHome mode")
	}
	if auth == nil || auth.Provider != ge.Provider {
		return enterprise.Invalid("Gemini Enterprise authentication is required")
	}
	binding := ge.FromMetadata(auth.Metadata)
	if err := binding.Validate(); err != nil {
		return err
	}
	if !ge.OfficialBinding(req, binding.ProjectID, binding.Location) {
		return enterprise.Invalid("Gemini Enterprise credentials require the official HTTPS endpoint matching the saved project and region")
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
	kind := enterpriseRequestMediaKind(req.Model)
	if kind == "image" && e.cfg != nil && (e.cfg.DisableImageGeneration == config.DisableImageGenerationAll || e.cfg.DisableImageGeneration == config.DisableImageGenerationChat && opts.SourceFormat.String() != "openai-image") {
		return nil, enterprise.Input{}, enterprise.Invalid("image generation is disabled for this endpoint")
	}
	var input enterprise.Input
	var err error
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if kind != "" {
		input, err = enterprise.NormalizeMedia(req.Payload, opts.SourceFormat.String(), req.Model, kind)
	} else {
		input, err = enterprise.Normalize(req.Payload, opts.SourceFormat.String(), baseModel)
	}
	if err != nil {
		if fields := helps.EnterpriseValidationFields("input", err); fields != nil {
			log.WithFields(fields).Warn("gemini-enterprise request validation failed")
		}
		return nil, input, err
	}
	if len(input.Ignored) > 0 {
		log.WithField("fields", input.Ignored).Debug("gemini-enterprise ignored optional parameters")
	}
	if input.ClientFunctionsIgnored {
		log.WithField("provider", e.Identifier()).Warn("client function fields ignored; continuing as text without client function execution")
	}
	finalize := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), req.Model, translator.FormatGeminiEnterprise.String(), "", input.Body, req, opts)
	if kind == "" {
		input.Body, err = helps.ApplyEnterpriseThinking(input.Body, req, opts)
		if err != nil {
			return nil, input, err
		}
	}
	body := finalize(input.Body)
	validate := enterprise.ValidateFinal
	if kind != "" {
		validate = func(b []byte) error { return enterprise.ValidateMediaFinal(b, kind) }
	}
	if err := validate(body); err != nil {
		if fields := helps.EnterpriseValidationFields("final_payload", err); fields != nil {
			log.WithFields(fields).Warn("gemini-enterprise request validation failed")
		}
		return nil, input, err
	}
	input.Body = body
	input.IncludeThoughts = kind == "" && gjson.GetBytes(body, "generationSpec.thinkingConfig.includeThoughts").Type == gjson.True
	if auth == nil {
		return nil, input, enterprise.Invalid("missing Gemini Enterprise authentication")
	}
	binding := ge.FromMetadata(auth.Metadata)
	if err := binding.Validate(); err != nil {
		return nil, input, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ge.OriginForLocation(binding.Location)+"/v1alpha/"+ge.Engine(binding)+"/assistants/"+binding.AssistantID+":streamAssist", bytes.NewReader(body))
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
		return nil, input, &ge.TransportError{Cause: err, Started: kind != ""}
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, nil)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, errRead := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
		_ = response.Body.Close()
		if ctx.Err() != nil {
			return nil, input, ctx.Err()
		}
		if errRead != nil {
			return nil, input, &ge.TransportError{Cause: errRead}
		}
		if len(raw) > 64*1024 {
			raw = nil
		}
		return nil, input, helps.EnterpriseGenerationError(raw, response.StatusCode, response.Header, auth, false)
	}
	return response, input, nil
}

func (e *GeminiEnterpriseExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (result coreexec.Response, err error) {
	if err = e.validate(opts); err != nil {
		return result, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	reporter.SetStream(opts.Stream)
	defer func() {
		if err != nil {
			reporter.PublishFailureWithDetail(ctx, usage.Detail{TokenPresence: &usage.TokenPresence{}}, err)
		}
	}()
	response, input, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		var started *ge.TransportError
		if input.MediaKind != "" && errors.As(err, &started) && started.Started {
			err = coreexec.MarkGenerationStarted(err)
		}
		return result, err
	}
	if input.MediaKind != "" {
		defer func() { err = coreexec.MarkGenerationStarted(err) }()
	}
	defer func() { _ = response.Body.Close() }()
	counter := helps.NewEnterpriseTokenCounter(input.Body)
	output := enterprise.NewOutput()
	refs := helps.EnterpriseMediaRefs{}
	var finalDetail *usage.Detail
	defer func() {
		var detail usage.Detail
		if finalDetail != nil {
			detail = *finalDetail
		} else {
			detail = counter.Detail(output.Parts)
		}
		if err != nil {
			reporter.PublishFailureWithDetail(ctx, detail, err)
		} else {
			reporter.Publish(ctx, detail)
		}
	}()
	parser := &enterprise.Parser{IncludeThoughts: input.IncludeThoughts, MediaKind: input.MediaKind, Started: input.MediaKind != "", ReadError: func(err error, started bool) error { return &ge.TransportError{Cause: err, Started: started} }, Error: func(raw []byte, started bool) error {
		return helps.EnterpriseGenerationError(raw, 200, response.Header, auth, started)
	}}
	err = parser.Parse(response.Body, func(event enterprise.Event) error {
		if errRef := refs.Add(event); errRef != nil {
			return errRef
		}
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
	detail := counter.Detail(output.Parts)
	finalDetail = &detail
	if input.MediaKind != "" {
		binding := ge.FromMetadata(auth.Metadata)
		data, errDownload := helps.DownloadEnterpriseMedia(ctx, newHTTPClient(ctx, e.cfg, auth, 0), func(r *http.Request) error { return e.PrepareRequest(r, auth) }, ge.Engine(binding), input.MediaKind, refs)
		if errDownload != nil {
			return result, errDownload
		}
		result.Payload = output.MediaResponse(req.Model, opts.SourceFormat.String(), input.MediaKind, refs.MIME, data)
		if opts.SourceFormat.String() == "openai" || opts.SourceFormat == translator.FormatGemini {
			result.Payload = helps.EnterpriseUsagePayload(result.Payload, opts.SourceFormat == translator.FormatGemini, detail)
		}
		result.Headers = helps.EnterpriseResponseHeaders(nil)
		return result, nil
	}
	if err = output.Finish(input); err != nil {
		return result, err
	}
	if opts.SourceFormat == translator.FormatGemini {
		result.Payload = output.Gemini(true, nil)
	} else {
		result.Payload = output.Chat(req.Model)
	}
	result.Payload = helps.EnterpriseUsagePayload(result.Payload, opts.SourceFormat == translator.FormatGemini, detail)
	result.Headers = helps.EnterpriseResponseHeaders(input.Ignored)
	helps.AppendAPIResponseChunk(ctx, e.cfg, result.Payload)
	return result, nil
}

func (e *GeminiEnterpriseExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (result *coreexec.StreamResult, err error) {
	if err = e.validate(opts); err != nil {
		return nil, err
	}
	if enterpriseRequestMediaKind(req.Model) != "" {
		response, errMedia := e.Execute(ctx, auth, req, opts)
		if errMedia != nil {
			return nil, errMedia
		}
		chunks := make(chan coreexec.StreamChunk, 3)
		if opts.SourceFormat == translator.FormatGemini {
			chunks <- coreexec.StreamChunk{Payload: response.Payload}
		} else {
			media, terminal := enterprise.MediaChatChunks(response.Payload)
			chunks <- coreexec.StreamChunk{Payload: media}
			chunks <- coreexec.StreamChunk{Payload: terminal}
			original := req.Payload
			if len(opts.OriginalRequest) > 0 {
				original = opts.OriginalRequest
			}
			if helps.EnterpriseStreamUsageRequested(original) {
				chunks <- coreexec.StreamChunk{Payload: enterprise.MediaChatUsageChunk(response.Payload)}
			}
		}
		close(chunks)
		return &coreexec.StreamResult{Chunks: chunks, Headers: response.Headers}, nil
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	reporter.SetStream(true)
	response, input, err := e.open(ctx, auth, req, opts, reporter)
	if err != nil {
		reporter.PublishFailureWithDetail(ctx, usage.Detail{TokenPresence: &usage.TokenPresence{}}, err)
		return nil, err
	}
	chunks := make(chan coreexec.StreamChunk)
	counter := helps.NewEnterpriseTokenCounter(input.Body)
	includeUsage := helps.EnterpriseStreamUsageRequested(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		includeUsage = helps.EnterpriseStreamUsageRequested(opts.OriginalRequest)
	}
	go func() {
		defer close(chunks)
		defer func() { _ = response.Body.Close() }()
		var streamErr error
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
		var finalDetail *usage.Detail
		defer func() {
			var detail usage.Detail
			if finalDetail != nil {
				detail = *finalDetail
			} else {
				detail = counter.Detail(output.Parts)
			}
			if streamErr != nil {
				reporter.PublishFailureWithDetail(ctx, detail, streamErr)
			} else {
				reporter.Publish(ctx, detail)
			}
		}()
		parser := &enterprise.Parser{IncludeThoughts: input.IncludeThoughts, ReadError: func(err error, started bool) error { return &ge.TransportError{Cause: err, Started: started} }, Error: func(raw []byte, started bool) error {
			return helps.EnterpriseGenerationError(raw, 200, response.Header, auth, started)
		}}
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
				if event.Type == "thought" {
					return send(output.ChatThoughtDelta(req.Model, event.Text))
				}
				return nil
			}
			return send(output.ChatDelta(req.Model, text, output.Annotations[previousAnnotations:], false))
		})
		if ctx.Err() != nil {
			streamErr = ctx.Err()
		}
		if streamErr == nil {
			detail := counter.Detail(output.Parts)
			finalDetail = &detail
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
			streamErr = send(helps.EnterpriseUsagePayload(output.Gemini(true, parts), true, *finalDetail))
		} else {
			if input.JSON {
				if output.Reasoning != "" {
					streamErr = send(output.ChatThoughtDelta(req.Model, output.Reasoning))
				}
				if streamErr == nil {
					streamErr = send(output.ChatDelta(req.Model, output.Text, nil, false))
				}
			}
			if streamErr == nil {
				streamErr = send(output.ChatDelta(req.Model, "", nil, true))
			}
			if streamErr == nil && includeUsage && finalDetail.TokenPresence.TotalTokens {
				streamErr = send(helps.EnterpriseChatUsageChunk(output.ChatDelta(req.Model, "", nil, true), *finalDetail))
			}
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
