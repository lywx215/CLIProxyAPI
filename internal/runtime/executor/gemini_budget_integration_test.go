package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type budgetIntegrationTransport func(*http.Request) (*http.Response, error)

func (f budgetIntegrationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func budgetIntegrationManager(t *testing.T, keys int) (*coreauth.Manager, *config.Config, []*coreauth.Auth) {
	t.Helper()
	cfg := &config.Config{GeminiFreeTierPolicy: config.GeminiFreeTierPolicyConfig{StoragePath: t.TempDir(), ManagedModels: map[string]int{"gemini-2.5-flash": 20, "gemini-3.5-flash-lite": 500}}}
	manager := coreauth.NewManager(nil, nil, nil)
	var auths []*coreauth.Auth
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("synthetic-budget-%s-%d", t.Name(), i)
		cfg.GeminiKey = append(cfg.GeminiKey, config.GeminiKey{APIKey: key, Models: []config.GeminiModel{{Name: "gemini-2.5-flash", Alias: "public-flash"}, {Name: "gemini-3.5-flash-lite", Alias: "public-lite"}}})
		auths = append(auths, &coreauth.Auth{ID: key, Provider: "gemini", Attributes: map[string]string{"api_key": key, "config_index": fmt.Sprint(i)}})
	}
	manager.SetConfig(cfg)
	engine, err := manager.EnsureGeminiBudget(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfg.GeminiFreeTierPolicy.Enabled = true
	manager.SetConfig(cfg)
	manager.SetRetryConfig(10, 0, 0)
	manager.RegisterExecutor(NewGeminiExecutor(cfg).WithBudgetEngine(engine))
	for i, a := range auths {
		registered, err := manager.Register(t.Context(), a)
		if err != nil {
			t.Fatal(err)
		}
		auths[i] = registered
		registry.GetGlobalRegistry().RegisterClient(a.ID, "gemini", []*registry.ModelInfo{{ID: "public-flash"}, {ID: "public-lite"}, {ID: "gemini-2.5-flash", Thinking: &registry.ThinkingSupport{Min: 0, Max: 24576, ZeroAllowed: true, DynamicAllowed: true}}})
		id := a.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	t.Cleanup(func() { _ = manager.CloseGeminiBudget() })
	return manager, cfg, auths
}

func TestGeminiBudgetConductorLimitsPhysicalSendsAndSanitizesErrors(t *testing.T) {
	m, _, auths := budgetIntegrationManager(t, 8)
	var sends atomic.Int32
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", budgetIntegrationTransport(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":503,"message":"projects/private-number/models/gemini-2.5-flash overloaded"}}`))}, nil
	}))
	_, err := m.Execute(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: "public-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err == nil {
		t.Fatal("expected upstream failure")
	}
	if sends.Load() != 3 {
		t.Fatalf("physical sends=%d, want 3 despite ten outer retries", sends.Load())
	}
	if strings.Contains(err.Error(), "gemini-2.5-flash") || strings.Contains(err.Error(), "projects/") {
		t.Fatalf("private routing leaked: %v", err)
	}
	for _, a := range auths {
		current, _ := m.GetByID(a.ID)
		if len(current.ModelStates) != 0 {
			t.Fatal("managed result entered generic credential cooldown persistence")
		}
	}
}

func TestGeminiBudgetConductorUnlistedModelAlsoLimitsPhysicalSends(t *testing.T) {
	m, cfg, _ := budgetIntegrationManager(t, 8)
	for i := range cfg.GeminiKey {
		cfg.GeminiKey[i].Models[0].Name = "unlisted-private-model"
	}
	m.SetConfig(cfg)
	var sends atomic.Int32
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", budgetIntegrationTransport(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"projects/private unlisted-private-model"}}`))}, nil
	}))
	_, err := m.Execute(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: "public-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err == nil || sends.Load() != 3 {
		t.Fatalf("unlisted model bypassed channel cap: sends=%d err=%v", sends.Load(), err)
	}
	if strings.Contains(err.Error(), "unlisted-private-model") || strings.Contains(err.Error(), "projects/") {
		t.Fatalf("unlisted model error leaked: %v", err)
	}
	if len(m.GeminiBudgetEngine().Snapshot().Entries) != 0 {
		t.Fatal("unlisted model charged a configured daily quota")
	}
}

func TestGeminiBudgetConductorFlashExhaustionLeavesLiteRoutable(t *testing.T) {
	m, _, auths := budgetIntegrationManager(t, 1)
	scope := coreauth.GeminiBudgetScope(auths[0], "gemini-2.5-flash")
	if err := m.GeminiBudgetEngine().Observe(t.Context(), scope, budget.Signal{Kind: budget.SignalRPD}); err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", budgetIntegrationTransport(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		if !strings.Contains(r.URL.Path, "gemini-3.5-flash-lite:") {
			t.Errorf("wrong upstream: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.5-flash-lite"}`))}, nil
	}))
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`)
	if _, err := m.Execute(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: "public-flash", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); err == nil {
		t.Fatal("exhausted Flash was routed")
	}
	response, err := m.Execute(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: "public-lite", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 {
		t.Fatalf("sends=%d, want only Lite", sends.Load())
	}
	if strings.Contains(string(response.Payload), "gemini-3.5-flash-lite") {
		t.Fatal("response model was not rewritten to alias")
	}
	if used := m.GeminiBudgetEngine().View(coreauth.GeminiBudgetScope(auths[0], "gemini-3.5-flash-lite")).Used; used != 1 {
		t.Fatalf("Lite ledger used=%d", used)
	}
}

func TestGeminiBudgetUnavailableBindingCannotUseStaleExecutorLedger(t *testing.T) {
	m, cfg, auths := budgetIntegrationManager(t, 1)
	exec := NewGeminiExecutor(cfg).WithBudgetEngine(m.GeminiBudgetEngine())
	var sends atomic.Int32
	ctx := budget.WithEngine(context.WithValue(context.Background(), "cliproxy.roundtripper", budgetIntegrationTransport(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return nil, fmt.Errorf("unexpected network")
	})), nil)
	_, err := exec.Execute(ctx, auths[0], cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err == nil || sends.Load() != 0 {
		t.Fatalf("stale ledger used: sends=%d err=%v", sends.Load(), err)
	}
}

func TestGeminiBudgetConductorThinkingVariantsShareActualBucket(t *testing.T) {
	m, _, auths := budgetIntegrationManager(t, 1)
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", budgetIntegrationTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "gemini-2.5-flash:generateContent") {
			t.Errorf("thinking suffix reached physical model: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))}, nil
	}))
	for _, model := range []string{"public-flash:low", "public-flash:thinking"} {
		if _, err := m.Execute(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); err != nil {
			t.Fatal(err)
		}
	}
	if used := m.GeminiBudgetEngine().View(coreauth.GeminiBudgetScope(auths[0], "gemini-2.5-flash")).Used; used != 2 {
		t.Fatalf("thinking variants used %d instead of same bucket twice", used)
	}
}

type budgetFailAfterChunk struct{ reader *strings.Reader }

func (r *budgetFailAfterChunk) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
func (*budgetFailAfterChunk) Close() error { return nil }

func TestGeminiBudgetConductorStreamNeverRegeneratesAfterDeliveredChunk(t *testing.T) {
	m, _, auths := budgetIntegrationManager(t, 5)
	var sends atomic.Int32
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", budgetIntegrationTransport(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &budgetFailAfterChunk{reader: strings.NewReader("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"first\"}]}}],\"modelVersion\":\"gemini-2.5-flash\"}\n\n")}}, nil
	}))
	result, err := m.ExecuteStream(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: "public-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if err != nil {
		t.Fatal(err)
	}
	var delivered, failed bool
	for chunk := range result.Chunks {
		if strings.Contains(string(chunk.Payload), "first") {
			delivered = true
		}
		if strings.Contains(string(chunk.Payload), "gemini-2.5-flash") {
			t.Fatal("stream leaked real model")
		}
		if chunk.Err != nil {
			failed = true
		}
	}
	if !delivered || !failed || sends.Load() != 1 {
		t.Fatalf("stream replayed or lost failure: delivered=%v failed=%v sends=%d", delivered, failed, sends.Load())
	}
	used := 0
	for _, a := range auths {
		used += m.GeminiBudgetEngine().View(coreauth.GeminiBudgetScope(a, "gemini-2.5-flash")).Used
	}
	if used != 1 {
		t.Fatalf("stream transmission was refunded: used=%d", used)
	}
}
