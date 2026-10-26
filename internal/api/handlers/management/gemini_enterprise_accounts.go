package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/credentialweight"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var errEnterpriseCollectionChanged = errors.New("enterprise collection changed")

func enterpriseManagementFailure(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": code, "code": code, "message": message})
}

func (h *Handler) enterpriseManagementAllowed(c *gin.Context) bool {
	if h == nil || h.authManager == nil {
		enterpriseManagementFailure(c, http.StatusServiceUnavailable, "credential_unavailable", "Credential management is unavailable")
		return false
	}
	h.mu.Lock()
	home := h.cfg != nil && h.cfg.Home.Enabled
	h.mu.Unlock()
	if home || h.authManager.HomeEnabled() {
		enterpriseManagementFailure(c, http.StatusConflict, "home_managed", "Gemini Enterprise account management is unavailable in Home mode")
		return false
	}
	return true
}

// enterpriseTarget requires an explicit collection for multi-binding accounts.
// Administrative account operations deliberately use lookupAuthFile instead.
func (h *Handler) enterpriseTarget(name, index, bindingID string) (*coreauth.Auth, bool) {
	auth, ok := h.lookupAuthFile(strings.TrimSpace(name), strings.TrimSpace(index))
	if !ok || auth == nil || auth.Provider != ge.Provider || coreauth.IsPluginVirtualAuth(auth) {
		return nil, false
	}
	if coreauth.IsEnterpriseAccountAuth(auth) {
		children := h.authManager.EnterpriseCollections(auth.ID)
		if bindingID == "" && len(children) != 1 {
			return nil, false
		}
		for _, child := range children {
			if child != nil && (bindingID == "" || coreauth.EnterpriseBindingID(child) == bindingID) {
				return child, true
			}
		}
		return nil, false
	}
	actualID := coreauth.EnterpriseBindingID(auth)
	if actualID == "" {
		actualID = ge.CollectionID(ge.Resources(ge.FromMetadata(auth.Metadata)))
	}
	if bindingID != "" && bindingID != actualID {
		return nil, false
	}
	return auth, true
}

func (h *Handler) enterpriseAccountSummary(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return gin.H{}
	}
	root, account, ok := h.authManager.EnterpriseAccountSnapshot(auth.ID)
	if !ok {
		root = auth
		var err error
		account, err = ge.AccountFromLegacy(auth.Metadata)
		if err != nil {
			return geminiEnterpriseCredentialSummary(auth.Metadata)
		}
	}
	summary := geminiEnterpriseCredentialSummary(root.Metadata)
	summary["account_id"], summary["revision"] = account.ID, account.Revision
	summary["account_management_supported"] = true
	summary["model_policy_supported"] = true
	summary["discovery"] = ge.CloneAccount(account).Discovery
	children := map[string]*coreauth.Auth{}
	for _, child := range h.authManager.EnterpriseCollections(root.ID) {
		if child != nil {
			children[coreauth.EnterpriseBindingID(child)] = child
		}
	}
	now := time.Now().UTC()
	collections := make([]gin.H, 0, len(account.Collections))
	for _, collection := range account.Collections {
		child := children[collection.ID]
		if child == nil && !coreauth.IsEnterpriseAccountAuth(root) {
			child = root
		}
		entry := gin.H{
			"binding_id": collection.ID, "revision": collection.Revision,
			"project_id": collection.ProjectID, "project_name": collection.ProjectName, "location": collection.Location,
			"collection_id": collection.CollectionID, "engine_id": collection.EngineID, "assistant_id": collection.AssistantID,
			"disabled": collection.Disabled, "effective_disabled": root.Disabled || collection.Disabled,
			"missing": collection.Missing, "unavailable": collection.Missing,
			"success": int64(0), "failed": int64(0), "recent_requests": []any{}, "cooldowns": nil,
			"settings": enterpriseCollectionSettings(collection.Settings),
		}
		if child != nil {
			entry["model_policy_state"] = h.authManager.EnterpriseModelPolicyView(child)
			unavailable, status, message, _ := reconcileAuthFileCooldownState(child, now)
			entry["auth_index"], entry["status"], entry["status_message"] = lockedAuthIndex(child), status, message
			entry["unavailable"] = unavailable || collection.Missing
			entry["effective_disabled"] = child.Disabled || root.Disabled || collection.Disabled
			entry["success"], entry["failed"] = child.Success, child.Failed
			entry["recent_requests"] = child.RecentRequestsSnapshot(now)
			if !h.authManager.HomeEnabled() {
				entry["cooldowns"] = coreauth.CooldownSnapshotForAuth(child, now)
			}
		}
		collections = append(collections, entry)
	}
	summary["collections"] = collections
	return summary
}

// Account-level consumers retain one row while traffic belongs to collections.
func (h *Handler) addEnterpriseAccountSummary(entry gin.H, auth *coreauth.Auth) {
	summary := h.enterpriseAccountSummary(auth)
	entry["gemini_enterprise"] = summary
	if !coreauth.IsEnterpriseAccountAuth(auth) {
		return
	}
	collections, _ := summary["collections"].([]gin.H)
	var success, failed int64
	var buckets []coreauth.RecentRequestBucket
	for _, collection := range collections {
		succeeded, _ := collection["success"].(int64)
		failures, _ := collection["failed"].(int64)
		success += succeeded
		failed += failures
		observed, _ := collection["recent_requests"].([]coreauth.RecentRequestBucket)
		if buckets == nil && len(observed) > 0 {
			buckets = make([]coreauth.RecentRequestBucket, len(observed))
			for i := range observed {
				buckets[i].Time = observed[i].Time
			}
		}
		for i := range observed {
			if i < len(buckets) && buckets[i].Time == observed[i].Time {
				buckets[i].Success += observed[i].Success
				buckets[i].Failed += observed[i].Failed
			}
		}
	}
	entry["success"], entry["failed"] = success, failed
	if buckets != nil {
		entry["recent_requests"] = buckets
	}
}

func enterpriseCollectionSettings(settings map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"prefix", "proxy_url", "note", "headers", "weight", "priority", "model_policy", "allowed_models", "excluded_models", "model_aliases", "oauth_model_alias", "disable_cooling", "request_retry", "request_scoped_errors", "credential_weight", "max_concurrent_requests"} {
		if value, exists := settings[key]; exists {
			result[key] = value
		}
	}
	return result
}

func (h *Handler) enterpriseProxy(auth *coreauth.Auth) string {
	h.mu.Lock()
	proxy := ""
	if h.cfg != nil {
		proxy = h.cfg.ProxyURL
	}
	h.mu.Unlock()
	if auth != nil && auth.ProxyURL != "" {
		proxy = auth.ProxyURL
	}
	return proxy
}

// DiscoverGeminiEnterpriseResources scans read-only metadata after OAuth is
// safely stored. Failed scopes never remove existing collection settings.
func (h *Handler) DiscoverGeminiEnterpriseResources(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var req struct {
		Name             string `json:"name"`
		AuthIndex        string `json:"auth_index"`
		AccountID        string `json:"account_id"`
		ExpectedRevision uint64 `json:"expected_revision"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	if c.ShouldBindJSON(&req) != nil || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.AuthIndex) == "" {
		enterpriseManagementFailure(c, 400, "invalid_discovery", "name and account auth_index are required")
		return
	}
	root, ok := h.lookupAuthFile(req.Name, req.AuthIndex)
	if !ok || root == nil || root.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(root) || coreauth.IsPluginVirtualAuth(root) {
		enterpriseManagementFailure(c, 404, "credential_not_found", "Gemini Enterprise account not found")
		return
	}
	account, err := ge.AccountFromLegacy(root.Metadata)
	if err != nil || (req.AccountID != "" && req.AccountID != account.ID) || (req.ExpectedRevision != 0 && req.ExpectedRevision != account.Revision) {
		enterpriseManagementFailure(c, 409, "credential_changed", "Account changed; reload before discovering resources")
		return
	}
	updated, added, merged, warnings, err := h.discoverEnterpriseAccount(c.Request.Context(), root)
	if c.Request.Context().Err() != nil {
		return
	}
	if err != nil {
		enterpriseManagementFailure(c, 422, "discovery_failed", "Resource discovery failed; the saved authorization and existing collections are retained")
		return
	}
	projectIDs := map[string]bool{}
	if graph, errRead := ge.ReadAccount(updated.Metadata); errRead == nil && graph != nil {
		addedSet := map[string]bool{}
		knownProjects := map[string]bool{}
		for _, id := range added {
			addedSet[id] = true
		}
		for _, collection := range graph.Collections {
			if !addedSet[collection.ID] {
				knownProjects[collection.ProjectID] = true
			}
		}
		for _, collection := range graph.Collections {
			if addedSet[collection.ID] && !knownProjects[collection.ProjectID] {
				projectIDs[collection.ProjectID] = true
			}
		}
	}
	status := "ok"
	if len(warnings) > 0 {
		status = "partial"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "gemini_enterprise": h.enterpriseAccountSummary(updated), "added_binding_ids": added, "added_project_count": len(projectIDs), "added_collection_count": len(added), "merged_sources": merged, "warnings": warnings})
}

func (h *Handler) discoverEnterpriseAccount(ctx context.Context, base *coreauth.Auth) (*coreauth.Auth, []string, []string, []ge.DiscoveryWarning, error) {
	root, err := h.authManager.EnsureEnterpriseAccountToken(ctx, base.ID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	client, err := newGeminiEnterpriseResourceClient(h.enterpriseProxy(root))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer client.CloseIdleConnections()
	token := ge.String(root.Metadata, "access_token")
	email, err := ge.Email(ctx, client, token)
	if err != nil || !strings.EqualFold(strings.TrimSpace(email), ge.String(root.Metadata, "email")) {
		return nil, nil, nil, nil, errors.New("authorized account identity could not be verified")
	}
	account, err := ge.AccountFromLegacy(root.Metadata)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	clientID := ge.String(root.Metadata, "client_id")
	verified := []string{}
	warnings := []ge.DiscoveryWarning{}
	for _, source := range h.authManager.List() {
		if source == nil || source.ID == root.ID || source.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(source) || coreauth.IsPluginVirtualAuth(source) || ge.String(source.Metadata, "enterprise_retired_to") != "" || ge.String(source.Metadata, "client_id") != clientID || !strings.EqualFold(ge.String(source.Metadata, "email"), email) {
			continue
		}
		fresh, errFresh := h.authManager.EnsureEnterpriseAccountToken(ctx, source.ID)
		if errFresh != nil {
			warnings = append(warnings, ge.DiscoveryWarning{Code: "merge_verification_unavailable", Message: "An older credential could not be verified and was retained separately."})
			continue
		}
		sourceClient, errClient := newGeminiEnterpriseResourceClient(h.enterpriseProxy(fresh))
		if errClient != nil {
			warnings = append(warnings, ge.DiscoveryWarning{Code: "merge_verification_unavailable", Message: "An older credential could not be verified and was retained separately."})
			continue
		}
		sourceEmail, errEmail := ge.Email(ctx, sourceClient, ge.String(fresh.Metadata, "access_token"))
		sourceClient.CloseIdleConnections()
		if errEmail == nil && strings.EqualFold(sourceEmail, email) {
			verified = append(verified, source.ID)
		} else {
			warnings = append(warnings, ge.DiscoveryWarning{Code: "merge_verification_unavailable", Message: "An older credential could not be verified and was retained separately."})
		}
	}
	// Merge selected-source settings before discovery, so existing choices are
	// passed to the enumerator rather than being replaced by a sorted default.
	merged := []string{}
	if len(verified) > 0 {
		root, merged, err = h.authManager.MergeEnterpriseAccounts(ctx, root.ID, verified)
		if err != nil {
			if root == nil {
				return nil, nil, merged, warnings, err
			}
			warnings = append(warnings, ge.DiscoveryWarning{Code: "merge_retirement_pending", Message: "The merged account was saved; an older credential recovery record could not be updated."})
		}
		account, err = ge.AccountFromLegacy(root.Metadata)
		if err != nil {
			return nil, nil, merged, warnings, err
		}
	}
	result, errDiscovery := ge.DiscoverResources(ctx, client, ge.FromMetadata(root.Metadata), ge.String(root.Metadata, "access_token"), account.Collections)
	if ctx.Err() != nil {
		return nil, nil, merged, warnings, ctx.Err()
	}
	warnings = append(warnings, result.Warnings...)
	added := []string{}
	updated, errUpdate := h.authManager.UpdateEnterpriseAccount(ctx, root, 0, func(current *ge.Account) error {
		*current, added = ge.MergeDiscoveredCollections(*current, result)
		return nil
	})
	if errUpdate != nil {
		return nil, nil, merged, warnings, errUpdate
	}
	for _, child := range h.authManager.EnterpriseCollections(updated.ID) {
		h.authManager.InvalidateEnterpriseSubscription(child)
	}
	if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{updated}); errHook != nil {
		return nil, nil, merged, warnings, errHook
	}
	if errDiscovery != nil && result.Status == "failed" {
		return updated, added, merged, warnings, errDiscovery
	}
	return updated, added, merged, warnings, nil
}

// PatchGeminiEnterpriseCollection changes one saved collection with optimistic
// concurrency. Engine and assistant edits preserve the collection's identity.
func (h *Handler) PatchGeminiEnterpriseCollection(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var req struct {
		Name             string              `json:"name"`
		AuthIndex        string              `json:"auth_index"`
		BindingID        string              `json:"binding_id"`
		ExpectedRevision uint64              `json:"expected_revision"`
		Disabled         *bool               `json:"disabled"`
		Resources        *ge.ResourceBinding `json:"resources"`
		Fields           map[string]any      `json:"fields"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
	if c.ShouldBindJSON(&req) != nil || req.Name == "" || req.AuthIndex == "" || req.BindingID == "" || req.ExpectedRevision == 0 || (req.Disabled == nil && req.Resources == nil && len(req.Fields) == 0) {
		enterpriseManagementFailure(c, 400, "invalid_collection", "name, collection auth_index, binding_id, expected_revision and changes are required")
		return
	}
	auth, ok := h.enterpriseTarget(req.Name, req.AuthIndex, req.BindingID)
	if !ok || lockedAuthIndex(auth) != strings.TrimSpace(req.AuthIndex) {
		enterpriseManagementFailure(c, 409, "credential_changed", "Collection changed; reload and try again")
		return
	}
	root, account, exists := h.authManager.EnterpriseAccountSnapshot(auth.ID)
	if !exists {
		root = auth
		var err error
		account, err = ge.AccountFromLegacy(auth.Metadata)
		if err != nil {
			enterpriseManagementFailure(c, 409, "credential_changed", "Collection is unavailable")
			return
		}
	}
	var selected *ge.Collection
	for i := range account.Collections {
		if account.Collections[i].ID == req.BindingID {
			selected = &account.Collections[i]
			break
		}
	}
	if selected == nil || selected.Revision != req.ExpectedRevision {
		enterpriseManagementFailure(c, 409, "credential_changed", "Collection changed; reload and try again")
		return
	}
	coreauth.NormalizeCredentialMetadata(req.Fields)
	fields := enterpriseCollectionSettings(req.Fields)
	if len(fields) != len(req.Fields) {
		enterpriseManagementFailure(c, 400, "invalid_collection_fields", "Unsupported collection settings")
		return
	}
	if err := validateEnterpriseCollectionFields(fields); err != nil {
		enterpriseManagementFailure(c, 400, "invalid_collection_fields", err.Error())
		return
	}
	if req.Resources != nil {
		binding := req.Resources.Apply(ge.FromMetadata(auth.Metadata))
		if binding.Validate() != nil || ge.CollectionID(ge.Resources(binding)) != req.BindingID {
			enterpriseManagementFailure(c, 400, "invalid_resources", "Keep the collection project and region; discover resources to add another collection")
			return
		}
		normalized := ge.Resources(binding)
		req.Resources = &normalized
		fresh, errRefresh := h.authManager.EnsureEnterpriseAccountToken(c.Request.Context(), auth.ID)
		if errRefresh != nil {
			enterpriseManagementFailure(c, 422, "credential_unavailable", "Cannot refresh the saved Google authorization")
			return
		}
		client, errClient := newGeminiEnterpriseResourceClient(h.enterpriseProxy(fresh))
		if errClient != nil {
			enterpriseManagementFailure(c, 422, "resource_validation_failed", "Cannot create the configured Google connection")
			return
		}
		errValidate := ge.ValidateResources(c.Request.Context(), client, binding, ge.String(fresh.Metadata, "access_token"))
		if errValidate == nil {
			errValidate = validateEnterpriseAssistant(c.Request.Context(), client, binding, ge.String(fresh.Metadata, "access_token"))
		}
		client.CloseIdleConnections()
		if c.Request.Context().Err() != nil {
			return
		}
		if errValidate != nil {
			enterpriseManagementFailure(c, 422, "resource_validation_failed", "Google could not validate the selected application and assistant")
			return
		}
	}
	updated, errUpdate := h.authManager.UpdateEnterpriseAccount(c.Request.Context(), root, 0, func(current *ge.Account) error {
		for i := range current.Collections {
			collection := &current.Collections[i]
			if collection.ID != req.BindingID {
				continue
			}
			if collection.Revision != req.ExpectedRevision {
				return errEnterpriseCollectionChanged
			}
			if req.Disabled != nil {
				collection.Disabled = *req.Disabled
			}
			if req.Resources != nil {
				collection.ResourceBinding = *req.Resources
				collection.Missing = false
			}
			if collection.Settings == nil {
				collection.Settings = map[string]any{}
			}
			for key, value := range fields {
				if value == nil {
					delete(collection.Settings, key)
					continue
				}
				if key == "headers" {
					copyAuth := &coreauth.Auth{Metadata: map[string]any{"headers": collection.Settings["headers"]}}
					applyAuthFileHeadersPatch(copyAuth, value)
					if headers, exists := copyAuth.Metadata["headers"]; exists {
						collection.Settings[key] = headers
					} else {
						delete(collection.Settings, key)
					}
					continue
				}
				collection.Settings[key] = value
			}
			collection.Revision++
			return nil
		}
		return errEnterpriseCollectionChanged
	})
	if c.Request.Context().Err() != nil {
		return
	}
	if errUpdate != nil {
		if errors.Is(errUpdate, errEnterpriseCollectionChanged) {
			enterpriseManagementFailure(c, 409, "credential_changed", "Collection changed while saving; reload and try again")
		} else {
			enterpriseManagementFailure(c, 500, "collection_save_failed", "Could not save collection settings")
		}
		return
	}
	if errHook := h.invokePostAuthPersistHooks(c.Request.Context(), []*coreauth.Auth{updated}); errHook != nil {
		enterpriseManagementFailure(c, 500, "runtime_sync_failed", "Collection saved, but runtime synchronization failed; reload")
		return
	}
	c.JSON(200, gin.H{"status": "ok", "gemini_enterprise": h.enterpriseAccountSummary(updated)})
}

func validateEnterpriseCollectionFields(fields map[string]any) error {
	policyFields := map[string]any{}
	for _, key := range []string{"model_policy", "allowed_models"} {
		if value, exists := fields[key]; exists && value != nil {
			policyFields[key] = value
		}
	}
	if _, _, err := ge.ModelPolicySettings(policyFields); err != nil {
		return err
	}
	for key, value := range fields {
		if value == nil {
			if key == "model_policy" || key == "allowed_models" || key == "weight" || key == "request_retry" || key == "model_aliases" || key == "request_scoped_errors" || key == "headers" {
				continue
			}
			return errors.New("this collection setting cannot be null")
		}
		switch key {
		case "prefix", "proxy_url", "note":
			if _, ok := value.(string); !ok {
				return errors.New("prefix and proxy_url must be strings")
			}
		case "disable_cooling":
			if _, ok := value.(bool); !ok {
				return errors.New("disable_cooling must be boolean")
			}
		case "weight", "credential_weight":
			weight, err := credentialweight.ParseValue(value)
			if err != nil {
				return err
			}
			fields[key] = weight
		case "priority", "request_retry", "max_concurrent_requests":
			number, ok := value.(float64)
			if !ok || number != float64(int64(number)) || number < -1000000 || number > 1000000 {
				return errors.New("numeric settings must be bounded integers")
			}
			if key == "request_retry" && number < 0 {
				fields[key] = nil
			}
		case "headers":
			items, ok := value.(map[string]any)
			if !ok {
				return errors.New("headers must be an object")
			}
			for name, raw := range items {
				text, ok := raw.(string)
				if !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(text, "\r\n") {
					return errors.New("headers must contain valid header names and string values")
				}
			}
		case "excluded_models":
			items, ok := value.([]any)
			if !ok {
				return errors.New("excluded_models must be an array")
			}
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return errors.New("excluded_models must contain strings")
				}
			}
		case "model_aliases", "oauth_model_alias":
			data, _ := json.Marshal(value)
			var aliases []config.OAuthModelAlias
			if json.Unmarshal(data, &aliases) != nil {
				return errors.New("model_aliases must contain valid alias rules")
			}
			for _, alias := range aliases {
				if strings.TrimSpace(alias.Name) == "" || strings.TrimSpace(alias.Alias) == "" {
					return errors.New("model aliases require a name and alias")
				}
			}
		case "request_scoped_errors":
			data, _ := json.Marshal(value)
			var rules []config.RequestScopedErrorRule
			if json.Unmarshal(data, &rules) != nil {
				return errors.New("request_scoped_errors must contain valid rules")
			}
			for _, rule := range rules {
				if rule.Status < 100 || rule.Status > 599 || (len(rule.Match) == 0 && len(rule.MatchRegexr) == 0) {
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

func validateEnterpriseAssistant(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string) error {
	resource := ge.Engine(binding) + "/assistants/" + binding.WithDefaults().AssistantID
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ge.OriginForLocation(binding.Location)+"/v1/"+resource, nil)
	if err != nil || !ge.OfficialBinding(request, binding.ProjectID, binding.Location) {
		return errors.New("invalid assistant resource")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Goog-User-Project", binding.ProjectID)
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := copyClient.Do(request)
	if err != nil {
		return errors.New("assistant resource could not be read")
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	var assistant struct {
		Name string `json:"name"`
	}
	if err != nil || response.StatusCode != 200 || len(data) > 1024*1024 || json.Unmarshal(data, &assistant) != nil || assistant.Name != resource {
		return errors.New("assistant resource could not be verified")
	}
	return nil
}
