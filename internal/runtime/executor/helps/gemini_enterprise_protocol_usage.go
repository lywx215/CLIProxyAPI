package helps

import (
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// parseEnterpriseProtocolUsage preserves nullable fields when an Enterprise
// response passes through a plugin adapter. OpenAI completion includes reasoning;
// the internal Enterprise output bucket contains ordinary output only.
func parseEnterpriseProtocolUsage(payload []byte) (usage.Detail, bool) {
	if !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "usageMetadata")
	input, output, reason, total := "promptTokenCount", "candidatesTokenCount", "thoughtsTokenCount", "totalTokenCount"
	gemini := node.IsObject()
	if !gemini {
		node = gjson.GetBytes(payload, "usage")
		input, output, reason, total = "prompt_tokens", "completion_tokens", "completion_tokens_details.reasoning_tokens", "total_tokens"
	}
	if !node.IsObject() {
		return usage.Detail{}, false
	}
	p := &usage.TokenPresence{}
	d := usage.Detail{TokenPresence: p}
	d.InputTokens, p.InputTokens = enterpriseProtocolCount(node.Get(input))
	d.OutputTokens, p.OutputTokens = enterpriseProtocolCount(node.Get(output))
	d.ReasoningTokens, p.ReasoningTokens = enterpriseProtocolCount(node.Get(reason))
	d.TotalTokens, p.TotalTokens = enterpriseProtocolCount(node.Get(total))
	if !gemini && p.OutputTokens && p.ReasoningTokens {
		if d.ReasoningTokens > d.OutputTokens {
			d.OutputTokens, p.OutputTokens = 0, false
		} else {
			d.OutputTokens -= d.ReasoningTokens
		}
	}
	return usage.EnsureTokenBreakdownForProvider(d, "gemini-enterprise", "gemini-enterprise"), true
}

func enterpriseProtocolCount(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	n, err := strconv.ParseInt(value.Raw, 10, 64)
	return n, err == nil && n >= 0
}
