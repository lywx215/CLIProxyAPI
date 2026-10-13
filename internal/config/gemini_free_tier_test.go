package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGeminiFreeTierPolicySurvivesConfigSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{GeminiFreeTierPolicy: GeminiFreeTierPolicyConfig{Enabled: true, StoragePath: filepath.Join(t.TempDir(), "ledger"), ManagedModels: map[string]int{"gemini-2.5-flash": 20}}}
	if err := SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !after.GeminiFreeTierPolicy.Enabled || after.GeminiFreeTierPolicy.StoragePath != cfg.GeminiFreeTierPolicy.StoragePath || after.GeminiFreeTierPolicy.ManagedModels["gemini-2.5-flash"] != 20 {
		t.Fatal("saving discarded the budget policy")
	}
}

func TestGeminiFreeTierV8ConfigAndProjectRoundTrip(t *testing.T) {
	raw := []byte(`config-version: 8
gemini-free-tier-policy:
  enabled: true
  storage-path: /data/gemini-budget
  cos-backup: {enabled: false, schedule: "00:10"}
  managed-models: {gemini-2.5-flash: 20, gemini-3.5-flash-lite: 500}
api-keys:
  gemini:
    - name: shared
      models: [{name: gemini-3.5-flash-lite, alias: public-lite}]
      keys:
        - api-key: synthetic-key-a
          quota-project: synthetic-project
        - api-key: synthetic-key-b
          quota-project: synthetic-project
`)
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GeminiFreeTierPolicy.Enabled || cfg.GeminiFreeTierPolicy.ManagedModels["gemini-3.5-flash-lite"] != 500 || len(cfg.GeminiKey) != 2 || cfg.GeminiKey[0].QuotaProject != "synthetic-project" {
		t.Fatalf("policy/project lost: %+v", cfg.GeminiFreeTierPolicy)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfigBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if after.GeminiKey[1].QuotaProject != "synthetic-project" || after.GeminiKey[0].Models[0].Alias != "public-lite" {
		t.Fatal("round trip changed project or mapping")
	}
}

func TestGeminiFreeTierConfigurationRejectsInvalidPolicy(t *testing.T) {
	for _, raw := range []string{
		"gemini-free-tier-policy: {enabled: true, managed-models: {gemini-2.5-flash: -1}}",
		"gemini-free-tier-policy: {cos-backup: {schedule: '24:10'}}",
		"gemini-free-tier-policy: {cos-backup: {object-key: 'config/config.yaml'}}",
		"gemini-free-tier-policy: {enabled: true, managed-models: {}}",
		"gemini-free-tier-policy: {managed-models: {Gemini-2.5-flash: 20}}",
		"gemini-free-tier-policy: {cos-backup: {object-key: 'quota//latest.json'}}",
		"gemini-free-tier-policy: {cos-backup: {object-key: 'quota\\latest.json'}}",
		"gemini-free-tier-policy: {admission-queue: {max-pending-per-model: -1}}",
		"gemini-free-tier-policy: {admission-queue: {max-pending-total: 8193}}",
		"gemini-free-tier-policy: {admission-queue: {max-wait-seconds: 301}}",
	} {
		if _, err := ParseConfigBytes([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid policy: %s", raw)
		}
	}
	cfg, err := ParseConfigBytes([]byte("port: 8317"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GeminiFreeTierPolicy.Enabled {
		t.Fatal("legacy config unexpectedly enabled budget")
	}
}

func TestGeminiAdmissionConfigurationDefaultsAndRoundTrip(t *testing.T) {
	defaults := (GeminiFreeTierPolicyConfig{}).AdmissionSettings()
	if !defaults.IsEnabled() || defaults.MaxPendingPerModel != 128 || defaults.MaxPendingTotal != 256 || defaults.MaxWaitSeconds != 75 {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
	raw := []byte("config-version: 8\ngemini-free-tier-policy:\n  enabled: true\n  managed-models: {gemini-2.5-flash: 20}\n  admission-queue: {enabled: false, max-pending-per-model: 8, max-pending-total: 16, max-wait-seconds: 30}\n")
	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	cloned := cfg.CloneForRuntime()
	*cfg.GeminiFreeTierPolicy.AdmissionQueue.Enabled = true
	if cloned.GeminiFreeTierPolicy.AdmissionSettings().IsEnabled() {
		t.Fatal("runtime queue config pointer was shared")
	}
	encoded, err := yaml.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfigBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	settings := after.GeminiFreeTierPolicy.AdmissionSettings()
	if settings.IsEnabled() || settings.MaxPendingPerModel != 8 || settings.MaxPendingTotal != 16 || settings.MaxWaitSeconds != 30 {
		t.Fatalf("queue config lost: %+v", settings)
	}
}

func TestGeminiFreeTierV8ManagementAcceptsPolicyAndValidatesValues(t *testing.T) {
	if err := ValidateV8Config([]byte("config-version: 8\ngemini-free-tier-policy: {enabled: false, managed-models: {gemini-2.5-flash: 20}}\n")); err != nil {
		t.Fatalf("valid management policy rejected: %v", err)
	}
	if err := ValidateV8Config([]byte("config-version: 8\ngemini-free-tier-policy: {enabled: true, managed-models: {gemini-2.5-flash: -1}}\n")); err == nil {
		t.Fatal("management accepted invalid quota limit")
	}
}
