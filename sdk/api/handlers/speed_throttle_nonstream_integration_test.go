package handlers_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type nonStreamingThrottleExecutor struct {
	response string
}

func (*nonStreamingThrottleExecutor) Identifier() string { return "nonstream-throttle-test" }

func (e *nonStreamingThrottleExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: []byte(e.response)}, nil
}

func (*nonStreamingThrottleExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("unexpected streaming request")
}

func (*nonStreamingThrottleExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*nonStreamingThrottleExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("unexpected count request")
}

func (*nonStreamingThrottleExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected HTTP request")
}

func TestNonStreamingHandlersThrottleReportedOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		path     string
		request  string
		response string
	}{
		{
			name: "chat completions", path: "/v1/chat/completions",
			request:  `{"model":"nonstream-throttle-model","messages":[{"role":"user","content":"hi"}]}`,
			response: `{"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"completion_tokens":2080,"completion_tokens_details":{"reasoning_tokens":1800}}}`,
		},
		{
			name: "responses", path: "/v1/responses",
			request:  `{"model":"nonstream-throttle-model","input":"hi"}`,
			response: `{"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"output_tokens":2080}}`,
		},
		{
			name: "gemini", path: "/v1beta/models/nonstream-throttle-model:generateContent",
			request:  `{"contents":[{"parts":[{"text":"hi"}]}]}`,
			response: `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}],"usageMetadata":{"candidatesTokenCount":280,"thoughtsTokenCount":1800}}`,
		},
	} {
		for _, mode := range []string{"enabled", "disabled", "cancelled"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					executor := &nonStreamingThrottleExecutor{response: tc.response}
					manager := coreauth.NewManager(nil, nil, nil)
					manager.RegisterExecutor(executor)
					auth := &coreauth.Auth{ID: "nonstream-throttle-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive}
					if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "nonstream-throttle-model"}})
					defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
					base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
						SpeedThrottle: sdkconfig.SpeedThrottleConfig{
							Enabled: mode != "disabled", MinTokensPerSecond: 100, MaxTokensPerSecond: 100,
							MinFirstTokenDelayMs: 3500, MaxFirstTokenDelayMs: 3500,
						},
					}, manager)
					router := gin.New()
					router.POST("/v1/chat/completions", openai.NewOpenAIAPIHandler(base).ChatCompletions)
					router.POST("/v1/responses", openai.NewOpenAIResponsesAPIHandler(base).Responses)
					router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(base).GeminiHandler)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.request)).WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					recorder := httptest.NewRecorder()
					start := time.Now()
					done := make(chan struct{})
					go func() {
						defer close(done)
						router.ServeHTTP(recorder, request)
					}()
					synctest.Wait()
					if mode != "disabled" && recorder.Body.Len() != 0 {
						t.Fatal("response body emitted before throttle delay")
					}
					if mode == "cancelled" {
						cancel()
					}
					<-done
					if mode == "cancelled" {
						if recorder.Body.Len() != 0 || time.Since(start) != 0 {
							t.Fatal("cancelled throttle should return immediately without a response body")
						}
						return
					}
					wantDuration := time.Duration(0)
					if mode == "enabled" {
						wantDuration = 20800 * time.Millisecond
					}
					if elapsed := time.Since(start); elapsed != wantDuration {
						t.Fatalf("duration = %v, want %v for 2080 output tokens at 100 tokens/s", elapsed, wantDuration)
					}
					if recorder.Code != http.StatusOK || recorder.Body.String() != tc.response {
						t.Fatalf("unexpected response: status=%d body=%s", recorder.Code, recorder.Body.String())
					}
				})
			})
		}
	}
}
