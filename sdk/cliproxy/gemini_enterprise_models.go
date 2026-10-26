package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const enterpriseCatalogTTL = 5 * time.Minute

type enterpriseCatalog struct {
	models          []ge.Model
	known           bool
	revision        uint64
	expires         time.Time
	appliedEpoch    uint64
	appliedRevision uint64
	appliedPolicy   string
	credential      string
}
type enterpriseFailure struct {
	attempts int
	next     time.Time
}
type enterpriseProbe struct {
	signature string
	cancel    context.CancelFunc
	revision  uint64
}
type enterpriseDiscovery struct {
	mu                  sync.Mutex
	catalogs            map[string]enterpriseCatalog
	failures            map[string]enterpriseFailure
	pending             map[string]enterpriseProbe
	subscriptionPending map[string]enterpriseProbe
	sequence            uint64
	closed              bool
	slots               chan struct{}
	wg                  sync.WaitGroup
	now                 func() time.Time
	// The injected fetch function makes lifecycle tests independent of Google credentials.
	fetch             func(context.Context, *coreauth.Auth, string) ([]ge.Model, error)
	fetchSubscription func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation
}

func (s *Service) enterpriseState() *enterpriseDiscovery {
	s.enterpriseMu.Lock()
	defer s.enterpriseMu.Unlock()
	if s.enterpriseDiscovery == nil {
		s.enterpriseDiscovery = &enterpriseDiscovery{catalogs: map[string]enterpriseCatalog{}, failures: map[string]enterpriseFailure{}, pending: map[string]enterpriseProbe{}, slots: make(chan struct{}, 5), now: time.Now}
	}
	return s.enterpriseDiscovery
}

func (s *Service) enterpriseProxy(auth *coreauth.Auth) string {
	if auth.ProxyURL != "" {
		return auth.ProxyURL
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg != nil {
		return s.cfg.ProxyURL
	}
	return ""
}

func (s *Service) enterpriseKey(auth *coreauth.Auth) string {
	return fmt.Sprintf("%q/%d/%s/%x", auth.ID, auth.RegistrationEpoch, ge.Binding(ge.FromMetadata(auth.Metadata)), sha256.Sum256([]byte(s.enterpriseProxy(auth))))
}

func enterpriseFailureKey(key string, auth *coreauth.Auth) string {
	return fmt.Sprintf("%s/%x", key, sha256.Sum256([]byte(ge.String(auth.Metadata, "access_token"))))
}

func (s *Service) enterpriseActive(auth *coreauth.Auth, key string) bool {
	if auth == nil || coreauth.IsEnterpriseAccountAuth(auth) || s.coreManager == nil || s.antigravityHomeEnabled() {
		return false
	}
	current, ok := s.coreManager.GetByID(auth.ID)
	return ok && current != nil && current.Provider == ge.Provider && !current.Disabled && current.Status != coreauth.StatusDisabled &&
		current.RegistrationEpoch == auth.RegistrationEpoch && current.Prefix == auth.Prefix && maps.Equal(current.Attributes, auth.Attributes) && s.enterpriseKey(current) == key
}

func (s *Service) enterpriseTokenCurrent(auth *coreauth.Auth) bool {
	current, ok := s.coreManager.GetByID(auth.ID)
	return ok && current != nil && ge.String(current.Metadata, "access_token") == ge.String(auth.Metadata, "access_token")
}

// publishEnterpriseCatalog uses registry CAS and a probe revision under the same lock.
// An empty successful catalog is authoritative and removes this account's models.
func (s *Service) publishEnterpriseCatalog(ctx context.Context, auth *coreauth.Auth, key string, catalog enterpriseCatalog, epoch uint64) bool {
	if ctx.Err() != nil || !catalog.known || !s.enterpriseActive(auth, key) {
		return false
	}
	currentAuth, ok := s.coreManager.GetByID(auth.ID)
	if !ok || !s.enterpriseActive(currentAuth, key) {
		return false
	}
	auth = currentAuth
	s.cfgMu.RLock()
	if s.cfg == nil || s.cfg.Home.Enabled {
		s.cfgMu.RUnlock()
		return false
	}
	cfg := s.cfg
	policy := s.coreManager.EnterpriseModelPolicyView(auth)
	policyData, _ := json.Marshal(policy.ModelPolicy)
	policySignature := string(policyData)
	models := make([]*ModelInfo, 0, len(catalog.models))
	for _, model := range catalog.models {
		if !policy.Allows(model.ID) || policy.Restricted() && strings.EqualFold(strings.TrimSpace(model.DisplayName), "auto") {
			continue
		}
		info := &ModelInfo{ID: model.ID, Object: "model", OwnedBy: ge.Provider, Type: "gemini", Name: "models/" + model.ID, DisplayName: model.DisplayName, EnterpriseMedia: model.MediaKind, SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"}}
		info.Thinking = registry.EnterpriseThinkingSupport(model.ID)
		if model.MediaKind != "" {
			info.SupportedInputModalities = []string{"TEXT"}
			info.SupportedOutputModalities = []string{strings.ToUpper(model.MediaKind)}
			if model.MediaKind == "video" {
				info.Description = "Veo 3.1 via the Gemini Enterprise default video tool. Enterprise selects the underlying variant; no publisher model ID is pinned."
				info.SupportedGenerationMethods = append(info.SupportedGenerationMethods, "predictLongRunning")
			}
		}
		models = append(models, info)
	}
	excluded := cfg.OAuthExcludedModels[ge.Provider]
	if value := auth.Attributes["excluded_models"]; value != "" {
		excluded = strings.Split(value, ",")
	}
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAliasForAuth(cfg, ge.Provider, auth.AuthKind(), auth.Attributes, models)
	models = applyOAuthSettingsForAuth(cfg, ge.Provider, auth.AuthKind(), models)
	models = applyModelPrefixes(models, auth.Prefix, cfg.ForceModelPrefix)
	d := s.enterpriseState()
	d.mu.Lock()
	current := d.catalogs[key]
	if current.revision != catalog.revision {
		d.mu.Unlock()
		s.cfgMu.RUnlock()
		return false
	}
	appliedEpoch, updated := registry.GetGlobalRegistry().ReplaceClientModels(auth.ID, ge.Provider, epoch, models)
	if updated {
		current.appliedEpoch = appliedEpoch
		current.appliedRevision = catalog.revision
		current.appliedPolicy = policySignature
		d.catalogs[key] = current
	}
	d.mu.Unlock()
	s.cfgMu.RUnlock()
	if updated && s.enterpriseActive(auth, key) {
		s.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)
		s.coreManager.RefreshSchedulerEntry(auth.ID)
	}
	return updated
}

func (s *Service) registerEnterpriseModels(ctx context.Context, auth *coreauth.Auth) {
	if s.antigravityHomeEnabled() {
		s.cancelEnterpriseProbes(auth.ID)
		log.Warn("gemini-enterprise is not supported in CLIProxyAPIHome mode")
		return
	}
	s.queueEnterpriseSubscription(ctx, auth)
	key := s.enterpriseKey(auth)
	if !s.enterpriseActive(auth, key) {
		return
	}
	reg := registry.GetGlobalRegistry()
	expectedEpoch := reg.ClientRegistrationEpoch(auth.ID)
	d := s.enterpriseState()
	d.mu.Lock()
	catalog := d.catalogs[key]
	d.mu.Unlock()
	if catalog.known {
		s.publishEnterpriseCatalog(ctx, auth, key, catalog, expectedEpoch)
	} else {
		// A rebound account must not retain the old resource's entitlements while
		// its first catalog is pending. Fence revocation too: a concurrent success
		// must not be removed by a caller that observed an unknown catalog earlier.
		s.cfgMu.RLock()
		if s.cfg == nil || !s.cfg.Home.Enabled {
			d.mu.Lock()
			if !d.catalogs[key].known && len(reg.GetModelsForClient(auth.ID)) > 0 {
				reg.ReplaceClientModels(auth.ID, ge.Provider, expectedEpoch, nil)
			}
			d.mu.Unlock()
		}
		s.cfgMu.RUnlock()
	}
	s.queueEnterpriseDiscovery(ctx, auth)
}

func (s *Service) queueEnterpriseDiscovery(ctx context.Context, auth *coreauth.Auth) {
	if auth == nil || coreauth.IsEnterpriseAccountAuth(auth) || auth.Provider != ge.Provider || auth.Disabled || auth.Status == coreauth.StatusDisabled || s.antigravityHomeEnabled() {
		return
	}
	auth = auth.Clone()
	key := s.enterpriseKey(auth)
	if !s.enterpriseActive(auth, key) || !s.enterpriseTokenCurrent(auth) {
		return
	}
	token := ge.String(auth.Metadata, "access_token")
	if token == "" {
		return
	}
	epoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)
	failureKey := enterpriseFailureKey(key, auth)
	signature := fmt.Sprintf("%s/%d", failureKey, epoch)
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
	if d.closed {
		d.mu.Unlock()
		return
	}
	if catalog := d.catalogs[key]; catalog.known && d.now().Before(catalog.expires) && catalog.credential == failureKey {
		d.mu.Unlock()
		return
	}
	if old, ok := d.pending[auth.ID]; ok {
		if old.signature == signature {
			d.mu.Unlock()
			return
		}
		old.cancel()
	}
	if d.now().Before(d.failures[failureKey].next) {
		d.mu.Unlock()
		return
	}
	d.sequence++
	revision := d.sequence
	probeCtx, cancel := context.WithCancel(lifetime)
	d.pending[auth.ID] = enterpriseProbe{signature: signature, cancel: cancel, revision: revision}
	d.wg.Add(1)
	d.mu.Unlock()
	proxy := s.enterpriseProxy(auth)
	go func() {
		defer d.wg.Done()
		defer cancel()
		defer func() {
			d.mu.Lock()
			if d.pending[auth.ID].revision == revision {
				delete(d.pending, auth.ID)
			}
			d.mu.Unlock()
		}()
		select {
		case d.slots <- struct{}{}:
		case <-probeCtx.Done():
			return
		}
		defer func() { <-d.slots }()
		var models []ge.Model
		var err error
		if d.fetch != nil {
			models, err = d.fetch(probeCtx, auth, proxy)
		} else {
			client, errClient := ge.Client(proxy)
			if errClient != nil {
				err = errClient
			} else {
				models, err = ge.Discover(probeCtx, client, ge.FromMetadata(auth.Metadata), token)
				client.CloseIdleConnections()
			}
		}
		if probeCtx.Err() != nil || !s.enterpriseActive(auth, key) || !s.enterpriseTokenCurrent(auth) {
			return
		}
		d.mu.Lock()
		if d.pending[auth.ID].revision != revision {
			d.mu.Unlock()
			return
		}
		if err != nil {
			failure := d.failures[failureKey]
			failure.attempts = min(failure.attempts+1, 6)
			failure.next = d.now().Add(min(time.Duration(1<<failure.attempts)*5*time.Second, enterpriseCatalogTTL))
			d.failures[failureKey] = failure
			d.mu.Unlock()
			log.WithField("auth_id", auth.ID).WithError(err).Warn("gemini-enterprise model refresh failed; retaining this run's last successful catalog")
			return
		}
		catalog := enterpriseCatalog{models: models, known: true, revision: revision, expires: d.now().Add(enterpriseCatalogTTL), credential: failureKey}
		d.catalogs[key] = catalog
		delete(d.failures, failureKey)
		d.mu.Unlock()
		s.coreManager.PublishEnterprisePolicyCatalog(auth, models)
		s.publishEnterpriseCatalog(probeCtx, auth, key, catalog, epoch)
	}()
}

func (s *Service) cancelEnterpriseProbes(id string) {
	d := s.enterpriseState()
	d.mu.Lock()
	defer d.mu.Unlock()
	for authID, probe := range d.pending {
		if id == "" || id == authID {
			probe.cancel()
			delete(d.pending, authID)
		}
	}
	for authID, probe := range d.subscriptionPending {
		if id == "" || id == authID {
			probe.cancel()
			delete(d.subscriptionPending, authID)
		}
	}
}

func (s *Service) stopEnterpriseDiscovery(ctx context.Context) {
	d := s.enterpriseState()
	d.mu.Lock()
	d.closed = true
	for _, probe := range d.pending {
		probe.cancel()
	}
	for _, probe := range d.subscriptionPending {
		probe.cancel()
	}
	d.mu.Unlock()
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *Service) refreshEnterpriseModels(ctx context.Context) {
	if s.coreManager == nil || s.antigravityHomeEnabled() {
		return
	}
	d := s.enterpriseState()
	for _, auth := range s.coreManager.List() {
		if auth == nil || coreauth.IsEnterpriseAccountAuth(auth) || auth.Provider != ge.Provider {
			continue
		}
		s.queueEnterpriseSubscription(ctx, auth)
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		key := s.enterpriseKey(auth)
		d.mu.Lock()
		catalog := d.catalogs[key]
		due := !catalog.known || !d.now().Before(catalog.expires) || catalog.credential != enterpriseFailureKey(key, auth)
		d.mu.Unlock()
		epoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)
		if catalog.known && (catalog.appliedEpoch != epoch || catalog.appliedRevision != catalog.revision || catalog.appliedPolicy != s.coreManager.EnterpriseModelPolicySignature(auth)) {
			s.publishEnterpriseCatalog(ctx, auth, key, catalog, epoch)
		}
		if due {
			s.queueEnterpriseDiscovery(ctx, auth)
		}
	}
}

func (s *Service) runEnterpriseModelRefresh(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	defer s.cancelEnterpriseProbes("")
	s.refreshEnterpriseModels(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshEnterpriseModels(ctx)
		}
	}
}
