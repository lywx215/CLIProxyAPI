package geminienterprise

import (
	"errors"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func TestEnterpriseStructuredPolicyPrecedesDenialReply(t *testing.T) {
	for _, state := range []string{"SKIPPED", "SUCCEEDED", "IN_PROGRESS"} {
		for _, prefix := range []string{"", `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"earlier output"}}]}}` + "\n"} {
			body := prefix + `{"answer":{"state":"` + state + `","replies":[{"content":{"text":"private-denial-reply"}}],"customerPolicyEnforcementResult":{"verdict":"BLOCK","violationSource":"SYSTEM","policyResults":[{"modelArmorEnforcementResult":{"modelArmorViolation":"private-finding"}}]}}}`
			for _, fragment := range []int{1, 7, 4096} {
				var visible strings.Builder
				var rejected strings.Builder
				done := false
				parser := &Parser{RejectedText: func(text string) { rejected.WriteString(text) }}
				err := parser.Parse(&fragmentReader{data: []byte(body), size: fragment}, func(event Event) error {
					visible.WriteString(event.Text)
					done = done || event.Type == "done"
					return nil
				})
				var skipped *ge.SkippedError
				if !errors.As(err, &skipped) || skipped.PolicyDetails() == nil || skipped.PolicyDetails().ViolationSource != "SYSTEM" || done || strings.Contains(visible.String(), "private-denial") || strings.Contains(err.Error(), "private-") {
					t.Fatalf("policy rejection became a successful reply: visible=%q error=%v", visible.String(), err)
				}
				if (prefix != "") != strings.Contains(visible.String(), "earlier output") || !skipped.IsRequestStop() {
					t.Fatal("earlier output or no-replay boundary changed")
				}
				if rejected.String() != "private-denial-reply" {
					t.Fatal("observed rejection text was lost from token accounting")
				}
			}
		}
	}
}

func TestEnterpriseRefusalTextAloneDoesNotInventPolicyDecision(t *testing.T) {
	const refusal = "This response was blocked by your organization's content policy."
	for _, policy := range []string{`{}`, `{"verdict":"ALLOW"}`, `{"verdict":"UNSPECIFIED"}`} {
		body := `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"` + refusal + `"}}],"customerPolicyEnforcementResult":` + policy + `}},{"answer":{"state":"SUCCEEDED"}}]`
		output := NewOutput()
		done := false
		err := (&Parser{}).Parse(strings.NewReader(body), func(event Event) error {
			done = done || event.Type == "done"
			_, _, errAdd := output.Add(event)
			return errAdd
		})
		if err != nil || !done || output.Text != refusal {
			t.Fatalf("upstream text or decision was overridden: %v", err)
		}
	}
}
