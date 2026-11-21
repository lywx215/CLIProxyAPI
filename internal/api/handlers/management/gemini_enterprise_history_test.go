package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestV8EnterpriseCredentialHistoryRestoredAfterServiceRestart(t *testing.T) {
	_, fixture, _ := enterpriseAccountFixture(t)
	metadata, err := json.Marshal(fixture.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	open := func() *Handler {
		store := sdkAuth.NewFileTokenStore()
		store.SetBaseDir(directory)
		manager := coreauth.NewManager(store, nil, nil)
		if err := manager.Load(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.OpenEnterpriseHistory(directory); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := manager.CloseEnterpriseHistory(); err != nil {
				t.Error(err)
			}
		})
		return &Handler{authManager: manager, cfg: &config.Config{AuthDir: directory}}
	}
	type lifetimeView struct {
		First  time.Time `json:"first_uploaded_at"`
		Server time.Time `json:"server_time"`
		Apps   []struct {
			Success int64                          `json:"success"`
			Failed  int64                          `json:"failed"`
			Recent  []coreauth.RecentRequestBucket `json:"recent_requests"`
		} `json:"collections"`
	}
	read := func(h *Handler) lifetimeView {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
		ctx.Set(ConfigV8ContextKey, true)
		h.ListAuthFiles(ctx)
		var payload struct {
			Files []struct {
				Enterprise lifetimeView `json:"gemini_enterprise"`
			} `json:"files"`
		}
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &payload) != nil || len(payload.Files) != 1 {
			t.Fatalf("unexpected credential listing status %d", recorder.Code)
		}
		return payload.Files[0].Enterprise
	}
	h := open()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/credentials?name=fixture.json", bytes.NewReader(metadata))
	ctx.Set(ConfigV8ContextKey, true)
	h.UploadAuthFile(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload returned %d: %s", recorder.Code, recorder.Body.String())
	}
	children := h.authManager.EnterpriseCollections("fixture.json")
	if len(children) != 3 {
		t.Fatalf("upload projected %d applications, want 3", len(children))
	}
	child := children[0]
	h.authManager.MarkResult(t.Context(), coreauth.Result{AuthID: child.ID, Provider: ge.Provider, Success: true})
	h.authManager.MarkResult(t.Context(), coreauth.Result{AuthID: child.ID, Provider: ge.Provider, Error: &coreauth.Error{Code: "enterprise_quota", HTTPStatus: 429}})
	before := read(h)
	if before.First.IsZero() || before.Server.Before(before.First) || len(before.Apps) != 3 || before.Apps[0].Success != 1 || before.Apps[0].Failed != 1 {
		t.Fatal("initial first upload and request counts did not reach v8")
	}
	if err := h.authManager.CloseEnterpriseHistory(); err != nil {
		t.Fatal(err)
	}
	after := read(open())
	if !after.First.Equal(before.First) || after.Server.Before(before.Server) || len(after.Apps) != 3 || after.Apps[0].Success != 1 || after.Apps[0].Failed != 1 {
		t.Fatal("v8 lost first upload or counts after reload")
	}
	var success, failed int64
	for _, bucket := range after.Apps[0].Recent {
		success += bucket.Success
		failed += bucket.Failed
	}
	if success != 1 || failed != 1 {
		t.Fatal("v8 lost recent requests after reload")
	}
}

func TestV8EnterpriseLegacyCredentialUsesInitializationTime(t *testing.T) {
	handler, _, _ := enterpriseAccountFixture(t)
	before := time.Now().UTC()
	if err := handler.authManager.OpenEnterpriseHistory(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handler.authManager.CloseEnterpriseHistory(); err != nil {
			t.Error(err)
		}
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
	ctx.Set(ConfigV8ContextKey, true)
	handler.ListAuthFiles(ctx)
	var payload struct {
		Files []struct {
			Enterprise struct {
				FirstUploadedAt time.Time `json:"first_uploaded_at"`
				InitializedAt   time.Time `json:"lifetime_initialized_at"`
				ServerTime      time.Time `json:"server_time"`
			} `json:"gemini_enterprise"`
		} `json:"files"`
	}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &payload) != nil || len(payload.Files) != 1 {
		t.Fatalf("unexpected credential listing status %d", recorder.Code)
	}
	view := payload.Files[0].Enterprise
	if !view.FirstUploadedAt.IsZero() || view.InitializedAt.Before(before) || view.InitializedAt.After(view.ServerTime) {
		t.Fatalf("unexpected legacy lifetime: %+v", view)
	}
}
