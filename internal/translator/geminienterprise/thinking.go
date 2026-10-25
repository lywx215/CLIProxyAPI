package geminienterprise

// thinkingConfig validates source structure only. Amount conversion and summary
// intent belong to the unified thinking pipeline, applied by the executor.
func (n *normalizer) thinkingConfig(generation map[string]any) {
	value := n.alias(generation, "thinkingConfig", "thinking_config", "generationConfig.")
	if value == nil {
		return
	}
	config := n.object(value, "generationConfig.thinkingConfig")
	level := n.alias(config, "thinkingLevel", "thinking_level", "generationConfig.thinkingConfig.")
	budget := n.alias(config, "thinkingBudget", "thinking_budget", "generationConfig.thinkingConfig.")
	if level != nil {
		n.text(level, "generationConfig.thinkingConfig.thinkingLevel")
	}
	if budget != nil {
		n.failField("generationConfig.thinkingConfig.thinkingBudget", "is not supported by the verified Enterprise level controls")
	}
	if include := n.alias(config, "includeThoughts", "include_thoughts", "generationConfig.thinkingConfig."); include != nil {
		if _, ok := include.(bool); !ok {
			n.failField("generationConfig.thinkingConfig.includeThoughts", "must be a boolean")
		}
	}
	n.unknown(config, "thinkingLevel|thinking_level|thinkingBudget|thinking_budget|includeThoughts|include_thoughts", "generationConfig.thinkingConfig.")
}

func (n *normalizer) chatThinking(body map[string]any) {
	if value, exists := body["reasoning_effort"]; exists {
		n.text(value, "reasoning_effort")
	}
	value, exists := body["extra_body"]
	if !exists {
		return
	}
	extra := n.object(value, "extra_body")
	n.unknown(extra, "google", "extra_body.")
	value, exists = extra["google"]
	if !exists {
		return
	}
	google := n.object(value, "extra_body.google")
	n.unknown(google, "thinking_config|thinkingConfig", "extra_body.google.")
	value = n.alias(google, "thinking_config", "thinkingConfig", "extra_body.google.")
	if value == nil {
		return
	}
	config := n.object(value, "extra_body.google.thinking_config")
	if include := n.alias(config, "include_thoughts", "includeThoughts", "extra_body.google.thinking_config."); include != nil {
		if _, ok := include.(bool); !ok {
			n.failField("extra_body.google.thinking_config.include_thoughts", "must be a boolean")
		}
	}
	n.unknown(config, "include_thoughts|includeThoughts", "extra_body.google.thinking_config.")
}
