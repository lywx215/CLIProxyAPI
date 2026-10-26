package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseAutomaticSubscriptionFiltersCatalogAndAliases(t *testing.T) {
	s, auth := enterpriseTestService(t, "subscription-catalog")
	auth.Metadata["model_policy"] = "auto"
	if auth.Attributes == nil {
		auth.Attributes = map[string]string{}
	}
	auth.Attributes["model_aliases"] = `[{"name":"gemini-4-argon","alias":"argon-enterprise","fork":true}]`
	auth, err := s.coreManager.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	d := s.enterpriseState()
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return []ge.Model{{ID: ge.ArgonModel}, {ID: "gemini-3.8-flash"}, {ID: "auto", DisplayName: "Auto"}}, nil
	}
	edition := "business_standard"
	d.fetchSubscription = func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation {
		trial := true
		return ge.SubscriptionObservation{Status: "ok", Edition: edition, FreeTrial: &trial}
	}
	s.registerEnterpriseModels(t.Context(), auth)
	d.wg.Wait()
	models := GlobalModelRegistry().GetModelsForClient(auth.ID)
	if len(models) != 2 {
		t.Fatalf("Business models=%v", models)
	}
	for _, model := range models {
		if model.ID != ge.ArgonModel && model.ID != "argon-enterprise" {
			t.Fatalf("forbidden model published: %s", model.ID)
		}
	}
	edition = "enterprise_plus"
	s.coreManager.InvalidateEnterpriseSubscription(auth)
	s.refreshEnterpriseModels(t.Context())
	d.wg.Wait()
	models = GlobalModelRegistry().GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "gemini-3.8-flash" {
		t.Fatalf("Plus policy did not replace models: %v", models)
	}
	view := s.coreManager.EnterpriseModelPolicyView(auth)
	if len(view.CatalogModels) != 3 {
		t.Fatal("policy erased the upstream catalog")
	}
}

func TestEnterpriseManualPolicyStillPublishesBackgroundQuotaIdentity(t *testing.T) {
	s, auth := enterpriseTestService(t, "manual-subscription-quota")
	d := s.enterpriseState()
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return []ge.Model{{ID: ge.ArgonModel}, {ID: "gemini-3.8-flash"}}, nil
	}
	d.fetchSubscription = func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation {
		trial := true
		return ge.SubscriptionObservation{Status: "ok", Edition: "business_standard", FreeTrial: &trial, ProjectNumber: "123", PrincipalHash: strings.Repeat("a", 64)}
	}
	s.registerEnterpriseModels(t.Context(), auth)
	d.wg.Wait()
	current, _ := s.coreManager.GetByID(auth.ID)
	if !current.EnterpriseQuotaIdentity.ValidFor(ge.FromMetadata(current.Metadata)) {
		t.Fatal("manual policy lost background quota attribution")
	}
	view := s.coreManager.EnterpriseModelPolicyView(current)
	if view.Mode != "catalog" || !view.Allows("gemini-3.8-flash") || len(GlobalModelRegistry().GetModelsForClient(auth.ID)) != 2 {
		t.Fatalf("subscription lookup overrode the manual policy: %+v", view)
	}
}

func TestEnterpriseSubscriptionAndCatalogShareFiveDiscoverySlots(t *testing.T) {
	s, original := enterpriseTestService(t, "shared-subscription-slots-0")
	d := s.enterpriseState()
	started, release := make(chan struct{}, 32), make(chan struct{})
	releaseFetch := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseFetch)
	var active, maximum atomic.Int32
	fetch := func() {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if previous >= current || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
	}
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		fetch()
		return []ge.Model{{ID: ge.ArgonModel}}, nil
	}
	d.fetchSubscription = func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation {
		fetch()
		return ge.SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable"}
	}
	for index := range 6 {
		auth := original.Clone()
		auth.ID = fmt.Sprintf("shared-subscription-slots-%d", index)
		auth.Metadata["project_id"] = fmt.Sprintf("slot-project-%d", index)
		var err error
		auth, err = s.coreManager.Register(t.Context(), auth)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
		if index < 3 {
			s.queueEnterpriseDiscovery(t.Context(), auth)
		} else {
			s.queueEnterpriseSubscription(t.Context(), auth)
		}
	}
	for range 5 {
		<-started
	}
	select {
	case <-started:
		t.Fatal("subscription and catalog exceeded the shared discovery limit")
	default:
	}
	releaseFetch()
	d.wg.Wait()
	if maximum.Load() != 5 {
		t.Fatalf("maximum concurrent discovery operations=%d", maximum.Load())
	}
}

func TestEnterpriseSubscriptionReplacementCancelsObsoleteProbe(t *testing.T) {
	s, original := enterpriseTestService(t, "subscription-replacement")
	d := s.enterpriseState()
	started, canceled := make(chan struct{}), make(chan struct{})
	d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) {
		return []ge.Model{{ID: ge.ArgonModel}}, nil
	}
	d.fetchSubscription = func(ctx context.Context, auth *coreauth.Auth, _ string) ge.SubscriptionObservation {
		if ge.String(auth.Metadata, "refresh_token") == "refresh" {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ge.SubscriptionObservation{Status: "unassigned", Reason: "license_unassigned"}
		}
		trial := true
		return ge.SubscriptionObservation{Status: "ok", Edition: "business_standard", FreeTrial: &trial, ProjectNumber: "123", PrincipalHash: strings.Repeat("a", 64)}
	}
	s.queueEnterpriseSubscription(t.Context(), original)
	<-started
	replacement := original.Clone()
	replacement.Metadata["refresh_token"], replacement.Metadata["access_token"] = "replacement", "replacement-access"
	replacement, err := s.coreManager.Update(t.Context(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	s.queueEnterpriseSubscription(t.Context(), replacement)
	d.wg.Wait()
	select {
	case <-canceled:
	default:
		t.Fatal("obsolete subscription probe retained a discovery slot")
	}
	if got := s.coreManager.EnterpriseSubscriptionSnapshot(replacement); got.Status != "ok" {
		t.Fatalf("obsolete authorization overwrote the new subscription: %+v", got)
	}
	if got := s.coreManager.EnterpriseSubscriptionSnapshot(original); got.Status != "unknown" {
		t.Fatalf("subscription crossed an authorization replacement: %+v", got)
	}
}

func TestEnterpriseDisabledCollectionDoesNotQueueSubscription(t *testing.T) {
	s, auth := enterpriseTestService(t, "disabled-subscription")
	var calls atomic.Int32
	s.enterpriseState().fetchSubscription = func(context.Context, *coreauth.Auth, string) ge.SubscriptionObservation {
		calls.Add(1)
		return ge.SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable"}
	}
	disabled := auth.Clone()
	disabled.Disabled, disabled.Status = true, coreauth.StatusDisabled
	disabled, err := s.coreManager.Update(t.Context(), disabled)
	if err != nil {
		t.Fatal(err)
	}
	s.queueEnterpriseSubscription(t.Context(), disabled)
	s.queueEnterpriseSubscription(t.Context(), auth)
	s.enterpriseState().wg.Wait()
	if calls.Load() != 0 {
		t.Fatal("disabled collection performed a subscription lookup")
	}
}
