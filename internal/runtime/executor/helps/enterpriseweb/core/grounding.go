// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

// groundingCollector projects a bounded text-grounding subset. Indices address
// the actual ordered Parts delivered by this process. A cumulative reply may
// anchor supports only through a proven source-to-delivered-Part projection.
// The complete result is emitted once, after successful upstream completion.
type groundingCollector struct {
	chunks   []contract.GroundingChunk
	supports []contract.GroundingSupport
	byURI    map[string]int
	seen     map[[32]byte]struct{}
	bytes    int
	err      error
}

func (g *groundingCollector) add(raw json.RawMessage, text string, partIndex int) error {
	var spans []streamTextSpan
	if partIndex >= 0 && text != "" {
		spans = []streamTextSpan{{SourceEnd: len(text), PartIndex: partIndex}}
	}
	return g.addProjected(raw, text, spans)
}

func (g *groundingCollector) addProjected(raw json.RawMessage, text string, spans []streamTextSpan) error {
	if g.err != nil {
		return g.err
	}
	g.err = g.collect(raw, text, spans)
	return g.err
}

func (g *groundingCollector) collect(raw json.RawMessage, text string, spans []streamTextSpan) error {
	if len(raw) > contract.MaxGroundingBytes {
		return groundingLimitFailure()
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if validateStreamFrame(raw) != nil {
		return groundingMalformed()
	}
	var metadata struct {
		References []json.RawMessage `json:"references"`
		Segments   json.RawMessage   `json:"segments"`
	}
	if json.Unmarshal(raw, &metadata) != nil {
		return groundingMalformed()
	}
	remap := make([]int, len(metadata.References))
	for i := range remap {
		remap[i] = -1
	}
	for i, reference := range metadata.References {
		var ref map[string]json.RawMessage
		if json.Unmarshal(reference, &ref) != nil || ref == nil {
			return groundingMalformed()
		}
		document := ref["documentMetadata"]
		if len(document) == 0 || bytes.Equal(bytes.TrimSpace(document), []byte("null")) {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(document, &doc) != nil || doc == nil {
			return groundingMalformed()
		}
		uri, err := groundingString(doc["uri"])
		if err != nil {
			return err
		}
		title, err := groundingString(doc["title"])
		if err != nil {
			return err
		}
		if !contract.ValidGroundingWebURI(uri) {
			// Retain the existing HTTP(S) citation-target subset; never fetch
			// the URI or reflect credentials from it to a downstream response.
			continue
		}
		retrieved := &contract.GroundingRetrievedContext{URI: uri, Title: title}
		if !contract.ValidGroundingRetrievedContext(retrieved) {
			return groundingMalformed()
		}
		if index, exists := g.byURI[uri]; exists {
			remap[i] = index
			continue
		}
		if len(g.chunks) >= contract.MaxGroundingChunks {
			return groundingLimitFailure()
		}
		chunk := contract.GroundingChunk{RetrievedContext: retrieved}
		if err := g.reserve(chunk); err != nil {
			return err
		}
		if g.byURI == nil {
			g.byURI = make(map[string]int)
		}
		remap[i] = len(g.chunks)
		g.byURI[uri] = len(g.chunks)
		g.chunks = append(g.chunks, chunk)
	}
	if len(spans) == 0 {
		// Do not infer that metadata-only frames describe the most recent
		// visible text, or that cumulative terminal text is one emitted Part.
		return nil
	}
	if err := validateGroundingSpans(text, spans); err != nil {
		return err
	}
	var segments []json.RawMessage
	if len(metadata.Segments) > 0 && json.Unmarshal(metadata.Segments, &segments) != nil {
		return groundingMalformed()
	}
	for _, rawSegment := range segments {
		var segment map[string]json.RawMessage
		if json.Unmarshal(rawSegment, &segment) != nil || segment == nil {
			return groundingMalformed()
		}
		start, err := groundingOffset(segment["startIndex"], true)
		if err != nil {
			return err
		}
		end, err := groundingOffset(segment["endIndex"], false)
		if err != nil || start < 0 || end <= start || end > int64(len(text)) {
			return groundingMalformed()
		}
		if (start > 0 && !utf8.RuneStart(text[int(start)])) || (end < int64(len(text)) && !utf8.RuneStart(text[int(end)])) {
			return groundingMalformed()
		}
		value := text[int(start):int(end)]
		if provided, exists := segment["text"]; exists {
			exact, err := groundingString(provided)
			if err != nil || exact != value {
				return groundingMalformed()
			}
		}
		var references []json.RawMessage
		if field, exists := segment["referenceIndices"]; exists {
			if json.Unmarshal(field, &references) != nil {
				return groundingMalformed()
			}
		}
		indices := []int{}
		seenIndices := make(map[int]bool)
		for _, reference := range references {
			var index int
			if bytes.Equal(bytes.TrimSpace(reference), []byte("null")) || json.Unmarshal(reference, &index) != nil || index < 0 || index >= len(remap) {
				return groundingMalformed()
			}
			mapped := remap[index]
			if mapped >= 0 && !seenIndices[mapped] {
				indices = append(indices, mapped)
				seenIndices[mapped] = true
			}
		}
		if len(indices) == 0 {
			continue
		}
		cursor := int(start)
		for _, span := range spans {
			from, to := max(int(start), span.SourceStart), min(int(end), span.SourceEnd)
			if from >= to {
				continue
			}
			if from != cursor {
				return groundingMalformed()
			}
			support := contract.GroundingSupport{
				Segment: contract.GroundingSegment{PartIndex: span.PartIndex,
					StartIndex: span.PartStart + from - span.SourceStart,
					EndIndex:   span.PartStart + to - span.SourceStart, Text: text[from:to]},
				GroundingChunkIndices: indices,
			}
			if err := g.addSupport(support); err != nil {
				return err
			}
			cursor = to
		}
		if cursor != int(end) {
			return groundingMalformed()
		}
	}
	return nil
}

func validateGroundingSpans(text string, spans []streamTextSpan) error {
	if !utf8.ValidString(text) {
		return groundingMalformed()
	}
	if len(spans) > maxStreamTextSpans {
		return groundingLimitFailure()
	}
	for i, span := range spans {
		if span.SourceStart < 0 || span.SourceEnd <= span.SourceStart || span.SourceEnd > len(text) || span.PartIndex < 0 || span.PartIndex >= maxStreamParts || span.PartStart < 0 || span.PartStart > (4<<20)-(span.SourceEnd-span.SourceStart) {
			return groundingMalformed()
		}
		if (span.SourceStart > 0 && !utf8.RuneStart(text[span.SourceStart])) || (span.SourceEnd < len(text) && !utf8.RuneStart(text[span.SourceEnd])) {
			return groundingMalformed()
		}
		if i > 0 {
			previous := spans[i-1]
			if span.SourceStart < previous.SourceEnd || span.PartIndex < previous.PartIndex || (span.PartIndex == previous.PartIndex && span.PartStart < previous.PartStart+previous.SourceEnd-previous.SourceStart) {
				return groundingMalformed()
			}
		}
	}
	return nil
}

func (g *groundingCollector) addSupport(support contract.GroundingSupport) error {
	encoded, err := json.Marshal(support)
	if err != nil {
		return groundingMalformed()
	}
	key := sha256.Sum256(encoded)
	if _, exists := g.seen[key]; exists {
		return nil
	}
	if len(g.supports) >= contract.MaxGroundingSupports || g.bytes+len(encoded)+1 > contract.MaxGroundingBytes-64 {
		return groundingLimitFailure()
	}
	g.bytes += len(encoded) + 1
	if g.seen == nil {
		g.seen = make(map[[32]byte]struct{})
	}
	g.seen[key] = struct{}{}
	g.supports = append(g.supports, support)
	return nil
}

func (g *groundingCollector) reserve(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return groundingMalformed()
	}
	// Include list separators and reserve object/list field-name overhead.
	if g.bytes+len(encoded)+1 > contract.MaxGroundingBytes-64 {
		return groundingLimitFailure()
	}
	g.bytes += len(encoded) + 1
	return nil
}

func (g *groundingCollector) result() (*contract.GroundingMetadata, error) {
	if g.err != nil {
		return nil, g.err
	}
	if len(g.chunks) == 0 {
		return nil, nil
	}
	result := &contract.GroundingMetadata{GroundingChunks: g.chunks, GroundingSupports: g.supports}
	encoded, err := json.Marshal(result)
	if err != nil {
		g.err = groundingMalformed()
	} else if len(encoded) > contract.MaxGroundingBytes {
		g.err = groundingLimitFailure()
	}
	if g.err != nil {
		return nil, g.err
	}
	return result, nil
}

func groundingString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", groundingMalformed()
	}
	return value, nil
}

func groundingOffset(raw json.RawMessage, zeroDefault bool) (int64, error) {
	if len(raw) == 0 {
		if zeroDefault {
			return 0, nil
		}
		return 0, groundingMalformed()
	}
	value := string(bytes.TrimSpace(raw))
	if len(value) > 0 && value[0] == '"' {
		if json.Unmarshal(raw, &value) != nil {
			return 0, groundingMalformed()
		}
	}
	if value == "" {
		return 0, groundingMalformed()
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, groundingMalformed()
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, groundingMalformed()
	}
	return parsed, nil
}

func groundingMalformed() error {
	return contract.Fail(502, "DATA_LOSS", "Business grounding metadata is malformed")
}

func groundingLimitFailure() error {
	return contract.Fail(502, "RESOURCE_EXHAUSTED", "Business grounding metadata exceeds its limit")
}
