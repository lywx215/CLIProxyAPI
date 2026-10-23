package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

type enterpriseTransport func(*http.Request) (*http.Response, error)

func (f enterpriseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func enterpriseExecutorAuth() *coreauth.Auth {
	return &coreauth.Auth{ID: "enterprise-executor", Provider: ge.Provider, Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "own-client", ProjectID: "project", EngineID: "engine"}, "test@example.com", &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)})}
}
func enterpriseOptions(format translator.Format) exec.Options {
	op := "chat_completions"
	if format == translator.FormatGemini {
		op = "generate_content"
	}
	return exec.Options{SourceFormat: format, Metadata: map[string]any{exec.OperationMetadataKey: op}}
}

const enterpriseAnswer = `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"hello"}}]},"assistToken":"private","sessionInfo":{"id":"private"}},{"answer":{"state":"SUCCEEDED"}}]`

func TestEnterpriseExecutorBothProtocolsAndStreams(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro", "future-enterprise-model"} {
		for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
			for _, stream := range []bool{false, true} {
				t.Run(model+"/"+format.String()+map[bool]string{true: "-stream", false: "-full"}[stream], func(t *testing.T) {
					transport := enterpriseTransport(func(req *http.Request) (*http.Response, error) {
						if _, ok := req.Context().Deadline(); ok {
							t.Error("inference has a deadline")
						}
						if req.Header.Get("Authorization") != "Bearer access" || req.URL.Host != "discoveryengine.googleapis.com" || !strings.HasSuffix(req.URL.Path, "/assistants/default_assistant:streamAssist") {
							t.Error("incorrect authorized upstream request")
						}
						body, _ := io.ReadAll(req.Body)
						if gjson.GetBytes(body, "generationSpec.modelId").String() != model || gjson.GetBytes(body, "session").Exists() {
							t.Error("incorrect model or session reuse")
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
					})
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
					body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
					if format == translator.FormatGemini {
						body = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
					}
					req := exec.Request{Model: model, Payload: body}
					opts := enterpriseOptions(format)
					e := NewGeminiEnterpriseExecutor(&config.Config{})
					var output []byte
					if stream {
						result, err := e.ExecuteStream(ctx, enterpriseExecutorAuth(), req, opts)
						if err != nil {
							t.Fatal(err)
						}
						first := true
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							if first && !strings.Contains(string(chunk.Payload), "hello") {
								t.Fatal("first chunk was not visible")
							}
							first = false
							output = append(output, chunk.Payload...)
						}
					} else {
						result, err := e.Execute(ctx, enterpriseExecutorAuth(), req, opts)
						if err != nil {
							t.Fatal(err)
						}
						output = result.Payload
					}
					for _, bad := range []string{"private", "searchQueries", "urlRetrievalStatus", "estimated", "usage_source"} {
						if strings.Contains(string(output), bad) {
							t.Fatalf("fabricated/leaked %s", bad)
						}
					}
					if !strings.Contains(string(output), "hello") {
						t.Fatal("lost output")
					}
					if !stream || format == translator.FormatGemini {
						if !strings.Contains(string(output), "TokenCount") && !strings.Contains(string(output), "prompt_tokens") {
							t.Fatal("missing usage")
						}
					}
				})
			}
		}
	}
}

func TestEnterprisePayloadFinalBarrier(t *testing.T) {
	modelRules := []config.PayloadModelRule{{Name: "gemini-4-argon", Protocol: "gemini-enterprise"}}
	for _, stream := range []bool{false, true} {
		for _, filter := range []string{"toolsSpec", "generationSpec.modelId", ""} {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: modelRules, Params: map[string]any{"generationSpec.modelId": "configured-model", "query.text": "configured-query"}}}}}
			if filter != "" {
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: modelRules, Params: []string{filter}}}
			}
			var calls int
			transport := enterpriseTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				body, _ := io.ReadAll(req.Body)
				if gjson.GetBytes(body, "generationSpec.modelId").String() != "configured-model" || gjson.GetBytes(body, "query.text").String() != "configured-query" {
					t.Fatal("built-in normalization overwrote user payload")
				}
				if filter == "toolsSpec" && gjson.GetBytes(body, "toolsSpec").Exists() {
					t.Fatal("filtered tool restored")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			req := exec.Request{Model: "gemini-4-argon", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"google_search":{}}]}`)}
			e := NewGeminiEnterpriseExecutor(cfg)
			opts := enterpriseOptions(translator.FormatOpenAI)
			var err error
			if stream {
				var result *exec.StreamResult
				result, err = e.ExecuteStream(ctx, enterpriseExecutorAuth(), req, opts)
				if err == nil {
					for c := range result.Chunks {
						if c.Err != nil {
							err = c.Err
						}
					}
				}
			} else {
				_, err = e.Execute(ctx, enterpriseExecutorAuth(), req, opts)
			}
			if filter == "generationSpec.modelId" {
				if err == nil || calls != 0 {
					t.Fatal("invalid filtered model was repaired or sent")
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		}
	}
}

func TestEnterpriseRejectsOperationsAndCredentialRedirects(t *testing.T) {
	e := NewGeminiEnterpriseExecutor(&config.Config{})
	auth := enterpriseExecutorAuth()
	var calls atomic.Int32
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://example.com/steal"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	for _, target := range []string{"http://discoveryengine.googleapis.com/x", "https://example.com/x", "https://discoveryengine.googleapis.com.evil/x"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		if _, err := e.HttpRequest(ctx, auth, req); err == nil {
			t.Fatal("credentials allowed on foreign origin")
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", ge.Origin+"/v1alpha/anything", nil)
	resp, err := e.HttpRequest(ctx, auth, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls.Load() != 1 || resp.StatusCode != 302 {
		t.Fatal("followed credential redirect")
	}
	for _, op := range []string{"", "completions", "responses", "count_tokens", "interactions"} {
		opts := enterpriseOptions(translator.FormatOpenAI)
		opts.Metadata[exec.OperationMetadataKey] = op
		_, err := e.Execute(ctx, auth, exec.Request{}, opts)
		if err == nil {
			t.Fatal("unsupported operation sent")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unsupported operation used upstream")
	}
}

func TestEnterpriseThoughtAndVisibleFailuresAreRequestScoped(t *testing.T) {
	for _, thought := range []bool{true, false} {
		for _, stream := range []bool{true, false} {
			body := `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"started","thought":` + map[bool]string{true: "true", false: "false"}[thought] + `}}]}}{"error":{"message":"failure"}}`
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			e := NewGeminiEnterpriseExecutor(&config.Config{})
			req := exec.Request{Model: "gemini-4-argon", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
			opts := enterpriseOptions(translator.FormatOpenAI)
			var err error
			if stream {
				result, errStart := e.ExecuteStream(ctx, enterpriseExecutorAuth(), req, opts)
				if errStart != nil {
					t.Fatal(errStart)
				}
				for c := range result.Chunks {
					if c.Err != nil {
						err = c.Err
					}
					if thought && len(c.Payload) > 0 {
						t.Fatal("thought leaked or role packet emitted")
					}
				}
			} else {
				_, err = e.Execute(ctx, enterpriseExecutorAuth(), req, opts)
			}
			var scoped exec.RequestScopedError
			if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
				t.Fatalf("started error may be replayed: %v", err)
			}
		}
	}
}

type enterpriseCancelBody struct {
	ctx    context.Context
	closed chan struct{}
}

func (b *enterpriseCancelBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *enterpriseCancelBody) Close() error             { close(b.closed); return nil }

func TestEnterpriseCancellationClosesUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	closed := make(chan struct{})
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", enterpriseTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &enterpriseCancelBody{ctx: req.Context(), closed: closed}}, nil
	}))
	e := NewGeminiEnterpriseExecutor(&config.Config{})
	result, err := e.ExecuteStream(ctx, enterpriseExecutorAuth(), exec.Request{Model: "gemini-4-argon", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, enterpriseOptions(translator.FormatOpenAI))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range result.Chunks {
	}
	<-closed
}

func TestEnterpriseDiagnosticHeadersAreBoundedAndSafe(t *testing.T) {
	header := helps.EnterpriseResponseHeaders([]string{"temperature", "bad\r\nInjected: value", "bad\x01field"}).Get("X-CLIProxyAPI-Ignored-Parameters")
	if strings.IndexFunc(header, func(r rune) bool { return r < ' ' || r > 126 }) >= 0 || !strings.Contains(header, "temperature") {
		t.Fatalf("unsafe diagnostic response header: %q", header)
	}
	fields := make([]string, 200)
	for i := range fields {
		fields[i] = strings.Repeat("field", 20)
	}
	if got := helps.EnterpriseResponseHeaders(fields).Get("X-CLIProxyAPI-Ignored-Parameters"); len(got) > 4096 {
		t.Fatalf("unbounded diagnostic header: %d bytes", len(got))
	}
	e := NewGeminiEnterpriseExecutor(&config.Config{})
	req, _ := http.NewRequest("GET", ge.Origin+"/v1alpha/test", nil)
	req.Header = nil
	if err := e.PrepareRequest(req, enterpriseExecutorAuth()); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer access" {
		t.Fatal("missing authorization after nil-header preparation")
	}
}

func executeEnterpriseTestRequest(t *testing.T, e *GeminiEnterpriseExecutor, ctx context.Context, model string, format translator.Format, stream bool) error {
	t.Helper()
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if format == translator.FormatGemini {
		body = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
	}
	req := exec.Request{Model: model, Payload: body}
	opts := enterpriseOptions(format)
	if !stream {
		_, err := e.Execute(ctx, enterpriseExecutorAuth(), req, opts)
		return err
	}
	result, err := e.ExecuteStream(ctx, enterpriseExecutorAuth(), req, opts)
	if err != nil {
		return err
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			err = chunk.Err
		}
	}
	return err
}

func TestEnterprisePayloadValidationBeforeTransportAcrossModels(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro", "future-enterprise-model"} {
		for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
			for _, stream := range []bool{false, true} {
				t.Run(model+"/"+format.String()+map[bool]string{true: "-stream", false: "-full"}[stream], func(t *testing.T) {
					models := []config.PayloadModelRule{{Name: "*", Protocol: "gemini-enterprise"}}
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"generationConfig.thinkingConfig.includeThoughts": true}}}}}
					var calls int
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
						calls++
						return nil, errors.New("validation unexpectedly reached transport")
					}))
					err := executeEnterpriseTestRequest(t, NewGeminiEnterpriseExecutor(cfg), ctx, model, format, stream)
					message, code, param := enterprise.PublicErrorDetail(err, 400)
					if calls != 0 || code != "invalid_field" || param != "generationConfig" || !strings.Contains(message, "server request payload configuration") {
						t.Fatalf("calls=%d message=%q code=%q param=%q", calls, message, code, param)
					}
				})
			}
		}
	}
}

func TestEnterpriseFinalPayloadRulesRunOnceAndDoNotRestoreFilters(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			t.Run(format.String()+map[bool]string{true: "-stream", false: "-full"}[stream], func(t *testing.T) {
				cfg := &config.Config{Payload: config.PayloadConfig{
					Override: []config.PayloadRule{
						{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini-enterprise", Match: []map[string]any{{"languageCode": "en"}}}}, Params: map[string]any{"languageCode": "fr"}},
						{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini-enterprise", NotExist: []string{"languageCode"}}}, Params: map[string]any{"languageCode": "en"}},
						{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini-enterprise"}}, Params: map[string]any{"query.text": "configured exact query", "generationSpec.modelId": "configured-model"}},
						{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini"}}, Params: map[string]any{"generationConfig.temperature": 0.1}},
					},
					Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini-enterprise"}}, Params: []string{"toolsSpec"}}},
				}}
				var calls int
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					if gjson.GetBytes(body, "languageCode").String() != "en" {
						t.Fatalf("payload rules ran more than once: %s", body)
					}
					if gjson.GetBytes(body, "query.text").String() != "configured exact query" || gjson.GetBytes(body, "generationSpec.modelId").String() != "configured-model" || gjson.GetBytes(body, "toolsSpec").Exists() || gjson.GetBytes(body, "generationConfig").Exists() {
						t.Fatalf("user overrides or protocol isolation were changed: %s", body)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
				}))
				if err := executeEnterpriseTestRequest(t, NewGeminiEnterpriseExecutor(cfg), ctx, "gemini-4-argon", format, stream); err != nil || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			})
		}
	}
}

func TestEnterpriseRejectsInvalidUserOverridesWithoutRepair(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "*", Protocol: "gemini-enterprise"}}
	for _, test := range []struct {
		name, path string
		payload    config.PayloadConfig
	}{
		{"native-system", "systemInstruction", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"systemInstruction.parts.0.text": "private-prompt"}}}}},
		{"nested-unknown", "toolsSpec.webGroundingSpec.unknown", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"toolsSpec.webGroundingSpec.unknown": "private-value"}}}}},
		{"wrong-type", "query.text", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"query.text": false}}}}},
		{"session", "session", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"session": "private-session"}}}}},
		{"raw-duplicate", "query.text", config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"query": `{"text":"private-first","text":"private-second"}`}}}}},
		{"filter-query", "query", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"query"}}}}},
	} {
		for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
			for _, stream := range []bool{false, true} {
				t.Run(test.name+"/"+format.String()+map[bool]string{true: "-stream", false: "-full"}[stream], func(t *testing.T) {
					var calls int
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
						calls++
						return nil, errors.New("invalid override reached transport")
					}))
					err := executeEnterpriseTestRequest(t, NewGeminiEnterpriseExecutor(&config.Config{Payload: test.payload}), ctx, "gemini-4-argon", format, stream)
					message, code, param := enterprise.PublicErrorDetail(err, 400)
					if calls != 0 || code != "invalid_field" || param != test.path || strings.Contains(message, "private-") {
						t.Fatalf("calls=%d message=%q code=%q param=%q", calls, message, code, param)
					}
				})
			}
		}
	}
}

type enterpriseObservedErrorBody struct {
	io.Reader
	read, closed int
}

func (b *enterpriseObservedErrorBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}
func (b *enterpriseObservedErrorBody) Close() error { b.closed++; return nil }

func TestEnterpriseUpstreamErrorsPreserveStatusBoundReadsAndCloseBody(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			for _, status := range []int{400, 401, 403, 404, 422, 429, 500, 503} {
				for _, oversize := range []bool{false, true} {
					name := format.String() + map[bool]string{true: "-stream", false: "-full"}[stream] + "/" + http.StatusText(status) + map[bool]string{true: "-oversize", false: "-bounded"}[oversize]
					t.Run(name, func(t *testing.T) {
						raw := `{"error":{"message":"Bearer private-token private-prompt","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"generationSpec.modelId","description":"private-body-value"}]}]}}`
						if oversize {
							raw += strings.Repeat(" ", 128*1024)
						}
						body := &enterpriseObservedErrorBody{Reader: strings.NewReader(raw)}
						var calls int
						ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
							calls++
							return &http.Response{StatusCode: status, Header: http.Header{}, Body: body}, nil
						}))
						err := executeEnterpriseTestRequest(t, NewGeminiEnterpriseExecutor(&config.Config{}), ctx, "gemini-4-argon", format, stream)
						var detail interface{ StatusCode() int }
						if !errors.As(err, &detail) || detail.StatusCode() != status || calls != 1 || body.closed != 1 || body.read > 64*1024+1 {
							t.Fatalf("status=%d calls=%d closed=%d read=%d err=%v", status, calls, body.closed, body.read, err)
						}
						message, code, param := enterprise.PublicErrorDetail(err, status)
						if message == "" || code == "" || strings.Contains(err.Error(), "private-") || strings.Contains(message, "Bearer") {
							t.Fatalf("missing or unsafe diagnostic: %v", err)
						}
						if (status == 400 || status == 422) && !oversize {
							if param != "generationSpec.modelId" {
								t.Fatalf("lost safe field path: %q", param)
							}
						} else if param != "" {
							t.Fatalf("non-schema or oversize response affected field hint: %q", param)
						}
					})
				}
			}
		}
	}
}

type enterpriseCancelErrorBody struct {
	cancel context.CancelFunc
	closed bool
}

func (b *enterpriseCancelErrorBody) Read([]byte) (int, error) {
	b.cancel()
	return 0, context.Canceled
}
func (b *enterpriseCancelErrorBody) Close() error { b.closed = true; return nil }

func TestEnterpriseCancellationDuringHTTPErrorReadIsPreserved(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			body := &enterpriseCancelErrorBody{cancel: cancel}
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 400, Header: http.Header{}, Body: body}, nil
			}))
			err := executeEnterpriseTestRequest(t, NewGeminiEnterpriseExecutor(&config.Config{}), ctx, "gemini-4-argon", format, stream)
			cancel()
			if !errors.Is(err, context.Canceled) || !body.closed {
				t.Fatalf("HTTP error replaced cancellation or left body open: closed=%v err=%v", body.closed, err)
			}
		}
	}
}
