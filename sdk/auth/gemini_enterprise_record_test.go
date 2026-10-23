package auth

import (
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

func TestGeminiEnterpriseManagementAndCLIShareRecordBinding(t *testing.T) {
	binding := config.GeminiEnterpriseOAuth{ClientID: "synthetic-client", ProjectID: "project", EngineID: "engine"}
	existing := &coreauth.Auth{Provider: ge.Provider, Metadata: ge.Metadata(binding, "fixture@example.com", &oauth2.Token{RefreshToken: "synthetic-refresh"})}
	token := &oauth2.Token{AccessToken: "synthetic-access"}
	record := NewGeminiEnterpriseRecord(binding, "fixture@example.com", token, []*coreauth.Auth{existing})
	if record.ID != enterpriseRecordID("fixture@example.com", binding) || ge.String(record.Metadata, "refresh_token") != "synthetic-refresh" || token.RefreshToken != "" {
		t.Fatal("shared construction changed identity, lost refresh credentials or mutated input token")
	}
	binding.EngineID = "other-engine"
	other := NewGeminiEnterpriseRecord(binding, "fixture@example.com", token, []*coreauth.Auth{existing})
	if record.ID == other.ID || ge.String(other.Metadata, "refresh_token") != "" {
		t.Fatal("an unrelated resource inherited account identity or refresh credentials")
	}
}

func TestGeminiEnterpriseRecordBindingIsolation(t *testing.T) {
	binding := config.GeminiEnterpriseOAuth{
		ClientID: "synthetic-client", ClientSecretEnv: "SYNTHETIC_SECRET", ProjectID: "project", EngineID: "engine",
	}.WithDefaults()
	token := &oauth2.Token{AccessToken: "synthetic-access", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
	metadata := ge.Metadata(binding, "fixture@example.com", &oauth2.Token{RefreshToken: "old-refresh"})
	existing := &coreauth.Auth{Provider: ge.Provider, Metadata: metadata}
	first := NewGeminiEnterpriseRecord(binding, "fixture@example.com", token, []*coreauth.Auth{existing})
	normalized := NewGeminiEnterpriseRecord(binding, "  FIXTURE@example.com  ", token, []*coreauth.Auth{existing})
	if first.ID != normalized.ID || ge.String(normalized.Metadata, "refresh_token") != "old-refresh" {
		t.Fatal("equivalent account identity lost its stored refresh credential")
	}
	for _, field := range []string{"client", "secret-env", "project", "location", "collection", "engine", "assistant", "account", "provider"} {
		t.Run(field, func(t *testing.T) {
			changed, email := binding, "fixture@example.com"
			old := existing.Clone()
			switch field {
			case "client":
				changed.ClientID = "other-client"
			case "secret-env":
				changed.ClientSecretEnv = "OTHER_SECRET"
			case "project":
				changed.ProjectID = "other-project"
			case "location":
				changed.Location = "other-location"
			case "collection":
				changed.CollectionID = "other-collection"
			case "engine":
				changed.EngineID = "other-engine"
			case "assistant":
				changed.AssistantID = "other-assistant"
			case "account":
				email = "other@example.com"
			case "provider":
				old.Provider = "gemini-cli"
			}
			record := NewGeminiEnterpriseRecord(changed, email, token, []*coreauth.Auth{old})
			if ge.String(record.Metadata, "refresh_token") != "" {
				t.Fatal("record borrowed a refresh credential from a different account, application, or resource")
			}
			if field != "provider" && record.ID == first.ID {
				t.Fatal("distinct account/application/resource bindings share a record identity")
			}
		})
	}
	fresh := *token
	fresh.RefreshToken = "new-refresh"
	renewed := NewGeminiEnterpriseRecord(binding, "fixture@example.com", &fresh, []*coreauth.Auth{existing})
	if ge.String(renewed.Metadata, "refresh_token") != "new-refresh" || ge.String(existing.Metadata, "refresh_token") != "old-refresh" || token.RefreshToken != "" {
		t.Fatal("new authorization did not take precedence or mutated caller-owned credentials")
	}
}
