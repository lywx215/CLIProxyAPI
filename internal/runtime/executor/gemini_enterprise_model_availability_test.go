package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const enterpriseModelDisabledResponse = `[{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"MODEL_NOT_ENABLED"}]}}]`

func TestEnterpriseModelProbeValidatesFinalPayload(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "stream"}[stream], func(t *testing.T) {
			auth := enterpriseExecutorAuth(t)
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-4-argon", Protocol: "gemini-enterprise"}}, Params: map[string]any{"generationSpec.modelId": "another-model"}}}}}
			calls := 0
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("must not dispatch changed probe model")
			}))
			ctx = coreauth.WithEnterpriseModelProbe(ctx, auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)), "gemini-4-argon")
			e := NewGeminiEnterpriseExecutor(cfg)
			req := coreexec.Request{Model: "gemini-4-argon", Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}
			var err error
			if stream {
				_, err = e.ExecuteStream(ctx, auth, req, enterpriseOptions(translator.FormatGemini))
			} else {
				_, err = e.Execute(ctx, auth, req, enterpriseOptions(translator.FormatGemini))
			}
			var conflict *coreauth.EnterpriseModelProbeError
			if !errors.As(err, &conflict) || conflict.Code != "model_changed" || calls != 0 {
				t.Fatalf("probe override dispatched or lost conflict: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestEnterpriseModelRejectionUsesFinalPayloadModel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, httpStatus := range []int{400, 200} {
			name := map[bool]string{false: "full", true: "stream"}[stream] + "/" + http.StatusText(httpStatus)
			t.Run(name, func(t *testing.T) {
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-4-argon", Protocol: "gemini-enterprise"}}, Params: map[string]any{"generationSpec.modelId": "actual-upstream-model"}}}}}
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: httpStatus, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseModelDisabledResponse))}, nil
				}))
				e := NewGeminiEnterpriseExecutor(cfg)
				req := coreexec.Request{Model: "gemini-4-argon", Payload: []byte(`{"contents":[{"parts":[{"text":"OK"}]}]}`)}
				var err error
				if stream {
					var result *coreexec.StreamResult
					result, err = e.ExecuteStream(ctx, enterpriseExecutorAuth(t), req, enterpriseOptions(translator.FormatGemini))
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = e.Execute(ctx, enterpriseExecutorAuth(t), req, enterpriseOptions(translator.FormatGemini))
				}
				var rejection *ge.GenerationError
				if !errors.As(err, &rejection) || !rejection.ModelNotEnabled || rejection.Model != "actual-upstream-model" || rejection.Started || rejection.StatusCode() != 400 {
					t.Fatalf("wrong rejection classification: %v", err)
				}
			})
		}
	}
}
