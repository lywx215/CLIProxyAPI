package management

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type enterpriseRotationTarget struct {
	Name                  string `json:"name" form:"name"`
	AuthIndex             string `json:"auth_index" form:"auth_index"`
	AccountID             string `json:"account_id" form:"account_id"`
	AuthorizationRevision uint64 `json:"authorization_revision" form:"authorization_revision"`
	Project               string `json:"project" form:"project"`
	Location              string `json:"location" form:"location"`
}

type enterpriseRotationCollection struct {
	BindingID     string   `json:"binding_id"`
	AuthIndex     string   `json:"auth_index"`
	Engine        string   `json:"engine"`
	Revision      uint64   `json:"revision"`
	Disabled      bool     `json:"disabled"`
	RouteDisabled bool     `json:"route_disabled"`
	Routable      bool     `json:"routable"`
	Models        []string `json:"routing_models"`
	InFlight      int64    `json:"in_flight"`
}

type enterpriseRotationState struct {
	AccountID             string                         `json:"account_id"`
	AuthorizationRevision uint64                         `json:"authorization_revision"`
	AccountRevision       uint64                         `json:"account_revision"`
	Project               string                         `json:"project"`
	Location              string                         `json:"location"`
	HoldOwner             string                         `json:"hold_owner"`
	Collections           []enterpriseRotationCollection `json:"collections"`
}

type enterpriseRotationRequest struct {
	enterpriseRotationTarget
	ExpectedRevision uint64   `json:"expected_revision"`
	BindingIDs       []string `json:"binding_ids"`
	Owner            string   `json:"owner"`
	Action           string   `json:"action"`
}

var errEnterpriseRotationConflict = errors.New("application rotation state changed")
var newGeminiEnterpriseRotationClient = ge.Client

func (h *Handler) GeminiEnterpriseRotationCapabilities(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h.enterpriseManagementAllowed(c) {
		c.JSON(http.StatusOK, gin.H{"protocol": 1, "scoped_drain": true, "refresh_while_disabled": true})
	}
}

func (h *Handler) enterpriseRotationState(target enterpriseRotationTarget) (*coreauth.Auth, enterpriseRotationState, error) {
	state := enterpriseRotationState{}
	scope := ge.RotationScope(target.Project, target.Location)
	if scope == "" || target.Name == "" || target.AuthIndex == "" || target.AccountID == "" || target.AuthorizationRevision == 0 {
		return nil, state, errEnterpriseRotationConflict
	}
	base, ok := h.lookupAuthFile(target.Name, target.AuthIndex)
	if !ok || base == nil || !coreauth.IsEnterpriseAccountAuth(base) || base.FileName != target.Name || lockedAuthIndex(base) != target.AuthIndex || coreauth.IsPluginVirtualAuth(base) {
		return nil, state, errEnterpriseRotationConflict
	}
	root, account, ok := h.authManager.EnterpriseAccountSnapshot(base.ID)
	if !ok || root.Disabled || root.Status == coreauth.StatusDisabled || account.ID != target.AccountID || coreauth.EnterpriseOAuthAuthorizationRevision(root) != target.AuthorizationRevision {
		return nil, state, errEnterpriseRotationConflict
	}
	state = enterpriseRotationState{AccountID: account.ID, AuthorizationRevision: target.AuthorizationRevision,
		AccountRevision: account.Revision, Project: target.Project, Location: target.Location, HoldOwner: account.RotationHolds[scope]}
	children := make(map[string]*coreauth.Auth)
	for _, child := range h.authManager.EnterpriseCollections(root.ID) {
		children[coreauth.EnterpriseBindingID(child)] = child
	}
	for _, collection := range account.Collections {
		if ge.RotationScope(collection.ProjectID, collection.Location) != scope {
			continue
		}
		child := children[collection.ID]
		if child == nil || collection.Missing || (state.HoldOwner != "" && !child.Disabled) {
			return nil, state, errEnterpriseRotationConflict
		}
		models := []string{}
		if !child.Disabled && child.Status != coreauth.StatusDisabled {
			for _, model := range registry.GetGlobalRegistry().GetModelsForClient(child.ID) {
				if model != nil && model.ID != "" {
					models = append(models, model.ID)
				}
			}
			sort.Strings(models)
		}
		state.Collections = append(state.Collections, enterpriseRotationCollection{
			BindingID: collection.ID, AuthIndex: lockedAuthIndex(child),
			Engine: ge.Engine(ge.FromMetadata(child.Metadata)), Revision: collection.Revision,
			Disabled: collection.Disabled, RouteDisabled: child.Disabled || child.Status == coreauth.StatusDisabled,
			Routable: len(models) > 0, Models: models,
			InFlight: h.authManager.EnterpriseRotationInFlight(child.ID),
		})
	}
	latest, graph, ok := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if len(state.Collections) == 0 || !ok || latest.Disabled || latest.Status == coreauth.StatusDisabled ||
		graph.Revision != account.Revision || latest.RegistrationEpoch != root.RegistrationEpoch ||
		coreauth.EnterpriseOAuthAuthorizationRevision(latest) != target.AuthorizationRevision {
		return nil, state, errEnterpriseRotationConflict
	}
	sort.Slice(state.Collections, func(i, j int) bool { return state.Collections[i].BindingID < state.Collections[j].BindingID })
	return root, state, nil
}

func (state enterpriseRotationState) matches(request enterpriseRotationRequest) bool {
	if state.AccountRevision != request.ExpectedRevision || len(state.Collections) != len(request.BindingIDs) {
		return false
	}
	// Sorting a copy keeps the request available for an exact readback.
	bindings := append([]string(nil), request.BindingIDs...)
	sort.Strings(bindings)
	for i, item := range state.Collections {
		if bindings[i] != item.BindingID {
			return false
		}
	}
	return true
}

func enterpriseRotationChanged(c *gin.Context) {
	enterpriseManagementFailure(c, http.StatusConflict, "rotation_state_changed", "The exact Enterprise authorization or application set changed")
}

func (h *Handler) GetGeminiEnterpriseRotation(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var target enterpriseRotationTarget
	if c.ShouldBindQuery(&target) != nil {
		enterpriseRotationChanged(c)
		return
	}
	_, state, err := h.enterpriseRotationState(target)
	if err != nil {
		enterpriseRotationChanged(c)
		return
	}
	c.JSON(http.StatusOK, state)
}

func (h *Handler) PatchGeminiEnterpriseRotation(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var request enterpriseRotationRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	if c.ShouldBindJSON(&request) != nil || !ge.ValidRotationOwner(request.Owner) {
		enterpriseManagementFailure(c, http.StatusBadRequest, "rotation_invalid", "A rotation task owner and exact application set are required")
		return
	}
	switch request.Action {
	case "hold", "release", "retire", "activate":
	default:
		enterpriseManagementFailure(c, http.StatusBadRequest, "rotation_invalid", "Unknown application rotation action")
		return
	}
	root, before, err := h.enterpriseRotationState(request.enterpriseRotationTarget)
	if err != nil || !before.matches(request) || (before.HoldOwner != "" && before.HoldOwner != request.Owner) ||
		(request.Action != "hold" && before.HoldOwner != request.Owner) {
		enterpriseRotationChanged(c)
		return
	}
	if request.Action == "retire" || request.Action == "activate" {
		for _, row := range before.Collections {
			if !row.RouteDisabled || row.InFlight != 0 {
				enterpriseManagementFailure(c, http.StatusConflict, "rotation_drain_pending", "The application region is still serving admitted requests")
				return
			}
		}
	}
	scope := ge.RotationScope(request.Project, request.Location)
	ctx := c.Request.Context()
	updated, err := h.authManager.UpdateEnterpriseRotation(ctx, root, before.AccountRevision, request.AuthorizationRevision, func(account *ge.Account) error {
		if request.Action == "hold" {
			if account.RotationHolds == nil {
				account.RotationHolds = map[string]string{}
			}
			account.RotationHolds[scope] = request.Owner
			return nil
		}
		if request.Action == "retire" || request.Action == "activate" {
			for i := range account.Collections {
				item := &account.Collections[i]
				if ge.RotationScope(item.ProjectID, item.Location) == scope {
					item.Disabled = request.Action == "retire"
				}
			}
		}
		delete(account.RotationHolds, scope)
		if len(account.RotationHolds) == 0 {
			account.RotationHolds = nil
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, coreauth.ErrEnterpriseAccountChanged) {
			enterpriseRotationChanged(c)
		} else {
			enterpriseManagementFailure(c, http.StatusInternalServerError, "rotation_save_unknown", "The rotation write must be checked by reading the application state")
		}
		return
	}
	if err = h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{updated}); err != nil {
		enterpriseManagementFailure(c, http.StatusInternalServerError, "rotation_runtime_sync_failed", "The change was saved but runtime publication needs reconciliation")
		return
	}
	_, state, err := h.enterpriseRotationState(request.enterpriseRotationTarget)
	if err != nil {
		enterpriseManagementFailure(c, http.StatusInternalServerError, "rotation_readback_pending", "The saved rotation change could not be read back")
		return
	}
	c.JSON(http.StatusOK, state)
}

// RefreshGeminiEnterpriseRotation reads upstream metadata using the exact
// paused application. No discovery or routing settings are changed.
func (h *Handler) RefreshGeminiEnterpriseRotation(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var request enterpriseRotationRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	if c.ShouldBindJSON(&request) != nil || !ge.ValidRotationOwner(request.Owner) {
		enterpriseManagementFailure(c, http.StatusBadRequest, "rotation_invalid", "A rotation task owner and exact application set are required")
		return
	}
	root, before, err := h.enterpriseRotationState(request.enterpriseRotationTarget)
	if err != nil || !before.matches(request) || before.HoldOwner != request.Owner {
		enterpriseRotationChanged(c)
		return
	}
	ctx := c.Request.Context()
	modelsByEngine := make(map[string][]ge.Model, len(before.Collections))
	var observation ge.SubscriptionObservation
	for i, item := range before.Collections {
		child, ok := h.enterpriseTarget(request.Name, request.AuthIndex, item.BindingID)
		if !ok {
			enterpriseRotationChanged(c)
			return
		}
		fresh, err := h.authManager.EnsureEnterpriseAccountToken(ctx, child.ID)
		if err != nil || fresh == nil {
			enterpriseManagementFailure(c, http.StatusUnprocessableEntity, "credential_unavailable", "Could not access the saved Google authorization for this application")
			return
		}
		binding := ge.FromMetadata(fresh.Metadata)
		if ge.Engine(binding) != item.Engine {
			enterpriseRotationChanged(c)
			return
		}
		client, err := newGeminiEnterpriseRotationClient(h.enterpriseProxy(fresh))
		if err != nil {
			enterpriseManagementFailure(c, http.StatusBadGateway, "rotation_google_unavailable", "Could not open the application metadata connection")
			return
		}
		if i == 0 {
			h.authManager.InvalidateEnterpriseSubscription(fresh)
			flight := h.authManager.BeginEnterpriseSubscriptionRefresh(fresh)
			if flight == nil {
				client.CloseIdleConnections()
				enterpriseManagementFailure(c, http.StatusConflict, "rotation_refresh_pending", "Another subscription refresh is in progress")
				return
			}
			observation = ge.ResolveSubscription(ctx, client, binding, ge.String(fresh.Metadata, "access_token"), time.Now())
			accepted := flight.Complete(observation)
			flight.Cancel()
			if !accepted || !observation.Authoritative() {
				client.CloseIdleConnections()
				c.JSON(http.StatusOK, gin.H{"status": "pending", "subscription": observation})
				return
			}
			h.authManager.PublishEnterpriseSubscriptionIdentity(fresh)
		}
		paid := observation.Status == "ok" && observation.FreeTrial != nil && !*observation.FreeTrial
		if paid {
			models, err := ge.Discover(ctx, client, binding, ge.String(fresh.Metadata, "access_token"))
			client.CloseIdleConnections()
			if err != nil {
				c.JSON(http.StatusOK, gin.H{"status": "pending", "subscription": observation, "catalog_error": ge.CatalogError(err)})
				return
			}
			if err = h.authManager.PersistEnterpriseRotationCatalog(ctx, fresh, models, time.Now(), request.Owner); err != nil {
				enterpriseManagementFailure(c, http.StatusConflict, "rotation_catalog_pending", "The exact application catalog could not be published")
				return
			}
			modelsByEngine[item.Engine] = models
		} else {
			client.CloseIdleConnections()
		}
	}
	latest, after, err := h.enterpriseRotationState(request.enterpriseRotationTarget)
	if err != nil || !after.matches(request) || after.HoldOwner != request.Owner ||
		latest.RegistrationEpoch != root.RegistrationEpoch {
		enterpriseRotationChanged(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "subscription": observation, "catalogs": modelsByEngine, "application": after})
}
