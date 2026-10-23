package redisqueue

import "encoding/json"

func (t tokenStats) MarshalJSON() ([]byte, error) {
	type wireTokens struct {
		Input            *int64 `json:"input_tokens"`
		Output           *int64 `json:"output_tokens"`
		Reasoning        *int64 `json:"reasoning_tokens"`
		Cached           *int64 `json:"cached_tokens"`
		CacheRead        *int64 `json:"cache_read_tokens"`
		CacheReadPresent bool   `json:"cache_read_tokens_present"`
		CacheCreation    *int64 `json:"cache_creation_tokens"`
		Total            *int64 `json:"total_tokens"`
	}
	w := wireTokens{&t.InputTokens, &t.OutputTokens, &t.ReasoningTokens, &t.CachedTokens, &t.CacheReadTokens, t.CacheReadTokensPresent, &t.CacheCreationTokens, &t.TotalTokens}
	if p := t.TokenPresence; p != nil {
		if !p.InputTokens {
			w.Input = nil
		}
		if !p.OutputTokens {
			w.Output = nil
		}
		if !p.ReasoningTokens {
			w.Reasoning = nil
		}
		if !p.CachedTokens {
			w.Cached = nil
		}
		if !p.CacheReadTokens {
			w.CacheRead = nil
		}
		if !p.CacheCreationTokens {
			w.CacheCreation = nil
		}
		if !p.TotalTokens {
			w.Total = nil
		}
	}
	return json.Marshal(w)
}
