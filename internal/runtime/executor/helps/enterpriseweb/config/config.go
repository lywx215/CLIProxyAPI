// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package config

// Binding deliberately has no default model selector or independent configuration store.
type Binding struct {
	GenerationParameters map[string]ParameterBinding `json:"generation_parameters,omitempty"`
}
type ParameterBinding struct {
	UpstreamField string `json:"upstream_field"`
	Verified      bool   `json:"verified"`
	Evidence      string `json:"evidence"`
}
