// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

const maxStreamParts = 4096
const maxStreamTextSpans = 2 * maxStreamParts

// streamTextSpan maps a byte interval in one original reply to an interval in
// the ordered output Parts. Prefix spans point to earlier output; suffix spans
// point to the exact Part that will be appended. No text is searched by value.
type streamTextSpan struct {
	SourceStart, SourceEnd int
	PartIndex, PartStart   int
}

type streamProjection struct {
	Parts   []contract.Part
	Text    [][]streamTextSpan
	Matched bool
}

// projectStreamParts removes a terminal prefix only when ALL previous output
// is represented in order. Ordinary text can be resegmented within a run;
// thought, signature, blob and typed-code boundaries must match exactly.
func projectStreamParts(previous, current []contract.Part, terminal bool) (streamProjection, error) {
	if len(previous) > maxStreamParts || len(current) > maxStreamParts {
		return streamProjection{}, streamPartsLimitFailure()
	}
	plan := streamProjection{Text: make([][]streamTextSpan, len(current))}
	start, offset, matched := 0, 0, false
	if terminal && len(previous) > 0 && streamPrefixHasIdentity(previous) {
		start, offset, matched = matchStreamPrefix(previous, current, plan.Text)
	}
	if !matched {
		// A partial match proves nothing about the rest of the snapshot. Drop
		// all tentative mappings and retain the entire frame without guessing.
		plan.Text = make([][]streamTextSpan, len(current))
		start, offset = 0, 0
	}
	plan.Matched = matched
	for i := start; i < len(current); i++ {
		part := current[i]
		cut := 0
		if i == start {
			cut = offset
			if cut > 0 {
				// Prefix matching can only split an unannotated text Part.
				if !part.IsOrdinaryText() || cut >= len(part.Text) || !utf8.RuneStart(part.Text[cut]) {
					return streamProjection{}, malformedStreamAt(streamInvalidText)
				}
				part.Text = part.Text[cut:]
			}
		}
		if streamGroundableText(part) && len(part.Text) > 0 {
			plan.Text[i] = append(plan.Text[i], streamTextSpan{SourceStart: cut, SourceEnd: len(current[i].Text), PartIndex: len(previous) + len(plan.Parts), PartStart: 0})
		}
		plan.Parts = append(plan.Parts, part)
	}
	if len(previous)+len(plan.Parts) > maxStreamParts {
		return streamProjection{}, streamPartsLimitFailure()
	}
	spans := 0
	for _, projection := range plan.Text {
		spans += len(projection)
	}
	if spans > maxStreamTextSpans {
		return streamProjection{}, streamPartsLimitFailure()
	}
	return plan, nil
}

func streamPartsLimitFailure() error {
	return contract.Fail(502, "RESOURCE_EXHAUSTED", "Business output exceeds 4096 parts")
}

func streamPrefixHasIdentity(parts []contract.Part) bool {
	for _, part := range parts {
		if !part.IsOrdinaryText() || part.Text != "" {
			return true
		}
	}
	// A run of empty ordinary text gives no content evidence of a snapshot.
	return false
}

func streamGroundableText(part contract.Part) bool {
	return !part.Thought && part.InlineData == nil && part.ExecutableCode == nil && part.CodeExecutionResult == nil
}

// matchStreamPrefix returns the terminal cursor only after consuming every
// previous Part. It never slides forward to find a similar suffix. Mappings
// produced on failure are tentative and discarded by projectStreamParts.
func matchStreamPrefix(previous, current []contract.Part, projections [][]streamTextSpan) (int, int, bool) {
	i, j := 0, 0
	for i < len(previous) {
		if j >= len(current) {
			return 0, 0, false
		}
		if !previous[i].IsOrdinaryText() {
			if !equalStreamPart(previous[i], current[j]) {
				return 0, 0, false
			}
			if streamGroundableText(current[j]) && current[j].Text != "" {
				projections[j] = append(projections[j], streamTextSpan{SourceEnd: len(current[j].Text), PartIndex: i})
			}
			i++
			j++
			continue
		}
		if !current[j].IsOrdinaryText() {
			return 0, 0, false
		}
		oldEnd, oldSize := i, 0
		for oldEnd < len(previous) && previous[oldEnd].IsOrdinaryText() {
			oldSize += len(previous[oldEnd].Text)
			oldEnd++
		}
		newEnd, newSize := j, 0
		for newEnd < len(current) && current[newEnd].IsOrdinaryText() {
			newSize += len(current[newEnd].Text)
			newEnd++
		}
		if oldSize > newSize || (oldEnd < len(previous) && oldSize != newSize) {
			return 0, 0, false
		}
		oldPart, newPart, oldOffset, newOffset := i, j, 0, 0
		remaining := oldSize
		for remaining > 0 {
			for oldPart < oldEnd && oldOffset == len(previous[oldPart].Text) {
				oldPart++
				oldOffset = 0
			}
			for newPart < newEnd && newOffset == len(current[newPart].Text) {
				newPart++
				newOffset = 0
			}
			size := min(len(previous[oldPart].Text)-oldOffset, len(current[newPart].Text)-newOffset)
			if previous[oldPart].Text[oldOffset:oldOffset+size] != current[newPart].Text[newOffset:newOffset+size] {
				return 0, 0, false
			}
			projections[newPart] = append(projections[newPart], streamTextSpan{SourceStart: newOffset, SourceEnd: newOffset + size, PartIndex: oldPart, PartStart: oldOffset})
			oldOffset += size
			newOffset += size
			remaining -= size
		}
		if oldEnd == len(previous) {
			for newPart < newEnd && newOffset == len(current[newPart].Text) {
				newPart++
				newOffset = 0
			}
			return newPart, newOffset, true
		}
		i, j = oldEnd, newEnd
	}
	return j, 0, true
}

func equalStreamPart(a, b contract.Part) bool {
	if a.Text != b.Text || a.Thought != b.Thought || a.ThoughtSignature != b.ThoughtSignature ||
		(a.InlineData == nil) != (b.InlineData == nil) ||
		(a.ExecutableCode == nil) != (b.ExecutableCode == nil) ||
		(a.CodeExecutionResult == nil) != (b.CodeExecutionResult == nil) {
		return false
	}
	if a.InlineData != nil && (a.InlineData.MIMEType != b.InlineData.MIMEType || !bytes.Equal(a.InlineData.Data, b.InlineData.Data)) {
		return false
	}
	if a.ExecutableCode != nil && *a.ExecutableCode != *b.ExecutableCode {
		return false
	}
	return a.CodeExecutionResult == nil || *a.CodeExecutionResult == *b.CodeExecutionResult
}
