package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/oauth2"
)

type enterpriseImportFailure struct {
	Code      string
	HTTP      int
	Uncertain bool
}

func (e *enterpriseImportFailure) Error() string {
	return fmt.Sprintf("Google authorization verification: %s (HTTP %d)", e.Code, e.HTTP)
}

// verifyEnterpriseImportedGrant proves refresh ownership, not merely access-token ownership.
func verifyEnterpriseImportedGrant(ctx context.Context, app config.GeminiEnterpriseOAuth, refresh, email, subject, proxy string) (*oauth2.Token, error) {
	oauth, err := ge.ApplicationOAuth(app, "")
	if err != nil {
		return nil, &enterpriseImportFailure{Code: "oauth_application_not_ready", HTTP: 409}
	}
	client, err := newGeminiEnterpriseOAuthClient(proxy)
	if err != nil {
		return nil, &enterpriseImportFailure{Code: "oauth_transport_unavailable", HTTP: 503}
	}
	defer client.CloseIdleConnections()
	credentialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	credentialCtx = context.WithValue(credentialCtx, oauth2.HTTPClient, client)
	token, err := oauth.TokenSource(credentialCtx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		failure := &enterpriseImportFailure{Code: "oauth_exchange_unknown", HTTP: 502, Uncertain: true}
		var retrieval *oauth2.RetrieveError
		if errors.As(err, &retrieval) && retrieval.Response != nil {
			failure.HTTP = retrieval.Response.StatusCode
			if failure.HTTP >= 400 && failure.HTTP < 500 {
				failure.Code, failure.Uncertain = "oauth_exchange_rejected", false
				for _, code := range []string{"invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "access_denied", "invalid_scope"} {
					if retrieval.ErrorCode == code {
						failure.Code = code
					}
				}
			}
		}
		return nil, failure
	}
	if token == nil || token.AccessToken == "" {
		return nil, &enterpriseImportFailure{Code: "empty_access_token", HTTP: 502, Uncertain: true}
	}
	if token.RefreshToken == "" {
		token.RefreshToken = refresh
	}
	readJSON := func(method, endpoint string, target any) error {
		request, errRequest := http.NewRequestWithContext(credentialCtx, method, endpoint, nil)
		if errRequest != nil {
			return &enterpriseImportFailure{Code: "oauth_verification_request_invalid", HTTP: 502, Uncertain: true}
		}
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
		}
		response, errResponse := client.Do(request)
		if errResponse != nil {
			// Transport errors may retain request objects containing the bearer token.
			return &enterpriseImportFailure{Code: "oauth_verification_unknown", HTTP: 502, Uncertain: true}
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return &enterpriseImportFailure{Code: "oauth_verification_rejected", HTTP: response.StatusCode, Uncertain: true}
		}
		if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target) != nil {
			return &enterpriseImportFailure{Code: "oauth_verification_invalid_response", HTTP: 502, Uncertain: true}
		}
		return nil
	}
	var identity struct {
		ID       string `json:"id"`
		Email    string `json:"email"`
		Verified bool   `json:"verified_email"`
	}
	if err = readJSON(http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo", &identity); err != nil {
		return nil, err
	}
	if !identity.Verified || !strings.EqualFold(identity.Email, email) || identity.ID != subject {
		return nil, &enterpriseImportFailure{Code: "oauth_identity_mismatch", HTTP: 409}
	}
	var info struct {
		IssuedTo string `json:"azp"`
		Audience string `json:"aud"`
		Scope    string `json:"scope"`
		UserID   string `json:"sub"`
	}
	if err = readJSON(http.MethodPost, "https://oauth2.googleapis.com/tokeninfo", &info); err != nil {
		return nil, err
	}
	if (info.IssuedTo == "" && info.Audience == "") || (info.IssuedTo != "" && info.IssuedTo != app.ClientID) ||
		(info.Audience != "" && info.Audience != app.ClientID) || (info.UserID != "" && info.UserID != subject) {
		return nil, &enterpriseImportFailure{Code: "oauth_client_mismatch", HTTP: 409}
	}
	scopes := map[string]bool{}
	for _, scope := range strings.Fields(info.Scope) {
		scopes[scope] = true
	}
	for _, scope := range oauth.Scopes {
		if !scopes[scope] {
			return nil, &enterpriseImportFailure{Code: "oauth_scope_missing", HTTP: 403}
		}
	}
	return token, nil
}
