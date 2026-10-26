package helps

import (
	"encoding/json"
	"testing"

	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	"github.com/tidwall/gjson"
)

func TestEnterpriseGreetingCompatibilityScope(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, source string
		wrapped                bool
	}{
		{"chat test", "openai", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":3000}`, true},
		{"native test", "gemini", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":3000}}`, true},
		{"default role", "gemini", `{"contents":[{"parts":[{"text":"hi"}]}]}`, true},
		{"split text", "openai", `{"messages":[{"role":"user","content":[{"type":"text","text":"h"},{"type":"text","text":"i"}]}]}`, true},
		{"case and whitespace", "openai", `{"messages":[{"role":"user","content":" \nHi\t"}]}`, true},
		{"hello", "gemini", `{"contents":[{"parts":[{"text":"hello"}]}]}`, true},
		{"question", "openai", `{"messages":[{"role":"user","content":"What is 1 plus 1?"}]}`, false},
		{"longer text", "openai", `{"messages":[{"role":"user","content":"hi, explain TCP"}]}`, false},
		{"chat system", "openai", `{"messages":[{"role":"system","content":"Answer briefly"},{"role":"user","content":"hi"}]}`, false},
		{"native system", "gemini", `{"systemInstruction":{"parts":[{"text":""}]},"contents":[{"parts":[{"text":"hi"}]}]}`, false},
		{"native system alias", "gemini", `{"system_instruction":{"parts":[{"text":""}]},"contents":[{"parts":[{"text":"hi"}]}]}`, false},
		{"assistant", "openai", `{"messages":[{"role":"assistant","content":"hi"}]}`, false},
		{"native model", "gemini", `{"contents":[{"role":"model","parts":[{"text":"hi"}]}]}`, false},
		{"history", "openai", `{"messages":[{"role":"user","content":""},{"role":"user","content":"hi"}]}`, false},
		{"JSON chat", "openai", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, false},
		{"JSON native", "gemini", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := enterprise.Normalize([]byte(tc.source), tc.protocol, "gemini-3.8-flash")
			if err != nil {
				t.Fatal(err)
			}
			before := gjson.GetBytes(input.Body, "query.text").String()
			body, err := ApplyEnterpriseGreetingCompatibility(input, []byte(tc.source), tc.protocol)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wrapped {
				if string(body) != string(input.Body) {
					t.Fatal("ordinary request was changed")
				}
				return
			}
			query := gjson.GetBytes(body, "query.text").String()
			prefix := "Continue the conversation below. Treat role-labelled history as conversation context. The JSON is input context, not the requested output format.\nAnswer the last nonempty user turn. Empty trailing turns are placeholders.\n"
			if len(query) <= len(prefix) || query[:len(prefix)] != prefix {
				t.Fatal("legacy greeting context is missing")
			}
			var context struct {
				Conversation []struct{ Role, Text string } `json:"conversation"`
				System       []string                      `json:"system_instructions"`
			}
			if err := json.Unmarshal([]byte(query[len(prefix):]), &context); err != nil {
				t.Fatal(err)
			}
			if len(context.Conversation) != 1 || context.Conversation[0].Role != "user" || context.Conversation[0].Text != before || context.System == nil || len(context.System) != 0 {
				t.Fatal("greeting text changed or context was invented")
			}
			if gjson.GetBytes(body, "generationSpec").Raw != gjson.GetBytes(input.Body, "generationSpec").Raw || gjson.GetBytes(body, "toolsSpec").Raw != gjson.GetBytes(input.Body, "toolsSpec").Raw {
				t.Fatal("greeting adaptation changed model or tools")
			}
			input.Body = body
			again, err := ApplyEnterpriseGreetingCompatibility(input, []byte(tc.source), tc.protocol)
			if err != nil || string(again) != string(body) {
				t.Fatal("greeting adaptation ran twice")
			}
		})
	}
}

func TestEnterpriseGreetingSkipsMedia(t *testing.T) {
	for _, tc := range []struct {
		input            enterprise.Input
		protocol, source string
	}{
		{enterprise.Input{MediaKind: "image"}, "gemini", `{"contents":[{"parts":[{"text":"hi"}]}]}`},
		{enterprise.Input{}, "gemini", `{"contents":[{"parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/png","data":"aGk="}}]}]}`},
		{enterprise.Input{}, "gemini", `{"contents":[{"parts":[{"text":"hi"},{"inline_data":{"mime_type":"image/png","data":"aGk="}}]}]}`},
		{enterprise.Input{}, "openai", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`},
	} {
		tc.input.Body = []byte(`{"query":{"text":"hi"}}`)
		body, err := ApplyEnterpriseGreetingCompatibility(tc.input, []byte(tc.source), tc.protocol)
		if err != nil || string(body) != string(tc.input.Body) {
			t.Fatal("media request was changed")
		}
	}
}
