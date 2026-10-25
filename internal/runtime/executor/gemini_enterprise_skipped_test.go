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
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestEnterpriseAssistDefaultRespectsFinalPayloadRules(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"builtin", "override", "default", "filter"} {
				t.Run(format.String()+"/"+map[bool]string{false: "full", true: "stream"}[stream]+"/"+mode, func(t *testing.T) {
					cfg := &config.Config{}
					rule := config.PayloadRule{Models: []config.PayloadModelRule{{Name: "gemini-4-argon", Protocol: ge.Provider}}, Params: map[string]any{"assistSkippingMode": "ASSIST_SKIPPING_MODE_UNSPECIFIED"}}
					switch mode {
					case "override":
						cfg.Payload.Override = []config.PayloadRule{rule}
					case "default":
						cfg.Payload.Default = []config.PayloadRule{rule}
					case "filter":
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: rule.Models, Params: []string{"assistSkippingMode"}}}
					}
					calls := 0
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						body, _ := io.ReadAll(r.Body)
						got := gjson.GetBytes(body, "assistSkippingMode")
						want := "REQUEST_ASSIST"
						if mode == "override" || mode == "default" {
							want = "ASSIST_SKIPPING_MODE_UNSPECIFIED"
						}
						if mode == "filter" {
							if got.Exists() {
								t.Fatal("filtered assistant preference was restored")
							}
						} else if got.String() != want {
							t.Fatalf("assistant preference=%s want=%s", got.String(), want)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
					}))
					body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
					if format == translator.FormatGemini {
						body = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
					}
					req := exec.Request{Model: "gemini-4-argon", Payload: body}
					opts := enterpriseOptions(format)
					opts.Stream = stream
					e := NewGeminiEnterpriseExecutor(cfg)
					if stream {
						result, err := e.ExecuteStream(ctx, enterpriseExecutorAuth(), req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						if _, err := e.Execute(ctx, enterpriseExecutorAuth(), req, opts); err != nil {
							t.Fatal(err)
						}
					}
					if calls != 1 {
						t.Fatalf("unexpected generation count: %d", calls)
					}
				})
			}
		}
	}
}

func TestEnterpriseSkippedRepliesBothProtocolsAndStreams(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatGemini} {
		for _, stream := range []bool{false, true} {
			for _, partial := range []bool{false, true} {
				name := format.String() + "/" + map[bool]string{false: "full", true: "stream"}[stream] + map[bool]string{false: "/before", true: "/after"}[partial]
				t.Run(name, func(t *testing.T) {
					body := `{"answer":{"state":"SKIPPED","assistSkippedReasons":["CUSTOMER_POLICY_VIOLATION"],"customerPolicyEnforcementResult":{"private":"synthetic-secret"}},"assistToken":"synthetic-secret"}`
					if partial {
						body = `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"partial answer"}}]}}` + body
					}
					calls := 0
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
					}))
					payload := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
					if format == translator.FormatGemini {
						payload = []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`)
					}
					opts := enterpriseOptions(format)
					opts.Stream = stream
					e := NewGeminiEnterpriseExecutor(&config.Config{})
					req := exec.Request{Model: "gemini-4-argon", Payload: payload}
					var err error
					var data []byte
					succeeded := false
					failures := 0
					if stream {
						var result *exec.StreamResult
						result, err = e.ExecuteStream(ctx, enterpriseExecutorAuth(), req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								data = append(data, chunk.Payload...)
								succeeded = succeeded || gjson.GetBytes(chunk.Payload, "choices.0.finish_reason").Str != "" || gjson.GetBytes(chunk.Payload, "candidates.0.finishReason").Exists() || gjson.GetBytes(chunk.Payload, "usageMetadata").Exists()
								if chunk.Err != nil {
									err = chunk.Err
									failures++
								}
							}
						}
					} else {
						var result exec.Response
						result, err = e.Execute(ctx, enterpriseExecutorAuth(), req, opts)
						data = result.Payload
					}
					var skipped *ge.SkippedError
					if !errors.As(err, &skipped) || skipped.SkipReasons()[0] != "CUSTOMER_POLICY_VIOLATION" || skipped.GenerationStarted() != partial || calls != 1 {
						t.Fatalf("skip response was misclassified: err=%v calls=%d", err, calls)
					}
					if stream && failures != 1 {
						t.Fatalf("terminal errors=%d want=1", failures)
					}
					if stream && partial && !strings.Contains(string(data), "partial answer") {
						t.Fatal("visible output was lost before the skip rejection")
					}
					if succeeded || strings.Contains(string(data), "synthetic-secret") || strings.Contains(err.Error(), "synthetic-secret") {
						t.Fatalf("skipped response fabricated success or leaked details: %s", data)
					}
				})
			}
		}
	}
}
