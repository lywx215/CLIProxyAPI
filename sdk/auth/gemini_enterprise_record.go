package auth

import (
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

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
