package test

import (
	"encoding/json"
	"fmt"
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

func TestEnterpriseLongHistoryPublicLimitIsExplicit(t *testing.T) {
	router, transport, _ := enterpriseFixtureRouter(t)
	contents := make([]map[string]any, 0, 513)
	messages := make([]map[string]any, 0, 513)
	for i := range 513 {
		role, chatRole := "user", "user"
		if i%2 == 1 {
			role, chatRole = "model", "assistant"
		}
		text := fmt.Sprintf("history-turn-%d ", i) + strings.Repeat("x", 2048)
		contents = append(contents, map[string]any{"role": role, "parts": []map[string]string{{"text": text}}})
		messages = append(messages, map[string]any{"role": chatRole, "content": text})
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
					wire := response.Body.Bytes()
					if response.Code != 400 || gjson.GetBytes(wire, "error.type").String() != "invalid_request_error" || gjson.GetBytes(wire, "error.code").String() != "invalid_field" || gjson.GetBytes(wire, "error.param").String() != "body" || !strings.Contains(gjson.GetBytes(wire, "error.message").String(), "text history exceeds 1 MiB") {
						t.Fatalf("long history limit is not explicit: status=%d body=%s", response.Code, wire)
					}
					if transport.calls.Load() != 0 || strings.Contains(string(wire), "history-turn-") || strings.Contains(string(wire), "256") {
						t.Fatalf("oversized history leaked content, used the old entry limit or reached upstream: %s", wire)
					}
					enterpriseAssertPublicErrorChannelNeutral(t, wire)
				})
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
