package geminienterprise

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestEnterpriseLogOnlyGoogleDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, category, evidence, field string
	}{
		{"required_template", `{"status":"INVALID_ARGUMENT","message":"contents[88].parts[0].functionResponse.name must be nonempty text; private-value"}`, "required", "message_template", "contents[88].parts[0].functionResponse.name"},
		{"code_only_template", `{"code":400,"message":"query.text must be a string. private-prompt"}`, "invalid_type", "message_template", "query.text"},
		{"type_template", `{"status":"INVALID_ARGUMENT","message":"query.text must be a string. private-prompt"}`, "invalid_type", "message_template", "query.text"},
		{"field_template", `{"status":"INVALID_ARGUMENT","message":"Invalid JSON payload received. Unknown name \"temperature\": private-value"}`, "unsupported_field", "message_template", "temperature"},
		{"value_template", `{"status":"INVALID_ARGUMENT","message":"generationSpec.modelId has an invalid value: private-model"}`, "invalid_value", "message_template", "generationSpec.modelId"},
		{"length_template", `{"status":"INVALID_ARGUMENT","message":"Query.text exceeds the maximum length: private-prompt"}`, "input_too_large", "message_template", ""},
		{"token_template", `{"status":"INVALID_ARGUMENT","message":"The input token count (12345) exceeds the maximum number of tokens allowed (1234). private-prompt"}`, "input_too_large", "message_template", ""},
		{"bad_request", `{"status":"INVALID_ARGUMENT","message":"private-prompt","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"query.text","reason":"MAX_LENGTH_EXCEEDED","description":"private-value"}]}]}`, "input_too_large", "field_violation", "query.text"},
		{"error_info", `{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"SERVICE_DISABLED","metadata":{"private":"private-token"}}]}`, "permission_denied", "error_info", ""},
		{"llm_policy_info", `{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"LLM_POLICY_CHECK_VIOLATION"}]}`, "llm_policy", "error_info", ""},
		{"unknown", `{"status":"INVALID_ARGUMENT","message":"private-prompt is long, maybe too large"}`, "unclassified", "unknown", ""},
		{"unsafe_field", `{"status":"INVALID_ARGUMENT","message":"query.private_token must be a string: private-value"}`, "unclassified", "unknown", ""},
		{"unsafe_domain", `{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"private.invalid","reason":"SERVICE_DISABLED"}]}`, "unclassified", "unknown", ""},
		{"unclassified_description", `{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"query.text","description":"private prompt mentions required but not supported"}]}]}`, "unclassified", "field_violation", "query.text"},
		{"status_conflict", `{"code":7,"status":"INVALID_ARGUMENT","message":"query.text must be a string"}`, "unclassified", "unknown", ""},
		{"same_http_rpc_conflict", `{"code":3,"status":"FAILED_PRECONDITION","message":"query.text must be a string"}`, "unclassified", "conflict", ""},
		{"reason_conflict", `{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"SERVICE_DISABLED"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"MODEL_NOT_ENABLED"}]}`, "unclassified", "conflict", ""},
		{"field_conflict", `{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"query.text","reason":"INVALID_TYPE"},{"field":"query.text","reason":"MAX_LENGTH_EXCEEDED"}]}]}`, "unclassified", "conflict", "query.text"},
		{"malformed", `{"message":"private-token",`, "unclassified", "unknown", ""},
		{"oversized", `{"message":"query.text must be a string","padding":"` + strings.Repeat("x", 64*1024) + `"}`, "unclassified", "unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, raw := range []string{tc.body, `{"error":` + tc.body + `}`, `[{"error":` + tc.body + `}]`} {
				err := UpstreamHTTPError(400, []byte(raw))
				d := LogUpstreamDiagnostic(fmt.Errorf("private-wrapper: %w", err), 400)
				if d == nil || d.Category != tc.category || d.Evidence != tc.evidence {
					t.Fatalf("unexpected log diagnostic: %#v", d)
				}
				if tc.field != "" && (len(d.Fields) == 0 || d.Fields[0].Field != tc.field) {
					t.Fatalf("lost safe path: %#v", d)
				}
				encoded, _ := json.Marshal(d)
				if strings.Contains(string(encoded), "private") || strings.Contains(err.Error(), "private") {
					t.Fatalf("unsafe diagnostic: %s", encoded)
				}
				// Log-only projections must never enter the public error object.
				before := err.Error()
				err.(*Error).logDiagnostic = nil
				if err.Error() != before {
					t.Fatal("internal diagnostics changed public API serialization")
				}
			}
		})
	}
	if LogUpstreamDiagnostic(errors.New("query.text must be a string"), 400) != nil {
		t.Fatal("free-form wrapped errors were treated as Google evidence")
	}
}

func TestEnterpriseLogDiagnosticCopiesAndPublicCompatibility(t *testing.T) {
	err := UpstreamStreamError(400, []byte(`{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"query.text must be a string: private-value"}}`))
	want := `{"error":{"code":"upstream_invalid_request","message":"The upstream service rejected the request; check model availability and supported request fields. Upstream status: INVALID_ARGUMENT.","type":"invalid_request_error","upstream":{"status":"INVALID_ARGUMENT"}}}`
	if err.Error() != want {
		t.Fatalf("public error changed: %s", err)
	}
	d := LogUpstreamDiagnostic(err, 400)
	d.Fields[0].Field = "private-mutated"
	if got := LogUpstreamDiagnostic(err, 400); got.Fields[0].Field != "query.text" || LogUpstreamDiagnostic(err, 503) != nil {
		t.Fatal("diagnostic shares mutable state or ignores the status")
	}
}
