package cliproxy

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseAccountServiceSyncRegistersAllChildrenAndRemovesAll(t *testing.T) {
	svc, legacy := enterpriseTestService(t, "enterprise-account-service")
	account, err := ge.AccountFromLegacy(legacy.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		other := account.Collections[0]
		other.ProjectID = fmt.Sprintf("%d", 456+i)
		other.ID = ge.CollectionID(other.ResourceBinding)
		account.Collections = append(account.Collections, other)
	}
	parent := legacy.Clone()
	parent.Metadata = ge.PutAccount(parent.Metadata, account)
	var calls atomic.Int32
	d := svc.enterpriseState()
	d.fetch = func(_ context.Context, auth *coreauth.Auth, _ string) ([]ge.Model, error) {
		if !coreauth.IsEnterpriseCollectionAuth(auth) {
			t.Error("catalog discovery received a parent")
		}
		calls.Add(1)
		return []ge.Model{{ID: "gemini-4-argon", DisplayName: "Argon"}}, nil
	}
	if err = svc.runtimeAuthSyncHook()(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	d.wg.Wait()
	children := svc.coreManager.EnterpriseCollections(parent.ID)
	if len(children) != 3 || calls.Load() != 3 || len(GlobalModelRegistry().GetModelsForClient(parent.ID)) != 0 {
		t.Fatalf("children=%d catalog calls=%d", len(children), calls.Load())
	}
	t.Cleanup(func() {
		for _, child := range children {
			GlobalModelRegistry().UnregisterClient(child.ID)
		}
	})
	for _, child := range children {
		if len(GlobalModelRegistry().GetModelsForClient(child.ID)) != 1 {
			t.Fatal("a child was omitted from synchronous model registration")
		}
	}
	current, _ := svc.coreManager.GetByID(parent.ID)
	current.Disabled, current.Status = true, coreauth.StatusDisabled
	if err = svc.runtimeAuthSyncHook()(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if len(GlobalModelRegistry().GetModelsForClient(child.ID)) != 0 {
			t.Fatal("parent disable left a child's models registered")
		}
	}
	svc.applyCoreAuthRemoval(t.Context(), parent.ID)
	for _, child := range children {
		if _, ok := svc.coreManager.GetByID(child.ID); ok {
			t.Fatal("parent removal retained a child")
		}
	}
}

func TestEnterpriseAccountHotReloadRejectsOldGraph(t *testing.T) {
	svc, legacy := enterpriseTestService(t, "enterprise-account-reload")
	account, err := ge.AccountFromLegacy(legacy.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	parent := legacy.Clone()
	parent.Metadata = ge.PutAccount(parent.Metadata, account)
	old := parent.Clone()
	account.Revision++
	account.Collections[0].Disabled = true
	parent.Metadata = ge.PutAccount(parent.Metadata, account)
	if _, err = svc.coreManager.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	old.Generation, old.RegistrationEpoch = 0, 0
	current, _ := svc.coreManager.GetByID(parent.ID)
	if !isStaleCoreAuth(current, old) {
		t.Fatal("old watcher graph could overwrite current collection settings")
	}
}
