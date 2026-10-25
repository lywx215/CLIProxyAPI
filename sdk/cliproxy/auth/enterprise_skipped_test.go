package auth

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestEnterpriseSkippedCannotRotateOrCoolCredentials(t *testing.T) {
	for _, action := range []string{RequestScopedActionContinue, RequestScopedActionContinueAndCooldown} {
		for _, mode := range []string{"nonstream", "stream_open", "stream_bootstrap", "stream_after_payload"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				model := "enterprise-skipped-" + strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())
				m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
				m.SetConfig(&config.Config{OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{ge.Provider: {{Status: 400, Match: []string{"upstream_skipped"}, Action: action}}}})
				m.SetRetryConfig(3, 0, 3)
				a := registerEnterpriseModelTestAuth(t, m, "a-"+model, "global", "engine-a", model)
				b := registerEnterpriseModelTestAuth(t, m, "b-"+model, "us", "engine-b", model)
				skipped := ge.NewSkippedError([]byte(`["NON_ASSIST_SEEKING_QUERY_IGNORED"]`), mode == "stream_after_payload")
				var calls atomic.Int32
				m.RegisterExecutor(&customStreamMockExecutor{identifier: ge.Provider, mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
					calls.Add(1)
					exec.MarkUpstreamAttempt(ctx)
					return exec.Response{}, skipped
				}}, streamFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
					calls.Add(1)
					exec.MarkUpstreamAttempt(ctx)
					if mode == "stream_open" {
						return nil, skipped
					}
					ch := make(chan exec.StreamChunk, 2)
					if mode == "stream_after_payload" {
						ch <- exec.StreamChunk{Payload: []byte(`{"partial":true}`)}
					}
					ch <- exec.StreamChunk{Err: skipped}
					close(ch)
					return &exec.StreamResult{Chunks: ch}, nil
				}})
				req := exec.Request{Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)}
				opts := exec.Options{SourceFormat: sdktranslator.FormatGemini, Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}}
				var err error
				if mode == "nonstream" {
					_, err = m.Execute(t.Context(), []string{ge.Provider}, req, opts)
				} else {
					var result *exec.StreamResult
					result, err = m.ExecuteStream(t.Context(), []string{ge.Provider}, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				}
				var actual *ge.SkippedError
				if !errors.As(err, &actual) || calls.Load() != 1 {
					t.Fatalf("request rejection was replayed: calls=%d err=%v", calls.Load(), err)
				}
				for _, id := range []string{a.ID, b.ID} {
					current, _ := m.GetByID(id)
					if current.Unavailable || current.LastError != nil || current.Quota.Exceeded || len(current.ModelStates) != 0 {
						t.Fatalf("request rejection penalized a credential: %s", id)
					}
					if id == b.ID && current.Failed != 0 {
						t.Fatal("alternate credential received the rejected request")
					}
				}
			})
		}
	}
}
