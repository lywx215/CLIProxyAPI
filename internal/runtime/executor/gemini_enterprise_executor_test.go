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
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			t.Run(format.String()+map[bool]string{true: "-stream", false: "-full"}[stream], func(t *testing.T) {
				transport := enterpriseTransport(func(req *http.Request) (*http.Response, error) {
					if _, ok := req.Context().Deadline(); ok {
						t.Error("inference has a deadline")
					}
					if req.Header.Get("Authorization") != "Bearer access" || req.URL.Host != "discoveryengine.googleapis.com" || !strings.HasSuffix(req.URL.Path, "/assistants/default_assistant:streamAssist") {
						t.Error("incorrect authorized upstream request")
					}
					body, _ := io.ReadAll(req.Body)
					if gjson.GetBytes(body, "generationSpec.modelId").String() != "gemini-4-argon" || gjson.GetBytes(body, "session").Exists() {
						t.Error("incorrect model or session reuse")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
				})
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
				body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
				if format == translator.FormatGemini {
					body = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
				}
				req := exec.Request{Model: "gemini-4-argon", Payload: body}
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
