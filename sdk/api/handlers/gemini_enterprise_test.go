package handlers

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

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
