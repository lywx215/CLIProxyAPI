package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestManagementV8GeminiLargeSnapshotAndMutationContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, size := range []int{100, 1000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var fixture strings.Builder
			fixture.WriteString("# Synthetic integration fixture\nconfig-version: 8\nserver: {port: 8317}\nmanagement: {secret-key: synthetic-management}\nplugins: {configs: {fixture: {opaque: {preserve: true}}}}\napi-keys:\n  gemini:\n")
			for group := 0; group < 10; group++ {
				fmt.Fprintf(&fixture, "    - name: group-%d\n      headers: {X-Fixture: inherited}\n      models: [{name: gemini-2.5-flash, alias: shared-fixture-model}]\n      keys:\n", group)
				for key := 0; key < size/10; key++ {
					fmt.Fprintf(&fixture, "        - api-key: synthetic-%d-%d\n          models: null\n", group, key)
				}
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(fixture.String()), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			h := management.NewHandler(cfg, path, nil)
			h.SetLocalPassword("synthetic-management")
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(true)
			s.registerManagementV8Routes()
			request := func(method, route string, value any, want int) map[string]any {
				t.Helper()
				body, errJSON := json.Marshal(value)
				if errJSON != nil {
					t.Fatal(errJSON)
				}
				req := httptest.NewRequest(method, "/v8/management/credentials/api-keys/gemini"+route, strings.NewReader(string(body)))
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Authorization", "Bearer synthetic-management")
				recorder := httptest.NewRecorder()
				s.engine.ServeHTTP(recorder, req)
				if recorder.Code != want {
					t.Fatalf("%s %s: status=%d body=%s", method, route, recorder.Code, recorder.Body.String())
				}
				var result map[string]any
				if errJSON = json.Unmarshal(recorder.Body.Bytes(), &result); errJSON != nil {
					t.Fatal(errJSON)
				}
				return result
			}
			snapshot := request(http.MethodGet, "/snapshot", nil, http.StatusOK)
			groups := snapshot["groups"].([]any)
			if len(groups) != 10 || len(groups[9].(map[string]any)["keys"].([]any)) != size/10 {
				t.Fatal("snapshot lost canonical group/key positions")
			}
			preview := request(http.MethodPost, "/alias-migration/preview", map[string]any{"revision": snapshot["revision"]}, http.StatusOK)
			candidates := preview["candidates"].([]any)
			if len(candidates) != 1 || int(candidates[0].(map[string]any)["affectedKeys"].(float64)) != size {
				t.Fatalf("expected one shared migration covering %d keys", size)
			}
			applied := request(http.MethodPost, "/alias-migration/apply", map[string]any{
				"revision": snapshot["revision"], "candidateIds": []any{candidates[0].(map[string]any)["id"]},
			}, http.StatusOK)
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			request(http.MethodPost, "/batch", map[string]any{
				"revision": snapshot["revision"], "action": "delete", "targets": []any{map[string]any{"groupIndex": 9, "keyIndex": size/10 - 1}},
			}, http.StatusConflict)
			afterConflict, _ := os.ReadFile(path)
			if string(saved) != string(afterConflict) {
				t.Fatal("stale batch changed configuration")
			}
			updated := request(http.MethodPost, "/batch", map[string]any{
				"revision": applied["revision"], "action": "disable", "targets": []any{map[string]any{"groupIndex": 9, "keyIndex": size/10 - 1}},
			}, http.StatusOK)
			if updated["affected"] != float64(1) {
				t.Fatal("batch affected keys outside the selected source")
			}
			saved, _ = os.ReadFile(path)
			sum := sha256.Sum256(saved)
			if updated["revision"] != hex.EncodeToString(sum[:]) {
				t.Fatal("mutation response revision does not identify persisted bytes")
			}
			restarted, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(restarted.GeminiKey) != size || len(restarted.APIKeyModelAlias["gemini"]) != 1 {
				t.Fatal("restart lost keys or the unified alias")
			}
			if got := restarted.GeminiKey[size-1].ExcludedModels; len(got) != 1 || got[0] != "*" {
				t.Fatal("restart lost selected key disabled state")
			}
			for _, key := range restarted.GeminiKey[:size-1] {
				if len(key.ExcludedModels) != 0 {
					t.Fatal("batch disabled an unselected key")
				}
			}
			if strings.Count(string(saved), "models: null") != size || !strings.Contains(string(saved), "opaque:") || !strings.Contains(string(saved), "# Synthetic integration fixture") {
				t.Fatal("migration or batch lost inheritance, opaque settings, or comments")
			}
		})
	}
}
