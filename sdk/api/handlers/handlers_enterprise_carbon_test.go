package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestUnregisteredHydrogenUsesNormalModelLookup(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, coreauth.NewManager(nil, nil, nil))
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"OK"}]}]}`)
	_, _, failure := handler.executeWithAuthManagerFormats(t.Context(), "gemini", "gemini", ge.HydrogenModel, payload, "", false, modelExecutionOptions{Operation: "generate_content"})
	if failure == nil || failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered Hydrogen lookup = %#v", failure)
	}
}

func TestEnterpriseCarbonMediaStillUsesIndependentWebChannel(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	cfg := &config.Config{}
	cfg.GeminiEnterpriseWeb.Enabled = true
	manager.SetConfig(cfg)
	credential := web.Credential{ModelPolicy: web.PolicyExcludeArgon, Revision: 1}
	if err := credential.SetURL("https://vertexaisearch.cloud.google.com/us/home/cid/carbon-media-fixture"); err != nil {
		t.Fatal(err)
	}
	if err := credential.SetCookies("SID=synthetic; SAPISID=synthetic"); err != nil {
		t.Fatal(err)
	}
	credential.SetCatalog([]web.AvailableModel{{ID: ge.HydrogenModel}}, time.Now())
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: "carbon-independent-web", Provider: web.Provider, Status: coreauth.StatusActive, Metadata: map[string]any{"enterprise_web": credential.MetadataValue()}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, web.Provider, []*registry.ModelInfo{{ID: ge.HydrogenModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	calls := 0
	manager.RegisterExecutor(&modelExecutionCaptureExecutor{provider: web.Provider, execute: func(_ context.Context, selected *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
		calls++
		if selected.ID != auth.ID || req.Model != ge.HydrogenModel {
			t.Errorf("media target changed: %s/%s", selected.ID, req.Model)
		}
		return coreexecutor.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP"}]}`)}, nil
	}})
	handler := NewBaseAPIHandlers(nil, manager)
	_, _, failure := handler.executeWithAuthManagerFormats(t.Context(), "gemini", "gemini", ge.HydrogenModel, []byte(`{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]}]}`), "", false, modelExecutionOptions{Operation: "generate_content"})
	if failure != nil || calls != 1 {
		t.Fatalf("OAuth Carbon admission captured an independent media request: calls=%d error=%#v", calls, failure)
	}
	if active := manager.WebRuntime().Status()["active_requests"]; active != 0 {
		t.Fatalf("media lease was not released: %#v", manager.WebRuntime().Status())
	}
}
