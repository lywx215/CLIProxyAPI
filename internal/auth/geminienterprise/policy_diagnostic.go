package geminienterprise

import (
	"encoding/json"
	"slices"
	"strings"
)

// PolicyDiagnostic exposes decisions and check types, never matched phrases,
// Model Armor findings, policy names or nested service error messages.
type PolicyDiagnostic struct {
	Verdict         string   `json:"verdict,omitempty"`
	ViolationSource string   `json:"violation_source,omitempty"`
	Checks          []string `json:"checks,omitempty"`
}

func parsePolicyDiagnostic(raw json.RawMessage) *PolicyDiagnostic {
	if len(raw) == 0 || len(raw) > 64*1024 {
		return nil
	}
	var body struct {
		Verdict         string          `json:"verdict"`
		ViolationSource string          `json:"violationSource"`
		PolicyResults   json.RawMessage `json:"policyResults"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	detail := &PolicyDiagnostic{}
	if body.Verdict == "BLOCK" || body.Verdict == "ALLOW" {
		detail.Verdict = body.Verdict
	}
	if slices.Contains([]string{"SYSTEM", "PROMPT", "ATTACHMENT"}, body.ViolationSource) {
		detail.ViolationSource = body.ViolationSource
	}
	var results []struct {
		BannedPhrase json.RawMessage `json:"bannedPhraseEnforcementResult"`
		ModelArmor   json.RawMessage `json:"modelArmorEnforcementResult"`
	}
	// Unknown check details cannot erase an explicit upstream decision.
	if json.Unmarshal(body.PolicyResults, &results) != nil {
		results = nil
	}
	for _, result := range results {
		for _, check := range []struct {
			name string
			raw  json.RawMessage
		}{{"banned_phrase", result.BannedPhrase}, {"model_armor", result.ModelArmor}} {
			var object map[string]json.RawMessage
			if json.Unmarshal(check.raw, &object) == nil && object != nil && !slices.Contains(detail.Checks, check.name) {
				detail.Checks = append(detail.Checks, check.name)
			}
		}
	}
	if detail.Verdict == "" && detail.ViolationSource == "" && len(detail.Checks) == 0 {
		return nil
	}
	return detail
}

// PolicyBlocksGeneration is determined by Google's structured verdict only.
// A refusal-looking sentence is still ordinary model text.
func PolicyBlocksGeneration(raw json.RawMessage) bool {
	detail := parsePolicyDiagnostic(raw)
	return detail != nil && detail.Verdict == "BLOCK"
}

func (d *PolicyDiagnostic) summary() string {
	var fields []string
	if d.Verdict != "" {
		fields = append(fields, "verdict="+d.Verdict)
	}
	if d.ViolationSource != "" {
		fields = append(fields, "source="+d.ViolationSource)
	}
	if len(d.Checks) > 0 {
		fields = append(fields, "checks="+strings.Join(d.Checks, ","))
	}
	return strings.Join(fields, "; ")
}
