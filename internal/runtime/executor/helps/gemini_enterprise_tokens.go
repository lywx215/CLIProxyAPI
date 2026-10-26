package helps

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	sentencepiece "github.com/eliben/go-sentencepiece"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This is the Gemma3 model used by Google's Go Gen AI local tokenizer.
// Source: google/gemma_pytorch, commit 014acb7ac4563a5f77c76d7ff98f31b568c16508.
//
//go:embed gemma3.model.gz
var enterpriseTokenizerModel []byte

var enterpriseTokenizer = sync.OnceValues(func() (*sentencepiece.Processor, error) {
	return loadEnterpriseTokenizer(enterpriseTokenizerModel)
})

func loadEnterpriseTokenizer(compressed []byte) (*sentencepiece.Processor, error) {
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open enterprise tokenizer: %w", err)
	}
	data, errRead := io.ReadAll(r)
	errClose := r.Close()
	if errRead != nil {
		return nil, fmt.Errorf("read enterprise tokenizer: %w", errRead)
	}
	if errClose != nil {
		return nil, fmt.Errorf("close enterprise tokenizer: %w", errClose)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != "1299c11d7cf632ef3b4e11937501358ada021bbdf7c47638d13c0ee982f2e79c" {
		return nil, fmt.Errorf("enterprise tokenizer checksum mismatch")
	}
	processor, errProcessor := sentencepiece.NewProcessor(bytes.NewReader(data))
	if errProcessor != nil {
		return nil, fmt.Errorf("parse enterprise tokenizer: %w", errProcessor)
	}
	return processor, nil
}

func countEnterpriseText(text string) (int64, error) {
	processor, err := enterpriseTokenizer()
	if err != nil {
		return 0, err
	}
	return int64(len(processor.Encode(text))), nil
}

// EnterpriseTokenCounter locally counts the final business text and visible output.
// These counts are not upstream-reported or billing-authoritative usage.
type EnterpriseTokenCounter struct {
	input      int64
	inputKnown bool
	count      func(string) (int64, error)
}

func NewEnterpriseTokenCounter(finalPayload []byte) *EnterpriseTokenCounter {
	c := &EnterpriseTokenCounter{count: countEnterpriseText}
	// Text tokenization cannot account for uploaded media or parsed attachments.
	if len(gjson.GetBytes(finalPayload, "fileIds").Array()) > 0 {
		return c
	}
	value := gjson.GetBytes(finalPayload, "query.text")
	if value.Type == gjson.String {
		var err error
		c.input, err = c.count(value.String())
		c.inputKnown = err == nil
	}
	return c
}

// Detail counts merged upstream parts before JSON cleanup or Markdown rendering.
// Missing hidden reasoning and cache counters deliberately remain unavailable.
func (c *EnterpriseTokenCounter) Detail(parts []map[string]any) usage.Detail {
	presence := &usage.TokenPresence{InputTokens: c.inputKnown, OutputTokens: true}
	detail := usage.Detail{InputTokens: c.input, TokenPresence: presence}
	if _, err := c.count(""); err != nil {
		presence.OutputTokens = false
		return detail
	}
	var semantic []map[string]any
	for _, part := range parts {
		if part["thought"] == true {
			continue
		}
		if text, ok := part["text"].(string); ok && len(part) == 1 && len(semantic) > 0 {
			previous := semantic[len(semantic)-1]
			if before, ok := previous["text"].(string); ok && len(previous) == 1 {
				semantic[len(semantic)-1] = map[string]any{"text": before + text}
				continue
			}
		}
		semantic = append(semantic, part)
	}
	for _, part := range semantic {
		var texts []string
		if text, ok := part["text"].(string); ok {
			texts = append(texts, text)
		}
		if code, ok := part["executableCode"].(map[string]any); ok {
			for _, key := range []string{"language", "code"} {
				if text, ok := code[key].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		if result, ok := part["codeExecutionResult"].(map[string]any); ok {
			for _, key := range []string{"outcome", "output"} {
				if text, ok := result[key].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		for _, text := range texts {
			value, err := c.count(text)
			if err != nil {
				presence.OutputTokens = false
				detail.OutputTokens = 0
				break
			}
			detail.OutputTokens += value
		}
		if !presence.OutputTokens {
			break
		}
	}
	presence.TotalTokens = presence.InputTokens && presence.OutputTokens
	if presence.TotalTokens {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	}
	return detail
}

// EnterpriseUsagePayload adds standard protocol counters without origin fields.
func EnterpriseUsagePayload(payload []byte, gemini bool, detail usage.Detail) []byte {
	p := detail.TokenPresence
	if p == nil {
		return payload
	}
	values := map[string]int64{}
	key := "usage"
	input, output, total := "prompt_tokens", "completion_tokens", "total_tokens"
	if gemini {
		key = "usageMetadata"
		input, output, total = "promptTokenCount", "candidatesTokenCount", "totalTokenCount"
	}
	if p.InputTokens {
		values[input] = detail.InputTokens
	}
	if p.OutputTokens {
		values[output] = detail.OutputTokens
	}
	if p.TotalTokens {
		values[total] = detail.TotalTokens
	}
	if len(values) == 0 {
		return payload
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, key, encoded)
	if err != nil {
		return payload
	}
	return updated
}

func EnterpriseStreamUsageRequested(payload []byte) bool {
	return gjson.GetBytes(payload, "stream_options.include_usage").Bool()
}

func EnterpriseChatUsageChunk(payload []byte, detail usage.Detail) []byte {
	updated, err := sjson.SetRawBytes(payload, "choices", []byte("[]"))
	if err != nil {
		return payload
	}
	return EnterpriseUsagePayload(updated, false, detail)
}
