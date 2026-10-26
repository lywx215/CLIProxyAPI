package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseQuotaHandlerKeepsPermissionFailuresOutOfCredentialHealth(t *testing.T) {
	previous := newGeminiEnterpriseQuotaClient
	previousIdentity := resolveGeminiEnterpriseQuotaIdentity
	t.Cleanup(func() {
		newGeminiEnterpriseQuotaClient = previous
		resolveGeminiEnterpriseQuotaIdentity = previousIdentity
	})
	resolveGeminiEnterpriseQuotaIdentity = func(context.Context, *http.Client, config.GeminiEnterpriseOAuth, string, time.Time) *ge.QuotaIdentity {
		return nil
	}
	newGeminiEnterpriseQuotaClient = func(string) (*http.Client, error) {
		return &http.Client{Transport: enterpriseTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"never-expose-upstream-details"}}`))}, nil
		})}, nil
	}
	manager := coreauth.NewManager(nil, nil, nil)
	metadata := map[string]any{
		"client_id": "fixture", "project_id": "fixture-project", "engine_id": "fixture-engine",
		"access_token": "synthetic-secret", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "enterprise-quota", Provider: ge.Provider, Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	auth.EnsureIndex()
	h := &Handler{authManager: manager}
	router := gin.New()
	router.GET("/quota", h.GetGeminiEnterpriseQuota)
	for _, index := range []string{auth.Index, "missing", ""} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/quota?auth_index="+index, nil))
		var result ge.QuotaSnapshot
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
			t.Fatalf("quota failure was not a soft response: %d", recorder.Code)
		}
		want := "unavailable"
		if index == auth.Index {
			want = "no_permission"
		}
		if result.Status != want || strings.Contains(recorder.Body.String(), "synthetic-secret") || strings.Contains(recorder.Body.String(), "never-expose") || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unsafe quota result: %s", recorder.Body.String())
		}
	}
	after, _ := manager.GetByID(auth.ID)
	if after.Unavailable != auth.Unavailable || after.Status != auth.Status || after.LastError != nil {
		t.Fatal("optional quota permission failure changed credential routing health")
	}
}

func TestEnterpriseQuotaHandlerUsesUnifiedSavedTokenRefresh(t *testing.T) {
	previousClient, previousRefresh := newGeminiEnterpriseQuotaClient, ensureGeminiEnterpriseManagementToken
	previousIdentity := resolveGeminiEnterpriseQuotaIdentity
	t.Cleanup(func() {
		newGeminiEnterpriseQuotaClient, ensureGeminiEnterpriseManagementToken = previousClient, previousRefresh
		resolveGeminiEnterpriseQuotaIdentity = previousIdentity
	})
	resolveGeminiEnterpriseQuotaIdentity = func(context.Context, *http.Client, config.GeminiEnterpriseOAuth, string, time.Time) *ge.QuotaIdentity {
		return nil
	}
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "refresh-failure"
		}
		t.Run(name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			metadata := map[string]any{
				"client_id": "saved-client", "project_id": "saved-project", "engine_id": "saved-engine",
				"access_token": "expired-synthetic", "refresh_token": "saved-synthetic-refresh",
				"expiry": time.Now().Add(-time.Hour).Format(time.RFC3339),
			}
			auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "quota-refresh", Provider: ge.Provider, ProxyURL: "http://127.0.0.1:9", Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			auth.EnsureIndex()
			before, _ := json.Marshal(auth.Metadata)
			refreshes, reads := 0, 0
			ensureGeminiEnterpriseManagementToken = func(ctx context.Context, manager *coreauth.Manager, id string) (*coreauth.Auth, error) {
				refreshes++
				current, _ := manager.GetByID(id)
				binding := ge.FromMetadata(current.Metadata)
				if binding.ClientID != "saved-client" || binding.ProjectID != "saved-project" || binding.EngineID != "saved-engine" || ge.String(current.Metadata, "refresh_token") != "saved-synthetic-refresh" || current.ProxyURL != auth.ProxyURL {
					t.Fatal("quota refresh inherited another credential or global binding")
				}
				if fail {
					return nil, errors.New("never-expose-refresh-secret")
				}
				current.Metadata["access_token"] = "renewed-synthetic"
				current.Metadata["refresh_token"] = "rotated-synthetic-refresh"
				return manager.Update(ctx, current)
			}
			newGeminiEnterpriseQuotaClient = func(proxy string) (*http.Client, error) {
				if proxy != auth.ProxyURL {
					t.Fatal("quota query did not use the credential proxy")
				}
				return &http.Client{Transport: enterpriseTestTransport(func(r *http.Request) (*http.Response, error) {
					reads++
					if r.Header.Get("Authorization") != "Bearer renewed-synthetic" || r.Header.Get("X-Goog-User-Project") != "saved-project" {
						t.Fatal("quota query did not use the refreshed credential")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})}, nil
			}
			cfg := &config.Config{}
			cfg.ProxyURL = "http://global.invalid"
			h := &Handler{authManager: manager, cfg: cfg}
			router := gin.New()
			router.GET("/quota", h.GetGeminiEnterpriseQuota)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/quota?auth_index="+auth.Index, nil))
			want, wantReads := "no_data", 1
			if fail {
				want, wantReads = "unavailable", 0
			}
			var snapshot ge.QuotaSnapshot
			if json.Unmarshal(recorder.Body.Bytes(), &snapshot) != nil || snapshot.Status != want || refreshes != 1 || reads != wantReads || strings.Contains(recorder.Body.String(), "synthetic") || strings.Contains(recorder.Body.String(), "never-expose") {
				t.Fatalf("unsafe refresh result: %s refreshes=%d reads=%d", recorder.Body.String(), refreshes, reads)
			}
			after, _ := manager.GetByID(auth.ID)
			afterMetadata, _ := json.Marshal(after.Metadata)
			if (fail && string(before) != string(afterMetadata)) || (!fail && ge.String(after.Metadata, "refresh_token") != "rotated-synthetic-refresh") || after.LastError != nil || after.Unavailable != auth.Unavailable || after.Status != auth.Status {
				t.Fatal("shared refresh lost the rotated token or changed routing health")
			}
		})
	}
}

func TestEnterpriseQuotaHandlerSkipsDisabledAndOtherProviders(t *testing.T) {
	previous := newGeminiEnterpriseQuotaClient
	t.Cleanup(func() { newGeminiEnterpriseQuotaClient = previous })
	newGeminiEnterpriseQuotaClient = func(string) (*http.Client, error) {
		t.Fatal("invalid quota target reached the upstream client")
		return nil, nil
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "disabled", Provider: ge.Provider, Disabled: true},
		{ID: "other", Provider: "codex"},
		{ID: "invalid-binding", Provider: ge.Provider},
	} {
		saved, err := manager.Register(context.Background(), auth)
		if err != nil {
			t.Fatal(err)
		}
		saved.EnsureIndex()
		h := &Handler{authManager: manager}
		router := gin.New()
		router.GET("/quota", h.GetGeminiEnterpriseQuota)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/quota?auth_index="+saved.Index, nil))
		var snapshot ge.QuotaSnapshot
		if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &snapshot) != nil || snapshot.Status != "unavailable" {
			t.Fatalf("invalid credential quota result: %s", recorder.Body.String())
		}
	}
}
