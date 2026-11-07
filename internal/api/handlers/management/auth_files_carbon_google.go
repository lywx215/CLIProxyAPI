package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const carbonGoogleSnapshotLifetime = 15 * time.Minute

type carbonGoogleApplicationRequest struct {
	BindingID        string             `json:"binding_id"`
	AuthIndex        string             `json:"auth_index"`
	ExpectedRevision uint64             `json:"expected_revision"`
	Expected         ge.ResourceBinding `json:"expected"`
}

type carbonGoogleConfigureRequest struct {
	Name             string                           `json:"name"`
	AuthIndex        string                           `json:"auth_index"`
	AccountID        string                           `json:"account_id"`
	ExpectedRevision uint64                           `json:"expected_revision"`
	SnapshotID       string                           `json:"snapshot_id"`
	Applications     []carbonGoogleApplicationRequest `json:"applications"`
}

type carbonGoogleApplicationResult struct {
	BindingID        string `json:"binding_id"`
	AuthIndex        string `json:"auth_index"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Revision         uint64 `json:"revision"`
	ge.CarbonGoogleConfigureResult
}

type carbonGoogleFrozenApplication struct {
	authID, authIndex, configuration string
	proxy                            string
	resources                        ge.ResourceBinding
	revision                         uint64
}

type carbonGoogleSnapshot struct {
	id, ownerID, accountID, authorization string
	ownerEpoch, rootRevision              uint64
	authorizationRevision                 uint64
	manager                               *coreauth.Manager
	expires                               time.Time
	applications                          map[string]carbonGoogleFrozenApplication
}

type carbonGoogleEngineLock struct {
	gate chan struct{}
	refs int
}

type enterpriseCarbonGoogleCoordinator struct {
	mu        sync.Mutex
	now       func() time.Time
	active    int
	running   map[string]bool
	snapshots map[string]*carbonGoogleSnapshot
	engines   map[string]*carbonGoogleEngineLock
}

func (c *enterpriseCarbonGoogleCoordinator) clock() time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *enterpriseCarbonGoogleCoordinator) begin(ownerID string) (func(), string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[ownerID] {
		return nil, "operation_in_progress"
	}
	if c.active >= 2 {
		return nil, "configure_busy"
	}
	if c.running == nil {
		c.running = map[string]bool{}
	}
	c.running[ownerID], c.active = true, c.active+1
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.running, ownerID)
			c.active--
			c.mu.Unlock()
		})
	}, ""
}

func (c *enterpriseCarbonGoogleCoordinator) lockEngine(ctx context.Context, name string) (func(), error) {
	c.mu.Lock()
	if c.engines == nil {
		c.engines = map[string]*carbonGoogleEngineLock{}
	}
	lock := c.engines[name]
	if lock == nil {
		lock = &carbonGoogleEngineLock{gate: make(chan struct{}, 1)}
		c.engines[name] = lock
	}
	lock.refs++
	c.mu.Unlock()
	releaseReference := func() {
		c.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(c.engines, name)
		}
		c.mu.Unlock()
	}
	select {
	case lock.gate <- struct{}{}:
	case <-ctx.Done():
		releaseReference()
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-lock.gate; releaseReference() }) }, nil
}

func (h *Handler) carbonGoogleSnapshot(req carbonGoogleConfigureRequest, root *coreauth.Auth, account ge.Account) (*carbonGoogleSnapshot, string) {
	coordinator := &h.enterpriseCarbonGoogle
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	now := coordinator.clock()
	for id, snapshot := range coordinator.snapshots {
		if !now.Before(snapshot.expires) {
			delete(coordinator.snapshots, id)
		}
	}
	if req.SnapshotID != "" {
		snapshot := coordinator.snapshots[req.SnapshotID]
		if snapshot == nil {
			return nil, "snapshot_expired"
		}
		if snapshot.ownerID != root.ID || snapshot.accountID != account.ID || snapshot.manager != h.authManager || snapshot.rootRevision != req.ExpectedRevision {
			return nil, "credential_changed"
		}
		return snapshot, ""
	}
	applicationCount := len(account.Collections)
	for id, previous := range coordinator.snapshots {
		if previous.ownerID == root.ID {
			delete(coordinator.snapshots, id)
		} else {
			applicationCount += len(previous.applications)
		}
	}
	if len(coordinator.snapshots) >= 128 || applicationCount > 8192 {
		return nil, "configure_busy"
	}
	authorization := ge.CarbonAuthorizationFingerprint(root.Metadata)
	if authorization == "" {
		return nil, "credential_changed"
	}
	snapshot := &carbonGoogleSnapshot{id: uuid.NewString(), ownerID: root.ID, accountID: account.ID, authorization: authorization, ownerEpoch: root.RegistrationEpoch, authorizationRevision: h.authManager.EnterpriseCarbonAuthorizationSnapshotRevision(root.ID), rootRevision: account.Revision, manager: h.authManager, expires: now.Add(carbonGoogleSnapshotLifetime), applications: map[string]carbonGoogleFrozenApplication{}}
	children := map[string]*coreauth.Auth{}
	for _, child := range h.authManager.EnterpriseCollections(root.ID) {
		children[coreauth.EnterpriseBindingID(child)] = child
	}
	for _, collection := range account.Collections {
		child := children[collection.ID]
		if child == nil && !coreauth.IsEnterpriseAccountAuth(root) {
			child = root
		}
		if child == nil || collection.Missing {
			continue
		}
		resources := ge.Resources(collection.ResourceBinding.Apply(ge.FromMetadata(root.Metadata)))
		snapshot.applications[collection.ID] = carbonGoogleFrozenApplication{authID: child.ID, authIndex: lockedAuthIndex(child), configuration: h.authManager.EnterpriseCarbonEvidenceFingerprint(child).Configuration, proxy: h.enterpriseProxy(child), resources: resources, revision: collection.Revision}
	}
	if coordinator.snapshots == nil {
		coordinator.snapshots = map[string]*carbonGoogleSnapshot{}
	}
	coordinator.snapshots[snapshot.id] = snapshot
	return snapshot, ""
}

func (h *Handler) carbonGoogleCurrent(snapshot *carbonGoogleSnapshot, bindingID string) (*coreauth.Auth, uint64, bool) {
	if snapshot == nil || snapshot.manager != h.authManager || !h.enterpriseCarbonGoogle.clock().Before(snapshot.expires) ||
		!h.authManager.EnterpriseCarbonAuthorizationSnapshotMatches(snapshot.ownerID, snapshot.ownerEpoch, snapshot.authorizationRevision, snapshot.authorization) {
		return nil, 0, false
	}
	frozen, exists := snapshot.applications[bindingID]
	if !exists {
		return nil, 0, false
	}
	root, ok := h.authManager.GetByID(snapshot.ownerID)
	if !ok {
		return nil, 0, false
	}
	account, err := ge.AccountFromLegacy(root.Metadata)
	if err != nil || account.ID != snapshot.accountID || account.Revision != snapshot.rootRevision {
		return nil, 0, false
	}
	for _, collection := range account.Collections {
		if collection.ID != bindingID {
			continue
		}
		resources := ge.Resources(collection.ResourceBinding.Apply(ge.FromMetadata(root.Metadata)))
		if collection.Revision != frozen.revision || resources != frozen.resources || collection.Missing {
			return nil, collection.Revision, false
		}
		current, ok := h.authManager.GetByID(frozen.authID)
		if !ok || current.Provider != ge.Provider || coreauth.EnterpriseOwnerID(current) != snapshot.ownerID || ge.Resources(ge.FromMetadata(current.Metadata)) != frozen.resources || h.authManager.EnterpriseCarbonEvidenceFingerprint(current).Configuration != frozen.configuration || h.enterpriseProxy(current) != frozen.proxy {
			return nil, collection.Revision, false
		}
		return current, collection.Revision, true
	}
	return nil, 0, false
}

// ConfigureGeminiEnterpriseCarbonGoogle is an authenticated, bounded foreground
// operation. It never generates content or changes local admission/proof state.
func (h *Handler) ConfigureGeminiEnterpriseCarbonGoogle(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enterpriseManagementAllowed(c) {
		return
	}
	var req carbonGoogleConfigureRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.AuthIndex) == "" || req.ExpectedRevision == 0 || len(req.Applications) == 0 || len(req.Applications) > 5 {
		enterpriseManagementFailure(c, 400, "invalid_carbon_configuration", "Select one to five imported applications with their current revisions and resources")
		return
	}
	root, ok := h.lookupAuthFile(req.Name, req.AuthIndex)
	if !ok || root == nil || root.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(root) || coreauth.IsPluginVirtualAuth(root) || coreauth.IsEnterpriseRetiredAuth(root) || lockedAuthIndex(root) != req.AuthIndex {
		enterpriseManagementFailure(c, 404, "credential_not_found", "Gemini Enterprise OAuth account not found")
		return
	}
	account, err := ge.AccountFromLegacy(root.Metadata)
	if err != nil || req.ExpectedRevision != account.Revision || req.AccountID != "" && req.AccountID != account.ID {
		enterpriseManagementFailure(c, 409, "credential_changed", "Account changed; reload before configuring Google Carbon")
		return
	}
	release, reason := h.enterpriseCarbonGoogle.begin(root.ID)
	if reason != "" {
		code := http.StatusConflict
		if reason == "configure_busy" {
			code = http.StatusTooManyRequests
		}
		enterpriseManagementFailure(c, code, reason, "Google Carbon configuration is already running; try again after it completes")
		return
	}
	defer release()
	snapshot, reason := h.carbonGoogleSnapshot(req, root, account)
	if reason != "" {
		enterpriseManagementFailure(c, 409, reason, "The configuration snapshot is no longer current; reload and start again")
		return
	}
	seen := map[string]bool{}
	for _, application := range req.Applications {
		frozen, exists := snapshot.applications[application.BindingID]
		if !exists || seen[application.BindingID] || application.ExpectedRevision == 0 || application.ExpectedRevision != frozen.revision || application.Expected != frozen.resources || application.AuthIndex != "" && application.AuthIndex != frozen.authIndex {
			enterpriseManagementFailure(c, 409, "credential_changed", "Select only the current imported application resources from this snapshot")
			return
		}
		seen[application.BindingID] = true
	}
	results := make([]carbonGoogleApplicationResult, 0, len(req.Applications))
	cache := &ge.CarbonGoogleConfigureCache{}
	partial := false
	ctx := c.Request.Context()
	for _, application := range req.Applications {
		if ctx.Err() != nil {
			return
		}
		frozen := snapshot.applications[application.BindingID]
		result := carbonGoogleApplicationResult{BindingID: application.BindingID, AuthIndex: frozen.authIndex, ExpectedRevision: application.ExpectedRevision, Revision: frozen.revision}
		current, revision, valid := h.carbonGoogleCurrent(snapshot, application.BindingID)
		if !valid {
			result.Revision = revision
			result.CarbonGoogleConfigureResult = ge.CarbonGoogleConfigureResult{Status: "changed", Reason: "credential_changed", HTTPStatus: 409}
		} else {
			fresh, errToken := h.authManager.EnsureEnterpriseAccountToken(ctx, current.ID)
			if errToken != nil {
				result.CarbonGoogleConfigureResult = ge.CarbonGoogleConfigureResult{Status: "failed", Reason: "google_authorization_required", HTTPStatus: 401}
			} else if _, _, valid = h.carbonGoogleCurrent(snapshot, application.BindingID); !valid {
				result.CarbonGoogleConfigureResult = ge.CarbonGoogleConfigureResult{Status: "changed", Reason: "credential_changed", HTTPStatus: 409}
			} else {
				result.CarbonGoogleConfigureResult = h.configureCarbonGoogleApplication(ctx, snapshot, application.BindingID, fresh, cache)
			}
		}
		if result.Status == "failed" || result.Status == "changed" || result.Status == "verification_required" {
			partial = true
		}
		results = append(results, result)
	}
	if ctx.Err() != nil {
		return
	}
	status := "completed"
	if partial {
		status = "partial"
	}
	body := gin.H{"status": status, "snapshot_id": snapshot.id, "snapshot_expires_at": snapshot.expires, "applications": results, "google_carbon_configure_supported": true}
	if fresh, exists := h.authManager.GetByID(root.ID); exists {
		body["gemini_enterprise"] = h.enterpriseAccountSummary(fresh)
	}
	c.JSON(http.StatusOK, body)
}

func (h *Handler) configureCarbonGoogleApplication(ctx context.Context, snapshot *carbonGoogleSnapshot, bindingID string, fresh *coreauth.Auth, cache *ge.CarbonGoogleConfigureCache) ge.CarbonGoogleConfigureResult {
	binding := ge.FromMetadata(fresh.Metadata)
	release, err := h.enterpriseCarbonGoogle.lockEngine(ctx, ge.Engine(binding))
	if err != nil {
		return ge.CarbonGoogleConfigureResult{Status: "failed", Reason: "cancelled"}
	}
	defer release()
	client, err := newGeminiEnterpriseResourceClient(h.enterpriseProxy(fresh))
	if err != nil {
		return ge.CarbonGoogleConfigureResult{Status: "failed", Reason: "google_unavailable"}
	}
	defer client.CloseIdleConnections()
	guard := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, _, valid := h.carbonGoogleCurrent(snapshot, bindingID); !valid {
			return ge.ErrCarbonGoogleCredentialChanged
		}
		return nil
	}
	result := ge.ConfigureCarbonGoogle(ctx, client, binding, ge.String(fresh.Metadata, "access_token"), guard, cache)
	if _, _, valid := h.carbonGoogleCurrent(snapshot, bindingID); !valid {
		return ge.CarbonGoogleConfigureResult{Status: "changed", Reason: "credential_changed", HTTPStatus: 409}
	}
	return result
}
