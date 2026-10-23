package redisqueue

import (
	"context"
	"encoding/json"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestTokenPresenceQueueJSON(t *testing.T) {
	data, err := json.Marshal(tokenStats{InputTokens: 12, OutputTokens: 3, TotalTokens: 15, TokenPresence: &coreusage.TokenPresence{InputTokens: true, OutputTokens: true, TotalTokens: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens"} {
		if gjson.GetBytes(data, field).Type != gjson.Null {
			t.Fatalf("%s became zero: %s", field, data)
		}
	}
	if gjson.GetBytes(data, "input_tokens").Int() != 12 || gjson.GetBytes(data, "cache_read_tokens_present").Bool() {
		t.Fatalf("wrong queue usage: %s", data)
	}
}

func TestTokenPresenceQueuePluginPreservesPartialUsage(t *testing.T) {
	withEnabledQueue(t, func() {
		plugin := &usageQueuePlugin{}
		plugin.HandleUsage(context.Background(), coreusage.Record{
			Provider: "gemini-enterprise", Model: "enterprise-fixture",
			Detail: coreusage.Detail{
				InputTokens: 12, OutputTokens: 0, TotalTokens: 12,
				TokenPresence: &coreusage.TokenPresence{InputTokens: true, OutputTokens: true, TotalTokens: true},
			},
		})
		payload := popSinglePayload(t)
		tokens := requireTokensPayload(t, payload)
		requireIntField(t, tokens, "input_tokens", 12)
		requireIntField(t, tokens, "output_tokens", 0)
		requireIntField(t, tokens, "total_tokens", 12)
		requireTokensBoolField(t, payload, "cache_read_tokens_present", false)
		for _, field := range []string{"reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens"} {
			if string(tokens[field]) != "null" {
				t.Fatalf("%s was filled: %s", field, tokens[field])
			}
		}
		requireTokenBreakdown(t, payload, coreusage.TokenAccountingQualityUnclassified, 12)
	})
}
