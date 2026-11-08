package auth

import (
	"encoding/json"
	"strings"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// AntigravityEnterpriseModelRoute retains the native model identity through aliases.
type AntigravityEnterpriseModelRoute struct {
	ID          string
	CatalogID   string
	DisplayName string
	Model       ae.Model
}

// ProjectAntigravityEnterpriseModels is shared by runtime registration and management.
func ProjectAntigravityEnterpriseModels(cfg *config.Config, auth *Auth, catalog ae.Config) []AntigravityEnterpriseModelRoute {
	if !IsAntigravityEnterpriseBindingAuth(auth) {
		return nil
	}
	excluded := strings.Split(auth.Attributes["excluded_models"], ",")
	if cfg != nil && normalizeOAuthModelAliasAuthKind(auth.AuthKind()) != "apikey" {
		excluded = append(excluded, cfg.OAuthExcludedModels[ae.Provider]...)
	}
	var allowed []string
	rawAllowed, restrictAllowed := auth.Metadata["allowed_models"]
	if restrictAllowed {
		data, _ := json.Marshal(rawAllowed)
		restrictAllowed = json.Unmarshal(data, &allowed) == nil
	}
	models := make([]AntigravityEnterpriseModelRoute, 0)
	for _, model := range ae.Models(catalog) {
		blocked := false
		for _, pattern := range excluded {
			if aeModelPattern(strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(model.ID)) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		if restrictAllowed {
			found := false
			for _, id := range allowed {
				found = found || id == model.ID
			}
			if !found {
				continue
			}
		}
		models = append(models, AntigravityEnterpriseModelRoute{ID: model.ID, CatalogID: model.ID, DisplayName: model.ID, Model: model})
	}
	var aliases []config.OAuthModelAlias
	if OAuthModelAliasChannel(ae.Provider, auth.AuthKind()) != "" {
		aliases = OAuthModelAliasesFromAttributes(auth.Attributes)
		if cfg != nil {
			aliases = append(append([]config.OAuthModelAlias{}, aliases...), cfg.OAuthModelAlias[ae.Provider]...)
		}
	}
	forward := map[string][]config.OAuthModelAlias{}
	seenAlias := map[string]bool{}
	for _, alias := range aliases {
		alias.Name, alias.Alias = strings.TrimSpace(alias.Name), strings.TrimSpace(alias.Alias)
		key := strings.ToLower(alias.Alias)
		if key == "" || seenAlias[key] {
			continue
		}
		seenAlias[key] = true
		if alias.Name != "" && !strings.EqualFold(alias.Name, alias.Alias) {
			forward[strings.ToLower(alias.Name)] = append(forward[strings.ToLower(alias.Name)], alias)
		}
	}
	if len(forward) != 0 {
		aliased := make([]AntigravityEnterpriseModelRoute, 0, len(models))
		seen := map[string]bool{}
		add := func(model AntigravityEnterpriseModelRoute) bool {
			key := strings.ToLower(model.ID)
			if seen[key] {
				return false
			}
			seen[key] = true
			aliased = append(aliased, model)
			return true
		}
		for _, model := range models {
			entries := forward[strings.ToLower(model.ID)]
			if len(entries) == 0 {
				add(model)
				continue
			}
			keep, added := false, false
			for _, entry := range entries {
				keep = keep || entry.Fork
			}
			if keep {
				add(model)
			}
			for _, entry := range entries {
				clone := model
				clone.ID, clone.CatalogID = entry.Alias, entry.Alias
				if displayName := strings.TrimSpace(entry.DisplayName); displayName != "" {
					clone.DisplayName = displayName
				}
				added = add(clone) || added
			}
			if !keep && !added {
				add(model)
			}
		}
		models = aliased
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return models
	}
	projected := make([]AntigravityEnterpriseModelRoute, 0, len(models)*2)
	seen := map[string]bool{}
	add := func(model AntigravityEnterpriseModelRoute) {
		if !seen[model.ID] {
			seen[model.ID] = true
			projected = append(projected, model)
		}
	}
	for _, model := range models {
		if cfg == nil || !cfg.ForceModelPrefix || prefix == model.ID {
			add(model)
		}
		model.ID = prefix + "/" + model.ID
		add(model)
	}
	return projected
}

func aeModelPattern(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(value, last) {
		return false
	}
	value = value[:len(value)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(value, part)
		if index < 0 {
			return false
		}
		value = value[index+len(part):]
	}
	return true
}
