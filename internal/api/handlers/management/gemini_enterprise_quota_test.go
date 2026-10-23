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
	"golang.org/x/oauth2"
)

func TestEnterpriseQuotaHandlerKeepsPermissionFailuresOutOfCredentialHealth(t *testing.T) {
	previous := newGeminiEnterpriseQuotaClient
	t.Cleanup(func() { newGeminiEnterpriseQuotaClient = previous })
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

func TestEnterpriseQuotaHandlerRefreshUsesSavedBindingWithoutMutatingAuth(t *testing.T) {
	previousClient, previousRefresh := newGeminiEnterpriseQuotaClient, refreshGeminiEnterpriseQuotaToken
	t.Cleanup(func() {
		newGeminiEnterpriseQuotaClient, refreshGeminiEnterpriseQuotaToken = previousClient, previousRefresh
	})
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
			refreshGeminiEnterpriseQuotaToken = func(ctx context.Context, binding config.GeminiEnterpriseOAuth, refresh, proxy string) (*oauth2.Token, error) {
				refreshes++
				if binding.ClientID != "saved-client" || binding.ProjectID != "saved-project" || binding.EngineID != "saved-engine" || refresh != "saved-synthetic-refresh" || proxy != auth.ProxyURL {
					t.Fatal("quota refresh inherited another credential or global binding")
				}
				if fail {
					return nil, errors.New("never-expose-refresh-secret")
				}
				return &oauth2.Token{AccessToken: "renewed-synthetic"}, nil
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
			if string(before) != string(afterMetadata) || after.LastError != nil || after.Unavailable != auth.Unavailable || after.Status != auth.Status {
				t.Fatal("quota refresh changed persisted credentials or routing health")
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
