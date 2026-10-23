package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/oauth2"
)

func enterpriseTestService(t *testing.T, id string) (*Service, *coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: id, Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "project", EngineID: "engine"}, "test@example.com", &oauth2.Token{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)})})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: &config.Config{}, coreManager: manager, antigravityContext: t.Context()}
	t.Cleanup(func() {
		svc.cancelEnterpriseProbes("")
		svc.enterpriseState().wg.Wait()
		GlobalModelRegistry().UnregisterClient(id)
	})
	return svc, auth
}

func TestEnterpriseCatalogLifecycle(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-catalog")
	d := svc.enterpriseState()
	now := time.Now()
	d.now = func() time.Time { return now }
	var calls int
	models := []ge.Model{{ID: "gemini-4-argon", DisplayName: "Argon"}, {ID: "virtual-future", DisplayName: "Future"}}
	var fetchErr error
	d.fetch = func(ctx context.Context, _ *coreauth.Auth, _ string) ([]ge.Model, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("discovery inherited a deadline")
		}
		calls++
		return models, fetchErr
	}
	svc.registerModelsForAuth(context.Background(), auth)
	d.wg.Wait()
	if got := GlobalModelRegistry().GetModelsForClient(auth.ID); len(got) != 2 {
		t.Fatalf("models=%v", got)
	}
	svc.refreshEnterpriseModels(context.Background())
	d.wg.Wait()
	if calls != 1 {
		t.Fatal("fresh catalog fetched again")
	}
	now = now.Add(enterpriseCatalogTTL + time.Second)
	fetchErr = errors.New("transport failed")
	svc.refreshEnterpriseModels(context.Background())
	d.wg.Wait()
	if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 2 {
		t.Fatal("failed refresh erased last success")
	}
	svc.refreshEnterpriseModels(context.Background())
	d.wg.Wait()
	if calls != 2 {
		t.Fatal("failure backoff ignored")
	}
	now = now.Add(enterpriseCatalogTTL)
	fetchErr = nil
	models = []ge.Model{{ID: "gemini-4-argon", DisplayName: "Argon"}}
	svc.refreshEnterpriseModels(context.Background())
	d.wg.Wait()
	if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 1 {
		t.Fatal("new catalog did not remove revoked model")
	}
	now = now.Add(enterpriseCatalogTTL)
	models = []ge.Model{}
	svc.refreshEnterpriseModels(context.Background())
	d.wg.Wait()
	if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 0 {
		t.Fatal("successful empty catalog not authoritative")
	}
}

func TestEnterpriseRebindingRevokesOldCatalogBeforeDiscovery(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-rebinding")
	d := svc.enterpriseState()
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return []ge.Model{{ID: "old-resource-only"}}, nil
	}
	svc.registerEnterpriseModels(t.Context(), auth)
	d.wg.Wait()
	if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 1 {
		t.Fatal("initial catalog missing")
	}
	updated := auth.Clone()
	updated.Metadata["engine_id"] = "other-engine"
	updated, err := svc.coreManager.Update(t.Context(), updated)
	if err != nil {
		t.Fatal(err)
	}
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return nil, errors.New("new resource unavailable")
	}
	svc.registerEnterpriseModels(t.Context(), updated)
	if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 0 {
		t.Fatal("rebound account retained the old resource's models")
	}
	d.wg.Wait()
	if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 0 {
		t.Fatal("first failed discovery restored the old resource's catalog")
	}
}

func TestEnterpriseCatalogPreservesCooldownAndFencesRevision(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-cooldown")
	const model = "gemini-4-argon"
	retry := time.Now().Add(time.Hour)
	auth.ModelStates = map[string]*coreauth.ModelState{
		model: {Unavailable: true, NextRetryAfter: retry, Status: coreauth.StatusError,
			Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: retry}},
	}
	auth, err := svc.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	svc.reconcileRegisteredModelStates(t.Context(), auth)
	current, _ := svc.coreManager.GetByID(auth.ID)
	if current.ModelStates[model] == nil || !current.ModelStates[model].NextRetryAfter.Equal(retry) {
		t.Fatal("an empty registry erased persisted cooldown before discovery")
	}
	d := svc.enterpriseState()
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return []ge.Model{{ID: model}}, nil
	}
	svc.registerEnterpriseModels(t.Context(), auth)
	d.wg.Wait()
	key := svc.enterpriseKey(auth)
	d.mu.Lock()
	catalog := d.catalogs[key]
	newer := catalog
	newer.revision++
	d.catalogs[key] = newer
	d.mu.Unlock()
	epoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)
	if svc.publishEnterpriseCatalog(t.Context(), auth, key, catalog, epoch) {
		t.Fatal("a stale probe revision was published")
	}
	if !svc.publishEnterpriseCatalog(t.Context(), auth, key, newer, epoch) {
		t.Fatal("the current catalog revision failed publication")
	}
	current, _ = svc.coreManager.GetByID(auth.ID)
	if current.ModelStates[model] == nil || !current.ModelStates[model].NextRetryAfter.Equal(retry) || !registry.GetGlobalRegistry().IsModelQuotaExceededForClient(auth.ID, model) {
		t.Fatal("catalog replacement cleared existing quota/cooldown state")
	}
}

func TestEnterpriseDiscoverySingleflightAndTokenFence(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-token-fence")
	d := svc.enterpriseState()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	d.fetch = func(_ context.Context, a *coreauth.Auth, _ string) ([]ge.Model, error) {
		calls.Add(1)
		if ge.String(a.Metadata, "access_token") == "old" {
			close(started)
			<-release
			return []ge.Model{{ID: "old"}}, nil
		}
		return []ge.Model{{ID: "new"}}, nil
	}
	svc.queueEnterpriseDiscovery(context.Background(), auth)
	<-started
	for range 12 {
		svc.queueEnterpriseDiscovery(context.Background(), auth)
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new"
	updated, err := svc.coreManager.Update(t.Context(), updated)
	if err != nil {
		t.Fatal(err)
	}
	svc.queueEnterpriseDiscovery(context.Background(), updated)
	close(release)
	d.wg.Wait()
	models := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if calls.Load() != 2 || len(models) != 1 || models[0].ID != "new" {
		t.Fatalf("calls=%d models=%v", calls.Load(), models)
	}
	if svc.enterpriseKey(updated) != svc.enterpriseKey(auth) {
		t.Fatal("token rotation changed catalog identity")
	}
	if svc.enterpriseState().catalogs[svc.enterpriseKey(auth)].models[0].ID != "new" {
		t.Fatal("old result overwrote newer catalog")
	}
	svc.queueEnterpriseDiscovery(context.Background(), auth)
	d.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("a stale auth snapshot restarted an obsolete token probe")
	}
}

func TestEnterpriseDiscoveryShutdownAndHomeAuthority(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-lifecycle")
	started, finished := make(chan struct{}), make(chan struct{})
	d := svc.enterpriseState()
	d.fetch = func(ctx context.Context, _ *coreauth.Auth, _ string) ([]ge.Model, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	}
	svc.queueEnterpriseDiscovery(t.Context(), auth)
	<-started
	svc.stopEnterpriseDiscovery(t.Context())
	<-finished
	svc.queueEnterpriseDiscovery(t.Context(), auth)
	d.mu.Lock()
	closed := d.closed
	pending := len(d.pending)
	d.mu.Unlock()
	if !closed || pending != 0 {
		t.Fatal("discovery lifecycle was not stopped")
	}
	GlobalModelRegistry().RegisterClient(auth.ID, ge.Provider, []*ModelInfo{{ID: "home-authoritative"}})
	svc.cfgMu.Lock()
	svc.cfg.Home.Enabled = true
	svc.cfgMu.Unlock()
	svc.registerEnterpriseModels(t.Context(), auth)
	models := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "home-authoritative" {
		t.Fatal("local registration overwrote Home authority")
	}
}

func TestEnterpriseDiscoveryFencesRemovedDisabledAndReboundAccounts(t *testing.T) {
	for _, action := range []string{"remove", "disable", "rebind", "registry-epoch", "home"} {
		t.Run(action, func(t *testing.T) {
			svc, auth := enterpriseTestService(t, "enterprise-fence-"+action)
			d := svc.enterpriseState()
			started, release := make(chan struct{}), make(chan struct{})
			d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
				close(started)
				<-release
				return []ge.Model{{ID: "stale"}}, nil
			}
			svc.queueEnterpriseDiscovery(context.Background(), auth)
			<-started
			switch action {
			case "remove":
				svc.applyCoreAuthRemoval(t.Context(), auth.ID)
			case "disable":
				updated := auth.Clone()
				updated.Disabled = true
				_, _ = svc.coreManager.Update(t.Context(), updated)
				GlobalModelRegistry().UnregisterClient(auth.ID)
			case "rebind":
				updated := auth.Clone()
				updated.Metadata["engine_id"] = "new-engine"
				_, _ = svc.coreManager.Update(t.Context(), updated)
			case "registry-epoch":
				GlobalModelRegistry().UnregisterClient(auth.ID)
			case "home":
				svc.cfgMu.Lock()
				svc.cfg.Home.Enabled = true
				svc.cfgMu.Unlock()
			}
			close(release)
			d.wg.Wait()
			if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 0 {
				t.Fatal("stale result restored models")
			}
		})
	}
}

func TestEnterpriseDiscoveryFiveSlotsAndAccountIsolation(t *testing.T) {
	svc, first := enterpriseTestService(t, "enterprise-slots-0")
	d := svc.enterpriseState()
	started, release := make(chan struct{}, 6), make(chan struct{})
	var active, maximum atomic.Int32
	d.fetch = func(_ context.Context, a *coreauth.Auth, _ string) ([]ge.Model, error) {
		n := active.Add(1)
		for {
			old := maximum.Load()
			if old >= n || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return []ge.Model{{ID: "model-" + a.ID}}, nil
	}
	for i := range 6 {
		a := first.Clone()
		a.ID = fmt.Sprintf("enterprise-slots-%d", i)
		if i > 0 {
			var err error
			a, err = svc.coreManager.Register(t.Context(), a)
			if err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
		svc.queueEnterpriseDiscovery(context.Background(), a)
	}
	for range 5 {
		<-started
	}
	select {
	case <-started:
		t.Fatal("more than five discovery requests active")
	default:
	}
	close(release)
	d.wg.Wait()
	if maximum.Load() != 5 {
		t.Fatalf("maximum=%d", maximum.Load())
	}
	for i := range 6 {
		id := fmt.Sprintf("enterprise-slots-%d", i)
		models := GlobalModelRegistry().GetModelsForClient(id)
		if len(models) != 1 || models[0].ID != "model-"+id {
			t.Fatal("borrowed another account's models")
		}
	}
}

func TestEnterpriseCatalogUsesHostAliasesExclusionsPrefixes(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-host-settings")
	svc.cfg.OAuthExcludedModels = map[string][]string{ge.Provider: {"hidden"}}
	svc.cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{ge.Provider: {{Name: "gemini-4-argon", Alias: "client-name"}}}
	svc.cfg.OAuthSettings = map[string][]config.OAuthModelSetting{ge.Provider: {{Name: "client-name", MaxContextLength: 12345}}}
	auth.Prefix = "explicit"
	auth, _ = svc.coreManager.Update(t.Context(), auth)
	svc.enterpriseState().fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return []ge.Model{{ID: "gemini-4-argon"}, {ID: "hidden"}}, nil
	}
	svc.registerEnterpriseModels(t.Context(), auth)
	svc.enterpriseState().wg.Wait()
	models := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if len(models) != 2 {
		t.Fatalf("models=%v", models)
	}
	for _, m := range models {
		if m.ID != "client-name" && m.ID != "explicit/client-name" {
			t.Fatalf("unexpected model %s", m.ID)
		}
		if m.MaxContextLength != 12345 {
			t.Fatal("host model settings lost")
		}
		if len(m.SupportedGenerationMethods) != 2 {
			t.Fatal("unexpected countTokens advertisement")
		}
	}
}
