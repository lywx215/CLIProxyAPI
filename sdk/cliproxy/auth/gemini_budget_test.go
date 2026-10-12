package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
)

func TestGeminiBudgetModelAndProjectIdentity(t *testing.T) {
	a := &Auth{Provider: "gemini", Attributes: map[string]string{"api_key": "synthetic-key", "quota_project": "synthetic-project", "config_index": "0"}}
	cfg := &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "synthetic-key", Models: []config.GeminiModel{{Name: "gemini-3.5-flash-lite", Alias: "public-lite"}}}}}
	for _, name := range []string{"public-lite", "public-lite:low", "public-lite:thinking", "public-lite(high)"} {
		if got := ResolveCanonicalBudgetModel(name, a, cfg); got != "gemini-3.5-flash-lite" {
			t.Fatalf("%s mapped to %s", name, got)
		}
	}
	b := a.Clone()
	b.Index = "new-group-index"
	if GeminiBudgetScope(a, "gemini-2.5-flash").CredentialID != GeminiBudgetScope(b, "gemini-2.5-flash").CredentialID {
		t.Fatal("regrouping changed credential quarantine identity")
	}
	b.Attributes["api_key"] = "another-synthetic-key"
	if GeminiBudgetScope(a, "gemini-2.5-flash").CredentialID == GeminiBudgetScope(b, "gemini-2.5-flash").CredentialID {
		t.Fatal("different keys shared credential quarantine identity")
	}
	b.Prefix = "different-group-prefix"
	if GeminiBudgetProject(a) != GeminiBudgetProject(b) {
		t.Fatal("same project split across keys/prefixes")
	}
	delete(b.Attributes, "quota_project")
	if GeminiBudgetProject(a) == GeminiBudgetProject(b) {
		t.Fatal("explicit and default project identities collided")
	}
	if !IsOfficialGeminiAuth(a) {
		t.Fatal("official auth was excluded")
	}
	b.Attributes["base_url"] = "https://custom.example.invalid"
	if IsOfficialGeminiAuth(b) {
		t.Fatal("custom upstream was managed")
	}
}

func newManagedLegacyTestManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	cfg := &config.Config{GeminiFreeTierPolicy: config.GeminiFreeTierPolicyConfig{StoragePath: t.TempDir(), ManagedModels: map[string]int{"gemini-2.5-flash": 20}}}
	m.SetConfig(cfg)
	engine, err := m.EnsureGeminiBudget(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseGeminiBudget() })
	if err = engine.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.GeminiFreeTierPolicy.Enabled = true
	m.SetConfig(cfg)
	return m
}

func legacyGoogleCooldownAuth(now time.Time, deadline time.Time) *Auth {
	raw := &Error{HTTPStatus: http.StatusTooManyRequests, Message: `{"error":{"message":"Quota exceeded for projects/123456 model gemini-private-upstream"}}`}
	return &Auth{ID: "synthetic-legacy-key", Provider: "gemini", Status: StatusError, Unavailable: true, NextRetryAfter: deadline, LastError: raw,
		Attributes:  map[string]string{"api_key": "synthetic-only-key"},
		Quota:       QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: deadline},
		ModelStates: map[string]*ModelState{"gemini-2.5-flash": {Status: StatusError, Unavailable: true, NextRetryAfter: deadline, LastError: raw, UpdatedAt: now, Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline}}}}
}

func TestGeminiBudgetLegacyCooldownUsesLedgerAdmission(t *testing.T) {
	m := newManagedLegacyTestManager(t)
	now := time.Now()
	for _, deadline := range []time.Time{now.Add(time.Hour), now.Add(-time.Hour)} {
		auth := legacyGoogleCooldownAuth(now, deadline)
		available, err := m.availableAuthsForRouteModel([]*Auth{auth}, "gemini", "gemini-2.5-flash", now)
		if err != nil || len(available) != 1 {
			t.Fatalf("legacy cooldown overrode healthy ledger: %v, candidates=%d", err, len(available))
		}
		if !auth.Unavailable || len(auth.ModelStates) != 1 || !strings.Contains(auth.LastError.Message, "projects/123456") {
			t.Fatal("selection mutated stored credential state")
		}
		models := m.filterExecutionModels(auth, "gemini-2.5-flash", []string{"gemini-2.5-flash"}, false)
		if len(models) != 1 {
			t.Fatal("execution retained obsolete generic cooldown")
		}
		projection := m.clientModelProjectionForAuth(auth, "gemini-2.5-flash", now)
		if projection.Suspended || projection.QuotaExceeded {
			t.Fatal("registry retained obsolete generic cooldown")
		}
	}
}

func TestGeminiBudgetLegacyRestrictionsAndErrors(t *testing.T) {
	m := newManagedLegacyTestManager(t)
	now := time.Now()
	for _, kind := range []string{"credential-disabled", "model-disabled", "terminal-invalid"} {
		auth := legacyGoogleCooldownAuth(now, now.Add(time.Hour))
		switch kind {
		case "credential-disabled":
			auth.Disabled = true
		case "model-disabled":
			auth.ModelStates["gemini-2.5-flash"].Status = StatusDisabled
		case "terminal-invalid":
			auth.NextRetryAfter = time.Time{}
			auth.LastError = &Error{HTTPStatus: http.StatusUnauthorized, Message: "Invalid credential for projects/123456 model gemini-private-upstream"}
		}
		available, err := m.availableAuthsForRouteModel([]*Auth{auth}, "gemini", "gemini-2.5-flash", now)
		if err == nil || len(available) != 0 {
			t.Fatalf("%s restriction was bypassed", kind)
		}
		for wrapped := err; wrapped != nil; {
			if strings.Contains(wrapped.Error(), "projects/123456") || strings.Contains(wrapped.Error(), "gemini-private-upstream") {
				t.Fatalf("%s leaked legacy upstream error: %v", kind, err)
			}
			unwrapper, ok := wrapped.(interface{ Unwrap() error })
			if !ok {
				break
			}
			wrapped = unwrapper.Unwrap()
		}
		if models := m.filterExecutionModels(auth, "gemini-2.5-flash", []string{"gemini-2.5-flash"}, false); len(models) != 0 {
			t.Fatalf("%s restriction was bypassed during execution", kind)
		}
	}
}

func TestGeminiBudgetLegacyUnmanagedAndCustomStateUnchanged(t *testing.T) {
	m := newManagedLegacyTestManager(t)
	now := time.Now()
	for _, kind := range []string{"unmanaged", "custom"} {
		auth := legacyGoogleCooldownAuth(now, now.Add(time.Hour))
		model := "gemini-2.5-flash"
		if kind == "unmanaged" {
			model = "gemini-unmanaged"
			auth.ModelStates[model] = auth.ModelStates["gemini-2.5-flash"]
		} else {
			auth.Attributes["base_url"] = "https://custom.example.invalid"
		}
		if got := m.geminiBudgetSelectionAuth(auth, model); got != auth {
			t.Fatalf("%s state was projected by managed policy", kind)
		}
		available, err := m.availableAuthsForRouteModel([]*Auth{auth}, "gemini", model, now)
		if err == nil || len(available) != 0 {
			t.Fatalf("%s legacy cooldown changed", kind)
		}
	}
}

func TestGeminiBudgetLegacyAliasHonorsActualModelDisable(t *testing.T) {
	m := newManagedLegacyTestManager(t)
	current, _ := m.runtimeConfig.Load().(*config.Config)
	cfg := current.CloneForRuntime()
	cfg.GeminiKey = []config.GeminiKey{{APIKey: "synthetic-only-key", Models: []config.GeminiModel{{Name: "gemini-2.5-flash", Alias: "public-flash"}}}}
	m.SetConfig(cfg)
	now := time.Now()
	auth := legacyGoogleCooldownAuth(now, now.Add(time.Hour))
	auth.Attributes["config_index"] = "0"
	available, err := m.availableAuthsForRouteModel([]*Auth{auth}, "gemini", "public-flash", now)
	if err != nil || len(available) != 1 {
		t.Fatalf("alias inherited obsolete cooldown: %v", err)
	}
	auth.ModelStates["gemini-2.5-flash"].Status = StatusDisabled
	available, err = m.availableAuthsForRouteModel([]*Auth{auth}, "gemini", "public-flash", now)
	if err == nil || len(available) != 0 {
		t.Fatal("alias bypassed administrative upstream model disable")
	}
	if models := m.filterExecutionModels(auth, "public-flash", []string{"gemini-2.5-flash"}, false); len(models) != 0 {
		t.Fatal("alias execution bypassed administrative upstream model disable")
	}
}

func TestGeminiBudgetHotReloadKeepsUsageAndRequiresRestartForPath(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := &config.Config{GeminiFreeTierPolicy: config.GeminiFreeTierPolicyConfig{StoragePath: t.TempDir(), ManagedModels: map[string]int{"gemini-2.5-flash": 20, "gemini-3.5-flash-lite": 500}}}
	m.SetConfig(cfg)
	engine, err := m.EnsureGeminiBudget(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseGeminiBudget() })
	if err = engine.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	scope := budget.Scope{Project: "test-project", Model: "gemini-2.5-flash"}
	ticket, err := engine.Reserve(budget.WithRequest(context.Background()), scope)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Release()
	cfg.GeminiFreeTierPolicy.Enabled = true
	m.SetConfig(cfg)
	same, err := m.EnsureGeminiBudget(context.Background(), false)
	if err != nil || same != engine {
		t.Fatalf("ledger recreated: %v", err)
	}
	if got := same.View(scope).Used; got != 1 {
		t.Fatalf("reload cleared usage: %d", got)
	}
	cfg.GeminiFreeTierPolicy.StoragePath = t.TempDir()
	m.SetConfig(cfg)
	if _, err = m.EnsureGeminiBudget(context.Background(), false); err == nil {
		t.Fatal("path switched without restart")
	}
	if m.GeminiBudgetEngine() != nil {
		t.Fatal("unsafe path change did not fail closed")
	}
}
