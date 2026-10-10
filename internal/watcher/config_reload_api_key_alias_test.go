package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestGlobalAPIKeyAliasesReloadUnchangedCredentials(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.yaml")
	var snapshots []*config.Config
	w := &Watcher{
		configPath: file, authDir: authDir, lastAuthHashes: make(map[string]string),
		reloadCallback: func(cfg *config.Config) { snapshots = append(snapshots, cfg) },
	}
	w.SetConfig(&config.Config{AuthDir: authDir, CredentialInFlight: config.DefaultCredentialInFlightConfig()})
	write := func(alias string) {
		t.Helper()
		rule := "[]"
		if alias != "" {
			rule = fmt.Sprintf("[{name: source, alias: %s, fork: true}]", alias)
		}
		raw := fmt.Sprintf("config-version: 8\noauth: {auth-dir: %q}\nrouting: {api-key-model-alias: {gemini: %s}}\napi-keys: {gemini: [{name: group, models: [{name: source, alias: source}], keys: [{api-key: synthetic}]}]}\n", filepath.ToSlash(authDir), rule)
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("public")
	w.reloadConfigIfChanged()
	if len(w.currentAuths) != 1 || len(snapshots) != 1 {
		t.Fatal("initial global aliases did not load")
	}
	var authID string
	for id := range w.currentAuths {
		authID = id
	}
	initialRevision := w.authRevisions[authID]
	w.reloadConfigIfChanged()
	if w.authRevisions[authID] != initialRevision || len(snapshots) != 1 {
		t.Fatal("unchanged file retriggered credential registration")
	}
	write("updated")
	w.reloadConfigIfChanged()
	if w.authRevisions[authID] != initialRevision+1 || len(snapshots) != 2 || snapshots[1].APIKeyModelAlias["gemini"][0].Alias != "updated" {
		t.Fatal("alias-only change did not refresh unchanged credential")
	}
	if snapshots[0].APIKeyModelAlias["gemini"][0].Alias != "public" || len(snapshots[1].GeminiKey[0].Models) != 1 {
		t.Fatal("reload changed old snapshot or expanded the credential allowlist")
	}
	write("")
	w.reloadConfigIfChanged()
	if w.authRevisions[authID] != initialRevision+2 || len(snapshots) != 3 || len(snapshots[2].APIKeyModelAlias) != 0 {
		t.Fatal("removing the last global alias did not refresh credential")
	}
}
