package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	nativeWebStreamPath    = "/v1beta/models/gemini-3.8-flash:streamGenerateContent"
	nativeWebSessionRoot   = "collections/default_collection/engines/synthetic/sessions/"
	nativeWebRotatedCookie = "__Secure-1PSIDTS=synthetic-concurrent-renewal"
	nativeWebFixtureHeader = "X-Web-Fixture-Request"
)

type nativeWebGate struct {
	entered chan struct{}
	release chan struct{}
}

func newNativeWebGate() *nativeWebGate {
	return &nativeWebGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *nativeWebGate) wait(ctx context.Context) error {
	close(g.entered)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type nativeWebSubmission struct {
	session string
	rotated bool
}

type nativeWebUpload struct {
	mime string
	data string
}

type nativeWebUpstream struct {
	bootstraps  atomic.Int32
	sessions    atomic.Int32
	generations atomic.Int32
	submitted   chan nativeWebSubmission
	uploaded    chan nativeWebUpload
	finish      chan struct{}
	preparing   chan struct{}
	firstUpload *nativeWebGate
	rotation    *nativeWebGate
}

func newNativeWebUpstream() *nativeWebUpstream {
	return &nativeWebUpstream{
		submitted: make(chan nativeWebSubmission, 8),
		uploaded:  make(chan nativeWebUpload, 8),
		finish:    make(chan struct{}),
	}
}

func (u *nativeWebUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	reply := func(body string) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
	}
	cookie := req.Header.Get("Cookie")
	if !strings.Contains(cookie, "SID=synthetic-session") {
		return nil, fmt.Errorf("unexpected test credential")
	}
	if req.URL.Host == "accounts.google.com" && req.URL.Path == "/RotateCookies" && u.rotation != nil {
		if u.generations.Load() != 1 {
			return nil, fmt.Errorf("renewal crossed the old request's submission boundary")
		}
		if err := u.rotation.wait(req.Context()); err != nil {
			return nil, err
		}
		res := reply(`{}`)
		res.Header.Set("Set-Cookie", nativeWebRotatedCookie+"; Domain=.google.com; Path=/; Secure; Max-Age=3600")
		return res, nil
	}
	if req.URL.Host == "vertexaisearch.cloud.google.com" && req.Method == http.MethodGet {
		u.bootstraps.Add(1)
		return reply(`window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`), nil
	}
	if req.URL.Host != "ca-discoveryengine.clients6.google.com" {
		return nil, fmt.Errorf("unexpected upstream host %s", req.URL.Host)
	}
	action := path.Base(req.URL.Path)
	switch action {
	case "lookupWidgetConfig":
		return reply(phaseOnePlusConfig), nil
	case "widgetCreateSession":
		id := u.sessions.Add(1)
		if u.preparing != nil {
			if id == 4 {
				close(u.preparing)
			}
			select {
			case <-u.preparing:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		session := fmt.Sprintf("%ssession-%d", nativeWebSessionRoot, id)
		return reply(fmt.Sprintf(`{"session":{"name":%q}}`, session)), nil
	case "widgetStreamAssist":
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		session := gjson.GetBytes(body, "streamAssistRequest.session").String()
		ids := gjson.GetBytes(body, "streamAssistRequest.fileIds").Array()
		if !strings.HasPrefix(session, nativeWebSessionRoot) || len(ids) != 1 || ids[0].String() != "file-"+path.Base(session) {
			return nil, fmt.Errorf("generation did not use its own session attachment")
		}
		u.generations.Add(1)
		u.submitted <- nativeWebSubmission{session: session, rotated: strings.Contains(cookie, nativeWebRotatedCookie)}
		res := reply("")
		res.Body = u.openStream(req.Context())
		return res, nil
	}
	session, upload := strings.CutSuffix(action, ":uploadFile")
	if !upload || !strings.HasPrefix(session, "session-") {
		return nil, fmt.Errorf("unexpected Web upstream action %s", action)
	}
	if req.Header.Get("X-Goog-Upload-Command") == "start" {
		res := reply(`{}`)
		res.Header.Set("X-Goog-Upload-URL", req.URL.String()+"?upload_id=synthetic")
		return res, nil
	}
	if u.firstUpload != nil && session == "session-1" {
		if err := u.firstUpload.wait(req.Context()); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.uploaded <- nativeWebUpload{mime: req.Header.Get("Content-Type"), data: string(data)}
	return reply(fmt.Sprintf(`{"fileId":%q}`, "file-"+session)), nil
}

func (u *nativeWebUpstream) openStream(ctx context.Context) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = writer.Close() }()
		if _, err := io.WriteString(writer, `[{"streamAssistResponse":{"answer":{"state":"IN_PROGRESS","replies":[{"groundedContent":{"content":{"text":"hello"}}}]}}}`); err != nil {
			return
		}
		select {
		case <-u.finish:
			_, _ = io.WriteString(writer, `,{"streamAssistResponse":{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"content":{"text":"hello world"}}}]}}}]`)
		case <-ctx.Done():
		}
	}()
	return reader
}

type nativeWebObservingExecutor struct {
	*GeminiEnterpriseWebExecutor
	selected chan web.Credential
}

func (e *nativeWebObservingExecutor) ExecuteStream(ctx context.Context, a *auth.Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	credential, err := web.CredentialFrom(a.Metadata)
	if err != nil {
		return nil, err
	}
	e.selected <- credential
	return e.GeminiEnterpriseWebExecutor.ExecuteStream(ctx, a, req, opts)
}

type nativeWebClient struct {
	ctx     context.Context
	server  *httptest.Server
	handled chan string
}

func newNativeWebClient(t *testing.T, budget int64, upstream *nativeWebUpstream) (*auth.Manager, *GeminiEnterpriseWebExecutor, *nativeWebClient) {
	t.Helper()
	m, _, e, _, _ := phaseOneManager(t)
	cfg := e.cfg.CloneForRuntime()
	cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
	cfg.GeminiEnterpriseWeb.MaxConcurrent = 4
	cfg.GeminiEnterpriseWeb.MaxInflightBytes = budget
	m.SetConfig(cfg)
	e = NewGeminiEnterpriseWebExecutor(cfg, m.WebRuntime())
	m.RegisterExecutor(e)
	router := gin.New()
	router.Use(middleware.EnterpriseWebBodyAdmission(m))
	h := geminihandlers.NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m))
	router.POST("/v1beta/models/*action", h.GeminiHandler)
	handled := make(chan string, 12)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), "cliproxy.roundtripper", upstream)))
		handled <- r.Header.Get(nativeWebFixtureHeader)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(func() {
		cancel()
		server.Close()
	})
	return m, e, &nativeWebClient{ctx: ctx, server: server, handled: handled}
}

type nativeWebResponse struct {
	res *http.Response
	err error
}

func (c *nativeWebClient) post(ctx context.Context, label string, payload []byte) <-chan nativeWebResponse {
	result := make(chan nativeWebResponse, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server.URL+nativeWebStreamPath, bytes.NewReader(payload))
		if err != nil {
			result <- nativeWebResponse{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(nativeWebFixtureHeader, label)
		res, err := c.server.Client().Do(req)
		result <- nativeWebResponse{res: res, err: err}
	}()
	return result
}

func awaitNativeWeb[T any](t *testing.T, ctx context.Context, ch <-chan T, stage string) T {
	t.Helper()
	select {
	case event := <-ch:
		return event
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", stage, ctx.Err())
		var zero T
		return zero
	}
}

func nativeWebResponseStatus(t *testing.T, ctx context.Context, result <-chan nativeWebResponse, status int) *http.Response {
	t.Helper()
	received := awaitNativeWeb(t, ctx, result, "native response headers")
	if received.err != nil {
		t.Fatal(received.err)
	}
	res := received.res
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != status {
		var body []byte
		if res.StatusCode >= 400 {
			body, _ = io.ReadAll(res.Body)
		}
		t.Fatalf("native status = %d, want %d: %s", res.StatusCode, status, body)
	}
	return res
}

func requireNativeWebTerminal(t *testing.T, res *http.Response) {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil || !strings.Contains(string(body), `"finishReason":"STOP"`) {
		t.Fatalf("stream did not finish successfully: %s, %v", body, err)
	}
}

func requireNativeWebReleased(t *testing.T, m *auth.Manager) {
	t.Helper()
	status := m.WebRuntime().Status()
	if status["active_requests"] != 0 || status["inflight_bytes"] != int64(0) {
		t.Fatal("Web request leases were not released", status)
	}
}

func TestEnterpriseWebNativeSameCredentialGlobalConcurrency(t *testing.T) {
	upstream := newNativeWebUpstream()
	// Block session creation until all four preparations arrive, before any stream opens.
	upstream.preparing = make(chan struct{})
	m, _, client := newNativeWebClient(t, 256<<20, upstream)
	payload := phaseOneRequest("image/png").Payload
	responses := make([]<-chan nativeWebResponse, 4)
	cancelCtx, cancel := context.WithCancel(client.ctx)
	defer cancel()
	responses[0] = client.post(cancelCtx, "cancelled", payload)
	for i := 1; i < len(responses); i++ {
		responses[i] = client.post(client.ctx, fmt.Sprintf("stream-%d", i), payload)
	}
	streams := make([]*http.Response, 0, len(responses))
	for _, response := range responses {
		streams = append(streams, nativeWebResponseStatus(t, client.ctx, response, http.StatusOK))
	}
	sessions := make(map[string]bool, 4)
	for range responses {
		submitted := awaitNativeWeb(t, client.ctx, upstream.submitted, "independent generation session")
		if sessions[submitted.session] {
			t.Fatal("requests shared a generation session", submitted.session)
		}
		sessions[submitted.session] = true
	}
	if status := m.WebRuntime().Status(); status["active_requests"] != 4 || upstream.bootstraps.Load() != 1 {
		t.Fatal("same-credential streams did not share authentication and overlap", status, upstream.bootstraps.Load())
	}
	fifth := nativeWebResponseStatus(t, client.ctx, client.post(client.ctx, "fifth", payload), http.StatusTooManyRequests)
	body, err := io.ReadAll(fifth.Body)
	if err != nil || !strings.Contains(string(body), "Media concurrency capacity is exhausted") || upstream.sessions.Load() != 4 {
		t.Fatalf("expected a global admission rejection before upstream work: %s, %v", body, err)
	}
	if got := awaitNativeWeb(t, client.ctx, client.handled, "rejected fifth handler"); got != "fifth" {
		t.Fatalf("handler ended early: %s", got)
	}
	cancel()
	if got := awaitNativeWeb(t, client.ctx, client.handled, "cancelled handler"); got != "cancelled" || m.WebRuntime().Status()["active_requests"] != 3 {
		t.Fatalf("cancellation did not release exactly one request: %s, %v", got, m.WebRuntime().Status())
	}
	_ = streams[0].Body.Close()
	replacement := nativeWebResponseStatus(t, client.ctx, client.post(client.ctx, "replacement", payload), http.StatusOK)
	if m.WebRuntime().Status()["active_requests"] != 4 || upstream.sessions.Load() != 5 {
		t.Fatal("released slot was not reusable", m.WebRuntime().Status())
	}
	close(upstream.finish)
	for _, stream := range append(streams[1:], replacement) {
		requireNativeWebTerminal(t, stream)
		awaitNativeWeb(t, client.ctx, client.handled, "stream completion")
	}
	requireNativeWebReleased(t, m)
}

func TestEnterpriseWebNativeBufferBudgetLimitsConcurrency(t *testing.T) {
	upstream := newNativeWebUpstream()
	m, _, client := newNativeWebClient(t, 64<<20, upstream)
	payload := phaseOneRequest("image/png").Payload
	first := nativeWebResponseStatus(t, client.ctx, client.post(client.ctx, "first", payload), http.StatusOK)
	second := nativeWebResponseStatus(t, client.ctx, client.post(client.ctx, "second", payload), http.StatusTooManyRequests)
	body, err := io.ReadAll(second.Body)
	if status := m.WebRuntime().Status(); err != nil || !strings.Contains(string(body), "Media buffer budget is exhausted") || status["rejected_memory"] != uint64(1) || status["active_requests"] != 1 || upstream.sessions.Load() != 1 {
		t.Fatalf("expected buffer admission rejection before upstream work: %s, %v, %v", body, err, status)
	}
	awaitNativeWeb(t, client.ctx, client.handled, "memory-rejected handler")
	close(upstream.finish)
	requireNativeWebTerminal(t, first)
	awaitNativeWeb(t, client.ctx, client.handled, "first handler")
	requireNativeWebReleased(t, m)
}

func TestEnterpriseWebNativeRenewalPreservesOpenStreams(t *testing.T) {
	upstream := newNativeWebUpstream()
	upstream.firstUpload = newNativeWebGate()
	upstream.rotation = newNativeWebGate()
	m, e, client := newNativeWebClient(t, 256<<20, upstream)
	selected := make(chan web.Credential, 2)
	m.RegisterExecutor(&nativeWebObservingExecutor{GeminiEnterpriseWebExecutor: e, selected: selected})
	payload := phaseOneRequest("image/png").Payload
	firstResult := client.post(client.ctx, "old", payload)
	old := awaitNativeWeb(t, client.ctx, selected, "first selected credential")
	awaitNativeWeb(t, client.ctx, upstream.firstUpload.entered, "old upload")
	a, ok := m.GetByID("web-phase-one-cookie")
	if !ok {
		t.Fatal("test credential disappeared")
	}
	checkCtx := context.WithValue(client.ctx, "cliproxy.roundtripper", upstream)
	checking := make(chan struct{})
	checked := make(chan bool, 1)
	go func() {
		close(checking)
		checked <- e.CheckCookie(checkCtx, a)
	}()
	awaitNativeWeb(t, client.ctx, checking, "scheduled check attempt")
	close(upstream.firstUpload.release)
	first := nativeWebResponseStatus(t, client.ctx, firstResult, http.StatusOK)
	if submitted := awaitNativeWeb(t, client.ctx, upstream.submitted, "old generation"); submitted.rotated {
		t.Fatal("in-flight upload lost its original snapshot")
	}
	awaitNativeWeb(t, client.ctx, upstream.rotation.entered, "renewal while old stream is open")
	cancelCtx, cancel := context.WithCancel(client.ctx)
	defer cancel()
	aborted := client.post(cancelCtx, "cancel-waiting", payload)
	awaitNativeWeb(t, client.ctx, selected, "selection before cancellation during renewal")
	cancel()
	if result := awaitNativeWeb(t, client.ctx, aborted, "cancelled waiting response"); !errors.Is(result.err, context.Canceled) {
		t.Fatal("request was not cancelled while awaiting renewal", result.err)
	}
	if got := awaitNativeWeb(t, client.ctx, client.handled, "cancelled waiting handler"); got != "cancel-waiting" || m.WebRuntime().Status()["active_requests"] != 1 || upstream.sessions.Load() != 1 {
		t.Fatalf("cancelled waiter retained a slot or started a session: %s, %v", got, m.WebRuntime().Status())
	}
	secondResult := client.post(client.ctx, "new", payload)
	if queued := awaitNativeWeb(t, client.ctx, selected, "selection during renewal"); queued.Revision != old.Revision || queued.Fingerprint() != old.Fingerprint() {
		t.Fatal("test did not select the pre-renewal snapshot")
	}
	close(upstream.rotation.release)
	if !awaitNativeWeb(t, client.ctx, checked, "completed Cookie check") {
		t.Fatal("scheduled check did not complete")
	}
	second := nativeWebResponseStatus(t, client.ctx, secondResult, http.StatusOK)
	if submitted := awaitNativeWeb(t, client.ctx, upstream.submitted, "generation using renewed Cookie"); !submitted.rotated {
		t.Fatal("queued request sent the stale snapshot")
	}
	current, ok := m.GetByID(a.ID)
	if !ok {
		t.Fatal("test credential disappeared after renewal")
	}
	renewed, err := web.CredentialFrom(current.Metadata)
	if err != nil || renewed.Revision != old.Revision+1 || renewed.Policy() != old.Policy() || !strings.Contains(renewed.Cookies, nativeWebRotatedCookie) {
		t.Fatal("verified renewal did not publish the expected revision and policy", err)
	}
	if status := m.WebRuntime().Status(); status["active_requests"] != 2 || upstream.bootstraps.Load() != 2 {
		t.Fatal("renewal disrupted streaming or reverified an already renewed snapshot", status, upstream.bootstraps.Load())
	}
	close(upstream.finish)
	for _, stream := range []*http.Response{first, second} {
		requireNativeWebTerminal(t, stream)
		awaitNativeWeb(t, client.ctx, client.handled, "stream completion after renewal")
	}
	requireNativeWebReleased(t, m)
}

func TestEnterpriseWebNativeNormalizesAttachmentMIME(t *testing.T) {
	upstream := newNativeWebUpstream()
	m, _, client := newNativeWebClient(t, 256<<20, upstream)
	close(upstream.finish)
	for _, tc := range []struct {
		mime, want string
	}{
		{mime: "image/jpg", want: "image/jpeg"},
		{mime: "text/plain; charset=UTF-8", want: "text/plain"},
	} {
		payload := phaseOneRequest(tc.mime).Payload
		res := nativeWebResponseStatus(t, client.ctx, client.post(client.ctx, tc.mime, payload), http.StatusOK)
		requireNativeWebTerminal(t, res)
		awaitNativeWeb(t, client.ctx, client.handled, "MIME-normalized handler")
		if got := awaitNativeWeb(t, client.ctx, upstream.uploaded, "MIME-normalized upload"); got.mime != tc.want || got.data != "input" {
			t.Fatalf("upload for %q changed the bytes or used the wrong MIME: %+v", tc.mime, got)
		}
	}
	requireNativeWebReleased(t, m)
}
