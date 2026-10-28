package geminienterprise

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEnterpriseStructuredUpstreamDiagnostics(t *testing.T) {
	const fieldError = `{"code":400,"status":"INVALID_ARGUMENT","message":"Bearer private-token private-prompt","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"contents[88].parts[0].functionResponse.name","description":"must be nonempty text; private-value"},{"field":"query.text","reason":"INVALID_TYPE","description":"private-description"}]},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"SERVICE_DISABLED","metadata":{"consumer":"projects/private-project","activationUrl":"https://private.invalid"}}]}`
	for _, body := range []string{fieldError, `{"error":` + fieldError + `}`, `[{"error":` + fieldError + `}]`} {
		err := UpstreamHTTPError(400, []byte(body))
		message, code, param := PublicErrorDetail(fmt.Errorf("private-wrapper: %w", err), 400)
		detail := PublicUpstreamDiagnostic(err, 400)
		if code != "upstream_invalid_request" || param != "contents[88].parts[0].functionResponse.name" || !strings.Contains(message, "requires a nonempty value") {
			t.Fatalf("missing concrete field diagnostic: %v", err)
		}
		if detail == nil || detail.Status != "INVALID_ARGUMENT" || detail.Reason != "SERVICE_DISABLED" || len(detail.FieldViolations) != 2 || detail.FieldViolations[0].Reason != "required" || detail.FieldViolations[1].Reason != "invalid_type" {
			t.Fatalf("lost structured diagnostic: %#v", detail)
		}
		if strings.Contains(err.Error(), "private-") || strings.Contains(message, "private-") {
			t.Fatalf("upstream values leaked: %v", err)
		}
		detail.FieldViolations[0].Field = "private-mutated"
		if strings.Contains(err.Error(), "private-mutated") || PublicUpstreamDiagnostic(err, 503) != nil {
			t.Fatal("public diagnostic shares mutable state or ignores status")
		}
	}
}

func TestEnterpriseUpstreamDiagnosticsFailClosed(t *testing.T) {
	for _, body := range []string{
		`{"error":{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"private.invalid","reason":"CUSTOMER_POLICY_VIOLATION"}]}}`,
		`{"error":{"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"private-reason","metadata":{"reason":"CUSTOMER_POLICY_VIOLATION"}}]}}`,
		`{"error":{"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"private_value.name","description":"private-description"},{"field":"contents[88].parts[0].functionResponse.name\nprivate-token"},{"field":"query.private_token"}]}]}}`,
		`{"error":{"status":"INVALID_ARGUMENT","details":false}}`,
		`{"error":{"status":"INVALID_ARGUMENT"}} trailing private-token`,
		`[{"error":{"status":"INVALID_ARGUMENT"}},{"error":{"status":"PERMISSION_DENIED"}}]`,
		`{"error":{"status":"INVALID_ARGUMENT"},"padding":"` + strings.Repeat("x", 64*1024) + `"}`,
	} {
		err := UpstreamHTTPError(400, []byte(body))
		if strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "private_") {
			t.Fatal("untrusted diagnostic values leaked")
		}
		detail := PublicUpstreamDiagnostic(err, 400)
		if detail != nil && (detail.Reason != "" || len(detail.FieldViolations) > 0 || detail.Status != "INVALID_ARGUMENT") {
			t.Fatalf("unsafe details became trusted: %#v", detail)
		}
		if _, code, _ := PublicErrorDetail(err, 400); code != "upstream_invalid_request" {
			t.Fatalf("untrusted data changed request classification: %v", err)
		}
	}
}

func TestEnterpriseStructuredPolicyHTTPErrorStopsRetry(t *testing.T) {
	err := UpstreamHTTPError(400, []byte(`{"error":{"code":400,"status":"FAILED_PRECONDITION","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"CUSTOMER_POLICY_VIOLATION","metadata":{"private":"private-policy"}}]}}`))
	message, code, _ := PublicErrorDetail(err, 400)
	if code != "content_policy_violation" || !err.(*Error).IsRequestStop() || !err.(*Error).IsRequestScoped() || !strings.Contains(message, "FAILED_PRECONDITION") || strings.Contains(err.Error(), "private-policy") {
		t.Fatalf("structured policy denial was not safely retained: %v", err)
	}
}

func TestEnterpriseEmptyHistoricalFunctionNamesReachUpstreamContext(t *testing.T) {
	for _, key := range []string{"functionCall", "function_call", "functionResponse", "function_response"} {
		for _, name := range []string{"missing", "empty", "spaces", "null"} {
			t.Run(key+"/"+name, func(t *testing.T) {
				contents := make([]any, 0, 89)
				for range 88 {
					contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"text": "earlier"}}})
				}
				function := map[string]any{}
				switch name {
				case "empty":
					function["name"] = ""
				case "spaces":
					function["name"] = "  "
				case "null":
					function["name"] = nil
				}
				role, field := "user", "response"
				if key == "functionCall" || key == "function_call" {
					role, field = "model", "args"
				}
				function[field] = map[string]any{"result": "preserved-value"}
				contents = append(contents, map[string]any{"role": role, "parts": []any{map[string]any{key: function}}})
				body, _ := json.Marshal(map[string]any{"contents": contents})
				original := string(body)
				input, err := Normalize(body, "gemini", "gemini-3.8-flash")
				if err != nil || ValidateFinal(input.Body) != nil || string(body) != original {
					t.Fatalf("historical name was rejected or request was mutated: %v", err)
				}
				var payload struct{ Query struct{ Text string } }
				_ = json.Unmarshal(input.Body, &payload)
				serialized, _ := json.Marshal(function)
				if !strings.Contains(payload.Query.Text, string(serialized)) {
					t.Fatal("history was dropped, repaired or replaced")
				}
			})
		}
	}
}
