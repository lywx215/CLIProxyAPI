// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package contract

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strings"
)

// HistoryPart is the content identity retained by the parser. File bytes and
// original user text are not retained by the session cache.
type HistoryPart struct {
	Text                *string
	Thought             bool
	ThoughtSignature    string
	MIMEType            string
	FileHash            string
	Filename            string
	ExecutableCode      *ExecutableCode
	CodeExecutionResult *CodeExecutionResult
}

func historyString(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}

func ordinaryHistoryText(p HistoryPart) bool {
	return p.Text != nil && !p.Thought && p.ThoughtSignature == "" && p.MIMEType == "" && p.FileHash == "" && p.Filename == "" && p.ExecutableCode == nil && p.CodeExecutionResult == nil
}

// Signatures remain opaque on the wire. Protobuf SDKs may re-encode the same
// bytes with the URL-safe alphabet or without padding; only the history
// identity normalizes valid encodings. Non-base64 opaque values stay exact.
func historySignature(h hash.Hash, value string) {
	encoding := base64.StdEncoding.Strict()
	validAlphabet := !strings.ContainsAny(value, " \r\n\t")
	if strings.ContainsAny(value, "-_") {
		validAlphabet = validAlphabet && !strings.ContainsAny(value, "+/")
		encoding = base64.URLEncoding.Strict()
	}
	if !strings.HasSuffix(value, "=") {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	if validAlphabet {
		if decoded, err := encoding.DecodeString(value); err == nil {
			historyString(h, "signature-bytes")
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(decoded)))
			_, _ = h.Write(size[:])
			_, _ = h.Write(decoded)
			return
		}
	}
	historyString(h, "signature-raw")
	historyString(h, value)
}

// HistoryDigest uses a versioned, length-delimited encoding. Adjacent unsigned
// ordinary text is one run, matching SDKs that concatenate streaming deltas.
// Thought, signature and inline-data boundaries remain significant.
func HistoryDigest(role string, parts []HistoryPart) string {
	h := sha256.New()
	historyString(h, "business-history-v2")
	historyString(h, role)
	for i := 0; i < len(parts); {
		p := parts[i]
		if ordinaryHistoryText(p) {
			end, size := i, uint64(0)
			for end < len(parts) && ordinaryHistoryText(parts[end]) {
				size += uint64(len(*parts[end].Text))
				end++
			}
			historyString(h, "text")
			var encodedSize [8]byte
			binary.BigEndian.PutUint64(encodedSize[:], size)
			_, _ = h.Write(encodedSize[:])
			for ; i < end; i++ {
				_, _ = h.Write([]byte(*parts[i].Text))
			}
			continue
		}
		historyString(h, "part")
		if p.Text != nil {
			historyString(h, "text")
			historyString(h, *p.Text)
		} else {
			historyString(h, "no-text")
		}
		if p.Thought {
			historyString(h, "thought")
		} else {
			historyString(h, "ordinary")
		}
		historySignature(h, p.ThoughtSignature)
		historyString(h, p.MIMEType)
		historyString(h, p.FileHash)
		historyString(h, p.Filename)
		if p.ExecutableCode != nil {
			historyString(h, "executable-code")
			historyString(h, p.ExecutableCode.Language)
			historyString(h, p.ExecutableCode.Code)
		}
		if p.CodeExecutionResult != nil {
			historyString(h, "code-execution-result")
			historyString(h, p.CodeExecutionResult.Outcome)
			historyString(h, p.CodeExecutionResult.Output)
		}
		i++
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ModelHistoryDigest(parts []Part) string {
	values := make([]HistoryPart, 0, len(parts))
	for _, p := range parts {
		value := HistoryPart{Thought: p.Thought, ThoughtSignature: p.ThoughtSignature}
		if p.ExecutableCode != nil {
			value.ExecutableCode = p.ExecutableCode
		} else if p.CodeExecutionResult != nil {
			value.CodeExecutionResult = p.CodeExecutionResult
		} else if p.InlineData != nil {
			sum := sha256.Sum256(p.InlineData.Data)
			value.MIMEType, value.FileHash = p.InlineData.MIMEType, hex.EncodeToString(sum[:])
		} else {
			text := p.Text
			value.Text = &text
		}
		values = append(values, value)
	}
	return HistoryDigest("model", values)
}
