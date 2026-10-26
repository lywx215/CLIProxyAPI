package management

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var newGeminiEnterpriseQuotaClient = ge.Client
var resolveGeminiEnterpriseQuotaIdentity = ge.ResolveQuotaIdentity
var ensureGeminiEnterpriseManagementToken = func(ctx context.Context, manager *coreauth.Manager, id string) (*coreauth.Auth, error) {
	return manager.EnsureEnterpriseAccountToken(ctx, id)
}

type enterpriseQuotaEntry struct {
	snapshot ge.QuotaSnapshot
	identity *ge.QuotaIdentity
	expires  time.Time
}

type enterpriseQuotaFlight struct {
	done    chan struct{}
	entry   enterpriseQuotaEntry
	waiters int
	cancel  context.CancelFunc
}

type enterpriseQuotaCache struct {
	mu      sync.Mutex
	entries map[string]enterpriseQuotaEntry
	flights map[string]*enterpriseQuotaFlight
}

type enterpriseQuotaResponse struct {
	ge.QuotaSnapshot
	SelectedPool *ge.QuotaPool `json:"selected_pool"`
	AccountID    string        `json:"account_id,omitempty"`
	BindingID    string        `json:"binding_id,omitempty"`
	Revision     uint64        `json:"revision,omitempty"`
}

// GetGeminiEnterpriseQuota is a management read, including for disabled
// collections. Neither observations nor permission failures alter routing.
func (h *Handler) GetGeminiEnterpriseQuota(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	now := time.Now().UTC()
	result := enterpriseQuotaResponse{QuotaSnapshot: ge.QuotaSnapshot{Status: "unavailable", ObservedAt: now, Pools: []ge.QuotaPool{}}}
	respond := func() { c.JSON(http.StatusOK, result) }
	if h.authManager == nil {
		respond()
		return
	}
	auth := h.authByIndex(strings.TrimSpace(c.Query("auth_index")))
	if auth == nil || auth.Provider != ge.Provider {
		respond()
		return
	}
	if coreauth.IsEnterpriseAccountAuth(auth) {
		var ok bool
		auth, ok = h.enterpriseTarget(auth.FileName, lockedAuthIndex(auth), strings.TrimSpace(c.Query("binding_id")))
		if !ok {
			enterpriseManagementFailure(c, 409, "collection_required", "Select a specific collection to read its quota")
			return
		}
	}
	bindingID := coreauth.EnterpriseBindingID(auth)
	if bindingID == "" {
		bindingID = ge.CollectionID(ge.Resources(ge.FromMetadata(auth.Metadata)))
	}
	if requested := strings.TrimSpace(c.Query("binding_id")); requested != "" && requested != bindingID {
		enterpriseManagementFailure(c, 409, "credential_changed", "Collection changed; reload and try again")
		return
	}
	binding := ge.FromMetadata(auth.Metadata)
	if binding.Validate() != nil {
		respond()
		return
	}
	result.BindingID = bindingID
	result.AccountID = ge.AccountID(ge.String(auth.Metadata, "email"), binding.ClientID)
	if _, graph, ok := h.authManager.EnterpriseAccountSnapshot(auth.ID); ok {
		result.AccountID = graph.ID
		for _, collection := range graph.Collections {
			if collection.ID == bindingID {
				result.Revision = collection.Revision
				break
			}
		}
	}
	fresh, err := ensureGeminiEnterpriseManagementToken(c.Request.Context(), h.authManager, auth.ID)
	if err != nil || fresh == nil {
		respond()
		return
	}
	if ge.Resources(ge.FromMetadata(fresh.Metadata)) != ge.Resources(binding) {
		enterpriseManagementFailure(c, 409, "credential_changed", "Collection changed during refresh; reload")
		return
	}
	proxy := h.enterpriseProxy(fresh)
	day := enterpriseQuotaDay(now)
	owner := coreauth.EnterpriseOwnerID(fresh)
	if owner == "" {
		owner = fresh.ID
	}
	authorization := sha256.Sum256([]byte(binding.ClientID + "\x00" + ge.String(fresh.Metadata, "refresh_token")))
	key := fmt.Sprintf("%s/%s/%s/%s/%x/%s", owner, binding.ProjectID, binding.Location, day, authorization, proxy)
	entry, ok := h.enterpriseQuotas.read(c.Request.Context(), key, c.Query("refresh") == "true", now, func(ctx context.Context) enterpriseQuotaEntry {
		client, errClient := newGeminiEnterpriseQuotaClient(proxy)
		if errClient != nil {
			return enterpriseQuotaEntry{snapshot: ge.QuotaSnapshot{Status: "unavailable", ProjectID: binding.ProjectID, Location: binding.Location, ObservedAt: now, Pools: []ge.QuotaPool{}}}
		}
		defer client.CloseIdleConnections()
		token := ge.String(fresh.Metadata, "access_token")
		identity := fresh.EnterpriseQuotaIdentity
		if !identity.ValidFor(binding) || identity.CheckedAt.After(now) || now.Sub(identity.CheckedAt) > time.Hour {
			identity = resolveGeminiEnterpriseQuotaIdentity(ctx, client, binding, token, now)
		}
		quotaBinding := binding
		if identity.ValidFor(binding) {
			quotaBinding.ProjectID = identity.ProjectNumber
		}
		snapshot := ge.FetchQuota(ctx, client, quotaBinding, token, now)
		snapshot.ProjectID = binding.ProjectID
		return enterpriseQuotaEntry{snapshot: snapshot, identity: identity, expires: now.Add(2 * time.Minute)}
	})
	if !ok || c.Request.Context().Err() != nil {
		return
	}
	current, exists := h.authManager.GetByID(auth.ID)
	if !exists || current == nil || ge.Resources(ge.FromMetadata(current.Metadata)) != ge.Resources(binding) {
		enterpriseManagementFailure(c, 409, "credential_changed", "Collection changed during quota reading; reload")
		return
	}
	result.QuotaSnapshot = ge.CloneQuotaSnapshot(entry.snapshot)
	identity := entry.identity
	if identity != nil && identity.Verified && identity.Location == binding.Location {
		// Project-level license identity is shared by this account's applications.
		copyIdentity := *identity
		copyIdentity.Binding = ge.Binding(binding)
		identity = &copyIdentity
	}
	result.SelectedPool = ge.SelectQuotaPool(result.QuotaSnapshot, identity, binding)
	respond()
}

func enterpriseQuotaDay(now time.Time) string {
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return now.UTC().Format("2006-01-02")
	}
	return now.In(zone).Format("2006-01-02")
}

func (cache *enterpriseQuotaCache) read(ctx context.Context, key string, force bool, now time.Time, fetch func(context.Context) enterpriseQuotaEntry) (enterpriseQuotaEntry, bool) {
	if ctx.Err() != nil {
		return enterpriseQuotaEntry{}, false
	}
	cache.mu.Lock()
	if !force {
		if entry, ok := cache.entries[key]; ok && now.Before(entry.expires) {
			cache.mu.Unlock()
			return entry, true
		}
	}
	if cache.flights == nil {
		cache.flights = map[string]*enterpriseQuotaFlight{}
	}
	flight := cache.flights[key]
	if flight == nil {
		fetchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &enterpriseQuotaFlight{done: make(chan struct{}), cancel: cancel}
		cache.flights[key] = flight
		go func() {
			entry := fetch(fetchCtx)
			cache.mu.Lock()
			if cache.entries == nil {
				cache.entries = map[string]enterpriseQuotaEntry{}
			}
			if previous, ok := cache.entries[key]; ok {
				entry.snapshot = ge.MergeQuotaObservations(previous.snapshot, entry.snapshot)
			}
			if fetchCtx.Err() == nil && cache.flights[key] == flight {
				if len(cache.entries) >= 256 {
					for cachedKey, cached := range cache.entries {
						if !now.Before(cached.expires) {
							delete(cache.entries, cachedKey)
						}
					}
				}
				cache.entries[key] = entry
			}
			flight.entry = entry
			if cache.flights[key] == flight {
				delete(cache.flights, key)
			}
			close(flight.done)
			cancel()
			cache.mu.Unlock()
		}()
	}
	flight.waiters++
	cache.mu.Unlock()
	select {
	case <-ctx.Done():
		cache.mu.Lock()
		flight.waiters--
		if flight.waiters == 0 && cache.flights[key] == flight {
			delete(cache.flights, key)
			flight.cancel()
		}
		cache.mu.Unlock()
		return enterpriseQuotaEntry{}, false
	case <-flight.done:
		return flight.entry, ctx.Err() == nil
	}
}
