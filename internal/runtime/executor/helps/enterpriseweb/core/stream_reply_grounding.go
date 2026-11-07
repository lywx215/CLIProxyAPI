// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

const (
	maxGroundingReplyIDBytes = 1024
	maxGroundingReplyIDs     = 256
	maxGroundingReplyIDTotal = 64 << 10
	maxReplyGroundingWork    = 16 << 20
	maxReplyGroundingChecks  = 128 << 10
)

type replyGroundingAnchor struct {
	spans     []streamTextSpan
	bytes     int
	text      string
	ambiguous bool
}

type pendingReplyGrounding struct {
	anchor *replyGroundingAnchor
	raw    json.RawMessage
	bytes  int
	spans  int
}

// replyGroundingIndex retains only proven output intervals. It never uses an
// ID to deduplicate output, and never searches output for a matching substring.
// Late metadata is resolved at EOF so a later ambiguity can invalidate the ID.
type replyGroundingIndex struct {
	anchors                         map[string]*replyGroundingAnchor
	pending                         []pendingReplyGrounding
	idBytes, textBytes, spans       int
	pendingBytes, workBytes, checks int
}

func decodeGroundingReplyID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var id string
	if !streamString(raw, &id) {
		return "", contract.Fail(502, "DATA_LOSS", "Business reply identity is invalid")
	}
	if len(id) > maxGroundingReplyIDBytes {
		return "", replyGroundingLimit()
	}
	return id, nil
}

func (g *replyGroundingIndex) anchor(id string) (*replyGroundingAnchor, error) {
	if id == "" {
		return nil, nil
	}
	if anchor := g.anchors[id]; anchor != nil {
		return anchor, nil
	}
	if len(id) > maxGroundingReplyIDBytes || len(g.anchors) >= maxGroundingReplyIDs || g.idBytes+len(id) > maxGroundingReplyIDTotal {
		return nil, replyGroundingLimit()
	}
	if g.anchors == nil {
		g.anchors = make(map[string]*replyGroundingAnchor)
	}
	anchor := &replyGroundingAnchor{}
	g.anchors[id] = anchor
	g.idBytes += len(id)
	return anchor, nil
}

func (g *replyGroundingIndex) work(size, checks int) error {
	if size < 0 || checks < 0 || size > maxReplyGroundingWork-g.workBytes || checks > maxReplyGroundingChecks-g.checks {
		return replyGroundingLimit()
	}
	g.workBytes += size
	g.checks += checks
	return nil
}

// record runs only after the frame's Parts were delivered. previousParts is
// their pre-frame count, allowing cumulative old intervals only when that same
// ID already owned them. A new ID cannot claim another ID's identical text.
func (g *replyGroundingIndex) record(id string, part contract.Part, spans []streamTextSpan, previousParts int) error {
	a, err := g.anchor(id)
	if err != nil || a == nil || a.ambiguous {
		return err
	}
	if !part.IsOrdinaryText() {
		// Widget readback proves that thought text is excluded from the
		// shared reply's visible-text offsets, including signed thought Parts.
		// Preserve those Parts in output but never put their bytes in this map.
		if part.Thought && part.InlineData == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil {
			return nil
		}
		// The observed widget layout inserts unannotated suggestions between
		// text and its metadata. Other typed/signature boundaries have
		// no proven offset contract and permanently disable this ID's anchor.
		if part.InlineData != nil && part.InlineData.MIMEType == contract.SuggestionsMIMEType && !part.Thought && part.ThoughtSignature == "" {
			return nil
		}
		a.ambiguous = true
		return nil
	}
	for _, span := range spans {
		if err := g.work(0, 1); err != nil {
			return err
		}
		size := span.SourceEnd - span.SourceStart
		if size <= 0 {
			a.ambiguous = true
			return nil
		}
		if span.PartIndex < previousParts {
			covered, err := g.covers(a, span.PartIndex, span.PartStart, span.PartStart+size)
			if err != nil {
				return err
			}
			if !covered {
				a.ambiguous = true
				return nil
			}
			continue
		}
		if len(a.spans) > 0 {
			last := a.spans[len(a.spans)-1]
			lastEnd := last.PartStart + last.SourceEnd - last.SourceStart
			if span.PartIndex < last.PartIndex || (span.PartIndex == last.PartIndex && span.PartStart < lastEnd) {
				a.ambiguous = true
				return nil
			}
		}
		if g.spans >= maxStreamTextSpans || size > (4<<20)-g.textBytes {
			return replyGroundingLimit()
		}
		a.spans = append(a.spans, streamTextSpan{SourceStart: a.bytes, SourceEnd: a.bytes + size, PartIndex: span.PartIndex, PartStart: span.PartStart})
		a.bytes += size
		g.textBytes += size
		g.spans++
	}
	return nil
}

func (g *replyGroundingIndex) covers(a *replyGroundingAnchor, part, start, end int) (bool, error) {
	i := sort.Search(len(a.spans), func(i int) bool {
		s := a.spans[i]
		return s.PartIndex > part || (s.PartIndex == part && s.PartStart+s.SourceEnd-s.SourceStart > start)
	})
	for cursor := start; cursor < end; i++ {
		if err := g.work(0, 1); err != nil {
			return false, err
		}
		if i >= len(a.spans) {
			return false, nil
		}
		s := a.spans[i]
		if s.PartIndex != part || s.PartStart > cursor || s.PartStart+s.SourceEnd-s.SourceStart <= cursor {
			return false, nil
		}
		cursor = min(end, s.PartStart+s.SourceEnd-s.SourceStart)
	}
	return true, nil
}

func (g *replyGroundingIndex) deferMetadata(id string, raw json.RawMessage) error {
	a, err := g.anchor(id)
	if err != nil || a == nil || a.ambiguous || a.bytes == 0 {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if len(g.pending) >= contract.MaxGroundingSupports || len(raw) > contract.MaxGroundingBytes-g.pendingBytes {
		return replyGroundingLimit()
	}
	// RawMessage is the separately allocated metadata field, not the full
	// response frame. Save the prefix known at this reply, excluding later text.
	g.pending = append(g.pending, pendingReplyGrounding{a, raw, a.bytes, len(a.spans)})
	g.pendingBytes += len(raw)
	return nil
}

func (g *replyGroundingIndex) finish(output []contract.Part, grounding *groundingCollector) error {
	for _, p := range g.pending {
		a := p.anchor
		if a.ambiguous || p.bytes == 0 || p.spans == 0 {
			continue
		}
		if a.text == "" {
			if err := g.work(a.bytes, len(a.spans)); err != nil {
				return err
			}
			var text strings.Builder
			text.Grow(a.bytes)
			for _, span := range a.spans {
				if span.PartIndex < 0 || span.PartIndex >= len(output) || !output[span.PartIndex].IsOrdinaryText() {
					a.ambiguous = true
					break
				}
				value := output[span.PartIndex].Text
				end := span.PartStart + span.SourceEnd - span.SourceStart
				if span.PartStart < 0 || end > len(value) || !utf8.ValidString(value[span.PartStart:end]) {
					a.ambiguous = true
					break
				}
				text.WriteString(value[span.PartStart:end])
			}
			if a.ambiguous {
				continue
			}
			a.text = text.String()
		}
		text, spans := a.text[:p.bytes], a.spans[:p.spans]
		if err := g.work(len(text)+len(p.raw), len(spans)); err != nil {
			return err
		}
		count, exact := exactLateGroundingSegments(p.raw, text)
		if !exact {
			continue // Sources were already collected; never guess this anchor.
		}
		if count > contract.MaxGroundingSupports || count > maxReplyGroundingChecks/max(1, len(spans)) {
			return replyGroundingLimit()
		}
		if err := g.work(0, count*len(spans)); err != nil {
			return err
		}
		if err := grounding.addProjected(p.raw, text, spans); err != nil {
			return err
		}
	}
	return nil
}

func exactLateGroundingSegments(raw json.RawMessage, text string) (int, bool) {
	var metadata struct {
		Segments json.RawMessage `json:"segments"`
	}
	if json.Unmarshal(raw, &metadata) != nil || len(metadata.Segments) == 0 {
		return 0, false
	}
	decoder := json.NewDecoder(bytes.NewReader(metadata.Segments))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return 0, false
	}
	count := 0
	for decoder.More() {
		count++
		if count > contract.MaxGroundingSupports {
			return count, true // The caller reports the fixed resource failure.
		}
		var segment map[string]json.RawMessage
		if decoder.Decode(&segment) != nil || segment == nil {
			return 0, false
		}
		start, err := groundingOffset(segment["startIndex"], true)
		if err != nil {
			return 0, false
		}
		end, err := groundingOffset(segment["endIndex"], false)
		if err != nil || start < 0 || end <= start || end > int64(len(text)) || (start > 0 && !utf8.RuneStart(text[int(start)])) || (end < int64(len(text)) && !utf8.RuneStart(text[int(end)])) {
			return 0, false
		}
		var exact string
		if !streamString(segment["text"], &exact) || exact == "" || exact != text[int(start):int(end)] {
			return 0, false
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return 0, false
	}
	return count, count > 0
}

func replyGroundingLimit() error {
	return contract.Fail(502, "RESOURCE_EXHAUSTED", "Business reply grounding exceeds its resource budget")
}
