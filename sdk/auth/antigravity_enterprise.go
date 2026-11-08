package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	ag "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

type AntigravityEnterpriseAuthenticator struct{}

func NewAntigravityEnterpriseAuthenticator() Authenticator {
	return &AntigravityEnterpriseAuthenticator{}
}
func (AntigravityEnterpriseAuthenticator) Provider() string            { return ae.Provider }
func (AntigravityEnterpriseAuthenticator) RefreshLead() *time.Duration { return new(5 * time.Minute) }

func NewAntigravityEnterpriseAccountRecord(identity ae.Identity, token *oauth2.Token, existing []*coreauth.Auth) (*coreauth.Auth, error) {
	identity.ID, identity.Email = strings.TrimSpace(identity.ID), strings.TrimSpace(identity.Email)
	if !identity.Verified || identity.ID == "" || identity.Email == "" || strings.ContainsAny(identity.Email, "\r\n\x00") || token == nil || token.AccessToken == "" {
		return nil, fmt.Errorf("%s: verified Google identity and access token required", ae.Provider)
	}
	clientID := ae.OAuth("").ClientID
	accountID := ae.AccountID(identity.ID, clientID)
	id := "antigravity-enterprise-account-" + accountID[:32] + ".json"
	account := ae.Account{Version: 1, ID: accountID, Revision: 1, Bindings: []ae.Binding{}}
	record := &coreauth.Auth{ID: id, FileName: id, Provider: ae.Provider, Label: strings.ToLower(identity.Email), Status: coreauth.StatusActive, CreatedAt: time.Now().UTC(), Metadata: map[string]any{}}
	for _, old := range existing {
		if old == nil || old.Provider != ae.Provider || coreauth.IsAntigravityEnterpriseBindingAuth(old) || ae.String(old.Metadata, "google_user_id") != identity.ID || ae.String(old.Metadata, "client_id") != clientID {
			continue
		}
		graph, err := ae.ReadAccount(old.Metadata)
		if err != nil {
			return nil, err
		}
		if graph != nil && graph.ID == accountID {
			record = old.Clone()
			account = *graph
			break
		}
	}
	m := record.Metadata
	m["type"], m["email"], m["google_user_id"], m["client_id"] = ae.Provider, strings.ToLower(identity.Email), identity.ID, clientID
	m["access_token"], m["token_type"], m["expired"] = token.AccessToken, token.TokenType, token.Expiry.UTC().Format(time.RFC3339Nano)
	if token.RefreshToken != "" {
		m["refresh_token"] = token.RefreshToken
	}
	record.Metadata = ae.PutAccount(m, account)
	return record, nil
}

func (AntigravityEnterpriseAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil || cfg.Home.Enabled {
		return nil, fmt.Errorf("%s: local OAuth requires standalone mode", ae.Provider)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	port := ag.CallbackPort
	if opts.CallbackPort > 0 {
		port = opts.CallbackPort
	}
	server, port, callbacks, err := startAntigravityCallbackServer(port)
	if err != nil {
		return nil, err
	}
	defer func() { _ = server.Close() }()
	state, err := misc.GenerateRandomState()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	application := ae.OAuth(fmt.Sprintf("http://localhost:%d/oauth-callback", port))
	authURL := application.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "select_account consent"))
	fmt.Printf("Open this Antigravity Enterprise authorization URL in your browser:\n%s\n", authURL)
	if !opts.NoBrowser && browser.IsAvailable() {
		_ = browser.OpenURL(authURL)
	} else {
		util.PrintSSHTunnelInstructions(port)
	}
	credentialCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	callback, err := waitAntigravityEnterpriseCallback(credentialCtx, state, callbacks, opts.Prompt)
	if err != nil {
		return nil, err
	}
	if callback.Code == "" || callback.Error != "" {
		return nil, fmt.Errorf("%s: invalid or denied OAuth callback", ae.Provider)
	}
	client := ae.SafeClient(util.SetProxy(&cfg.SDKConfig, &http.Client{}))
	credentialCtx = context.WithValue(credentialCtx, oauth2.HTTPClient, client)
	token, err := application.Exchange(credentialCtx, callback.Code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("%s: token exchange failed", ae.Provider)
	}
	identity, err := ae.UserInfo(credentialCtx, client, token.AccessToken)
	if err != nil {
		return nil, err
	}
	var existing []*coreauth.Auth
	if store := GetTokenStore(); store != nil {
		if setter, ok := store.(interface{ SetBaseDir(string) }); ok && strings.TrimSpace(cfg.AuthDir) != "" {
			setter.SetBaseDir(cfg.AuthDir)
		}
		existing, _ = store.List(ctx)
	}
	record, err := NewAntigravityEnterpriseAccountRecord(identity, token, existing)
	if err == nil && cfg.OAuth.Providers.AntigravityEnterprise != nil {
		record.Metadata["discovery_quota_project"] = cfg.OAuth.Providers.AntigravityEnterprise.DiscoveryQuotaProject
	}
	return record, err
}

func (AntigravityEnterpriseAuthenticator) PostLogin(ctx context.Context, cfg *config.Config, record *coreauth.Auth) {
	if cfg == nil || record == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	startedAt := time.Now().UTC()
	client := ae.SafeClient(util.SetProxy(&cfg.SDKConfig, &http.Client{}))
	licenses, unreachable, err := ae.FetchLicenses(ctx, client, ae.String(record.Metadata, "access_token"), ae.String(record.Metadata, "discovery_quota_project"))
	if err != nil {
		fmt.Printf("Authorization saved; subscription discovery failed: %v\n", err)
		return
	}
	account, err := ae.ReadAccount(record.Metadata)
	if err != nil || account == nil {
		return
	}
	merged, err := ae.MergeLicenses(*account, licenses, unreachable, startedAt)
	if err != nil {
		fmt.Printf("Authorization saved; subscription discovery failed: %v\n", err)
		return
	}
	record.Metadata = ae.PutAccount(record.Metadata, merged)
	fmt.Printf("Discovered %d Antigravity Enterprise subscription bindings.\n", len(merged.Bindings))
}
func (AntigravityEnterpriseAuthenticator) PostLoginNeedsPersistence() bool { return true }

func waitAntigravityEnterpriseCallback(ctx context.Context, state string, callbacks <-chan callbackResult, prompt func(string) (string, error)) (callbackResult, error) {
	var promptTimer *time.Timer
	var promptC <-chan time.Time
	if prompt != nil {
		promptTimer = time.NewTimer(15 * time.Second)
		promptC = promptTimer.C
		defer promptTimer.Stop()
	}
	var inputs <-chan string
	var inputErrors <-chan error
	for {
		select {
		case callback, open := <-callbacks:
			if !open {
				return callbackResult{}, fmt.Errorf("%s: callback receiver closed", ae.Provider)
			}
			// An unrelated loopback request must not consume the current OAuth flow.
			if callback.State == state {
				return callback, nil
			}
		case <-promptC:
			promptC = nil
			inputs, inputErrors = misc.AsyncPrompt(prompt, "Paste the Antigravity Enterprise callback URL (or press Enter to keep waiting): ")
		case input := <-inputs:
			inputs, inputErrors = nil, nil
			parsed, err := misc.ParseOAuthCallback(input)
			if err != nil {
				return callbackResult{}, fmt.Errorf("%s: invalid manual callback", ae.Provider)
			}
			if parsed != nil && parsed.State == state {
				return callbackResult{Code: parsed.Code, State: parsed.State, Error: parsed.Error}, nil
			}
			if promptTimer != nil {
				promptTimer.Reset(15 * time.Second)
				promptC = promptTimer.C
			}
		case err := <-inputErrors:
			inputs, inputErrors = nil, nil
			if err != nil && !errors.Is(err, io.EOF) {
				return callbackResult{}, fmt.Errorf("%s: manual callback unavailable", ae.Provider)
			}
		case <-ctx.Done():
			return callbackResult{}, fmt.Errorf("%s: authorization expired", ae.Provider)
		}
	}
}
