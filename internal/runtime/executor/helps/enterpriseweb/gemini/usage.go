package gemini

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

// Media usage is unavailable; never claim local estimates are upstream counters.
func (p *historyPlan) localInputUsage(context.Context) (contract.Usage, error) {
	return contract.Usage{}, nil
}
