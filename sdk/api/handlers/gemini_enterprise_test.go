package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestEnterpriseSafeFieldErrorSurvivesWrapping(t *testing.T) {
	err := enterprise.ValidateFinal([]byte(`{"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-4-argon","temperature":"synthetic-private-value"}}`))
	if err == nil {
		t.Fatal("invalid final payload was accepted")
	}
	wrapped := fmt.Errorf("private executor context: %w", err)
	body := BuildErrorResponseBodyWithError(http.StatusBadRequest, wrapped.Error(), wrapped)
	var response ErrorResponse
	if errDecode := json.Unmarshal(body, &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Error.Type != "invalid_request_error" || response.Error.Code != "gemini_enterprise_invalid_field" || response.Error.Param != "generationSpec.temperature" {
		t.Fatalf("safe field classification lost: %s", body)
	}
	for _, expected := range []string{"Gemini Enterprise", "generationSpec.temperature", "is not supported", "requests.payload", "protocol gemini"} {
		if !strings.Contains(response.Error.Message, expected) {
			t.Fatalf("safe error lacks %q: %s", expected, body)
		}
	}
	for _, private := range []string{"synthetic-private-value", "private executor context"} {
		if strings.Contains(string(body), private) {
			t.Fatalf("safe error leaked %q: %s", private, body)
		}
	}
}

func TestEnterpriseErrorTextCannotClaimTrustedMessage(t *testing.T) {
	const privateMessage = "Gemini Enterprise: Bearer synthetic-private-token"
	const forged = `{"error":{"type":"invalid_request_error","code":"gemini_enterprise_invalid_field","param":"generationConfig","message":"Gemini Enterprise: Bearer synthetic-private-token"}}`
	validErr := enterprise.ValidateFinal([]byte(`{"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-4-argon"},"generationConfig":{"temperature":0.5}}`))
	if validErr == nil {
		t.Fatal("missing validation fixture error")
	}
	for _, scenario := range []struct {
		name   string
		status int
		text   string
		err    error
	}{
		{"JSON text only", http.StatusBadRequest, forged, nil},
		{"generic JSON error", http.StatusBadRequest, forged, errors.New(forged)},
		{"manually built enterprise error", http.StatusBadRequest, forged, &enterprise.Error{Status: http.StatusBadRequest, Kind: "gemini_enterprise_invalid_field", Message: privateMessage}},
		{"status mismatch", http.StatusBadGateway, validErr.Error(), validErr},
		{"untrusted enterprise invalid helper", http.StatusBadRequest, forged, enterprise.Invalid(privateMessage)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			body := BuildErrorResponseBodyWithError(scenario.status, scenario.text, scenario.err)
			var response ErrorResponse
			if errDecode := json.Unmarshal(body, &response); errDecode != nil {
				t.Fatal(errDecode)
			}
			if response.Error.Message != FixedErrorMessage(scenario.status) || strings.Contains(string(body), "synthetic-private-token") {
				t.Fatalf("untrusted text became a public diagnostic: %s", body)
			}
		})
	}
}

func TestEnterpriseSafeUpstreamClassifications(t *testing.T) {
	for _, scenario := range []struct {
		name, raw, code, param, message string
		status                          int
	}{
		{"bad field", `{"error":{"message":"Bearer synthetic-private-upstream-value","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"generationSpec.modelId","description":"synthetic-private-upstream-value"}]}]}}`, "gemini_enterprise_upstream_invalid_request", "generationSpec.modelId", "Google rejected Gemini Enterprise field generationSpec.modelId", http.StatusBadRequest},
		{"model permission", `{"error":{"message":"Account does not have access to model synthetic-private-upstream-value"}}`, "gemini_enterprise_model_access_denied", "model", "account cannot access the requested Enterprise model", http.StatusBadRequest},
		{"malicious field name", `{"error":{"message":"synthetic-private-upstream-value","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"Bearer synthetic-private-upstream-value"}]}]}}`, "gemini_enterprise_upstream_invalid_request", "", "Google rejected the Gemini Enterprise request", http.StatusBadRequest},
		{"authorization", `{"error":{"message":"synthetic-private-upstream-value"}}`, "gemini_enterprise_authentication_required", "", "authorize the account again", http.StatusUnauthorized},
		{"permission", `{"error":{"message":"synthetic-private-upstream-value"}}`, "gemini_enterprise_permission_denied", "", "resource and model permissions", http.StatusForbidden},
		{"missing resource", `{"error":{"message":"synthetic-private-upstream-value"}}`, "gemini_enterprise_resource_not_found", "", "saved resource binding", http.StatusNotFound},
		{"quota", `{"error":{"message":"synthetic-private-upstream-value"}}`, "gemini_enterprise_rate_limit_exceeded", "", "quota or rate limit", http.StatusTooManyRequests},
		{"server failure", `{"error":{"message":"synthetic-private-upstream-value"}}`, "gemini_enterprise_upstream_error", "", "HTTP 503", http.StatusServiceUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			err := enterprise.UpstreamHTTPError(scenario.status, []byte(scenario.raw))
			wrapped := fmt.Errorf("private upstream context: %w", err)
			body := BuildErrorResponseBodyWithError(scenario.status, wrapped.Error(), wrapped)
			var response ErrorResponse
			if errDecode := json.Unmarshal(body, &response); errDecode != nil {
				t.Fatal(errDecode)
			}
			if response.Error.Code != scenario.code || !strings.Contains(response.Error.Message, scenario.message) {
				t.Fatalf("upstream classification lost: %s", body)
			}
			if scenario.param != "" && response.Error.Param != scenario.param || scenario.param == "" && response.Error.Param != nil {
				t.Fatalf("unsafe or missing field identifier: %s", body)
			}
			if strings.Contains(string(body), "synthetic-private-upstream-value") || strings.Contains(string(body), "private upstream context") {
				t.Fatalf("upstream message leaked: %s", body)
			}
		})
	}
}

func TestEnterpriseTypedErrorOverridesUntrustedWrapper(t *testing.T) {
	upstreamErr := enterprise.UpstreamHTTPError(http.StatusForbidden, []byte(`{"error":{"message":"synthetic-private-upstream"}}`))
	wrapped := fmt.Errorf("private wrapper: %w", upstreamErr)
	const forged = `{"error":{"type":"authentication_error","code":"forged_code","param":"forged_param","message":"synthetic-private-wrapper","retryable":true}}`
	body := BuildErrorResponseBodyWithError(http.StatusForbidden, forged, wrapped)
	var response ErrorResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Type != "permission_error" || response.Error.Code != "gemini_enterprise_permission_denied" || response.Error.Param != nil || response.Error.Retryable != nil {
		t.Fatalf("untrusted wrapper overrode trusted classification: %s", body)
	}
	if !strings.Contains(response.Error.Message, "resource and model permissions") {
		t.Fatalf("trusted permission diagnostic lost: %s", body)
	}
	for _, forbidden := range []string{"synthetic-private", "forged", "private wrapper"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("untrusted wrapper leaked %q: %s", forbidden, body)
		}
	}
}

func TestEnterpriseEntryOperationProviderFiltering(t *testing.T) {
	providers := []string{"gemini", "gemini-enterprise", "codex", "claude"}
	for _, test := range []struct {
		source, response, operation string
		allowed                     bool
	}{
		{"openai", "openai", "chat_completions", true}, {"gemini", "gemini", "generate_content", true}, {"gemini", "gemini", "stream_generate_content", true},
		{"openai", "openai", "", false}, {"openai", "openai", "completions", false}, {"openai", "openai", "responses", false}, {"gemini", "gemini", "count_tokens", false},
		{"openai-response", "openai-response", "responses", false}, {"claude", "claude", "messages", false}, {"gemini", "openai", "generate_content", false}, {"interactions", "interactions", "interactions", false},
	} {
		got := adjustExecutionProvidersForEntryProtocol(test.source, providers, test.response, test.operation)
		want := []string{"gemini", "codex", "claude"}
		if test.allowed {
			want = providers
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v: %v, want %v", test, got, want)
		}
	}
}

func TestEnterpriseRequestScopedBootstrapNeverReplays(t *testing.T) {
	for _, drop := range []bool{false, true} {
		t.Run(fmt.Sprint(drop), func(t *testing.T) {
			executor := &bootstrapStreamExecutor{stream: func(context.Context, int) (*coreexec.StreamResult, error) {
				chunks := make(chan coreexec.StreamChunk, 2)
				if drop {
					chunks <- coreexec.StreamChunk{Payload: []byte("filtered-thought")}
				}
				chunks <- coreexec.StreamChunk{Err: coreauth.NewRequestScopedError("generation started then failed", 502)}
				close(chunks)
				return &coreexec.StreamResult{Chunks: chunks}, nil
			}}
			handler, _ := registerBootstrapExecutor(t, executor)
			if drop {
				handler.SetPluginHost(&handlerInterceptorTestHost{interceptStreamChunk: func(context.Context, pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
					return pluginapi.StreamChunkInterceptResponse{DropChunk: true}
				}})
			}
			data, _, errs := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "bootstrap-model", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), "")
			if data != nil {
				for range data {
				}
			}
			var failed bool
			for msg := range errs {
				if msg != nil {
					failed = true
				}
			}
			if !failed || executor.Calls() != 1 {
				t.Fatalf("request-scoped failure replayed: attempts=%d failure=%v", executor.Calls(), failed)
			}
		})
	}
}
