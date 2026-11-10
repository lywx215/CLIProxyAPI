package handlers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type unknownAcceptanceExecutor struct {
	failOnceStreamExecutor
	count atomic.Int32
}

func (e *unknownAcceptanceExecutor) Identifier() string { return "antigravity" }
func (e *unknownAcceptanceExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (*coreexec.StreamResult, error) {
	e.count.Add(1)
	return nil, coreexec.WithFailureFacts(errors.New("uncertain model acceptance"), coreexec.FailureFacts{Code: "uncertain_acceptance", Stage: "transport", Scope: "request", HTTPStatus: 502, SendState: "possibly_sent", Generation: "unknown", Replay: "unsafe"})
}

func TestObservedUnknownAcceptanceBypassesHandlerBootstrapRetries(t *testing.T) {
	exec := &unknownAcceptanceExecutor{}
	m := coreauth.NewManager(nil, nil, nil)
	m.RegisterExecutor(exec)
	m.SetRetryConfig(3, 0, 2)
	model := "unknown-acceptance-handler-fixture"
	for _, id := range []string{"unknown-auth-a", "unknown-auth-b"} {
		if _, err := m.Register(t.Context(), &coreauth.Auth{ID: id, Provider: "antigravity", Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 3}}, m)
	data, _, errs := h.ExecuteStreamWithAuthManager(t.Context(), "openai", model, []byte(`{"model":"unknown-acceptance-handler-fixture","stream":true}`), "")
	if data != nil {
		for range data {
		}
	}
	var failed bool
	for e := range errs {
		if e != nil {
			failed = true
		}
	}
	if !failed || exec.count.Load() != 1 {
		t.Fatalf("unknown request replayed: failed=%v calls=%d", failed, exec.count.Load())
	}
}
