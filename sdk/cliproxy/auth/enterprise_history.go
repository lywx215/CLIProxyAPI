package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	log "github.com/sirupsen/logrus"
)

const enterpriseHistoryName = ".gemini-enterprise-history"

type enterpriseCredentialUploadContextKey struct{}

func withEnterpriseCredentialUpload(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, enterpriseCredentialUploadContextKey{}, true)
}

func (m *Manager) saveCredentialWithHistory(ctx context.Context, store Store, auth *Auth) error {
	path, err := store.Save(ctx, auth)
	if err == nil && path != "" && ctx != nil && (HasAuthCreationIntent(ctx) || ctx.Value(enterpriseCredentialUploadContextKey{}) == true) {
		m.RememberEnterpriseCredentialUpload(auth)
	}
	return err
}

// EnterpriseRequestHistory is the retained accounting view of an Enterprise application.
type EnterpriseRequestHistory struct {
	Success, Failed int64
	RecentRequests  []RecentRequestBucket
}

// EnterpriseCredentialLifetime stores the verified upload and legacy initialization times.
type EnterpriseCredentialLifetime struct {
	FirstUploadedAt time.Time
	InitializedAt   time.Time
}

type enterpriseHistoryKey struct {
	account, binding string
}

type enterpriseHistoryCounts struct {
	success, failed int64
	recent          recentRequestRing
	seeded          bool
}

func enterpriseHistoryAccountID(auth *Auth) string {
	if auth == nil || auth.Provider != ge.Provider || IsPluginVirtualAuth(auth) || enterpriseRetired(auth) {
		return ""
	}
	return ge.AccountID(ge.String(auth.Metadata, "email"), ge.String(auth.Metadata, "client_id"))
}

func validEnterpriseHistoryID(id string) bool {
	if len(id) != 64 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// RuntimeStateDir keeps local runtime state outside remote-backed auth mirrors.
// Local-file stores retain the configured directory, which is typically mounted.
func (m *Manager) RuntimeStateDir(authDir string) string {
	if m != nil {
		m.mu.RLock()
		store, ok := m.store.(interface{ LocalRuntimeStateDir() string })
		m.mu.RUnlock()
		if ok {
			if directory := strings.TrimSpace(store.LocalRuntimeStateDir()); directory != "" {
				return directory
			}
		}
	}
	return strings.TrimSpace(authDir)
}

// OpenEnterpriseHistory restores server-local history on the auth volume before
// accepting traffic and initializes the lifetime of loaded legacy credentials.
func (m *Manager) OpenEnterpriseHistory(authDir string) error {
	if m == nil || m.HomeEnabled() || m.enterpriseHistory.Load() != nil {
		return nil
	}
	directory := m.RuntimeStateDir(authDir)
	if directory == "" {
		return nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Enterprise history directory: %w", err)
	}
	path := filepath.Join(directory, enterpriseHistoryName)
	store, err := openEnterpriseHistory(path)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err = store.importBaseline(path+".baseline", now); err == nil {
		seen := make(map[string]bool)
		var legacy []enterpriseHistoryEvent
		for _, auth := range m.List() {
			if id := enterpriseHistoryAccountID(auth); id != "" && !IsEnterpriseCollectionAuth(auth) && !seen[id] {
				seen[id] = true
				if lifetime := store.lifetimes[id]; lifetime.FirstUploadedAt.IsZero() && lifetime.InitializedAt.IsZero() {
					legacy = append(legacy, enterpriseHistoryEvent{Account: id, Kind: historyUpload, InitializedAt: now})
				}
			}
		}
		err = store.appendEvents(legacy)
	}
	if err != nil {
		return fmt.Errorf("initialize Enterprise credential history: %w", errors.Join(err, store.closeFile()))
	}
	go store.run()
	if !m.enterpriseHistory.CompareAndSwap(nil, store) {
		return store.close()
	}
	return nil
}

// CloseEnterpriseHistory flushes pending writes before releasing the volume lock.
func (m *Manager) CloseEnterpriseHistory() error {
	if m != nil {
		if store := m.enterpriseHistory.Swap(nil); store != nil {
			return store.close()
		}
	}
	return nil
}

// RememberEnterpriseCredentialUpload records a successful local credential save.
// Only explicit upload/authorization and new registration paths call it.
func (m *Manager) RememberEnterpriseCredentialUpload(auth *Auth) {
	if m == nil || IsEnterpriseCollectionAuth(auth) {
		return
	}
	store := m.enterpriseHistory.Load()
	account := enterpriseHistoryAccountID(auth)
	if store == nil || account == "" {
		return
	}
	store.mu.RLock()
	_, known := store.lifetimes[account]
	store.mu.RUnlock()
	if !known {
		store.record(enterpriseHistoryEvent{Account: account, Kind: historyUpload, At: time.Now().UTC()})
	}
}

// EnterpriseFirstUploadedAt returns the timestamp recorded at the first local
// upload, or the zero value when the original upload predates retained evidence.
func (m *Manager) EnterpriseFirstUploadedAt(auth *Auth) time.Time {
	return m.EnterpriseCredentialLifetime(auth).FirstUploadedAt
}

// EnterpriseCredentialLifetime returns the persistent lifetime associated with the account.
func (m *Manager) EnterpriseCredentialLifetime(auth *Auth) EnterpriseCredentialLifetime {
	if m == nil {
		return EnterpriseCredentialLifetime{}
	}
	store := m.enterpriseHistory.Load()
	if store == nil {
		return EnterpriseCredentialLifetime{}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.lifetimes[enterpriseHistoryAccountID(auth)]
}

// EnterpriseRequestHistory returns durable counters. Discovery can canonicalize
// an old binding ID, so its retained aliases contribute to the same app row.
func (m *Manager) EnterpriseRequestHistory(accountID string, collection ge.Collection, now time.Time) (EnterpriseRequestHistory, bool) {
	if m == nil {
		return EnterpriseRequestHistory{}, false
	}
	store := m.enterpriseHistory.Load()
	if store == nil {
		return EnterpriseRequestHistory{}, false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	var total Auth
	found := false
	seen := make(map[string]bool, len(collection.LegacyAliases)+1)
	for _, binding := range append([]string{collection.ID}, collection.LegacyAliases...) {
		if seen[binding] {
			continue
		}
		seen[binding] = true
		counts := store.counts[enterpriseHistoryKey{account: accountID, binding: binding}]
		if counts == nil {
			continue
		}
		found = true
		total.Success += counts.success
		total.Failed += counts.failed
		for _, bucket := range counts.recent.buckets {
			addEnterpriseHistoryBucket(&total.recentRequests, bucket.bucketID, bucket.success, bucket.failed)
		}
	}
	if !found {
		return EnterpriseRequestHistory{}, false
	}
	return EnterpriseRequestHistory{Success: total.Success, Failed: total.Failed, RecentRequests: total.RecentRequestsSnapshot(now)}, true
}

func addEnterpriseHistoryBucket(ring *recentRequestRing, id, success, failed int64) {
	if success == 0 && failed == 0 {
		return
	}
	bucket := &ring.buckets[recentRequestBucketIndex(id)]
	if bucket.bucketID > id {
		return
	}
	if bucket.bucketID != id {
		*bucket = recentRequestBucket{bucketID: id}
	}
	bucket.success += success
	bucket.failed += failed
}

// Persist after releasing the routing/auth locks. The account and binding in
// projected auth IDs survive credential rotation and also identify late results.
func (m *Manager) recordEnterpriseHistory(result Result, now time.Time) {
	if m == nil || result.Provider != ge.Provider || result.AuthID == "" {
		return
	}
	store := m.enterpriseHistory.Load()
	if store == nil {
		return
	}
	if result.Error != nil && (result.Error.Code == "enterprise_local_gate" || result.Error.Code == "enterprise_model_gate") {
		return
	}
	var account, binding string
	if id, child := strings.CutPrefix(result.AuthID, "enterprise-collection:"); child {
		account, binding, _ = strings.Cut(id, ":")
	} else {
		m.mu.RLock()
		current := m.auths[result.AuthID]
		if current != nil && !isStaleExecutionResult(result, current) && !enterpriseRetired(current) {
			account, binding = enterpriseHistoryAccountID(current), EnterpriseBindingID(current)
		} else if transfer, exists := m.enterpriseAccounts.transfers[result.AuthID]; exists &&
			(result.RegistrationEpoch == 0 || result.RegistrationEpoch == transfer.sourceEpoch) {
			if child := m.auths[transfer.targetID]; child != nil && child.RegistrationEpoch == transfer.targetEpoch &&
				ge.Binding(ge.FromMetadata(child.Metadata)) == transfer.binding {
				account, binding = enterpriseHistoryAccountID(child), EnterpriseBindingID(child)
			}
		}
		m.mu.RUnlock()
	}
	if !validEnterpriseHistoryID(account) || !validEnterpriseHistoryID(binding) {
		return
	}
	status := statusCodeFromResult(result.Error)
	if result.Success {
		status = http.StatusOK
	}
	if status < 100 || status > 599 {
		status = 0
	}
	store.record(enterpriseHistoryEvent{
		Kind: historyResult, Account: account, Binding: binding, At: now.UTC(), Success: result.Success, Status: status,
	})
}

func (s *enterpriseHistoryStore) record(event enterpriseHistoryEvent) {
	if err := s.write(event); err != nil {
		s.logError.Do(func() { log.WithError(err).Error("failed to persist Gemini Enterprise credential history") })
	}
}
