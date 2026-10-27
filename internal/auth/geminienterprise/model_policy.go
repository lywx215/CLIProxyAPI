package geminienterprise

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

const ArgonModel = "gemini-4-argon"

type ModelPolicy struct {
	Mode                  string   `json:"mode"`
	EffectiveMode         string   `json:"effective_mode"`
	Status                string   `json:"status"`
	Reason                string   `json:"reason,omitempty"`
	AllowedModels         []string `json:"allowed_models"`
	ExcludedModels        []string `json:"excluded_models"`
	EnforcedAllowedModels []string `json:"enforced_allowed_models"`
	EnforcedReason        string   `json:"enforced_reason"`
}

func ValidModelPolicy(mode string) bool {
	switch mode {
	case "auto", "catalog", "argon_only", "exclude_argon", "custom":
		return true
	}
	return false
}

// ModelPolicySettings validates both saved credentials and management patches.
func ModelPolicySettings(metadata map[string]any) (string, []string, error) {
	mode := "auto"
	if raw, exists := metadata["model_policy"]; exists {
		value, ok := raw.(string)
		if !ok || !ValidModelPolicy(value) {
			return "", nil, fmt.Errorf("model_policy must be auto, catalog, argon_only, exclude_argon or custom")
		}
		mode = value
	}
	var allowed []string
	if raw, exists := metadata["allowed_models"]; exists && raw != nil {
		data, err := json.Marshal(raw)
		if err != nil {
			return "", nil, fmt.Errorf("allowed_models must be an array of model IDs or null")
		}
		if string(data) == "null" {
			return mode, nil, nil
		}
		if json.Unmarshal(data, &allowed) != nil || allowed == nil {
			return "", nil, fmt.Errorf("allowed_models must be an array of model IDs or null")
		}
		seen := map[string]bool{}
		out := make([]string, 0, len(allowed))
		for _, value := range allowed {
			value = strings.TrimSpace(value)
			if value == "" || strings.ContainsAny(value, "*?,()/\\") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
				return "", nil, fmt.Errorf("allowed_models must contain exact upstream model IDs")
			}
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
		allowed = out
	}
	return mode, allowed, nil
}

func ModelPolicyFor(metadata map[string]any, subscription SubscriptionObservation) ModelPolicy {
	mode, allowed, err := ModelPolicySettings(metadata)
	policy := ModelPolicy{Mode: mode, EffectiveMode: mode, Status: "ok", ExcludedModels: []string{}}
	if raw, exists := metadata["excluded_models"]; exists {
		var excluded []string
		data, _ := json.Marshal(raw)
		if json.Unmarshal(data, &excluded) == nil {
			policy.ExcludedModels = append(policy.ExcludedModels, excluded...)
		}
	}
	if err != nil {
		policy.Mode, policy.EffectiveMode, policy.Status, policy.Reason = "auto", "auto", "blocked", "invalid_model_policy"
		policy.EnforcedAllowedModels, policy.EnforcedReason = []string{}, policy.Reason
		return policy
	}
	confirmed := (subscription.Status == "ok" || subscription.Status == "stale") && subscription.FreeTrial != nil
	if mode == "auto" && confirmed {
		policy.EffectiveMode = "catalog"
		if subscription.FreeTrial != nil && *subscription.FreeTrial {
			switch subscription.Edition {
			case "enterprise_plus":
				policy.EffectiveMode = "exclude_argon"
			case "business_standard":
				policy.EffectiveMode = "argon_only"
			}
		}
	}
	if policy.EffectiveMode == "argon_only" {
		policy.AllowedModels = []string{ArgonModel}
	}
	if policy.EffectiveMode == "exclude_argon" {
		policy.ExcludedModels = append(policy.ExcludedModels, ArgonModel)
	}
	if policy.EffectiveMode == "custom" {
		policy.AllowedModels = allowed
	}
	if !confirmed {
		policy.Status, policy.Reason = "blocked", subscription.Reason
		if policy.Reason == "" {
			policy.Reason = "subscription_unverified"
		}
		policy.EnforcedAllowedModels, policy.EnforcedReason = []string{}, policy.Reason
		return policy
	}
	if subscription.Status == "stale" {
		policy.Status = "stale"
	}
	if !*subscription.FreeTrial {
		// The paid ceiling comes only from runtime license verification, never saved settings.
		policy.EnforcedAllowedModels = []string{ArgonModel, "gemini-3-pro-image", "veo-3.1"}
		policy.EnforcedReason = "paid_subscription_restricted"
	}
	return policy
}

func (p ModelPolicy) Restricted() bool {
	return p.EnforcedAllowedModels != nil || p.AllowedModels != nil || len(p.ExcludedModels) > 0
}

func (p ModelPolicy) Allows(model string) bool {
	if p.Status == "blocked" {
		return false
	}
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		return false
	}
	if p.Restricted() && IsAutoModel(model) {
		return false
	}
	if p.EnforcedAllowedModels != nil && !slices.Contains(p.EnforcedAllowedModels, model) {
		return false
	}
	if p.AllowedModels != nil {
		found := false
		for _, allowed := range p.AllowedModels {
			if model == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, excluded := range p.ExcludedModels {
		if MatchModelPattern(excluded, model) {
			return false
		}
	}
	return true
}

func IsAutoModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "auto" || model == "default" || strings.HasSuffix(model, "-auto") || strings.HasPrefix(model, "auto-")
}

// MatchModelPattern preserves the existing case-insensitive '*' exclusion syntax.
func MatchModelPattern(pattern, model string) bool {
	pattern, model = strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(strings.TrimSpace(model))
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == model
	}
	if !strings.HasPrefix(model, parts[0]) {
		return false
	}
	model = model[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(model, part)
		if i < 0 {
			return false
		}
		model = model[i+len(part):]
	}
	return strings.HasSuffix(model, parts[len(parts)-1])
}
