package geminienterprise

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestLLMPolicyErrorRequiresStructuredConsistentReason(t *testing.T) {
	for _, tc := range []struct {
		name, domain, statusName, reason string
		status, code                     int
		policy                           bool
	}{
		{"policy", "discoveryengine.googleapis.com", "INVALID_ARGUMENT", "LLM_POLICY_CHECK_VIOLATION", 400, 400, true},
		{"canonical_code", "googleapis.com", "INVALID_ARGUMENT", "LLM_POLICY_CHECK_VIOLATION", 400, 3, true},
		{"unknown_domain", "private.invalid", "INVALID_ARGUMENT", "LLM_POLICY_CHECK_VIOLATION", 400, 400, false},
		{"unrelated_reason", "googleapis.com", "INVALID_ARGUMENT", "MODEL_NOT_ENABLED", 400, 400, false},
		{"conflicting_status", "googleapis.com", "UNAVAILABLE", "LLM_POLICY_CHECK_VIOLATION", 400, 14, false},
		{"not_rate_limit", "googleapis.com", "RESOURCE_EXHAUSTED", "LLM_POLICY_CHECK_VIOLATION", 429, 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"error":{"code":%d,"status":%q,"message":"private-prompt LLM_POLICY_CHECK_VIOLATION","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":%q,"reason":%q,"metadata":{"access_token":"private-token"}}]}}`, tc.code, tc.statusName, tc.domain, tc.reason)
			err := UpstreamHTTPError(tc.status, []byte(raw)).(*Error)
			if (err.Kind == "upstream_policy_violation") != tc.policy || err.Status != tc.status || strings.Contains(err.Error(), "private-") {
				t.Fatalf("classification mismatch: %s", err.Error())
			}
			if tc.policy {
				var body struct {
					Error map[string]any `json:"error"`
				}
				_ = json.Unmarshal([]byte(err.Error()), &body)
				if !err.IsRequestScoped() || !err.IsRequestStop() || body.Error["retryable"] != false || !strings.Contains(err.Error(), "LLM_POLICY_CHECK_VIOLATION") {
					t.Fatal("policy refusal became retryable or lost its safe reason")
				}
			}
		})
	}
	err := UpstreamHTTPError(400, []byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"LLM_POLICY_CHECK_VIOLATION"}}`)).(*Error)
	if err.Kind == "upstream_policy_violation" {
		t.Fatal("message keyword became an authoritative policy reason")
	}
}
