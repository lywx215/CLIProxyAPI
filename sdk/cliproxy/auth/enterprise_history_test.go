package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func openTestEnterpriseHistory(t *testing.T, m *Manager, directory string) {
	t.Helper()
	if err := m.OpenEnterpriseHistory(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.CloseEnterpriseHistory(); err != nil {
			t.Errorf("close history: %v", err)
		}
	})
}

func historyForFixture(t *testing.T, m *Manager, root *Auth, now time.Time) EnterpriseRequestHistory {
	t.Helper()
	_, account, ok := m.EnterpriseAccountSnapshot(root.ID)
	if !ok {
		t.Fatal("missing Enterprise account")
	}
	view, ok := m.EnterpriseRequestHistory(account.ID, account.Collections[0], now)
	if !ok {
		t.Fatal("missing Enterprise request history")
	}
	return view
}

func TestEnterpriseCredentialHistorySurvivesRestartAndReplacement(t *testing.T) {
	directory := t.TempDir()
	store := &enterpriseAccountStore{}
	m := NewManager(store, nil, nil)
	openTestEnterpriseHistory(t, m, directory)
	parent := enterpriseAccountFixture(t, 2)
	parent.Metadata["access_token"] = "secret-history-access-token"
	store.failID = parent.ID
	if _, err := m.Register(t.Context(), parent); err == nil || !m.EnterpriseFirstUploadedAt(parent).IsZero() {
		t.Fatal("a failed credential save started the clock")
	}
	store.failID = ""
	before := time.Now().UTC()
	parent = registerEnterpriseFixture(t, m, parent)
	first := m.EnterpriseFirstUploadedAt(parent)
	if first.Before(before) || first.After(time.Now()) {
		t.Fatalf("unexpected upload timestamp: %v", first)
	}
	child := m.EnterpriseCollections(parent.ID)[0]
	const successful = 24
	var calls sync.WaitGroup
	for range successful {
		calls.Go(func() {
			m.MarkResult(t.Context(), Result{AuthID: child.ID, Provider: ge.Provider, Success: true})
		})
	}
	calls.Wait()
	m.MarkResult(t.Context(), Result{AuthID: child.ID, Provider: ge.Provider, Error: &Error{
		Code: "enterprise_quota", HTTPStatus: http.StatusTooManyRequests, Message: "private-upstream-error-body",
	}})
	m.MarkResult(t.Context(), Result{AuthID: child.ID, Provider: ge.Provider, Error: &Error{
		Code: "enterprise_local_gate", HTTPStatus: http.StatusTooManyRequests,
	}})

	parent.Disabled, parent.Status = true, StatusDisabled
	if _, err := m.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	renewed := parent.Clone()
	renewed.Metadata["access_token"] = "secret-changed-token"
	if _, err := m.ReauthorizeEnterpriseAccount(t.Context(), renewed); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseEnterpriseHistory(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(directory, enterpriseHistoryName+".v1.wal"))
	if err != nil || bytes.Count(body, []byte(`"status_code":429`)) != 1 || bytes.Contains(body, []byte("private-upstream")) || bytes.Contains(body, []byte("secret-")) {
		t.Fatalf("journal did not isolate status data from secrets: %v", err)
	}

	restarted := NewManager(store, nil, nil)
	if err := restarted.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	openTestEnterpriseHistory(t, restarted, directory)
	if got := restarted.EnterpriseFirstUploadedAt(parent); !got.Equal(first) {
		t.Fatalf("first upload changed after reauthorization/restart: %v, want %v", got, first)
	}
	now := time.Now()
	view := historyForFixture(t, restarted, parent, now)
	var recentSuccess, recentFailed int64
	for _, bucket := range view.RecentRequests {
		recentSuccess += bucket.Success
		recentFailed += bucket.Failed
	}
	if view.Success != successful || view.Failed != 1 || recentSuccess != successful || recentFailed != 1 {
		t.Fatalf("history did not survive restart: totals %d/%d, recent %d/%d", view.Success, view.Failed, recentSuccess, recentFailed)
	}
	view = historyForFixture(t, restarted, parent, now.Add(4*time.Hour))
	for _, bucket := range view.RecentRequests {
		if bucket.Success != 0 || bucket.Failed != 0 {
			t.Fatal("expired request still appeared in recent buckets")
		}
	}
	if view.Success != successful || view.Failed != 1 {
		t.Fatal("expiry of recent requests changed cumulative counters")
	}

	restarted.Remove(t.Context(), parent.ID)
	restored := registerEnterpriseFixture(t, restarted, renewed)
	if got := restarted.EnterpriseFirstUploadedAt(restored); !got.Equal(first) {
		t.Fatal("same credential started a new clock after removal and re-upload")
	}
	newChild := restarted.EnterpriseCollections(restored.ID)[0]
	restarted.MarkResult(t.Context(), Result{AuthID: newChild.ID, Provider: ge.Provider, Success: true})
	if got := historyForFixture(t, restarted, restored, time.Now()).Success; got != successful+1 {
		t.Fatalf("new traffic after restart started from an incorrect total: %d", got)
	}

	otherServer := NewManager(store, nil, nil)
	if err := otherServer.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	openTestEnterpriseHistory(t, otherServer, t.TempDir())
	otherServer.RememberEnterpriseCredentialUpload(restored)
	if got := otherServer.EnterpriseFirstUploadedAt(restored); !got.IsZero() {
		t.Fatalf("a legacy credential with unknown first upload was assigned %v", got)
	}
}

func TestEnterpriseHistoryBaselineReplayAndInterruptedAppend(t *testing.T) {
	directory := t.TempDir()
	parent := enterpriseAccountFixture(t, 1)
	account, err := ge.ReadAccount(parent.Metadata)
	if err != nil || account == nil {
		t.Fatal("missing fixture account", err)
	}
	path := filepath.Join(directory, enterpriseHistoryName)
	captured := time.Date(2024, 11, 20, 2, 15, 0, 0, time.UTC)
	bucketAt := captured.Truncate(10 * time.Minute)
	first := captured.Add(-48 * time.Hour)
	baseline := enterpriseHistorySeed{
		Version: 1, CapturedAt: captured,
		Accounts: []enterpriseHistorySeedAccount{{AccountID: account.ID, FirstUploadedAt: first,
			Collections: []enterpriseHistorySeedCollection{{BindingID: account.Collections[0].ID,
				enterpriseHistorySeedStats: enterpriseHistorySeedStats{Success: 120, Failed: 3,
					Recent: []enterpriseHistorySeedBucket{{At: bucketAt, Success: 5, Failed: 2}},
				},
			}},
		}},
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".baseline", data, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, nil)
	openTestEnterpriseHistory(t, m, directory)
	registerEnterpriseFixture(t, m, parent)
	if err := m.CloseEnterpriseHistory(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(path+".v1.wal", os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_, err = file.Write([]byte{0, 0, 1})
	if err = errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	resumed := NewManager(nil, nil, nil)
	openTestEnterpriseHistory(t, resumed, directory)
	registerEnterpriseFixture(t, resumed, parent)
	if got := resumed.EnterpriseFirstUploadedAt(parent); !got.Equal(first) {
		t.Fatalf("verified legacy first upload = %v, want %v", got, first)
	}
	view := historyForFixture(t, resumed, parent, captured)
	recent := view.RecentRequests[len(view.RecentRequests)-1]
	if view.Success != 120 || view.Failed != 3 || recent.Success != 5 || recent.Failed != 2 {
		t.Fatalf("baseline was lost or applied twice: %+v", view)
	}
	if recovered, err := os.Stat(path + ".v1.wal"); err != nil || recovered.Size() != info.Size() {
		t.Fatalf("replay did not remove just the incomplete append: %v", err)
	}
	if err := NewManager(nil, nil, nil).OpenEnterpriseHistory(directory); err == nil {
		t.Fatal("a second process owner can append to the same ledger")
	}
}
