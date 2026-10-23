package auth_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"golang.org/x/oauth2"
)

type enterpriseRefreshTransport func(*http.Request) (*http.Response, error)

func (f enterpriseRefreshTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f enterpriseRefreshTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper  { return f }

func TestEnterpriseConcurrentUnauthorizedRefreshPersistsOnce(t *testing.T) {
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(t.TempDir())
	m := coreauth.NewManager(store, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	m.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(&config.Config{}))
	binding := config.GeminiEnterpriseOAuth{ClientID: "own-client", ProjectID: "project", EngineID: "engine"}
	before, err := m.Register(context.Background(), &coreauth.Auth{ID: "enterprise-refresh.json", FileName: "enterprise-refresh.json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: ge.Metadata(binding, "test@example.com", &oauth2.Token{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)})})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(before.ID, ge.Provider, []*registry.ModelInfo{{ID: "gemini-4-argon"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(before.ID) })
	var tokenCalls, oldCalls atomic.Int32
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = enterpriseRefreshTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://oauth2.googleapis.com/token" {
			t.Errorf("unexpected credential origin %s", req.URL.Host)
			return nil, io.ErrUnexpectedEOF
		}
		tokenCalls.Add(1)
		_ = req.ParseForm()
		if req.Form.Get("client_id") != "own-client" || req.Form.Get("refresh_token") != "refresh" {
			t.Error("refresh lost account-owned OAuth client")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"new","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`))}, nil
	})
	gate := make(chan struct{})
	m.SetRoundTripperProvider(enterpriseRefreshTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "www.googleapis.com" {
			return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		if req.Header.Get("Authorization") == "Bearer old" {
			if oldCalls.Add(1) == 8 {
				close(gate)
			}
			<-gate
			return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("rejected"))}, nil
		}
		if req.Header.Get("Authorization") != "Bearer new" {
			t.Error("rotated access token not used")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"ok"}}]}},{"answer":{"state":"SUCCEEDED"}}]`))}, nil
	}))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := m.Execute(context.Background(), []string{ge.Provider}, exec.Request{Model: "gemini-4-argon", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, exec.Options{SourceFormat: translator.FormatOpenAI, Metadata: map[string]any{exec.OperationMetadataKey: "chat_completions"}})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("concurrent refreshes=%d, want 1", tokenCalls.Load())
	}
	after, _ := m.GetByID(before.ID)
	if after.CredentialVersion <= before.CredentialVersion || ge.String(after.Metadata, "refresh_token") != "rotated" {
		t.Fatal("manager lost credential version or refresh-token rotation")
	}
	restored, err := store.List(context.Background())
	if err != nil || len(restored) != 1 || ge.String(restored[0].Metadata, "access_token") != "new" || ge.String(restored[0].Metadata, "refresh_token") != "rotated" || ge.Binding(ge.FromMetadata(restored[0].Metadata)) != ge.Binding(binding) {
		t.Fatalf("stored refresh roundtrip failed: records=%d err=%v", len(restored), err)
	}
}
