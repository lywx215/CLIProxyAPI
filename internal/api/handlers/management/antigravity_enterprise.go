package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ag "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/credentialweight"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/oauth2"
)

var antigravityEnterpriseCallbacks = &enterpriseCallbackReceiver{provider: ae.Provider}

type aeMetadataExecutor interface {
	Metadata(context.Context, *coreauth.Auth, bool) (ae.Config, ae.Quota, error)
}

type aeCatalogReader interface {
	Catalog(context.Context, *coreauth.Auth, bool) (ae.Config, error)
}
type aeQuotaReader interface {
	Quota(context.Context, *coreauth.Auth, bool) (ae.Quota, error)
}

func (h *Handler) antigravityEnterpriseCatalog(ctx context.Context, auth *coreauth.Auth, refresh bool) (ae.Config, error) {
	if e, ok := h.authManager.Executor(ae.Provider); ok {
		if reader, yes := e.(aeCatalogReader); yes {
			return reader.Catalog(ctx, auth, refresh)
		}
	}
	config, _, err := h.antigravityEnterpriseMetadata(ctx, auth, refresh)
	return config, err
}
func (h *Handler) antigravityEnterpriseQuotaRead(ctx context.Context, auth *coreauth.Auth, refresh bool) (ae.Quota, error) {
	if e, ok := h.authManager.Executor(ae.Provider); ok {
		if reader, yes := e.(aeQuotaReader); yes {
			return reader.Quota(ctx, auth, refresh)
		}
	}
	_, quota, err := h.antigravityEnterpriseMetadata(ctx, auth, refresh)
	return quota, err
}

func (h *Handler) RequestAntigravityEnterpriseToken(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	if cfg == nil {
		h.mu.Unlock()
		enterpriseManagementFailure(c, 503, "config_unavailable", "Server configuration is unavailable")
		return
	}
	authDir := cfg.AuthDir
	quotaProject := ""
	if cfg.OAuth.Providers.AntigravityEnterprise != nil {
		quotaProject = cfg.OAuth.Providers.AntigravityEnterprise.DiscoveryQuotaProject
	}
	h.mu.Unlock()
	if authDir == "" {
		enterpriseManagementFailure(c, 400, "auth_directory_required", "Configure credential storage before authorization")
		return
	}
	if value := c.Query("discovery_quota_project"); value != "" {
		quotaProject = value
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		enterpriseManagementFailure(c, 500, "oauth_failed", "Could not initialize authorization")
		return
	}
	verifier := oauth2.GenerateVerifier()
	redirect := fmt.Sprintf("http://localhost:%d/oauth-callback", ag.CallbackPort)
	application := ae.OAuth(redirect)
	RegisterOAuthSession(state, ae.Provider)
	release, automatic := antigravityEnterpriseCallbacks.acquire(state, authDir, ag.CallbackPort)
	ctx := PopulateAuthContext(context.Background(), c)
	go func() {
		defer release()
		path := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", ae.Provider, state))
		defer func() { _ = os.Remove(path) }()
		callback, errWait := waitProviderCallback(ctx, state, path, ae.Provider)
		if errWait != nil || callback.State != state || callback.Code == "" || callback.Error != "" {
			SetOAuthSessionError(state, "Antigravity Enterprise authorization expired or was denied")
			return
		}
		credentialCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		client := ae.SafeClient(util.SetProxy(&cfg.SDKConfig, &http.Client{}))
		credentialCtx = context.WithValue(credentialCtx, oauth2.HTTPClient, client)
		token, errToken := application.Exchange(credentialCtx, callback.Code, oauth2.VerifierOption(verifier))
		if errToken != nil {
			SetOAuthSessionError(state, "Antigravity Enterprise token exchange failed")
			return
		}
		identity, errIdentity := ae.UserInfo(credentialCtx, client, token.AccessToken)
		if errIdentity != nil {
			SetOAuthSessionError(state, "Could not verify Google identity")
			return
		}
		record, errRecord := sdkAuth.NewAntigravityEnterpriseAccountRecord(identity, token, h.authManager.List())
		if errRecord != nil {
			SetOAuthSessionError(state, "Could not create account record")
			return
		}
		record.Metadata["discovery_quota_project"] = quotaProject
		if !oauthSessions.ClaimSave(state, ae.Provider) {
			return
		}
		if h.postAuthHook != nil {
			if errHook := h.postAuthHook(ctx, record); errHook != nil {
				SetOAuthSessionError(state, "Could not prepare authorization")
				return
			}
		}
		saved, errSave := h.authManager.ReauthorizeAntigravityEnterpriseAccount(coreauth.WithAuthCreationIntent(ctx), record)
		if errSave != nil {
			SetOAuthSessionError(state, "Could not save authorization; existing bindings retained")
			return
		}
		if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{saved}); errHook != nil {
			SetOAuthSessionError(state, "Authorization saved; runtime synchronization failed")
			return
		}
		// Authorization is durable before resource discovery and independent of its result.
		_, _ = h.discoverAntigravityEnterprise(ctx, saved)
		CompleteOAuthSession(state)
	}()
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"status": "ok", "url": application.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "select_account consent")), "state": state, "callback_url": redirect, "manual_callback_required": !automatic, "automatic_resource_discovery": true})
}

func (h *Handler) addAntigravityEnterpriseSummary(c *gin.Context, entry gin.H, auth *coreauth.Auth) {
	if !c.GetBool(ConfigV8ContextKey) || !coreauth.IsAntigravityEnterpriseAccountAuth(auth) {
		return
	}
	root, account, ok := h.authManager.AntigravityEnterpriseAccountSnapshot(auth.ID)
	if !ok {
		return
	}
	children := map[string]*coreauth.Auth{}
	for _, child := range h.authManager.AntigravityEnterpriseBindings(root.ID) {
		children[ae.String(child.Metadata, "antigravity_enterprise_binding_id")] = child
	}
	now := time.Now().UTC()
	bindings := []gin.H{}
	for _, binding := range account.Bindings {
		settings := gin.H{}
		for key, value := range binding.Settings {
			if antigravityEnterpriseBindingField(key) && key != "disabled" {
				settings[key] = value
			}
		}
		b := gin.H{"binding_id": binding.ID, "revision": binding.Revision, "license": binding.License, "disabled": binding.Disabled, "effective_disabled": root.Disabled || root.Status == coreauth.StatusDisabled || binding.Disabled || binding.Missing, "missing": binding.Missing, "settings": settings}
		if child := children[binding.ID]; child != nil {
			b["effective_disabled"] = child.Disabled
			b["auth_index"], b["status"], b["status_message"] = lockedAuthIndex(child), child.Status, child.StatusMessage
			b["success"], b["failed"] = child.Success, child.Failed
			b["recent_requests"] = child.RecentRequestsSnapshot(now)
			b["cooldowns"] = coreauth.CooldownSnapshotForAuth(child, now)
		}
		bindings = append(bindings, b)
	}
	entry["antigravity_enterprise"] = gin.H{"account_id": account.ID, "revision": account.Revision, "account_management_supported": true, "bindings": bindings, "discovery": account.Discovery, "has_refresh_token": ae.String(root.Metadata, "refresh_token") != ""}
}

func (h *Handler) antigravityEnterpriseTarget(name, index, bindingID string) (*coreauth.Auth, bool) {
	root, _, ok := h.authManager.AntigravityEnterpriseAccountSnapshot(name)
	if !ok {
		return nil, false
	}
	children := h.authManager.AntigravityEnterpriseBindings(root.ID)
	if bindingID == "" && len(children) != 1 {
		return nil, false
	}
	for _, child := range children {
		if (bindingID == "" || ae.String(child.Metadata, "antigravity_enterprise_binding_id") == bindingID) && (index == "" || lockedAuthIndex(child) == index) {
			return child, true
		}
	}
	return nil, false
}
func (h *Handler) antigravityEnterpriseMetadata(ctx context.Context, auth *coreauth.Auth, refresh bool) (ae.Config, ae.Quota, error) {
	e, ok := h.authManager.Executor(ae.Provider)
	if !ok {
		return ae.Config{}, ae.Quota{}, fmt.Errorf("enterprise executor unavailable")
	}
	metadata, ok := e.(aeMetadataExecutor)
	if !ok {
		return ae.Config{}, ae.Quota{}, fmt.Errorf("enterprise metadata unavailable")
	}
	return metadata.Metadata(ctx, auth, refresh)
}

func (h *Handler) currentAntigravityEnterpriseTarget(c *gin.Context, before *coreauth.Auth) (*coreauth.Auth, bool) {
	current, ok := h.authManager.GetByID(before.ID)
	if !ok || !coreauth.IsAntigravityEnterpriseBindingAuth(current) || current.RegistrationEpoch != before.RegistrationEpoch || coreauth.BoundOwnerID(current) != coreauth.BoundOwnerID(before) || coreauth.AntigravityEnterpriseLicense(current) != coreauth.AntigravityEnterpriseLicense(before) {
		enterpriseManagementFailure(c, 409, "binding_changed", "Binding changed; refresh and retry")
		return nil, false
	}
	return current, true
}

func (h *Handler) antigravityEnterpriseReadTarget(c *gin.Context) (*coreauth.Auth, bool) {
	auth, ok := h.antigravityEnterpriseTarget(c.Query("name"), c.Query("auth_index"), c.Query("binding_id"))
	if !ok {
		enterpriseManagementFailure(c, 409, "binding_required", "Select a current subscription binding")
		return nil, false
	}
	if text, supplied := c.GetQuery("revision"); supplied {
		revision, err := strconv.ParseUint(text, 10, 64)
		current, _ := strconv.ParseUint(fmt.Sprint(auth.Metadata["antigravity_enterprise_binding_revision"]), 10, 64)
		if err != nil || revision == 0 || revision != current {
			enterpriseManagementFailure(c, 409, "binding_changed", "Binding revision changed; refresh and retry")
			return nil, false
		}
	}
	return auth, true
}

func (h *Handler) antigravityEnterpriseModelRoutes(auth *coreauth.Auth, catalog ae.Config) []coreauth.AntigravityEnterpriseModelRoute {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	return coreauth.ProjectAntigravityEnterpriseModels(cfg, auth, catalog)
}

func (h *Handler) discoverAntigravityEnterprise(ctx context.Context, base *coreauth.Auth) (*coreauth.Auth, error) {
	started := time.Now().UTC()
	fresh, err := h.authManager.EnsureAntigravityEnterpriseAccountToken(ctx, base.ID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if cfg == nil {
		return nil, errors.New("server configuration unavailable")
	}
	client := ae.SafeClient(util.SetProxy(&cfg.SDKConfig, &http.Client{}))
	licenses, unreachable, err := ae.FetchLicenses(ctx, client, ae.String(fresh.Metadata, "access_token"), ae.String(fresh.Metadata, "discovery_quota_project"))
	if err != nil {
		return nil, err
	}
	updated, err := h.authManager.UpdateAntigravityEnterpriseAccount(ctx, fresh, 0, func(account *ae.Account) error {
		merged, errMerge := ae.MergeLicenses(*account, licenses, unreachable, started)
		if errMerge == nil {
			*account = merged
		}
		return errMerge
	})
	if err == nil {
		err = h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{updated})
	}
	return updated, err
}
func (h *Handler) DiscoverAntigravityEnterpriseBindings(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var req struct {
		Name     string `json:"name"`
		Index    string `json:"auth_index"`
		Revision uint64 `json:"revision"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Name == "" || req.Revision == 0 {
		enterpriseManagementFailure(c, 400, "invalid_discovery", "name and revision are required")
		return
	}
	root, a, ok := h.authManager.AntigravityEnterpriseAccountSnapshot(req.Name)
	if !ok || a.Revision != req.Revision || req.Index != "" && lockedAuthIndex(root) != req.Index {
		enterpriseManagementFailure(c, 409, "credential_changed", "Account changed; refresh and retry")
		return
	}
	updated, err := h.discoverAntigravityEnterprise(c.Request.Context(), root)
	if err != nil {
		enterpriseManagementFailure(c, 502, "discovery_failed", err.Error())
		return
	}
	entry := gin.H{}
	h.addAntigravityEnterpriseSummary(c, entry, updated)
	c.JSON(200, entry)
}
func (h *Handler) PatchAntigravityEnterpriseBinding(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var req struct {
		Name      string         `json:"name"`
		Index     string         `json:"auth_index"`
		BindingID string         `json:"binding_id"`
		Revision  uint64         `json:"revision"`
		Fields    map[string]any `json:"fields"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if c.ShouldBindJSON(&req) != nil || req.Name == "" || req.Index == "" || req.BindingID == "" || req.Revision == 0 || len(req.Fields) == 0 {
		enterpriseManagementFailure(c, 400, "invalid_binding", "A current binding and fields are required")
		return
	}
	auth, ok := h.antigravityEnterpriseTarget(req.Name, req.Index, req.BindingID)
	if !ok {
		enterpriseManagementFailure(c, 409, "binding_changed", "Binding changed; refresh and retry")
		return
	}
	for key := range req.Fields {
		if !antigravityEnterpriseBindingField(key) {
			enterpriseManagementFailure(c, 400, "invalid_field", "Unsupported binding field: "+key)
			return
		}
	}
	if err := validateAntigravityEnterpriseFields(req.Fields); err != nil {
		enterpriseManagementFailure(c, 400, "invalid_field", err.Error())
		return
	}
	if value, exists := req.Fields["disabled"]; exists {
		if _, valid := value.(bool); !valid {
			enterpriseManagementFailure(c, 400, "invalid_field", "disabled must be a boolean")
			return
		}
	}
	updated, err := h.authManager.UpdateAntigravityEnterpriseAccount(c.Request.Context(), auth, 0, func(account *ae.Account) error {
		for i := range account.Bindings {
			b := &account.Bindings[i]
			if b.ID != req.BindingID {
				continue
			}
			if b.Revision != req.Revision {
				return fmt.Errorf("binding revision changed")
			}
			if b.Settings == nil {
				b.Settings = map[string]any{}
			}
			for k, v := range req.Fields {
				if k == "disabled" {
					b.Disabled = v.(bool)
				} else if v == nil {
					delete(b.Settings, k)
				} else {
					b.Settings[k] = v
				}
			}
			return nil
		}
		return fmt.Errorf("binding changed")
	})
	if err != nil {
		enterpriseManagementFailure(c, 409, "binding_changed", err.Error())
		return
	}
	if err = h.invokePostAuthPersistHooks(c.Request.Context(), []*coreauth.Auth{updated}); err != nil {
		enterpriseManagementFailure(c, 503, "runtime_sync_failed", "Binding saved; reload credentials")
		return
	}
	entry := gin.H{}
	h.addAntigravityEnterpriseSummary(c, entry, updated)
	c.JSON(200, entry)
}

func antigravityEnterpriseBindingField(key string) bool {
	switch key {
	case "disabled", "prefix", "proxy_url", "priority", "weight", "credential_weight", "note", "model_aliases", "oauth_model_alias", "excluded_models", "allowed_models", "disable_cooling", "request_retry", "request_scoped_errors", "headers", "max_concurrent_requests":
		return true
	}
	return false
}

func validateAntigravityEnterpriseFields(fields map[string]any) error {
	for key, value := range fields {
		if value == nil && key != "disabled" {
			continue
		}
		switch key {
		case "disabled", "disable_cooling":
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s must be a boolean", key)
			}
		case "prefix", "proxy_url", "note":
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s must be text", key)
			}
		case "weight", "credential_weight":
			weight, err := credentialweight.ParseValue(value)
			if err != nil {
				return err
			}
			fields[key] = weight
		case "priority", "request_retry", "max_concurrent_requests":
			n, ok := value.(float64)
			if !ok || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || math.Abs(n) > 1000000 || key == "max_concurrent_requests" && n < 0 {
				return fmt.Errorf("%s must be a bounded integer", key)
			}
			if key == "request_retry" && n < 0 {
				fields[key] = nil
			}
		case "excluded_models", "allowed_models":
			items, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s must be a model list", key)
			}
			for _, item := range items {
				if text, ok := item.(string); !ok || strings.TrimSpace(text) == "" {
					return fmt.Errorf("%s entries must be model names", key)
				}
			}
		case "headers":
			headers, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("headers must be an object")
			}
			for name, item := range headers {
				text, ok := item.(string)
				if !ok || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(text) {
					return fmt.Errorf("invalid header")
				}
			}
		case "model_aliases", "oauth_model_alias":
			data, _ := json.Marshal(value)
			var aliases []config.OAuthModelAlias
			if json.Unmarshal(data, &aliases) != nil || aliases == nil {
				return fmt.Errorf("model_aliases must be a list")
			}
			for _, alias := range aliases {
				if strings.TrimSpace(alias.Name) == "" || strings.TrimSpace(alias.Alias) == "" {
					return fmt.Errorf("model aliases require name and alias")
				}
			}
		case "request_scoped_errors":
			data, _ := json.Marshal(value)
			var rules []config.RequestScopedErrorRule
			if json.Unmarshal(data, &rules) != nil || rules == nil {
				return errors.New("request_scoped_errors must contain valid rules")
			}
			for _, rule := range rules {
				if rule.Status < 100 || rule.Status > 599 || len(rule.Match) == 0 && len(rule.MatchRegexr) == 0 {
					return errors.New("error rules require a valid status and match")
				}
				switch rule.Action {
				case "stop", "stop-and-cooldown", "continue", "continue-and-cooldown":
				default:
					return errors.New("unsupported error rule action")
				}
				for _, pattern := range rule.MatchRegexr {
					if _, err := regexp.Compile(pattern); err != nil {
						return errors.New("invalid error rule regular expression")
					}
				}
			}
		}
	}
	return nil
}
func (h *Handler) GetAntigravityEnterpriseQuota(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	auth, ok := h.antigravityEnterpriseReadTarget(c)
	if !ok {
		return
	}
	quota, err := h.antigravityEnterpriseQuotaRead(c.Request.Context(), auth, c.Query("refresh") == "true")
	auth, ok = h.currentAntigravityEnterpriseTarget(c, auth)
	if !ok {
		return
	}
	status := "ok"
	if err != nil {
		status = "unavailable"
		if previous, exists := h.authManager.AntigravityEnterpriseQuota(auth); exists {
			quota = previous
		}
	} else if !quota.ObservedAt.IsZero() {
		h.authManager.ObserveAntigravityEnterpriseQuota(auth, quota)
	}
	l := coreauth.AntigravityEnterpriseLicense(auth)
	response := gin.H{"provider": ae.Provider, "account_id": ae.String(auth.Metadata, "antigravity_enterprise_account_id"), "binding_id": ae.String(auth.Metadata, "antigravity_enterprise_binding_id"), "revision": auth.Metadata["antigravity_enterprise_binding_revision"], "pool_id": ae.Digest(l.PoolKey()), "pool_version": h.authManager.AntigravityEnterpriseQuotaVersion(auth), "project_id": l.ProjectID, "location": l.Location, "user_tier": l.UserTier, "quota": quota, "status": status, "stale": err != nil || quota.ObservedAt.IsZero() || time.Since(quota.ObservedAt) > ae.QuotaTTL}
	if err != nil {
		response["message"] = err.Error()
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, response)
}
func (h *Handler) getAntigravityEnterpriseModels(c *gin.Context) bool {
	root, ok := h.lookupAuthFile(c.Query("name"), "")
	if !ok || root.Provider != ae.Provider {
		return false
	}
	auth, ok := h.antigravityEnterpriseReadTarget(c)
	if !ok {
		return true
	}
	config, err := h.antigravityEnterpriseCatalog(c.Request.Context(), auth, false)
	auth, ok = h.currentAntigravityEnterpriseTarget(c, auth)
	if !ok {
		return true
	}
	if err != nil {
		enterpriseManagementFailure(c, 502, "catalog_unavailable", err.Error())
		return true
	}
	models := []gin.H{}
	for _, route := range h.antigravityEnterpriseModelRoutes(auth, config) {
		model := route.Model
		models = append(models, gin.H{"id": route.ID, "display_name": route.DisplayName, "native_model": model.ID, "upstream_model": model.Upstream, "thinking_level": model.Level, "catalog_source": model.Source, "upstream_listed": model.Source == "fetch_config", "generation_verified": false, "type": ae.Provider})
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"models": models, "binding_id": ae.String(auth.Metadata, "antigravity_enterprise_binding_id"), "revision": auth.Metadata["antigravity_enterprise_binding_revision"]})
	return true
}

// Dispatch before the legacy Enterprise request validator consumes the body.
func (h *Handler) dispatchAntigravityEnterpriseModelTest(c *gin.Context) bool {
	if !c.GetBool(ConfigV8ContextKey) || h.authManager == nil {
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10))
	c.Request.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		enterpriseManagementFailure(c, 400, "invalid_model_test", "Model test request is too large")
		return true
	}
	var probe struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return false
	}
	root, ok := h.lookupAuthFile(probe.Name, "")
	if !ok || root.Provider != ae.Provider {
		return false
	}
	h.testAntigravityEnterpriseModel(c)
	return true
}
func (h *Handler) testAntigravityEnterpriseModel(c *gin.Context) {
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var req struct {
		Name          string `json:"name"`
		Index         string `json:"auth_index"`
		BindingID     string `json:"binding_id"`
		Revision      uint64 `json:"expected_revision"`
		Model         string `json:"model"`
		AllowUnlisted bool   `json:"allow_unlisted_model"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Name == "" || req.Index == "" || req.BindingID == "" || req.Revision == 0 || req.Model == "" {
		enterpriseManagementFailure(c, 400, "invalid_model_test", "name, auth_index, binding_id, expected_revision and model are required")
		return
	}
	auth, ok := h.antigravityEnterpriseTarget(req.Name, req.Index, req.BindingID)
	if !ok {
		enterpriseManagementFailure(c, 409, "binding_changed", "Binding changed")
		return
	}
	_, graph, ok := h.authManager.AntigravityEnterpriseAccountSnapshot(auth.ID)
	currentRevision := uint64(0)
	for _, binding := range graph.Bindings {
		if binding.ID == req.BindingID {
			currentRevision = binding.Revision
		}
	}
	if !ok || currentRevision != req.Revision || auth.Disabled {
		enterpriseManagementFailure(c, 409, "binding_changed", "Binding changed or is disabled")
		return
	}
	if req.AllowUnlisted && req.Model != ae.CarbonModel {
		enterpriseManagementFailure(c, 400, "invalid_model_test", "Unlisted diagnostics support only gemini-4-carbon")
		return
	}
	nativeModel := req.Model
	if !req.AllowUnlisted {
		catalog, err := h.antigravityEnterpriseCatalog(c.Request.Context(), auth, false)
		var current bool
		auth, current = h.currentAntigravityEnterpriseTarget(c, auth)
		if !current {
			return
		}
		if err != nil {
			enterpriseManagementFailure(c, 502, "catalog_unavailable", err.Error())
			return
		}
		found := false
		for _, route := range h.antigravityEnterpriseModelRoutes(auth, catalog) {
			if route.ID == req.Model {
				nativeModel, found = route.Model.ID, true
				break
			}
		}
		if !found {
			enterpriseManagementFailure(c, 400, "model_not_available", "Model is not available under the selected binding's routing policy")
			return
		}
	}
	for _, cooldown := range coreauth.CooldownSnapshotForAuth(auth, time.Now()) {
		if cooldown.Scope == "credential" || cooldown.ModelKey == req.Model || cooldown.ModelKey == nativeModel {
			c.JSON(200, gin.H{"status": "cooling", "model": req.Model, "auth_index": req.Index, "binding_id": req.BindingID, "revision": currentRevision, "checked_at": time.Now().UTC(), "http_status": 429, "code": "model_cooldown", "retry_after_seconds": cooldown.RemainingSeconds})
			return
		}
	}
	l := coreauth.AntigravityEnterpriseLicense(auth)
	options := coreexec.Options{SourceFormat: sdktranslator.FormatOpenAI, Metadata: map[string]any{coreexec.PinnedAuthMetadataKey: auth.ID, "source": "management_model_test"}}
	options.Metadata["antigravity_enterprise_expected_model"] = nativeModel
	request := coreexec.Request{Model: req.Model, Payload: []byte(`{"messages":[{"role":"user","content":"Reply exactly OK."}],"max_tokens":1024}`)}
	usageContext := c.Copy()
	usageContext.Set("userApiKey", "")
	executionCtx := context.WithValue(c.Request.Context(), "gin", usageContext)
	start := time.Now()
	var response coreexec.Response
	var err error
	if req.AllowUnlisted && req.Model == ae.CarbonModel {
		options.Metadata["antigravity_enterprise_diagnostic_model"] = ae.CarbonModel
		ex, _ := h.authManager.Executor(ae.Provider)
		if ex == nil {
			enterpriseManagementFailure(c, 503, "executor_unavailable", "Enterprise executor unavailable")
			return
		}
		response, err = ex.Execute(executionCtx, auth, request, options)
		var upstream *ae.Error
		attempted := err == nil || errors.As(err, &upstream) && upstream.Attempted
		if attempted {
			result := coreauth.Result{AuthID: auth.ID, Provider: ae.Provider, Model: req.Model, Success: err == nil, RegistrationEpoch: auth.RegistrationEpoch, CredentialVersion: auth.CredentialVersion}
			if err != nil {
				result.Error = &coreauth.Error{Code: "antigravity_enterprise_diagnostic", Message: err.Error(), HTTPStatus: 502}
				if upstream != nil {
					result.Error.HTTPStatus = upstream.HTTP
					result.RetryAfter = upstream.RetryAfter()
				}
			}
			h.authManager.MarkResult(executionCtx, result)
		}
	} else {
		response, err = h.authManager.Execute(executionCtx, []string{ae.Provider}, request, options)
	}
	if c.Request.Context().Err() != nil {
		return
	}
	latest, exists := h.antigravityEnterpriseTarget(req.Name, req.Index, req.BindingID)
	if !exists || latest.RegistrationEpoch != auth.RegistrationEpoch {
		enterpriseManagementFailure(c, 409, "binding_changed", "Binding changed during testing")
		return
	}
	status, httpStatus, code, message := "ok", 200, "", ""
	if err != nil {
		status, httpStatus, code, message = "failed", 502, "upstream_error", err.Error()
		var upstream interface{ StatusCode() int }
		if errors.As(err, &upstream) {
			httpStatus = upstream.StatusCode()
		}
	} else if strings.TrimSpace(gjson.GetBytes(response.Payload, "choices.0.message.content").String()) == "" {
		status, httpStatus, code, message = "failed", 502, "empty_response", "No visible model response"
	} else if gjson.GetBytes(response.Payload, "choices.0.finish_reason").String() != "stop" {
		status, httpStatus, code, message = "failed", 502, "incomplete_response", "Model did not complete normally"
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"status": status, "model": req.Model, "auth_index": req.Index, "binding_id": req.BindingID, "revision": currentRevision, "checked_at": time.Now().UTC(), "elapsed_ms": time.Since(start).Milliseconds(), "http_status": httpStatus, "code": code, "message": message, "resources": gin.H{"project_id": l.ProjectID, "location": l.Location}})
}

// Weekly reset is a metadata recheck, never a local grant of additional quota.
func (h *Handler) resetAntigravityEnterpriseQuota(c *gin.Context, auth *coreauth.Auth, poolID string, version uint64) bool {
	if auth == nil || auth.Provider != ae.Provider {
		return false
	}
	if !h.enterpriseManagementAllowed(c) {
		return true
	}
	matched := false
	for _, view := range coreauth.CooldownSnapshotForAuth(auth, time.Now()) {
		if view.PoolID == poolID && view.PoolVersion == version && version != 0 {
			matched = true
			break
		}
	}
	if !matched {
		enterpriseManagementFailure(c, 409, "pool_changed", "Quota pool changed; refresh credentials before retrying")
		return true
	}
	quota, err := h.antigravityEnterpriseQuotaRead(c.Request.Context(), auth, true)
	auth, matched = h.currentAntigravityEnterpriseTarget(c, auth)
	if !matched {
		return true
	}
	if err != nil {
		enterpriseManagementFailure(c, 502, "quota_unavailable", err.Error())
		return true
	}
	if !h.authManager.ObserveAntigravityEnterpriseQuota(auth, quota) {
		enterpriseManagementFailure(c, 409, "binding_changed", "Binding changed during quota refresh")
		return true
	}
	phase := "normal"
	if h.authManager.CheckAntigravityEnterpriseQuota(auth, time.Now()) != nil {
		phase = "probe_pending"
		for _, view := range coreauth.CooldownSnapshotForAuth(auth, time.Now()) {
			if view.PoolID == poolID {
				phase = view.Phase
				break
			}
		}
	}
	indexes := []string{}
	l := coreauth.AntigravityEnterpriseLicense(auth)
	for _, candidate := range h.authManager.List() {
		if coreauth.IsAntigravityEnterpriseBindingAuth(candidate) && coreauth.AntigravityEnterpriseLicense(candidate).PoolKey() == l.PoolKey() {
			indexes = append(indexes, lockedAuthIndex(candidate))
		}
	}
	c.JSON(200, gin.H{"reset_scope": "shared_pool", "pool_id": poolID, "pool_version": h.authManager.AntigravityEnterpriseQuotaVersion(auth), "phase": phase, "affected_auth_indexes": indexes, "cooldowns": coreauth.CooldownSnapshotForAuth(auth, time.Now())})
	return true
}
