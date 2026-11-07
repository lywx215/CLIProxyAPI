package helps

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/resources"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const webTokenTextLimit = 4 << 20

type webTokenText struct {
	segments []string
	bytes    int
	merge    bool
}

// EnterpriseWebTokenCounter shares Enterprise's tokenizer, but never promotes
// the text subset of an attachment request into a complete prompt or total.
// It consumes the parser's deduplicated parts, not raw cumulative wire frames.
type EnterpriseWebTokenCounter struct {
	ctx                context.Context
	count              func(string) (int64, error)
	input              *int64
	output             []webTokenText
	thoughts           webTokenText
	retained           int
	sawOutput          bool
	outputUnavailable  bool
	thoughtUnavailable bool
	detail             *usage.Detail
}

func NewEnterpriseWebTokenCounter(ctx context.Context, finalPayload []byte) *EnterpriseWebTokenCounter {
	c := &EnterpriseWebTokenCounter{ctx: ctx, count: countEnterpriseText}
	// This is the final small business payload; never inspect the original
	// request containing inlineData or tokenize file IDs / authentication data.
	if len(finalPayload) > webTokenTextLimit {
		return c
	}
	release, err := resources.Reserve(ctx, int64(len(finalPayload))*4+65536)
	if err != nil {
		return c
	}
	defer release()
	parts := gjson.GetBytes(finalPayload, "streamAssistRequest.query.parts")
	if !parts.IsArray() {
		return c
	}
	var input webTokenText
	valid := true
	parts.ForEach(func(_, part gjson.Result) bool {
		value := part.Get("text")
		if !part.IsObject() || value.Type != gjson.String || input.bytes+len(value.Str) > webTokenTextLimit {
			valid = false
			return false
		}
		input.segments = append(input.segments, value.Str)
		input.bytes += len(value.Str)
		return true
	})
	if valid {
		if value, ok := c.countText(input); ok {
			c.input = &value
		}
	}
	return c
}

func (c *EnterpriseWebTokenCounter) countText(group webTokenText) (int64, bool) {
	// SentencePiece retains symbols, merge candidates and tokens. Reserve
	// conservative per-byte workspace before joining or tokenizing. Optional
	// accounting becomes unavailable if it cannot fit; generation still works.
	release, err := resources.Reserve(c.ctx, int64(group.bytes)*256+65536)
	if err != nil {
		return 0, false
	}
	defer release()
	value, err := c.count(strings.Join(group.segments, ""))
	return value, err == nil && value >= 0
}

func (c *EnterpriseWebTokenCounter) retain(size int, thought bool) bool {
	if c.retained+size > webTokenTextLimit {
		if thought {
			c.thoughtUnavailable = true
		} else {
			c.outputUnavailable = true
		}
		return false
	}
	if _, err := resources.Reserve(c.ctx, int64(size)*2+256); err != nil {
		if thought {
			c.thoughtUnavailable = true
		} else {
			c.outputUnavailable = true
		}
		return false
	}
	c.retained += size
	return true
}

func (c *EnterpriseWebTokenCounter) addOutput(text string, merge bool) {
	c.sawOutput = true
	if c.outputUnavailable || !c.retain(len(text), false) {
		return
	}
	if merge && len(c.output) > 0 && c.output[len(c.output)-1].merge {
		last := &c.output[len(c.output)-1]
		last.segments = append(last.segments, text)
		last.bytes += len(text)
		return
	}
	c.output = append(c.output, webTokenText{segments: []string{text}, bytes: len(text), merge: merge})
}

func (c *EnterpriseWebTokenCounter) Observe(parts []contract.Part) {
	if c == nil || c.detail != nil {
		return
	}
	for _, part := range parts {
		if part.Thought {
			if part.InlineData == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil &&
				!c.thoughtUnavailable && c.retain(len(part.Text), true) {
				c.thoughts.segments = append(c.thoughts.segments, part.Text)
				c.thoughts.bytes += len(part.Text)
			}
			continue
		}
		switch {
		case part.ExecutableCode != nil:
			c.addOutput(part.ExecutableCode.Language, false)
			c.addOutput(part.ExecutableCode.Code, false)
		case part.CodeExecutionResult != nil:
			c.addOutput(part.CodeExecutionResult.Outcome, false)
			c.addOutput(part.CodeExecutionResult.Output, false)
		case part.InlineData == nil:
			c.addOutput(part.Text, true)
		}
	}
}

func (c *EnterpriseWebTokenCounter) Detail(complete bool) usage.Detail {
	if c == nil {
		return usage.Detail{TokenPresence: &usage.TokenPresence{}}
	}
	if c.detail == nil {
		p := &usage.TokenPresence{}
		d := usage.Detail{TokenPresence: p, Estimate: &usage.TokenEstimate{
			Source: usage.LocalGemma3SentencePiece, InputTextTokens: c.input, Incomplete: true,
		}}
		if !c.outputUnavailable && (complete || c.sawOutput) {
			p.OutputTokens = true
			groups := c.output
			if len(groups) == 0 {
				groups = []webTokenText{{}}
			}
			for _, group := range groups {
				n, ok := c.countText(group)
				if ok {
					d.OutputTokens, ok = safeUsageTokenSum(d.OutputTokens, n)
				}
				if !ok {
					d.OutputTokens, p.OutputTokens = 0, false
					break
				}
			}
		}
		if !c.thoughtUnavailable && len(c.thoughts.segments) > 0 {
			d.ReasoningTokens, p.ReasoningTokens = c.countText(c.thoughts)
		}
		c.detail = &d
		c.output, c.thoughts = nil, webTokenText{}
	}
	d := *c.detail
	p := *d.TokenPresence
	d.TokenPresence = &p
	d.Estimate = d.Estimate.Clone()
	d.Estimate.ResponseComplete = complete
	return d
}

// Standard usage includes only available output fields. The separate extension
// explains their local provenance and keeps the input text subset out of totals.
func EnterpriseWebUsagePayload(payload []byte, detail usage.Detail) []byte {
	payload = EnterpriseUsagePayload(payload, true, detail)
	if detail.Estimate != nil {
		if updated, err := sjson.SetBytes(payload, "usageEstimate", detail.Estimate); err == nil {
			return updated
		}
	}
	return payload
}

// Preserve the text subset and unknown media totals through plugin adapters.
// Ordinary Gemini responses have no estimate extension and keep their parser.
func parseEnterpriseWebProtocolUsage(payload []byte) (usage.Detail, bool) {
	node := gjson.GetBytes(payload, "usageEstimate")
	if node.Get("source").String() != usage.LocalGemma3SentencePiece ||
		node.Get("incomplete").Type != gjson.True ||
		(node.Get("response_complete").Type != gjson.True && node.Get("response_complete").Type != gjson.False) ||
		!gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	d, ok := parseEnterpriseProtocolUsage(payload)
	if !ok {
		d = usage.Detail{TokenPresence: &usage.TokenPresence{}}
	}
	d.Estimate = &usage.TokenEstimate{Source: usage.LocalGemma3SentencePiece, Incomplete: true, ResponseComplete: node.Get("response_complete").Bool()}
	if input, valid := enterpriseProtocolCount(node.Get("input_text_tokens")); valid {
		d.Estimate.InputTextTokens = &input
	}
	d.InputTokens, d.CachedTokens, d.TotalTokens = 0, 0, 0
	d.TokenPresence.InputTokens, d.TokenPresence.CachedTokens, d.TokenPresence.TotalTokens = false, false, false
	return usage.EnsureTokenBreakdownForProvider(d, "gemini-enterprise-web", "gemini-enterprise-web"), true
}
