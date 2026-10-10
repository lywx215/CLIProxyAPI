package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestV8ForkMigrationKeepsEffectiveConfiguration(t *testing.T) {
	raw := []byte(`api-key-rate-limit: {enabled: true, default-rpm: 60, overrides: [{api-key: unlimited, rpm: 0}]}
speed-throttle: {enabled: true, min-tokens-per-second: 20, max-tokens-per-second: 30, min-first-token-delay-ms: 100, max-first-token-delay-ms: 200, max-concurrent: 3, queue-size: 5}
enable-gemini-cli-endpoint: true
quota-exceeded: {antigravity-credits-force: true, antigravity-credits: true}
codex: {identity-confuse: true, response-steering: true}
claude-api-key: [{api-key: mock-key, fingerprint-profile: claude-code-cli}]
unknown-extension: {nested: [one, {two: 2}]}
`)
	before, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, err := NormalizeConfigLayout(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(canonical); err != nil {
		t.Fatalf("fork migration is not valid v8: %v\n%s", err, canonical)
	}
	after, err := ParseConfigBytes(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if after.APIKeyRateLimit.DefaultRPM != before.APIKeyRateLimit.DefaultRPM || !after.APIKeyRateLimit.Enabled || len(after.APIKeyRateLimit.Overrides) != 1 || after.APIKeyRateLimit.Overrides[0].RPM != 0 || after.SpeedThrottle != before.SpeedThrottle || !after.EnableGeminiCLIEndpoint || !after.QuotaExceeded.AntigravityCreditsForce || !after.QuotaExceeded.AntigravityCredits || !after.Codex.IdentityConfuse || !after.Codex.ResponseSteering || len(after.ClaudeKey) != 1 || after.ClaudeKey[0].FingerprintProfile != "claude-code-cli" {
		t.Fatalf("migration changed fork behavior: %#v", after)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(canonical, &doc); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"access.api-key-rate-limit", "requests.speed-throttle", "server.enable-gemini-cli-endpoint", "oauth.providers.antigravity.antigravity-credits-force", "upstream.codex.identity-confuse"} {
		if yamlPath(doc.Content[0], path) == nil {
			t.Fatalf("missing canonical fork path %s", path)
		}
	}
	if yamlPath(doc.Content[0], "unknown-extension") != nil || !strings.Contains(string(canonical), "# unknown-extension:") || !strings.Contains(string(canonical), "two: 2") {
		t.Fatalf("unknown data was not archived without activation: %s", canonical)
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(file, canonical, 0600); err != nil {
		t.Fatal(err)
	}
	after.SpeedThrottle.Enabled = false
	after.Codex.IdentityConfuse = false
	if err = SaveConfigPreserveComments(file, after, false); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(saved); err != nil {
		t.Fatal(err)
	}
	final, err := ParseConfigBytes(saved)
	if err != nil || final.SpeedThrottle.Enabled || final.SpeedThrottle.MaxConcurrent != 3 || final.SpeedThrottle.QueueSize != 5 || final.Codex.IdentityConfuse || !final.EnableGeminiCLIEndpoint || !final.QuotaExceeded.AntigravityCreditsForce || strings.Count(string(saved), "# unknown-extension:") != 1 {
		t.Fatalf("legacy update lost the canonical layout or archive: %v\n%s", err, saved)
	}
}

func TestV8ForkCanonicalExplicitValuesWin(t *testing.T) {
	raw := []byte(`api-key-rate-limit: {enabled: true, default-rpm: 60}
access: {api-key-rate-limit: {enabled: false, default-rpm: 0}}
speed-throttle: {enabled: true, max-concurrent: 3, queue-size: 5}
requests: {speed-throttle: {enabled: false, max-concurrent: 0, queue-size: 0}}
enable-gemini-cli-endpoint: true
server: {enable-gemini-cli-endpoint: false}
quota-exceeded: {antigravity-credits-force: true}
oauth: {providers: {antigravity: {antigravity-credits-force: false}}}
codex: {identity-confuse: true}
upstream: {codex: {identity-confuse: false}}
`)
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKeyRateLimit.Enabled || cfg.APIKeyRateLimit.DefaultRPM != 0 || cfg.SpeedThrottle.Enabled || cfg.SpeedThrottle.MaxConcurrent != 0 || cfg.SpeedThrottle.QueueSize != 0 || cfg.EnableGeminiCLIEndpoint || cfg.QuotaExceeded.AntigravityCreditsForce || cfg.Codex.IdentityConfuse {
		t.Fatal("legacy value overrode explicit canonical false/zero")
	}
}

func TestV8ForkTemplateValidates(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(raw); err != nil {
		t.Fatalf("fork template is not canonical v8: %v", err)
	}
}

func TestV8ForkScopeSurvivesConfigurationSnapshots(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`config-version: 8
requests: {speed-throttle: {enabled: true, min-tokens-per-second: 20, max-concurrent: 3, queue-size: 5}}
upstream: {codex: {identity-confuse: true}}
oauth: {providers: {antigravity: {antigravity-credits-force: true}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*Config{cfg, cfg.CloneForRuntime()} {
		encoded, errMarshal := yaml.Marshal(snapshot)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		reloaded, errParse := ParseConfigBytes(encoded)
		if errParse != nil {
			t.Fatal(errParse)
		}
		apiKeyView := reloaded.ForAPIKey()
		if !reloaded.QuotaExceeded.AntigravityCreditsForce || apiKeyView.QuotaExceeded.AntigravityCreditsForce || !apiKeyView.SpeedThrottle.Enabled || apiKeyView.SpeedThrottle.MinTokensPerSecond != 20 || apiKeyView.SpeedThrottle.MaxConcurrent != 3 || apiKeyView.SpeedThrottle.QueueSize != 5 || !apiKeyView.Codex.IdentityConfuse {
			t.Fatal("configuration snapshot changed fork field scopes")
		}
	}
}
