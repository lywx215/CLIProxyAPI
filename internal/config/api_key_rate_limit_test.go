package config

import "testing"

func TestParseConfigBytesAPIKeyRateLimitEnabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
api-key-rate-limit:
  enabled: true
  default-rpm: 50
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if !cfg.APIKeyRateLimit.Enabled {
		t.Fatal("API key rate limit enabled = false, want true")
	}
	if cfg.APIKeyRateLimit.DefaultRPM != 50 {
		t.Fatalf("API key rate limit default RPM = %d, want 50", cfg.APIKeyRateLimit.DefaultRPM)
	}
}
