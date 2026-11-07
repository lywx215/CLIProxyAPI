package usage

// LocalGemma3SentencePiece identifies the shared local text tokenizer.
const LocalGemma3SentencePiece = "local_gemma3_sentencepiece"

// TokenEstimate describes local text measurements, not provider billing usage.
// InputTextTokens excludes attachments and provider-internal context. Output and
// reasoning counters use the same source; reasoning covers returned summaries.
// ResponseComplete means a complete upstream answer was observed, not a client
// acknowledgement of delivery or complete provider billing measurements.
type TokenEstimate struct {
	Source           string `json:"source"`
	InputTextTokens  *int64 `json:"input_text_tokens"`
	Incomplete       bool   `json:"incomplete"`
	ResponseComplete bool   `json:"response_complete"`
}

func (e *TokenEstimate) Clone() *TokenEstimate {
	if e == nil {
		return nil
	}
	copy := *e
	if e.InputTextTokens != nil {
		value := *e.InputTextTokens
		copy.InputTextTokens = &value
	}
	return &copy
}
