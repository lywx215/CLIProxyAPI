// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package contract

import (
	"encoding/base64"
	"net/url"
	"strings"
	"unicode/utf8"
)

// These values describe upstream execution. No code is executed locally.
type ExecutableCode struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}
type CodeExecutionResult struct {
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

func ValidExecutionOutcome(value string) bool {
	return value == "OUTCOME_OK" || value == "OUTCOME_FAILED" || value == "OUTCOME_DEADLINE_EXCEEDED"
}
func (p Part) IsOrdinaryText() bool {
	return p.InlineData == nil && p.ExecutableCode == nil && p.CodeExecutionResult == nil && !p.Thought && p.ThoughtSignature == ""
}
func (p Part) PayloadBytes() int {
	size := len(p.Text) + len(p.ThoughtSignature)
	if p.InlineData != nil {
		size += len(p.InlineData.Data) + base64.StdEncoding.EncodedLen(len(p.InlineData.Data))
	}
	if p.ExecutableCode != nil {
		size += len(p.ExecutableCode.Language) + len(p.ExecutableCode.Code)
	}
	if p.CodeExecutionResult != nil {
		size += len(p.CodeExecutionResult.Outcome) + len(p.CodeExecutionResult.Output)
	}
	return size
}

const MaxGroundingBytes = 512 << 10
const MaxGroundingChunks = 128
const MaxGroundingSupports = 512
const MaxGroundingURIBytes = 8192
const MaxGroundingTitleBytes = 4096

// ValidGroundingWebURI permits a citation target without fetching it. Non-web
// URIs and userinfo are deliberately excluded from the projected web subset.
// Keep field sizes separate so the collector can reject oversized web sources
// while continuing to ignore sources that cannot be projected at all.
func ValidGroundingWebURI(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Hostname() != "" && parsed.User == nil && parsed.Opaque == ""
}

func ValidGroundingWeb(value *GroundingWeb) bool {
	return value != nil && len(value.URI) <= MaxGroundingURIBytes && len(value.Title) <= MaxGroundingTitleBytes && utf8.ValidString(value.URI) && utf8.ValidString(value.Title) && ValidGroundingWebURI(value.URI)
}

// A document URI identifies a retrieved source, not the tool that retrieved it.
// The same bounded HTTP(S) policy applies to both native source variants.
func ValidGroundingRetrievedContext(value *GroundingRetrievedContext) bool {
	return value != nil && ValidGroundingWeb(&GroundingWeb{URI: value.URI, Title: value.Title})
}

func ValidGroundingChunk(value GroundingChunk) bool {
	if value.Web != nil {
		return value.RetrievedContext == nil && ValidGroundingWeb(value.Web)
	}
	return ValidGroundingRetrievedContext(value.RetrievedContext)
}

// Grounding is candidate metadata, never a synthetic text or function part.
// It is emitted once at successful completion; indices refer to the ordered
// cumulative content.parts delivered by the handler, not each delta alone.
type GroundingMetadata struct {
	GroundingChunks   []GroundingChunk   `json:"groundingChunks,omitempty"`
	GroundingSupports []GroundingSupport `json:"groundingSupports,omitempty"`
}
type GroundingChunk struct {
	Web              *GroundingWeb              `json:"web,omitempty"`
	RetrievedContext *GroundingRetrievedContext `json:"retrievedContext,omitempty"`
}
type GroundingWeb struct {
	URI   string `json:"uri"`
	Title string `json:"title,omitempty"`
}
type GroundingRetrievedContext struct {
	URI   string `json:"uri"`
	Title string `json:"title,omitempty"`
}
type GroundingSupport struct {
	Segment               GroundingSegment `json:"segment"`
	GroundingChunkIndices []int            `json:"groundingChunkIndices"`
}
type GroundingSegment struct {
	PartIndex  int    `json:"partIndex"`
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	Text       string `json:"text"`
}

const MaxCitationBytes = 128 << 10
const MaxCitationSources = 128
const MaxCitationLicenseBytes = 4096

// The Enterprise citation offsets are not necessarily native candidate
// offsets. Preserve source attribution without inventing a text location.
type CitationMetadata struct {
	CitationSources []CitationSource `json:"citationSources,omitempty"`
}
type CitationSource struct {
	URI     string `json:"uri"`
	License string `json:"license,omitempty"`
}

func ValidCitationSource(value CitationSource) bool {
	return len(value.URI) <= MaxGroundingURIBytes && ValidGroundingWebURI(value.URI) &&
		len(value.License) <= MaxCitationLicenseBytes && utf8.ValidString(value.License)
}
