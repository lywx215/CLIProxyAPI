package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

func geminiTestRoot(t *testing.T, raw string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || len(doc.Content) == 0 {
		t.Fatalf("invalid test document: %v", err)
	}
	return doc.Content[0]
}

func geminiTestYAML(t *testing.T, node *yaml.Node) string {
	t.Helper()
	raw, err := yaml.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func newGeminiManagementTest(t *testing.T, raw string) (*Handler, *gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.GET("/snapshot", h.GeminiAPIKeysSnapshot)
	router.POST("/batch", h.GeminiAPIKeysBatch)
	router.POST("/preview", h.GeminiAliasMigrationPreview)
	router.POST("/apply", h.GeminiAliasMigrationApply)
	return h, router, path
}

func geminiManagementRequest(t *testing.T, router *gin.Engine, method, path string, body any, expected int) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(string(raw))))
	if response.Code != expected {
		t.Fatalf("%s %s returned %d: %s", method, path, response.Code, response.Body.String())
	}
	return response
}

const geminiManagementFixture = `# Keep source comments
config-version: 8
server: {port: 8317}
api-keys:
  gemini:
    - name: primary
      base-url: https://synthetic.invalid
      excluded-models: [blocked-model]
      models: [{name: source, alias: client, max-context-length: 32768, is-compat: true}]
      keys:
        - api-key: synthetic-a
          models: null
          headers: {}
          opaque-option: retained
        - api-key: synthetic-b
          excluded-models: [key-blocked]
          weight: 2
    - name: secondary
      models: [{name: source, alias: client, max-context-length: 65536}]
      keys: [{api-key: synthetic-c}]
`

func TestGeminiSnapshotDoesNotPersistAndPreservesInheritance(t *testing.T) {
	h, router, path := newGeminiManagementTest(t, geminiManagementFixture)
	response := geminiManagementRequest(t, router, http.MethodGet, "/snapshot", nil, http.StatusOK)
	var result struct {
		Revision     string
		Groups       []map[string]any
		Aliases      []any
		Capabilities map[string]bool
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision != configV8Revision([]byte(geminiManagementFixture)) || len(result.Groups) != 2 || result.Aliases == nil || !result.Capabilities["batch"] {
		t.Fatalf("unexpected snapshot: %s", response.Body.String())
	}
	key := result.Groups[0]["keys"].([]any)[0].(map[string]any)
	if key["models"] != nil || !reflect.DeepEqual(key["headers"], map[string]any{}) || key["opaque-option"] != "retained" {
		t.Fatalf("snapshot flattened or discarded inheritance: %#v", key)
	}
	after, _ := os.ReadFile(path)
	if string(after) != geminiManagementFixture || h.reloadGeneration != 0 {
		t.Fatal("snapshot modified persisted config")
	}
}

func TestGeminiBatchValidatesBeforeSaving(t *testing.T) {
	h, router, path := newGeminiManagementTest(t, geminiManagementFixture)
	revision := configV8Revision([]byte(geminiManagementFixture))
	for _, test := range []struct {
		body   any
		status int
	}{
		{map[string]any{"revision": "stale", "action": "delete", "targets": []any{map[string]any{"groupIndex": 0}}}, 409},
		{map[string]any{"revision": "*", "action": "delete", "targets": []any{map[string]any{"groupIndex": 0}}}, 409},
		{map[string]any{"revision": revision, "action": "delete", "targets": []any{map[string]any{}}}, 400},
		{map[string]any{"revision": revision, "action": "delete", "targets": []any{map[string]any{"groupIndex": 0, "keyIndex": 0}, map[string]any{"groupIndex": 999}}}, 400},
		{map[string]any{"revision": revision, "action": "patch", "targets": []any{map[string]any{"groupIndex": 0, "keyIndex": 0}}, "changes": map[string]any{"base-url": "https://invalid.invalid"}}, 400},
		{map[string]any{"revision": revision, "action": "patch", "targets": []any{map[string]any{"groupIndex": 0}}, "changes": map[string]any{"weight": 2}}, 400},
		{map[string]any{"revision": revision, "action": "patch", "targets": []any{map[string]any{"groupIndex": 0, "keyIndex": 0}}, "changes": map[string]any{"models": []any{}}}, 400},
		{map[string]any{"revision": revision, "action": "patch", "targets": []any{map[string]any{"groupIndex": 0, "keyIndex": 0}}, "changes": map[string]any{"headers": map[string]any{"x-test": 1}}}, 400},
		{map[string]any{"revision": revision, "action": "patch", "targets": []any{map[string]any{"groupIndex": 0, "keyIndex": 0}}, "changes": map[string]any{"weight": 1000001}}, 400},
		{map[string]any{"action": "delete", "targets": []any{map[string]any{"groupIndex": 0}}}, 400},
	} {
		geminiManagementRequest(t, router, http.MethodPost, "/batch", test.body, test.status)
		after, _ := os.ReadFile(path)
		if string(after) != geminiManagementFixture || h.reloadGeneration != 0 || len(h.cfg.GeminiKey) != 3 {
			t.Fatalf("rejected request modified state: %#v", test.body)
		}
	}
}

func TestGeminiBatchDisablesAndEnablesEffectiveExclusions(t *testing.T) {
	root := geminiTestRoot(t, geminiManagementFixture)
	request := geminiBatchRequest{Action: "disable", Targets: []GeminiKeyTarget{{GroupIndex: 0}}}
	affected, err := applyGeminiBatch(root, request)
	if err != nil || affected != 2 {
		t.Fatalf("disable: affected %d, error %v", affected, err)
	}
	groups := configV8Node(root, geminiGroupsPath)
	keys := configV8Node(groups.Content[0], []string{"keys"})
	for keyIndex, expected := range [][]string{{"blocked-model", "*"}, {"key-blocked", "*"}} {
		var result []string
		_ = configV8Node(keys.Content[keyIndex], []string{"excluded-models"}).Decode(&result)
		if !reflect.DeepEqual(result, expected) {
			t.Fatalf("key %d exclusions %#v, expected %#v", keyIndex, result, expected)
		}
	}
	request.Action = "enable"
	if _, err = applyGeminiBatch(root, request); err != nil {
		t.Fatal(err)
	}
	for keyIndex, expected := range [][]string{{"blocked-model"}, {"key-blocked"}} {
		var result []string
		_ = configV8Node(keys.Content[keyIndex], []string{"excluded-models"}).Decode(&result)
		if !reflect.DeepEqual(result, expected) {
			t.Fatalf("enable removed other exclusions: %#v", result)
		}
	}
	if configV8Node(keys.Content[0], []string{"models"}).Tag != "!!null" {
		t.Fatal("toggle copied inherited models into key")
	}
}

func TestGeminiBatchDeleteUsesOriginalCoordinatesAndDeduplicates(t *testing.T) {
	root := geminiTestRoot(t, geminiManagementFixture)
	zero, one := 0, 1
	affected, err := applyGeminiBatch(root, geminiBatchRequest{Action: "delete", Targets: []GeminiKeyTarget{
		{GroupIndex: 0, KeyIndex: &zero}, {GroupIndex: 0, KeyIndex: &one}, {GroupIndex: 0, KeyIndex: &zero},
		{GroupIndex: 1}, {GroupIndex: 1, KeyIndex: &zero},
	}})
	if err != nil || affected != 3 {
		t.Fatalf("delete: %d %v", affected, err)
	}
	groups := configV8Node(root, geminiGroupsPath)
	if len(groups.Content) != 1 || len(configV8Node(groups.Content[0], []string{"keys"}).Content) != 0 {
		t.Fatal("delete indexes shifted or overlap deleted wrong groups")
	}
}

func TestGeminiBatchCanonicalSavePreservesRawFieldsAndReloadsOnce(t *testing.T) {
	h, router, path := newGeminiManagementTest(t, geminiManagementFixture)
	body := map[string]any{"revision": configV8Revision([]byte(geminiManagementFixture)), "action": "patch",
		"targets": []any{map[string]any{"groupIndex": 0, "keyIndex": 0}, map[string]any{"groupIndex": 0, "keyIndex": 1}},
		"changes": map[string]any{"priority": 7, "proxy-url": nil, "weight": nil, "headers": map[string]string{}}}
	response := geminiManagementRequest(t, router, http.MethodPost, "/batch", body, 200)
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), "models: null") || !strings.Contains(string(saved), "headers: {}") || !strings.Contains(string(saved), "opaque-option: retained") || !strings.Contains(string(saved), "# Keep source comments") {
		t.Fatalf("canonical batch lost raw values: %s", saved)
	}
	if h.reloadGeneration != 1 || h.cfg.GeminiKey[0].Priority != 7 || h.cfg.GeminiKey[1].Weight != nil {
		t.Fatal("batch did not update runtime exactly once")
	}
	var result struct {
		Revision string
		Affected int
	}
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	if result.Revision != configV8Revision(saved) || result.Affected != 2 {
		t.Fatal("response revision or affected count is incorrect")
	}
	restarted, err := config.LoadConfig(path)
	if err != nil || restarted.GeminiKey[1].Priority != 7 || restarted.GeminiKey[1].Weight != nil {
		t.Fatalf("saved batch did not survive reload: %v", err)
	}
	geminiManagementRequest(t, router, http.MethodPost, "/batch", body, 409)
	if h.reloadGeneration != 1 {
		t.Fatal("stale retry scheduled another reload")
	}
}

func TestGeminiBatchLargeSyntheticCollections(t *testing.T) {
	for _, count := range []int{100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var raw strings.Builder
			raw.WriteString("config-version: 8\nserver: {port: 8317}\napi-keys:\n  gemini:\n    - name: synthetic\n      models: [{name: source, alias: client}]\n      keys:\n")
			targets := make([]any, 0, count)
			for index := range count {
				fmt.Fprintf(&raw, "        - api-key: synthetic-%d\n          models: null\n          headers: {}\n", index)
				targets = append(targets, map[string]any{"groupIndex": 0, "keyIndex": index})
			}
			h, router, path := newGeminiManagementTest(t, raw.String())
			response := geminiManagementRequest(t, router, http.MethodPost, "/batch", map[string]any{
				"revision": configV8Revision([]byte(raw.String())), "action": "patch", "targets": targets, "changes": map[string]any{"priority": 12},
			}, 200)
			var result struct{ Affected int }
			_ = json.Unmarshal(response.Body.Bytes(), &result)
			loaded, err := config.LoadConfig(path)
			if err != nil || result.Affected != count || len(loaded.GeminiKey) != count || h.reloadGeneration != 1 {
				t.Fatalf("large batch invalid: count=%d, affected=%d, reloads=%d, error=%v", count, result.Affected, h.reloadGeneration, err)
			}
			for _, key := range loaded.GeminiKey {
				if key.Priority != 12 || len(key.Models) != 1 {
					t.Fatal("large batch lost inherited model scope")
				}
			}
		})
	}
}

func TestGeminiBatchGroupPatchKeepsKeyOverridesAndScope(t *testing.T) {
	root := geminiTestRoot(t, geminiManagementFixture)
	keys := configV8Node(configV8Node(root, geminiGroupsPath).Content[0], []string{"keys"})
	beforeKey := geminiTestYAML(t, keys.Content[0])
	changes := map[string]json.RawMessage{"base-url": json.RawMessage(`"https://updated.synthetic.invalid"`), "priority": json.RawMessage(`4`)}
	affected, err := applyGeminiBatch(root, geminiBatchRequest{Action: "patch", Targets: []GeminiKeyTarget{{GroupIndex: 0}}, Changes: changes})
	if err != nil || affected != 2 {
		t.Fatalf("group patch: affected %d, error %v", affected, err)
	}
	group := configV8Node(root, geminiGroupsPath).Content[0]
	if yamlMapScalar(group, "base-url") != "https://updated.synthetic.invalid" || geminiTestYAML(t, keys.Content[0]) != beforeKey {
		t.Fatal("group base URL was copied into key fields")
	}
}

func TestGeminiBatchSaveFailureDoesNotAdvanceRuntime(t *testing.T) {
	raw := strings.ReplaceAll(geminiManagementFixture, "          opaque-option: retained\n", "")
	h, router, path := newGeminiManagementTest(t, raw)
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0600) }()
	body, _ := json.Marshal(map[string]any{"revision": configV8Revision([]byte(raw)), "action": "disable", "targets": []any{map[string]any{"groupIndex": 0}}})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/batch", strings.NewReader(string(body))))
	if response.Code == http.StatusOK {
		t.Skip("current filesystem or user permits writing a read-only file")
	}
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "write_failed") {
		t.Fatalf("unexpected save failure: %d %s", response.Code, response.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(after) != raw || h.reloadGeneration != 0 || len(h.cfg.GeminiKey[0].ExcludedModels) != 1 || h.cfg.GeminiKey[0].ExcludedModels[0] != "blocked-model" {
		t.Fatal("failed batch save advanced disk, runtime, or hot reload")
	}
}
