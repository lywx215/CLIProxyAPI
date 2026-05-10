package main

import (
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"gopkg.in/yaml.v3"
)

func main() {
	yamlData := `
payload:
  override:
    - models:
        - name: gemini-*
      params:
        generationConfig.thinkingConfig.thinkingBudget: 24576
`
	var cfg config.Config
	yaml.Unmarshal([]byte(yamlData), &cfg)
	cfg.SanitizePayloadRules()

	// What if the payload has thinking_budget instead of thinkingBudget?
	payload := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinking_budget":32768}}}}`)
	
	out := helps.ApplyPayloadConfigWithRoot(&cfg, "gemini-2.5-flash", "antigravity", "request", payload, nil, "gemini-2.5-flash", "")
	fmt.Println("Result:", string(out))
}
