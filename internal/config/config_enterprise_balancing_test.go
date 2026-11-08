package config

import "testing"

func TestEnterpriseQuotaBalancingV8RoundTrip(t *testing.T) {
	raw := []byte("config-version: 8\nrouting:\n  strategy: round-robin\n  gemini-enterprise-quota-balancing: true\n  gemini-enterprise-quota-reserve: 10\n")
	if err := ValidateV8Config(raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cfg, err := ParseConfigBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Routing.GeminiEnterpriseQuotaBalancing || cfg.Routing.GeminiEnterpriseQuotaReserve != 10 || cfg.Routing.Strategy != "round-robin" {
			t.Fatalf("routing configuration lost: %+v", cfg.Routing)
		}
		raw, _, err = NormalizeConfigLayout(raw, true)
		if err != nil {
			t.Fatal(err)
		}
	}
}
