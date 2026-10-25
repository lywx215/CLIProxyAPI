package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestEnterpriseGenerationStartedCannotBeOverriddenByContinueRule(t *testing.T) {
	for _, action := range []string{RequestScopedActionContinue, RequestScopedActionContinueAndCooldown} {
		for _, mode := range []string{"nonstream", "stream_open", "stream_bootstrap", "stream_after_payload"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				model := "enterprise-started-" + strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())
				cfg := &internalconfig.Config{OAuthRequestScopedErrors: map[string][]internalconfig.RequestScopedErrorRule{
					"gemini-enterprise": {{Status: http.StatusBadGateway, Match: []string{"media_download_failed"}, Action: action}},
				}}
				m := NewManager(nil, nil, nil)
				m.SetConfig(cfg)
				m.SetRetryConfig(3, 0, 2)
				for i := 0; i < 2; i++ {
					auth := &Auth{ID: fmt.Sprintf("%s-%d", model, i), Provider: "gemini-enterprise", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}
					if _, err := m.Register(context.Background(), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model, Type: "gemini", EnterpriseMedia: "video"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				}
				baseErr := enterprise.Failure("media_download_failed", "media_download_failed", true)
				probe := &Auth{Provider: "gemini-enterprise", Attributes: map[string]string{"auth_kind": "oauth"}}
				if matched, ok := matchRequestScopedErrorAction(probe, baseErr, cfg); !ok || matched != action {
					t.Fatalf("configured replay rule did not match unmarked error: %q, %v", matched, ok)
				}
				errGenerated := coreexecutor.MarkGenerationStarted(baseErr)
				var calls atomic.Int32
				exec := &customStreamMockExecutor{
					identifier: "gemini-enterprise",
					mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, _ *Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
						calls.Add(1)
						coreexecutor.MarkUpstreamAttempt(ctx)
						return coreexecutor.Response{}, errGenerated
					}},
					streamFn: func(ctx context.Context, _ *Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
						calls.Add(1)
						coreexecutor.MarkUpstreamAttempt(ctx)
						if mode == "stream_open" {
							return nil, errGenerated
						}
						chunks := make(chan coreexecutor.StreamChunk, 2)
						if mode == "stream_after_payload" {
							chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"started":true}`)}
						}
						chunks <- coreexecutor.StreamChunk{Err: errGenerated}
						close(chunks)
						return &coreexecutor.StreamResult{Chunks: chunks}, nil
					},
				}
				m.RegisterExecutor(exec)
				req := coreexecutor.Request{Model: model, Payload: []byte(`{"prompt":"cat"}`)}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.Format("openai-video"), Metadata: map[string]any{coreexecutor.OperationMetadataKey: "video_generation"}}
				var resultErr error
				if mode == "nonstream" {
					_, resultErr = m.Execute(t.Context(), []string{"gemini-enterprise"}, req, opts)
				} else {
					var result *coreexecutor.StreamResult
					result, resultErr = m.ExecuteStream(t.Context(), []string{"gemini-enterprise"}, req, opts)
					if resultErr == nil && result != nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								resultErr = chunk.Err
								break
							}
						}
					}
				}
				if resultErr == nil || !coreexecutor.GenerationHasStarted(resultErr) {
					t.Fatalf("generation boundary was lost: %T %v", resultErr, resultErr)
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("%s overrode completed generation boundary: %d executions", action, got)
				}
			})
		}
	}
}
