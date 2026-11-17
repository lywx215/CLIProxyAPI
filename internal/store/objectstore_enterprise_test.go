package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type testObject struct {
	body []byte
	tag  string
}

type testS3 struct {
	mu      sync.Mutex
	objects map[string]testObject
	puts    map[string]int
	deletes int
	fault   func(string) string
	barrier func(string)
}

func newTestS3(t *testing.T) (*testS3, string) {
	t.Helper()
	s := &testS3{objects: make(map[string]testObject), puts: make(map[string]int)}
	host := httptest.NewServer(s)
	t.Cleanup(host.Close)
	return s, host.URL
}

func testS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code)
}

func (s *testS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	if r.Method == http.MethodPut && s.barrier != nil {
		s.barrier(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	object, exists := s.objects[key]
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			s.serveList(w, r.URL.Query().Get("prefix"))
			return
		}
		if !exists {
			testS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", object.tag)
		_, _ = w.Write(object.body)
	case http.MethodPut:
		s.puts[key]++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			testS3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		if (r.Header.Get("If-None-Match") == "*" && exists) ||
			(r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != object.tag) {
			testS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		var fault string
		if s.fault != nil {
			fault = s.fault(key)
		}
		if fault == "before" {
			testS3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable")
			return
		}
		tag := fmt.Sprintf("\"%x\"", sha256.Sum256(body))
		s.objects[key] = testObject{body, tag}
		if fault == "after" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("ETag", tag)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		s.deletes++
		if match := r.Header.Get("If-Match"); match != "" && match != object.tag {
			testS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		testS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (s *testS3) serveList(w http.ResponseWriter, prefix string) {
	keys := make([]string, 0)
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var response struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		IsTruncated bool
		Contents    []struct{ Key string }
	}
	for _, key := range keys {
		response.Contents = append(response.Contents, struct{ Key string }{key})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(response)
}

func checkedTestObjectStore(t *testing.T, endpoint, dir string, enabled bool) *ObjectTokenStore {
	t.Helper()
	s, err := NewObjectTokenStore(ObjectStoreConfig{
		Endpoint: endpoint, Bucket: "test-bucket", Prefix: "isolated",
		Region: "us-east-1", LocalRoot: dir, PathStyle: true,
		AccessKey: "fixture", SecretKey: "fixture-only", EnterpriseOAuthImportCAS: enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testImportIntent(id, mode string) auth.EnterpriseOAuthImportJournal {
	email, client := "fixture@example.test", "fixture-client"
	account := ge.AccountID(email, client)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
	return auth.EnterpriseOAuthImportJournal{
		RequestID: id, Mode: mode, Digest: digest, AccountID: account,
		CredentialID: auth.EnterpriseOAuthCredentialID(account),
		Email:        email, ClientID: client, Subject: "fixture-subject",
		Status: "reconcile_required", Code: "import_in_progress",
	}
}

func beginTestImport(t *testing.T, s *ObjectTokenStore, j auth.EnterpriseOAuthImportJournal) auth.EnterpriseOAuthImportJournal {
	t.Helper()
	result, acquired, err := s.BeginEnterpriseOAuthImport(t.Context(), j)
	if err != nil || !acquired {
		t.Fatalf("begin acquired=%t error=%v", acquired, err)
	}
	return result
}

func testImportRecord(j auth.EnterpriseOAuthImportJournal, generation string) *auth.Auth {
	meta := map[string]any{"type": ge.Provider, "email": j.Email, "client_id": j.ClientID,
		"refresh_token": "fixture-" + generation, "access_token": "fixture-access-" + generation}
	record := &auth.Auth{ID: j.CredentialID, FileName: j.CredentialID, Provider: ge.Provider,
		Metadata: ge.PutAccount(meta, ge.Account{ID: j.AccountID, Version: ge.AccountVersion, Revision: 1, Collections: []ge.Collection{}})}
	return record
}

func installTestImport(ctx context.Context, manager *auth.Manager, j auth.EnterpriseOAuthImportJournal, tokenVersion string) (*auth.Auth, error) {
	return manager.ImportEnterpriseAuthorization(ctx, testImportRecord(j, tokenVersion), auth.EnterpriseOAuthImportOptions{
		RequestID: j.RequestID, Digest: j.Digest, Mode: j.Mode,
		ExpectedRevision: j.ExpectedRevision, Subject: j.Subject,
	})
}

func TestObjectStoreEnterpriseImportGateAndLegacyBehavior(t *testing.T) {
	fake, endpoint := newTestS3(t)
	s := checkedTestObjectStore(t, endpoint, t.TempDir(), false)
	if s.SupportsEnterpriseOAuthImport() || s.SupportsExistingEnterpriseReauthorization() {
		t.Fatal("S3 import must be explicitly enabled")
	}
	j := testImportIntent("fixture_request_gate", "create")
	if _, acquired, err := s.BeginEnterpriseOAuthImport(t.Context(), j); acquired || err == nil {
		t.Fatalf("disabled import acquired=%t error=%v", acquired, err)
	}
	record := testImportRecord(j, "legacy")
	if _, err := s.Save(t.Context(), record); err != nil {
		t.Fatalf("legacy save: %v", err)
	}
	if len(fake.puts) != 1 {
		t.Fatalf("expected only the legacy credential PUT, got %v", fake.puts)
	}
	record.Metadata["note"] = "legacy mirror edit"
	body, err := json.Marshal(record.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.AuthDir(), record.FileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = s.PersistAuthFiles(t.Context(), "", record.FileName); err != nil {
		t.Fatalf("legacy mirror save: %v", err)
	}
	if err = s.Delete(t.Context(), record.ID); err != nil || fake.deletes != 1 {
		t.Fatalf("legacy deletion: %v, deletes=%d", err, fake.deletes)
	}
}

func TestObjectStoreExactRenewalRequiresConditionalStorage(t *testing.T) {
	_, endpoint := newTestS3(t)
	store := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	manager := auth.NewManager(store, nil, nil)
	if !manager.SupportsExistingEnterpriseReauthorization() {
		t.Fatal("conditional object store did not advertise exact renewal")
	}
	j := beginTestImport(t, store, testImportIntent("fixture_exact_renewal", "create"))
	base, err := installTestImport(t.Context(), manager, j, "original")
	if err != nil {
		t.Fatal(err)
	}
	renewed := base.Clone()
	renewed.Metadata["access_token"] = "fixture-renewed-access"
	renewed.Metadata["refresh_token"] = "fixture-renewed-refresh"
	saved, err := manager.ReauthorizeExistingGeminiEnterpriseCredential(t.Context(), base, renewed)
	if err != nil || saved.Metadata["refresh_token"] != "fixture-renewed-refresh" {
		t.Fatalf("conditional renewal failed: %v", err)
	}
	if _, err = manager.ReauthorizeExistingGeminiEnterpriseCredential(t.Context(), base, renewed); err == nil {
		t.Fatal("conditional renewal accepted a stale authorization")
	}
}

func TestObjectStoreEnterpriseImportLostCommitReply(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			fake, endpoint := newTestS3(t)
			fake.fault = func(key string) string {
				if strings.Contains(key, "/auths/") {
					return phase
				}
				return ""
			}
			writer := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
			readback := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
			j := beginTestImport(t, writer, testImportIntent("fixture_request_lost", "create"))
			manager := auth.NewManager(writer, nil, nil)
			if _, err := installTestImport(t.Context(), manager, j, "one"); !errors.Is(err, errObjectOutcomeUnknown) {
				t.Fatalf("expected unknown save after %s, got %v", phase, err)
			}
			if _, err := os.Stat(filepath.Join(writer.AuthDir(), j.CredentialID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unconfirmed save modified the mirror: %v", err)
			}
			if _, exists := manager.GetByID(j.CredentialID); exists {
				t.Fatal("unknown save was published")
			}
			key := "isolated/auths/" + j.CredentialID
			if fake.puts[key] != 1 {
				t.Fatalf("credential PUT retried: %d", fake.puts[key])
			}
			priorWrites := len(fake.puts)
			intent, saved, err := readback.LookupEnterpriseOAuthImport(t.Context(), j.RequestID)
			if err != nil || intent.Status != "reconcile_required" || (saved != nil) != (phase == "after") {
				t.Fatalf("readback intent=%v saved=%t error=%v", intent.Status, saved != nil, err)
			}
			if len(fake.puts) != priorWrites || fake.puts[key] != 1 || fake.deletes != 0 {
				t.Fatal("readback wrote to object storage")
			}
			if saved != nil {
				receipt, found := auth.EnterpriseOAuthReceipt(saved, j.RequestID)
				if !found || !auth.MatchesEnterpriseOAuthReceipt(saved, receipt) {
					t.Fatal("credential and embedded receipt are inconsistent")
				}
			}
			if _, acquired, err := readback.BeginEnterpriseOAuthImport(t.Context(), j); err != nil || acquired {
				t.Fatalf("same ID replayed, acquired=%t error=%v", acquired, err)
			}
			j.Digest = strings.Repeat("a", 64)
			if _, acquired, err := readback.BeginEnterpriseOAuthImport(t.Context(), j); acquired || !errors.Is(err, auth.ErrEnterpriseOAuthImportIDConflict) {
				t.Fatalf("same ID accepted different input: %t %v", acquired, err)
			}
		})
	}
}

func TestObjectStoreEnterpriseImportLostIntentReply(t *testing.T) {
	for _, stage := range []string{"requests", "owners"} {
		for _, phase := range []string{"before", "after"} {
			t.Run(stage+"/"+phase, func(t *testing.T) {
				fake, endpoint := newTestS3(t)
				fail := true
				fake.fault = func(key string) string {
					if fail && strings.Contains(key, "/"+stage+"/") {
						fail = false
						return phase
					}
					return ""
				}
				writer := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
				restarted := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
				j := testImportIntent("fixture_request_intent", "create")
				if _, acquired, err := writer.BeginEnterpriseOAuthImport(t.Context(), j); acquired || !errors.Is(err, errObjectOutcomeUnknown) {
					t.Fatalf("uncertain intent acquired=%t error=%v", acquired, err)
				}
				var key string
				if stage == "requests" {
					key = objectImportRequestKey(j.RequestID)
				} else {
					key = objectImportOwnerKey(j.AccountID)
				}
				if fake.puts[writer.prefixedKey(key)] != 1 {
					t.Fatal("intent PUT was retried")
				}
				intent, saved, err := restarted.LookupEnterpriseOAuthImport(t.Context(), j.RequestID)
				if stage == "requests" && phase == "before" {
					if !errors.Is(err, auth.ErrEnterpriseOAuthImportNotFound) || saved != nil {
						t.Fatalf("unexpected persisted result %v", err)
					}
					return
				}
				if err != nil || saved != nil || intent.Status != "reconcile_required" {
					t.Fatalf("unexpected intent result status=%q saved=%t error=%v", intent.Status, saved != nil, err)
				}
				if _, acquired, err := restarted.BeginEnterpriseOAuthImport(t.Context(), j); err != nil || acquired {
					t.Fatalf("existing intent replayed: %t %v", acquired, err)
				}
				if stage == "owners" && phase == "after" {
					newID := testImportIntent("fixture_request_later", "create")
					if _, acquired, err := restarted.BeginEnterpriseOAuthImport(t.Context(), newID); acquired || !errors.Is(err, auth.ErrEnterpriseOAuthImportBusy) {
						t.Fatalf("unknown owner was replaced: %t %v", acquired, err)
					}
				}
			})
		}
	}
}

func TestObjectStoreEnterpriseImportProtectsReauthorizationAndSnapshots(t *testing.T) {
	fake, endpoint := newTestS3(t)
	first := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	second := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	manager := auth.NewManager(first, nil, nil)
	creation := beginTestImport(t, first, testImportIntent("fixture_request_create", "create"))
	saved, err := installTestImport(t.Context(), manager, creation, "first")
	if err != nil {
		t.Fatal(err)
	}
	stale := saved.Clone()
	reauthorize := testImportIntent("fixture_request_reauth", "reauthorize")
	reauthorize.ExpectedRevision, reauthorize.TargetIndex = 1, saved.Index
	reauthorize = beginTestImport(t, first, reauthorize)
	// Install a graph/status edit after intent acquisition. Reauthorization rebases
	// onto the same authorization revision with the latest remote settings.
	saved.Metadata["note"], saved.Disabled = "retain this setting", true
	if _, err = first.Save(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	saved, err = installTestImport(t.Context(), manager, reauthorize, "second")
	if err != nil {
		t.Fatal(err)
	}
	if auth.EnterpriseOAuthAuthorizationRevision(saved) != 2 || !saved.Disabled || saved.Metadata["note"] != "retain this setting" {
		t.Fatal("reauthorization did not preserve remote settings and disabled state")
	}
	oldJournal := creation
	_, old, err := second.LookupEnterpriseOAuthImport(t.Context(), oldJournal.RequestID)
	oldReceipt, found := auth.EnterpriseOAuthReceipt(old, oldJournal.RequestID)
	if err != nil || !found || auth.MatchesEnterpriseOAuthReceipt(old, oldReceipt) {
		t.Fatalf("superseded receipt invalid: %v", err)
	}
	stale.Attributes[auth.AttributePath] = filepath.Join(second.AuthDir(), stale.ID)
	if _, err := second.Save(t.Context(), stale); !errors.Is(err, auth.ErrEnterpriseOAuthImportConflict) {
		t.Fatalf("stale write error: %v", err)
	}
	changed := saved.Clone()
	delete(changed.Attributes, auth.AttributeEnterpriseObjectETag)
	if _, err := first.Save(t.Context(), changed); !errors.Is(err, auth.ErrEnterpriseAuthVersionRequired) {
		t.Fatalf("unversioned write error: %v", err)
	}
	if err := first.PersistAuthFiles(t.Context(), "", saved.FileName); !errors.Is(err, auth.ErrEnterpriseAuthVersionRequired) {
		t.Fatalf("unversioned mirror write error: %v", err)
	}
	if err := first.Delete(t.Context(), saved.ID); !errors.Is(err, auth.ErrEnterpriseAuthVersionRequired) {
		t.Fatalf("unversioned delete error: %v", err)
	}
	key := "isolated/auths/" + saved.ID
	if fake.puts[key] != 3 || fake.deletes != 0 {
		t.Fatalf("protected object replaced or deleted: puts=%d deletes=%d", fake.puts[key], fake.deletes)
	}
}

// TestObjectStoreEnterpriseImportWorker runs in a separate OS process with its
// own mirror and manager. The only shared state is the HTTP S3 fixture.
func TestObjectStoreEnterpriseImportWorker(t *testing.T) {
	if os.Getenv("CPA_TEST_OBJECT_WORKER") != "1" {
		t.Skip("worker subprocess")
	}
	endpoint := os.Getenv("CPA_TEST_OBJECT_ENDPOINT")
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatal("worker requires the loopback fixture")
	}
	s := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	j := testImportIntent(os.Getenv("CPA_TEST_OBJECT_REQUEST"), "create")
	if os.Getenv("CPA_TEST_OBJECT_STALE_SAVE") == "1" {
		staleETagWorker(t, s, j)
		return
	}
	j, owner, err := s.BeginEnterpriseOAuthImport(t.Context(), j)
	if !owner && (err == nil || errors.Is(err, auth.ErrEnterpriseOAuthImportBusy) ||
		errors.Is(err, auth.ErrEnterpriseOAuthImportConflict)) {
		fmt.Println("OBJECT_IMPORT_NOT_OWNER")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = installTestImport(t.Context(), auth.NewManager(s, nil, nil), j, "winner"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("OBJECT_IMPORT_OWNER")
}

func staleETagWorker(t *testing.T, s *ObjectTokenStore, j auth.EnterpriseOAuthImportJournal) {
	t.Helper()
	_, saved, err := s.LookupEnterpriseOAuthImport(t.Context(), j.RequestID)
	if err != nil || saved == nil {
		t.Fatalf("read base for conditional update: %v", err)
	}
	saved.Metadata["note"] = s.authDir
	_, err = s.Save(t.Context(), saved)
	if errors.Is(err, auth.ErrEnterpriseOAuthImportConflict) {
		fmt.Println("OBJECT_STALE_WRITER")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("OBJECT_IMPORT_OWNER")
}

func TestObjectStoreEnterpriseImportTwoProcesses(t *testing.T) {
	for _, mode := range []string{"same-account-different-request", "same-request", "same-etag"} {
		t.Run(mode, func(t *testing.T) {
			fake, endpoint := newTestS3(t)
			ids := []string{"fixture_process_first", "fixture_process_second"}
			targetSegment := "/requests/"
			expectedPuts := 1
			if mode == "same-request" {
				ids[1] = ids[0]
			}
			if mode == "same-etag" {
				ids[1] = ids[0]
				targetSegment = "/auths/"
				expectedPuts = 3
				initial := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
				j := beginTestImport(t, initial, testImportIntent(ids[0], "create"))
				if _, err := installTestImport(t.Context(), auth.NewManager(initial, nil, nil), j, "base"); err != nil {
					t.Fatal(err)
				}
			}
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			fake.barrier = func(key string) {
				if strings.Contains(key, targetSegment) {
					entered <- struct{}{}
					<-release
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			workers := make([]*exec.Cmd, 2)
			outputs := make([]bytes.Buffer, 2)
			for i, id := range ids {
				worker := exec.CommandContext(ctx, executable, "-test.run=^TestObjectStoreEnterpriseImportWorker$")
				worker.Env = append(os.Environ(), "CPA_TEST_OBJECT_WORKER=1", "CPA_TEST_OBJECT_ENDPOINT="+endpoint, "CPA_TEST_OBJECT_REQUEST="+id)
				if mode == "same-etag" {
					worker.Env = append(worker.Env, "CPA_TEST_OBJECT_STALE_SAVE=1")
				}
				worker.Stdout, worker.Stderr = &outputs[i], &outputs[i]
				if err = worker.Start(); err != nil {
					t.Fatal(err)
				}
				workers[i] = worker
			}
			for range workers {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("workers did not reach the same S3 request barrier")
				}
			}
			releaseOnce.Do(func() { close(release) })
			winners := 0
			for i, worker := range workers {
				if err = worker.Wait(); err != nil {
					t.Fatalf("worker %d: %v\n%s", i, err, &outputs[i])
				}
				winners += strings.Count(outputs[i].String(), "OBJECT_IMPORT_OWNER")
			}
			if winners != 1 {
				t.Fatalf("expected one execution owner, got %d", winners)
			}
			j := testImportIntent(ids[0], "create")
			if got := fake.puts["isolated/auths/"+j.CredentialID]; got != expectedPuts {
				t.Fatalf("credential PUT attempts = %d, want %d", got, expectedPuts)
			}
			if mode == "same-etag" && !strings.Contains(outputs[0].String()+outputs[1].String(), "OBJECT_STALE_WRITER") {
				t.Fatal("the losing conditional write did not report a conflict")
			}
		})
	}
}

func TestObjectStoreEnterpriseRefreshAndGraphEditsCannotRetryAStaleWrite(t *testing.T) {
	fake, endpoint := newTestS3(t)
	first := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	second := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	primary := auth.NewManager(first, nil, nil)
	creation := beginTestImport(t, first, testImportIntent("object_refresh_creation", "create"))
	current, err := installTestImport(t.Context(), primary, creation, "one")
	if err != nil {
		t.Fatal(err)
	}
	// Bootstrap captures the authoritative bytes and ETag together.
	if err = second.syncAuthFromBucket(t.Context()); err != nil {
		t.Fatal(err)
	}
	peer := auth.NewManager(second, nil, nil)
	if err = peer.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	stale, _ := peer.GetByID(creation.CredentialID)
	if stale == nil || stale.Attributes[auth.AttributeEnterpriseObjectETag] == "" {
		t.Fatal("initial load did not propagate the ETag")
	}
	reauth := testImportIntent("object_refresh_reauth", "reauthorize")
	reauth.ExpectedRevision, reauth.TargetIndex = 1, current.Index
	reauth = beginTestImport(t, first, reauth)
	newer, err := installTestImport(t.Context(), primary, reauth, "two")
	if err != nil {
		t.Fatal(err)
	}
	key := "isolated/auths/" + newer.ID
	before := fake.puts[key]
	refreshed := stale.Clone()
	refreshed.Metadata["access_token"] = "fixture-refresh-from-stale-version"
	if _, err = peer.UpdateRefreshedAuth(t.Context(), stale, refreshed); !errors.Is(err, auth.ErrEnterpriseOAuthImportConflict) {
		t.Fatalf("stale refresh failure: %v", err)
	}
	stillLocal, _ := peer.GetByID(stale.ID)
	if stillLocal.Metadata["access_token"] != stale.Metadata["access_token"] {
		t.Fatal("uncommitted refresh entered the runtime")
	}
	if _, err = peer.UpdateEnterpriseAccount(t.Context(), stale, 1, func(a *ge.Account) error {
		a.Discovery = map[string]any{"attempt": "from stale graph"}
		return nil
	}); !errors.Is(err, auth.ErrEnterpriseOAuthImportConflict) {
		t.Fatalf("stale graph failure: %v", err)
	}
	if fake.puts[key] != before {
		t.Fatal("a stale writer acquired a fresh ETag")
	}
	_, authoritative, err := second.LookupEnterpriseOAuthImport(t.Context(), reauth.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := auth.EnterpriseOAuthReceipt(authoritative, reauth.RequestID)
	if err = peer.PublishEnterpriseOAuthImport(t.Context(), authoritative, receipt); err != nil {
		t.Fatal(err)
	}
	base, _ := peer.GetByID(creation.CredentialID)
	unversioned := stale.Clone()
	delete(unversioned.Attributes, auth.AttributeEnterpriseObjectETag)
	unversioned.RegistrationEpoch = 0
	unversioned.Generation = 0
	if _, err = peer.Update(auth.WithSkipPersist(t.Context()), unversioned); err != nil {
		t.Fatalf("unversioned watcher update: %v", err)
	}
	base, _ = peer.GetByID(creation.CredentialID)
	if !auth.MatchesEnterpriseOAuthReceipt(base, receipt) || base.Attributes[auth.AttributeEnterpriseObjectETag] == "" {
		t.Fatal("unversioned watcher data replaced the authoritative version")
	}
	refreshed = base.Clone()
	refreshed.Metadata["access_token"] = "fixture-refreshed-current"
	if _, err = peer.UpdateRefreshedAuth(t.Context(), base, refreshed); err != nil {
		t.Fatalf("conditional refresh from current auth: %v", err)
	}
	_, refreshedRemote, err := second.LookupEnterpriseOAuthImport(t.Context(), reauth.RequestID)
	if err != nil || !auth.MatchesEnterpriseOAuthReceipt(refreshedRemote, receipt) ||
		ge.String(refreshedRemote.Metadata, "access_token") != "fixture-refreshed-current" || fake.puts[key] != before+1 {
		t.Fatalf("refresh did not preserve the durable receipt: %v", err)
	}
}

func TestObjectStoreEnterpriseMirrorFailureLeavesRecoverableRemoteReceipt(t *testing.T) {
	fake, endpoint := newTestS3(t)
	writer := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	reader := checkedTestObjectStore(t, endpoint, t.TempDir(), true)
	j := beginTestImport(t, writer, testImportIntent("object_commit_mirror_failure", "create"))
	// A directory occupying the mirror path makes atomic replacement fail after PUT.
	if err := os.Mkdir(filepath.Join(writer.AuthDir(), j.CredentialID), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := auth.NewManager(writer, nil, nil)
	if _, err := installTestImport(t.Context(), manager, j, "committed"); err == nil || !strings.Contains(err.Error(), "mirror update failed") {
		t.Fatalf("expected a mirror failure, got %v", err)
	}
	if _, ok := manager.GetByID(j.CredentialID); ok {
		t.Fatal("failed mirror update published the grant")
	}
	_, saved, err := reader.LookupEnterpriseOAuthImport(t.Context(), j.RequestID)
	if err != nil || saved == nil {
		t.Fatalf("failed local mirror hid the remote commit: %v", err)
	}
	receipt, _ := auth.EnterpriseOAuthReceipt(saved, j.RequestID)
	if !auth.MatchesEnterpriseOAuthReceipt(saved, receipt) || fake.puts["isolated/auths/"+j.CredentialID] != 1 {
		t.Fatal("remote commit or embedded receipt is incomplete")
	}
}
