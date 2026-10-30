package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const enterpriseThoughtAnswer = `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"summary 你好🙂"}},{"content":{"text":"{\"o"}},{"content":{"thought":true,"text":"summary continued"}},{"content":{"text":"k\":true}"}}]},"assistToken":"opaque-secret","sessionInfo":{"session":"opaque-secret"}},{"answer":{"state":"SUCCEEDED"}}]`

func TestEnterpriseThinkingProtocolsStreamsAndUsage(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			for _, level := range []string{"LOW", "HIGH"} {
				wantOutput := helps.NewEnterpriseTokenCounter([]byte(`{"query":{"text":"hi"}}`)).Detail([]map[string]any{{"text": `{"ok":true}`}}).OutputTokens
				thoughtCounter := helps.NewEnterpriseTokenCounter([]byte(`{"query":{"text":"hi"}}`))
				thoughtCounter.ObserveThought("summary 你好🙂summary continued")
				wantReasoning := thoughtCounter.Detail(nil).ReasoningTokens
				for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
					for _, include := range []bool{false, true} {
						for _, stream := range []bool{false, true} {
							name := level + "/" + format.String() + map[bool]string{false: "/hidden", true: "/shown"}[include] + map[bool]string{false: "/full", true: "/stream"}[stream]
							t.Run(name, func(t *testing.T) {
								flag := "false"
								if include {
									flag = "true"
								}
								body := []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `","includeThoughts":` + flag + `}}}`)
								if format == translator.FormatOpenAI {
									body = []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + strings.ToLower(level) + `","extra_body":{"google":{"thinking_config":{"include_thoughts":` + flag + `}}},"stream_options":{"include_usage":true}}`)
								}
								calls := 0
								transport := enterpriseTransport(func(request *http.Request) (*http.Response, error) {
									calls++
									payload, _ := io.ReadAll(request.Body)
									if gjson.GetBytes(payload, "generationSpec.modelId").String() != model || gjson.GetBytes(payload, "generationSpec.thinkingConfig.thinkingLevel").String() != level || gjson.GetBytes(payload, "generationSpec.thinkingConfig.includeThoughts").Bool() != include {
										t.Fatalf("thinking controls did not reach the wire: %s", payload)
									}
									return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseThoughtAnswer))}, nil
								})
								ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
								executor := NewGeminiEnterpriseExecutor(&config.Config{})
								req := coreexec.Request{Model: model, Payload: body}
								opts := enterpriseOptions(format)
								var packets [][]byte
								if stream {
									result, err := executor.ExecuteStream(ctx, enterpriseExecutorAuth(t), req, opts)
									if err != nil {
										t.Fatal(err)
									}
									for chunk := range result.Chunks {
										if chunk.Err != nil {
											t.Fatal(chunk.Err)
										}
										packets = append(packets, chunk.Payload)
									}
								} else {
									response, err := executor.Execute(ctx, enterpriseExecutorAuth(t), req, opts)
									if err != nil {
										t.Fatal(err)
									}
									packets = append(packets, response.Payload)
								}
								if calls != 1 || len(packets) == 0 {
									t.Fatalf("unexpected executions: %d", calls)
								}
								seenThought, outputTokens := false, int64(0)
								for _, packet := range packets {
									seenThought = seenThought || strings.Contains(string(packet), "summary")
									if strings.Contains(string(packet), "opaque-secret") || strings.Contains(string(packet), "cachedContentTokenCount") || strings.Contains(string(packet), "cached_tokens") {
										t.Fatalf("opaque data or fabricated cache usage: %s", packet)
									}
									path := "usageMetadata.candidatesTokenCount"
									if format == translator.FormatOpenAI {
										path = "usage.completion_tokens"
									}
									if value := gjson.GetBytes(packet, path); value.Exists() {
										outputTokens = value.Int()
										if format == translator.FormatOpenAI {
											outputTokens -= wantReasoning
										}
										if outputTokens != wantOutput {
											t.Fatal("body and thought token buckets were not kept separate")
										}
									}
								}
								if seenThought != include || outputTokens != wantOutput {
									t.Fatalf("incorrect visibility or missing usage: shown=%v output=%d", seenThought, outputTokens)
								}
								firstPath := "candidates.0.content.parts.0.thought"
								if format == translator.FormatOpenAI {
									firstPath = "choices.0.message.reasoning_content"
									if stream {
										firstPath = "choices.0.delta.reasoning_content"
									}
								}
								if gjson.GetBytes(packets[0], firstPath).Exists() != include {
									t.Fatalf("incorrect first packet representation: %s", packets[0])
								}
							})
						}
					}
				}
			}
		})
	}
}

func TestEnterpriseThinkingFinalPayloadBarrier(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			models := []config.PayloadModelRule{{Name: model, Protocol: "gemini-enterprise"}}
			for _, filter := range []bool{false, true} {
				for _, stream := range []bool{false, true} {
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"generationSpec.thinkingConfig.thinkingLevel": "LOW", "generationSpec.thinkingConfig.includeThoughts": false}}}}}
					if filter {
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"generationSpec.thinkingConfig"}}}
					}
					transport := enterpriseTransport(func(request *http.Request) (*http.Response, error) {
						payload, _ := io.ReadAll(request.Body)
						if filter {
							if gjson.GetBytes(payload, "generationSpec.thinkingConfig").Exists() {
								t.Fatal("thinking was restored after user filtering")
							}
						} else if gjson.GetBytes(payload, "generationSpec.thinkingConfig.thinkingLevel").String() != "LOW" || gjson.GetBytes(payload, "generationSpec.thinkingConfig.includeThoughts").Bool() {
							t.Fatal("thinking overwrote the final user payload")
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseThoughtAnswer))}, nil
					})
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
					req := coreexec.Request{Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true}}}`)}
					executor := NewGeminiEnterpriseExecutor(cfg)
					opts := enterpriseOptions(translator.FormatGemini)
					var packets [][]byte
					if stream {
						result, err := executor.ExecuteStream(ctx, enterpriseExecutorAuth(t), req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							packets = append(packets, chunk.Payload)
						}
					} else {
						result, err := executor.Execute(ctx, enterpriseExecutorAuth(t), req, opts)
						if err != nil {
							t.Fatal(err)
						}
						packets = append(packets, result.Payload)
					}
					for _, packet := range packets {
						if strings.Contains(string(packet), "summary") {
							t.Fatal("response visibility ignored the final payload setting")
						}
					}
				}
			}
		})
	}
}

func TestEnterpriseThinkingSuffixAndSnakeAliases(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			transport := enterpriseTransport(func(request *http.Request) (*http.Response, error) {
				payload, _ := io.ReadAll(request.Body)
				if gjson.GetBytes(payload, "generationSpec.modelId").String() != model || gjson.GetBytes(payload, "generationSpec.thinkingConfig.thinkingLevel").String() != "LOW" || gjson.GetBytes(payload, "generationSpec.thinkingConfig.includeThoughts").Bool() {
					t.Fatalf("incorrect suffix or source aliases: %s", payload)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			_, err := NewGeminiEnterpriseExecutor(&config.Config{}).Execute(ctx, enterpriseExecutorAuth(t), coreexec.Request{Model: model + "(low)", Payload: []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generation_config":{"thinking_config":{"thinking_level":"HIGH","include_thoughts":false}}}`)}, enterpriseOptions(translator.FormatGemini))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnterpriseThinkingJSONAcrossProtocols(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
				for _, stream := range []bool{false, true} {
					body := map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": "hi"}}}}, "generationConfig": map[string]any{"responseMimeType": "application/json", "thinkingConfig": map[string]any{"thinkingLevel": "HIGH", "includeThoughts": true}}}
					if format == translator.FormatOpenAI {
						body = map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}, "reasoning_effort": "high", "response_format": map[string]string{"type": "json_object"}}
					}
					encoded, _ := json.Marshal(body)
					transport := enterpriseTransport(func(request *http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseThoughtAnswer))}, nil
					})
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
					req := coreexec.Request{Model: model, Payload: encoded}
					executor := NewGeminiEnterpriseExecutor(&config.Config{})
					var packets [][]byte
					if stream {
						response, err := executor.ExecuteStream(ctx, enterpriseExecutorAuth(t), req, enterpriseOptions(format))
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							packets = append(packets, chunk.Payload)
						}
					} else {
						response, err := executor.Execute(ctx, enterpriseExecutorAuth(t), req, enterpriseOptions(format))
						if err != nil {
							t.Fatal(err)
						}
						packets = append(packets, response.Payload)
					}
					foundSummary, foundJSON := false, false
					for _, packet := range packets {
						foundSummary = foundSummary || strings.Contains(string(packet), "summary")
						if format == translator.FormatGemini {
							for _, part := range gjson.GetBytes(packet, "candidates.0.content.parts").Array() {
								if !part.Get("thought").Bool() && part.Get("text").String() == `{"ok":true}` {
									foundJSON = true
								}
							}
						} else {
							path := "choices.0.message.content"
							if stream {
								path = "choices.0.delta.content"
							}
							foundJSON = foundJSON || gjson.GetBytes(packet, path).String() == `{"ok":true}`
						}
					}
					if !foundSummary || !foundJSON {
						t.Fatal("JSON and thoughts were not kept separate")
					}
				}
			}
		})
	}
}

func TestEnterpriseInvalidThinkingDoesNotCallGoogle(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			for _, controls := range []string{`{"thinkingLevel":"MEDIUM"}`, `{"thinkingLevel":7}`, `{"includeThoughts":"true"}`, `{"thinkingBudget":128}`} {
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
					t.Fatal("invalid local controls reached Google")
					return nil, nil
				}))
				_, err := NewGeminiEnterpriseExecutor(&config.Config{}).Execute(ctx, enterpriseExecutorAuth(t), coreexec.Request{Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":` + controls + `}}`)}, enterpriseOptions(translator.FormatGemini))
				if err == nil {
					t.Fatalf("invalid thinking controls accepted: %s", controls)
				}
			}
		})
	}
}

func TestEnterpriseUnverifiedModelsKeepThinkingIgnored(t *testing.T) {
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(request *http.Request) (*http.Response, error) {
		payload, _ := io.ReadAll(request.Body)
		if gjson.GetBytes(payload, "generationSpec.thinkingConfig").Exists() {
			t.Fatal("unverified model received new thinking controls")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseThoughtAnswer))}, nil
	}))
	response, err := NewGeminiEnterpriseExecutor(&config.Config{}).Execute(ctx, enterpriseExecutorAuth(t), coreexec.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true}}}`)}, enterpriseOptions(translator.FormatGemini))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response.Payload), "summary") {
		t.Fatal("unverified model changed its default thought visibility")
	}
}

func TestEnterpriseThinkingRespectsExplicitRegisteredCapability(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			registry.GetGlobalRegistry().RegisterClient("enterprise-thinking-explicit-disabled-"+model, "gemini-enterprise", []*registry.ModelInfo{{ID: model, Type: "gemini", ExplicitThinking: true}})
			defer registry.GetGlobalRegistry().UnregisterClient("enterprise-thinking-explicit-disabled-" + model)
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(request *http.Request) (*http.Response, error) {
				payload, _ := io.ReadAll(request.Body)
				if gjson.GetBytes(payload, "generationSpec.thinkingConfig").Exists() {
					t.Fatal("built-in support overrode an explicit registered capability")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
			}))
			_, err := NewGeminiEnterpriseExecutor(&config.Config{}).Execute(ctx, enterpriseExecutorAuth(t), coreexec.Request{Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true}}}`)}, enterpriseOptions(translator.FormatGemini))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
