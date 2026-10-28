package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestEnterpriseRetryPinsActualModelAfterPayload(t *testing.T) {
	ctx := withEnterpriseModelAttempts(t.Context())
	if err := RecordEnterpriseGenerationModel(ctx, "gemini-4-argon"); err != nil {
		t.Fatal(err)
	}
	if err := RecordEnterpriseGenerationModel(ctx, "gemini-4-argon"); err != nil {
		t.Fatal(err)
	}
	if err := RecordEnterpriseGenerationModel(ctx, "gemini-3.8-flash"); err == nil || !isRequestInvalidError(err) {
		t.Fatalf("retry changed the actual upstream model: %v", err)
	}
	// A nested execution owns a separate model pin and cannot overwrite its parent.
	nested := withEnterpriseModelAttempts(ctx)
	if err := RecordEnterpriseGenerationModel(nested, "gemini-3.8-flash"); err != nil {
		t.Fatal(err)
	}
	if err := RecordEnterpriseGenerationModel(ctx, "gemini-4-argon"); err != nil {
		t.Fatal(err)
	}
	if err := RecordEnterpriseGenerationModel(context.Background(), "gemini-3.8-flash"); err != nil {
		t.Fatal(err)
	}
}

func TestEnterpriseRetryDoesNotExceedCredentialBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			model := "enterprise-budget-" + strings.ReplaceAll(t.Name(), "/", "-")
			m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
			m.SetConfig(&config.Config{})
			m.SetRetryConfig(0, 0, 2)
			registerEnterpriseModelTestAuth(t, m, "a-budget", "global", "engine-a", model)
			registerEnterpriseModelTestAuth(t, m, "b-budget", "global", "engine-b", model)
			var calls atomic.Int32
			failure := func(ctx context.Context) error {
				exec.MarkUpstreamAttempt(ctx)
				calls.Add(1)
				return &ge.GenerationError{HTTPStatus: 503, UpstreamHTTPStatus: 503, RPCStatus: "UNAVAILABLE"}
			}
			m.RegisterExecutor(&customStreamMockExecutor{identifier: ge.Provider,
				mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
					return exec.Response{}, failure(ctx)
				}},
				streamFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
					return nil, failure(ctx)
				},
			})
			req := exec.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"offline"}]}`)}
			opts := exec.Options{SourceFormat: translator.FormatOpenAI, Metadata: map[string]any{exec.OperationMetadataKey: "chat_completions"}}
			var err error
			if stream {
				_, err = m.ExecuteStream(t.Context(), []string{ge.Provider}, req, opts)
			} else {
				_, err = m.Execute(t.Context(), []string{ge.Provider}, req, opts)
			}
			if err == nil || statusCodeFromError(err) != http.StatusServiceUnavailable || calls.Load() != 2 {
				t.Fatalf("retry budget or upstream status lost: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestEnterpriseRetryModelPinConcurrent(t *testing.T) {
	ctx := withEnterpriseModelAttempts(t.Context())
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := RecordEnterpriseGenerationModel(ctx, "gemini-3.8-flash"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := RecordEnterpriseGenerationModel(ctx, "gemini-4-argon"); err == nil {
		t.Fatal("concurrent model pin was lost")
	}
}
