package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

const geminiEnterpriseCallbackURL = "http://localhost:8788/oauth-callback"

var newGeminiEnterpriseOAuthClient = ge.Client
var acquireGeminiEnterpriseCallback = enterpriseCallbacks.acquire

// RequestGeminiEnterpriseToken is registered only by the v8 management API.
func (h *Handler) RequestGeminiEnterpriseToken(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "configure oauth.providers.gemini-enterprise before login"})
		return
	}
	settings := h.cfg.OAuth
	proxy, authDir, home := h.cfg.ProxyURL, h.cfg.AuthDir, h.cfg.Home.Enabled
	h.mu.Unlock()
	if home {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gemini-enterprise OAuth is not supported in CLIProxyAPIHome mode"})
		return
	}
	binding, discover, target, err := h.geminiEnterpriseLoginBinding(c, settings)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	oauth, err := ge.ApplicationOAuth(binding, geminiEnterpriseCallbackURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(authDir) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OAuth auth directory is not configured"})
		return
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate OAuth state"})
		return
	}
	verifier := oauth2.GenerateVerifier()
	RegisterOAuthSession(state, ge.Provider)
	release, automatic := acquireGeminiEnterpriseCallback(state, authDir, 8788)
	ctx := PopulateAuthContext(context.Background(), c)
	go func() {
		defer release()
		waitPath := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", ge.Provider, state))
		defer func() { _ = os.Remove(waitPath) }()
		result, errWait := waitEnterpriseCallback(ctx, state, waitPath)
		if errWait != nil {
			if !errors.Is(errWait, errOAuthSessionNotPending) {
				SetOAuthSessionError(state, "Gemini Enterprise authorization timed out; start a new login")
			}
			return
		}
		if result.Error != "" {
			SetOAuthSessionError(state, "Google authorization was denied; check the consent screen test users and try again")
			return
		}
		if result.State != state || result.Code == "" {
			SetOAuthSessionError(state, "Invalid Gemini Enterprise OAuth callback")
			return
		}
		h.finishGeminiEnterpriseLoginAttempt(ctx, state, binding, oauth, verifier, result.Code, proxy, discover, target)
	}()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "url": ge.AuthorizationURL(oauth, state, verifier), "state": state,
		"callback_url": geminiEnterpriseCallbackURL, "manual_callback_required": !automatic,
		"resource_inputs_supported":    true,
		"reauthorization":              target != nil,
		"automatic_resource_discovery": discover,
	})
}

func (h *Handler) geminiEnterpriseLoginBinding(c *gin.Context, settings config.OAuthConfig) (config.GeminiEnterpriseOAuth, bool, *coreauth.Auth, error) {
	var binding config.GeminiEnterpriseOAuth
	selected, selectApp := c.GetQuery("application_client_id")
	name, renewName := c.GetQuery("reauthorize_name")
	index, renewIndex := c.GetQuery("reauthorize_auth_index")
	explicitClient := strings.TrimSpace(c.Query("client_id"))
	explicitSecret := strings.TrimSpace(c.Query("client_secret_env"))
	if renewName || renewIndex {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(index) == "" || selectApp || explicitClient != "" || explicitSecret != "" {
			return binding, false, nil, errors.New("select the current Gemini Enterprise credential for reauthorization")
		}
		if !h.authManager.SupportsExistingEnterpriseReauthorization() {
			return binding, false, nil, coreauth.ErrEnterpriseReauthorizationUnsupported
		}
		target, ok := h.lookupAuthFile(name, index)
		if !ok || target == nil || target.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(target) ||
			coreauth.IsPluginVirtualAuth(target) || ge.String(target.Metadata, "enterprise_retired_to") != "" ||
			strings.TrimSpace(ge.String(target.Metadata, "email")) == "" {
			return binding, false, nil, errors.New("Gemini Enterprise reauthorization target is unavailable")
		}
		binding = ge.FromMetadata(target.Metadata)
		return binding, false, target, binding.ValidateApplication()
	}
	selected = strings.TrimSpace(selected)
	if selectApp && (selected == "" || explicitClient != "" || explicitSecret != "") {
		return binding, false, nil, errors.New("select one configured Gemini Enterprise OAuth application")
	}
	var configured bool
	binding, configured = settings.GeminiEnterpriseApplication(selected)
	if selectApp && !configured {
		return binding, false, nil, errors.New("selected Gemini Enterprise OAuth application is unavailable")
	}
	mode, specified := c.GetQuery("auto_discover_resources")
	if !specified {
		return binding, binding.Validate() != nil, nil, binding.ValidateLogin()
	}
	if mode != "true" && mode != "false" {
		return binding, false, nil, errors.New("invalid resource discovery mode")
	}
	if !selectApp {
		binding = config.GeminiEnterpriseOAuth{ClientID: explicitClient, ClientSecretEnv: explicitSecret}
	}
	binding.EngineID = strings.TrimSpace(c.Query("engine_id"))
	binding.AssistantID = strings.TrimSpace(c.Query("assistant_id"))
	if mode == "true" {
		return binding.WithDefaults(), true, nil, binding.ValidateDiscovery()
	}
	binding.ProjectID = strings.TrimSpace(c.Query("project_id"))
	binding.Location = strings.TrimSpace(c.Query("location"))
	binding.CollectionID = strings.TrimSpace(c.Query("collection_id"))
	return binding.WithDefaults(), false, nil, binding.Validate()
}

func waitEnterpriseCallback(ctx context.Context, state, path string) (oauthCallbackFilePayload, error) {
	return waitProviderCallback(ctx, state, path, ge.Provider)
}

func waitProviderCallback(ctx context.Context, state, path, provider string) (oauthCallbackFilePayload, error) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !IsOAuthSessionPending(state, provider) {
			return oauthCallbackFilePayload{}, errOAuthSessionNotPending
		}
		if data, errRead := os.ReadFile(path); errRead == nil {
			var result oauthCallbackFilePayload
			if errJSON := json.Unmarshal(data, &result); errJSON != nil {
				return result, errJSON
			}
			return result, nil
		}
		select {
		case <-ctx.Done():
			return oauthCallbackFilePayload{}, ctx.Err()
		case <-timer.C:
			return oauthCallbackFilePayload{}, context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func (h *Handler) finishGeminiEnterpriseLogin(ctx context.Context, state string, binding config.GeminiEnterpriseOAuth, oauth *oauth2.Config, verifier, code, proxy string) {
	h.finishGeminiEnterpriseLoginWithDiscovery(ctx, state, binding, oauth, verifier, code, proxy, false)
}

func (h *Handler) finishGeminiEnterpriseLoginWithDiscovery(ctx context.Context, state string, binding config.GeminiEnterpriseOAuth, oauth *oauth2.Config, verifier, code, proxy string, discover bool) {
	h.finishGeminiEnterpriseLoginAttempt(ctx, state, binding, oauth, verifier, code, proxy, discover, nil)
}

func (h *Handler) finishGeminiEnterpriseLoginAttempt(ctx context.Context, state string, binding config.GeminiEnterpriseOAuth, oauth *oauth2.Config, verifier, code, proxy string, discover bool, target *coreauth.Auth) {
	if !IsOAuthSessionPending(state, ge.Provider) {
		return
	}
	client, err := newGeminiEnterpriseOAuthClient(proxy)
	if err != nil {
		SetOAuthSessionError(state, "Invalid Gemini Enterprise proxy configuration")
		return
	}
	defer client.CloseIdleConnections()
	credentialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	go watchOAuthSessionCancel(credentialCtx, cancel, state, ge.Provider)
	credentialCtx = context.WithValue(credentialCtx, oauth2.HTTPClient, client)
	token, err := oauth.Exchange(credentialCtx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		SetOAuthSessionError(state, ge.TokenError(err).Error())
		return
	}
	if token.AccessToken == "" {
		SetOAuthSessionError(state, "Google returned an empty access token")
		return
	}
	if !IsOAuthSessionPending(state, ge.Provider) {
		return
	}
	email, subject, err := ge.UserIdentity(credentialCtx, client, token.AccessToken)
	if err != nil {
		SetOAuthSessionError(state, "Failed to read the authorized Google account")
		return
	}
	if target != nil {
		if expected := ge.String(target.Metadata, coreauth.EnterpriseOAuthSubjectKey); expected != "" && expected != subject {
			SetOAuthSessionError(state, "Google account does not match the Gemini Enterprise credential")
			return
		}
		h.finishGeminiEnterpriseReauthorization(ctx, state, target, email, token)
		return
	}
	var existing []*coreauth.Auth
	if h.authManager != nil {
		existing = h.authManager.List()
	} else if store := h.tokenStoreWithBaseDir(); store != nil {
		existing, _ = store.List(ctx)
	}
	record := sdkAuth.NewGeminiEnterpriseRecord(binding, email, token, existing)
	if discover {
		record, err = sdkAuth.NewGeminiEnterpriseAccountRecord(binding, email, token, existing)
		if err != nil {
			SetOAuthSessionError(state, "Could not create the authorized account record")
			return
		}
	}
	if !oauthSessions.ClaimSave(state, ge.Provider) {
		return
	}
	accountSaved := false
	if discover && h.authManager != nil {
		if h.postAuthHook != nil {
			if errHook := h.postAuthHook(ctx, record); errHook != nil {
				SetOAuthSessionError(state, "Failed to prepare Gemini Enterprise authorization")
				return
			}
		}
		var errSave error
		record, errSave = h.authManager.ReauthorizeEnterpriseAccount(ctx, record)
		if errSave != nil {
			SetOAuthSessionError(state, "Failed to save Gemini Enterprise authorization; existing settings were retained")
			return
		}
		if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{record}); errHook != nil {
			SetOAuthSessionError(state, "Authorization saved, but runtime synchronization failed; refresh the account")
			return
		}
		accountSaved = true
	}
	if !accountSaved {
		if _, errSave := h.saveTokenRecord(ctx, record); errSave != nil {
			SetOAuthSessionError(state, "Failed to save Gemini Enterprise credentials")
			return
		}
	}
	if discover {
		// Authorization is durable before any optional metadata call. Discovery
		// cancellation cannot discard that authorization or claim a second save.
		cancel()
		oauthSessions.mu.Lock()
		if session, ok := oauthSessions.sessions[state]; ok {
			session.Saving = false
			if session.Metadata == nil {
				session.Metadata = map[string]any{}
			}
			session.Metadata["enterprise_authorization_saved"] = true
			oauthSessions.sessions[state] = session
		}
		oauthSessions.mu.Unlock()
		resourceCtx, cancelResources := context.WithCancel(ctx)
		defer cancelResources()
		go watchOAuthSessionCancel(resourceCtx, cancelResources, state, ge.Provider)
		if h.authManager != nil {
			if _, exists := h.authManager.GetByID(record.ID); !exists {
				_, _ = h.authManager.Register(coreauth.WithSkipPersist(resourceCtx), record)
			}
			_, _, _, _, _ = h.discoverEnterpriseAccount(resourceCtx, record)
		} else {
			graph, errGraph := ge.ReadAccount(record.Metadata)
			if errGraph == nil && graph != nil {
				result, _ := ge.DiscoverResources(resourceCtx, client, binding, token.AccessToken, graph.Collections)
				if resourceCtx.Err() == nil {
					merged, _ := ge.MergeDiscoveredCollections(*graph, result)
					record.Metadata = ge.PutAccount(record.Metadata, merged)
					_, _ = h.saveTokenRecord(resourceCtx, record)
				}
			}
		}
		if resourceCtx.Err() != nil {
			return
		}
	}
	CompleteOAuthSession(state)
}

func (h *Handler) finishGeminiEnterpriseReauthorization(ctx context.Context, state string, target *coreauth.Auth, email string, token *oauth2.Token) {
	if !strings.EqualFold(email, ge.String(target.Metadata, "email")) {
		SetOAuthSessionError(state, "Google account does not match the Gemini Enterprise credential")
		return
	}
	if token.RefreshToken == "" && ge.String(target.Metadata, "refresh_token") == "" {
		SetOAuthSessionError(state, "Google did not return an offline refresh token")
		return
	}
	if !oauthSessions.ClaimSave(state, ge.Provider) {
		return
	}
	record := target.Clone()
	delete(record.Metadata, "refresh_token")
	ge.PutToken(record.Metadata, token)
	record.LastRefreshedAt = time.Now()
	saved, err := h.authManager.ReauthorizeExistingGeminiEnterpriseCredential(ctx, target, record)
	if err != nil {
		SetOAuthSessionError(state, "Failed to renew the selected Gemini Enterprise credential; start from the current account")
		return
	}
	if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{saved}); errHook != nil {
		SetOAuthSessionError(state, "Authorization saved, but runtime synchronization failed; refresh the account")
		return
	}
	CompleteOAuthSession(state)
}

func addGeminiEnterpriseCredentialSummary(c *gin.Context, entry gin.H, provider string, metadata map[string]any) {
	if c.GetBool(ConfigV8ContextKey) && provider == ge.Provider {
		entry["gemini_enterprise"] = geminiEnterpriseCredentialSummary(metadata)
	}
}

// Return only the public binding and renewal capability, never credential values.
func geminiEnterpriseCredentialSummary(metadata map[string]any) gin.H {
	summary := gin.H{"has_refresh_token": ge.String(metadata, "refresh_token") != "", "resource_edit_supported": true, "endpoint": ge.OriginForLocation(ge.FromMetadata(metadata).Location)}
	for _, key := range []string{"client_id", "project_id", "location", "collection_id", "engine_id", "assistant_id", "expiry"} {
		if value := ge.String(metadata, key); value != "" {
			summary[key] = value
		}
	}
	return summary
}
