package modelconfig

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// GeminiAPIKeyAlias binds a global rule to an available source for one credential.
// Model is a private routing projection; it is never written into key.Models.
type GeminiAPIKeyAlias struct {
	Rule       config.OAuthModelAlias
	SourceName string
	Model      config.GeminiModel
}

// GeminiAPIKeyAliases resolves global defaults after credential/group mappings.
// Identity model rows retain an allowlist entry without overriding global aliases.
func GeminiAPIKeyAliases(cfg *config.Config, key *config.GeminiKey) []GeminiAPIKeyAlias {
	if cfg == nil || key == nil || len(cfg.APIKeyModelAlias["gemini"]) == 0 {
		return nil
	}
	sources := key.Models
	if len(sources) == 0 {
		for _, info := range registry.GetGeminiModels() {
			if info != nil {
				sources = append(sources, config.GeminiModel{Name: info.ID, Alias: info.ID, IsCompat: info.IsCompat})
			}
		}
	}
	reserved := make(map[string]struct{})
	localSources := make(map[string]struct{})
	for _, model := range key.Models {
		name, alias := strings.TrimSpace(model.Name), strings.TrimSpace(model.Alias)
		if name == "" {
			name = alias
		}
		if alias == "" {
			alias = name
		}
		reserved[aliasModelBase(name)] = struct{}{}
		reserved[aliasModelBase(alias)] = struct{}{}
		if !strings.EqualFold(name, alias) {
			localSources[aliasModelBase(name)] = struct{}{}
		}
	}
	seen := make(map[string]struct{})
	var out []GeminiAPIKeyAlias
	for _, rule := range cfg.APIKeyModelAlias["gemini"] {
		rule.Name, rule.Alias = strings.TrimSpace(rule.Name), strings.TrimSpace(rule.Alias)
		if rule.Name == "" || rule.Alias == "" || strings.EqualFold(rule.Name, rule.Alias) {
			continue
		}
		aliasKey := strings.ToLower(rule.Alias)
		if _, exists := seen[aliasKey]; exists {
			continue
		}
		if _, exists := reserved[aliasModelBase(rule.Alias)]; exists {
			continue
		}
		if _, exists := localSources[aliasModelBase(rule.Name)]; exists {
			continue
		}
		var source *config.GeminiModel
		for _, exact := range []bool{true, false} {
			for i := range sources {
				name := strings.TrimSpace(sources[i].Name)
				if name == "" {
					name = strings.TrimSpace(sources[i].Alias)
				}
				if name != "" && ((exact && strings.EqualFold(name, rule.Name)) || (!exact && aliasModelBase(name) == aliasModelBase(rule.Name))) {
					source = &sources[i]
					break
				}
			}
			if source != nil {
				break
			}
		}
		if source == nil {
			continue
		}
		model := *source
		sourceName := strings.TrimSpace(model.Name)
		if sourceName == "" {
			sourceName = strings.TrimSpace(model.Alias)
		}
		if thinking.ParseSuffix(sourceName).HasSuffix {
			rule.Name = sourceName
		}
		if apiKeyAliasExcluded(key.ExcludedModels, sourceName, rule.Name, rule.Alias) {
			continue
		}
		model.Name, model.Alias = rule.Name, rule.Alias
		model.ForceMapping = rule.ForceMapping
		if displayName := strings.TrimSpace(rule.DisplayName); displayName != "" {
			model.DisplayName = displayName
		}
		seen[aliasKey] = struct{}{}
		out = append(out, GeminiAPIKeyAlias{Rule: rule, SourceName: sourceName, Model: model})
	}
	return out
}

// GeminiAPIKeyRoutingModels returns effective mappings without changing the source config.
func GeminiAPIKeyRoutingModels(cfg *config.Config, key *config.GeminiKey) []config.GeminiModel {
	if key == nil {
		return nil
	}
	aliases := GeminiAPIKeyAliases(cfg, key)
	if len(aliases) == 0 {
		return key.Models
	}
	models := append([]config.GeminiModel(nil), key.Models...)
	for _, alias := range aliases {
		models = append(models, alias.Model)
	}
	return models
}

func aliasModelBase(model string) string {
	return strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(model)).ModelName))
}

func apiKeyAliasExcluded(patterns []string, models ...string) bool {
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		for _, model := range models {
			for _, value := range []string{strings.ToLower(strings.TrimSpace(model)), aliasModelBase(model)} {
				if matchAliasExclusion(pattern, value) {
					return true
				}
			}
		}
	}
	return false
}

// matchAliasExclusion follows the existing star-only model exclusion syntax.
func matchAliasExclusion(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(value, part)
		if index < 0 {
			return false
		}
		value = value[index+len(part):]
	}
	return strings.HasSuffix(value, parts[len(parts)-1])
}
