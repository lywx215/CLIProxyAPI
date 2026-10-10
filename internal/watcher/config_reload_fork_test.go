package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestV8ForkConfigChangeReachesReloadCallback(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	authDir := filepath.Join(dir, "auths")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	var snapshots []*config.Config
	w := &Watcher{
		configPath:     file,
		authDir:        authDir,
		lastAuthHashes: make(map[string]string),
		reloadCallback: func(cfg *config.Config) { snapshots = append(snapshots, cfg) },
	}
	w.SetConfig(&config.Config{AuthDir: authDir, CredentialInFlight: config.DefaultCredentialInFlightConfig()})
	write := func(enabled bool, rpm, tokens, concurrent, queue int) {
		t.Helper()
		raw := fmt.Sprintf(`config-version: 8
oauth:
  auth-dir: %q
  providers:
    antigravity: {antigravity-credits-force: %t}
access:
  api-key-rate-limit: {enabled: %t, default-rpm: %d, overrides: [{api-key: synthetic-key, rpm: 0}]}
server: {enable-gemini-cli-endpoint: %t}
requests:
  speed-throttle: {enabled: %t, min-tokens-per-second: %d, max-tokens-per-second: %d, max-concurrent: %d, queue-size: %d}
upstream:
  codex: {identity-confuse: %t}
`, filepath.ToSlash(authDir), enabled, enabled, rpm, enabled, enabled, tokens, tokens, concurrent, queue, enabled)
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(true, 10, 20, 3, 5)
	w.reloadConfigIfChanged()
	w.reloadConfigIfChanged()
	if len(snapshots) != 1 {
		t.Fatalf("unchanged v8 configuration retriggered reload: calls=%d", len(snapshots))
	}
	first := snapshots[0]
	if !first.EnableGeminiCLIEndpoint || !first.APIKeyRateLimit.Enabled || first.APIKeyRateLimit.DefaultRPM != 10 || first.APIKeyRateLimit.Overrides[0].RPM != 0 || !first.SpeedThrottle.Enabled || first.SpeedThrottle.MinTokensPerSecond != 20 || first.SpeedThrottle.MaxConcurrent != 3 || first.SpeedThrottle.QueueSize != 5 || !first.Codex.IdentityConfuse || !first.QuotaExceeded.AntigravityCreditsForce {
		t.Fatal("initial fork configuration was lost before the callback")
	}
	write(false, 0, 30, 0, 0)
	w.reloadConfigIfChanged()
	if len(snapshots) != 2 {
		t.Fatalf("fork-only v8 changes did not trigger reload: calls=%d", len(snapshots))
	}
	final := snapshots[1]
	if final.EnableGeminiCLIEndpoint || final.APIKeyRateLimit.Enabled || final.APIKeyRateLimit.DefaultRPM != 0 || final.SpeedThrottle.Enabled || final.SpeedThrottle.MinTokensPerSecond != 30 || final.SpeedThrottle.MaxConcurrent != 0 || final.SpeedThrottle.QueueSize != 0 || final.Codex.IdentityConfuse || final.QuotaExceeded.AntigravityCreditsForce {
		t.Fatal("fork false/zero values were lost during reload")
	}
	if first == final || !first.SpeedThrottle.Enabled || first.SpeedThrottle.MinTokensPerSecond != 20 {
		t.Fatal("reload mutated the previous configuration snapshot")
	}
}
