package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	EnterpriseWebSingleTurn        = "single-turn"
	EnterpriseWebSerializedContext = "serialized-context"
)

// GeminiEnterpriseWebConfig controls only the optional inline-media channel.
type GeminiEnterpriseWebConfig struct {
	Enabled                bool                           `yaml:"enabled" json:"enabled"`
	CookieCCPollingEnabled bool                           `yaml:"cookie-cc-polling-enabled" json:"cookie-cc-polling-enabled"`
	HistoryMode            string                         `yaml:"history-mode" json:"history-mode"`
	MaxConcurrent          int                            `yaml:"max-concurrent" json:"max-concurrent"`
	MaxInflightBytes       int64                          `yaml:"max-inflight-bytes" json:"max-inflight-bytes"`
	InputLimits            GeminiEnterpriseWebInputLimits `yaml:"input-limits" json:"input-limits"`
}

func (c *GeminiEnterpriseWebConfig) UnmarshalYAML(node *yaml.Node) error {
	var toggle struct {
		CC yaml.Node `yaml:"cookie-cc-polling-enabled"`
	}
	if err := node.Decode(&toggle); err != nil {
		return err
	}
	if toggle.CC.Kind != 0 && toggle.CC.ShortTag() != "!!bool" {
		return fmt.Errorf("gemini-enterprise-web cookie-cc-polling-enabled must be a boolean")
	}
	type plain GeminiEnterpriseWebConfig
	return node.Decode((*plain)(c))
}

func (c GeminiEnterpriseWebConfig) WithDefaults() GeminiEnterpriseWebConfig {
	if c.HistoryMode == "" {
		c.HistoryMode = EnterpriseWebSingleTurn
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 2
	}
	if c.MaxInflightBytes == 0 {
		c.MaxInflightBytes = 256 << 20
	}
	c.InputLimits = c.InputLimits.WithDefaults()
	return c
}
func (c GeminiEnterpriseWebConfig) Validate() error {
	if mode := c.WithDefaults().HistoryMode; mode != EnterpriseWebSingleTurn && mode != EnterpriseWebSerializedContext {
		return fmt.Errorf("gemini-enterprise-web history-mode must be single-turn or serialized-context")
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 16 || c.MaxInflightBytes < 64<<20 || c.MaxInflightBytes > 4096<<20 {
		return fmt.Errorf("gemini-enterprise-web requires concurrency 1..16 and a buffer budget of 64..4096 MiB")
	}
	return c.InputLimits.Validate()
}
