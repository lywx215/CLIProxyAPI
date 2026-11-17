package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

type enterpriseLegacyWriteFixture struct {
	manager *coreauth.Manager
	other   *coreauth.Manager
	base    *coreauth.Auth
	handler *Handler
	path    string
}

func newEnterpriseLegacyWriteFixture(t *testing.T) enterpriseLegacyWriteFixture {
	t.Helper()
	directory := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(directory)
	manager := coreauth.NewManager(store, nil, nil)
	record := sdkAuth.NewGeminiEnterpriseRecord(
		config.GeminiEnterpriseOAuth{ClientID: "fixture-old", ClientSecretEnv: "FIXTURE_ORIGINAL_SECRET",
			ProjectID: "fixture-project", EngineID: "fixture-engine"},
		"legacy@example.test", &oauth2.Token{AccessToken: "fixture-access",
			RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour)}, nil)
	base, err := manager.Register(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	// Separate managers model independent writers sharing the same source.
	otherStore := sdkAuth.NewFileTokenStore()
	otherStore.SetBaseDir(directory)
	other := coreauth.NewManager(otherStore, nil, nil)
	if err = other.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return enterpriseLegacyWriteFixture{
		manager: manager, other: other, base: base, path: base.Attributes[coreauth.AttributePath],
		handler: &Handler{cfg: &config.Config{AuthDir: directory}, tokenStore: otherStore, authManager: other},
	}
}

func (f enterpriseLegacyWriteFixture) pauseRenewal(t *testing.T) func() {
	t.Helper()
	barrier := &enterpriseDeleteTokenBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	incoming := f.base.Clone()
	incoming.Metadata["access_token"] = "fixture-renewed-access"
	incoming.Metadata["refresh_token"] = "fixture-renewed-refresh"
	incoming.Metadata["id_token"] = barrier
	done := make(chan error, 1)
	go func() {
		_, err := f.manager.ReauthorizeExistingGeminiEnterpriseCredential(t.Context(), f.base, incoming)
		done <- err
	}()
	var once sync.Once
	finish := func() {
		t.Helper()
		once.Do(func() {
			close(barrier.release)
			if err := <-done; err != nil {
				t.Errorf("original-application renewal failed: %v", err)
			}
		})
	}
	t.Cleanup(finish)
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not reach its checked write boundary")
	}
	return finish
}

func (f enterpriseLegacyWriteFixture) bytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f enterpriseLegacyWriteFixture) metadata(t *testing.T) map[string]any {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal(f.bytes(t), &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func (f enterpriseLegacyWriteFixture) request(t *testing.T, operation string) *httptest.ResponseRecorder {
	t.Helper()
	current, ok := f.other.GetByID(f.base.ID)
	if !ok {
		t.Fatal("existing legacy credential is missing")
	}
	method, endpoint := http.MethodPatch, "/v8/management/credentials/fields"
	payload := map[string]any{"name": current.FileName, "client_secret_env": "FIXTURE_UPDATED_SECRET", "engine_id": "fixture-new-engine"}
	handler := f.handler.PatchAuthFileFields
	switch operation {
	case "status":
		endpoint, handler = "/v8/management/credentials/status", f.handler.PatchAuthFileStatus
		payload = map[string]any{"name": current.FileName, "disabled": true}
	case "upload", "replace-provider":
		method, endpoint, handler = http.MethodPost, "/v8/management/credentials?name="+url.QueryEscape(current.FileName), f.handler.UploadAuthFile
		payload = current.Clone().Metadata
		payload["engine_id"] = "fixture-new-engine"
		if operation == "replace-provider" {
			payload["type"] = "codex"
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(ConfigV8ContextKey, true)
	c.Request = httptest.NewRequest(method, endpoint, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return recorder
}

func TestEnterpriseLegacyRenewalSerializesManagementWrites(t *testing.T) {
	for _, operation := range []string{"fields", "status", "upload", "replace-provider"} {
		t.Run(operation, func(t *testing.T) {
			f := newEnterpriseLegacyWriteFixture(t)
			before := f.bytes(t)
			finish := f.pauseRenewal(t)
			result := f.request(t, operation)
			if result.Code != http.StatusConflict {
				t.Fatalf("concurrent %s returned %d, want 409", operation, result.Code)
			}
			if !bytes.Equal(before, f.bytes(t)) {
				t.Fatal("rejected management write altered the source")
			}
			current, _ := f.other.GetByID(f.base.ID)
			if current.Disabled || current.Provider != ge.Provider ||
				ge.String(current.Metadata, "client_secret_env") != "FIXTURE_ORIGINAL_SECRET" ||
				ge.String(current.Metadata, "engine_id") != "fixture-engine" {
				t.Fatal("rejected management write was published in runtime")
			}
			finish()
			renewed := f.metadata(t)
			if ge.String(renewed, "access_token") != "fixture-renewed-access" ||
				ge.String(renewed, "refresh_token") != "fixture-renewed-refresh" ||
				ge.String(renewed, "client_id") != "fixture-old" ||
				ge.String(renewed, "client_secret_env") != "FIXTURE_ORIGINAL_SECRET" ||
				ge.String(renewed, "engine_id") != "fixture-engine" {
				t.Fatal("renewal changed the original application or existing configuration")
			}
			if operation == "replace-provider" {
				return
			}
			if operation != "upload" {
				snapshot := f.bytes(t)
				if stale := f.request(t, operation); stale.Code != http.StatusConflict {
					t.Fatalf("stale runtime snapshot returned %d, want 409", stale.Code)
				}
				if !bytes.Equal(snapshot, f.bytes(t)) {
					t.Fatal("stale retry overwrote renewed OAuth")
				}
			}
			if err := f.other.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			if retried := f.request(t, operation); retried.Code != http.StatusOK {
				t.Fatalf("retry after reload returned %d, want 200", retried.Code)
			}
			updated := f.metadata(t)
			if ge.String(updated, "access_token") != "fixture-renewed-access" ||
				ge.String(updated, "refresh_token") != "fixture-renewed-refresh" ||
				ge.String(updated, "client_id") != "fixture-old" {
				t.Fatal("normal retry lost the original application's renewed OAuth")
			}
			if operation == "status" {
				if updated["disabled"] != true {
					t.Fatal("status retry was not persisted")
				}
			} else if ge.String(updated, "engine_id") != "fixture-new-engine" {
				t.Fatal("configuration retry was not persisted")
			}
			if operation == "fields" && ge.String(updated, "client_secret_env") != "FIXTURE_UPDATED_SECRET" {
				t.Fatal("configuration retry lost the requested secret reference")
			}
		})
	}
}

func TestEnterpriseLegacyRenewalRejectsConcurrentRegisterAndRefresh(t *testing.T) {
	for _, operation := range []string{"register", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			f := newEnterpriseLegacyWriteFixture(t)
			before := f.bytes(t)
			finish := f.pauseRenewal(t)
			base, _ := f.other.GetByID(f.base.ID)
			incoming := base.Clone()
			incoming.Metadata["access_token"] = "fixture-competing-access"
			var err error
			if operation == "register" {
				_, err = f.other.Register(t.Context(), incoming)
			} else {
				_, err = f.other.UpdateRefreshedAuth(t.Context(), base, incoming)
			}
			if !errors.Is(err, coreauth.ErrAuthFileWriteConflict) {
				t.Fatalf("%s must report a durable conflict, got %v", operation, err)
			}
			current, _ := f.other.GetByID(f.base.ID)
			if ge.String(current.Metadata, "access_token") != "fixture-access" ||
				current.RegistrationEpoch != base.RegistrationEpoch || !bytes.Equal(before, f.bytes(t)) {
				t.Fatal("failed write changed the durable or runtime authorization")
			}
			finish()
			snapshot := f.bytes(t)
			if operation == "refresh" {
				if _, err = f.other.UpdateRefreshedAuth(t.Context(), base, incoming); !errors.Is(err, coreauth.ErrAuthFileWriteConflict) {
					t.Fatalf("stale refresh was accepted after renewal: %v", err)
				}
				if !bytes.Equal(snapshot, f.bytes(t)) {
					t.Fatal("stale refresh replaced the new grant")
				}
			}
			if err = f.other.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			base, _ = f.other.GetByID(f.base.ID)
			incoming = base.Clone()
			incoming.Metadata["access_token"] = "fixture-refreshed-after-renewal"
			if operation == "register" {
				_, err = f.other.Register(t.Context(), incoming)
			} else {
				_, err = f.other.UpdateRefreshedAuth(t.Context(), base, incoming)
			}
			if err != nil {
				t.Fatalf("ordinary %s failed after conflict and reload: %v", operation, err)
			}
			if saved := f.metadata(t); ge.String(saved, "access_token") != "fixture-refreshed-after-renewal" ||
				ge.String(saved, "client_id") != "fixture-old" || ge.String(saved, "refresh_token") != "fixture-renewed-refresh" {
				t.Fatal("ordinary write was skipped or changed the original application")
			}
		})
	}
}

func TestEnterpriseLegacyRenewalPreservesPreviouslySavedSettings(t *testing.T) {
	f := newEnterpriseLegacyWriteFixture(t)
	changed, _ := f.other.GetByID(f.base.ID)
	changed.Metadata["engine_id"] = "fixture-new-engine"
	changed.Metadata["label"] = "preserve label"
	changed.Metadata["headers"] = map[string]any{"X-Fixture": "preserve header"}
	changed.Disabled = true
	if _, err := f.other.Update(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	renewed := f.base.Clone()
	renewed.Metadata["access_token"] = "fixture-renewed-access"
	renewed.Metadata["refresh_token"] = "fixture-renewed-refresh"
	if _, err := f.manager.ReauthorizeExistingGeminiEnterpriseCredential(t.Context(), f.base, renewed); err != nil {
		t.Fatal(err)
	}
	saved := f.metadata(t)
	if ge.String(saved, "engine_id") != "fixture-new-engine" || saved["disabled"] != true ||
		ge.String(saved, "label") != "preserve label" ||
		ge.String(saved, "client_id") != "fixture-old" ||
		ge.String(saved, "client_secret_env") != "FIXTURE_ORIGINAL_SECRET" ||
		ge.String(saved, "access_token") != "fixture-renewed-access" {
		t.Fatal("renewal overwrote previously committed settings or lost new OAuth")
	}
	if headers, ok := saved["headers"].(map[string]any); !ok || headers["X-Fixture"] != "preserve header" {
		t.Fatal("renewal overwrote saved custom headers")
	}
}

func TestFileStoreUploadPreservesOtherProviderSources(t *testing.T) {
	for _, provider := range []string{"codex", "custom-fixture"} {
		t.Run(provider, func(t *testing.T) {
			directory := t.TempDir()
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(directory)
			manager := coreauth.NewManager(store, nil, nil)
			h := &Handler{cfg: &config.Config{AuthDir: directory}, tokenStore: store, authManager: manager}
			payload, err := json.Marshal(map[string]any{
				"type": provider, "email": "fixture@example.test", "access_token": "fixture-access",
				"disabled": true, "custom_field": map[string]any{"preserve": true},
			})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Set(ConfigV8ContextKey, true)
			c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/credentials?name=fixture.json", bytes.NewReader(payload))
			c.Request.Header.Set("Content-Type", "application/json")
			h.UploadAuthFile(c)
			if response.Code != http.StatusOK {
				t.Fatalf("ordinary upload returned %d", response.Code)
			}
			data, err := os.ReadFile(filepath.Join(directory, "fixture.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]any
			if err = json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved["type"] != provider || saved["disabled"] != true || saved["access_token"] != "fixture-access" {
				t.Fatal("upload changed provider data or the disabled state")
			}
			if custom, ok := saved["custom_field"].(map[string]any); !ok || custom["preserve"] != true {
				t.Fatal("upload dropped custom source fields")
			}
			if current, ok := manager.GetByID("fixture.json"); !ok || current.Provider != provider || !current.Disabled {
				t.Fatal("upload was not published with its original provider and status")
			}
		})
	}
}
