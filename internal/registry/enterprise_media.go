package registry

// EnterpriseMediaKind follows catalog aliases and prefixes without inventing capabilities.
func EnterpriseMediaKind(model string) string {
	info := GetGlobalRegistry().GetModelInfo(model, "gemini-enterprise")
	if info == nil {
		return ""
	}
	return info.EnterpriseMedia
}
