package geminienterprise

import (
	"encoding/json"
	"testing"
)

func TestCarbonRequiresManagerAdmissionInEveryArgonPolicy(t *testing.T) {
	for _, trial := range []bool{false, true} {
		for _, edition := range []string{"enterprise_plus", "enterprise_standard", "business_standard"} {
			for _, status := range []string{"ok", "stale", "unknown", "inactive"} {
				for _, mode := range []string{"auto", "catalog", "argon_only", "exclude_argon", "custom"} {
					metadata := map[string]any{"model_policy": mode, "allowed_models": []string{ArgonModel, CarbonModel}, "free_trial": false}
					before, _ := json.Marshal(metadata)
					policy := ModelPolicyFor(metadata, SubscriptionObservation{Status: status, Edition: edition, FreeTrial: &trial})
					want := false
					if policy.Allows(CarbonModel) != want || policy.Allows("models/"+CarbonModel) != want {
						t.Fatalf("trial=%v edition=%s status=%s mode=%s: %+v", trial, edition, status, mode, policy)
					}
					after, _ := json.Marshal(metadata)
					if string(before) != string(after) {
						t.Fatal("runtime enforcement changed saved settings")
					}
				}
			}
		}
	}
	paid := false
	for _, metadata := range []map[string]any{
		{"model_policy": "custom", "allowed_models": []string{ArgonModel}},
		{"model_policy": "custom", "allowed_models": []string{}},
		{"model_policy": "argon_only", "excluded_models": []string{"gemini-4-c*"}},
	} {
		if ModelPolicyFor(metadata, SubscriptionObservation{Status: "ok", FreeTrial: &paid}).Allows(CarbonModel) {
			t.Fatalf("Carbon bypassed an explicit user restriction: %v", metadata)
		}
	}
}

func TestCarbonAdmissionPreservesExistingTrialAutomaticSelection(t *testing.T) {
	trial := true
	policy := ModelPolicyFor(map[string]any{"model_policy": "catalog"}, SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial})
	if policy.Restricted() || !policy.Allows("auto") || !policy.Allows(ArgonModel) || policy.Allows(CarbonModel) {
		t.Fatalf("Carbon admission changed unrelated trial routing: %+v", policy)
	}
	if len(policy.EnforcedExcludedModels) != 0 {
		t.Fatalf("a trial subscription carried an obsolete Carbon exclusion: %+v", policy)
	}
}

func TestTrialCarbonAdmissionPreservesArgonTemplates(t *testing.T) {
	trial := true
	for _, scenario := range []struct {
		name, edition, mode string
		allowed             []string
		argon, flash, auto  bool
	}{
		{"plus-auto", "enterprise_plus", "auto", nil, false, true, false},
		{"business-auto", "business_standard", "auto", nil, true, false, false},
		{"standard-auto", "enterprise_standard", "auto", nil, true, true, true},
		{"catalog", "enterprise_plus", "catalog", nil, true, true, true},
		{"argon-only", "enterprise_plus", "argon_only", nil, true, false, false},
		{"exclude-argon", "business_standard", "exclude_argon", nil, false, true, false},
		{"custom-argon", "enterprise_standard", "custom", []string{ArgonModel, CarbonModel}, true, false, false},
		{"custom-empty", "enterprise_standard", "custom", []string{}, false, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			metadata := map[string]any{"model_policy": scenario.mode, "allowed_models": scenario.allowed}
			for _, admitted := range []bool{false, true} {
				policy := ModelPolicyFor(metadata, SubscriptionObservation{Status: "ok", Edition: scenario.edition, FreeTrial: &trial, State: "ACTIVE"})
				policy.CarbonAllowed = admitted
				if policy.Allows(ArgonModel) != scenario.argon || policy.Allows("gemini-3.8-flash") != scenario.flash || policy.Allows("auto") != scenario.auto {
					t.Fatalf("Carbon admission=%v changed Argon or Auto behavior: %+v", admitted, policy)
				}
			}
		})
	}
}
