package auth

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func carbonEvidenceFixture(t *testing.T, legacy bool) (*Manager, *Auth, *Auth, *enterpriseAccountStore) {
	t.Helper()
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	m.SetConfig(&config.Config{})
	root := regionalTestAuth()
	if !legacy {
		root = enterpriseAccountFixture(t, 2)
	}
	root = registerEnterpriseFixture(t, m, root)
	target := root
	if !legacy {
		target = m.EnterpriseCollections(root.ID)[0]
	}
	publishSubscription(t, m, target, "enterprise_plus", false)
	return m, root, target, store
}

func TestEnterpriseCarbonAndHydrogenEvidenceAndSwitchesAreIndependent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "legacy"}[legacy], func(t *testing.T) {
			m, root, target, _ := carbonEvidenceFixture(t, legacy)
			carbon, err := m.BeginEnterpriseCarbonTest(target, 1)
			if err != nil {
				t.Fatal(err)
			}
			hydrogen, err := m.BeginEnterpriseHydrogenTest(target, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, snapshot := range []*EnterpriseCarbonTestSnapshot{carbon, hydrogen} {
				outcome := &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: snapshot.Model.ID, HTTPStatus: 200}}
				if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), snapshot, outcome); err != nil {
					t.Fatalf("overlapping %s proof lost: %v", snapshot.Model.Label, err)
				}
			}
			current, _ := m.GetByID(target.ID)
			updated, err := m.UpdateEnterpriseCarbonEnabled(t.Context(), current, 1, false, "")
			if err != nil {
				t.Fatal(err)
			}
			current = m.EnterpriseCollections(root.ID)[0]
			revision, _ := carbonCollectionRevision(updated, EnterpriseBindingID(current))
			if _, err = m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, revision, true, ""); err != nil {
				t.Fatal(err)
			}
			current, _ = m.GetByID(current.ID)
			carbonSettings, errCarbon := ge.ReadCarbonSettings(current.Metadata)
			hydrogenSettings, errHydrogen := ge.HydrogenAdmissionModel.ReadSettings(current.Metadata)
			if errCarbon != nil || errHydrogen != nil || carbonSettings.Enabled || !hydrogenSettings.Enabled {
				t.Fatalf("switches crossed model boundaries: carbon=%+v hydrogen=%+v errors=%v/%v", carbonSettings, hydrogenSettings, errCarbon, errHydrogen)
			}
			if !m.EnterpriseCarbonEvidenceState(current).Valid || !m.EnterpriseHydrogenEvidenceState(current).Valid || carbonSettings.Evidence.TestID != carbon.TestID || hydrogenSettings.Evidence.TestID != hydrogen.TestID {
				t.Fatal("a model's evidence replaced the other model's evidence")
			}
			if !m.EnterpriseHydrogenAdmissionView(current).Available || !m.EnterpriseModelPolicyView(current).Allows(ge.CarbonModel) {
				t.Fatal("Hydrogen manual admission or Carbon shared policy was lost")
			}
		})
	}
}

type carbonBlockingEvidenceStore struct {
	enterpriseAccountStore
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *carbonBlockingEvidenceStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if s.block.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.enterpriseAccountStore.Save(ctx, auth)
}

func TestCarbonEvidenceConcurrentApplicationsPreserveEachOtherAndLeaseOrdering(t *testing.T) {
	store := &carbonBlockingEvidenceStore{entered: make(chan struct{}), release: make(chan struct{})}
	m := NewManager(store, nil, nil)
	m.SetConfig(&config.Config{})
	root := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 2))
	children := m.EnterpriseCollections(root.ID)
	for _, child := range children {
		publishSubscription(t, m, child, "enterprise_plus", false)
	}
	first, err := m.BeginEnterpriseHydrogenTest(children[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.BeginEnterpriseHydrogenTest(children[1], 1)
	if err != nil {
		t.Fatal(err)
	}
	verified := &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}}
	store.block.Store(true)
	doneA := make(chan error, 1)
	go func() { _, err := m.RecordEnterpriseCarbonTestEvidence(t.Context(), first, verified); doneA <- err }()
	<-store.entered
	// Begin cannot supersede the committing lease while the owner's Store CAS is
	// still pending. A canceled waiter must not start a new model test.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = m.BeginEnterpriseHydrogenTestContext(cancelled, children[0], 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Begin crossed the mutation fence: %v", err)
	}
	doneB := make(chan error, 1)
	go func() { _, err := m.RecordEnterpriseCarbonTestEvidence(t.Context(), second, verified); doneB <- err }()
	close(store.release)
	if err = <-doneA; err != nil {
		t.Fatal(err)
	}
	if err = <-doneB; err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		current, _ := m.GetByID(child.ID)
		if !m.EnterpriseHydrogenEvidenceState(current).Valid {
			t.Fatal("one application's metadata save overwrote another proof")
		}
	}
	owner, _, _ := m.EnterpriseAccountSnapshot(root.ID)
	if len(ge.HydrogenAdmissionModel.ReadEvidenceStore(owner.Metadata)) != 2 {
		t.Fatal("lost app-scoped evidence")
	}
}

func TestCarbonRebindSaveFailureRetainsThePreviouslyPublishedApplication(t *testing.T) {
	m, root, target, store := carbonEvidenceFixture(t, false)
	_, snapshot := recordCarbonVerified(t, m, target)
	current, _ := m.GetByID(target.ID)
	if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 1, true, snapshot.TestID); err != nil {
		t.Fatal(err)
	}
	owner, _, _ := m.EnterpriseAccountSnapshot(root.ID)
	store.failID = root.ID
	_, err := m.UpdateEnterpriseAccount(t.Context(), owner, 0, func(account *ge.Account) error { account.Collections[0].EngineID = "unsaved-rebind"; return nil })
	if err == nil {
		t.Fatal("failed resource save reported success")
	}
	current, _ = m.GetByID(target.ID)
	if !m.EnterpriseHydrogenAdmissionView(current).Available || ge.FromMetadata(current.Metadata).EngineID != "app" {
		t.Fatal("failed save leaked the draft rebind or erased old proof")
	}
}

func TestCarbonProjectionNeverInheritsAnotherApplicationsLegacySwitch(t *testing.T) {
	root := regionalTestAuth()
	root.Metadata[ge.HydrogenEnabledKey] = true
	account, err := ge.AccountFromLegacy(root.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	second := account.Collections[0]
	second.ProjectID = "456"
	second.ID = ge.CollectionID(second.ResourceBinding)
	second.Settings = nil
	account.Collections = append(account.Collections, second)
	root.Metadata = ge.PutAccount(root.Metadata, account)
	projected, err := ProjectEnterpriseAuth(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 3 {
		t.Fatal("missing projected applications")
	}
	firstSettings, _ := ge.HydrogenAdmissionModel.ReadSettings(projected[1].Metadata)
	secondSettings, _ := ge.HydrogenAdmissionModel.ReadSettings(projected[2].Metadata)
	if !firstSettings.Enabled || secondSettings.Enabled || secondSettings.Evidence != nil {
		t.Fatal("new application inherited legacy Carbon enablement")
	}
}

func TestCarbonProjectionAndHotUpdateRejectNonBooleanSettings(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		m, root, target, _ := carbonEvidenceFixture(t, legacy)
		for _, value := range []any{"true", "false", 1, 0, nil} {
			invalid := root.Clone()
			if legacy {
				invalid.Metadata[ge.HydrogenEnabledKey] = value
			} else {
				account, err := ge.ReadAccount(invalid.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				account.Collections[0].Settings = map[string]any{ge.HydrogenEnabledKey: value}
				invalid.Metadata = ge.PutAccount(invalid.Metadata, *account)
			}
			if _, err := ProjectEnterpriseAuth(invalid); err == nil {
				t.Fatalf("projection accepted invalid Carbon setting %T (legacy=%v)", value, legacy)
			}
			if _, err := m.Update(t.Context(), invalid); err == nil {
				t.Fatalf("hot update accepted invalid Carbon setting %T (legacy=%v)", value, legacy)
			}
			current, _ := m.GetByID(target.ID)
			settings, err := ge.HydrogenAdmissionModel.ReadSettings(current.Metadata)
			if err != nil || settings.Enabled {
				t.Fatalf("rejected hot update modified current settings: %#v %v", settings, err)
			}
		}
	}
}

func TestCarbonMergePreservesManualIntentAndScopesDiagnosticProofToItsGrant(t *testing.T) {
	for _, sameGrant := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-grant", false: "replacement-grant"}[sameGrant], func(t *testing.T) {
			m := NewManager(&enterpriseAccountStore{}, nil, nil)
			m.SetConfig(&config.Config{})
			target := enterpriseAccountFixture(t, 1)
			target.ID = "merge-target"
			target.FileName = "merge-target.json"
			target = registerEnterpriseFixture(t, m, target)
			source := regionalTestAuth()
			source.ID = "merge-source"
			source.FileName = "merge-source.json"
			source.Metadata["project_id"] = "456"
			if !sameGrant {
				source.Metadata["refresh_token"] = "independent-authorization"
			}
			source = registerEnterpriseFixture(t, m, source)
			publishSubscription(t, m, source, "enterprise_plus", false)
			_, proof := recordCarbonVerified(t, m, source)
			current, _ := m.GetByID(source.ID)
			if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 1, true, proof.TestID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.MergeEnterpriseAccounts(t.Context(), target.ID, []string{source.ID}); err != nil {
				t.Fatal(err)
			}
			for _, child := range m.EnterpriseCollections(target.ID) {
				if ge.FromMetadata(child.Metadata).ProjectID != "456" {
					continue
				}
				settings, err := ge.HydrogenAdmissionModel.ReadSettings(child.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				if !settings.Enabled || m.EnterpriseHydrogenEvidenceState(child).Valid != sameGrant {
					t.Fatal("merge lost the same-account manual setting or transferred diagnostic proof to another grant")
				}
			}
		})
	}
}

func TestCarbonResourceAliasMigrationResetsOpeningAndOldProof(t *testing.T) {
	m, root, target, _ := carbonEvidenceFixture(t, false)
	_, proof := recordCarbonVerified(t, m, target)
	current, _ := m.GetByID(target.ID)
	if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 1, true, proof.TestID); err != nil {
		t.Fatal(err)
	}
	owner, _, _ := m.EnterpriseAccountSnapshot(root.ID)
	if _, err := m.UpdateEnterpriseAccount(t.Context(), owner, 0, func(account *ge.Account) error {
		collection := &account.Collections[0]
		oldID := collection.ID
		collection.ProjectID = "456"
		collection.ID = ge.CollectionID(collection.ResourceBinding)
		collection.LegacyAliases = append(collection.LegacyAliases, oldID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current = m.EnterpriseCollections(root.ID)[0]
	settings, _ := ge.HydrogenAdmissionModel.ReadSettings(current.Metadata)
	if settings.Enabled || settings.Evidence != nil {
		t.Fatal("renamed resource identity retained prior grant")
	}
}

func recordCarbonVerified(t *testing.T, m *Manager, target *Auth) (*Auth, *EnterpriseCarbonTestSnapshot) {
	t.Helper()
	owner, _, _ := m.EnterpriseAccountSnapshot(target.ID)
	revision, ok := carbonCollectionRevision(owner, EnterpriseBindingID(target))
	if !ok {
		t.Fatal("missing collection")
	}
	snapshot, err := m.BeginEnterpriseHydrogenTest(target, revision)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.RecordEnterpriseCarbonTestEvidence(t.Context(), snapshot, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}})
	if err != nil {
		t.Fatal(err)
	}
	return updated, snapshot
}

func TestCarbonEvidencePersistsWithoutAutoEnableAndSurvivesToggleRestart(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "legacy"}[legacy], func(t *testing.T) {
			m, root, target, store := carbonEvidenceFixture(t, legacy)
			original := target.Clone()
			updated, snapshot := recordCarbonVerified(t, m, target)
			current, _ := m.GetByID(target.ID)
			if state := m.EnterpriseHydrogenEvidenceState(current); !state.Valid || state.Test.TestID != snapshot.TestID {
				t.Fatalf("missing verified evidence: %+v", state)
			}
			settings, _ := ge.HydrogenAdmissionModel.ReadSettings(current.Metadata)
			if settings.Enabled || !reflect.DeepEqual(original.Metadata, target.Metadata) {
				t.Fatal("test enabled the application or mutated its input snapshot")
			}
			revision, _ := carbonCollectionRevision(updated, EnterpriseBindingID(current))
			if revision != 1 {
				t.Fatal("evidence invalidated its own collection edit revision")
			}
			updated, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, revision, true, "")
			if err != nil {
				t.Fatal(err)
			}
			current = m.EnterpriseCollections(root.ID)[0]
			if !m.EnterpriseHydrogenAdmissionView(current).Available {
				t.Fatalf("manual activation unavailable: %+v", m.EnterpriseHydrogenAdmissionView(current))
			}
			revision, _ = carbonCollectionRevision(updated, EnterpriseBindingID(current))
			if revision != 2 {
				t.Fatal("manual activation did not advance collection revision")
			}
			updated, err = m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, revision, false, "")
			if err != nil {
				t.Fatal(err)
			}
			current, _ = m.GetByID(current.ID)
			if m.EnterpriseHydrogenAdmissionView(current).Enabled || !m.EnterpriseHydrogenEvidenceState(current).Valid {
				t.Fatal("manual close destroyed evidence")
			}
			revision, _ = carbonCollectionRevision(updated, EnterpriseBindingID(current))
			if _, err = m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, revision-1, true, snapshot.TestID); err == nil {
				t.Fatal("stale edit revision enabled Carbon")
			}
			if _, err = m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, revision, true, snapshot.TestID); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			saved := store.records[root.ID].Clone()
			store.mu.Unlock()
			restarted := NewManager(nil, nil, nil)
			restarted.SetConfig(&config.Config{})
			if _, err = restarted.Register(t.Context(), saved); err != nil {
				t.Fatal(err)
			}
			restored := restarted.EnterpriseCollections(root.ID)[0]
			if !restarted.EnterpriseHydrogenEvidenceState(restored).Valid {
				t.Fatal("restart invalidated persisted evidence")
			}
			if restarted.EnterpriseHydrogenAdmissionView(restored).Available {
				t.Fatal("restart skipped fresh subscription verification")
			}
			publishSubscription(t, restarted, restored, "enterprise_plus", false)
			if !restarted.EnterpriseHydrogenAdmissionView(restored).Available {
				t.Fatal("verified restart did not restore manual activation")
			}
		})
	}
}

func TestCarbonEvidenceRefreshVersusAuthorizationReplacement(t *testing.T) {
	m, root, target, _ := carbonEvidenceFixture(t, false)
	_, first := recordCarbonVerified(t, m, target)
	current, _ := m.GetByID(target.ID)
	owner, _, _ := m.EnterpriseAccountSnapshot(target.ID)
	if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 1, true, first.TestID); err != nil {
		t.Fatal(err)
	}
	for _, rotation := range []bool{false, true} {
		owner, _, _ = m.EnterpriseAccountSnapshot(target.ID)
		current, _ = m.GetByID(target.ID)
		revision, _ := carbonCollectionRevision(owner, EnterpriseBindingID(current))
		pending, err := m.BeginEnterpriseHydrogenTest(current, revision)
		if err != nil {
			t.Fatal(err)
		}
		refresh := owner.Clone()
		refresh.Metadata["access_token"] = "new-short-lived-access"
		if rotation {
			refresh.Metadata["refresh_token"] = "trusted-rotated-refresh"
		}
		if _, err = m.UpdateRefreshedAuth(t.Context(), owner, refresh); err != nil {
			t.Fatal(err)
		}
		current, _ = m.GetByID(target.ID)
		if !m.EnterpriseHydrogenEvidenceState(current).Valid {
			t.Fatal("trusted OAuth refresh invalidated evidence")
		}
		// Subscription verification belongs to its own bounded runtime cache.
		publishSubscription(t, m, current, "enterprise_plus", false)
		if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}}); err != nil {
			t.Fatalf("OAuth refresh invalidated an in-flight fixed application test: %v", err)
		}
	}
	owner, _, _ = m.EnterpriseAccountSnapshot(root.ID)
	replacement := owner.Clone()
	replacement.Metadata["refresh_token"] = "different-authorization"
	if _, err := m.Update(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(target.ID)
	settings, _ := ge.HydrogenAdmissionModel.ReadSettings(current.Metadata)
	if !settings.Enabled || settings.Evidence != nil {
		t.Fatal("same-account reauthorization lost manual intent or retained old diagnostic proof")
	}
	publishSubscription(t, m, current, "enterprise_plus", false)
	if !m.EnterpriseHydrogenAdmissionView(current).Available {
		t.Fatal("same-account reauthorization required a model test after subscription confirmation")
	}
}

func TestCarbonEvidenceRejectsLateCancelledAndRevisionChangedTests(t *testing.T) {
	m, root, target, _ := carbonEvidenceFixture(t, false)
	first, err := m.BeginEnterpriseHydrogenTest(target, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.BeginEnterpriseHydrogenTest(target, 1)
	if err != nil {
		t.Fatal(err)
	}
	verified := &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}}
	if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), first, verified); err == nil {
		t.Fatal("superseded test wrote evidence")
	}
	m.CancelEnterpriseCarbonTest(first)
	if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), second, verified); err != nil {
		t.Fatal("canceling an old test canceled the newer lease", err)
	}
	current, _ := m.GetByID(target.ID)
	third, err := m.BeginEnterpriseHydrogenTest(current, 1)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error {
		account.Collections[0].Settings["note"] = "unrelated note"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), third, verified); err == nil {
		t.Fatal("late test overwrote a new collection revision")
	}
	current, _ = m.GetByID(target.ID)
	if !m.EnterpriseHydrogenEvidenceState(current).Valid {
		t.Fatal("unrelated edit revoked the existing successful evidence")
	}
	revision, _ := carbonCollectionRevision(updated, EnterpriseBindingID(current))
	fourth, err := m.BeginEnterpriseHydrogenTest(current, revision)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = m.RecordEnterpriseCarbonTestEvidence(cancelled, fourth, verified); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	m.CancelEnterpriseCarbonTest(fourth)
	if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), fourth, verified); err == nil {
		t.Fatal("cancelled test wrote a late success")
	}
}

func TestCarbonEvidenceTemporaryFailuresAndOldRejectionCannotRevokeNewProof(t *testing.T) {
	m, _, target, _ := carbonEvidenceFixture(t, false)
	_, first := recordCarbonVerified(t, m, target)
	old, _ := m.GetByID(target.ID)
	for _, status := range []int{408, 429, 500, 503} {
		current, _ := m.GetByID(target.ID)
		pending, err := m.BeginEnterpriseHydrogenTest(current, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "failed", HTTPStatus: status, Reason: "upstream_unavailable"}}); err != nil {
			t.Fatal(err)
		}
		current, _ = m.GetByID(target.ID)
		if state := m.EnterpriseHydrogenEvidenceState(current); !state.Valid || state.Test.TestID != first.TestID {
			t.Fatal("temporary failure revoked successful evidence")
		}
	}
	current, _ := m.GetByID(target.ID)
	_, second := recordCarbonVerified(t, m, current)
	if err := m.MarkEnterpriseHydrogenNeedsReview(t.Context(), old, "model_not_enabled", 400); err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(target.ID)
	if state := m.EnterpriseHydrogenEvidenceState(current); !state.Valid || state.Test.TestID != second.TestID {
		t.Fatal("old request invalidated fresh proof")
	}
	if err := m.MarkEnterpriseHydrogenNeedsReview(t.Context(), current, "model_not_enabled", 400); err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(target.ID)
	if state := m.EnterpriseHydrogenEvidenceState(current); state.Valid || !state.NeedsReview {
		t.Fatal("current model rejection did not require review")
	}
}

func TestCarbonIncompleteVerificationDoesNotReuseAnOlderSuccessfulTest(t *testing.T) {
	m, _, target, _ := carbonEvidenceFixture(t, false)
	_, _ = recordCarbonVerified(t, m, target)
	current, _ := m.GetByID(target.ID)
	pending, err := m.BeginEnterpriseHydrogenTest(current, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "incomplete", HTTPStatus: 502, Reason: "model_verification_incomplete"}})
	if err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(target.ID)
	if state := m.EnterpriseHydrogenEvidenceState(current); state.Valid || state.Test == nil || state.Test.Status != "incomplete" {
		t.Fatal("incomplete verification retained activation evidence")
	}
}

func TestCarbonEvidenceRebindAndPersistenceFailureDoNotLeakState(t *testing.T) {
	m, root, target, store := carbonEvidenceFixture(t, false)
	_, test := recordCarbonVerified(t, m, target)
	current, _ := m.GetByID(target.ID)
	if _, err := m.UpdateEnterpriseHydrogenEnabled(t.Context(), current, 1, true, test.TestID); err != nil {
		t.Fatal(err)
	}
	owner, _, _ := m.EnterpriseAccountSnapshot(root.ID)
	if _, err := m.UpdateEnterpriseAccount(t.Context(), owner, 0, func(account *ge.Account) error { account.Collections[0].EngineID = "replacement-app"; return nil }); err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(target.ID)
	settings, _ := ge.HydrogenAdmissionModel.ReadSettings(current.Metadata)
	if settings.Enabled || settings.Evidence != nil {
		t.Fatal("new application inherited Carbon state")
	}
	publishSubscription(t, m, current, "enterprise_plus", false)
	owner, _, _ = m.EnterpriseAccountSnapshot(root.ID)
	revision, _ := carbonCollectionRevision(owner, EnterpriseBindingID(current))
	pending, err := m.BeginEnterpriseHydrogenTest(current, revision)
	if err != nil {
		t.Fatal(err)
	}
	store.failID = root.ID
	if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), pending, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.HydrogenModel, HTTPStatus: 200}}); err == nil {
		t.Fatal("failed persistence reported success")
	}
	current, _ = m.GetByID(current.ID)
	if m.EnterpriseHydrogenEvidenceState(current).Valid {
		t.Fatal("unsaved proof was published")
	}
}

func TestCarbonEvidenceFingerprintIgnoresUnrelatedEditsAndBindsPayload(t *testing.T) {
	m, _, target, _ := carbonEvidenceFixture(t, false)
	_, _ = recordCarbonVerified(t, m, target)
	current, _ := m.GetByID(target.ID)
	baseline := m.EnterpriseHydrogenEvidenceFingerprint(current)
	copy := current.Clone()
	copy.Metadata["note"] = "memo"
	copy.Attributes["priority"] = "99"
	copy.Metadata["access_token"] = "rotated"
	if baseline != m.EnterpriseHydrogenEvidenceFingerprint(copy) {
		t.Fatal("unrelated edits changed Carbon proof identity")
	}
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: ge.ArgonModel}}, Params: map[string]any{"query.text": "unrelated"}}}}}
	m.SetConfig(cfg)
	if baseline != m.EnterpriseHydrogenEvidenceFingerprint(current) {
		t.Fatal("unrelated model rule revoked Carbon evidence")
	}
	cfg = &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: ge.HydrogenModel}}, Params: map[string]any{"query.text": "changed"}}}}}
	m.SetConfig(cfg)
	if m.EnterpriseHydrogenEvidenceState(current).Valid {
		t.Fatal("changed Carbon payload retained old verification")
	}
	copy.ProxyURL = "http://127.0.0.1:1234"
	if m.EnterpriseHydrogenEvidenceFingerprint(copy).Configuration == baseline.Configuration {
		t.Fatal("proxy did not bind Carbon evidence")
	}
}
