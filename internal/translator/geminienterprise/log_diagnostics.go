package geminienterprise

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

// LogDiagnostic is an internal projection. It is never serialized into model API errors.
// No upstream message, description, metadata value or user-defined field is retained.
type LogDiagnostic struct {
	Status   string
	Reason   string
	Category string
	Evidence string
	Fields   []FieldViolation
}

// LogUpstreamDiagnostic returns an independent snapshot without exposing raw errors.
func LogUpstreamDiagnostic(err error, status int) *LogDiagnostic {
	var detail *Error
	if !errors.As(err, &detail) || detail.Status != status || detail.logDiagnostic == nil {
		return nil
	}
	copy := *detail.logDiagnostic
	copy.Fields = slices.Clone(copy.Fields)
	return &copy
}

// LogFieldPath rejects user-defined fields while preserving public protocol paths.
func LogFieldPath(path string) string { return safeUpstreamFieldPath(path) }

func logUpstreamDiagnostic(body upstreamErrorBody, public *UpstreamDiagnostic, status int) *LogDiagnostic {
	d := &LogDiagnostic{Category: "unclassified", Evidence: "unknown"}
	if !validUpstreamErrorStatus(body, status) {
		return d
	}
	// RPC codes with different names can share an HTTP status. Keep the public
	// error's historical handling, but do not infer a log reason from that conflict.
	rpcNames := [...]string{"", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND", "ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED"}
	if body.Code > 0 && body.Code < 100 && body.Status != "" && (body.Code >= len(rpcNames) || rpcNames[body.Code] != body.Status) {
		d.Evidence = "conflict"
		return d
	}
	if public != nil {
		d.Status, d.Reason = public.Status, public.Reason
		d.Fields = slices.Clone(public.FieldViolations)
		for i := range d.Fields {
			// Public compatibility retains its historical description classifier.
			// The new log projection independently requires explicit evidence.
			d.Fields[i].Reason = ""
		}
	}
	for _, detail := range body.Details {
		if detail.Type != "type.googleapis.com/google.rpc.ErrorInfo" || !slices.Contains([]string{"googleapis.com", "discoveryengine.googleapis.com", "serviceusage.googleapis.com", "iam.googleapis.com"}, detail.Domain) {
			continue
		}
		category := logReasonCategory(detail.Reason)
		if category == "" {
			if d.Reason != "" {
				d.Reason, d.Category, d.Evidence = "", "unclassified", "conflict"
				return d
			}
			continue
		}
		if d.Reason != "" && d.Reason != detail.Reason {
			d.Reason, d.Category, d.Evidence = "", "unclassified", "conflict"
			return d
		}
		d.Reason, d.Category, d.Evidence = detail.Reason, category, "error_info"
	}
	if d.Evidence == "error_info" {
		return d
	}
	if len(d.Fields) > 0 {
		d.Evidence = "field_violation"
		conflicts := map[string]bool{}
		for _, detail := range body.Details {
			if detail.Type != "type.googleapis.com/google.rpc.BadRequest" {
				continue
			}
			for _, violation := range detail.FieldViolations {
				for i := range d.Fields {
					if safeUpstreamFieldPath(violation.Field) == d.Fields[i].Field {
						reason := logFieldReason(violation.Reason, violation.Description, d.Fields[i].Field)
						if prior := d.Fields[i].Reason; prior != "" && reason != "" && prior != reason {
							conflicts[d.Fields[i].Field] = true
						}
						if reason != "" {
							d.Fields[i].Reason = reason
						}
					}
				}
			}
		}
		for i := range d.Fields {
			if conflicts[d.Fields[i].Field] {
				d.Fields[i].Reason = ""
				d.Evidence = "conflict"
			}
		}
		if d.Evidence == "conflict" {
			return d
		}
		if d.Fields[0].Reason != "" {
			d.Category = d.Fields[0].Reason
		}
		return d
	}
	// Only explicit protocol-shaped templates are classified. Arbitrary prose,
	// quoted values and descriptions never become a diagnostic string.
	if field, category := logMessageCategory(body.Message); category != "" {
		d.Category, d.Evidence = category, "message_template"
		if field != "" {
			d.Fields = []FieldViolation{{Field: field, Reason: category}}
		}
	}
	return d
}

func logFieldReason(reason, description, field string) string {
	switch reason {
	case "REQUIRED_FIELD_MISSING", "FIELD_REQUIRED", "MISSING_REQUIRED_FIELD":
		return "required"
	case "INVALID_TYPE", "FIELD_TYPE_MISMATCH":
		return "invalid_type"
	case "UNKNOWN_FIELD", "UNSUPPORTED_FIELD":
		return "unsupported_field"
	case "INVALID_VALUE", "FIELD_VALUE_INVALID":
		return "invalid_value"
	case "INPUT_TOO_LONG", "INPUT_TOO_LARGE", "MAX_LENGTH_EXCEEDED":
		return "input_too_large"
	}
	if path, category := logMessageCategory(description); category != "" && (path == "" || path == field) {
		return category
	}
	_, category := logMessageCategory(field + " " + description)
	return category
}

func logReasonCategory(reason string) string {
	switch reason {
	case "MODEL_NOT_ENABLED":
		return "model_unavailable"
	case "SERVICE_DISABLED", "BILLING_DISABLED", "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "IAM_PERMISSION_DENIED", "USER_PROJECT_DENIED", "CONSUMER_INVALID":
		return "permission_denied"
	case "CUSTOMER_POLICY_VIOLATION":
		return "content_policy"
	case "LLM_POLICY_CHECK_VIOLATION":
		return "llm_policy"
	}
	return ""
}

var logFieldMessage = regexp.MustCompile(`(?i)^([a-z_][a-z0-9_.\[\]]{0,191})\s+(must be nonempty text|must not be empty|is required|must be a string|must be an object|must be an array|is not supported|has an invalid value)(?:[.;:]|\s|$)`)
var logUnknownFieldMessage = regexp.MustCompile(`(?i)^Invalid JSON payload received\. Unknown name ["']([a-z_][a-z0-9_.\[\]]{0,191})["']`)
var logInputLimitMessage = regexp.MustCompile(`(?i)^(?:the )?(?:query\.text|input|prompt|query|request)(?: token count| text| length| size)? (?:is too (?:long|large)|exceeds? (?:the )?(?:maximum|limit)|(?:is |was )?longer than (?:the )?(?:maximum|limit))(?:[.;:]|\s|$)`)
var logTokenLimitMessage = regexp.MustCompile(`(?i)^The input token count \([0-9]{1,12}\) exceeds the maximum number of tokens allowed \([0-9]{1,12}\)(?:[.;:]|\s|$)`)

func logMessageCategory(message string) (string, string) {
	if match := logFieldMessage.FindStringSubmatch(message); len(match) == 3 {
		field := safeUpstreamFieldPath(match[1])
		if field == "" {
			return "", ""
		}
		category := "invalid_value"
		switch strings.ToLower(match[2]) {
		case "must be nonempty text", "must not be empty", "is required":
			category = "required"
		case "must be a string", "must be an object", "must be an array":
			category = "invalid_type"
		case "is not supported":
			category = "unsupported_field"
		}
		return field, category
	}
	if match := logUnknownFieldMessage.FindStringSubmatch(message); len(match) == 2 {
		if field := safeUpstreamFieldPath(match[1]); field != "" {
			return field, "unsupported_field"
		}
	}
	if logInputLimitMessage.MatchString(message) || logTokenLimitMessage.MatchString(message) {
		return "", "input_too_large"
	}
	return "", ""
}
