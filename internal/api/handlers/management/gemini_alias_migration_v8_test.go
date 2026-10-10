package management

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestGeminiMigrationPreservesGroupInheritanceAndCapabilities(t *testing.T) {
	root := geminiTestRoot(t, geminiManagementFixture)
	analysis, err := analyzeGeminiAliasMigration(root)
	if err != nil || len(analysis.Candidates) != 1 {
		t.Fatalf("expected repeated candidate: %#v %v", analysis, err)
	}
	candidate := analysis.Candidates[0]
	if candidate.AffectedKeys != 3 || len(candidate.Targets) != 2 || !candidate.Rule.Fork || !candidate.OriginalNameAdded {
		t.Fatalf("incorrect candidate impact: %#v", candidate)
	}
	for _, target := range candidate.Targets {
		if target.KeyIndex != nil {
			t.Fatal("inherited group mappings were expanded into keys")
		}
	}
	affected, err := applyGeminiAliasMigration(root, []string{candidate.ID})
	if err != nil || affected != 3 {
		t.Fatalf("apply: %d %v", affected, err)
	}
	groups := configV8Node(root, geminiGroupsPath)
	for index, expected := range []int{32768, 65536} {
		models := configV8Node(groups.Content[index], []string{"models"})
		if len(models.Content) != 1 || yamlMapScalar(models.Content[0], "name") != "source" || yamlMapScalar(models.Content[0], "alias") != "source" {
			t.Fatal("migration widened or emptied model scope")
		}
		var maxContext int
		_ = configV8Node(models.Content[0], []string{"max-context-length"}).Decode(&maxContext)
		if maxContext != expected {
			t.Fatal("source capability metadata was replaced by global metadata")
		}
	}
	keys := configV8Node(groups.Content[0], []string{"keys"})
	if configV8Node(keys.Content[0], []string{"models"}).Tag != "!!null" || configV8Node(keys.Content[1], []string{"models"}) != nil {
		t.Fatal("migration copied inherited models into each key")
	}
	var aliases []config.OAuthModelAlias
	_ = configV8Node(root, geminiAliasesPath).Decode(&aliases)
	if len(aliases) != 1 || aliases[0].Name != "source" || aliases[0].Alias != "client" || !aliases[0].Fork {
		t.Fatalf("global rule incorrect: %#v", aliases)
	}
	analysis, err = analyzeGeminiAliasMigration(root)
	if err != nil || len(analysis.Candidates) != 0 {
		t.Fatal("migration was not idempotent")
	}
}

func TestGeminiMigrationRemovesOnlyEquivalentPassthroughDuplicate(t *testing.T) {
	root := geminiTestRoot(t, `api-keys:
  gemini:
    - models:
        - {name: source, alias: client, force-mapping: true, thinking: {levels: [low, high]}}
        - {name: source, alias: source, thinking: {levels: [low, high]}}
      keys: [{api-key: synthetic-a}, {api-key: synthetic-b}]
`)
	analysis, err := analyzeGeminiAliasMigration(root)
	if err != nil || len(analysis.Candidates) != 1 || analysis.Candidates[0].OriginalNameAdded {
		t.Fatalf("passthrough preview incorrect: %#v %v", analysis, err)
	}
	if _, err = applyGeminiAliasMigration(root, []string{analysis.Candidates[0].ID}); err != nil {
		t.Fatal(err)
	}
	models := configV8Node(root, geminiGroupsPath).Content[0]
	models = configV8Node(models, []string{"models"})
	if len(models.Content) != 1 || configV8Node(models.Content[0], []string{"thinking"}) == nil {
		t.Fatal("duplicate cleanup lost the source model or thinking capabilities")
	}
	var aliases []config.OAuthModelAlias
	_ = configV8Node(root, geminiAliasesPath).Decode(&aliases)
	if !aliases[0].ForceMapping || configV8Node(models.Content[0], []string{"force-mapping"}) != nil {
		t.Fatal("force mapping was not moved into the global rule")
	}
}

func TestGeminiMigrationRetainsLocalExceptions(t *testing.T) {
	root := geminiTestRoot(t, `api-keys:
  gemini:
    - models: [{name: source, alias: client, max-context-length: 32768}]
      keys:
        - {api-key: synthetic-a}
        - {api-key: synthetic-b}
        - api-key: synthetic-c
          models:
            - {name: source, alias: client, max-context-length: 32768}
            - {name: source, alias: source, max-context-length: 65536}
        - api-key: synthetic-d
          models:
            - {name: source, alias: client, max-context-length: 32768}
            - {name: source, alias: local-alternative, max-context-length: 32768}
`)
	group := configV8Node(root, geminiGroupsPath).Content[0]
	keys := configV8Node(group, []string{"keys"})
	capabilityException := geminiTestYAML(t, keys.Content[2])
	aliasException := geminiTestYAML(t, keys.Content[3])
	analysis, err := analyzeGeminiAliasMigration(root)
	if err != nil || len(analysis.Candidates) != 1 || analysis.Candidates[0].AffectedKeys != 2 {
		t.Fatalf("eligible keys incorrect: %#v %v", analysis, err)
	}
	if _, err = applyGeminiAliasMigration(root, []string{analysis.Candidates[0].ID}); err != nil {
		t.Fatal(err)
	}
	if geminiTestYAML(t, keys.Content[2]) != capabilityException || geminiTestYAML(t, keys.Content[3]) != aliasException {
		t.Fatal("migration rewrote local exceptions")
	}
	found := map[string]bool{}
	for _, skipped := range analysis.Skipped {
		found[skipped.Reason] = true
	}
	if !found["local_capability_exception"] || !found["local_alias_exception"] {
		t.Fatalf("missing local exception reasons: %#v", analysis.Skipped)
	}
}

func TestGeminiMigrationDoesNotSelectConflictingAliases(t *testing.T) {
	for _, raw := range []string{
		`api-keys:
  gemini:
    - models: [{name: source, alias: client}]
      keys: [{api-key: synthetic-a}, {api-key: synthetic-b}]
    - models: [{name: other-source, alias: client}]
      keys: [{api-key: synthetic-c}]
`,
		`routing:
  api-key-model-alias:
    gemini: [{name: other-source, alias: client, fork: true}]
api-keys:
  gemini:
    - models: [{name: source, alias: client}]
      keys: [{api-key: synthetic-a}, {api-key: synthetic-b}]
`,
	} {
		root := geminiTestRoot(t, raw)
		analysis, err := analyzeGeminiAliasMigration(root)
		if err != nil || len(analysis.Candidates) != 0 || len(analysis.Skipped) == 0 {
			t.Fatalf("conflicting alias selected: %#v %v", analysis, err)
		}
	}
}

func TestGeminiMigrationExistingGlobalForkIsPreserved(t *testing.T) {
	root := geminiTestRoot(t, `routing:
  api-key-model-alias:
    gemini: [{name: source, alias: client, fork: false}]
api-keys:
  gemini:
    - models: [{name: source, alias: client}]
      keys: [{api-key: synthetic-a}, {api-key: synthetic-b}]
`)
	analysis, err := analyzeGeminiAliasMigration(root)
	if err != nil || len(analysis.Candidates) != 1 || analysis.Candidates[0].Rule.Fork || analysis.Candidates[0].OriginalNameAdded {
		t.Fatalf("existing global options changed: %#v %v", analysis, err)
	}
	if _, err = applyGeminiAliasMigration(root, []string{analysis.Candidates[0].ID}); err != nil {
		t.Fatal(err)
	}
	if len(configV8Node(root, geminiAliasesPath).Content) != 1 {
		t.Fatal("existing global rule was duplicated")
	}
}

func TestGeminiMigrationPrevalidatesAllCandidateIDs(t *testing.T) {
	root := geminiTestRoot(t, geminiManagementFixture)
	before := geminiTestYAML(t, root)
	analysis, err := analyzeGeminiAliasMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = applyGeminiAliasMigration(root, []string{analysis.Candidates[0].ID, "unknown"}); err == nil {
		t.Fatal("unknown candidate accepted")
	}
	if geminiTestYAML(t, root) != before {
		t.Fatal("invalid final candidate partially changed the document")
	}
}

func TestGeminiMigrationPreviewAndApplySaveOnce(t *testing.T) {
	raw := strings.Replace(geminiManagementFixture, "max-context-length: 32768, is-compat: true", "max-context-length: 32768, is-compat: true, future-model-option: {mode: retained}", 1)
	h, router, path := newGeminiManagementTest(t, raw)
	revision := configV8Revision([]byte(raw))
	preview := geminiManagementRequest(t, router, http.MethodPost, "/preview", map[string]any{"revision": revision}, 200)
	var result struct {
		Revision   string
		Candidates []geminiMigrationCandidate
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &result); err != nil || len(result.Candidates) != 1 {
		t.Fatalf("preview contract failed: %s %v", preview.Body.String(), err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != raw || h.reloadGeneration != 0 {
		t.Fatal("preview saved configuration")
	}
	geminiManagementRequest(t, router, http.MethodPost, "/apply", map[string]any{"revision": "stale", "candidateIds": []string{result.Candidates[0].ID}}, 409)
	geminiManagementRequest(t, router, http.MethodPost, "/apply", map[string]any{"revision": revision, "candidateIds": []string{"unknown"}}, 400)
	if h.reloadGeneration != 0 {
		t.Fatal("failed apply scheduled a reload")
	}
	geminiManagementRequest(t, router, http.MethodPost, "/apply", map[string]any{"revision": revision, "candidateIds": []string{result.Candidates[0].ID}}, 200)
	loaded, err := config.LoadConfig(path)
	if err != nil || len(loaded.GeminiKey) != 3 || loaded.GeminiKey[0].Models[0].Alias != "source" || loaded.GeminiKey[2].Models[0].MaxContextLength != 65536 || h.reloadGeneration != 1 {
		t.Fatalf("migration not persisted correctly: %v", err)
	}
	saved, _ := os.ReadFile(path)
	savedRoot := geminiTestRoot(t, string(saved))
	if len(configV8Node(savedRoot, geminiAliasesPath).Content) != 1 || !strings.Contains(string(saved), "models: null") || !strings.Contains(string(saved), "# Keep source comments") {
		t.Fatal("migration save lost global rule, source comments or inheritance")
	}
	if !strings.Contains(string(saved), "opaque-option: retained") || !strings.Contains(string(saved), "future-model-option:") || !strings.Contains(string(saved), "mode: retained") {
		t.Fatal("migration save discarded opaque source/key metadata")
	}
}

func TestGeminiMigrationRuntimeExceptionsProtectSharedRows(t *testing.T) {
	for _, fixture := range []struct{ name, models, keys string }{
		{"source exclusion", "[{name: source, alias: client}]", "[{api-key: synthetic-a, excluded-models: [source]}, {api-key: synthetic-b}]"},
		{"thinking suffix precedence", "[{name: 'source(low)', alias: client}, {name: 'source(high)', alias: other-local}]", "[{api-key: synthetic-a}, {api-key: synthetic-b}]"},
		{"local upstream-name collision", "[{name: source, alias: client}, {name: client, alias: other-local}]", "[{api-key: synthetic-a}, {api-key: synthetic-b}]"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source := "source"
			if fixture.name == "thinking suffix precedence" {
				source = "source(low)"
			}
			raw := "api-keys:\n  gemini:\n    - models: " + fixture.models + "\n      keys: " + fixture.keys + "\n    - models: [{name: '" + source + "', alias: client}]\n      keys: [{api-key: synthetic-c}, {api-key: synthetic-d}]\n"
			root := geminiTestRoot(t, raw)
			groups := configV8Node(root, geminiGroupsPath)
			protected := geminiTestYAML(t, groups.Content[0])
			analysis, err := analyzeGeminiAliasMigration(root)
			var selected *geminiMigrationCandidate
			for index := range analysis.Candidates {
				if analysis.Candidates[index].Rule.Alias == "client" {
					selected = &analysis.Candidates[index]
				}
			}
			if err != nil || selected == nil || selected.AffectedKeys != 2 || len(selected.Targets) != 1 || selected.Targets[0].GroupIndex != 1 {
				t.Fatalf("unsafe physical rows selected: %#v %v", analysis, err)
			}
			found := false
			for _, skipped := range analysis.Skipped {
				found = found || skipped.Reason == "runtime_alias_exception"
			}
			if !found {
				t.Fatal("runtime exception was not reported")
			}
			if _, err = applyGeminiAliasMigration(root, []string{selected.ID}); err != nil {
				t.Fatal(err)
			}
			if geminiTestYAML(t, groups.Content[0]) != protected {
				t.Fatal("shared group row with runtime exception was rewritten")
			}
		})
	}
}
