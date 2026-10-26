package cliproxy

import (
	"context"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Subscription and catalog requests share the same bounded discovery worker slots.
func (s *Service) queueEnterpriseSubscription(ctx context.Context, auth *coreauth.Auth) {
	if s.coreManager == nil || auth == nil || auth.Provider != ge.Provider || coreauth.IsEnterpriseAccountAuth(auth) || auth.Disabled || auth.Status == coreauth.StatusDisabled || s.antigravityHomeEnabled() {
		return
	}
	if _, _, errSettings := ge.ModelPolicySettings(auth.Metadata); errSettings != nil || ge.String(auth.Metadata, "access_token") == "" {
		return
	}
	if !s.enterpriseActive(auth, s.enterpriseKey(auth)) || !s.enterpriseTokenCurrent(auth) {
		return
	}
	auth = auth.Clone()
	flight := s.coreManager.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil {
		return
	}
	s.cfgMu.RLock()
	lifetime := s.antigravityContext
	s.cfgMu.RUnlock()
	if lifetime == nil {
		lifetime = ctx
	}
	if lifetime == nil {
		lifetime = context.Background()
	}
	probeCtx, cancel := context.WithCancel(lifetime)
	d := s.enterpriseState()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		cancel()
		flight.Cancel()
		return
	}
	if d.subscriptionPending == nil {
		d.subscriptionPending = map[string]enterpriseProbe{}
	}
	if old, ok := d.subscriptionPending[auth.ID]; ok {
		old.cancel()
	}
	d.sequence++
	revision := d.sequence
	d.subscriptionPending[auth.ID] = enterpriseProbe{cancel: cancel, revision: revision}
	d.wg.Add(1)
	d.mu.Unlock()
	proxy := s.enterpriseProxy(auth)
	go func() {
		defer d.wg.Done()
		defer cancel()
		defer flight.Cancel()
		defer func() {
			d.mu.Lock()
			if d.subscriptionPending[auth.ID].revision == revision {
				delete(d.subscriptionPending, auth.ID)
			}
			d.mu.Unlock()
		}()
		select {
		case d.slots <- struct{}{}:
		case <-probeCtx.Done():
			return
		}
		defer func() { <-d.slots }()
		observation := ge.SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable", CheckedAt: time.Now().UTC()}
		if d.fetchSubscription != nil {
			observation = d.fetchSubscription(probeCtx, auth, proxy)
		} else if client, errClient := ge.Client(proxy); errClient == nil {
			observation = ge.ResolveSubscription(probeCtx, client, ge.FromMetadata(auth.Metadata), ge.String(auth.Metadata, "access_token"), time.Now())
			client.CloseIdleConnections()
		}
		if probeCtx.Err() != nil {
			return
		}
		if flight.Complete(observation) {
			s.coreManager.PublishEnterpriseSubscriptionIdentity(auth)
			s.refreshEnterpriseModels(probeCtx)
		}
	}()
}
