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
