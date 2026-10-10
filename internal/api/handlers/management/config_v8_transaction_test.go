package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func newConfigV8TransactionTest(t *testing.T) (*Handler, *gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "# Configuration comment\nconfig-version: 8\nserver: {port: 8317}\napi-keys:\n  gemini:\n    - name: example\n      models: [{name: gemini-source, alias: gemini-source}]\n      keys:\n        - api-key: synthetic-test-key\n          models: null\n          headers: {}\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.GET("/config", h.ConfigV8)
	router.PATCH("/config", h.ConfigV8)
	return h, router, path
}

func TestConfigV8IfMatchPreventsLostUpdate(t *testing.T) {
	h, router, path := newConfigV8TransactionTest(t)
	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/config", nil))
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstTag := get.Header().Get("ETag")
	if get.Code != http.StatusOK || firstTag != `"`+configV8Revision(initial)+`"` {
		t.Fatalf("invalid GET revision: %d %q", get.Code, firstTag)
	}
	patch := func(tag, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(body))
		request.Header.Set("If-Match", tag)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	response := patch(firstTag, `{"server":{"port":8318}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("first patch failed: %d %s", response.Code, response.Body.String())
	}
	saved, _ := os.ReadFile(path)
	var result struct{ Revision string }
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision != configV8Revision(saved) || response.Header().Get("ETag") != `"`+result.Revision+`"` {
		t.Fatal("successful write returned a revision different from the persisted bytes")
	}
	if !strings.Contains(string(saved), "models: null") || !strings.Contains(string(saved), "headers: {}") || !strings.Contains(string(saved), "# Configuration comment") {
		t.Fatal("canonical transaction lost explicit inheritance or comments")
	}
	h.mu.Lock()
	generation := h.reloadGeneration
	h.mu.Unlock()
	response = patch(firstTag, `{"server":{"port":8319}}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "config_conflict") {
		t.Fatalf("stale patch was accepted: %d %s", response.Code, response.Body.String())
	}
	after, _ := os.ReadFile(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	if string(after) != string(saved) || h.cfg.Port != 8318 || h.reloadGeneration != generation {
		t.Fatal("stale mutation changed disk, runtime, or reload generation")
	}
}

func TestConfigV8CommitDetectsExternalEdit(t *testing.T) {
	h, _, path := newConfigV8TransactionTest(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		t.Fatal(err)
	}
	configV8Node(state.Root, []string{"server", "port"}).Value = "8319"
	external := append(append([]byte{}, state.Data...), []byte("# External edit\n")...)
	if err = os.WriteFile(path, external, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = h.commitConfigV8Locked(context.Background(), state); err == nil {
		t.Fatal("external edit did not invalidate the transaction")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(external) || h.cfg.Port != 8317 || h.reloadGeneration != 0 {
		t.Fatal("conflicted commit changed persisted or runtime state")
	}
}

func TestConfigV8CommitSaveFailureKeepsRuntime(t *testing.T) {
	h, _, path := newConfigV8TransactionTest(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		t.Fatal(err)
	}
	configV8Node(state.Root, []string{"server", "port"}).Value = "8319"
	// A portable read-only file fails the mounted-inode write without truncation.
	if err = os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0600) }()
	_, err = h.commitConfigV8Locked(context.Background(), state)
	if err == nil {
		t.Skip("current filesystem or user permits writing a read-only file")
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if configV8Revision(after) != state.Revision || h.cfg.Port != 8317 || h.reloadGeneration != 0 {
		t.Fatal("failed save advanced runtime state or reload generation")
	}
}

func TestConfigV8RevisionMatchesStrongTags(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{{"abc", true}, {`"abc"`, true}, {`"old", "abc"`, true}, {"*", true}, {`W/"abc"`, false}, {"", false}, {"old", false}} {
		if got := configV8RevisionMatches(test.value, "abc"); got != test.want {
			t.Errorf("match %q = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestConfigV8PreservesExistingExtensionsAndRejectsNewOnes(t *testing.T) {
	h, router, path := newConfigV8TransactionTest(t)
	raw, _ := os.ReadFile(path)
	raw = []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), "alias: gemini-source}", "alias: gemini-source, future-model-option: {enabled: true}}"), "models: null", "models: null\n          future-key-option: {mode: existing}"))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	patch := func(body string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(body)))
		if response.Code != want {
			t.Fatalf("patch status=%d body=%s", response.Code, response.Body.String())
		}
	}
	patch(`{"server":{"port":8318}}`, http.StatusOK)
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), "future-key-option:") || !strings.Contains(string(saved), "future-model-option:") {
		t.Fatal("validated save discarded preexisting opaque fields")
	}
	patch(`{"api-keys":{"gemini":[{"name":"example","keys":[{"api-key":"synthetic-test-key","future-key-option":{"mode":"changed"}}]}]}}`, http.StatusBadRequest)
	patch(`{"api-keys":{"gemini":[{"name":"example","keys":[{"api-key":"synthetic-test-key","new-option":true}]}]}}`, http.StatusBadRequest)
	patch(`{"api-keys":{"gemini":[{"name":"example","keys":[{"api-key":"synthetic-test-key","future-key-option":{"mode":"existing"}},{"api-key":"another-synthetic-key","future-key-option":{"mode":"existing"}}]}]}}`, http.StatusBadRequest)
	after, _ := os.ReadFile(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	if string(after) != string(saved) || h.cfg.Port != 8318 || h.reloadGeneration != 1 {
		t.Fatal("rejected extension changes modified persisted or runtime configuration")
	}
}

func TestConfigV8ExtensionsFollowUnnamedEmptyGroupAfterDeletion(t *testing.T) {
	h, _, path := newConfigV8TransactionTest(t)
	raw := "config-version: 8\nserver: {port: 8317}\napi-keys:\n  gemini:\n    - models: [{name: source, alias: source, opaque: one}]\n      keys: [{api-key: first}]\n    - models: [{name: source, alias: source, opaque: two}]\n      keys: []\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		t.Fatal(err)
	}
	groups := configV8Node(state.Root, []string{"api-keys", "gemini"})
	groups.Content = groups.Content[1:]
	if _, err = h.commitConfigV8Locked(context.Background(), state); err != nil {
		t.Fatalf("unchanged empty group could not follow its original ownership: %v", err)
	}
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), "opaque: two") || strings.Contains(string(saved), "opaque: one") {
		t.Fatal("group deletion did not preserve the surviving opaque options")
	}
}

func TestConfigV8ExtensionsFollowDuplicateGroupsAfterReordering(t *testing.T) {
	h, _, path := newConfigV8TransactionTest(t)
	raw := "config-version: 8\nserver: {port: 8317}\napi-keys:\n  gemini:\n    - name: duplicate\n      models: [{name: source, alias: source, opaque: one}]\n      keys: [{api-key: duplicate}]\n    - name: duplicate\n      models: [{name: source, alias: source, opaque: two}]\n      keys: [{api-key: duplicate}]\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		t.Fatal(err)
	}
	groups := configV8Node(state.Root, []string{"api-keys", "gemini"})
	groups.Content[0], groups.Content[1] = groups.Content[1], groups.Content[0]
	if _, err = h.commitConfigV8Locked(context.Background(), state); err != nil {
		t.Fatalf("duplicate groups could not retain opaque model options: %v", err)
	}
	saved, _ := os.ReadFile(path)
	if strings.Index(string(saved), "opaque: two") > strings.Index(string(saved), "opaque: one") {
		t.Fatal("persisted groups did not follow the requested order")
	}
}
