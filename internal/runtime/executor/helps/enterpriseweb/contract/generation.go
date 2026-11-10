// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package contract

import (
	"context"
	"encoding/json"
	"errors"
)

// Request retains original field presence for validation. Only validated fields
// are mapped into QueryParts / Generation before an account can be leased.
type Request struct {
	Raw         map[string]json.RawMessage
	QueryParts  []map[string]string
	Generation  map[string]json.RawMessage
	Attachments []Attachment
	// GoogleSearch requests only the empty Gemini Google Search tool. The core
	// verifies the selected account's search provider before enabling it.
	GoogleSearch bool
	// History contains a digest for each validated content, including the last
	// user turn. The core may only continue a previously completed exact prefix.
	History     []string
	OptionsHash string
	// InputUsage contains only locally counted validated input, never credentials.
	InputUsage Usage
	// ContextSummary contains bounded diagnostics only, never client text or file bytes.
	ContextSummary ContextSummary
}

type ContextSummary struct {
	Contents, LogicalContents, Attachments, AttachmentBytes int
	FilteredThoughts, FilteredSignatures                    int
}

// Attachment contains an immutable request-owned snapshot. The account lease
// uploads new files only after a Business session has been selected. Upstream
// file IDs remain private to that account epoch and exact continuing session.
type Attachment struct {
	Filename string
	MIMEType string
	Hash     string
	Data     []byte
}

type FileResolver func(context.Context, string, string) (Attachment, error)

type Selection struct {
	PublicID   string
	ModelID    string
	Auto       bool
	AccountIDs []string
}
type Part struct {
	Text                string               `json:"text"`
	Thought             bool                 `json:"thought,omitempty"`
	ThoughtSignature    string               `json:"thoughtSignature,omitempty"`
	InlineData          *InlineData          `json:"inlineData,omitempty"`
	ExecutableCode      *ExecutableCode      `json:"executableCode,omitempty"`
	CodeExecutionResult *CodeExecutionResult `json:"codeExecutionResult,omitempty"`
}

const SuggestionsMIMEType = "application/json+suggestions"

// InlineData owns decoded bytes. The standard JSON encoder writes data using
// padded standard base64, as required by the native Gemini Blob wire format.
type InlineData struct {
	MIMEType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

// MarshalJSON preserves the native Part union, including an explicitly empty
// text part. An inlineData part must not acquire an invented text:"" sibling.
func (p Part) MarshalJSON() ([]byte, error) {
	values := 0
	for _, present := range []bool{p.InlineData != nil, p.ExecutableCode != nil, p.CodeExecutionResult != nil} {
		if present {
			values++
		}
	}
	if values == 0 {
		type plain Part
		return json.Marshal(plain(p))
	}
	if p.Text != "" || values != 1 {
		return nil, errors.New("Business part contains multiple content values")
	}
	return json.Marshal(struct {
		InlineData          *InlineData          `json:"inlineData,omitempty"`
		ExecutableCode      *ExecutableCode      `json:"executableCode,omitempty"`
		CodeExecutionResult *CodeExecutionResult `json:"codeExecutionResult,omitempty"`
		Thought             bool                 `json:"thought,omitempty"`
		ThoughtSignature    string               `json:"thoughtSignature,omitempty"`
	}{p.InlineData, p.ExecutableCode, p.CodeExecutionResult, p.Thought, p.ThoughtSignature})
}

type Event struct {
	Parts             []Part
	Finished          bool
	GroundingMetadata *GroundingMetadata
	CitationMetadata  *CitationMetadata
}

type Generator interface {
	Generate(context.Context, Selection, Request, func(Event) error) error
}

// Failure is safe to expose; no underlying transport error or response body is
// included. Submitted operations are not retried automatically.
type Failure struct {
	Code             int
	Status, Message  string
	Reason, Param    string
	Limit            *InputLimit
	AttachmentReason AttachmentReason
}

func (e *Failure) Error() string { return e.Message }
func Fail(code int, status, message string) error {
	return &Failure{Code: code, Status: status, Message: message}
}

type AttachmentReason string

const (
	AttachmentFieldConflict    AttachmentReason = "field_conflict"
	AttachmentInvalidObject    AttachmentReason = "invalid_attachment_object"
	AttachmentUnexpectedField  AttachmentReason = "unexpected_attachment_field"
	AttachmentInvalidMIME      AttachmentReason = "invalid_mime"
	AttachmentUnsupportedMIME  AttachmentReason = "unsupported_mime"
	AttachmentMissingData      AttachmentReason = "missing_attachment_data"
	AttachmentInvalidData      AttachmentReason = "invalid_attachment_data"
	AttachmentInvalidBase64    AttachmentReason = "invalid_base64"
	AttachmentCharsetMismatch  AttachmentReason = "attachment_charset_mismatch"
	AttachmentUnsupportedModel AttachmentReason = "unsupported_model_attachment"
)

func (r AttachmentReason) Valid() bool {
	switch r {
	case AttachmentFieldConflict, AttachmentInvalidObject, AttachmentUnexpectedField, AttachmentInvalidMIME,
		AttachmentUnsupportedMIME, AttachmentMissingData, AttachmentInvalidData, AttachmentInvalidBase64,
		AttachmentCharsetMismatch, AttachmentUnsupportedModel:
		return true
	default:
		return false
	}
}
