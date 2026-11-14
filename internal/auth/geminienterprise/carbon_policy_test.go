package geminienterprise

import (
	"slices"
	"testing"
)

func TestHydrogenRequiresPaidSubscriptionInEveryPolicy(t *testing.T) {
	trial, paid := true, false
	for _, observation := range []SubscriptionObservation{
		{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial},
		{Status: "stale", Edition: "business_standard", FreeTrial: &trial},
		{Status: "inactive", FreeTrial: &paid},
		{Status: "ok", FreeTrial: nil},
		{Status: "unknown"},
	} {
		for _, mode := range []string{"auto", "catalog", "argon_only", "exclude_argon", "custom"} {
			for _, allModels := range []bool{false, true} {
				settings := map[string]any{
					"model_policy": mode, "allowed_models": []string{HydrogenModel},
					AllModelsEnabledKey: allModels, ArgonForceEnabledKey: true, HydrogenEnabledKey: true,
				}
				policy := ModelPolicyFor(settings, observation)
				if policy.Allows(HydrogenModel) || policy.Allows("models/"+HydrogenModel) {
					t.Fatalf("%+v, %s, all=%v allowed Hydrogen", observation, mode, allModels)
				}
				if !policy.Allows(ArgonModel) || !policy.Allows(CarbonModel) {
					t.Fatal("historical force must still apply to Argon and Carbon")
				}
			}
		}
	}
}

func TestTrialHydrogenExclusionRequiresAnExplicitUpstreamModel(t *testing.T) {
	trial := true
	policy := ModelPolicyFor(map[string]any{"model_policy": "catalog"}, SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial})
	if !policy.Restricted() || policy.Allows("auto") || !policy.Allows(ArgonModel) || policy.Allows(HydrogenModel) {
		t.Fatalf("trial routing must pin an allowed upstream model: %+v", policy)
	}
	if !slices.Equal(policy.EnforcedExcludedModels, []string{HydrogenModel}) {
		t.Fatalf("missing trial exclusion: %+v", policy)
	}
}

func TestTrialArgonTemplatesStillApplyToCarbon(t *testing.T) {
	trial := true
	for _, scenario := range []struct {
		edition      string
		argon, flash bool
	}{
		{"enterprise_plus", false, true},
		{"business_standard", true, false},
		{"enterprise_standard", true, true},
	} {
		policy := ModelPolicyFor(nil, SubscriptionObservation{Status: "ok", Edition: scenario.edition, FreeTrial: &trial})
		if policy.Allows(ArgonModel) != scenario.argon || policy.Allows(CarbonModel) != scenario.argon || policy.Allows("gemini-3.8-flash") != scenario.flash {
			t.Fatalf("trial model policy changed for %s: %+v", scenario.edition, policy)
		}
	}
}
