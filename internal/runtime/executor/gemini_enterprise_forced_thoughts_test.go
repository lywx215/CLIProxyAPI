package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func enterpriseForcedThoughtConfig(t *testing.T) *config.Config {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "gemini-enterprise-force-thoughts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseConfigBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Payload.Override) != 1 || len(cfg.Payload.Override[0].Params) != 1 {
		t.Fatal("the configuration must override summary visibility only")
	}
	return cfg
}

func TestEnterpriseForcedThoughtsPreserveLevelsAndCompleteStreams(t *testing.T) {
	cfg := enterpriseForcedThoughtConfig(t)
	for _, model := range []string{"gemini-3.8-flash", "gemini-3.1-pro-preview"} {
		for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
			for _, level := range []string{"", "LOW", "HIGH"} {
				for _, visibility := range []string{"absent", "false", "true"} {
					for _, hasSummary := range []bool{false, true} {
						name := model + "/" + format.String() + "/level=" + level + "/client=" + visibility
						if hasSummary {
							name += "/summary"
						} else {
							name += "/no-summary"
						}
						t.Run(name, func(t *testing.T) {
							thinkingConfig := map[string]any{}
							if visibility != "absent" {
								thinkingConfig["includeThoughts"] = visibility == "true"
							}
							body := map[string]any{
								"contents":         []any{map[string]any{"parts": []any{map[string]any{"text": "hi"}}}},
								"generationConfig": map[string]any{"thinkingConfig": thinkingConfig},
							}
							if level != "" {
								thinkingConfig["thinkingLevel"] = level
							}
							if format == translator.FormatOpenAI {
								googleConfig := map[string]any{}
								if visibility != "absent" {
									googleConfig["include_thoughts"] = visibility == "true"
								}
								body = map[string]any{
									"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
									"extra_body":     map[string]any{"google": map[string]any{"thinking_config": googleConfig}},
									"stream_options": map[string]any{"include_usage": true},
								}
								if level != "" {
									body["reasoning_effort"] = strings.ToLower(level)
								}
							}
							payload, err := json.Marshal(body)
							if err != nil {
								t.Fatal(err)
							}
							upstream, wantThought, wantText := enterpriseAnswer, "", "hello"
							if hasSummary {
								upstream, wantThought, wantText = enterpriseThoughtAnswer, "summary 你好🙂summary continued", `{"ok":true}`
							}
							calls := 0
							transport := enterpriseTransport(func(request *http.Request) (*http.Response, error) {
								calls++
								wire, errRead := io.ReadAll(request.Body)
								if errRead != nil {
									t.Fatal(errRead)
								}
								if gjson.GetBytes(wire, "generationSpec.thinkingConfig.includeThoughts").Type != gjson.True || gjson.GetBytes(wire, "generationSpec.modelId").String() != model {
									t.Fatal("the final override did not enable summaries for the selected model")
								}
								gotLevel := gjson.GetBytes(wire, "generationSpec.thinkingConfig.thinkingLevel")
								if gotLevel.String() != level || gotLevel.Exists() != (level != "") || gjson.GetBytes(wire, "generationSpec.thinkingConfig.thinkingBudget").Exists() {
									t.Fatal("forcing summaries changed the original thinking strength")
								}
								return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(upstream)))}, nil
							})
							ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
							response, err := NewGeminiEnterpriseExecutor(cfg).ExecuteStream(ctx, enterpriseExecutorAuth(t), coreexec.Request{Model: model, Payload: payload}, enterpriseOptions(format))
							if err != nil {
								t.Fatal(err)
							}
							var thought, text strings.Builder
							terminals, usages := 0, 0
							for chunk := range response.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								packet := gjson.ParseBytes(chunk.Payload)
								usagePath := "usageMetadata"
								if format == translator.FormatGemini {
									for _, part := range packet.Get("candidates.0.content.parts").Array() {
										if part.Get("thought").Bool() {
											thought.WriteString(part.Get("text").String())
										} else {
											text.WriteString(part.Get("text").String())
										}
									}
									if packet.Get("candidates.0.finishReason").String() == "STOP" {
										terminals++
									}
								} else {
									usagePath = "usage"
									thought.WriteString(packet.Get("choices.0.delta.reasoning_content").String())
									text.WriteString(packet.Get("choices.0.delta.content").String())
									if packet.Get("choices.0.finish_reason").String() == "stop" {
										terminals++
									}
								}
								if packet.Get(usagePath).Exists() {
									usages++
								}
								if strings.Contains(string(chunk.Payload), "opaque-secret") || packet.Get("usageMetadata.cachedContentTokenCount").Exists() || packet.Get("usage.prompt_tokens_details.cached_tokens").Exists() {
									t.Fatal("the stream leaked opaque data or fabricated cache usage")
								}
								if packet.Get(usagePath).Exists() {
									path := "usageMetadata.thoughtsTokenCount"
									if format == translator.FormatOpenAI {
										path = "usage.completion_tokens_details.reasoning_tokens"
									}
									if packet.Get(path).Exists() != hasSummary {
										t.Fatal("summary availability was lost")
									}
								}
							}
							if calls != 1 || thought.String() != wantThought || text.String() != wantText || terminals != 1 || usages != 1 {
								t.Fatalf("calls=%d thought=%q text=%q terminals=%d usages=%d", calls, thought.String(), text.String(), terminals, usages)
							}
						})
					}
				}
			}
		}
	}
}

func TestEnterpriseForcedThoughtRuleIsScoped(t *testing.T) {
	cfg := enterpriseForcedThoughtConfig(t)
	for _, tc := range []struct{ model, protocol string }{
		{"gemini-2.5-pro", "gemini-enterprise"},
		{"gemini-4-argon", "gemini-enterprise"},
		{"gemini-3.8-flash", "gemini"},
		{"gemini-3.1-pro-preview", "gemini"},
	} {
		t.Run(tc.model+"/"+tc.protocol, func(t *testing.T) {
			payload := []byte(`{"generationSpec":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":false}}}`)
			out := helps.ApplyPayloadConfigWithRequestForExecutor(cfg, tc.protocol, tc.model, tc.protocol, "gemini", "", payload, payload, tc.model, "", nil)
			if string(out) != string(payload) {
				t.Fatal("the override affected another model or provider")
			}
		})
	}
}
