package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type enterprise502ReadError struct {
	text *strings.Reader
	err  error
}

func (r *enterprise502ReadError) Read(p []byte) (int, error) {
	if r.text.Len() > 0 {
		n, _ := r.text.Read(p)
		return n, r.err
	}
	return 0, r.err
}
func (*enterprise502ReadError) Close() error { return nil }

func TestEnterprise502ReadFailureKeepsGenerationBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tt := range []struct{ name, raw string }{
			{"partial", `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"partial"}}]}}`},
			{"completed", `{"answer":{"state":"SUCCEEDED"}}`},
		} {
			t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream]+"/"+tt.name, func(t *testing.T) {
				var calls atomic.Int32
				transport := enterpriseTransport(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					if _, ok := req.Context().Deadline(); ok {
						t.Error("executor added a generation deadline")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &enterprise502ReadError{text: strings.NewReader(tt.raw + "\n"), err: io.ErrUnexpectedEOF}}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
				e := NewGeminiEnterpriseExecutor(&config.Config{})
				auth := enterpriseExecutorAuth(t)
				req := coreexec.Request{Model: "gemini-3.8-flash", Payload: []byte(`{"messages":[{"role":"user","content":"offline fixture"}]}`)}
				var resultErr error
				if !stream {
					_, resultErr = e.Execute(ctx, auth, req, enterpriseOptions(translator.FormatOpenAI))
				} else {
					result, err := e.ExecuteStream(ctx, auth, req, enterpriseOptions(translator.FormatOpenAI))
					resultErr = err
					if result != nil {
						for c := range result.Chunks {
							if c.Err != nil {
								resultErr = c.Err
							}
						}
					}
				}
				var network *ge.TransportError
				if !errors.As(resultErr, &network) || !network.Started || !coreexec.GenerationHasStarted(resultErr) || network.StatusCode() != 502 || calls.Load() != 1 {
					t.Fatalf("read failure lost generation safety or status: %T %v", resultErr, resultErr)
				}
			})
		}
	}
}

func TestEnterprise502StructuredErrorSurvivesSimultaneousReadFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			transport := enterpriseTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &enterprise502ReadError{text: strings.NewReader(`{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"private-token"}}`), err: io.ErrUnexpectedEOF}}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
			e := NewGeminiEnterpriseExecutor(&config.Config{})
			auth := enterpriseExecutorAuth(t)
			req := coreexec.Request{Model: "gemini-3.8-flash", Payload: []byte(`{"messages":[{"role":"user","content":"offline"}]}`)}
			var resultErr error
			if !stream {
				_, resultErr = e.Execute(ctx, auth, req, enterpriseOptions(translator.FormatOpenAI))
			} else {
				result, err := e.ExecuteStream(ctx, auth, req, enterpriseOptions(translator.FormatOpenAI))
				resultErr = err
				if result != nil {
					for c := range result.Chunks {
						if c.Err != nil {
							resultErr = c.Err
						}
					}
				}
			}
			var generation *ge.GenerationError
			if !errors.As(resultErr, &generation) || generation.StatusCode() != 400 || generation.RPCStatus != "INVALID_ARGUMENT" || strings.Contains(resultErr.Error(), "private-") {
				t.Fatalf("read failure replaced a complete Google error: %v", resultErr)
			}
		})
	}
}
