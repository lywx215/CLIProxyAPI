package helps

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// GeminiBudgetHTTPClient must be called before diagnostics and usage transport
// wrappers so its clone can disable transparent retries on the concrete transport.
// It inspects the final business payload without modifying it.
func GeminiBudgetHTTPClient(ctx context.Context, client *http.Client, cfg *config.Config, auth *cliproxyauth.Auth, requestedAlias string) *http.Client {
	if cfg == nil || !cfg.GeminiFreeTierPolicy.Enabled || !cliproxyauth.IsOfficialGeminiAuth(auth) {
		return client
	}
	if client == nil {
		client = &http.Client{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	clone := *client
	transport := clone.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if concrete, ok := transport.(*http.Transport); ok {
		fresh := concrete.Clone()
		fresh.DisableKeepAlives = true
		fresh.ForceAttemptHTTP2 = false
		fresh.TLSNextProto = nil
		fresh.Protocols = new(http.Protocols)
		fresh.Protocols.SetHTTP1(true)
		if fresh.TLSClientConfig == nil {
			fresh.TLSClientConfig = &tls.Config{}
		} else {
			fresh.TLSClientConfig = fresh.TLSClientConfig.Clone()
		}
		fresh.TLSClientConfig.NextProtos = []string{"http/1.1"}
		transport = fresh
	}
	clone.Transport = &geminiBudgetTransport{
		base: transport, ctx: budget.WithRequest(ctx), auth: auth,
		models: cfg.GeminiFreeTierPolicy.WithDefaults().ManagedModels, alias: requestedAlias,
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

type geminiBudgetTransport struct {
	base   http.RoundTripper
	ctx    context.Context
	auth   *cliproxyauth.Auth
	models map[string]int
	alias  string
}

// Cancellation belongs to the concrete HTTP request; request-wide counters and
// the ledger can fall back to the enclosing executor context.
type geminiBudgetContext struct {
	context.Context
	fallback context.Context
}

func (c geminiBudgetContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.fallback.Value(key)
}

func (t *geminiBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	model, generation, err := geminiBudgetRequestModel(req)
	if err != nil {
		return geminiBudgetErrorResponse(req, http.StatusBadRequest, "The generation request could not be validated.", t.alias, 0), nil
	}
	if !officialGeminiURL(req.URL) {
		return t.base.RoundTrip(req)
	}
	model = cliproxyauth.CanonicalBudgetModel(model)
	ctx := geminiBudgetContext{Context: req.Context(), fallback: t.ctx}
	if _, managed := t.models[model]; !generation || !managed {
		// Unlisted generation shares the channel send limit without spending RPD.
		// Auxiliary Google calls also sanitize errors, without spending generation quota.
		if generation {
			if errStart := budget.StartUnmeteredTransmission(ctx, cliproxyauth.GeminiBudgetScope(t.auth, model)); errStart != nil {
				return geminiBudgetAdmissionResponse(req, errStart, t.alias), nil
			}
		}
		resp, errSend := t.base.RoundTrip(req)
		if errSend != nil || resp == nil {
			return geminiBudgetErrorResponse(req, 503, "The upstream connection could not be completed.", t.alias, 0), nil
		}
		if resp.Body == nil {
			resp.Body = io.NopCloser(strings.NewReader(""))
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return geminiBudgetErrorResponse(req, resp.StatusCode, geminiBudgetMessage(resp.StatusCode), t.alias, geminiBudgetRetryDelay(resp.Header, raw)), nil
		}
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") || strings.HasSuffix(req.URL.Path, ":streamGenerateContent") {
			resp.Body = &geminiBudgetStreamBody{ReadCloser: resp.Body, reader: bufio.NewReader(resp.Body), release: func() {}, alias: t.alias, observe: func(int, []byte) {}}
		}
		return resp, nil
	}
	engine := budget.EngineFromContext(ctx)
	if engine == nil {
		return geminiBudgetErrorResponse(req, http.StatusServiceUnavailable, "Request quota state is temporarily unavailable.", t.alias, 0), nil
	}
	if err = req.Context().Err(); err != nil {
		return geminiBudgetErrorResponse(req, http.StatusServiceUnavailable, "The request was cancelled before transmission.", t.alias, 0), nil
	}
	scope := cliproxyauth.GeminiBudgetScope(t.auth, model)
	ticket, err := engine.Reserve(ctx, scope)
	if err != nil {
		return geminiBudgetAdmissionResponse(req, err, t.alias), nil
	}
	if err = ticket.TryStartTransmission(ctx); err != nil {
		_ = ticket.Compensate(context.WithoutCancel(ctx))
		ticket.Release()
		return geminiBudgetAdmissionResponse(req, err, t.alias), nil
	}
	// The one-way transmission flag is already set before delegating to any
	// network transport. Errors from this point cannot refund the reservation.
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		ticket.Release()
		return geminiBudgetErrorResponse(req, http.StatusServiceUnavailable, "The upstream connection could not be completed.", t.alias, 0), nil
	}
	if resp == nil {
		ticket.Release()
		return geminiBudgetErrorResponse(req, http.StatusServiceUnavailable, "The upstream response is unavailable.", t.alias, 0), nil
	}
	if resp.Body == nil {
		resp.Body = io.NopCloser(strings.NewReader(""))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		ticket.Release()
		signal := geminiBudgetSignal(resp.StatusCode, resp.Header, body)
		signal.AttemptID = ticket.ID()
		if signal.Kind != "" {
			_ = engine.Observe(context.WithoutCancel(ctx), scope, signal)
		}
		delay := signal.RetryAfter
		if view := engine.View(scope); !view.RetryAt.IsZero() {
			delay = max(delay, time.Until(view.RetryAt))
		}
		return geminiBudgetErrorResponse(req, resp.StatusCode, geminiBudgetMessage(resp.StatusCode), t.alias, delay), nil
	}
	onSuccess := func() {
		_ = engine.Observe(context.WithoutCancel(ctx), scope, budget.Signal{Kind: budget.SignalSuccess, AttemptID: ticket.ID()})
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") || strings.HasSuffix(req.URL.Path, ":streamGenerateContent") {
		resp.Body = &geminiBudgetStreamBody{
			ReadCloser: resp.Body, reader: bufio.NewReader(resp.Body), release: ticket.Release,
			alias: t.alias, onSuccess: onSuccess, observe: func(status int, raw []byte) {
				signal := geminiBudgetSignal(status, resp.Header, raw)
				signal.AttemptID = ticket.ID()
				if signal.Kind != "" {
					_ = engine.Observe(context.WithoutCancel(ctx), scope, signal)
				}
			},
		}
	} else {
		resp.Body = &geminiBudgetReleaseBody{ReadCloser: resp.Body, release: ticket.Release, onSuccess: onSuccess}
	}
	return resp, nil
}

func officialGeminiURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && strings.EqualFold(u.Host, "generativelanguage.googleapis.com") && u.User == nil
}

func geminiBudgetRequestModel(req *http.Request) (string, bool, error) {
	if req == nil || req.URL == nil || req.Method != http.MethodPost {
		return "", false, nil
	}
	if index := strings.LastIndex(req.URL.Path, "/models/"); index >= 0 {
		modelAction := req.URL.Path[index+len("/models/"):]
		action := strings.LastIndexByte(modelAction, ':')
		if action < 0 {
			return "", false, nil
		}
		if modelAction[action+1:] != "generateContent" && modelAction[action+1:] != "streamGenerateContent" {
			return "", false, nil
		}
		return modelAction[:action], true, nil
	}
	if !strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/interactions") {
		return "", false, nil
	}
	if req.GetBody == nil {
		return "", true, errors.New("cannot inspect final interactions body")
	}
	body, err := req.GetBody()
	if err != nil {
		return "", true, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		return "", true, err
	}
	model := gjson.GetBytes(data, "model").String()
	if model == "" {
		return "", true, errors.New("interactions body has no actual model")
	}
	return model, true, nil
}

func geminiBudgetAdmissionResponse(req *http.Request, err error, alias string) *http.Response {
	status := http.StatusServiceUnavailable
	delay := time.Duration(0)
	var statusError interface{ StatusCode() int }
	if errors.As(err, &statusError) {
		status = statusError.StatusCode()
	}
	var retryError interface{ RetryAfter() *time.Duration }
	if errors.As(err, &retryError) {
		if retry := retryError.RetryAfter(); retry != nil {
			delay = *retry
		}
	}
	return geminiBudgetErrorResponse(req, status, geminiBudgetMessage(status), alias, delay)
}

func geminiBudgetMessage(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "Request quota is temporarily unavailable."
	case http.StatusBadRequest:
		return "The generation request could not be processed."
	case http.StatusUnauthorized, http.StatusForbidden:
		return "The upstream credential requires administrator attention."
	case http.StatusNotFound:
		return "The requested model is currently unavailable."
	default:
		return "The upstream service is temporarily unavailable."
	}
}

func geminiBudgetErrorResponse(req *http.Request, status int, message, alias string, delay time.Duration) *http.Response {
	body := geminiBudgetErrorJSON(status, message, alias)
	header := http.Header{"Content-Type": {"application/json"}}
	if delay > 0 {
		header.Set("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
	}
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req,
	}
}

func geminiBudgetErrorJSON(status int, message, alias string) []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": status, "message": message, "model": alias,
	}})
	return body
}

func geminiBudgetSignal(status int, headers http.Header, body []byte) budget.Signal {
	signal := budget.Signal{RetryAfter: geminiBudgetRetryDelay(headers, body)}
	details := gjson.GetBytes(body, "error.details")
	if !details.Exists() {
		details = gjson.GetBytes(body, "details")
	}
	reason := ""
	daily, zero, minuteRequests, minuteTokens := false, false, false, false
	classifyQuota := func(quota string) {
		quota = strings.ToLower(quota)
		if strings.Contains(quota, "perday") || strings.Contains(quota, "per_day") || strings.Contains(quota, "requests per day") {
			daily = true
		}
		if strings.Contains(quota, "perminute") || strings.Contains(quota, "per_minute") || strings.Contains(quota, "per minute") {
			if strings.Contains(quota, "token") {
				minuteTokens = true
			} else if strings.Contains(quota, "request") {
				minuteRequests = true
			}
		}
	}
	details.ForEach(func(_, detail gjson.Result) bool {
		switch {
		case strings.HasSuffix(detail.Get("@type").String(), "QuotaFailure"):
			detail.Get("violations").ForEach(func(_, violation gjson.Result) bool {
				quota := strings.ToLower(violation.Get("quotaId").String() + " " + violation.Get("quotaMetric").String() + " " + violation.Get("description").String())
				classifyQuota(quota)
				if strings.Contains(quota, "perday") || strings.Contains(quota, "per_day") || strings.Contains(quota, "requests per day") {
					daily = true
					if value := violation.Get("quotaValue"); value.Exists() {
						limit, err := strconv.Atoi(value.String())
						if err == nil && limit >= 0 {
							signal.Limit = limit
							zero = limit == 0
						}
					}
				}
				return true
			})
		case strings.HasSuffix(detail.Get("@type").String(), "ErrorInfo"):
			reason = strings.ToUpper(detail.Get("reason").String())
			detail.Get("metadata").ForEach(func(key, value gjson.Result) bool {
				name := strings.ToLower(key.String())
				if strings.Contains(name, "quota") || strings.Contains(name, "limit") || strings.Contains(name, "metric") {
					classifyQuota(value.String())
				}
				return true
			})
		}
		return true
	})
	switch {
	case reason == "API_KEY_INVALID" || reason == "API_KEY_EXPIRED" || reason == "API_KEY_REVOKED" || reason == "API_KEY_LEAKED":
		signal.Kind = budget.SignalCredentialInvalid
	case reason == "MODEL_NOT_FOUND" || reason == "MODEL_PERMISSION_DENIED" || zero || (status == http.StatusNotFound && gjson.GetBytes(body, "error.status").String() == "NOT_FOUND"):
		signal.Kind = budget.SignalModelUnsupported
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		signal.Kind = budget.SignalTemporaryAuth
	case status == http.StatusTooManyRequests && daily:
		signal.Kind = budget.SignalRPD
	case status == http.StatusTooManyRequests && minuteTokens:
		signal.Kind = budget.SignalTPM
	case status == http.StatusTooManyRequests && minuteRequests:
		signal.Kind = budget.SignalRPM
	case status == http.StatusTooManyRequests:
		signal.Kind = budget.SignalUnknown429
	case status >= 500:
		signal.Kind = budget.Signal503
	}
	return signal
}

func geminiBudgetRetryDelay(header http.Header, body []byte) time.Duration {
	delay := time.Duration(0)
	if value := header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= 86400*365 {
			delay = time.Duration(seconds) * time.Second
		} else if date, errDate := http.ParseTime(value); errDate == nil {
			delay = time.Until(date)
		}
	}
	details := gjson.GetBytes(body, "error.details")
	if !details.Exists() {
		details = gjson.GetBytes(body, "details")
	}
	details.ForEach(func(_, detail gjson.Result) bool {
		if strings.HasSuffix(detail.Get("@type").String(), "RetryInfo") {
			if retry, err := time.ParseDuration(detail.Get("retryDelay").String()); err == nil && retry > delay {
				delay = retry
			}
		}
		return true
	})
	if delay < 0 {
		return 0
	}
	return delay
}

// GeminiBudgetRetryAfter preserves the safe header after conversion to executor errors.
func GeminiBudgetRetryAfter(header http.Header) *time.Duration {
	delay := geminiBudgetRetryDelay(header, nil)
	if delay <= 0 {
		return nil
	}
	return &delay
}

type geminiBudgetReleaseBody struct {
	io.ReadCloser
	release   func()
	onSuccess func()
	once      sync.Once
}

func (b *geminiBudgetReleaseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(func() {
			if err == io.EOF && b.onSuccess != nil {
				b.onSuccess()
			}
			b.release()
		})
	}
	return n, err
}

func (b *geminiBudgetReleaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

type geminiBudgetStreamBody struct {
	io.ReadCloser
	reader    *bufio.Reader
	pending   []byte
	release   func()
	once      sync.Once
	alias     string
	observe   func(int, []byte)
	onSuccess func()
	failed    bool
	eof       error
}

func (b *geminiBudgetStreamBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.eof != nil {
			b.once.Do(func() {
				if b.eof == io.EOF && !b.failed && b.onSuccess != nil {
					b.onSuccess()
				}
				b.release()
			})
			return 0, b.eof
		}
		var frame, data []byte
		eventError := false
		for {
			line, err := b.reader.ReadBytes('\n')
			frame = append(frame, line...)
			b.eof = err
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("event:")) {
				eventError = strings.EqualFold(strings.TrimSpace(string(bytes.TrimPrefix(trimmed, []byte("event:")))), "error")
			}
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))...)
			}
			if err != nil || len(trimmed) == 0 {
				break
			}
		}
		if eventError || gjson.GetBytes(data, "error").Exists() || gjson.GetBytes(data, "type").String() == "error" {
			b.failed = true
			status := int(gjson.GetBytes(data, "error.code").Int())
			if status < 400 || status > 599 {
				status = http.StatusServiceUnavailable
			}
			b.observe(status, data)
			frame = append([]byte("data: "), geminiBudgetErrorJSON(status, geminiBudgetMessage(status), b.alias)...)
			frame = append(frame, '\n', '\n')
		}
		b.pending = frame
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *geminiBudgetStreamBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
