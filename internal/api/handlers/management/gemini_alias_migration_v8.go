package management

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelconfig"
	"gopkg.in/yaml.v3"
)

type geminiMigrationRequest struct {
	Revision     string   `json:"revision"`
	CandidateIDs []string `json:"candidateIds,omitempty"`
}

type geminiMigrationCandidate struct {
	ID                string                 `json:"id"`
	Rule              config.OAuthModelAlias `json:"rule"`
	Targets           []GeminiKeyTarget      `json:"targets"`
	AffectedKeys      int                    `json:"affectedKeys"`
	OriginalNameAdded bool                   `json:"originalNameAdded"`
	sources           []geminiMigrationSource
	existingGlobal    bool
}

type geminiMigrationSource struct {
	target   GeminiKeyTarget
	models   *yaml.Node
	row      *yaml.Node
	rowIndex int
}

type geminiMigrationSkipped struct {
	Reason string `json:"reason"`
	Alias  string `json:"alias,omitempty"`
	Name   string `json:"name,omitempty"`
}

type geminiMigrationAnalysis struct {
	Candidates []geminiMigrationCandidate `json:"candidates"`
	Skipped    []geminiMigrationSkipped   `json:"skipped"`
}

// GeminiAliasMigrationPreview computes changes without saving or flattening groups.
func (h *Handler) GeminiAliasMigrationPreview(c *gin.Context) {
	var request geminiMigrationRequest
	if !decodeGeminiRequest(c, &request) {
		return
	}
	if len(request.CandidateIDs) != 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": "preview does not accept candidateIds"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	if !geminiRevisionRequired(c, request.Revision, state.Revision) {
		return
	}
	analysis, err := analyzeGeminiAliasMigration(state.Root)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_migration", "message": err.Error()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"revision": state.Revision, "candidates": analysis.Candidates, "skipped": analysis.Skipped})
}

// GeminiAliasMigrationApply recomputes selected candidates at the submitted revision.
func (h *Handler) GeminiAliasMigrationApply(c *gin.Context) {
	var request geminiMigrationRequest
	if !decodeGeminiRequest(c, &request) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	if !geminiRevisionRequired(c, request.Revision, state.Revision) {
		return
	}
	affected, err := applyGeminiAliasMigration(state.Root, request.CandidateIDs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_migration", "message": err.Error()})
		return
	}
	revision, err := h.commitConfigV8Locked(c.Request.Context(), state)
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "revision": revision, "affected": affected})
}

func geminiRuleKey(rule config.OAuthModelAlias) string {
	return strings.ToLower(strings.TrimSpace(rule.Name)) + "\x00" + strings.ToLower(strings.TrimSpace(rule.Alias)) +
		"\x00" + strings.TrimSpace(rule.DisplayName) + fmt.Sprintf("\x00%t", rule.ForceMapping)
}

func geminiMigrationRule(row *yaml.Node) (config.OAuthModelAlias, error) {
	var rule config.OAuthModelAlias
	if err := row.Decode(&rule); err != nil {
		return rule, err
	}
	rule.Name, rule.Alias, rule.DisplayName = strings.TrimSpace(rule.Name), strings.TrimSpace(rule.Alias), strings.TrimSpace(rule.DisplayName)
	if rule.Alias == "" {
		rule.Alias = rule.Name
	}
	return rule, nil
}

// Mapping and response rewriting differ between a migrated row and passthrough;
// every remaining model option must be identical before removing a duplicate.
func geminiModelMetadata(row *yaml.Node) any {
	copy := cloneConfigV8Node(row)
	deleteMapKey(copy, "alias")
	deleteMapKey(copy, "force-mapping")
	var metadata any
	_ = copy.Decode(&metadata)
	return metadata
}

func geminiModelCapabilities(row *yaml.Node) any {
	copy := cloneConfigV8Node(row)
	for _, field := range []string{"name", "alias", "display-name", "force-mapping"} {
		deleteMapKey(copy, field)
	}
	var metadata any
	_ = copy.Decode(&metadata)
	return metadata
}

func analyzeGeminiAliasMigration(root *yaml.Node) (geminiMigrationAnalysis, error) {
	analysis := geminiMigrationAnalysis{Candidates: []geminiMigrationCandidate{}, Skipped: []geminiMigrationSkipped{}}
	groups := configV8Node(root, geminiGroupsPath)
	if groups == nil {
		return analysis, nil
	}
	if groups.Kind != yaml.SequenceNode {
		return analysis, fmt.Errorf("Gemini groups must be a list")
	}
	type aggregate struct {
		rule                  config.OAuthModelAlias
		keys                  map[geminiKeyLocation]bool
		sources               map[*yaml.Node]geminiMigrationSource
		sourceKeys            map[*yaml.Node]map[geminiKeyLocation]bool
		sourceOriginalMissing map[*yaml.Node]bool
		originalMissing       bool
	}
	byRule := make(map[string]*aggregate)
	byAlias := make(map[string]map[string]bool)
	skipped := make(map[string]geminiMigrationSkipped)
	unsafeSources := make(map[*yaml.Node]bool)
	rememberSkipped := func(rule config.OAuthModelAlias, reason string) {
		key := geminiRuleKey(rule) + "\x00" + reason
		skipped[key] = geminiMigrationSkipped{Reason: reason, Alias: rule.Alias, Name: rule.Name}
	}
	for groupIndex, group := range groups.Content {
		keys := configV8Node(group, []string{"keys"})
		if keys == nil || keys.Kind != yaml.SequenceNode {
			return analysis, fmt.Errorf("groupIndex %d has invalid keys", groupIndex)
		}
		for keyIndex, key := range keys.Content {
			models := geminiEffectiveField(group, key, "models")
			if models == nil || models.Tag == "!!null" {
				continue
			}
			if models.Kind != yaml.SequenceNode {
				return analysis, fmt.Errorf("groupIndex %d keyIndex %d has invalid models", groupIndex, keyIndex)
			}
			target := GeminiKeyTarget{GroupIndex: groupIndex}
			if own := configV8Node(key, []string{"models"}); own != nil && own.Tag != "!!null" {
				index := keyIndex
				target.KeyIndex = &index
			}
			for rowIndex, row := range models.Content {
				rule, err := geminiMigrationRule(row)
				if err != nil || rule.Name == "" {
					return analysis, fmt.Errorf("invalid Gemini model mapping")
				}
				alias := strings.ToLower(rule.Alias)
				if byAlias[alias] == nil {
					byAlias[alias] = make(map[string]bool)
				}
				byAlias[alias][geminiRuleKey(rule)] = true
				if strings.EqualFold(rule.Name, rule.Alias) {
					continue
				}
				unsafeReason := ""
				for _, other := range models.Content {
					otherRule, errOther := geminiMigrationRule(other)
					if errOther != nil {
						return analysis, errOther
					}
					if strings.EqualFold(otherRule.Name, rule.Name) && !reflect.DeepEqual(geminiModelCapabilities(other), geminiModelCapabilities(row)) {
						unsafeReason = "local_capability_exception"
						break
					}
					if strings.EqualFold(otherRule.Name, rule.Name) && !strings.EqualFold(otherRule.Alias, rule.Alias) && !strings.EqualFold(otherRule.Alias, rule.Name) {
						unsafeReason = "local_alias_exception"
						break
					}
				}
				if unsafeReason != "" {
					unsafeSources[row] = true
					rememberSkipped(rule, unsafeReason)
					continue
				}
				eligible, errEligibility := geminiMigrationRuntimeEligible(group, key, models, rowIndex, rule)
				if errEligibility != nil {
					return analysis, errEligibility
				}
				if !eligible {
					unsafeSources[row] = true
					rememberSkipped(rule, "runtime_alias_exception")
					continue
				}
				ruleKey := geminiRuleKey(rule)
				if byRule[ruleKey] == nil {
					byRule[ruleKey] = &aggregate{rule: rule, keys: make(map[geminiKeyLocation]bool), sources: make(map[*yaml.Node]geminiMigrationSource), sourceKeys: make(map[*yaml.Node]map[geminiKeyLocation]bool), sourceOriginalMissing: make(map[*yaml.Node]bool)}
				}
				entry := byRule[ruleKey]
				originalPresent := false
				for _, other := range models.Content {
					otherRule, _ := geminiMigrationRule(other)
					if strings.EqualFold(otherRule.Alias, rule.Name) {
						originalPresent = true
						break
					}
				}
				entry.originalMissing = entry.originalMissing || !originalPresent
				entry.sourceOriginalMissing[row] = !originalPresent
				entry.keys[geminiKeyLocation{groupIndex, keyIndex}] = true
				if entry.sourceKeys[row] == nil {
					entry.sourceKeys[row] = make(map[geminiKeyLocation]bool)
				}
				entry.sourceKeys[row][geminiKeyLocation{groupIndex, keyIndex}] = true
				entry.sources[row] = geminiMigrationSource{target: target, models: models, row: row, rowIndex: rowIndex}
			}
		}
	}
	globals := make(map[string][]config.OAuthModelAlias)
	if aliases := configV8Node(root, geminiAliasesPath); aliases != nil && aliases.Tag != "!!null" {
		var rules []config.OAuthModelAlias
		if err := aliases.Decode(&rules); err != nil {
			return analysis, err
		}
		for _, rule := range rules {
			alias := strings.ToLower(strings.TrimSpace(rule.Alias))
			globals[alias] = append(globals[alias], rule)
		}
	}
	for ruleKey, entry := range byRule {
		rule := entry.rule
		// A physical group row is shared by all inheriting keys. Keep it local if
		// even one key cannot inherit the proposed global alias after rewriting.
		entry.keys = make(map[geminiKeyLocation]bool)
		entry.originalMissing = false
		for row := range entry.sources {
			if unsafeSources[row] {
				delete(entry.sources, row)
				continue
			}
			entry.originalMissing = entry.originalMissing || entry.sourceOriginalMissing[row]
			for key := range entry.sourceKeys[row] {
				entry.keys[key] = true
			}
		}
		if len(entry.sources) == 0 {
			continue
		}
		if len(byAlias[strings.ToLower(rule.Alias)]) > 1 {
			rememberSkipped(rule, "conflicting_local_alias")
			continue
		}
		if len(entry.keys) < 2 {
			rememberSkipped(rule, "not_repeated")
			continue
		}
		rule.Fork = true
		existing, conflict := false, false
		for _, global := range globals[strings.ToLower(rule.Alias)] {
			if geminiRuleKey(global) != ruleKey {
				conflict = true
				break
			}
			rule.Fork, existing = global.Fork, true
		}
		if conflict {
			rememberSkipped(rule, "conflicting_global_alias")
			continue
		}
		digest := sha256.Sum256([]byte(ruleKey))
		candidate := geminiMigrationCandidate{ID: hex.EncodeToString(digest[:]), Rule: rule,
			Targets: []GeminiKeyTarget{}, AffectedKeys: len(entry.keys), OriginalNameAdded: rule.Fork && entry.originalMissing, existingGlobal: existing}
		owners := make(map[string]bool)
		for _, source := range entry.sources {
			candidate.sources = append(candidate.sources, source)
			owner := fmt.Sprintf("%d/group", source.target.GroupIndex)
			if source.target.KeyIndex != nil {
				owner = fmt.Sprintf("%d/%d", source.target.GroupIndex, *source.target.KeyIndex)
			}
			if !owners[owner] {
				owners[owner] = true
				candidate.Targets = append(candidate.Targets, source.target)
			}
		}
		sortGeminiTargets(candidate.Targets)
		sort.Slice(candidate.sources, func(i, j int) bool {
			a, b := candidate.sources[i], candidate.sources[j]
			if a.target.GroupIndex != b.target.GroupIndex {
				return a.target.GroupIndex < b.target.GroupIndex
			}
			ai, bi := -1, -1
			if a.target.KeyIndex != nil {
				ai = *a.target.KeyIndex
			}
			if b.target.KeyIndex != nil {
				bi = *b.target.KeyIndex
			}
			if ai != bi {
				return ai < bi
			}
			return a.rowIndex < b.rowIndex
		})
		analysis.Candidates = append(analysis.Candidates, candidate)
	}
	sort.Slice(analysis.Candidates, func(i, j int) bool {
		return analysis.Candidates[i].ID < analysis.Candidates[j].ID
	})
	for _, value := range skipped {
		analysis.Skipped = append(analysis.Skipped, value)
	}
	sort.Slice(analysis.Skipped, func(i, j int) bool {
		a, b := analysis.Skipped[i], analysis.Skipped[j]
		return a.Alias+"\x00"+a.Name+"\x00"+a.Reason < b.Alias+"\x00"+b.Name+"\x00"+b.Reason
	})
	return analysis, nil
}

func geminiMigrationRuntimeEligible(group, key, models *yaml.Node, rowIndex int, rule config.OAuthModelAlias) (bool, error) {
	var projected config.GeminiKey
	if err := models.Decode(&projected.Models); err != nil {
		return false, err
	}
	if excluded := geminiEffectiveField(group, key, "excluded-models"); excluded != nil && excluded.Tag != "!!null" {
		if err := excluded.Decode(&projected.ExcludedModels); err != nil {
			return false, err
		}
	}
	// A disabled key may later be enabled without changing its other exclusions.
	filtered := make([]string, 0, len(projected.ExcludedModels))
	for _, pattern := range projected.ExcludedModels {
		if strings.TrimSpace(pattern) != "*" {
			filtered = append(filtered, pattern)
		}
	}
	projected.ExcludedModels = filtered
	projected.Models[rowIndex].Alias = rule.Name
	projected.Models[rowIndex].ForceMapping = false
	rule.Fork = true
	cfg := &config.Config{APIKeyModelAlias: map[string][]config.OAuthModelAlias{"gemini": {rule}}}
	return len(modelconfig.GeminiAPIKeyAliases(cfg, &projected)) == 1, nil
}

func applyGeminiAliasMigration(root *yaml.Node, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, fmt.Errorf("at least one candidateId is required")
	}
	analysis, err := analyzeGeminiAliasMigration(root)
	if err != nil {
		return 0, err
	}
	available := make(map[string]geminiMigrationCandidate, len(analysis.Candidates))
	for _, candidate := range analysis.Candidates {
		available[candidate.ID] = candidate
	}
	selected := make(map[string]bool)
	for _, id := range ids {
		if _, ok := available[id]; !ok {
			return 0, fmt.Errorf("unknown candidateId")
		}
		selected[id] = true
	}
	aliases := configV8Node(root, geminiAliasesPath)
	if aliases == nil || aliases.Tag == "!!null" {
		aliases = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setGeminiPath(root, geminiAliasesPath, aliases)
		aliases = configV8Node(root, geminiAliasesPath)
	}
	affected := make(map[geminiKeyLocation]bool)
	groups := configV8Node(root, geminiGroupsPath)
	for _, candidate := range analysis.Candidates {
		if !selected[candidate.ID] {
			continue
		}
		if !candidate.existingGlobal {
			var rule yaml.Node
			_ = rule.Encode(candidate.Rule)
			aliases.Content = append(aliases.Content, &rule)
		}
		for _, source := range candidate.sources {
			if source.target.KeyIndex != nil {
				affected[geminiKeyLocation{source.target.GroupIndex, *source.target.KeyIndex}] = true
			} else {
				group := groups.Content[source.target.GroupIndex]
				keys := configV8Node(group, []string{"keys"})
				for keyIndex, key := range keys.Content {
					if geminiEffectiveField(group, key, "models") == source.models {
						affected[geminiKeyLocation{source.target.GroupIndex, keyIndex}] = true
					}
				}
			}
			migrateGeminiSourceRow(source)
		}
	}
	return len(affected), nil
}

func migrateGeminiSourceRow(source geminiMigrationSource) {
	rule, _ := geminiMigrationRule(source.row)
	for _, other := range source.models.Content {
		if other == source.row {
			continue
		}
		otherRule, err := geminiMigrationRule(other)
		if err != nil || !strings.EqualFold(otherRule.Name, rule.Name) || !strings.EqualFold(otherRule.Alias, rule.Name) || otherRule.ForceMapping {
			continue
		}
		if !reflect.DeepEqual(geminiModelMetadata(other), geminiModelMetadata(source.row)) {
			continue
		}
		for index, row := range source.models.Content {
			if row == source.row {
				other.HeadComment = strings.TrimSpace(other.HeadComment + "\n" + row.HeadComment)
				other.FootComment = strings.TrimSpace(other.FootComment + "\n" + row.FootComment)
				source.models.Content = append(source.models.Content[:index], source.models.Content[index+1:]...)
				return
			}
		}
	}
	setGeminiMapNode(source.row, "alias", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rule.Name})
	deleteMapKey(source.row, "force-mapping")
}
