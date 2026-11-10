package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

const antigravityEnterpriseDiagnosticKey = "antigravity_enterprise_diagnostic_model"

type antigravityEnterpriseMetadata struct {
	config   ae.Config
	configAt time.Time
	quota    ae.Quota
}

// AntigravityEnterpriseExecutor uses the subscription-only Business AI Code API.
type AntigravityEnterpriseExecutor struct {
	cfg      *config.Config
	manager  *coreauth.Manager
	mu       sync.Mutex
	metadata map[string]antigravityEnterpriseMetadata
	flights  singleflight.Group
}

func NewAntigravityEnterpriseExecutor(cfg *config.Config) *AntigravityEnterpriseExecutor {
	return &AntigravityEnterpriseExecutor{cfg: cfg, metadata: make(map[string]antigravityEnterpriseMetadata)}
}
func (e *AntigravityEnterpriseExecutor) WithManager(manager *coreauth.Manager) *AntigravityEnterpriseExecutor {
	e.manager = manager
	return e
}
func (*AntigravityEnterpriseExecutor) Identifier() string { return ae.Provider }
func (*AntigravityEnterpriseExecutor) RequestToFormat(coreexec.Request, coreexec.Options) translator.Format {
	return translator.FormatGemini
}

func (e *AntigravityEnterpriseExecutor) current(ctx context.Context, auth *coreauth.Auth, allowDisabled bool) (*coreauth.Auth, error) {
	if e.cfg != nil && e.cfg.Home.Enabled {
		return nil, &ae.Error{HTTP: 409, Code: "home_managed"}
	}
	if auth == nil || auth.Provider != ae.Provider || coreauth.IsAntigravityEnterpriseAccountAuth(auth) {
		return nil, &ae.Error{HTTP: 409, Code: "binding_required"}
	}
	if err := coreauth.AntigravityEnterpriseLicense(auth).Validate(); err != nil {
		return nil, &ae.Error{HTTP: 409, Code: "binding_invalid"}
	}
	if !allowDisabled && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return nil, &ae.Error{HTTP: 409, Code: "binding_disabled"}
	}
	if e.manager != nil {
		latest, ok := e.manager.GetByID(auth.ID)
		if !ok || latest.RegistrationEpoch != auth.RegistrationEpoch || !allowDisabled && (latest.Disabled || latest.Status == coreauth.StatusDisabled) {
			return nil, &ae.Error{HTTP: 409, Code: "binding_changed"}
		}
		updated, err := e.manager.EnsureAntigravityEnterpriseAccountToken(ctx, auth.ID)
		if err != nil {
			return nil, err
		}
		if updated == nil || updated.RegistrationEpoch != auth.RegistrationEpoch || !allowDisabled && updated.Disabled {
			return nil, &ae.Error{HTTP: 409, Code: "binding_changed"}
		}
		return updated, nil
	}
	if auth.HasValidAccessToken(time.Now().Add(30 * time.Second)) {
		return auth.Clone(), nil
	}
	return e.Refresh(ctx, auth)
}

func (e *AntigravityEnterpriseExecutor) client(ctx context.Context, auth *coreauth.Auth) *http.Client {
	return diagnostics.FinalizeClient(ctx, ae.SafeClient(helps.NewProxyAwareHTTPClientBase(ctx, e.cfg, auth, 0)))
}
func antigravityEnterpriseMetadataKey(auth *coreauth.Auth) string {
	l := coreauth.AntigravityEnterpriseLicense(auth)
	return ae.Digest(auth.ID, l.PoolKey(), auth.ProxyURL, time.Unix(int64(auth.RegistrationEpoch), 0).Format(time.RFC3339), time.Unix(int64(auth.CredentialVersion), 0).Format(time.RFC3339))
}

func antigravityEnterpriseQuotaFresh(quota ae.Quota, now time.Time) bool {
	if quota.ObservedAt.IsZero() || quota.ObservedAt.After(now) || now.Sub(quota.ObservedAt) >= ae.QuotaTTL {
		return false
	}
	if bucket, err := quota.Weekly(); err == nil && bucket.ResetTime != nil && !now.Before(*bucket.ResetTime) {
		return false
	}
	return true
}

// Catalog reads only model configuration. Quota permissions cannot hide a readable directory.
func (e *AntigravityEnterpriseExecutor) Catalog(ctx context.Context, auth *coreauth.Auth, refresh bool) (ae.Config, error) {
	current, err := e.current(ctx, auth, true)
	if err != nil {
		return ae.Config{}, err
	}
	key := antigravityEnterpriseMetadataKey(current)
	e.mu.Lock()
	cached := e.metadata[key]
	e.mu.Unlock()
	if !refresh && !cached.configAt.IsZero() && time.Since(cached.configAt) < 3*time.Hour {
		return cached.config, nil
	}
	flight := e.flights.DoChan("catalog:"+key, func() (any, error) {
		e.mu.Lock()
		latest := e.metadata[key]
		e.mu.Unlock()
		if !refresh && !latest.configAt.IsZero() && time.Since(latest.configAt) < 3*time.Hour {
			return latest.config, nil
		}
		config, err := ae.FetchConfig(ctx, e.client(ctx, current), ae.String(current.Metadata, "access_token"), coreauth.AntigravityEnterpriseLicense(current))
		if err != nil {
			return latest.config, err
		}
		if !e.metadataCurrent(current) {
			return latest.config, &ae.Error{HTTP: 409, Code: "binding_changed"}
		}
		e.mu.Lock()
		latest = e.metadata[key]
		latest.config, latest.configAt = config, time.Now()
		e.metadata[key] = latest
		e.mu.Unlock()
		return config, nil
	})
	select {
	case result := <-flight:
		if config, ok := result.Val.(ae.Config); ok {
			return config, result.Err
		}
		return cached.config, result.Err
	case <-ctx.Done():
		return cached.config, ctx.Err()
	}
}

// Quota reads only subscription quota and never changes a binding's user preference.
func (e *AntigravityEnterpriseExecutor) Quota(ctx context.Context, auth *coreauth.Auth, refresh bool) (ae.Quota, error) {
	current, err := e.current(ctx, auth, true)
	if err != nil {
		return ae.Quota{}, err
	}
	key := antigravityEnterpriseMetadataKey(current)
	e.mu.Lock()
	cached := e.metadata[key].quota
	e.mu.Unlock()
	if e.manager != nil {
		if q, ok := e.manager.AntigravityEnterpriseQuota(current); ok && q.ObservedAt.After(cached.ObservedAt) {
			cached = q
		}
	}
	if !refresh && antigravityEnterpriseQuotaFresh(cached, time.Now()) {
		return cached, nil
	}
	flight := e.flights.DoChan("quota:"+key, func() (any, error) {
		e.mu.Lock()
		latest := e.metadata[key].quota
		e.mu.Unlock()
		if cached.ObservedAt.After(latest.ObservedAt) {
			latest = cached
		}
		if !refresh && antigravityEnterpriseQuotaFresh(latest, time.Now()) {
			return latest, nil
		}
		quota, err := ae.FetchQuota(ctx, e.client(ctx, current), ae.String(current.Metadata, "access_token"), coreauth.AntigravityEnterpriseLicense(current))
		if err != nil {
			return latest, err
		}
		if !e.metadataCurrent(current) {
			return latest, &ae.Error{HTTP: 409, Code: "binding_changed"}
		}
		if e.manager != nil && !e.manager.ObserveAntigravityEnterpriseQuota(current, quota) {
			return latest, &ae.Error{HTTP: 409, Code: "binding_changed"}
		}
		e.mu.Lock()
		entry := e.metadata[key]
		entry.quota = quota
		e.metadata[key] = entry
		e.mu.Unlock()
		return quota, nil
	})
	select {
	case result := <-flight:
		if quota, ok := result.Val.(ae.Quota); ok {
			return quota, result.Err
		}
		return cached, result.Err
	case <-ctx.Done():
		return cached, ctx.Err()
	}
}

func (e *AntigravityEnterpriseExecutor) metadataCurrent(auth *coreauth.Auth) bool {
	if e.manager == nil {
		return true
	}
	current, ok := e.manager.GetByID(auth.ID)
	return ok && current.RegistrationEpoch == auth.RegistrationEpoch && current.CredentialVersion == auth.CredentialVersion && coreauth.AntigravityEnterpriseLicense(current) == coreauth.AntigravityEnterpriseLicense(auth)
}

// Metadata requires both independent reads for subscription generation preflight.
func (e *AntigravityEnterpriseExecutor) Metadata(ctx context.Context, auth *coreauth.Auth, refresh bool) (ae.Config, ae.Quota, error) {
	config, configErr := e.Catalog(ctx, auth, refresh)
	quota, quotaErr := e.Quota(ctx, auth, refresh)
	if configErr != nil {
		return config, quota, configErr
	}
	return config, quota, quotaErr
}

func (e *AntigravityEnterpriseExecutor) resolveModel(config ae.Config, name string, opts coreexec.Options) (ae.Model, *registry.ModelInfo, error) {
	base := thinking.ParseSuffix(name).ModelName
	for _, m := range ae.Models(config) {
		if m.ID != base {
			continue
		}
		for _, info := range AntigravityEnterpriseModels(config) {
			if info.ID == m.ID {
				return m, info, nil
			}
		}
	}
	if opts.Metadata != nil && opts.Metadata[antigravityEnterpriseDiagnosticKey] == ae.CarbonModel {
		for _, level := range []string{"", "low", "medium", "high"} {
			id := ae.CarbonModel
			if level != "" {
				id += "-" + level
			}
			if base == id {
				return ae.Model{ID: base, Upstream: base, Family: ae.CarbonModel, Canonical: ae.CarbonModel, Level: level, Source: "explicit_diagnostic"}, &registry.ModelInfo{ID: base, OwnedBy: ae.Provider, Type: ae.Provider, Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}}, nil
			}
		}
	}
	return ae.Model{}, nil, &ae.Error{HTTP: 400, Code: "model_not_available"}
}

func (e *AntigravityEnterpriseExecutor) prepare(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options, stream bool) (*coreauth.Auth, *http.Request, []byte, ae.Model, error) {
	current, err := e.current(ctx, auth, false)
	if err != nil {
		return nil, nil, nil, ae.Model{}, err
	}
	config, quota, err := e.Metadata(ctx, current, false)
	if err != nil {
		// Failed metadata reads do not constitute generation attempts and must not
		// place an otherwise valid binding into a model or credential cooldown.
		var failure *ae.Error
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &failure) && (failure.HTTP == http.StatusUnauthorized || failure.HTTP == http.StatusConflict) {
			return nil, nil, nil, ae.Model{}, err
		}
		return nil, nil, nil, ae.Model{}, &ae.Error{HTTP: http.StatusServiceUnavailable, Code: "metadata_pending"}
	}
	if err = quota.Admit(time.Now()); err != nil {
		return nil, nil, nil, ae.Model{}, err
	}
	model, info, err := e.resolveModel(config, req.Model, opts)
	if err != nil {
		return nil, nil, nil, model, err
	}
	if expected, _ := opts.Metadata["antigravity_enterprise_expected_model"].(string); expected != "" && expected != model.ID {
		return nil, nil, nil, model, &ae.Error{HTTP: 409, Code: "model_changed"}
	}
	source := req.Payload
	if len(opts.OriginalRequest) > 0 {
		source = opts.OriginalRequest
	}
	original, body, err := helps.TranslateRequestPairReturningError(ctx, opts.Headers, e.cfg, opts.SourceFormat, translator.FormatGemini, model.Upstream, source, req.Payload, stream, false)
	if err != nil {
		return nil, nil, nil, model, err
	}
	finalize := helps.NewPayloadFinalizer(e.cfg, ae.Provider, model.Upstream, translator.FormatGemini.String(), "", original, req, opts)
	if !gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel").Exists() && !gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingBudget").Exists() && model.Level != "" {
		body, err = sjson.SetBytes(body, "generationConfig.thinkingConfig.thinkingLevel", model.Level)
		if err != nil {
			return nil, nil, nil, model, err
		}
	}
	body, err = thinking.ApplyThinkingWithModelInfo(body, source, req.Model, opts.SourceFormat.String(), ae.Provider, ae.Provider, info)
	if err != nil {
		return nil, nil, nil, model, err
	}
	body, err = helps.NormalizeBusinessAICodeSafety(body)
	if err != nil {
		return nil, nil, nil, model, err
	}
	body = signature.SanitizeGeminiRequestThoughtSignatures(body, "contents")
	body, err = helps.NormalizeBusinessAICodeFunctionResponses(body)
	if err != nil {
		return nil, nil, nil, model, err
	}
	body = helps.EnsureGeminiBoundaryUserContent(body, "contents")
	body, err = helps.NormalizeBusinessAICodeSearchTools(body)
	if err != nil {
		return nil, nil, nil, model, err
	}
	license := coreauth.AntigravityEnterpriseLicense(current)
	for _, path := range []string{"session_id", "project", "request", "entitlement", "aicode"} {
		body, err = sjson.DeleteBytes(body, path)
		if err != nil {
			return nil, nil, nil, model, err
		}
	}
	body, err = sjson.SetBytes(body, "model", model.Upstream)
	if err != nil {
		return nil, nil, nil, model, err
	}
	body, err = sjson.SetBytes(body, "parent", license.Parent())
	if err != nil {
		return nil, nil, nil, model, err
	}
	body, err = sjson.SetBytes(body, "entitlement.userTier", license.UserTier)
	if err != nil {
		return nil, nil, nil, model, err
	}
	// User rules are the final business-payload mutation in every execution path.
	body = finalize(body)
	if !helps.BusinessAICodeSearchToolsValid(body) {
		return nil, nil, nil, model, &ae.Error{HTTP: http.StatusBadRequest, Code: "invalid_search_tools"}
	}
	// model and aicode are mutually exclusive members of the native oneof.
	if !gjson.ValidBytes(body) || !helps.BusinessAICodeJSONIsUnambiguous(body) || !helps.BusinessAICodeSafetyValid(body) || gjson.GetBytes(body, "model").String() != model.Upstream || gjson.GetBytes(body, "parent").String() != license.Parent() || gjson.GetBytes(body, "entitlement.userTier").String() != license.UserTier || len(gjson.GetBytes(body, "entitlement").Map()) != 1 || gjson.GetBytes(body, "aicode").Exists() || !gjson.GetBytes(body, "contents").IsArray() {
		return nil, nil, nil, model, &ae.Error{HTTP: 409, Code: "payload_binding_conflict"}
	}
	if e.manager != nil {
		if err = e.manager.CheckAntigravityEnterpriseQuota(current, time.Now()); err != nil {
			return nil, nil, nil, model, err
		}
		latest, ok := e.manager.GetByID(current.ID)
		if !ok || latest.Disabled || latest.RegistrationEpoch != current.RegistrationEpoch || coreauth.AntigravityEnterpriseLicense(latest) != license {
			return nil, nil, nil, model, &ae.Error{HTTP: 409, Code: "binding_changed"}
		}
	}
	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}
	target := ae.Origin(license.Location) + "/v1beta/" + license.Parent() + ":" + action
	if stream {
		target += "?alt=sse"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil, model, err
	}
	util.ApplyCustomHeadersFromAttrs(request, current.Attributes, opts.Headers)
	if err = e.PrepareRequest(request, current); err != nil {
		return nil, nil, nil, model, err
	}
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: target, Method: http.MethodPost, Headers: request.Header.Clone(), Body: body, Provider: ae.Provider, AuthID: current.ID, AuthLabel: current.Label})
	return current, request, body, model, nil
}

// PrepareRequest only accepts this binding's official Business AI Code origin.
func (e *AntigravityEnterpriseExecutor) PrepareRequest(req *http.Request, auth *coreauth.Auth) error {
	if req == nil || req.URL == nil || auth == nil || auth.Provider != ae.Provider {
		return &ae.Error{HTTP: 400, Code: "invalid_request"}
	}
	license := coreauth.AntigravityEnterpriseLicense(auth)
	if license.Validate() != nil || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" || req.Host != "" && req.Host != req.URL.Host || "https://"+req.URL.Host != ae.Origin(license.Location) {
		return &ae.Error{HTTP: 400, Code: "untrusted_endpoint"}
	}
	base := "/v1beta/" + license.Parent() + ":"
	allowed := false
	if req.Method == http.MethodGet {
		allowed = req.URL.Path == base+"fetchConfig" || req.URL.Path == base+"fetchQuotaStatus"
	}
	if req.Method == http.MethodPost {
		allowed = req.URL.Path == base+"generateContent" || req.URL.Path == base+"streamGenerateContent"
	}
	if !allowed {
		return &ae.Error{HTTP: 400, Code: "operation_not_supported"}
	}
	if ae.String(auth.Metadata, "access_token") == "" {
		return &ae.Error{HTTP: 401, Code: "missing_access_token"}
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Del("x-goog-api-key")
	ae.SetHeaders(req, ae.String(auth.Metadata, "access_token"), license.ProjectID)
	return nil
}

func (e *AntigravityEnterpriseExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil || req.Method != http.MethodGet {
		return nil, &ae.Error{HTTP: 400, Code: "read_only_operation_required"}
	}
	current, err := e.current(ctx, auth, true)
	if err != nil {
		return nil, err
	}
	copyRequest := req.Clone(ctx)
	copyRequest.Header = req.Header.Clone()
	if err = e.PrepareRequest(copyRequest, current); err != nil {
		return nil, err
	}
	license := coreauth.AntigravityEnterpriseLicense(current)
	if copyRequest.URL.Query().Get("entitlement.userTier") != license.UserTier {
		return nil, &ae.Error{HTTP: 409, Code: "payload_binding_conflict"}
	}
	return e.client(ctx, current).Do(copyRequest)
}

func (e *AntigravityEnterpriseExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (response coreexec.Response, err error) {
	if opts.Alt == "responses/compact" {
		return response, &ae.Error{HTTP: 400, Code: "operation_not_supported"}
	}
	current, request, body, model, err := e.prepare(ctx, auth, req, opts, false)
	if err != nil {
		return response, antigravityEnterpriseLocalError(err)
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, current)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetUpstreamModel(model.Canonical)
	reporter.ObserveGeminiRequest(body)
	reporter.SetTranslatedReasoningEffort(body, translator.FormatGemini.String())
	upstream, err := reporter.TrackHTTPClient(e.client(ctx, current)).Do(request)
	if err != nil {
		return response, coreexec.TransportFailure(&ae.Error{HTTP: 502, Code: "upstream_transport", Attempted: true, Cause: err})
	}
	defer func() { _ = upstream.Body.Close() }()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, upstream.StatusCode, upstream.Header.Clone())
	data, err := io.ReadAll(upstream.Body)
	if err != nil {
		return response, reporter.GeminiReadFailure(data, &ae.Error{HTTP: 502, Code: "upstream_read", Attempted: true, Cause: err})
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 || gjson.GetBytes(data, "error").Exists() {
		failure := ae.DecodeError(upstream.StatusCode, upstream.Header, data, false)
		failure.Attempted = true
		e.observeError(ctx, current, failure, uuid.NewString())
		return response, reporter.GeminiResponseFailure(data, failure)
	}
	reporter.ObserveResponseModel(data)
	if err = reporter.ObserveGeminiNonStream(data, upstream.Trailer); err != nil {
		return response, err
	}
	var state any
	responseFormat := coreexec.ResponseFormatOrSource(opts)
	converted := translator.TranslateNonStream(ctx, translator.FormatGemini, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, data, &state)
	if helps.ApplyPatchTranslationError(state) != nil || len(converted) == 0 {
		return response, reporter.GeminiTranslationFailure(&ae.Error{HTTP: 502, Code: "response_translation_failed", Attempted: true})
	}
	if responseFormat == translator.FormatOpenAIResponse {
		converted = helps.EnsureResponsesUsageDetails(converted)
	}
	reporter.Publish(ctx, helps.ParseGeminiUsage(data))
	return coreexec.Response{Payload: helps.RewriteResponseModelVersion(converted, helps.PayloadRequestedModel(opts, req.Model), model.Upstream), Headers: upstream.Header.Clone()}, nil
}

func (e *AntigravityEnterpriseExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (_ *coreexec.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, &ae.Error{HTTP: 400, Code: "operation_not_supported"}
	}
	current, request, body, model, err := e.prepare(ctx, auth, req, opts, true)
	if err != nil {
		return nil, antigravityEnterpriseLocalError(err)
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, current)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetUpstreamModel(model.Canonical)
	reporter.ObserveGeminiRequest(body)
	reporter.SetTranslatedReasoningEffort(body, translator.FormatGemini.String())
	upstream, err := reporter.TrackHTTPClient(e.client(ctx, current)).Do(request)
	if err != nil {
		return nil, coreexec.TransportFailure(&ae.Error{HTTP: 502, Code: "upstream_transport", Attempted: true, Cause: err})
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, upstream.StatusCode, upstream.Header.Clone())
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		defer func() { _ = upstream.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(upstream.Body, 4<<20))
		failure := ae.DecodeError(upstream.StatusCode, upstream.Header, data, false)
		failure.Attempted = true
		e.observeError(ctx, current, failure, uuid.NewString())
		return nil, reporter.GeminiResponseFailure(data, failure)
	}
	var state any
	format := coreexec.ResponseFormatOrSource(opts)
	attemptID := uuid.NewString()
	var observeFailure sync.Once
	claudeTokens := helps.NewClaudeInputTokenState(opts.SourceFormat, translator.FormatGemini, format, req.Payload)
	helps.InitializeApplyPatchStream(ctx, translator.FormatGemini, format, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, &state)
	result := helps.ForwardObservedGeminiStream(ctx, upstream, reporter, helps.ObservedGeminiStreamOptions{
		Native:  format == translator.FormatGemini,
		OnEvent: func(raw []byte) { helps.AppendAPIResponseChunk(ctx, e.cfg, raw) },
		Translate: func(raw []byte) ([][]byte, error) {
			lines := helps.TranslateStreamWithClaudeInputTokens(ctx, translator.FormatGemini, format, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, raw, &state, claudeTokens)
			if helps.ApplyPatchTranslationError(state) != nil {
				return lines, &ae.Error{HTTP: 502, Code: "response_translation_failed", Attempted: true}
			}
			for i := range lines {
				lines[i] = helps.RewriteSSEModelVersion(lines[i], helps.PayloadRequestedModel(opts, req.Model), model.Upstream)
			}
			return lines, nil
		},
		Finalize: func() ([][]byte, error) {
			chunks := helps.FinalizeApplyPatchStream(state)
			if helps.ApplyPatchTranslationError(state) != nil {
				return chunks, &ae.Error{HTTP: 502, Code: "response_translation_failed", Attempted: true}
			}
			return chunks, nil
		},
		DecodeError: func(raw []byte) error {
			failure := ae.DecodeError(200, upstream.Header, raw, false)
			failure.Attempted = true
			observeFailure.Do(func() { e.observeError(ctx, current, failure, attemptID) })
			return failure
		},
	})
	return result, nil
}

// observeError rechecks 429 metadata; HTTP status alone never means weekly exhaustion.
func (e *AntigravityEnterpriseExecutor) observeError(ctx context.Context, auth *coreauth.Auth, failure *ae.Error, attemptID string) {
	if failure.HTTP != 429 || ctx.Err() != nil {
		return
	}
	if e.manager != nil {
		if deadline := e.manager.ObserveAntigravityEnterpriseRateLimit(auth, attemptID, failure); !deadline.IsZero() {
			failure.Code, failure.Scope, failure.RetryAt = "binding_rate_limit", "binding", deadline
		}
	}
	license := coreauth.AntigravityEnterpriseLicense(auth)
	quota, err := ae.FetchQuota(ctx, e.client(ctx, auth), ae.String(auth.Metadata, "access_token"), license)
	if err != nil {
		return
	}
	if e.manager != nil {
		e.manager.ObserveAntigravityEnterpriseQuota(auth, quota)
	}
	if bucket, err := quota.Weekly(); err == nil && bucket.Exhausted() {
		failure.Code, failure.Scope = "weekly_quota", "shared_pool"
		failure.RetryAt = time.Time{}
		if bucket.ResetTime != nil && bucket.ResetTime.After(time.Now()) {
			failure.RetryAt = *bucket.ResetTime
		}
		if e.manager != nil {
			e.manager.RestrictAntigravityEnterpriseQuota(auth, failure)
		}
	}
}

func (*AntigravityEnterpriseExecutor) CountTokens(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, &ae.Error{HTTP: 400, Code: "operation_not_supported"}
}

func (e *AntigravityEnterpriseExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if e.cfg != nil && e.cfg.Home.Enabled {
		return nil, &ae.Error{HTTP: 409, Code: "home_managed"}
	}
	if auth == nil || auth.Provider != ae.Provider || ae.String(auth.Metadata, "refresh_token") == "" {
		return nil, &ae.Error{HTTP: 401, Code: "reauthorization_required"}
	}
	application := ae.OAuth("")
	if clientID := ae.String(auth.Metadata, "client_id"); clientID != "" && clientID != application.ClientID {
		return nil, &ae.Error{HTTP: 401, Code: "oauth_client_mismatch"}
	}
	credentialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	credentialCtx = context.WithValue(credentialCtx, oauth2.HTTPClient, e.client(ctx, auth))
	token, err := application.TokenSource(credentialCtx, &oauth2.Token{RefreshToken: ae.String(auth.Metadata, "refresh_token")}).Token()
	if err != nil {
		var retrieve *oauth2.RetrieveError
		if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
			return nil, &ae.Error{HTTP: 401, Code: "invalid_grant"}
		}
		return nil, &ae.Error{HTTP: 502, Code: "token_refresh_failed"}
	}
	if token.AccessToken == "" {
		return nil, &ae.Error{HTTP: 401, Code: "missing_access_token"}
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = token.AccessToken
	if token.RefreshToken != "" {
		updated.Metadata["refresh_token"] = token.RefreshToken
	}
	updated.Metadata["expired"] = token.Expiry.UTC().Format(time.RFC3339Nano)
	updated.Metadata["expiry"] = token.Expiry.UTC().Format(time.RFC3339Nano)
	updated.Metadata["timestamp"] = time.Now().UnixMilli()
	updated.LastRefreshedAt = time.Now()
	return updated, nil
}

// UsesConfig allows the service to retain metadata cache across account registrations.
func (e *AntigravityEnterpriseExecutor) UsesConfig(cfg *config.Config) bool { return e.cfg == cfg }

// AvailableModels is useful for account management without any generation request.
func (e *AntigravityEnterpriseExecutor) AvailableModels(ctx context.Context, auth *coreauth.Auth, refresh bool) ([]ae.Model, error) {
	config, err := e.Catalog(ctx, auth, refresh)
	if err != nil {
		return nil, err
	}
	return ae.Models(config), nil
}

func AntigravityEnterpriseModels(config ae.Config) []*registry.ModelInfo {
	return helps.AntigravityEnterpriseModels(config)
}

func antigravityEnterpriseLocalError(err error) error {
	var failure *ae.Error
	if errors.As(err, &failure) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &ae.Error{HTTP: 400, Code: "invalid_request"}
}

var _ coreauth.ProviderExecutor = (*AntigravityEnterpriseExecutor)(nil)
