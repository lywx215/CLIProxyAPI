// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package contract

import "context"

const LocalUsageSource = "local_gemma3_sentencepiece"

// Usage is a local usage estimate. Unknown media and provider-internal work
// must not be represented as a verified zero or as native billing metadata.
type Usage struct {
	PromptTokenCount     int64  `json:"promptTokenCount"`
	CandidatesTokenCount int64  `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int64  `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount      int64  `json:"totalTokenCount"`
	CountingSource       string `json:"countingSource"`
	Estimated            bool   `json:"estimated"`
	Incomplete           bool   `json:"incomplete,omitempty"`
}

// GenerationObserver connects the isolated Business runtime to the host's
// existing access log and ledger. It never receives credentials or prompt text.
type GenerationObserver struct {
	Target   func(model, account string)
	Upstream func()
	Usage    func(Usage)
	Error    func(error)
}

type generationObserverKey struct{}

func WithGenerationObserver(ctx context.Context, observer GenerationObserver) context.Context {
	return context.WithValue(ctx, generationObserverKey{}, observer)
}

func ReportTarget(ctx context.Context, model, account string) {
	if o, ok := ctx.Value(generationObserverKey{}).(GenerationObserver); ok && o.Target != nil {
		o.Target(model, account)
	}
}

func ReportUpstream(ctx context.Context) {
	if o, ok := ctx.Value(generationObserverKey{}).(GenerationObserver); ok && o.Upstream != nil {
		o.Upstream()
	}
}

func ReportUsage(ctx context.Context, usage Usage) {
	if o, ok := ctx.Value(generationObserverKey{}).(GenerationObserver); ok && o.Usage != nil {
		o.Usage(usage)
	}
}

func ReportError(ctx context.Context, err error) {
	if o, ok := ctx.Value(generationObserverKey{}).(GenerationObserver); ok && o.Error != nil && err != nil {
		o.Error(err)
	}
}
