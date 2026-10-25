package registry

// EnterpriseThinkingSupport is intentionally limited to the directly verified
// models and levels. A readable catalog alone does not establish these controls.
func EnterpriseThinkingSupport(model string) *ThinkingSupport {
	if model != "gemini-3.1-pro-preview" && model != "gemini-3.8-flash" {
		return nil
	}
	return &ThinkingSupport{Levels: []string{"low", "high"}}
}
