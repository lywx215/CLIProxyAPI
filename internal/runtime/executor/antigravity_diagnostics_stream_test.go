package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/diagnostics"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityStreamDiagnosticsSettleBeforeDeliveryCompletion(t *testing.T) {
	usage.StartDefault(context.Background())
	cache.CacheSignature(t.Name(), "diagnostics fixture", strings.Repeat("x", cache.MinValidSignatureLen))
	defer cache.ClearSignatureCache(t.Name())
	for _, tc := range []struct {
		name, frame, result string
		end                 error
		wantError           bool
		wantConverted       bool
	}{
		{"eof", `{"response":{"candidates":[{"content":{"parts":[{"text":"fixture output"}]},"finishReason":"STOP"}]}}`, "success", io.EOF, false, true},
		{"missing-terminal", `{"response":{"candidates":[{"content":{"parts":[{"text":"fixture output"}]}}]}}`, "incomplete", io.EOF, false, true},
		{"malformed", `{"response":`, "incomplete", io.EOF, false, false},
		{"error-frame", `{"error":{"code":500,"message":"fixture failure"}}`, "error", io.EOF, true, false},
		{"read-error", `{"response":{"candidates":[{"content":{"parts":[{"text":"fixture output"}]}}]}}`, "error", errors.New("fixture read failure"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attemptStarted, releaseAttempt := make(chan struct{}), make(chan struct{})
				var mu sync.Mutex
				var records []diagnostics.Record
				engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
					var record diagnostics.Record
					if err := json.Unmarshal(b[6:], &record); err != nil {
						return err
					}
					if record.Event == "upstream.attempt_finished" {
						close(attemptStarted)
						<-releaseAttempt
					}
					mu.Lock()
					records = append(records, record)
					mu.Unlock()
					return nil
				})
				ctx, span := engine.StartServer(context.Background(), nil, "antigravity-stream-fixture")
				ctx, finishDelivery := usage.WithStreamDelivery(ctx)
				defer finishDelivery(nil)
				body := &semanticBody{chunks: [][]byte{[]byte("data: " + tc.frame + "\n\n")}, end: tc.end}
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", semanticRT(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
				}))
				auth := &cliproxyauth.Auth{
					ID:         "antigravity-stream-fixture",
					Attributes: map[string]string{"base_url": "http://fixture.invalid"},
					Metadata: map[string]any{
						"access_token":    "fixture-token",
						"expired":         time.Now().Add(time.Hour).Format(time.RFC3339),
						"project_id":      "fixture-project",
						"disable_cooling": true,
					},
				}
				result, errExecute := NewAntigravityExecutor(&config.Config{}).ExecuteStream(ctx, auth,
					cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: true})
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				boundaryReached := make(chan struct{})
				var streamErr error
				go func() {
					defer close(boundaryReached)
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							streamErr = chunk.Err
							return
						}
					}
				}()
				// Hold the synchronous diagnostic sink. Neither EOF nor Err may let
				// the consumer seal the span before this observation has settled.
				synctest.Wait()
				select {
				case <-attemptStarted:
				default:
					t.Error("stream terminal bypassed diagnostic settlement")
				}
				select {
				case <-boundaryReached:
					t.Error("consumer reached EOF or Err while diagnostics were unsettled")
				default:
				}
				close(releaseAttempt)
				<-boundaryReached
				if (streamErr != nil) != tc.wantError {
					t.Errorf("stream error = %v, want error %t", streamErr, tc.wantError)
				}
				span.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
				synctest.Wait()
				if body.closed.Load() {
					t.Error("delivery accounting no longer waits for consumer acknowledgment")
				}
				mu.Lock()
				attempts, converted, servers := 0, 0, 0
				for _, record := range records {
					d, _ := record.Data.(map[string]any)
					switch record.Event {
					case "upstream.attempt_finished":
						attempts++
						if servers != 0 || d["resultClass"] != tc.result {
							t.Errorf("upstream diagnostic = %v, want %s before server terminal", d, tc.result)
						}
					case "response.converted":
						converted++
						if servers != 0 {
							t.Error("conversion diagnostic followed server terminal")
						}
					case "diag.server":
						servers++
						if d["coverage"].(map[string]any)["debugCapture"] != "enabled_throughout" {
							t.Errorf("settled stream was marked interrupted: %v", d)
						}
					}
				}
				before := len(records)
				mu.Unlock()
				if attempts != 1 || servers != 1 || (converted == 1) != tc.wantConverted || converted > 1 {
					t.Errorf("attempts=%d converted=%d servers=%d", attempts, converted, servers)
				}
				finishDelivery(nil)
				synctest.Wait()
				if !body.closed.Load() {
					t.Error("upstream cleanup was lost")
				}
				mu.Lock()
				for _, record := range records[before:] {
					if record.SpanKind == "server" {
						t.Error("delivery acknowledgment emitted a late server diagnostic")
					}
				}
				mu.Unlock()
			})
		})
	}
}
