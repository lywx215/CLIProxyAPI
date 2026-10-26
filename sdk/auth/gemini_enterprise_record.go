package auth

import (
	"fmt"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

// NewGeminiEnterpriseAccountRecord builds the shared account credential used by
// new CLI and management logins. The email must already be verified by Google.
// It preserves an existing account graph and never performs discovery or saves.
func NewGeminiEnterpriseAccountRecord(application config.GeminiEnterpriseOAuth, email string, token *oauth2.Token, existing []*coreauth.Auth) (*coreauth.Auth, error) {
	application = application.WithDefaults()
	if err := application.ValidateLogin(); err != nil {
		return nil, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if token == nil || strings.TrimSpace(token.AccessToken) == "" || email == "" || strings.ContainsAny(email, "\r\n\x00") {
		return nil, fmt.Errorf("gemini-enterprise: a verified email and OAuth access token are required")
	}
	accountID := ge.AccountID(email, application.ClientID)
	account := ge.Account{Version: ge.AccountVersion, ID: accountID, Revision: 1, Collections: []ge.Collection{}}
	var previous *coreauth.Auth
	for _, old := range existing {
		if old == nil || old.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(old) || ge.String(old.Metadata, "enterprise_retired_to") != "" || ge.AccountID(ge.String(old.Metadata, "email"), ge.String(old.Metadata, "client_id")) != accountID {
			continue
		}
		graph, err := ge.ReadAccount(old.Metadata)
		if err != nil {
			return nil, err
		}
		if graph != nil {
			if previous == nil || old.ID < previous.ID {
				previous, account = old, *graph
			}
		}
	}
	if previous == nil && application.Validate() == nil {
		var err error
		account, err = ge.AccountFromLegacy(ge.Metadata(application, email, token))
		if err != nil {
			return nil, err
		}
	}
	copyToken := *token
	if copyToken.RefreshToken == "" {
		if previous != nil {
			copyToken.RefreshToken = ge.String(previous.Metadata, "refresh_token")
		}
		if copyToken.RefreshToken == "" {
			for _, old := range existing {
				if old != nil && old.Provider == ge.Provider && !coreauth.IsEnterpriseCollectionAuth(old) && ge.String(old.Metadata, "enterprise_retired_to") == "" && ge.AccountID(ge.String(old.Metadata, "email"), ge.String(old.Metadata, "client_id")) == accountID && ge.String(old.Metadata, "refresh_token") != "" {
					copyToken.RefreshToken = ge.String(old.Metadata, "refresh_token")
					break
				}
			}
		}
	}
	now := time.Now().UTC()
	id := "gemini-enterprise-account-" + accountID[:32] + ".json"
	record := &coreauth.Auth{ID: id, FileName: id, Provider: ge.Provider, Label: email, Status: coreauth.StatusActive, CreatedAt: now}
	metadata := ge.Metadata(application, email, &copyToken)
	if previous != nil {
		record = previous.Clone()
		metadata = ge.PutAccount(previous.Metadata, account)
		metadata["type"], metadata["email"], metadata["client_id"], metadata["client_secret_env"] = ge.Provider, email, application.ClientID, application.ClientSecretEnv
		ge.PutToken(metadata, &copyToken)
	}
	for _, key := range []string{"project_id", "location", "collection_id", "engine_id", "assistant_id"} {
		delete(metadata, key)
	}
	record.Label, record.UpdatedAt, record.LastRefreshedAt = email, now, now
	record.RejectedAccessToken, record.RefreshFailures = "", 0
	record.NextRefreshAfter = time.Time{}
	record.Metadata = ge.PutAccount(metadata, account)
	if !record.Disabled {
		record.Status, record.StatusMessage, record.Unavailable, record.LastError = coreauth.StatusActive, "", false, nil
	}
	return record, nil
}

// NewGeminiEnterpriseRecord shares the account identity and refresh-token binding
// rules between CLI login and v8 management login. It does not persist credentials.
func NewGeminiEnterpriseRecord(binding config.GeminiEnterpriseOAuth, email string, token *oauth2.Token, existing []*coreauth.Auth) *coreauth.Auth {
	if token == nil {
		return nil
	}
	binding = binding.WithDefaults()
	email = strings.TrimSpace(email)
	copyToken := *token
	if copyToken.RefreshToken == "" {
		copyToken.RefreshToken = retainedEnterpriseRefresh(existing, email, binding)
	}
	now := time.Now().UTC()
	id := enterpriseRecordID(email, binding)
	return &coreauth.Auth{
		ID: id, FileName: id, Provider: ge.Provider, Label: email,
		Status: coreauth.StatusActive, CreatedAt: now, UpdatedAt: now,
		LastRefreshedAt: now, Metadata: ge.Metadata(binding, email, &copyToken),
	}
}
