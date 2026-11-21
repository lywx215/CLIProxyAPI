package cliproxy

import (
	"context"
	"fmt"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const enterpriseQuotaRefreshInterval = 2 * time.Minute
const enterpriseQuotaRetryInterval = 30 * time.Second

func (s *Service) configureEnterpriseQuotaProtection(ctx context.Context, cfg *config.Config) {
	if s.coreManager == nil || cfg == nil {
		return
	}
	dir := ""
	if !cfg.Home.Enabled {
		var errResolve error
		dir, errResolve = resolveCooldownStateAuthDir(cfg)
		if errResolve != nil {
			log.WithError(errResolve).Warn("failed to resolve Gemini Enterprise quota protection directory")
			return
		}
		dir = s.coreManager.RuntimeStateDir(dir)
	}
	if err := s.coreManager.ConfigureEnterpriseQuotaProtectionStore(ctx, dir); err != nil && ctx.Err() == nil {
		log.WithError(err).Warn("failed to restore Gemini Enterprise quota protection")
	}
}

// Quota reads share the bounded, cancellable discovery workers. They never run
// on the inference path and do not depend on an open management dashboard.
func (s *Service) queueEnterpriseQuota(ctx context.Context, auth *coreauth.Auth) {
	if s.coreManager == nil || auth == nil || auth.Provider != ge.Provider || coreauth.IsEnterpriseAccountAuth(auth) || auth.Disabled || auth.Status == coreauth.StatusDisabled || !s.coreManager.EnterpriseQuotaRefreshNeeded(auth) {
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
			quotaBinding := binding
			quotaBinding.ProjectID = identity.ProjectNumber
			observation = ge.FetchQuota(probeCtx, client, quotaBinding, ge.String(auth.Metadata, "access_token"), d.now())
			observation.ProjectID = binding.ProjectID
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
		// Keep retries bounded across the whole pool. A different credential
		// can supply the next reading after a permission or transport error.
		d.quotaNext[authKey] = now.Add(enterpriseQuotaRefreshInterval)
		next := now.Add(enterpriseQuotaRetryInterval)
		if accepted {
			next = now.Add(enterpriseQuotaRefreshInterval)
		}
		if reset := ge.NextPacificMidnight(now); reset.Before(next) {
			next = reset
		}
		d.quotaNext[poolKey] = next
	}()
}
