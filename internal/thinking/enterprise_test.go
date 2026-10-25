package thinking_test

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/geminienterprise"
	"github.com/tidwall/gjson"
)

func TestEnterpriseUnifiedThinkingAmountAndSummary(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			info := &registry.ModelInfo{ID: model, Type: "gemini", Thinking: registry.EnterpriseThinkingSupport(model)}
			for _, test := range []struct {
				name, source, format, model, level string
				include                            bool
			}{
				{"native-high-hidden", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":false}}}`, "gemini", info.ID, "HIGH", false},
				{"native-low-visible", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}}`, "gemini", info.ID, "LOW", true},
				{"native-snake", `{"generation_config":{"thinking_config":{"thinking_level":"HIGH","include_thoughts":true}}}`, "gemini", info.ID, "HIGH", true},
				{"native-mixed", `{"generationConfig":{"thinking_config":{"thinking_level":"LOW","include_thoughts":true}}}`, "gemini", info.ID, "LOW", true},
				{"suffix-preserves-hidden", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":false}}}`, "gemini", info.ID + "(low)", "LOW", false},
				{"chat-effort", `{"reasoning_effort":"high"}`, "openai", info.ID, "HIGH", true},
				{"chat-explicit-hidden", `{"reasoning_effort":"high","extra_body":{"google":{"thinking_config":{"include_thoughts":false}}}}`, "openai", info.ID, "HIGH", false},
			} {
				t.Run(test.name, func(t *testing.T) {
					body := []byte(`{"query":{"text":"preserved"},"generationSpec":{"modelId":"` + model + `"}}`)
					before := bytes.Clone(body)
					source := []byte(test.source)
					summary := thinking.ExtractSummaryConfig(source, test.format)
					out, err := thinking.ApplyThinkingWithModelInfoAndSummary(body, source, test.model, test.format, "gemini-enterprise", "gemini-enterprise", info, summary)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(body, before) || gjson.GetBytes(out, "query.text").String() != "preserved" {
						t.Fatal("thinking mutated source or business text")
					}
					if gjson.GetBytes(out, "generationSpec.thinkingConfig.thinkingLevel").String() != test.level || gjson.GetBytes(out, "generationSpec.thinkingConfig.includeThoughts").Bool() != test.include {
						t.Fatalf("incorrect canonical controls: %s", out)
					}
					if gjson.GetBytes(out, "generationSpec.thinkingConfig.thinkingBudget").Exists() {
						t.Fatal("level model received an unverified budget")
					}
					if got := thinking.ExtractTranslatedReasoningEffort(out, "gemini-enterprise"); got != string(bytes.ToLower([]byte(test.level))) {
						t.Fatalf("effective effort metadata = %q", got)
					}
				})
			}
		})
	}
}

func TestEnterpriseUnifiedThinkingRejectsUnverifiedNativeLevel(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			info := &registry.ModelInfo{ID: model, Type: "gemini", Thinking: registry.EnterpriseThinkingSupport(model)}
			_, err := thinking.ApplyThinkingWithModelInfo([]byte(`{"generationSpec":{"modelId":"`+model+`"}}`), []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"MEDIUM"}}}`), info.ID, "gemini", "gemini-enterprise", "gemini-enterprise", info)
			if err == nil {
				t.Fatal("unverified native level was silently applied")
			}
			if registry.EnterpriseThinkingSupport("gemini-2.5-pro") != nil {
				t.Fatal("unverified model was advertised with controls")
			}
		})
	}
}
