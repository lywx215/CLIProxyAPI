package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// Catalog lifecycle tests use an explicit manual policy; automatic policy has separate fixtures.
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: id, Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "project", EngineID: "engine"}, "test@example.com", &oauth2.Token{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)})})
	if err != nil {
		t.Fatal(err)
	}
	auth.Metadata["model_policy"] = "catalog"
	auth, err = manager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: &config.Config{}, coreManager: manager, antigravityContext: t.Context()}
	svc.enterpriseState().fetchSubscription = func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation {
		trial := true
		return ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial}
	}
	t.Cleanup(func() {
		svc.cancelEnterpriseProbes("")
		svc.enterpriseState().wg.Wait()
		GlobalModelRegistry().UnregisterClient(id)
	})
	return svc, auth
}

func publishEnterpriseTrialFixture(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	trial := true
	flight := manager.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial}) {
		t.Fatal("verified fixture subscription was not published")
	}
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
	publishEnterpriseTrialFixture(t, svc.coreManager, auth)
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
	publishEnterpriseTrialFixture(t, svc.coreManager, first)
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

func TestEnterpriseMediaCatalogAliasesPrefixesAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		model, kind, alias, output string
		methods                    int
	}{
		{"gemini-3-pro-image", "image", "portrait", "IMAGE", 2},
		{"veo-3.1", "video", "movie", "VIDEO", 3},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			svc, auth := enterpriseTestService(t, "enterprise-media-catalog-"+tc.kind)
			svc.cfg.OAuthExcludedModels = map[string][]string{ge.Provider: {"hidden-media-fixture"}}
			svc.cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{ge.Provider: {{Name: tc.model, Alias: tc.alias}}}
			auth.Prefix = "work"
			auth, _ = svc.coreManager.Update(t.Context(), auth)
			svc.enterpriseState().fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
				return []ge.Model{{ID: tc.model, MediaKind: tc.kind}, {ID: "hidden-media-fixture", MediaKind: tc.kind}}, nil
			}
			svc.registerEnterpriseModels(t.Context(), auth)
			svc.enterpriseState().wg.Wait()
			models := GlobalModelRegistry().GetModelsForClient(auth.ID)
			if len(models) != 2 {
				t.Fatalf("models=%v", models)
			}
			for _, m := range models {
				if m.ID != tc.alias && m.ID != "work/"+tc.alias {
					t.Fatalf("incorrect alias: %s", m.ID)
				}
				if m.MetadataModelID != tc.model || registry.EnterpriseMediaKind(m.ID) != tc.kind || len(m.SupportedGenerationMethods) != tc.methods || m.SupportedOutputModalities[0] != tc.output {
					t.Fatal("alias lost media capabilities")
				}
			}
		})
	}
}

func TestEnterpriseImageCatalogPublishesOfficialModelID(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-official-image-catalog")
	svc.enterpriseState().fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return ge.ParseModels([]byte(`{"uiSettings":{"features":{"disable-image-generation":"FEATURE_STATE_OFF"},"modelConfigs":{"gemini-3-pro-image":"MODEL_ENABLED"},"modelConfigInfo":{"resolvedModels":[{"modelId":"text"}]}}}`))
	}
	svc.registerEnterpriseModels(t.Context(), auth)
	svc.enterpriseState().wg.Wait()
	models := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if len(models) != 2 {
		t.Fatalf("catalog=%v", models)
	}
	var image *ModelInfo
	for _, model := range models {
		if model.ID == "gemini-3-pro-image" {
			image = model
		}
	}
	if image == nil || image.Name != "models/gemini-3-pro-image" || image.EnterpriseMedia != "image" || image.OwnedBy != ge.Provider {
		t.Fatalf("official image entry missing or mislabeled: %+v", image)
	}
	if len(image.SupportedInputModalities) != 1 || image.SupportedInputModalities[0] != "TEXT" || len(image.SupportedOutputModalities) != 1 || image.SupportedOutputModalities[0] != "IMAGE" {
		t.Fatal("image input/output capabilities changed")
	}
	if len(image.SupportedGenerationMethods) != 2 || image.SupportedGenerationMethods[0] != "generateContent" || image.SupportedGenerationMethods[1] != "streamGenerateContent" {
		t.Fatal("image model advertised unsupported generation methods")
	}
}

func TestEnterpriseVideoCatalogPublishesDefaultVariantContractAndHonorsExclusions(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		t.Run(fmt.Sprintf("excluded=%t", excluded), func(t *testing.T) {
			svc, auth := enterpriseTestService(t, fmt.Sprintf("enterprise-default-video-catalog-%t", excluded))
			if excluded {
				svc.cfg.OAuthExcludedModels = map[string][]string{ge.Provider: {"veo-3.1"}}
			}
			svc.enterpriseState().fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
				return ge.ParseModels([]byte(`{"uiSettings":{"features":{"disable-video-generation":"FEATURE_STATE_OFF"},"modelConfigInfo":{"resolvedModels":[{"modelId":"text"}]}}}`))
			}
			svc.registerEnterpriseModels(t.Context(), auth)
			svc.enterpriseState().wg.Wait()
			models := GlobalModelRegistry().GetModelsForClient(auth.ID)
			if excluded {
				if len(models) != 1 || models[0].ID != "text" {
					t.Fatalf("excluded video entry was published: %+v", models)
				}
				return
			}
			if len(models) != 2 {
				t.Fatalf("catalog=%v", models)
			}
			var video *ModelInfo
			for _, model := range models {
				if model.ID == "veo-3.1" {
					video = model
				}
			}
			if video == nil || video.Name != "models/veo-3.1" || video.EnterpriseMedia != "video" || video.OwnedBy != ge.Provider || video.DisplayName != "Veo 3.1 (Enterprise default variant)" {
				t.Fatalf("default video tool entry missing or mislabeled: %+v", video)
			}
			if len(video.SupportedInputModalities) != 1 || video.SupportedInputModalities[0] != "TEXT" || len(video.SupportedOutputModalities) != 1 || video.SupportedOutputModalities[0] != "VIDEO" {
				t.Fatal("video input/output capabilities changed")
			}
			if len(video.SupportedGenerationMethods) != 3 || video.SupportedGenerationMethods[0] != "generateContent" || video.SupportedGenerationMethods[1] != "streamGenerateContent" || video.SupportedGenerationMethods[2] != "predictLongRunning" {
				t.Fatal("video entry lost its native generation methods")
			}
			if !strings.Contains(video.Description, "Enterprise selects the underlying variant") || !strings.Contains(video.Description, "no publisher model ID is pinned") {
				t.Fatal("video metadata implies a pinned upstream variant")
			}
			var native map[string]any
			for _, entry := range GlobalModelRegistry().GetAvailableModels("gemini") {
				if entry["name"] == "models/veo-3.1" {
					native = entry
				}
			}
			if native == nil || native["description"] != video.Description {
				t.Fatal("native model metadata lost its default variant description")
			}
			methods, ok := native["supportedGenerationMethods"].([]string)
			if !ok || len(methods) != 3 || methods[2] != "predictLongRunning" {
				t.Fatal("native model metadata omitted predictLongRunning")
			}
		})
	}
}

func TestEnterpriseThinkingCatalogPreservesCapabilitiesThroughAliases(t *testing.T) {
	svc, auth := enterpriseTestService(t, "enterprise-thinking-catalog")
	svc.cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{ge.Provider: {
		{Name: "gemini-3.1-pro-preview", Alias: "pro-enterprise"},
		{Name: "gemini-3.8-flash", Alias: "flash-enterprise"},
	}}
	auth.Prefix = "tenant"
	var err error
	auth, err = svc.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	svc.enterpriseState().fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return []ge.Model{{ID: "gemini-3.1-pro-preview"}, {ID: "gemini-3.8-flash"}, {ID: "future-unverified"}}, nil
	}
	svc.registerEnterpriseModels(t.Context(), auth)
	svc.enterpriseState().wg.Wait()
	models := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if len(models) != 6 {
		t.Fatalf("unexpected catalog: %+v", models)
	}
	seen := make(map[string]bool)
	for _, model := range models {
		seen[model.ID] = true
		base := strings.TrimPrefix(model.ID, "tenant/")
		switch base {
		case "pro-enterprise", "flash-enterprise":
			support := model.Thinking
			if support == nil || strings.Join(support.Levels, ",") != "low,high" || support.Min != 0 || support.Max != 0 || support.DynamicAllowed || support.ZeroAllowed {
				t.Fatalf("alias %q lost verified level capabilities or advertised budgets: %+v", model.ID, support)
			}
		case "future-unverified":
			if model.Thinking != nil {
				t.Fatalf("unverified model %q advertised thinking controls", model.ID)
			}
		default:
			t.Fatalf("unexpected model %q", model.ID)
		}
	}
	for _, id := range []string{"pro-enterprise", "flash-enterprise", "future-unverified"} {
		if !seen[id] || !seen["tenant/"+id] {
			t.Fatalf("missing alias or prefix for %q", id)
		}
	}
}
