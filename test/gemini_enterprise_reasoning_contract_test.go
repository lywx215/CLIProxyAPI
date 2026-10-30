package test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	stats "github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

type enterpriseReasoningContractCapture struct {
	statistics *stats.RequestStatistics
	rows       chan coreusage.Record
}

type enterpriseReasoningContractNoop struct{}

func (enterpriseReasoningContractNoop) HandleUsage(context.Context, coreusage.Record) {}

func (c *enterpriseReasoningContractCapture) HandleUsage(ctx context.Context, record coreusage.Record) {
	if record.AuthID != "enterprise-wire-fixture" {
		return
	}
	record.APIKey = "preview"
	c.statistics.Record(ctx, record)
	c.rows <- record
}

func TestEnterpriseReasoningPublicManagementContract(t *testing.T) {
	capture := &enterpriseReasoningContractCapture{statistics: stats.NewRequestStatistics(), rows: make(chan coreusage.Record, 32)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), enterpriseReasoningContractNoop{}) })
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-3.8-flash", Protocol: "gemini-enterprise"}, {Name: "gemini-3.1-pro-preview", Protocol: "gemini-enterprise"}}, Params: map[string]any{"generationSpec.thinkingConfig.includeThoughts": true}}}}}
	router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
	var publicResponses []map[string]any
	for _, scenario := range []string{"reported", "summary", "zero", "unknown"} {
		model := "gemini-3.8-flash"
		if scenario == "summary" || scenario == "zero" {
			model = "gemini-3.1-pro-preview"
		}
		for _, protocol := range []string{"gemini", "openai"} {
			for _, stream := range []bool{false, true} {
				t.Run(scenario+"/"+protocol+"/"+map[bool]string{false: "full", true: "stream"}[stream], func(t *testing.T) {
					thought := `,{"content":{"thought":true,"text":"思考 summary🙂"}}`
					report := ""
					if scenario == "reported" {
						report = `,"usageMetadata":{"thoughtsTokenCount":50}`
					}
					if scenario == "zero" {
						report = `,"usageMetadata":{"thoughtsTokenCount":0}`
					}
					if scenario == "unknown" {
						thought = ""
					}
					transport.upstreamStatus = 200
					transport.upstreamBody = `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"fixture answer"}}` + thought + `]}},{"answer":{"state":"SUCCEEDED"}` + report + `}]`
					fields := map[string]any{"generationConfig": map[string]any{"thinkingConfig": map[string]any{"includeThoughts": false}}}
					if protocol == "openai" {
						fields = map[string]any{"stream_options": map[string]any{"include_usage": true}, "extra_body": map[string]any{"google": map[string]any{"thinking_config": map[string]any{"include_thoughts": false}}}}
					}
					path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
					before := transport.calls.Load()
					response := enterprisePublicCall(router, path, body)
					if response.Code != 200 || transport.calls.Load() != before+1 {
						t.Fatalf("unexpected status or replay: %d %s", response.Code, response.Body.String())
					}
					var record coreusage.Record
					select {
					case record = <-capture.rows:
					case <-time.After(5 * time.Second):
						t.Fatal("no request record")
					}
					d := record.Detail
					if d.TokenPresence.ReasoningTokens != (scenario != "unknown") || d.TokenPresence.CachedTokens || d.TotalTokens != d.InputTokens+d.OutputTokens+d.ReasoningTokens {
						t.Fatalf("wrong canonical detail: %+v", d)
					}
					if scenario == "reported" && d.ReasoningTokens != 50 || scenario == "zero" && d.ReasoningTokens != 0 || scenario == "summary" && d.ReasoningTokens <= 0 {
						t.Fatal("wrong precedence")
					}
					payloads := []string{response.Body.String()}
					if stream {
						payloads = nil
						for _, line := range strings.Split(response.Body.String(), "\n") {
							if strings.HasPrefix(line, "data:") {
								payloads = append(payloads, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
							}
						}
					}
					usagePackets := 0
					for _, packet := range payloads {
						root, input, output, reason, total := "usage", "prompt_tokens", "completion_tokens", "completion_tokens_details.reasoning_tokens", "total_tokens"
						wantO := d.OutputTokens + d.ReasoningTokens
						if protocol == "gemini" {
							root, output, reason = "usageMetadata", "candidatesTokenCount", "thoughtsTokenCount"
							input, total = "promptTokenCount", "totalTokenCount"
							wantO = d.OutputTokens
						}
						if gjson.Get(packet, root).Exists() {
							usagePackets++
							if gjson.Get(packet, root+"."+input).Int() != d.InputTokens || gjson.Get(packet, root+"."+output).Int() != wantO || gjson.Get(packet, root+"."+total).Int() != d.TotalTokens || gjson.Get(packet, root+"."+reason).Exists() != d.TokenPresence.ReasoningTokens || d.TokenPresence.ReasoningTokens && gjson.Get(packet, root+"."+reason).Int() != d.ReasoningTokens {
								t.Fatal("public token semantics differ from management")
							}
						}
					}
					if usagePackets != 1 || stream && protocol == "openai" && strings.Count(response.Body.String(), "data: [DONE]") != 1 {
						t.Fatal("invalid stream termination or usage")
					}
					counter := helps.NewEnterpriseTokenCounter(transport.requestBody())
					if d.InputTokens != counter.Detail(nil).InputTokens {
						t.Fatal("input was not counted after payload rules")
					}
					publicResponses = append(publicResponses, map[string]any{"scenario": scenario, "protocol": protocol, "stream": stream, "model": model, "response": response.Body.String()})
				})
			}
		}
	}
	snapshot := capture.statistics.Snapshot()
	if snapshot.TotalRequests != 16 || snapshot.SuccessCount != 16 {
		t.Fatal("usage publication was not once per execution")
	}
	if path := os.Getenv("CPA_ENTERPRISE_REASONING_BROWSER_FIXTURE"); path != "" {
		data, err := json.MarshalIndent(map[string]any{"usage": snapshot, "public_responses": publicResponses, "synthetic_google_requests": transport.calls.Load()}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
