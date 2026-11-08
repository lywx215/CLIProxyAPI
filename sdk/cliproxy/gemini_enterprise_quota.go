package cliproxy

import (
	"context"
	"fmt"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const enterpriseQuotaRefreshInterval = 2 * time.Minute

// Quota reads share the bounded, cancellable discovery workers. They never run
// on the inference path and do not depend on an open management dashboard.
func (s *Service) queueEnterpriseQuota(ctx context.Context, auth *coreauth.Auth) {
	if s.coreManager == nil || !s.coreManager.EnterpriseQuotaBalancingEnabled() || auth == nil || auth.Provider != ge.Provider || coreauth.IsEnterpriseAccountAuth(auth) || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return
	}
	auth = auth.Clone()
	binding := ge.FromMetadata(auth.Metadata)
	identity := auth.EnterpriseQuotaIdentity
	if !identity.ValidFor(binding) || ge.String(auth.Metadata, "access_token") == "" {
		return
	}
	key := s.enterpriseKey(auth)
	if !s.enterpriseActive(auth, key) || !s.enterpriseTokenCurrent(auth) {
		return
	}
	poolKey := fmt.Sprintf("pool:%s/%s/%s", identity.ProjectNumber, identity.Location, identity.Edition)
	authKey := "auth:" + key
	s.cfgMu.RLock()
	lifetime := s.antigravityContext
	s.cfgMu.RUnlock()
	if lifetime == nil {
		lifetime = ctx
	}
	if lifetime == nil {
		lifetime = context.Background()
	}
	d := s.enterpriseState()
	d.mu.Lock()
	now := d.now()
	if d.closed || d.quotaPending[poolKey].cancel != nil || now.Before(d.quotaNext[poolKey]) || now.Before(d.quotaNext[authKey]) {
		d.mu.Unlock()
		return
	}
	if d.quotaPending == nil {
		d.quotaPending = map[string]enterpriseProbe{}
		d.quotaNext = map[string]time.Time{}
	}
	for cachedKey, next := range d.quotaNext {
		if !now.Before(next) {
			delete(d.quotaNext, cachedKey)
		}
	}
	probeCtx, cancel := context.WithCancel(lifetime)
	d.sequence++
	revision := d.sequence
	d.quotaPending[poolKey] = enterpriseProbe{authID: auth.ID, cancel: cancel, revision: revision}
	d.wg.Add(1)
	d.mu.Unlock()
	proxy := s.enterpriseProxy(auth)
	go func() {
		defer d.wg.Done()
		defer cancel()
		defer func() {
			d.mu.Lock()
			if d.quotaPending[poolKey].revision == revision {
				delete(d.quotaPending, poolKey)
			}
			d.mu.Unlock()
		}()
		select {
		case d.slots <- struct{}{}:
		case <-probeCtx.Done():
			return
		}
		defer func() { <-d.slots }()
		var observation ge.QuotaSnapshot
		if d.fetchQuota != nil {
			observation = d.fetchQuota(probeCtx, auth, proxy)
		} else if client, errClient := ge.Client(proxy); errClient == nil {
			observation = ge.FetchQuota(probeCtx, client, binding, ge.String(auth.Metadata, "access_token"), d.now())
			client.CloseIdleConnections()
		}
		if probeCtx.Err() != nil || !s.enterpriseActive(auth, key) {
			return
		}
		accepted := s.coreManager.ObserveEnterpriseQuota(auth, observation)
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.quotaPending[poolKey].revision != revision {
			return
		}
		now := d.now()
		// Retry another credential in the pool after a permission/transport error.
		d.quotaNext[authKey] = now.Add(enterpriseQuotaRefreshInterval)
		if accepted {
			next := now.Add(enterpriseQuotaRefreshInterval)
			if reset := ge.NextPacificMidnight(now); reset.Before(next) {
				next = reset
			}
			d.quotaNext[poolKey] = next
		}
	}()
}
