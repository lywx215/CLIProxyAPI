// Package geminienterprise applies canonical thinking controls to StreamAssist.
package geminienterprise

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/sjson"
)

type Applier struct{}

func init() {
	thinking.RegisterProvider("gemini-enterprise", &Applier{})
}

// Apply receives a normalized level; source parsing, suffixes and capability
// conversions remain in the shared thinking pipeline.
func (*Applier) Apply(body []byte, config thinking.ThinkingConfig, _ *registry.ModelInfo) ([]byte, error) {
	level := config.Level
	if config.Mode != thinking.ModeLevel && config.Mode != thinking.ModeNone {
		return body, nil
	}
	if level == "" {
		return sjson.DeleteBytes(body, "generationSpec.thinkingConfig.thinkingLevel")
	}
	if level != thinking.LevelLow && level != thinking.LevelHigh {
		return body, thinking.NewThinkingError(thinking.ErrLevelNotSupported, "Enterprise supports the verified low and high levels")
	}
	result, err := sjson.DeleteBytes(body, "generationSpec.thinkingConfig.thinkingBudget")
	if err != nil {
		return body, err
	}
	return sjson.SetBytes(result, "generationSpec.thinkingConfig.thinkingLevel", strings.ToUpper(string(level)))
}
