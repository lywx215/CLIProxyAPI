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

// RequestGeminiEnterpriseToken is registered only by the v8 management API.
func (h *Handler) RequestGeminiEnterpriseToken(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "configure oauth.providers.gemini-enterprise before login"})
		return
	}
	var binding config.GeminiEnterpriseOAuth
	if h.cfg.OAuth.Providers.GeminiEnterprise != nil {
		binding = h.cfg.OAuth.Providers.GeminiEnterprise.WithDefaults()
	}
	proxy, authDir, home := h.cfg.ProxyURL, h.cfg.AuthDir, h.cfg.Home.Enabled
	h.mu.Unlock()
	if home {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gemini-enterprise OAuth is not supported in CLIProxyAPIHome mode"})
		return
	}
	discover := false
	var oauth *oauth2.Config
	var err error
	if mode, specified := c.GetQuery("auto_discover_resources"); specified {
		if mode != "true" && mode != "false" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid resource discovery mode"})
			return
		}
		discover = mode == "true"
		// Session inputs are independent of the global binding and other accounts.
		binding = (config.GeminiEnterpriseOAuth{
			ClientID: strings.TrimSpace(c.Query("client_id")), ClientSecretEnv: strings.TrimSpace(c.Query("client_secret_env")),
			EngineID: strings.TrimSpace(c.Query("engine_id")), AssistantID: strings.TrimSpace(c.Query("assistant_id")),
		}).WithDefaults()
		if discover {
			err = binding.ValidateDiscovery()
		} else {
			binding.ProjectID = strings.TrimSpace(c.Query("project_id"))
			if location := strings.TrimSpace(c.Query("location")); location != "" {
				binding.Location = location
			}
			if collection := strings.TrimSpace(c.Query("collection_id")); collection != "" {
				binding.CollectionID = collection
			}
			err = binding.Validate()
		}
		if err == nil {
			oauth, err = ge.ApplicationOAuth(binding, geminiEnterpriseCallbackURL)
		}
	} else {
		discover = binding.Validate() != nil
		oauth, err = ge.ApplicationOAuth(binding, geminiEnterpriseCallbackURL)
		if err == nil {
			err = binding.ValidateLogin()
		}
	}
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
	release, automatic := enterpriseCallbacks.acquire(state, authDir, 8788)
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
		h.finishGeminiEnterpriseLoginWithDiscovery(ctx, state, binding, oauth, verifier, result.Code, proxy, discover)
	}()
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "url": ge.AuthorizationURL(oauth, state, verifier), "state": state,
		"callback_url": geminiEnterpriseCallbackURL, "manual_callback_required": !automatic,
		"resource_inputs_supported":    true,
		"automatic_resource_discovery": discover,
	})
}

func waitEnterpriseCallback(ctx context.Context, state, path string) (oauthCallbackFilePayload, error) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !IsOAuthSessionPending(state, ge.Provider) {
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
	email, err := ge.Email(credentialCtx, client, token.AccessToken)
	if err != nil {
		SetOAuthSessionError(state, "Failed to read the authorized Google account")
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
