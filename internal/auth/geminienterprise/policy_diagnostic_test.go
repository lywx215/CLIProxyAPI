package geminienterprise

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestGenerationStreamInvalidRequestStatusIsPreserved(t *testing.T) {
	for _, scenario := range []struct {
		code   int
		status string
	}{
		{3, "INVALID_ARGUMENT"}, {9, "FAILED_PRECONDITION"}, {11, "OUT_OF_RANGE"},
	} {
		raw := []byte(fmt.Sprintf(`{"error":{"code":%d,"status":%q,"message":"private-upstream-message"}}`, scenario.code, scenario.status))
		err := ParseGenerationError(raw, 200, nil, config.GeminiEnterpriseOAuth{}, nil, time.Now(), false)
		if err.StatusCode() != 400 || err.ModelNotEnabled || !err.IsRequestScoped() || !err.IsRequestStop() || err.Restriction != nil || strings.Contains(err.Error(), "private-") {
			t.Fatalf("stream invalid-request status or ordinary retry behavior changed: %v", err)
		}
	}
}

func TestSkippedPolicyDiagnosticContainsOnlyDecisions(t *testing.T) {
	raw := json.RawMessage(`{"verdict":"BLOCK","violationSource":"PROMPT","policyResults":[{"bannedPhraseEnforcementResult":{"bannedPhrases":["private-phrase"]}},{"modelArmorEnforcementResult":{"modelArmorViolation":"private-finding","error":{"message":"Bearer private-token"}}}]}`)
	err := NewSkippedErrorWithPolicy(nil, raw, false)
	if len(err.SkipReasons()) != 1 || err.SkipReasons()[0] != "CUSTOMER_POLICY_VIOLATION" || !strings.Contains(err.SkipMessage(), "source=PROMPT") {
		t.Fatalf("lost upstream policy decision: %v", err)
	}
	policy := err.PolicyDetails()
	if policy == nil || policy.Verdict != "BLOCK" || policy.ViolationSource != "PROMPT" || len(policy.Checks) != 2 || !PolicyBlocksGeneration(raw) {
		t.Fatalf("missing safe policy categories: %#v", policy)
	}
	if strings.Contains(err.Error(), "private-") || strings.Contains(err.SkipMessage(), "private-") {
		t.Fatal("raw policy details leaked")
	}
	policy.Checks[0] = "private-mutated"
	policy.Verdict = "ALLOW"
	if err.PolicyDetails().Verdict != "BLOCK" || strings.Contains(err.Error(), "private-mutated") {
		t.Fatal("policy diagnostics expose mutable internal state")
	}
}

func TestPolicyDiagnosticsDoNotInferDecisions(t *testing.T) {
	for _, raw := range []string{
		`{"verdict":"ALLOW"}`, `{"verdict":"private-BLOCK","violationSource":"private-source"}`,
		`{"message":"This response was blocked by your organization's content policy."}`,
		`{"policyResults":[{"bannedPhraseEnforcementResult":null}]}`,
		`{"verdict":"BLOCK"} private-trailing`, `null`,
	} {
		if PolicyBlocksGeneration(json.RawMessage(raw)) {
			t.Fatalf("policy decision inferred from invalid or unrelated data: %s", raw)
		}
		err := NewSkippedErrorWithPolicy(json.RawMessage(`["NON_ASSIST_SEEKING_QUERY_IGNORED"]`), json.RawMessage(raw), false)
		if strings.Contains(err.Error(), "private-") || len(err.SkipReasons()) != 1 || err.SkipReasons()[0] != "NON_ASSIST_SEEKING_QUERY_IGNORED" {
			t.Fatalf("untrusted policy details affected rejection: %v", err)
		}
	}
}

func TestPolicyDiagnosticsPreserveBlockWithUnknownCheckDetails(t *testing.T) {
	raw := json.RawMessage(`{"verdict":"BLOCK","violationSource":"PROMPT","policyResults":false}`)
	err := NewSkippedErrorWithPolicy(nil, raw, false)
	if !PolicyBlocksGeneration(raw) || err.PolicyDetails() == nil || err.PolicyDetails().Verdict != "BLOCK" || len(err.PolicyDetails().Checks) != 0 {
		t.Fatalf("unknown policy details erased the upstream verdict: %v", err)
	}
}
