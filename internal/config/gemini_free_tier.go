package config

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// GeminiFreeTierPolicyConfig is opt-in and only manages official Google API key generation.
type GeminiFreeTierPolicyConfig struct {
	Enabled       bool                        `yaml:"enabled" json:"enabled"`
	StoragePath   string                      `yaml:"storage-path" json:"storage-path"`
	COSBackup     GeminiBudgetCOSBackupConfig `yaml:"cos-backup" json:"cos-backup"`
	ManagedModels map[string]int              `yaml:"managed-models" json:"managed-models"`
}

type GeminiBudgetCOSBackupConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	Schedule  string `yaml:"schedule" json:"schedule"`
	ObjectKey string `yaml:"object-key" json:"object-key"`
}

func (p GeminiFreeTierPolicyConfig) WithDefaults() GeminiFreeTierPolicyConfig {
	if strings.TrimSpace(p.StoragePath) == "" {
		p.StoragePath = "/data/gemini-budget"
	}
	if strings.TrimSpace(p.COSBackup.Schedule) == "" {
		p.COSBackup.Schedule = "00:10"
	}
	if strings.TrimSpace(p.COSBackup.ObjectKey) == "" {
		p.COSBackup.ObjectKey = "quota/gemini-budget/latest.json"
	}
	if p.ManagedModels == nil {
		p.ManagedModels = map[string]int{
			"gemini-2.5-flash": 20, "gemini-3.5-flash": 20, "gemini-3.6-flash": 20, "gemini-3.7-flash": 20, "gemini-3.8-flash": 20,
			"gemini-2.5-flash-lite": 500, "gemini-3.1-flash-lite": 500, "gemini-3.1-flash-lite-preview": 500, "gemini-3.5-flash-lite": 500,
		}
	}
	return p
}

func (p GeminiFreeTierPolicyConfig) Validate() error {
	p = p.WithDefaults()
	if p.Enabled && len(p.ManagedModels) == 0 {
		return fmt.Errorf("gemini-free-tier-policy.managed-models cannot be empty when enabled")
	}
	if _, err := time.Parse("15:04", p.COSBackup.Schedule); err != nil {
		return fmt.Errorf("gemini-free-tier-policy.cos-backup.schedule must be HH:MM")
	}
	key := p.COSBackup.ObjectKey
	if strings.HasPrefix(key, "/") || strings.Contains(key, "..") || path.Clean(key) != key || strings.Contains(key, "\\") || strings.HasPrefix(key, "auths/") || strings.HasPrefix(key, "config/") {
		return fmt.Errorf("gemini-free-tier-policy.cos-backup.object-key must be a separate relative object key")
	}
	for model, limit := range p.ManagedModels {
		if strings.TrimSpace(model) != model || model != strings.ToLower(model) || model == "" || strings.ContainsAny(model, "/(): \t\r\n") || limit <= 0 || limit > 1000000 {
			return fmt.Errorf("gemini-free-tier-policy.managed-models requires canonical model names and positive limits")
		}
	}
	if strings.TrimSpace(p.StoragePath) == "" || filepath.Clean(p.StoragePath) == "." {
		return fmt.Errorf("gemini-free-tier-policy.storage-path is required")
	}
	return nil
}
