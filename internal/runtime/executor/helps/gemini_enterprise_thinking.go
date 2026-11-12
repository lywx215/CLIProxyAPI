package helps

import (
	"strings"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ApplyEnterpriseThinking supplies the original protocol intent and exact model
// capabilities to the unified pipeline before the final payload barrier.
func ApplyEnterpriseThinking(body []byte, req coreexec.Request, opts coreexec.Options) ([]byte, error) {
	base := thinking.ParseSuffix(req.Model).ModelName
	support := registry.EnterpriseThinkingSupport(base)
	if support == nil {
		actual := strings.TrimPrefix(strings.TrimSpace(base), "models/")
		if selected, ok := coreauth.ResolvedModelInfo(req); ok && selected != nil {
			actual = selected.MetadataModelID
			if actual == "" {
				actual = selected.ID
			}
			actual = thinking.ParseSuffix(strings.TrimPrefix(strings.TrimSpace(actual), "models/")).ModelName
		}
		if actual == ge.CarbonModel || actual == ge.HydrogenModel {
			// Carbon has no verified thinking levels or budget controls. Preserve
			// only summary visibility without the generic nil-capability cleanup.
			source := req.Payload
			if len(source) == 0 {
				source = opts.OriginalRequest
			}
			summary := thinking.ExtractSummaryConfig(source, opts.SourceFormat.String())
			return thinking.ApplySummaryConfig(body, ge.Provider, summary), nil
		}
		return body, nil
	}
	info := &registry.ModelInfo{ID: base, Type: "gemini", Thinking: support}
	if registered := registry.GetGlobalRegistry().GetModelInfo(base, "gemini-enterprise"); registered != nil {
		copyInfo := *registered
		info = &copyInfo
		if !info.ExplicitThinking {
			info.Thinking = support
		}
	}
	if selected, ok := coreauth.ResolvedModelInfo(req); ok && selected != nil {
		copyInfo := *selected
		info = &copyInfo
		if info.Thinking == nil && !info.ExplicitThinking {
			info.Thinking = support
		}
	}
	source := req.Payload
	if len(source) == 0 {
		source = opts.OriginalRequest
	}
	from := opts.SourceFormat.String()
	summary := thinking.ExtractSummaryConfig(source, from)
	return thinking.ApplyThinkingWithModelInfoAndSummary(body, source, req.Model, from, "gemini-enterprise", "gemini-enterprise", info, summary)
}
