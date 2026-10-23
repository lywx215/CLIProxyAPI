package auth

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

func TestEnterpriseRefreshRetentionAndStableIDs(t *testing.T) {
	binding := config.GeminiEnterpriseOAuth{ClientID: "own-client", ClientSecretEnv: "OWN_SECRET", ProjectID: "project", EngineID: "engine"}
	email := "user@example.com"
	metadata := ge.Metadata(binding, email, &oauth2.Token{AccessToken: "access", RefreshToken: "existing-refresh", Expiry: time.Now().Add(time.Hour)})
	old := &coreauth.Auth{Provider: ge.Provider, Metadata: metadata}
	if retainedEnterpriseRefresh([]*coreauth.Auth{old}, email, binding) != "existing-refresh" {
		t.Fatal("same binding lost refresh token")
	}
	empty := old.Clone()
	empty.Metadata["refresh_token"] = ""
	if retainedEnterpriseRefresh([]*coreauth.Auth{empty, old}, email, binding) != "existing-refresh" {
		t.Fatal("an empty duplicate suppressed a matching refresh credential")
	}
	for _, change := range []string{"client", "account", "resource", "provider"} {
		other := old.Clone()
		switch change {
		case "client":
			other.Metadata["client_id"] = "other-client"
		case "account":
			other.Metadata["email"] = "other@example.com"
		case "resource":
			other.Metadata["engine_id"] = "other-engine"
		case "provider":
			other.Provider = "gemini-cli"
		}
		if retainedEnterpriseRefresh([]*coreauth.Auth{other}, email, binding) != "" {
			t.Fatalf("retained unrelated %s refresh token", change)
		}
	}
	first := enterpriseRecordID(email, binding)
	binding.EngineID = "other-engine"
	if first == enterpriseRecordID(email, binding) || first != enterpriseRecordID(strings.ToUpper(email), ge.FromMetadata(metadata)) {
		t.Fatal("unstable or conflicting account binding ID")
	}
	if duration := NewGeminiEnterpriseAuthenticator().RefreshLead(); duration == nil || *duration != 5*time.Minute {
		t.Fatal("incorrect refresh lead")
	}
}

func TestEnterpriseTokenStoreRoundtrip(t *testing.T) {
	store := NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	binding := config.GeminiEnterpriseOAuth{ClientID: "own-client", ClientSecretEnv: "OWN_SECRET", ProjectID: "project", EngineID: "engine"}
	metadata := ge.Metadata(binding, "test@example.com", &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)})
	id := enterpriseRecordID("test@example.com", binding)
	auth := &coreauth.Auth{ID: id, FileName: id, Provider: ge.Provider, Metadata: metadata, Status: coreauth.StatusActive}
	path, err := store.Save(coreauth.WithAuthCreationIntent(context.Background()), auth)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if json.Unmarshal(data, &decoded) != nil {
		t.Fatal("invalid stored record")
	}
	if _, exists := decoded["client_secret"]; exists {
		t.Fatal("client secret persisted")
	}
	listed, err := store.List(context.Background())
	if err != nil || len(listed) != 1 {
		t.Fatalf("roundtrip records=%v err=%v", listed, err)
	}
	if listed[0].Provider != ge.Provider || ge.Binding(ge.FromMetadata(listed[0].Metadata)) != ge.Binding(binding) || ge.String(listed[0].Metadata, "refresh_token") != "refresh" {
		t.Fatal("restart changed provider, binding or refresh credentials")
	}
	metadata["refresh_token"] = ""
	if retainedEnterpriseRefresh(listed, "test@example.com", binding) != "refresh" {
		t.Fatal("refresh retention after restart failed")
	}
}

func TestEnterpriseOAuthCallbackStateAndDenial(t *testing.T) {
	for _, test := range []struct {
		query            string
		status           int
		received, denied bool
	}{
		{"?state=wrong&code=untrusted", 400, false, false}, {"?state=expected", 400, false, false},
		{"?state=expected&error=access_denied", 403, true, true}, {"?state=expected&code=accepted", 200, true, false},
	} {
		codes := make(chan enterpriseCallbackResult, 1)
		recorder := httptest.NewRecorder()
		enterpriseCallbackHandler("expected", codes)(recorder, httptest.NewRequest("GET", "http://localhost:8788/oauth-callback"+test.query, nil))
		if recorder.Code != test.status {
			t.Fatalf("callback status=%d, want %d", recorder.Code, test.status)
		}
		select {
		case result := <-codes:
			if !test.received || result.denied != test.denied {
				t.Fatal("invalid callback accepted")
			}
		default:
			if test.received {
				t.Fatal("valid callback/denial was lost")
			}
		}
	}
}
