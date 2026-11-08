package antigravityenterprise

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestAntigravityEnterpriseThinkingUsesCanonicalLevelsAndSummary(t *testing.T) {
	info := &registry.ModelInfo{ID: "gemini-4-argon-high", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}}
	source := []byte(`{"reasoning":{"effort":"medium","summary":"none"}}`)
	body := []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}}`)
	result, err := thinking.ApplyThinkingWithModelInfo(body, source, info.ID, "openai-response", "antigravity-enterprise", "antigravity-enterprise", info)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(result, "generationConfig.thinkingConfig.thinkingLevel").String() != "MEDIUM" || gjson.GetBytes(result, "generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("effort or summary intent lost: %s", result)
	}
	result, err = thinking.ApplyThinkingWithModelInfo(body, source, info.ID+"(low)", "openai-response", "antigravity-enterprise", "antigravity-enterprise", info)
	if err != nil || gjson.GetBytes(result, "generationConfig.thinkingConfig.thinkingLevel").String() != "LOW" {
		t.Fatalf("suffix did not override source: %s %v", result, err)
	}
}
