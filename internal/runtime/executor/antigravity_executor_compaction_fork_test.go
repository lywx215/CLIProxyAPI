package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityCompactionPreservesForkCreditsAndAlias(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, disableCooling := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/disable-cooling=%v", streaming, disableCooling), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil || gjson.GetBytes(body, "enabledCreditTypes.0").String() != "GOOGLE_ONE_AI" {
						t.Error("compaction summary request lost forced credits")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Summary\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":20,\"candidatesTokenCount\":10,\"totalTokenCount\":30}}}\n\n")
				}))
				defer server.Close()
				executor := NewAntigravityExecutor(&config.Config{
					DisableCooling: disableCooling,
					QuotaExceeded:  config.QuotaExceeded{AntigravityCreditsForce: true},
				})
				credential := &cliproxyauth.Auth{
					ID: t.Name(), Provider: "antigravity",
					Metadata:   map[string]any{"access_token": "synthetic", "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "project_id": "synthetic"},
					Attributes: map[string]string{"base_url": server.URL},
				}
				ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx := context.WithValue(context.Background(), "gin", ginCtx)
				req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"input":[{"role":"user","content":"summarize"},{"type":"compaction_trigger"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "client-visible-alias"}}
				var output strings.Builder
				if streaming {
					stream, err := executor.ExecuteStream(ctx, credential, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output.Write(chunk.Payload)
					}
				} else {
					resp, err := executor.Execute(ctx, credential, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					output.Write(resp.Payload)
				}
				if calls.Load() != 1 || !helps.CreditsUsed(ctx) {
					t.Fatalf("summary calls=%d credits=%v, want one credited request", calls.Load(), helps.CreditsUsed(ctx))
				}
				if !strings.Contains(output.String(), `"model":"client-visible-alias"`) || !strings.Contains(output.String(), `"total_tokens":30`) {
					t.Fatalf("compaction lost alias or usage: %s", output.String())
				}
			})
		}
	}
}
