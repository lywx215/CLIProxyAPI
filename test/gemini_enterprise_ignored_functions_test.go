package test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func enterpriseIgnoredFunctionsRequest(protocol string, history, builtins bool) map[string]any {
	declaration := map[string]any{"name": "unused-private-function", "description": "unused-private-description", "parameters": map[string]any{"type": "object", "properties": map[string]any{"private-default": map[string]string{"type": "string", "default": "unused-private-default"}}}}
	if protocol == "openai" {
		tools := []map[string]any{{"type": "function", "function": declaration}}
		if builtins {
			tools = append(tools, map[string]any{"google_search": map[string]any{}})
		}
		messages := []map[string]any{{"role": "user", "content": "history-question"}}
		if history {
			messages = append(messages,
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"type": "function", "id": "history-call-17", "function": map[string]string{"name": "historical_add", "arguments": `{"a":2,"b":3}`}}}},
				map[string]any{"role": "tool", "tool_call_id": "history-call-17", "content": `{"result":5,"note":"前🙂"}`})
		}
		return map[string]any{"messages": messages, "tools": tools, "tool_choice": "required", "parallel_tool_calls": true}
	}
	tools := map[string]any{"functionDeclarations": []map[string]any{declaration}}
	if builtins {
		tools["googleSearch"] = map[string]any{}
	}
	contents := []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "history-question"}}}}
	if history {
		contents = append(contents,
			map[string]any{"role": "model", "parts": []map[string]any{{"functionCall": map[string]any{"id": "history-call-17", "name": "historical_add", "args": map[string]int{"a": 2, "b": 3}}}}},
			map[string]any{"role": "user", "parts": []map[string]any{{"functionResponse": map[string]any{"id": "history-call-17", "name": "historical_add", "response": map[string]any{"result": 5, "note": "前🙂"}}}}})
	}
	return map[string]any{"contents": contents, "tools": []map[string]any{tools}, "toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []string{"unused-private-function"}}}}
}

func TestEnterpriseIgnoredFunctionsPublicAllModels(t *testing.T) {
	cfg := &config.Config{}
	cfg.PassthroughHeaders = true
	router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
	for _, model := range enterpriseFixtureModels {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				for _, scenario := range []struct {
					name              string
					history, builtins bool
				}{{"declarations", false, false}, {"history", true, false}, {"mixed-builtins", true, true}} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", model, protocol, stream, scenario.name), func(t *testing.T) {
						path, body := enterprisePublicRequest(t, model, protocol, stream, enterpriseIgnoredFunctionsRequest(protocol, scenario.history, scenario.builtins))
						before := transport.calls.Load()
						response := enterprisePublicCall(router, path, body)
						if response.Code != 200 || transport.calls.Load() != before+1 || !strings.Contains(enterprisePublicText(response.Body.Bytes(), protocol, stream), "offline") {
							t.Fatalf("ignored functions did not become one text request: status=%d body=%s", response.Code, response.Body.String())
						}
						ignored := response.Header().Get("X-CLIProxyAPI-Ignored-Parameters")
						if !strings.Contains(ignored, "tools[0]") || !strings.Contains(ignored, map[string]string{"openai": "tool_choice", "gemini": "toolConfig"}[protocol]) {
							t.Fatalf("ignored field notice missing: %s", ignored)
						}
						wire := transport.requestBody()
						query := gjson.GetBytes(wire, "query.text").String()
						if !strings.Contains(query, "custom functions are unavailable") || gjson.GetBytes(wire, "tools").Exists() || strings.Contains(string(wire), "unused-private-") || strings.Contains(ignored, "unused-private-") {
							t.Fatal("ignored declaration was forwarded or downgrade was not explained")
						}
						if strings.Contains(response.Body.String(), `"tool_calls"`) || strings.Contains(response.Body.String(), `"functionCall"`) || strings.Contains(response.Body.String(), `"finish_reason":"tool_calls"`) {
							t.Fatal("text response fabricated a client function call")
						}
						if scenario.history {
							var context struct {
								History []map[string]string `json:"conversation"`
							}
							if err := json.Unmarshal([]byte(query[strings.LastIndex(query, "\n")+1:]), &context); err != nil {
								t.Fatal(err)
							}
							if len(context.History) != 3 || context.History[0]["text"] != "history-question" || !strings.Contains(context.History[1]["text"], "historical_add") || !strings.Contains(context.History[2]["text"], "history-call-17") || !strings.Contains(context.History[2]["text"], "前🙂") {
								t.Fatal("history entries, calls or supplied function results were lost")
							}
						}
						if gjson.GetBytes(wire, "toolsSpec.webGroundingSpec").Exists() != scenario.builtins {
							t.Fatal("supported built-in tools changed while ignoring functions")
						}
					})
				}
			}
		}
	}
}

func TestEnterpriseIgnoredFunctionsFinalPayloadAndJSON(t *testing.T) {
	for _, protocol := range []string{"openai", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"override", "filter", "json"} {
				cfg := &config.Config{}
				rules := []config.PayloadModelRule{{Name: "gemini-*", Protocol: "gemini-enterprise"}}
				if mode == "override" {
					cfg.Payload.Override = []config.PayloadRule{{Models: rules, Params: map[string]any{"query.text": "final configured text"}}}
				} else if mode == "filter" {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: rules, Params: []string{"query.text"}}}
				}
				router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
				fields := enterpriseIgnoredFunctionsRequest(protocol, true, false)
				if mode == "json" {
					if protocol == "openai" {
						fields["response_format"] = map[string]string{"type": "json_object"}
					} else {
						fields["generationConfig"] = map[string]string{"responseMimeType": "application/json"}
					}
				}
				path, body := enterprisePublicRequest(t, "gemini-3.1-pro-preview", protocol, stream, fields)
				response := enterprisePublicCall(router, path, body)
				if mode == "filter" {
					if response.Code != 400 || transport.calls.Load() != 0 || gjson.GetBytes(response.Body.Bytes(), "error.param").String() != "query.text" {
						t.Fatal("ignored functions restored a filtered final payload")
					}
					enterpriseAssertPublicErrorChannelNeutral(t, response.Body.Bytes())
					continue
				}
				if response.Code != 200 {
					t.Fatalf("compatible final request failed: %d", response.Code)
				}
				if mode == "override" && gjson.GetBytes(transport.requestBody(), "query.text").String() != "final configured text" {
					t.Fatal("function warning overwrote final configuration")
				}
				if mode == "json" && !json.Valid([]byte(enterprisePublicText(response.Body.Bytes(), protocol, stream))) {
					t.Fatal("ignoring functions broke the requested JSON response")
				}
			}
		}
	}
}
