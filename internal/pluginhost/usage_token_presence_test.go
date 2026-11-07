package pluginhost

import (
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageTokenPresenceClone(t *testing.T) {
	if cloneUsageTokenPresence(nil) != nil {
		t.Fatal("legacy availability changed")
	}
	original := &coreusage.TokenPresence{InputTokens: true, OutputTokens: true, TotalTokens: true}
	cloned := cloneUsageTokenPresence(original)
	if !cloned.InputTokens || !cloned.OutputTokens || !cloned.TotalTokens || cloned.ReasoningTokens || cloned.CachedTokens {
		t.Fatal("plugin lost availability")
	}
	cloned.InputTokens = false
	if !original.InputTokens {
		t.Fatal("plugin mutated shared usage")
	}
}

func TestUsageTokenEstimateClone(t *testing.T) {
	if cloneUsageTokenEstimate(nil) != nil {
		t.Fatal("legacy estimates changed")
	}
	input := int64(23)
	original := &coreusage.TokenEstimate{Source: coreusage.LocalGemma3SentencePiece, InputTextTokens: &input, Incomplete: true, ResponseComplete: true}
	cloned := cloneUsageTokenEstimate(original)
	if cloned.Source != original.Source || cloned.InputTextTokens == nil || *cloned.InputTextTokens != 23 || !cloned.Incomplete || !cloned.ResponseComplete {
		t.Fatal("plugin lost estimate")
	}
	*cloned.InputTextTokens = 1
	if input != 23 {
		t.Fatal("plugin mutated original estimate")
	}
}
