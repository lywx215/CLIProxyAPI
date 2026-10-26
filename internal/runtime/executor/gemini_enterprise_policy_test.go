package executor

import (
	"bytes"
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

func contextWithEnterpriseTransport(t *testing.T, transport enterpriseTransport) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
}

func TestEnterprisePolicyBlocksFinalPayloadWithoutDispatch(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"argon_only", "exclude_argon", "custom", "auto"} {
				t.Run(format.String()+"/"+mode+map[bool]string{true: "/stream", false: "/full"}[stream], func(t *testing.T) {
					auth := enterpriseExecutorAuth()
					auth.Metadata["model_policy"] = mode
					if mode == "custom" {
						auth.Metadata["allowed_models"] = []string{}
					}
					target := ge.ArgonModel
					if mode == "argon_only" {
						target = "gemini-3.8-flash"
					}
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: ge.ArgonModel, Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": target}}}}}
					calls := 0
					ctx := contextWithEnterpriseTransport(t, func(*http.Request) (*http.Response, error) {
						calls++
						return nil, errors.New("forbidden generation dispatched")
					})
					req := coreexec.Request{Model: ge.ArgonModel, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
					if format == translator.FormatGemini {
						req.Payload = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
					}
					e := NewGeminiEnterpriseExecutor(cfg)
					var err error
					if stream {
						_, err = e.ExecuteStream(ctx, auth, req, enterpriseOptions(format))
					} else {
						_, err = e.Execute(ctx, auth, req, enterpriseOptions(format))
					}
					var rejection *coreauth.EnterpriseModelPolicyError
					if !errors.As(err, &rejection) || calls != 0 {
						t.Fatalf("policy bypass: calls=%d err=%v", calls, err)
					}
				})
			}
		}
	}
}

func TestEnterprisePolicyGatesManagementRawGenerationAndMedia(t *testing.T) {
	for _, body := range []string{
		`{"generationSpec":{"modelId":"gemini-3.8-flash"}}`,
		`{"generationSpec":{"modelId":"gemini-4-argon"},"toolsSpec":{"imageGenerationSpec":{}}}`,
		`{"toolsSpec":{"videoGenerationSpec":{}}}`,
	} {
		auth := enterpriseExecutorAuth()
		auth.Metadata["model_policy"] = "argon_only"
		calls := 0
		ctx := contextWithEnterpriseTransport(t, func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("forbidden generation dispatched")
		})
		req, _ := http.NewRequestWithContext(ctx, "POST", ge.OriginForLocation("global")+"/v1alpha/"+ge.Engine(ge.FromMetadata(auth.Metadata))+"/assistants/default_assistant:streamAssist", bytes.NewBufferString(body))
		_, err := NewGeminiEnterpriseExecutor(&config.Config{}).HttpRequest(ctx, auth, req)
		var rejection *coreauth.EnterpriseModelPolicyError
		if !errors.As(err, &rejection) || calls != 0 {
			t.Fatalf("raw call bypass: calls=%d err=%v", calls, err)
		}
	}
}

func TestEnterprisePolicyRawOperationsCannotBypassModelValidation(t *testing.T) {
	for _, tc := range []struct {
		name, mode, method, suffix, body string
		allowed                          []string
		wantCalls                        int
	}{
		{name: "unsupported-generation", mode: "argon_only", method: http.MethodPost, suffix: "/assistants/default_assistant:answer", body: `{}`},
		{name: "wrong-application", mode: "argon_only", method: http.MethodPost, suffix: "/assistants/another_assistant:streamAssist", body: `{"generationSpec":{"modelId":"gemini-4-argon"}}`},
		{name: "all-media-models-validated", mode: "custom", method: http.MethodPost, suffix: "/assistants/default_assistant:streamAssist", body: `{"toolsSpec":{"imageGenerationSpec":{},"videoGenerationSpec":{}}}`, allowed: []string{"gemini-3-pro-image"}},
		{name: "paused-read-only-resource-access", mode: "auto", method: http.MethodGet, suffix: "/assistants/default_assistant", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := enterpriseExecutorAuth()
			auth.Metadata["model_policy"] = tc.mode
			if tc.allowed != nil {
				auth.Metadata["allowed_models"] = tc.allowed
			}
			calls := 0
			ctx := contextWithEnterpriseTransport(t, func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			url := ge.OriginForLocation("global") + "/v1alpha/" + ge.Engine(ge.FromMetadata(auth.Metadata)) + tc.suffix
			req, err := http.NewRequestWithContext(ctx, tc.method, url, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := NewGeminiEnterpriseExecutor(&config.Config{}).HttpRequest(ctx, auth, req)
			if calls != tc.wantCalls || (err == nil) != (tc.wantCalls > 0) {
				t.Fatalf("raw operation validation: calls=%d want=%d err=%v", calls, tc.wantCalls, err)
			}
			if response != nil {
				if errClose := response.Body.Close(); errClose != nil {
					t.Fatal(errClose)
				}
			}
		})
	}
}

func TestEnterpriseManualArgonRetainsStreamingUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		auth := enterpriseExecutorAuth()
		auth.Metadata["model_policy"] = "argon_only"
		calls := 0
		ctx := contextWithEnterpriseTransport(t, func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
		})
		req := coreexec.Request{Model: ge.ArgonModel, Payload: []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)}
		e := NewGeminiEnterpriseExecutor(&config.Config{})
		var output []byte
		if stream {
			result, err := e.ExecuteStream(ctx, auth, req, enterpriseOptions(translator.FormatGemini))
			if err != nil {
				t.Fatal(err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output = append(output, chunk.Payload...)
			}
		} else {
			result, err := e.Execute(ctx, auth, req, enterpriseOptions(translator.FormatGemini))
			if err != nil {
				t.Fatal(err)
			}
			output = result.Payload
		}
		if calls != 1 || !strings.Contains(string(output), "usageMetadata") {
			t.Fatalf("allowed generation lost usage: calls=%d output=%s", calls, output)
		}
	}
}
