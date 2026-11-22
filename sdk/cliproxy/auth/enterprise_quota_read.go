package auth

import (
	"context"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

type enterpriseQuotaReadKey struct{ project, location, edition string }
type enterpriseQuotaRead struct {
	done             chan struct{}
	gate             *enterpriseQuotaGate
	principal, proxy string
	result           ge.QuotaSnapshot
	reusable         bool
}

// ReadEnterpriseQuota coordinates manual and background metadata reads. Only
// the same credential and route may reuse a result. Other members of the
// same pool wait their turn and fetch with their own Google permissions.
func (m *Manager) ReadEnterpriseQuota(ctx context.Context, auth *Auth, identity *ge.QuotaIdentity, proxy string, fetch func(context.Context) ge.QuotaSnapshot) ge.QuotaSnapshot {
	if ctx.Err() != nil {
		return ge.QuotaSnapshot{}
	}
	if auth == nil || auth.enterpriseQuota == nil || !identity.ValidFor(ge.FromMetadata(auth.Metadata)) {
		return fetch(ctx)
	}
	key := enterpriseQuotaReadKey{identity.ProjectNumber, identity.Location, identity.Edition}
	c := &m.enterpriseQuota
	for ctx.Err() == nil {
		c.mu.Lock()
		if flight := c.reads[key]; flight != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return ge.QuotaSnapshot{}
			case <-flight.done:
				if ctx.Err() == nil && flight.reusable && flight.gate == auth.enterpriseQuota && flight.principal == identity.PrincipalHash && flight.proxy == proxy {
					return ge.CloneQuotaSnapshot(flight.result)
				}
				continue
			}
		}
		if c.reads == nil {
			c.reads = make(map[enterpriseQuotaReadKey]*enterpriseQuotaRead)
		}
		flight := &enterpriseQuotaRead{done: make(chan struct{}), gate: auth.enterpriseQuota, principal: identity.PrincipalHash, proxy: proxy}
		c.reads[key] = flight
		c.mu.Unlock()

		defer func() {
			c.mu.Lock()
			delete(c.reads, key)
			close(flight.done)
			c.mu.Unlock()
		}()
		result := fetch(ctx)
		if ctx.Err() != nil {
			return ge.QuotaSnapshot{}
		}
		flight.result, flight.reusable = ge.CloneQuotaSnapshot(result), true
		return result
	}
	return ge.QuotaSnapshot{}
}
