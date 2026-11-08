package auth

import (
	"context"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

func TestAntigravityEnterpriseReauthorizationRetainsBindingsAndRefresh(t *testing.T) {
	identity := ae.Identity{ID: "google-subject", Email: "owner@example.com", Verified: true}
	first, err := NewAntigravityEnterpriseAccountRecord(identity, &oauth2.Token{AccessToken: "old-access", RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ := ae.ReadAccount(first.Metadata)
	license := ae.License{ProjectID: "project-a", Location: "global", UserTier: "gcp-ge-plus-tier"}
	graph.Bindings = []ae.Binding{{ID: ae.BindingID(graph.ID, license), Revision: 7, License: license, Disabled: true, Settings: map[string]any{"prefix": "retain"}}}
	graph.Revision = 9
	first.Metadata = ae.PutAccount(first.Metadata, *graph)
	fresh, err := NewAntigravityEnterpriseAccountRecord(identity, &oauth2.Token{AccessToken: "new-access", Expiry: time.Now().Add(time.Hour)}, []*coreauth.Auth{first})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := ae.ReadAccount(fresh.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 9 || !updated.Bindings[0].Disabled || updated.Bindings[0].Settings["prefix"] != "retain" || ae.String(fresh.Metadata, "refresh_token") != "fixture-refresh" {
		t.Fatal("reauthorization lost account preferences or renewal")
	}
	if ae.String(first.Metadata, "access_token") != "old-access" {
		t.Fatal("mutated constructor input")
	}
}

func TestAntigravityEnterpriseUnrelatedBrokenCredentialDoesNotBlockLogin(t *testing.T) {
	identity := ae.Identity{ID: "target-subject", Email: "target@example.com", Verified: true}
	broken := &coreauth.Auth{Provider: ae.Provider, Metadata: map[string]any{"google_user_id": "other-subject", "client_id": ae.OAuth("").ClientID, ae.AccountKey: "invalid"}}
	if _, err := NewAntigravityEnterpriseAccountRecord(identity, &oauth2.Token{AccessToken: "fixture"}, []*coreauth.Auth{broken}); err != nil {
		t.Fatal("unrelated malformed account blocked login", err)
	}
	broken.Metadata["google_user_id"] = identity.ID
	if _, err := NewAntigravityEnterpriseAccountRecord(identity, &oauth2.Token{AccessToken: "fixture"}, []*coreauth.Auth{broken}); err == nil {
		t.Fatal("matching malformed account was silently replaced")
	}
}

func TestAntigravityEnterpriseCallbackIgnoresUnrelatedState(t *testing.T) {
	callbacks := make(chan callbackResult, 2)
	callbacks <- callbackResult{State: "other", Code: "unrelated-code"}
	callbacks <- callbackResult{State: "expected", Code: "correct-code"}
	result, err := waitAntigravityEnterpriseCallback(context.Background(), "expected", callbacks, nil)
	if err != nil || result.Code != "correct-code" {
		t.Fatal("unrelated loopback callback consumed authorization", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitAntigravityEnterpriseCallback(ctx, "expected", nil, nil); err == nil {
		t.Fatal("cancelled authorization kept waiting")
	}
}
func TestAntigravityEnterpriseRequiresVerifiedSubject(t *testing.T) {
	for _, identity := range []ae.Identity{{Email: "x@example.com", Verified: true}, {ID: "id", Email: "x@example.com"}} {
		if _, err := NewAntigravityEnterpriseAccountRecord(identity, &oauth2.Token{AccessToken: "fixture"}, nil); err == nil {
			t.Fatal("unverified or unstable identity accepted")
		}
	}
}
