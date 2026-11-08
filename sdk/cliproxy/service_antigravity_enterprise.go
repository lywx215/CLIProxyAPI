package cliproxy

import (
	"context"
	"sync"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const antigravityEnterpriseMetadataWorkers = 3
const antigravityEnterpriseMetadataInterval = ae.QuotaTTL / 2

type antigravityEnterpriseMetadata struct {
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	slots   chan struct{}
	wake    chan struct{}
	pending map[string]bool
	queue   []*coreauth.Auth
}

func (s *Service) antigravityEnterpriseState() *antigravityEnterpriseMetadata {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.antigravityEnterpriseMetadata == nil {
		s.antigravityEnterpriseMetadata = &antigravityEnterpriseMetadata{slots: make(chan struct{}, antigravityEnterpriseMetadataWorkers)}
	}
	return s.antigravityEnterpriseMetadata
}
func (s *Service) registerAntigravityEnterpriseModels(ctx context.Context, a *coreauth.Auth) {
	s.registerAntigravityEnterpriseModelsWithQuotaRefresh(ctx, a, false)
}

func (s *Service) registerAntigravityEnterpriseModelsWithQuotaRefresh(ctx context.Context, a *coreauth.Auth, refreshQuota bool) {
	if s == nil || s.coreManager == nil || !coreauth.IsAntigravityEnterpriseBindingAuth(a) || a.Disabled || s.coreManager.HomeEnabled() {
		return
	}
	s.ensureExecutorsForAuthWithContext(ctx, a, false)
	provider, ok := s.coreManager.Executor(ae.Provider)
	if !ok {
		return
	}
	native, ok := provider.(*executor.AntigravityEnterpriseExecutor)
	if !ok {
		return
	}
	state := s.antigravityEnterpriseState()
	select {
	case state.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-state.slots }()
	registryEpoch := GlobalModelRegistry().ClientRegistrationEpoch(a.ID)
	config, err := native.Catalog(ctx, a, false)
	if _, errQuota := native.Quota(ctx, a, refreshQuota); errQuota != nil && ctx.Err() == nil {
		log.WithFields(log.Fields{"provider": ae.Provider, "auth_id": a.ID}).Warn("could not refresh Antigravity Enterprise quota metadata")
	}
	if err != nil {
		return
	}
	s.applyAntigravityEnterpriseModels(ctx, a, config, registryEpoch)
}

func (s *Service) applyAntigravityEnterpriseModels(ctx context.Context, a *coreauth.Auth, config ae.Config, registryEpoch uint64) {
	if ctx != nil && ctx.Err() != nil {
		return
	}
	current, ok := s.coreManager.GetByID(a.ID)
	if !ok || current.Disabled || current.RegistrationEpoch != a.RegistrationEpoch || coreauth.AntigravityEnterpriseLicense(current) != coreauth.AntigravityEnterpriseLicense(a) {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	native := map[string]*ModelInfo{}
	for _, model := range executor.AntigravityEnterpriseModels(config) {
		native[model.ID] = model
	}
	models := make([]*ModelInfo, 0)
	for _, route := range coreauth.ProjectAntigravityEnterpriseModels(cfg, a, config) {
		model := native[route.Model.ID]
		if model == nil {
			continue
		}
		clone := cloneModelInfoForCatalogRoute(model)
		clone.ID, clone.Name, clone.DisplayName = route.CatalogID, "models/"+route.CatalogID, route.DisplayName
		configured := applyOAuthSettingsForAuth(cfg, ae.Provider, a.AuthKind(), []*ModelInfo{&clone})[0]
		configured.ID = route.ID
		models = append(models, configured)
	}
	modelRegistry := GlobalModelRegistry()
	if replacer, ok := modelRegistry.(interface {
		ReplaceClientModels(string, string, uint64, []*ModelInfo) (uint64, bool)
	}); ok {
		if _, applied := replacer.ReplaceClientModels(a.ID, ae.Provider, registryEpoch, models); !applied {
			return
		}
	} else {
		if modelRegistry.ClientRegistrationEpoch(a.ID) != registryEpoch {
			return
		}
		modelRegistry.RegisterClient(a.ID, ae.Provider, models)
	}
	s.coreManager.ReconcileRegistryModelStates(ctx, a.ID)
	s.coreManager.RefreshSchedulerEntry(a.ID)
}

func (s *Service) queueAntigravityEnterpriseMetadata(ctx context.Context) {
	if ctx.Err() != nil || s.coreManager.HomeEnabled() {
		return
	}
	state := s.antigravityEnterpriseState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return
	}
	if state.pending == nil {
		state.pending = make(map[string]bool)
	}
	for _, auth := range s.coreManager.List() {
		if !coreauth.IsAntigravityEnterpriseBindingAuth(auth) || auth.Disabled || state.pending[auth.ID] {
			continue
		}
		state.pending[auth.ID] = true
		state.queue = append(state.queue, auth)
	}
	for i := 0; i < antigravityEnterpriseMetadataWorkers; i++ {
		select {
		case state.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) antigravityEnterpriseMetadataWorker(ctx context.Context, state *antigravityEnterpriseMetadata) {
	defer state.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		state.mu.Lock()
		var auth *coreauth.Auth
		if len(state.queue) > 0 {
			auth = state.queue[0]
			state.queue[0] = nil
			state.queue = state.queue[1:]
		}
		state.mu.Unlock()
		if auth == nil {
			select {
			case <-ctx.Done():
				return
			case <-state.wake:
				continue
			}
		}
		if latest, ok := s.coreManager.GetByID(auth.ID); ok && !latest.Disabled && latest.RegistrationEpoch == auth.RegistrationEpoch {
			// Refresh before the admission TTL rather than reading the still-fresh
			// cache at every tick and discovering its expiry on a user request.
			s.registerAntigravityEnterpriseModelsWithQuotaRefresh(ctx, latest, true)
		}
		state.mu.Lock()
		delete(state.pending, auth.ID)
		state.mu.Unlock()
	}
}

// startAntigravityEnterpriseMetadata only schedules metadata reads, never generation probes.
func (s *Service) startAntigravityEnterpriseMetadata(ctx context.Context) {
	if s == nil || s.coreManager == nil || s.coreManager.HomeEnabled() {
		return
	}
	state := s.antigravityEnterpriseState()
	state.mu.Lock()
	if state.started {
		state.mu.Unlock()
		return
	}
	state.started = true
	lifetime, cancel := context.WithCancel(ctx)
	state.cancel = cancel
	state.wake = make(chan struct{}, antigravityEnterpriseMetadataWorkers)
	state.wg.Add(antigravityEnterpriseMetadataWorkers + 1)
	state.mu.Unlock()
	for i := 0; i < antigravityEnterpriseMetadataWorkers; i++ {
		go s.antigravityEnterpriseMetadataWorker(lifetime, state)
	}
	go func() {
		defer state.wg.Done()
		ticker := time.NewTicker(antigravityEnterpriseMetadataInterval)
		defer ticker.Stop()
		s.queueAntigravityEnterpriseMetadata(lifetime)
		for {
			select {
			case <-lifetime.Done():
				return
			case <-ticker.C:
				s.queueAntigravityEnterpriseMetadata(lifetime)
			}
		}
	}()
}
func (s *Service) stopAntigravityEnterpriseMetadata(ctx context.Context) {
	s.cfgMu.RLock()
	state := s.antigravityEnterpriseMetadata
	s.cfgMu.RUnlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.closed = true
	state.queue = nil
	state.pending = nil
	if state.cancel != nil {
		state.cancel()
	}
	state.mu.Unlock()
	done := make(chan struct{})
	go func() { state.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
