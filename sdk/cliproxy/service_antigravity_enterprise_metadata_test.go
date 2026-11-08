package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type enterpriseMetadataTransport func(*http.Request) (*http.Response, error)

func (f enterpriseMetadataTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func enterpriseMetadataService(t *testing.T, bindings int) (*Service, []*coreauth.Auth) {
	t.Helper()
	graph := ae.Account{ID: ae.AccountID(t.Name(), "metadata-client"), Version: 1, Revision: 1}
	for i := 0; i < bindings; i++ {
		license := ae.License{ProjectID: fmt.Sprintf("metadata-project-%d", i), Location: "global", UserTier: "gcp-ge-plus-tier"}
		graph.Bindings = append(graph.Bindings, ae.Binding{ID: ae.BindingID(graph.ID, license), Revision: 1, License: license})
	}
	m := coreauth.NewManager(nil, nil, nil)
	parent, err := m.Register(coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{ID: "metadata-fixture-" + graph.ID,
		Provider: ae.Provider, Status: coreauth.StatusActive, Metadata: ae.PutAccount(map[string]any{
			"google_user_id": t.Name(), "client_id": "metadata-client", "access_token": "synthetic-access",
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}, graph)})
	if err != nil {
		t.Fatal(err)
	}
	children := m.AntigravityEnterpriseBindings(parent.ID)
	for _, child := range children {
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(child.ID) })
	}
	cfg := &config.Config{}
	m.RegisterExecutor(executor.NewAntigravityEnterpriseExecutor(cfg).WithManager(m))
	return &Service{coreManager: m, cfg: cfg}, children
}

func metadataResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAntigravityEnterpriseBackgroundRefreshesBeforeQuotaCacheExpiry(t *testing.T) {
	s, children := enterpriseMetadataService(t, 1)
	var quotaReads atomic.Int32
	refreshed := make(chan struct{}, 1)
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", enterpriseMetadataTransport(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, ":fetchConfig"):
			return metadataResponse(`{"adminControls":{}}`), nil
		case strings.HasSuffix(request.URL.Path, ":fetchQuotaStatus"):
			if quotaReads.Add(1) == 2 {
				refreshed <- struct{}{}
			}
			return metadataResponse(`{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":1}]}],"overageInfo":{}}`), nil
		default:
			t.Errorf("metadata refresh attempted generation: %s", request.URL.Path)
			return nil, fmt.Errorf("unexpected request")
		}
	}))
	s.registerAntigravityEnterpriseModels(ctx, children[0])
	if quotaReads.Load() != 1 {
		t.Fatal("initial registration did not populate quota cache")
	}
	s.startAntigravityEnterpriseMetadata(ctx)
	defer s.stopAntigravityEnterpriseMetadata(t.Context())
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Fatal("background refresh reused the fresh cache instead of refreshing ahead of admission expiry")
	}
}

func TestAntigravityEnterpriseBackgroundWorkIsBoundedDeduplicatedAndCancellable(t *testing.T) {
	s, _ := enterpriseMetadataService(t, 6)
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 6)
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", enterpriseMetadataTransport(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, ":fetchConfig") {
			return metadataResponse(`{"adminControls":{}}`), nil
		}
		if !strings.HasSuffix(request.URL.Path, ":fetchQuotaStatus") {
			t.Errorf("metadata worker attempted generation: %s", request.URL.Path)
			return nil, fmt.Errorf("unexpected request")
		}
		count := active.Add(1)
		defer active.Add(-1)
		for prior := maximum.Load(); count > prior; prior = maximum.Load() {
			if maximum.CompareAndSwap(prior, count) {
				break
			}
		}
		started <- struct{}{}
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	s.startAntigravityEnterpriseMetadata(ctx)
	defer s.stopAntigravityEnterpriseMetadata(t.Context())
	for i := 0; i < antigravityEnterpriseMetadataWorkers; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("background workers did not start")
		}
	}
	for i := 0; i < 3; i++ {
		s.queueAntigravityEnterpriseMetadata(ctx)
	}
	state := s.antigravityEnterpriseState()
	state.mu.Lock()
	pending, queued := len(state.pending), len(state.queue)
	state.mu.Unlock()
	if pending != 6 || queued != 3 || maximum.Load() != antigravityEnterpriseMetadataWorkers {
		t.Fatalf("background work was duplicated or unbounded: pending=%d queued=%d concurrent=%d", pending, queued, maximum.Load())
	}
	s.stopAntigravityEnterpriseMetadata(t.Context())
	state.mu.Lock()
	pending, queued = len(state.pending), len(state.queue)
	state.mu.Unlock()
	if pending != 0 || queued != 0 || active.Load() != 0 || len(state.slots) != 0 {
		t.Fatalf("shutdown leaked background work: pending=%d queued=%d active=%d slots=%d", pending, queued, active.Load(), len(state.slots))
	}
}
