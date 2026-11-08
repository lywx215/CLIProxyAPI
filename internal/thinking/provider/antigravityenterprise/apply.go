// Package antigravityenterprise applies thinking to the Business AI Code payload.
package antigravityenterprise

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/gemini"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type Applier struct{}

func init() { thinking.RegisterProvider("antigravity-enterprise", &Applier{}) }

func (*Applier) Apply(body []byte, config thinking.ThinkingConfig, model *registry.ModelInfo) ([]byte, error) {
	result, err := gemini.NewApplier().Apply(body, config, model)
	if err != nil {
		return nil, err
	}
	// The Business AI Code protobuf JSON transport uses enum identifiers.
	if level := gjson.GetBytes(result, "generationConfig.thinkingConfig.thinkingLevel"); level.Exists() {
		result, err = sjson.SetBytes(result, "generationConfig.thinkingConfig.thinkingLevel", strings.ToUpper(level.String()))
	}
	return result, err
}
