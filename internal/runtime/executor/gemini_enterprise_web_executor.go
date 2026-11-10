package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	webconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	wire "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	webcore "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/core"
	webgemini "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

type GeminiEnterpriseWebExecutor struct {
	cfg     *config.Config
	runtime *web.Runtime
}

func NewGeminiEnterpriseWebExecutor(cfg *config.Config, r *web.Runtime) *GeminiEnterpriseWebExecutor {
	return &GeminiEnterpriseWebExecutor{cfg, r}
}
func (*GeminiEnterpriseWebExecutor) Identifier() string { return web.Provider }
func (e *GeminiEnterpriseWebExecutor) client(ctx context.Context, a *auth.Auth) *http.Client {
	return helps.NewProxyAwareHTTPClientBase(ctx, e.cfg, a, 0)
}
func (e *GeminiEnterpriseWebExecutor) cookieRoute(ctx context.Context, a *auth.Auth) (string, bool) {
	requestProxy := exec.RequestProxyURL(ctx)
	return a.ProxyURL + ":" + e.cfg.ProxyURL + ":" + requestProxy, requestProxy == ""
}

func webCookieSelection(a *auth.Auth) web.CookieSelection {
	return web.CookieSelection{
		ID: a.ID, RegistrationEpoch: a.RegistrationEpoch, CredentialVersion: a.CredentialVersion,
		Disabled: a.Disabled || a.Status == auth.StatusDisabled,
	}
}

// CheckCookie authenticates a scheduled credential without starting a Web session.
func (e *GeminiEnterpriseWebExecutor) CheckCookie(ctx context.Context, a *auth.Auth) bool {
	c, err := web.CredentialFrom(a.Metadata)
	if err != nil || ctx.Err() != nil {
		return false
	}
	release := e.runtime.ClaimCookieCheck()
	if release == nil {
		return e.runtime.DeferCookieCC(a.ID, c)
	}
	defer release()
	err = e.runtime.CheckCookie(ctx, &c, e.client(ctx, a), webCookieSelection(a), a.ProxyURL+":"+e.cfg.ProxyURL+":")
	var failure *web.Failure
	return ctx.Err() == nil && e.runtime.Enabled() && !(errors.As(err, &failure) && failure.Code == http.StatusConflict)
}

func (e *GeminiEnterpriseWebExecutor) Refresh(ctx context.Context, a *auth.Auth) (*auth.Auth, error) {
	c, err := web.CredentialFrom(a.Metadata)
	if err != nil {
		return nil, err
	}
	route, rotate := e.cookieRoute(ctx, a)
	token, err := e.runtime.RefreshCookie(ctx, &c, e.client(ctx, a), webCookieSelection(a), route, rotate)
	if err != nil {
		return nil, err
	}
	c.SetCatalog(web.ModelCatalog(token), time.Now())
	updated := a.Clone()
	updated.Metadata["enterprise_web"] = c.MetadataValue()
	return updated, nil
}
func (*GeminiEnterpriseWebExecutor) CountTokens(context.Context, *auth.Auth, exec.Request, exec.Options) (exec.Response, error) {
	return exec.Response{}, web.Fail(501, "Media token counting is unavailable")
}
func (*GeminiEnterpriseWebExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, web.Fail(501, "Direct media transport calls are unavailable")
}

func webOutput(parts []wire.Part, finished bool, event wire.Event) []byte {
	if parts == nil {
		parts = []wire.Part{}
	}
	candidate := map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}}
	if finished {
		candidate["finishReason"] = "STOP"
		if event.GroundingMetadata != nil {
			candidate["groundingMetadata"] = event.GroundingMetadata
		}
		if event.CitationMetadata != nil {
			candidate["citationMetadata"] = event.CitationMetadata
		}
	}
	b, _ := json.Marshal(map[string]any{"candidates": []any{candidate}})
	return b
}
func webInputExecutionError(err error) error {
	if err == nil {
		return nil
	}
	var safe *web.Failure
	if errors.Is(err, context.Canceled) {
		err = web.Fail(499, "Media request was cancelled")
	} else if errors.Is(err, context.DeadlineExceeded) {
		err = web.Fail(504, "Media request deadline exceeded")
	}
	if !errors.As(err, &safe) {
		code := webcore.Status(err)
		var f *wire.Failure
		if errors.As(err, &f) {
			code = f.Code
		}
		err = web.Fail(code, "Media input could not be completed")
	}
	return err
}

type webModelEvidence struct {
	credential   web.Credential
	epoch        uint64
	submitted    bool
	counter      *helps.EnterpriseWebTokenCounter
	observation  *web.ExecutionObservation
	stopDelivery func()
}

func (e *webModelEvidence) settle(ctx context.Context, reporter *helps.UsageReporter, detail usage.Detail, err error, release func()) {
	defer e.stopDelivery()
	deliveryErr, tracked := usage.WaitStreamDelivery(ctx)
	if release != nil {
		release()
	}
	observation, finalErr := e.observation.Finish(ctx, err, deliveryErr, tracked, detail.Estimate != nil)
	if e.submitted {
		reporter.PublishWebResult(ctx, detail, observation, finalErr)
	}
}

func (e *webModelEvidence) settleSync(ctx context.Context, reporter *helps.UsageReporter, detail usage.Detail, err error) {
	e.observation.Stage(ctx, "delivery")
	if usage.StreamDeliveryTracked(ctx) {
		go e.settle(ctx, reporter, detail, err, nil)
	} else {
		e.settle(ctx, reporter, detail, err, nil)
	}
}

func (e *webModelEvidence) openFailure(ctx context.Context, reporter *helps.UsageReporter, err error) error {
	if e.stopDelivery == nil {
		return err
	}
	err = e.observation.OperationFailure(ctx, err)
	if e.submitted {
		err = exec.MarkGenerationStarted(err)
	}
	e.settleSync(ctx, reporter, e.counter.Detail(false), err)
	return err
}

func (e *GeminiEnterpriseWebExecutor) open(ctx context.Context, a *auth.Auth, req exec.Request, opts exec.Options, reporter *helps.UsageReporter, evidence *webModelEvidence) (response *http.Response, done func(), err error) {
	done = func() {}
	if op := exec.Operation(opts); op != "generate_content" && op != "stream_generate_content" {
		return nil, done, web.Fail(400, "A native content generation operation is required")
	}
	if web.RequestsImageOutput(req.Payload) {
		return nil, done, web.Fail(400, "Image generation is unavailable on the media input channel")
	}

	if opts.SourceFormat.String() != "gemini" || exec.ResponseFormatOrSource(opts).String() != "gemini" {
		return nil, done, web.Fail(400, "Only native content generation is supported")
	}
	if !web.HasMediaInContext(ctx, req.Payload) {
		return nil, done, web.Fail(400, "An inline attachment is required")
	}
	credential, errCredential := web.CredentialFrom(a.Metadata)
	if errCredential != nil {
		return nil, done, errCredential
	}
	model := resolveWebModel(a, req.Model)
	if model == "" {
		return nil, done, web.Fail(400, "An explicit media model ID is required")
	}
	observation := evidence.observation
	observation.SetModel(model)
	observation.Stage(ctx, "validation")
	if errPolicy := credential.ValidateModelPolicy(model, e.cfg.OAuthExcludedModels[web.Provider]); errPolicy != nil {
		return nil, done, errPolicy
	}
	lease := web.ExistingLease(ctx)
	ownsLease := lease == nil
	if lease == nil {
		var acquireErr error
		ctx, lease, acquireErr = e.runtime.Acquire(ctx, int64(len(req.Payload)))
		if acquireErr != nil {
			return nil, done, acquireErr
		}
		done = lease.Release
	}
	defer func() {
		// A manager-owned lease reports only the final result after any allowed
		// credential fallback. Direct executor callers own their own admission.
		if ownsLease && err != nil {
			e.runtime.RecordPreflight(ctx, err)
		}
	}()
	if errHistory := web.ValidateHistoryMode(ctx, req.Payload); errHistory != nil {
		e.runtime.RecordPreflight(ctx, errHistory)
		done()
		return nil, func() {}, errHistory
	}
	releaseLease := done
	budget := resources.New(lease.Budget())
	parseCtx, resourceLease := budget.Begin(ctx)
	parseCtx = webgemini.WithInputLimits(parseCtx, lease.InputLimits())
	parseCtx = webgemini.WithHistoryReservation(parseCtx, func(size int64) error {
		snapshot := budget.Snapshot()
		required := snapshot.InUseBytes + size
		if required <= snapshot.LimitBytes {
			return nil
		}
		if err := lease.EnsureBudget(ctx, required+resources.GenerationReserveBytes); err != nil {
			return err
		}
		budget.SetLimit(lease.Budget())
		return nil
	})
	done = func() { resourceLease.Close(); releaseLease() }
	failed := true
	defer func() {
		if failed {
			done()
			done = func() {}
		}
	}()
	if _, err = resources.Reserve(parseCtx, int64(len(req.Payload))); err != nil {
		return nil, done, webInputExecutionError(err)
	}
	var input wire.Request
	var errParse error
	if lease.HistoryMode() == config.EnterpriseWebSerializedContext {
		input, errParse = webgemini.ParseSerializedBytes(parseCtx, req.Payload, webconfig.Binding{})
	} else {
		input, errParse = webgemini.ParseBytes(parseCtx, req.Payload, webconfig.Binding{}, nil)
	}
	if errParse != nil {
		var failure *wire.Failure
		if errors.As(errParse, &failure) {
			return nil, done, web.InputFailureFrom(failure)
		}
		return nil, done, webInputExecutionError(errParse)
	}
	if len(input.Attachments) == 0 {
		return nil, done, web.Fail(400, "An inline attachment is required")
	}
	if lease.HistoryMode() == config.EnterpriseWebSingleTurn {
		input.ContextSummary.Contents, input.ContextSummary.LogicalContents = 1, 1
		input.ContextSummary.Attachments = len(input.Attachments)
		for _, attachment := range input.Attachments {
			input.ContextSummary.AttachmentBytes += len(attachment.Data)
		}
	}
	observation.SetSummary(input.ContextSummary)
	reporter.SetUpstreamModel(model)
	if err = web.ValidateModelProbe(ctx, a.ID, credential, model, e.runtime.EvidenceEpoch(a.ID)); err != nil {
		return nil, done, err
	}
	client := diagnostics.FinalizeClient(ctx, e.client(ctx, a))
	ctx = observation.Context(ctx)
	observation.Stage(ctx, "authentication")
	authCtx := diagnostics.WithCallKind(ctx, "auth")
	route, rotate := e.cookieRoute(ctx, a)
	token, releasePreparation, errToken := e.runtime.PrepareCookie(authCtx, &credential, client, webCookieSelection(a), route, model, rotate)
	if errToken != nil {
		return nil, done, web.AuthenticationPreflightFailure(errToken)
	}
	defer releasePreparation()
	if !web.AuthorizedModel(token, model) {
		return nil, done, web.Fail(400, "The selected model ID is unavailable in this Plus configuration")
	}
	if input.GoogleSearch {
		if err = web.RequireGoogleSearch(authCtx, client, credential, token); err != nil {
			return nil, done, web.AuthenticationPreflightFailure(err)
		}
	}
	if err = web.ValidateModelPolicy(ctx, a.ID, credential, model, e.cfg.OAuthExcludedModels[web.Provider]); err != nil {
		return nil, done, err
	}
	envelope := func(action string, data any) map[string]any {
		return map[string]any{"configId": credential.ConfigID, "additionalParams": map[string]string{"token": "-", "origin": "ORIGIN_UNSPECIFIED"}, action: data}
	}
	exec.MarkUpstreamAttempt(ctx)
	lease.MarkSessionStarted()
	web.PublishWebSessionStarted(ctx)
	evidence.stopDelivery = usage.SupportObservedStreamDelivery(ctx)
	observation.Stage(ctx, "session")
	sessionResponse, errSession := web.PostJSON(ctx, client, credential, token, "widgetCreateSession", envelope("createSessionRequest", map[string]any{"session": map[string]string{"name": "-", "displayName": ""}}))
	if errSession != nil {
		return nil, done, errSession
	}
	var session struct {
		Session struct {
			Name string `json:"name"`
		} `json:"session"`
	}
	if err = web.ReadJSON(sessionResponse, &session); err != nil {
		return nil, done, err
	}
	session.Session.Name, err = web.NormalizeSession(token, session.Session.Name)
	if err != nil {
		return nil, done, err
	}
	ids := make([]string, 0, len(input.Attachments))
	observation.Stage(ctx, "upload")
	for _, attachment := range input.Attachments {
		if err = web.ValidateModelPolicy(ctx, a.ID, credential, model, e.cfg.OAuthExcludedModels[web.Provider]); err != nil {
			return nil, done, err
		}
		fileID, errUpload := web.Upload(ctx, client, credential, token, session.Session.Name, attachment)
		if errUpload != nil {
			return nil, done, errUpload
		}
		ids = append(ids, fileID)
	}
	toolsSpec := map[string]any{"toolRegistry": "default_tool_registry"}
	if input.GoogleSearch {
		toolsSpec["webGroundingSpec"] = struct{}{}
	}
	assist := map[string]any{"session": session.Session.Name, "query": map[string]any{"parts": input.QueryParts}, "fileIds": ids, "answerGenerationMode": "NORMAL", "assistSkippingMode": "REQUEST_ASSIST", "assistGenerationConfig": map[string]string{"modelId": model}, "agentsSpec": map[string]any{}, "toolsSpec": toolsSpec, "languageCode": "en"}
	body, _ := json.Marshal(envelope("streamAssistRequest", assist))
	// The native body is request-owned and immutable; avoid a second base64-sized snapshot.
	body = helps.ApplyPayloadConfigWithRequestForExecutor(e.cfg, e.Identifier(), req.Model, e.Identifier(), opts.SourceFormat.String(), "", body, req.Payload, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	// Validate bindings after payload rules without changing their result.
	if !json.Valid(body) || gjson.GetBytes(body, "configId").String() != credential.ConfigID || gjson.GetBytes(body, "streamAssistRequest.session").String() != session.Session.Name || gjson.GetBytes(body, "streamAssistRequest.assistGenerationConfig.modelId").String() != model || !web.SameFileIDs(body, ids) {
		return nil, done, web.Fail(400, "Final media payload is missing required fields or changes its binding")
	}
	if input.GoogleSearch {
		spec := gjson.GetBytes(body, "streamAssistRequest.toolsSpec.webGroundingSpec")
		if !spec.IsObject() || len(spec.Map()) != 0 {
			return nil, done, web.Fail(400, "Payload rules must preserve the requested Google Search tool")
		}
	}
	// Optional tokenization can take time on the first request. Keep the final
	// credential/policy barriers after it, immediately before generation submission.
	evidence.counter = helps.NewEnterpriseWebTokenCounter(parseCtx, body)
	evidence.credential, evidence.epoch = credential, e.runtime.EvidenceEpoch(a.ID)
	if err = web.ValidateModelProbe(ctx, a.ID, credential, model, evidence.epoch); err != nil {
		return nil, done, err
	}
	if err = web.ValidateModelPolicy(ctx, a.ID, credential, model, e.cfg.OAuthExcludedModels[web.Provider]); err != nil {
		return nil, done, err
	}
	observation.Stage(ctx, "generation")
	reporter.StartResponseTTFT()
	response, err = web.Post(diagnostics.WithCallKind(ctx, "model"), client, credential, token, "widgetStreamAssist", bytes.NewReader(body), int64(len(body)))
	evidence.submitted = observation.Submitted()
	if err != nil {
		return nil, done, err
	}
	failed = false
	reporter.ObserveResponsePacketOnly(response)
	return response, done, nil
}

func closeWebResponseOnCancel(ctx context.Context, response *http.Response) func() {
	var once sync.Once
	closeBody := func() { once.Do(func() { _ = response.Body.Close() }) }
	stop := context.AfterFunc(ctx, closeBody)
	return func() {
		stop()
		closeBody()
	}
}

func (e *GeminiEnterpriseWebExecutor) Execute(ctx context.Context, a *auth.Auth, req exec.Request, opts exec.Options) (out exec.Response, err error) {
	reporter := newWebUsageReporter(ctx, e, a, req, opts)
	evidence := webModelEvidence{observation: web.NewExecutionObservation(helps.PayloadRequestedModel(opts, req.Model), reporter.TraceID(), a.EnsureIndex(), false)}
	res, done, err := e.open(ctx, a, req, opts, reporter, &evidence)
	if err != nil {
		return out, evidence.openFailure(ctx, reporter, err)
	}
	closeBody := closeWebResponseOnCancel(ctx, res)
	defer func() {
		closeBody()
		detail := evidence.counter.Detail(evidence.observation.Complete())
		done()
		evidence.settleSync(ctx, reporter, detail, err)
	}()
	var parts []wire.Part
	var final wire.Event
	err = evidence.observation.Parse(ctx, res.Body, func(event wire.Event) error {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
		evidence.counter.Observe(event.Parts)
		parts = append(parts, event.Parts...)
		if event.Finished {
			final = event
		}
		return nil
	})
	if err == nil && ctx.Err() != nil {
		err = evidence.observation.OperationFailure(ctx, ctx.Err())
	}
	if err != nil {
		return out, exec.MarkGenerationStarted(err)
	}
	e.runtime.RecordCredentialModel(a.ID, evidence.credential, reporter.UpstreamModel(), evidence.epoch)
	detail := evidence.counter.Detail(true)
	return exec.Response{Payload: helps.EnterpriseWebUsagePayload(webOutput(parts, true, final), detail)}, nil
}
func (e *GeminiEnterpriseWebExecutor) ExecuteStream(ctx context.Context, a *auth.Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	reporter := newWebUsageReporter(ctx, e, a, req, opts)
	reporter.SetStream(true)
	evidence := webModelEvidence{observation: web.NewExecutionObservation(helps.PayloadRequestedModel(opts, req.Model), reporter.TraceID(), a.EnsureIndex(), true)}
	res, done, err := e.open(ctx, a, req, opts, reporter, &evidence)
	if err != nil {
		return nil, evidence.openFailure(ctx, reporter, err)
	}
	chunks := make(chan exec.StreamChunk)
	go func() {
		closeBody := closeWebResponseOnCancel(ctx, res)
		errStream := evidence.observation.Parse(ctx, res.Body, func(event wire.Event) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			evidence.counter.Observe(event.Parts)
			if len(event.Parts) == 0 && !event.Finished {
				return nil
			}
			b := webOutput(event.Parts, event.Finished, event)
			if event.Finished {
				b = helps.EnterpriseWebUsagePayload(b, evidence.counter.Detail(true))
			}
			select {
			case chunks <- exec.StreamChunk{Payload: b}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		closeBody()
		if errStream != nil {
			errStream = exec.MarkGenerationStarted(errStream)
			select {
			case chunks <- exec.StreamChunk{Err: errStream}:
			case <-ctx.Done():
			}
		} else {
			e.runtime.RecordCredentialModel(a.ID, evidence.credential, reporter.UpstreamModel(), evidence.epoch)
		}
		detail := evidence.counter.Detail(evidence.observation.Complete())
		evidence.observation.Stage(ctx, "delivery")
		if usage.StreamDeliveryTracked(ctx) {
			close(chunks)
			evidence.settle(ctx, reporter, detail, errStream, done)
		} else {
			evidence.settle(ctx, reporter, detail, errStream, done)
			close(chunks)
		}
	}()
	return &exec.StreamResult{Chunks: chunks}, nil
}

func resolveWebModel(a *auth.Auth, requested string) string {
	name := thinking.ParseSuffix(strings.TrimPrefix(requested, a.Prefix+"/")).ModelName
	if strings.EqualFold(name, "auto") || strings.EqualFold(name, "default") {
		return ""
	}
	return name
}
func newWebUsageReporter(ctx context.Context, e *GeminiEnterpriseWebExecutor, a *auth.Auth, req exec.Request, opts exec.Options) *helps.UsageReporter {
	model := req.Model
	if _, err := web.CredentialFrom(a.Metadata); err == nil {
		if selected := resolveWebModel(a, req.Model); selected != "" {
			model = selected
		}
	}
	if usage.RequestedModelAliasFromContext(ctx) == "" {
		ctx = usage.WithRequestedModelAlias(ctx, helps.PayloadRequestedModel(opts, req.Model))
	}
	return helps.NewExecutorUsageReporter(ctx, e, model, a)
}
