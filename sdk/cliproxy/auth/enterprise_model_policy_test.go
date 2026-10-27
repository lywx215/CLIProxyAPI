package auth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func policyFixture(t *testing.T) (*Manager, *Auth, *time.Time) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	m.enterprisePolicies.nowFunc = func() time.Time { return now }
	auth := regionalTestAuth()
	auth.Metadata["model_policy"] = "auto"
	auth, err := m.Register(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	m.PublishEnterprisePolicyCatalog(auth, []ge.Model{{ID: ge.ArgonModel}, {ID: "gemini-3.8-flash"}, {ID: "auto"}, {ID: "gemini-3-pro-image", MediaKind: "image"}})
	return m, auth, &now
}

func publishSubscription(t *testing.T, m *Manager, auth *Auth, edition string, trial bool) {
	t.Helper()
	m.InvalidateEnterpriseSubscription(auth)
	flight := m.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: edition, FreeTrial: &trial, State: "ACTIVE", CheckedAt: m.enterprisePolicies.now()}) {
		t.Fatal("subscription completion failed")
	}
}

// Runtime observations keep unrelated fixtures independent of metadata imports and Google.
func verifyEnterpriseTrialFixture(t *testing.T, m *Manager, auth *Auth, models ...string) {
	t.Helper()
	publishSubscription(t, m, auth, "enterprise_standard", true)
	if models != nil {
		catalog := make([]ge.Model, 0, len(models))
		for _, model := range models {
			catalog = append(catalog, ge.Model{ID: model})
		}
		m.PublishEnterprisePolicyCatalog(auth, catalog)
	}
}

func TestEnterprisePolicyCacheLifecycleAndRevocation(t *testing.T) {
	m, auth, now := policyFixture(t)
	if m.EnterpriseModelPolicyView(auth).Status != "blocked" {
		t.Fatal("first unknown subscription was admitted")
	}
	publishSubscription(t, m, auth, "business_standard", true)
	if got := m.EnterpriseModelPolicyView(auth); len(got.EffectiveModels) != 1 || got.EffectiveModels[0] != ge.ArgonModel {
		t.Fatalf("business policy=%+v", got)
	}
	if m.BeginEnterpriseSubscriptionRefresh(auth) != nil {
		t.Fatal("fresh cache refetched")
	}
	*now = now.Add(enterpriseSubscriptionTTL)
	flight := m.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil || m.BeginEnterpriseSubscriptionRefresh(auth) != nil {
		t.Fatal("single flight failed")
	}
	flight.Complete(ge.SubscriptionObservation{Status: "unknown", Reason: "permission_denied", CheckedAt: *now})
	if got := m.EnterpriseModelPolicyView(auth); got.Status != "stale" || !got.Allows(ge.ArgonModel) {
		t.Fatalf("lost bounded stale policy: %+v", got)
	}
	if m.BeginEnterpriseSubscriptionRefresh(auth) != nil {
		t.Fatal("failure backoff bypassed")
	}
	*now = now.Add(25 * time.Minute)
	if got := m.EnterpriseModelPolicyView(auth); got.Status != "blocked" || got.Reason != "subscription_cache_expired" {
		t.Fatalf("stale cache did not expire: %+v", got)
	}
	publishSubscription(t, m, auth, "enterprise_plus", true)
	if got := m.EnterpriseModelPolicyView(auth); got.Allows(ge.ArgonModel) || !got.Allows("gemini-3.8-flash") {
		t.Fatalf("tier transition lost: %+v", got)
	}
	publishSubscription(t, m, auth, "enterprise_plus", false)
	if !m.EnterpriseModelPolicyView(auth).Allows(ge.ArgonModel) {
		t.Fatal("paid upgrade retained trial restriction")
	}
	m.InvalidateEnterpriseSubscription(auth)
	m.BeginEnterpriseSubscriptionRefresh(auth).Complete(ge.SubscriptionObservation{Status: "unassigned", Reason: "license_unassigned", CheckedAt: *now})
	if m.EnterpriseModelPolicyView(auth).Status != "blocked" {
		t.Fatal("revocation retained old policy")
	}
}

func TestEnterpriseSubscriptionIsolationAndObsoleteCompletion(t *testing.T) {
	m, auth, _ := policyFixture(t)
	flight := m.BeginEnterpriseSubscriptionRefresh(auth)
	rotated := auth.Clone()
	rotated.Metadata["access_token"] = "rotated"
	rotated, err := m.Update(t.Context(), rotated)
	if err != nil {
		t.Fatal(err)
	}
	if EnterpriseSubscriptionKey(auth) != EnterpriseSubscriptionKey(rotated) {
		t.Fatal("access refresh changed subscription identity")
	}
	trial := true
	if flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "business_standard", FreeTrial: &trial}) {
		t.Fatal("stale token completion was accepted")
	}
	publishSubscription(t, m, rotated, "business_standard", true)
	for _, key := range []string{"project_id", "location", "email", "refresh_token"} {
		other := rotated.Clone()
		other.Metadata[key] = "another"
		if m.EnterpriseModelPolicyView(other).Status != "blocked" {
			t.Fatalf("cache crossed %s", key)
		}
	}
	otherApp := rotated.Clone()
	otherApp.Metadata["engine_id"] = "other-app"
	if m.EnterpriseModelPolicyView(otherApp).Subscription.Status != "ok" || m.EnterpriseModelPolicyView(otherApp).CatalogKnown {
		t.Fatal("subscription sharing or application catalog isolation failed")
	}
}

func TestEnterprisePolicyFinalGateUsesLiveSettingsAndDoesNotCoolAuth(t *testing.T) {
	m, auth, _ := policyFixture(t)
	publishSubscription(t, m, auth, "business_standard", true)
	if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.ArgonModel+"(high)"); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gemini-3.8-flash", "auto", "gemini-3-pro-image", "invented-model"} {
		err := ValidateEnterpriseModelRequest(t.Context(), auth, model)
		var denied *EnterpriseModelPolicyError
		if !errors.As(err, &denied) || !isRequestScopedError(err) || !isEnterpriseModelAvailabilityError(err) {
			t.Fatalf("unsafe gate error for %s: %v", model, err)
		}
	}
	changed := auth.Clone()
	changed.Metadata["model_policy"] = "exclude_argon"
	if _, err := m.Update(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.ArgonModel); err == nil {
		t.Fatal("old auth snapshot bypassed live policy")
	}
	current, _ := m.GetByID(auth.ID)
	if current.Disabled || current.Unavailable || current.Quota.Exceeded {
		t.Fatal("policy denial cooled or disabled the account")
	}
	if err := ValidateEnterpriseModelRequest(t.Context(), current, "gemini-3.8-flash"); err != nil {
		t.Fatal(err)
	}
}

// Explicit administrative preparation remains available; generation never invokes it.
type policyPreparationExecutor struct {
	ProviderExecutor
	calls int
}

func (e *policyPreparationExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }
func (e *policyPreparationExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.calls++
	return auth, nil
}

func TestEnterpriseGenerationDoesNotSynchronouslyResolveSubscription(t *testing.T) {
	m, auth, _ := policyFixture(t)
	executor := &policyPreparationExecutor{}
	if _, err := m.prepareRequestAuth(t.Context(), executor, auth); err != nil || executor.calls != 0 {
		t.Fatalf("generation performed a subscription lookup: calls=%d err=%v", executor.calls, err)
	}
}

func TestEnterpriseSubscriptionSharesCanonicalProjectNumber(t *testing.T) {
	m, numeric, _ := policyFixture(t)
	legacy := numeric.Clone()
	legacy.ID, legacy.FileName = "legacy-project.json", "legacy-project.json"
	legacy.Metadata["project_id"] = "legacy-project"
	legacy, err := m.Register(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	trial := true
	flight := m.BeginEnterpriseSubscriptionRefresh(legacy)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial, ProjectNumber: "123", PrincipalHash: strings.Repeat("a", 64)}) {
		t.Fatal("legacy observation failed")
	}
	if m.EnterpriseSubscriptionSnapshot(numeric).Status != "ok" || m.BeginEnterpriseSubscriptionRefresh(numeric) != nil {
		t.Fatal("project name and number did not share the verified observation")
	}
	m.InvalidateEnterpriseSubscription(numeric)
	if flight = m.BeginEnterpriseSubscriptionRefresh(legacy); flight == nil {
		t.Fatal("canonical invalidation did not affect legacy binding")
	}
	flight.Cancel()
}

func TestEnterpriseSubscriptionCanonicalCompletionFencing(t *testing.T) {
	for _, newerLegacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "numeric-newer", true: "legacy-newer"}[newerLegacy], func(t *testing.T) {
			m, numeric, _ := policyFixture(t)
			legacy := numeric.Clone()
			legacy.ID, legacy.FileName = "legacy-fence.json", "legacy-fence.json"
			legacy.Metadata["project_id"] = "legacy-project"
			legacy, err := m.Register(t.Context(), legacy)
			if err != nil {
				t.Fatal(err)
			}
			first, second := legacy, numeric
			if newerLegacy {
				first, second = numeric, legacy
			}
			older, newer := m.BeginEnterpriseSubscriptionRefresh(first), m.BeginEnterpriseSubscriptionRefresh(second)
			if older == nil || newer == nil {
				t.Fatal("independent uncanonicalized refreshes were not started")
			}
			observation := ge.SubscriptionObservation{Status: "unassigned", Reason: "license_unassigned", ProjectNumber: "123"}
			if !newer.Complete(observation) {
				t.Fatal("newer canonical refresh was rejected")
			}
			trial := true
			if older.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "business_standard", FreeTrial: &trial, ProjectNumber: "123"}) {
				t.Fatal("older refresh restored revoked access")
			}
			for _, auth := range []*Auth{numeric, legacy} {
				if got := m.EnterpriseSubscriptionSnapshot(auth); got.Status != "unassigned" {
					t.Fatalf("canonical revocation lost: %+v", got)
				}
			}
		})
	}
}

func TestEnterpriseSubscriptionCanonicalReadFailureAndAuthoritativeUnknown(t *testing.T) {
	for _, reason := range []string{"subscription_unavailable", "unknown_subscription_tier", "invalid_license_binding"} {
		t.Run(reason, func(t *testing.T) {
			m, numeric, _ := policyFixture(t)
			publishSubscription(t, m, numeric, "business_standard", true)
			legacy := numeric.Clone()
			legacy.ID, legacy.FileName = "legacy-unknown.json", "legacy-unknown.json"
			legacy.Metadata["project_id"] = "legacy-project"
			legacy, err := m.Register(t.Context(), legacy)
			if err != nil {
				t.Fatal(err)
			}
			flight := m.BeginEnterpriseSubscriptionRefresh(legacy)
			if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unknown", Reason: reason, ProjectNumber: "123"}) {
				t.Fatal("canonical unknown refresh failed")
			}
			for _, auth := range []*Auth{numeric, legacy} {
				view := m.EnterpriseModelPolicyView(auth)
				if reason == "subscription_unavailable" {
					if view.Status != "stale" || !view.Allows(ge.ArgonModel) {
						t.Fatalf("transient failure discarded the shared verified observation: %+v", view)
					}
				} else if view.Status != "blocked" || view.Reason != reason || view.Allows(ge.ArgonModel) {
					t.Fatalf("authoritative unknown retained old access: %+v", view)
				}
			}
			manual := numeric.Clone()
			manual.Metadata["model_policy"] = "catalog"
			if got := m.EnterpriseModelPolicyView(manual); got.Allows(ge.ArgonModel) != (reason == "subscription_unavailable") {
				t.Fatalf("manual policy bypassed subscription verification: %+v", got)
			}
		})
	}
}

func TestEnterpriseSubscriptionSnapshotsOwnTrialValue(t *testing.T) {
	m, auth, _ := policyFixture(t)
	trial := true
	flight := m.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "business_standard", FreeTrial: &trial}) {
		t.Fatal("subscription completion failed")
	}
	trial = false
	first := m.EnterpriseSubscriptionSnapshot(auth)
	if first.FreeTrial == nil || !*first.FreeTrial {
		t.Fatal("caller mutated the cached trial value")
	}
	*first.FreeTrial = false
	if next := m.EnterpriseSubscriptionSnapshot(auth); next.FreeTrial == nil || !*next.FreeTrial {
		t.Fatal("snapshot mutated the cached trial value")
	}
}

func TestEnterpriseSubscriptionIdentityInvalidatesAuthoritativeUnknown(t *testing.T) {
	for _, observation := range []ge.SubscriptionObservation{
		{Status: "unknown", Reason: "subscription_unavailable"},
		{Status: "unknown", Reason: "unknown_subscription_tier"},
		{Status: "unknown", Reason: "invalid_license_binding"},
		{Status: "conflict", Reason: "conflicting_subscriptions"},
		{Status: "unassigned", Reason: "license_unassigned"},
		{Status: "inactive", Reason: "subscription_inactive"},
	} {
		t.Run(observation.Reason, func(t *testing.T) {
			m, auth, _ := policyFixture(t)
			flight := m.BeginEnterpriseSubscriptionRefresh(auth)
			trial := true
			if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial, ProjectNumber: "123", PrincipalHash: strings.Repeat("a", 64)}) {
				t.Fatal("verified subscription failed")
			}
			m.PublishEnterpriseSubscriptionIdentity(auth)
			current, _ := m.GetByID(auth.ID)
			if !current.EnterpriseQuotaIdentity.ValidFor(ge.FromMetadata(current.Metadata)) {
				t.Fatal("verified subscription did not publish quota attribution")
			}
			m.InvalidateEnterpriseSubscription(auth)
			if flight = m.BeginEnterpriseSubscriptionRefresh(auth); flight == nil || !flight.Complete(observation) {
				t.Fatal("subscription update failed")
			}
			m.PublishEnterpriseSubscriptionIdentity(auth)
			current, _ = m.GetByID(auth.ID)
			if current.EnterpriseQuotaIdentity.Invalidated != observation.Authoritative() {
				t.Fatalf("incorrect stale quota attribution: %+v", current.EnterpriseQuotaIdentity)
			}
		})
	}
}

func TestEnterpriseCustomEmptyWhitelistSurvivesRuntimeProjection(t *testing.T) {
	m, auth, _ := policyFixture(t)
	verifyEnterpriseTrialFixture(t, m, auth)
	auth.Metadata["model_policy"], auth.Metadata["allowed_models"] = "custom", []string{}
	auth, err := m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*Auth{auth, auth.Clone()} {
		if view := m.EnterpriseModelPolicyView(snapshot); view.AllowedModels == nil || view.Allows(ge.ArgonModel) {
			t.Fatalf("empty whitelist became unrestricted: %+v", view)
		}
	}
	registered := auth.Clone()
	registered.ID, registered.FileName = "empty-whitelist.json", "empty-whitelist.json"
	registered, err = m.Register(t.Context(), registered)
	if err != nil {
		t.Fatal(err)
	}
	verifyEnterpriseTrialFixture(t, m, registered)
	if view := m.EnterpriseModelPolicyView(registered); view.AllowedModels == nil || view.Allows(ge.ArgonModel) {
		t.Fatalf("registered empty whitelist became unrestricted: %+v", view)
	}
	account, err := ge.AccountFromLegacy(auth.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	parent := auth.Clone()
	parent.Metadata = ge.PutAccount(parent.Metadata, account)
	projected, err := ProjectEnterpriseAuth(parent)
	if err != nil || len(projected) != 2 {
		t.Fatalf("account projection failed: %v", err)
	}
	if policy := ge.ModelPolicyFor(projected[1].Metadata, ge.SubscriptionObservation{}); policy.AllowedModels == nil || policy.Allows(ge.ArgonModel) {
		t.Fatalf("projected empty whitelist became unrestricted: %+v", policy)
	}
}

func TestEnterprisePolicyFinalGateFencesAuthorizationReplacement(t *testing.T) {
	m, original, _ := policyFixture(t)
	original.Metadata["model_policy"] = "catalog"
	original, err := m.Update(t.Context(), original)
	if err != nil {
		t.Fatal(err)
	}
	verifyEnterpriseTrialFixture(t, m, original)
	replacement := original.Clone()
	replacement.Metadata["refresh_token"] = "replacement-authorization"
	if _, err = m.Update(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	var changed *EnterpriseModelProbeError
	if err = ValidateEnterpriseModelRequest(t.Context(), original, ge.ArgonModel); !errors.As(err, &changed) || changed.Code != "credential_changed" {
		t.Fatalf("obsolete authorization was admitted: %v", err)
	}
	if policy := EnterpriseModelPolicyForRequest(original); policy.Status != "blocked" || policy.Reason != "credential_changed" {
		t.Fatalf("operation policy admitted obsolete authorization: %+v", policy)
	}
}

func TestEnterpriseSubscriptionFailureBackoffStartsAtFiveSeconds(t *testing.T) {
	m, auth, now := policyFixture(t)
	flight := m.BeginEnterpriseSubscriptionRefresh(auth)
	for _, delay := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute} {
		if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable"}) {
			t.Fatal("failed refresh did not complete")
		}
		*now = now.Add(delay - time.Nanosecond)
		if m.BeginEnterpriseSubscriptionRefresh(auth) != nil {
			t.Fatalf("refresh retried before %s", delay)
		}
		*now = now.Add(time.Nanosecond)
		flight = m.BeginEnterpriseSubscriptionRefresh(auth)
		if flight == nil {
			t.Fatalf("refresh did not retry after %s", delay)
		}
	}
	flight.Cancel()
}

func TestEnterprisePaidPolicyViewReasonsSignatureAndFinalAdmission(t *testing.T) {
	m, auth, _ := policyFixture(t)
	auth.Metadata["model_policy"] = "catalog"
	auth, err := m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	catalog := []ge.Model{{ID: ge.ArgonModel}, {ID: "gemini-3-pro-image", MediaKind: "image"}, {ID: "veo-3.1", MediaKind: "video"}, {ID: "gemini-3.8-flash"}, {ID: "auto"}}
	m.PublishEnterprisePolicyCatalog(auth, catalog)
	publishSubscription(t, m, auth, "enterprise_standard", true)
	trialSignature := m.EnterpriseModelPolicySignature(auth)
	publishSubscription(t, m, auth, "enterprise_standard", false)
	view := m.EnterpriseModelPolicyView(auth)
	if !slices.Equal(view.EffectiveModels, []string{ge.ArgonModel, "gemini-3-pro-image", "veo-3.1"}) || view.AllowedModels != nil || view.EnforcedReason != "paid_subscription_restricted" {
		t.Fatalf("incorrect paid effective policy: %+v", view)
	}
	if trialSignature == m.EnterpriseModelPolicySignature(auth) {
		t.Fatal("paid enforcement did not change the catalog publication signature")
	}
	for _, entry := range view.CatalogModels {
		if !entry.Allowed && entry.Reason != "paid_subscription_restricted" {
			t.Fatalf("paid rejection lost its hard restriction reason: %+v", entry)
		}
	}
	if err = ValidateEnterpriseModelRequest(t.Context(), auth, ge.ArgonModel+"(high)"); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gemini-3.8-flash", "auto", "customer-argon-alias"} {
		var denied *EnterpriseModelPolicyError
		err = ValidateEnterpriseModelRequest(t.Context(), auth, model)
		if !errors.As(err, &denied) || denied.Code != "model_not_allowed" || denied.HTTPStatus != 400 || !strings.Contains(denied.Message, "Paid Enterprise subscriptions") || !isRequestScopedError(err) {
			t.Fatalf("paid final admission for %s: %v", model, err)
		}
	}
	updated := auth.Clone()
	updated.Metadata["excluded_models"] = []string{"gemini-3-pro-image"}
	updated, err = m.Update(t.Context(), updated)
	if err != nil {
		t.Fatal(err)
	}
	view = m.EnterpriseModelPolicyView(updated)
	for _, entry := range view.CatalogModels {
		if entry.ID == "gemini-3-pro-image" && (entry.Allowed || entry.Reason != "model_not_allowed") {
			t.Fatalf("user restriction was attributed to the paid guard: %+v", entry)
		}
	}
	current, _ := m.GetByID(auth.ID)
	if current.Unavailable || current.Disabled || current.Quota.Exceeded || len(current.ModelStates) != 0 {
		t.Fatal("paid policy denial changed credential health")
	}
}

func TestEnterpriseEveryModelPolicyRequiresBoundedVerifiedSubscription(t *testing.T) {
	for _, mode := range []string{"auto", "catalog", "argon_only", "exclude_argon", "custom"} {
		t.Run(mode, func(t *testing.T) {
			m, auth, now := policyFixture(t)
			auth.Metadata["model_policy"] = mode
			auth.Metadata["allowed_models"] = nil
			auth.Metadata["subscription"] = map[string]any{"status": "ok", "free_trial": true}
			auth.Metadata["free_trial"] = true
			auth, err := m.Update(t.Context(), auth)
			if err != nil {
				t.Fatal(err)
			}
			assertBlocked := func(reason string) {
				t.Helper()
				view := m.EnterpriseModelPolicyView(auth)
				if view.Status != "blocked" || view.Reason != reason || view.EnforcedAllowedModels == nil || len(view.EnforcedAllowedModels) != 0 || view.EnforcedReason != reason || len(view.EffectiveModels) != 0 {
					t.Fatalf("unverified policy was admitted: %+v", view)
				}
				var denied *EnterpriseModelPolicyError
				if err := ValidateEnterpriseModelRequest(t.Context(), auth, ge.ArgonModel); !errors.As(err, &denied) || denied.Code != "subscription_unverified" || denied.HTTPStatus != 503 {
					t.Fatalf("unverified final admission: %v", err)
				}
				encoded, errJSON := json.Marshal(view)
				if errJSON != nil || !strings.Contains(string(encoded), `"enforced_allowed_models":[]`) {
					t.Fatalf("blocked enforcement was not serialized as an empty array: %s %v", encoded, errJSON)
				}
			}
			assertBlocked("subscription_unverified")
			publishSubscription(t, m, auth, "enterprise_plus", false)
			*now = now.Add(enterpriseSubscriptionTTL)
			flight := m.BeginEnterpriseSubscriptionRefresh(auth)
			if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unknown", Reason: "subscription_unavailable"}) {
				t.Fatal("temporary refresh failure did not complete")
			}
			if view := m.EnterpriseModelPolicyView(auth); view.Status != "stale" || view.EnforcedReason != "paid_subscription_restricted" || view.Allows("gemini-3.8-flash") {
				t.Fatalf("temporary read failure relaxed paid enforcement: %+v", view)
			}
			*now = now.Add(enterpriseSubscriptionMaxStale - enterpriseSubscriptionTTL)
			assertBlocked("subscription_cache_expired")
			publishSubscription(t, m, auth, "enterprise_plus", false)
			m.InvalidateEnterpriseSubscription(auth)
			flight = m.BeginEnterpriseSubscriptionRefresh(auth)
			if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "unassigned", Reason: "license_unassigned"}) {
				t.Fatal("revocation did not complete")
			}
			assertBlocked("license_unassigned")
		})
	}
}

func TestEnterprisePaidGuardSurvivesAccessRefreshAndFencesNewAuthorization(t *testing.T) {
	m, auth, _ := policyFixture(t)
	auth.Metadata["model_policy"] = "catalog"
	auth, err := m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	publishSubscription(t, m, auth, "enterprise_standard", false)
	rotated := auth.Clone()
	rotated.Metadata["access_token"] = "short-lived-rotation"
	rotated, err = m.Update(t.Context(), rotated)
	if err != nil {
		t.Fatal(err)
	}
	if view := m.EnterpriseModelPolicyView(rotated); view.Subscription.Status != "ok" || view.EnforcedReason != "paid_subscription_restricted" || !view.Allows(ge.ArgonModel) || view.Allows("gemini-3.8-flash") {
		t.Fatalf("access refresh lost the verified paid guard: %+v", view)
	}
	reauthorized := rotated.Clone()
	reauthorized.Metadata["refresh_token"] = "new-authorization"
	reauthorized, err = m.Update(t.Context(), reauthorized)
	if err != nil {
		t.Fatal(err)
	}
	if view := m.EnterpriseModelPolicyView(reauthorized); view.Status != "blocked" || view.EnforcedAllowedModels == nil || len(view.EnforcedAllowedModels) != 0 {
		t.Fatalf("new authorization inherited an earlier subscription: %+v", view)
	}
}
