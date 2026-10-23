package usage

import (
	"encoding/json"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type nullableTokenStats struct {
	InputTokens     *int64 `json:"input_tokens"`
	OutputTokens    *int64 `json:"output_tokens"`
	ReasoningTokens *int64 `json:"reasoning_tokens"`
	CachedTokens    *int64 `json:"cached_tokens"`
	TotalTokens     *int64 `json:"total_tokens"`
}

func (t TokenStats) MarshalJSON() ([]byte, error) {
	wire := nullableTokenStats{&t.InputTokens, &t.OutputTokens, &t.ReasoningTokens, &t.CachedTokens, &t.TotalTokens}
	if p := t.TokenPresence; p != nil {
		if !p.InputTokens {
			wire.InputTokens = nil
		}
		if !p.OutputTokens {
			wire.OutputTokens = nil
		}
		if !p.ReasoningTokens {
			wire.ReasoningTokens = nil
		}
		if !p.CachedTokens {
			wire.CachedTokens = nil
		}
		if !p.TotalTokens {
			wire.TotalTokens = nil
		}
	}
	return json.Marshal(wire)
}

func (t *TokenStats) UnmarshalJSON(data []byte) error {
	var wire nullableTokenStats
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*t = TokenStats{TokenPresence: &coreusage.TokenPresence{
		InputTokens: wire.InputTokens != nil, OutputTokens: wire.OutputTokens != nil,
		ReasoningTokens: wire.ReasoningTokens != nil, CachedTokens: wire.CachedTokens != nil,
		TotalTokens: wire.TotalTokens != nil,
	}}
	if wire.InputTokens != nil {
		t.InputTokens = *wire.InputTokens
	}
	if wire.OutputTokens != nil {
		t.OutputTokens = *wire.OutputTokens
	}
	if wire.ReasoningTokens != nil {
		t.ReasoningTokens = *wire.ReasoningTokens
	}
	if wire.CachedTokens != nil {
		t.CachedTokens = *wire.CachedTokens
	}
	if wire.TotalTokens != nil {
		t.TotalTokens = *wire.TotalTokens
	}
	if wire.InputTokens != nil && wire.OutputTokens != nil && wire.ReasoningTokens != nil && wire.CachedTokens != nil && wire.TotalTokens != nil {
		t.TokenPresence = nil
	}
	return nil
}
