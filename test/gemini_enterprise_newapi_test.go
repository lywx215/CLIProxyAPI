package test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func enterpriseNewAPIHistory(parts []map[string]any) map[string]any {
	return map[string]any{"contents": []map[string]any{
		{"role": "user", "parts": []map[string]string{{"text": "previous question"}}},
		{"role": "model", "parts": parts},
		{"role": "user", "parts": []map[string]string{{"text": "continue"}}},
	}}
}

func TestEnterpriseNewAPITextHistorySignature(t *testing.T) {
	cfg := &config.Config{}
	cfg.PassthroughHeaders = true
	router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
	for _, model := range enterpriseFixtureModels {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				fields := enterpriseNewAPIHistory([]map[string]any{{"text": "previous answer", "thoughtSignature": "context_engineering_is_the_way_to_go", "thought": false}})
				path, body := enterprisePublicRequest(t, model, "gemini", stream, fields)
				before := transport.calls.Load()
				response := enterprisePublicCall(router, path, body)
				if response.Code != 200 || transport.calls.Load() != before+1 || !strings.Contains(enterprisePublicText(response.Body.Bytes(), "gemini", stream), "offline") {
					t.Fatalf("new-api text history was rejected: status=%d body=%s", response.Code, response.Body.String())
				}
				wire := string(transport.requestBody())
				if !strings.Contains(wire, "previous answer") || strings.Contains(wire, "context_engineering_is_the_way_to_go") || strings.Contains(wire, "thoughtSignature") {
					t.Fatalf("text or opaque signature handling is wrong: %s", wire)
				}
				if !strings.Contains(response.Header().Get("X-CLIProxyAPI-Ignored-Parameters"), "contents[1].parts[0].thoughtSignature") {
					t.Fatal("ignored signature field is not reported")
				}
			})
		}
	}
}

func TestEnterpriseGeminiOwnResponseHistory(t *testing.T) {
	router, transport, _ := enterpriseFixtureRouter(t)
	for _, stream := range []bool{false, true} {
		path, body := enterprisePublicRequest(t, "gemini-3.1-pro-preview", "gemini", false, nil)
		first := enterprisePublicCall(router, path, body)
		if first.Code != 200 {
			t.Fatalf("first response failed: %s", first.Body.String())
		}
		var parts []map[string]any
		if err := json.Unmarshal([]byte(gjson.GetBytes(first.Body.Bytes(), "candidates.0.content.parts").Raw), &parts); err != nil {
			t.Fatal(err)
		}
		if len(parts) < 3 {
			t.Fatal("fixture did not emit code, result and visible text")
		}
		fields := enterpriseNewAPIHistory(parts)
		path, body = enterprisePublicRequest(t, "gemini-3.1-pro-preview", "gemini", stream, fields)
		second := enterprisePublicCall(router, path, body)
		if second.Code != 200 {
			t.Fatalf("own response history was rejected: status=%d body=%s", second.Code, second.Body.String())
		}
		query := gjson.GetBytes(transport.requestBody(), "query.text").String()
		for _, want := range []string{"print(7)", "OUTCOME_OK", "offline 前🙂引用"} {
			if !strings.Contains(query, want) {
				t.Fatalf("history lost %q: %s", want, query)
			}
		}
	}
}

func TestEnterpriseLongHistoryPublicRoutes(t *testing.T) {
	router, transport, _ := enterpriseFixtureRouter(t)
	contents := make([]map[string]any, 0, 513)
	messages := make([]map[string]any, 0, 513)
	for i := range 513 {
		role, chatRole := "user", "user"
		if i%2 == 1 {
			role, chatRole = "model", "assistant"
		}
		text := fmt.Sprintf("history-turn-%d", i)
		contents = append(contents, map[string]any{"role": role, "parts": []map[string]string{{"text": text, "thoughtSignature": "opaque-signature"}}})
		messages = append(messages, map[string]any{"role": chatRole, "content": text})
	}
	for _, model := range enterpriseFixtureModels {
		for _, protocol := range []string{"gemini", "openai"} {
			for _, stream := range []bool{false, true} {
				fields := map[string]any{"contents": contents}
				if protocol == "openai" {
					fields = map[string]any{"messages": messages}
				}
				path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
				response := enterprisePublicCall(router, path, body)
				if response.Code != 200 {
					t.Fatalf("%s %s stream=%t long history: status=%d body=%s", model, protocol, stream, response.Code, response.Body.String())
				}
				query := gjson.GetBytes(transport.requestBody(), "query.text").String()
				for _, want := range []string{"history-turn-0", "history-turn-257", "history-turn-512"} {
					if !strings.Contains(query, want) {
						t.Fatalf("long conversation lost %q", want)
					}
				}
			}
		}
	}
}

func TestEnterpriseMultiMiBContinuationPublicRoutes(t *testing.T) {
	router, transport, _ := enterpriseFixtureRouter(t)
	contents := make([]map[string]any, 0, 514)
	messages := make([]map[string]any, 0, 514)
	want := make([]map[string]string, 0, 514)
	for i := range 514 {
		role, chatRole := "user", "user"
		if i%2 == 1 {
			role, chatRole = "model", "assistant"
		}
		text := fmt.Sprintf("history-turn-%d 前🙂\n<>& start", i) + strings.Repeat(" ", 1100) + "middle" + strings.Repeat(" ", 1100) + "end"
		contents = append(contents, map[string]any{"role": role, "parts": []map[string]string{{"text": text}}})
		messages = append(messages, map[string]any{"role": chatRole, "content": text})
		want = append(want, map[string]string{"role": chatRole, "text": text})
	}
	for _, model := range enterpriseFixtureModels {
		for _, protocol := range []string{"gemini", "openai"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
					fields := map[string]any{"contents": contents}
					if protocol == "openai" {
						fields = map[string]any{"messages": messages}
					}
					path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
					response := enterprisePublicCall(router, path, body)
					if response.Code != 200 || !strings.Contains(enterprisePublicText(response.Body.Bytes(), protocol, stream), "offline") {
						t.Fatalf("large continuation rejected: status=%d body=%s", response.Code, response.Body.String())
					}
					query := gjson.GetBytes(transport.requestBody(), "query.text").String()
					if len(query) <= 1024*1024 || !strings.Contains(query, "Continue the last nonempty assistant") {
						t.Fatal("fixture did not exercise a large assistant continuation")
					}
					enterpriseAssertExactHistory(t, query, want)
				})
			}
		}
	}
}

func enterpriseAssertExactHistory(t *testing.T, query string, want []map[string]string) {
	t.Helper()
	var context struct {
		History []map[string]string `json:"conversation"`
	}
	if err := json.Unmarshal([]byte(query[strings.LastIndex(query, "\n")+1:]), &context); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(context.History, want) {
		t.Fatal("forwarded context lost or changed history entries, roles or text")
	}
}

func TestEnterpriseContinuationPlaceholdersPublicRoutes(t *testing.T) {
	router, transport, _ := enterpriseFixtureRouter(t)
	for _, model := range enterpriseFixtureModels {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				for _, tail := range []string{"", " \n", "unfinished"} {
					want := []map[string]string{{"role": "user", "text": "question"}, {"role": "assistant", "text": tail}}
					fields := map[string]any{"messages": []map[string]any{{"role": "user", "content": "question"}, {"role": "assistant", "content": tail}}}
					if protocol == "gemini" {
						fields = map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "question"}}}, {"role": "model", "parts": []map[string]string{{"text": tail, "thoughtSignature": "opaque"}}}}}
					}
					path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
					response := enterprisePublicCall(router, path, body)
					if response.Code != 200 || !strings.Contains(enterprisePublicText(response.Body.Bytes(), protocol, stream), "offline") {
						t.Fatalf("%s %s stream=%t placeholder failed: status=%d", model, protocol, stream, response.Code)
					}
					query := gjson.GetBytes(transport.requestBody(), "query.text").String()
					enterpriseAssertExactHistory(t, query, want)
					mode := "Answer the last nonempty user"
					if strings.TrimSpace(tail) != "" {
						mode = "Continue the last nonempty assistant"
					}
					if !strings.Contains(query, mode) {
						t.Fatal("incorrect continuation mode")
					}
				}
			}
		}
	}
}

func TestEnterpriseNewAPIHistoryPayloadRemainsFinal(t *testing.T) {
	for _, filter := range []bool{false, true} {
		cfg := &config.Config{}
		models := []config.PayloadModelRule{{Name: "gemini-*", Protocol: "gemini-enterprise"}}
		if filter {
			cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"query.text"}}}
		} else {
			cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"query.text": "final configured text"}}}
		}
		router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
		for _, stream := range []bool{false, true} {
			path, body := enterprisePublicRequest(t, "gemini-3.1-pro-preview", "gemini", stream, enterpriseNewAPIHistory([]map[string]any{{"text": "previous answer", "thoughtSignature": "opaque-signature"}}))
			response := enterprisePublicCall(router, path, body)
			if filter {
				if response.Code != 400 || transport.calls.Load() != 0 || gjson.GetBytes(response.Body.Bytes(), "error.param").String() != "query.text" || gjson.GetBytes(response.Body.Bytes(), "error.code").String() != "invalid_field" {
					t.Fatal("filtered history was restored or reached upstream")
				}
				enterpriseAssertPublicErrorChannelNeutral(t, response.Body.Bytes())
			} else if response.Code != 200 || gjson.GetBytes(transport.requestBody(), "query.text").String() != "final configured text" {
				t.Fatal("history normalization overwrote final payload configuration")
			}
		}
	}
}

func TestEnterpriseLargeConfiguredTextRemainsFinal(t *testing.T) {
	text := "configured start" + strings.Repeat(" ", 1024*1024+1) + "configured end"
	cfg := &config.Config{}
	cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-*", Protocol: "gemini-enterprise"}}, Params: map[string]any{"query.text": text}}}
	router, transport, _ := enterpriseFixtureRouterWithConfig(t, cfg)
	for _, protocol := range []string{"openai", "gemini"} {
		for _, stream := range []bool{false, true} {
			path, body := enterprisePublicRequest(t, "gemini-3.1-pro-preview", protocol, stream, nil)
			response := enterprisePublicCall(router, path, body)
			if response.Code != 200 || gjson.GetBytes(transport.requestBody(), "query.text").String() != text {
				t.Fatalf("large configured text rejected or changed: status=%d", response.Code)
			}
		}
	}
}

func TestEnterpriseJSONContinuationPublicRoutes(t *testing.T) {
	router, _, _ := enterpriseFixtureRouter(t)
	for _, model := range enterpriseFixtureModels {
		for _, protocol := range []string{"openai", "gemini"} {
			for _, stream := range []bool{false, true} {
				fields := map[string]any{"messages": []map[string]string{{"role": "user", "content": "Return a JSON object"}, {"role": "assistant", "content": `{"ok":`}}, "response_format": map[string]string{"type": "json_object"}}
				if protocol == "gemini" {
					fields = map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "Return a JSON object"}}}, {"role": "model", "parts": []map[string]string{{"text": `{"ok":`}}}}, "generationConfig": map[string]string{"responseMimeType": "application/json"}}
				}
				path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
				response := enterprisePublicCall(router, path, body)
				text := enterprisePublicText(response.Body.Bytes(), protocol, stream)
				if response.Code != 200 || !json.Valid([]byte(text)) || !gjson.Get(text, "ok").Bool() {
					t.Fatalf("%s %s stream=%t JSON continuation lost its complete JSON contract: status=%d", model, protocol, stream, response.Code)
				}
			}
		}
	}
}
