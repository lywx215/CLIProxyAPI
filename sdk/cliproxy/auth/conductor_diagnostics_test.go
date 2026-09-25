package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type diagnosticAttemptExecutor struct{ invocations int }

func (*diagnosticAttemptExecutor) Identifier() string { return "gemini" }
func (e *diagnosticAttemptExecutor) observe(ctx context.Context) error {
	e.invocations++
	diagnostics.ObserveNormalized(ctx, []byte(`{"contents":[]}`), []byte(`{"contents":[]}`))
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	return &Error{HTTPStatus: 500, Message: "synthetic upstream failure"}
}
func (e *diagnosticAttemptExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.observe(ctx)
}
func (e *diagnosticAttemptExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.observe(ctx)
}
func (*diagnosticAttemptExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }
func (*diagnosticAttemptExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (*diagnosticAttemptExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestDIAG05ConductorOwnsAttemptIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		m := NewManager(nil, quotaAttemptIsolationSelector{}, nil)
		ex := &diagnosticAttemptExecutor{}
		m.RegisterExecutor(ex)
		model := "diagnostic-conductor-fixture"
		for _, id := range []string{"diag-a", "diag-b"} {
			if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "gemini", Status: StatusActive}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(id, "gemini", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(id)
		}
		var records []diagnostics.Record
		e := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
			var r diagnostics.Record
			if err := json.Unmarshal(b[6:], &r); err != nil {
				return err
			}
			if r.Event == "request.normalized" {
				records = append(records, r)
			}
			return nil
		})
		ctx, s := e.StartServer(context.Background(), nil, "fixture")
		var err error
		if stream {
			_, err = m.ExecuteStream(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		} else {
			_, err = m.Execute(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		}
		if err == nil {
			t.Fatal("expected fixture failure")
		}
		s.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
		if len(records) != 2 || ex.invocations != 2 {
			t.Fatalf("owner retry changed: %d records, %d invocations", len(records), ex.invocations)
		}
		for i, r := range records {
			if r.AttemptNo == nil || *r.AttemptNo != uint64(i+1) || r.AttemptID == nil || r.RetryScope == nil || *r.RetryScope != "conductor_gemini_family" {
				t.Fatalf("unowned attempt %+v", r)
			}
		}
		if *records[0].AttemptID == *records[1].AttemptID {
			t.Fatal("attempt reused")
		}
	}
}
