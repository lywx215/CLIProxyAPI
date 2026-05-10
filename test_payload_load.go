package main

import (
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"gopkg.in/yaml.v3"
)

func main() {
	yamlData := `
payload:
  default:
    - models:
        - name: gemini-3-pro-preview
        - name: gemini-2.5-pro
        - name: gemini-3.1-pro-preview
      params:
        generationConfig.thinkingConfig.includeThoughts: true
  override:
    - models:
        - name: gemini-3-pro-preview
        - name: gemini-2.5-pro
        - name: gemini-3.1-pro-preview
        - name: gemini-2.5-flash
      params:
        generationConfig.thinkingConfig.thinkingBudget: 24576
`
	var cfg config.Config
	err := yaml.Unmarshal([]byte(yamlData), &cfg)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Printf("Default rules: %d\n", len(cfg.Payload.Default))
	fmt.Printf("Override rules: %d\n", len(cfg.Payload.Override))
	if len(cfg.Payload.Override) > 0 {
	    fmt.Printf("Override models: %d\n", len(cfg.Payload.Override[0].Models))
	    fmt.Printf("Override params: %v\n", cfg.Payload.Override[0].Params)
	}
}
