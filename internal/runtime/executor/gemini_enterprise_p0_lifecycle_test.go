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
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type enterpriseP0FailingBody struct {
	reader *strings.Reader
	closed atomic.Int32
}

func (b *enterpriseP0FailingBody) Read(p []byte) (int, error) {
	if b.reader.Len() == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	return b.reader.Read(p)
}

func (b *enterpriseP0FailingBody) Close() error {
	b.closed.Add(1)
	return nil
}

type enterpriseP0UsageCapture struct {
	authPrefix string
	records    chan usage.Record
}

func (c *enterpriseP0UsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if strings.HasPrefix(record.AuthID, c.authPrefix+"/") {
		c.records <- record
	}
}

func TestEnterpriseP0ReadFailuresNeverPublishSuccess(t *testing.T) {
	capture := &enterpriseP0UsageCapture{authPrefix: t.Name(), records: make(chan usage.Record, 32)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	for _, scenario := range []struct {
		name, raw        string
		started, visible bool
	}{
		{name: "before-start"},
		{name: "after-hidden-thought", raw: `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"private-hidden-thought","thought":true}}]}}`, started: true},
		{name: "after-visible-text", raw: `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"hello"}}]}}`, started: true, visible: true},
		{name: "after-successful-terminal", raw: enterpriseAnswer, started: true, visible: true},
	} {
		for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
			for _, stream := range []bool{false, true} {
				name := scenario.name + "/" + format.String() + map[bool]string{false: "/full", true: "/stream"}[stream]
				t.Run(name, func(t *testing.T) {
					body := &enterpriseP0FailingBody{reader: strings.NewReader(scenario.raw)}
					var calls atomic.Int32
					transport := enterpriseTransport(func(*http.Request) (*http.Response, error) {
						calls.Add(1)
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
					})
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
					auth := enterpriseExecutorAuth(t, enterpriseExecutorIdentity(t.Name(), ""))
					requestBody := `{"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`
					if format == translator.FormatGemini {
						requestBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
					}
					request := exec.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(requestBody)}
					executor := NewGeminiEnterpriseExecutor(&config.Config{})
					var err error
					var wire []byte
					if stream {
						result, errStart := executor.ExecuteStream(ctx, auth, request, enterpriseOptions(format))
						if errStart != nil {
							t.Fatal(errStart)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								if err != nil {
									t.Fatal("read failure emitted multiple stream errors")
								}
								err = chunk.Err
							}
							if gjson.GetBytes(chunk.Payload, "choices.0.finish_reason").String() == "stop" || gjson.GetBytes(chunk.Payload, "candidates.0.finishReason").String() == "STOP" || gjson.GetBytes(chunk.Payload, "usage").Exists() || gjson.GetBytes(chunk.Payload, "usageMetadata").Exists() {
								t.Fatal("read failure emitted a success terminal or usage chunk")
							}
							wire = append(wire, chunk.Payload...)
						}
					} else {
						result, errExecute := executor.Execute(ctx, auth, request, enterpriseOptions(format))
						err = errExecute
						if len(result.Payload) > 0 {
							t.Fatal("failed non-streaming execution returned a success payload")
						}
					}
					var transportError *ge.TransportError
					if !errors.As(err, &transportError) || !errors.Is(err, io.ErrUnexpectedEOF) || transportError.Started != scenario.started || transportError.IsRequestScoped() != scenario.started {
						t.Fatalf("read failure lost its cause or started scope: %v", err)
					}
					if calls.Load() != 1 || body.closed.Load() != 1 {
						t.Fatalf("calls=%d closes=%d", calls.Load(), body.closed.Load())
					}
					if strings.Contains(string(wire), "private-hidden-thought") || (stream && strings.Contains(string(wire), "hello") != scenario.visible) {
						t.Fatal("failure lost visible output or leaked a hidden thought")
					}
					select {
					case record := <-capture.records:
						if record.AuthID != auth.ID || !record.Failed {
							t.Fatalf("read failure was not recorded as failed: auth=%q failed=%v", record.AuthID, record.Failed)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("failed usage record was not published")
					}
				})
			}
		}
	}
}

type enterpriseP0CancelAfterBody struct {
	ctx        context.Context
	reader     *strings.Reader
	waiting    chan struct{}
	closed     chan struct{}
	closeCount atomic.Int32
}

func (b *enterpriseP0CancelAfterBody) Read(p []byte) (int, error) {
	if b.reader.Len() > 0 {
		return b.reader.Read(p)
	}
	close(b.waiting)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *enterpriseP0CancelAfterBody) Close() error {
	if b.closeCount.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}

func TestEnterpriseP0CancellationAfterProgressClosesFailedAttempt(t *testing.T) {
	capture := &enterpriseP0UsageCapture{authPrefix: t.Name(), records: make(chan usage.Record, 32)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	for _, scenario := range []struct {
		name, raw     string
		json, visible bool
	}{
		{name: "hidden-thought", raw: `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"private-hidden-thought","thought":true}}]}}`},
		{name: "visible-text", raw: `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"hello"}}]}}`, visible: true},
		{name: "buffered-json", raw: `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"{\"ok\":true}"}}]}}`, json: true},
	} {
		for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
			for _, stream := range []bool{false, true} {
				name := scenario.name + "/" + format.String() + map[bool]string{false: "/full", true: "/stream"}[stream]
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					t.Cleanup(cancel)
					body := &enterpriseP0CancelAfterBody{ctx: ctx, reader: strings.NewReader(scenario.raw), waiting: make(chan struct{}), closed: make(chan struct{})}
					var calls atomic.Int32
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", enterpriseTransport(func(request *http.Request) (*http.Response, error) {
						calls.Add(1)
						if _, ok := request.Context().Deadline(); ok {
							t.Error("inference acquired a deadline")
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
					}))
					requestBody := `{"messages":[{"role":"user","content":"hi"}]}`
					if scenario.json {
						requestBody = `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`
					}
					if format == translator.FormatGemini {
						requestBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
						if scenario.json {
							requestBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json"}}`
						}
					}
					request := exec.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(requestBody)}
					auth := enterpriseExecutorAuth(t, enterpriseExecutorIdentity(t.Name(), ""))
					executor := NewGeminiEnterpriseExecutor(&config.Config{})
					done := make(chan struct{})
					var executionErr error
					var wire []byte
					go func() {
						defer close(done)
						if stream {
							result, err := executor.ExecuteStream(ctx, auth, request, enterpriseOptions(format))
							if err != nil {
								executionErr = err
								return
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									executionErr = chunk.Err
								}
								wire = append(wire, chunk.Payload...)
							}
							return
						}
						result, err := executor.Execute(ctx, auth, request, enterpriseOptions(format))
						executionErr = err
						wire = result.Payload
					}()
					select {
					case <-body.waiting:
					case <-time.After(5 * time.Second):
						t.Fatal("executor did not consume the progress packet")
					}
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Fatal("canceled executor did not finish")
					}
					if !stream && !errors.Is(executionErr, context.Canceled) {
						t.Fatalf("cancellation was replaced: %v", executionErr)
					}
					if executionErr != nil && !errors.Is(executionErr, context.Canceled) {
						t.Fatalf("stream replaced cancellation: %v", executionErr)
					}
					if strings.Contains(string(wire), "private-hidden-thought") || strings.Contains(string(wire), "finishReason") || strings.Contains(string(wire), "finish_reason\":\"stop") || strings.Contains(string(wire), "usageMetadata") {
						t.Fatal("canceled attempt leaked hidden content or emitted success")
					}
					if strings.Contains(string(wire), "hello") != (stream && scenario.visible) {
						t.Fatal("unexpected visible bytes on a canceled attempt")
					}
					select {
					case <-body.closed:
					default:
						t.Fatal("canceled response body was not closed")
					}
					if calls.Load() != 1 || body.closeCount.Load() != 1 {
						t.Fatalf("calls=%d closes=%d", calls.Load(), body.closeCount.Load())
					}
					select {
					case record := <-capture.records:
						if record.AuthID != auth.ID || !record.Failed {
							t.Fatalf("cancellation was not recorded as failed: auth=%q failed=%v", record.AuthID, record.Failed)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("canceled usage record was not published")
					}
				})
			}
		}
	}
}
