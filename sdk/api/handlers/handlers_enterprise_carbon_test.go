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

func TestEnterpriseCarbonUnavailableDoesNotFallBackToOtherProviders(t *testing.T) {
	for _, alias := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := ge.CarbonModel
			if alias {
				name = "enterprise-carbon-handler-alias"
			}
			t.Run(name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				calls := 0
				executor := &modelExecutionCaptureExecutor{
					execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
						calls++
						return coreexecutor.Response{Payload: []byte("wrong provider")}, nil
					},
					stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
						calls++
						chunks := make(chan coreexecutor.StreamChunk)
						close(chunks)
						return &coreexecutor.StreamResult{Chunks: chunks}, nil
					},
				}
				handler := newModelExecutionHandler(t, name, executor, nil)
				if alias {
					handler.AuthManager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{ge.Provider: {{Name: ge.CarbonModel, Alias: name}}})
				}
				payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"OK"}]}]}`)
				if stream {
					data, _, failures := handler.executeStreamWithAuthManagerFormats(t.Context(), "gemini", "gemini", name, payload, "", false, modelExecutionOptions{Operation: "generate_content"})
					if data != nil {
						t.Fatal("closed Carbon returned a business stream")
					}
					failure := <-failures
					if failure == nil || failure.StatusCode != http.StatusServiceUnavailable {
						t.Fatalf("closed Carbon stream error = %#v", failure)
					}
				} else {
					_, _, failure := handler.executeWithAuthManagerFormats(t.Context(), "gemini", "gemini", name, payload, "", false, modelExecutionOptions{Operation: "generate_content"})
					if failure == nil || failure.StatusCode != http.StatusServiceUnavailable {
						t.Fatalf("closed Carbon error = %#v", failure)
					}
				}
				if calls != 0 {
					t.Fatalf("closed Carbon was sent to another provider %d times", calls)
				}
			})
		}
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
	credential.SetCatalog([]web.AvailableModel{{ID: ge.CarbonModel}}, time.Now())
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: "carbon-independent-web", Provider: web.Provider, Status: coreauth.StatusActive, Metadata: map[string]any{"enterprise_web": credential.MetadataValue()}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, web.Provider, []*registry.ModelInfo{{ID: ge.CarbonModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	calls := 0
	manager.RegisterExecutor(&modelExecutionCaptureExecutor{provider: web.Provider, execute: func(_ context.Context, selected *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
		calls++
		if selected.ID != auth.ID || req.Model != ge.CarbonModel {
			t.Errorf("media target changed: %s/%s", selected.ID, req.Model)
		}
		return coreexecutor.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP"}]}`)}, nil
	}})
	handler := NewBaseAPIHandlers(nil, manager)
	_, _, failure := handler.executeWithAuthManagerFormats(t.Context(), "gemini", "gemini", ge.CarbonModel, []byte(`{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]}]}`), "", false, modelExecutionOptions{Operation: "generate_content"})
	if failure != nil || calls != 1 {
		t.Fatalf("OAuth Carbon admission captured an independent media request: calls=%d error=%#v", calls, failure)
	}
	if active := manager.WebRuntime().Status()["active_requests"]; active != 0 {
		t.Fatalf("media lease was not released: %#v", manager.WebRuntime().Status())
	}
}
