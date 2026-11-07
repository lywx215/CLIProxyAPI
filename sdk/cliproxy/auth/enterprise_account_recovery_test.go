package auth

import (
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func TestEnterpriseAccountAuthorizationChangeRecoversChildAvailability(t *testing.T) {
	for _, operation := range []string{"reauthorize", "refresh"} {
		for _, failure := range []string{"orphaned", "unauthorized", "unauthorized_with_retry"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				parent := registerEnterpriseFixture(t, m, enterpriseAccountFixture(t, 2))
				before := m.EnterpriseCollections(parent.ID)
				m.mu.Lock()
				for _, snapshot := range before {
					child := m.auths[snapshot.ID]
					child.Unavailable = true
					child.Success, child.Failed = 7, 3
					if failure != "orphaned" {
						child.Status, child.StatusMessage = StatusError, "unauthorized"
						child.LastError = &Error{HTTPStatus: 401, Code: "unauthorized", Message: "synthetic expired authorization"}
					}
					if failure == "unauthorized_with_retry" {
						child.NextRetryAfter = time.Now().Add(time.Hour)
					}
				}
				m.mu.Unlock()
				incoming := parent.Clone()
				incoming.Metadata["access_token"] = "synthetic-new-authorization"
				incoming.Metadata["expiry"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				var err error
				if operation == "reauthorize" {
					_, err = m.ReauthorizeEnterpriseAccount(t.Context(), incoming)
				} else {
					_, err = m.UpdateRefreshedAuth(t.Context(), parent, incoming)
				}
				if err != nil {
					t.Fatal(err)
				}
				for i, child := range m.EnterpriseCollections(parent.ID) {
					if child.Unavailable || child.Status != StatusActive || child.LastError != nil || child.StatusMessage != "" || !child.NextRetryAfter.IsZero() {
						t.Fatalf("new authorization left collection blocked: status=%s unavailable=%t message=%q error=%v retry=%v", child.Status, child.Unavailable, child.StatusMessage, child.LastError, child.NextRetryAfter)
					}
					if child.Index != before[i].Index || child.RegistrationEpoch != before[i].RegistrationEpoch || child.Success != 7 || child.Failed != 3 {
						t.Fatal("authorization recovery changed collection identity or counters")
					}
					if blocked, _, _ := isAuthBlockedForModel(child, "model", time.Now()); blocked {
						t.Fatal("recovered collection remains blocked for inference")
					}
				}
			})
		}
	}
}

func TestEnterpriseAccountAuthorizationRecoveryPreservesQuotaAndDisabledCollections(t *testing.T) {
	m := NewManager(nil, nil, nil)
	fixture := enterpriseAccountFixture(t, 5)
	graph, err := ge.ReadAccount(fixture.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	graph.Collections[2].Disabled = true
	fixture.Metadata = ge.PutAccount(fixture.Metadata, *graph)
	parent := registerEnterpriseFixture(t, m, fixture)
	children := m.EnterpriseCollections(parent.ID)
	until := time.Now().Add(time.Hour)
	m.mu.Lock()
	credentialLimited := m.auths[children[0].ID]
	credentialLimited.Unavailable = true
	credentialLimited.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: until}
	modelLimited := m.auths[children[1].ID]
	modelLimited.Unavailable = true
	modelLimited.ModelStates = map[string]*ModelState{
		"limited-model": {Status: StatusError, Unavailable: true, NextRetryAfter: until, LastError: &Error{HTTPStatus: 429}},
	}
	legacyLimited := m.auths[children[3].ID]
	legacyLimited.Unavailable = true
	legacyLimited.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
	transportLimited := m.auths[children[4].ID]
	transportLimited.Unavailable, transportLimited.NextRetryAfter = true, until
	transportLimited.LastError = &Error{HTTPStatus: 503, Message: "synthetic upstream unavailable"}
	m.mu.Unlock()
	incoming := parent.Clone()
	incoming.Metadata["access_token"] = "synthetic-replacement-token"
	if _, err = m.ReauthorizeEnterpriseAccount(t.Context(), incoming); err != nil {
		t.Fatal(err)
	}
	after := m.EnterpriseCollections(parent.ID)
	if !after[0].Unavailable || !after[0].Quota.Exceeded || !after[0].Quota.NextRecoverAt.Equal(until) {
		t.Fatal("reauthorization discarded an independent credential quota restriction")
	}
	if state := after[1].ModelStates["limited-model"]; state == nil || !state.Unavailable || !state.NextRetryAfter.Equal(until) || state.LastError == nil || state.LastError.HTTPStatus != 429 {
		t.Fatal("reauthorization discarded an independent model cooldown")
	}
	if !after[2].Disabled || after[2].Status != StatusDisabled {
		t.Fatal("reauthorization enabled a manually disabled collection")
	}
	if !after[3].Unavailable || !after[3].Quota.Exceeded || !after[3].Quota.NextRecoverAt.Equal(until) {
		t.Fatal("reauthorization discarded an independent legacy quota restriction")
	}
	if !after[4].Unavailable || !after[4].NextRetryAfter.Equal(until) || after[4].LastError == nil || after[4].LastError.HTTPStatus != 503 {
		t.Fatal("reauthorization discarded an independent transport retry restriction")
	}
}
