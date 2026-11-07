package helps

import (
	"bytes"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/geminienterprise"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestApplyEnterpriseThinkingCarbonSummaryUsesCanonicalSourceIntent(t *testing.T) {
	if registry.EnterpriseThinkingSupport(ge.CarbonModel) != nil {
		t.Fatal("this regression must exercise Carbon without advertised level capabilities")
	}
	for _, test := range []struct {
		name, format, source string
		want                 gjson.Type
	}{
		{"native-enabled", "gemini", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`, gjson.True},
		{"native-disabled", "gemini", `{"generationConfig":{"thinkingConfig":{"includeThoughts":false}}}`, gjson.False},
		{"native-unspecified", "gemini", `{"contents":[{"parts":[{"text":"hi"}]}]}`, gjson.Null},
		{"native-snake", "gemini", `{"generation_config":{"thinking_config":{"include_thoughts":true}}}`, gjson.True},
		{"native-invalid-type", "gemini", `{"generationConfig":{"thinkingConfig":{"includeThoughts":"true"}}}`, gjson.Null},
		{"native-amount-only", "gemini", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","thinkingBudget":8192}}}`, gjson.Null},
		{"chat-google-enabled", "openai", `{"extra_body":{"google":{"thinking_config":{"include_thoughts":true}}}}`, gjson.True},
		{"chat-explicit-disabled-over-effort", "openai", `{"reasoning_effort":"high","extra_body":{"google":{"thinking_config":{"include_thoughts":false}}}}`, gjson.False},
		{"chat-summary-enabled", "openai", `{"reasoning":{"summary":"detailed"}}`, gjson.True},
		{"chat-summary-disabled", "openai", `{"reasoning":{"summary":"none"}}`, gjson.False},
		{"chat-summary-null", "openai", `{"reasoning":{"summary":null}}`, gjson.False},
		{"chat-visible-compatibility", "openai", `{"include_reasoning":true}`, gjson.True},
		{"chat-hidden-compatibility", "openai", `{"reasoning":{"exclude":true}}`, gjson.False},
		{"chat-effort-existing-summary-contract", "openai", `{"reasoning_effort":"high"}`, gjson.True},
		{"chat-unspecified", "openai", `{"messages":[{"role":"user","content":"hi"}]}`, gjson.Null},
		{"responses-summary-enabled", "openai-response", `{"reasoning":{"summary":"auto"}}`, gjson.True},
		{"responses-summary-disabled", "openai-response", `{"reasoning":{"summary":"none"}}`, gjson.False},
		{"responses-effort-only", "openai-response", `{"reasoning":{"effort":"high"}}`, gjson.Null},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"query":{"text":"preserved"},"generationSpec":{"modelId":"gemini-4-carbon","temperature":0.3},"toolsSpec":{"keep":true}}`)
			source := []byte(test.source)
			beforeBody, beforeSource := bytes.Clone(body), bytes.Clone(source)
			out, err := ApplyEnterpriseThinking(body, coreexec.Request{Model: ge.CarbonModel, Payload: source}, coreexec.Options{SourceFormat: sdktranslator.Format(test.format)})
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "generationSpec.thinkingConfig.includeThoughts"); got.Type != test.want {
				t.Fatalf("summary type=%v, want=%v; body=%s", got.Type, test.want, out)
			}
			if !bytes.Equal(body, beforeBody) || !bytes.Equal(source, beforeSource) || gjson.GetBytes(out, "query.text").String() != "preserved" || gjson.GetBytes(out, "generationSpec.temperature").Float() != 0.3 || !gjson.GetBytes(out, "toolsSpec.keep").Bool() {
				t.Fatal("summary handling changed the input buffers or unrelated business fields")
			}
			if test.want == gjson.Null && !bytes.Equal(out, body) {
				t.Fatal("unspecified summary modified the target body")
			}
			assertEnterpriseCarbonSummaryHasNoAmount(t, out)
		})
	}
}

func TestApplyEnterpriseThinkingCarbonSummaryUsesResolvedUpstreamIdentity(t *testing.T) {
	for _, test := range []struct {
		name, model string
		selected    *registry.ModelInfo
		want        bool
	}{
		{"direct", ge.CarbonModel, nil, true},
		{"existing-level-suffix", ge.CarbonModel + "(high)", nil, true},
		{"existing-budget-suffix", ge.CarbonModel + "(8192)", nil, true},
		{"existing-none-suffix", ge.CarbonModel + "(none)", nil, true},
		{"sdk-resolved-prefix-and-alias", ge.CarbonModel, &registry.ModelInfo{ID: ge.CarbonModel, DisplayName: "Public model"}, true},
		{"selected-upstream-id", "team/carbon-public(high)", &registry.ModelInfo{ID: ge.CarbonModel}, true},
		{"selected-metadata-upstream-id", "team/carbon-public(high)", &registry.ModelInfo{ID: "team/carbon-public", MetadataModelID: ge.CarbonModel, DisplayName: ge.ArgonModel}, true},
		{"selected-resource-id", "public", &registry.ModelInfo{ID: "models/" + ge.CarbonModel}, true},
		{"selected-suffixed-upstream-id", "public", &registry.ModelInfo{ID: ge.CarbonModel + "(8192)"}, true},
		{"unresolved-prefix", "team/" + ge.CarbonModel, nil, false},
		{"unresolved-alias", "carbon-public", nil, false},
		{"display-name-only", "other-model", &registry.ModelInfo{ID: "other-model", DisplayName: ge.CarbonModel}, false},
		{"actual-metadata-over-public-carbon-name", ge.CarbonModel, &registry.ModelInfo{ID: ge.CarbonModel, MetadataModelID: ge.ArgonModel}, false},
		{"actual-id-over-request-carbon-name", ge.CarbonModel, &registry.ModelInfo{ID: ge.ArgonModel, DisplayName: ge.CarbonModel}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"generationSpec":{"modelId":"kept-exactly"}}`)
			req := coreexec.Request{Model: test.model, Payload: []byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`)}
			if test.selected != nil {
				req.Metadata = map[string]any{"cliproxy.resolved_api_key_model_info": test.selected}
			}
			out, err := ApplyEnterpriseThinking(body, req, coreexec.Options{SourceFormat: sdktranslator.FormatGemini})
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(out, "generationSpec.thinkingConfig.includeThoughts").Exists() != test.want || gjson.GetBytes(out, "generationSpec.modelId").String() != "kept-exactly" {
				t.Fatalf("summary used a display/public name instead of the actual model identity: %s", out)
			}
			if !test.want && !bytes.Equal(out, body) {
				t.Fatal("an unrelated nil-support model lost its original no-op")
			}
			if test.selected != nil && test.selected.Thinking != nil {
				t.Fatal("summary handling invented a model thinking capability")
			}
			assertEnterpriseCarbonSummaryHasNoAmount(t, out)
		})
	}
}

func TestApplyEnterpriseThinkingCarbonSummaryCurrentPayloadPrecedesOriginal(t *testing.T) {
	for _, test := range []struct {
		name, current, original, body string
		want                          gjson.Type
	}{
		{"empty-current-falls-back", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`, `{"generationSpec":{}}`, gjson.True},
		{"current-disabled-wins", `{"generationConfig":{"thinkingConfig":{"includeThoughts":false}}}`, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`, `{"generationSpec":{"thinkingConfig":{"includeThoughts":true}}}`, gjson.False},
		{"current-unspecified-does-not-resurrect", `{}`, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`, `{"generationSpec":{}}`, gjson.Null},
		{"unspecified-retains-target-disabled", `{}`, `{}`, `{"generationSpec":{"thinkingConfig":{"includeThoughts":false}}}`, gjson.False},
		{"unspecified-retains-target-enabled", `{}`, `{}`, `{"generationSpec":{"thinkingConfig":{"includeThoughts":true}}}`, gjson.True},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := ApplyEnterpriseThinking([]byte(test.body), coreexec.Request{Model: ge.CarbonModel, Payload: []byte(test.current)}, coreexec.Options{SourceFormat: sdktranslator.FormatGemini, OriginalRequest: []byte(test.original)})
			if err != nil || gjson.GetBytes(out, "generationSpec.thinkingConfig.includeThoughts").Type != test.want {
				t.Fatalf("current/original source intent changed: err=%v body=%s", err, out)
			}
			assertEnterpriseCarbonSummaryHasNoAmount(t, out)
		})
	}
}

func TestApplyEnterpriseThinkingOtherNilCapabilitiesRemainNoOp(t *testing.T) {
	for _, model := range []string{ge.ArgonModel, "gemini-2.5-pro", "unverified-thinking-model", "team/" + ge.CarbonModel} {
		for _, source := range []string{`{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"HIGH"}}}`, "not valid JSON"} {
			body := []byte(` {"query":{"text":"keep"},"generationSpec":{"thinkingConfig":{"includeThoughts":false,"thinkingLevel":"untouched"}}} `)
			out, err := ApplyEnterpriseThinking(body, coreexec.Request{Model: model, Payload: []byte(source)}, coreexec.Options{SourceFormat: sdktranslator.FormatGemini})
			if err != nil || !bytes.Equal(out, body) {
				t.Fatalf("nil-capability model %q no longer returns the original body: err=%v body=%s", model, err, out)
			}
		}
	}
}

func TestApplyEnterpriseThinkingVerifiedLevelModelsKeepExistingControls(t *testing.T) {
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"generationSpec":{"modelId":"` + model + `"}}`)
			source := []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":false}}}`)
			out, err := ApplyEnterpriseThinking(body, coreexec.Request{Model: model + "(low)", Payload: source}, coreexec.Options{SourceFormat: sdktranslator.FormatGemini})
			if err != nil || gjson.GetBytes(out, "generationSpec.thinkingConfig.thinkingLevel").String() != "LOW" || gjson.GetBytes(out, "generationSpec.thinkingConfig.includeThoughts").Type != gjson.False || gjson.GetBytes(out, "generationSpec.thinkingConfig.thinkingBudget").Exists() {
				t.Fatalf("verified level/summary behavior changed: err=%v body=%s", err, out)
			}
		})
	}
}

func assertEnterpriseCarbonSummaryHasNoAmount(t *testing.T, body []byte) {
	t.Helper()
	for _, field := range []string{"thinkingLevel", "thinkingBudget", "thinking_level", "thinking_budget", "budgetTokens"} {
		if gjson.GetBytes(body, "generationSpec.thinkingConfig."+field).Exists() {
			t.Fatalf("summary-only Carbon acquired an unverified thinking amount: %s", body)
		}
	}
}
