package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type enterpriseQuotaPressureExecutor struct {
	coreauth.ProviderExecutor
	calls int
	body  []byte
}

func (*enterpriseQuotaPressureExecutor) Identifier() string { return ge.Provider }

func (e *enterpriseQuotaPressureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexec.Request, _ coreexec.Options) (coreexec.Response, error) {
	e.calls++
	binding := ge.FromMetadata(auth.Metadata)
	rejection := ge.ParseGenerationError(e.body, http.StatusTooManyRequests, nil, binding, auth.EnterpriseQuotaIdentity, time.Now(), false)
	rejection.Model = req.Model
	return coreexec.Response{}, rejection
}

func TestEnterpriseQuotaPressureRefreshWithoutBalancing(t *testing.T) {
	for _, test := range []struct {
		name             string
		usageBeforeThird bool
		responseBody     []byte
	}{
		{name: "observation before third request", usageBeforeThird: true},
		{name: "observation after third request"},
		{
			name:             "Google array error before quota read",
			usageBeforeThird: true,
			responseBody:     []byte(`[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota reached"}}]`),
		},
		{
			name:         "Google array error after quota read",
			responseBody: []byte(`[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota reached"}}]`),
		},
		{
			name:             "nested Google error before quota read",
			usageBeforeThird: true,
			responseBody:     []byte(`{"error":{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}}`),
		},
		{
			name:         "nested Google error after quota read",
			responseBody: []byte(`{"error":{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}}`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, a := enterpriseTestService(t, "quota-pressure-service")
			s.cfg.AuthDir = t.TempDir()
			s.configureEnterpriseQuotaProtection(t.Context(), s.cfg)
			m := s.coreManager
			b := a.Clone()
			b.ID = a.ID + "-sibling"
			b.Metadata["project_id"] = "123"
			b.Metadata["email"] = "quota-sibling@example.test"
			b.Metadata["refresh_token"] = "synthetic-sibling-refresh"
			b, err := m.Register(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(b.ID) })

			d := s.enterpriseState()
			models := []ge.Model{
				{ID: "gemini-3.8-flash"},
				{ID: "gemini-3.1-pro-preview"},
				{ID: ge.ArgonModel},
				{ID: "gemini-4-future"},
				{ID: "gemini-5-future"},
			}
			d.fetch = func(context.Context, *coreauth.Auth, string) ([]ge.Model, error) { return models, nil }
			d.fetchSubscription = func(_ context.Context, auth *coreauth.Auth, _ string) ge.SubscriptionObservation {
				trial := false
				principal := "a"
				if auth.ID == b.ID {
					principal = "b"
				}
				return ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial, ProjectNumber: "123", PrincipalHash: strings.Repeat(principal, 64), CheckedAt: time.Now()}
			}
			var quotaReads atomic.Int32
			d.fetchQuota = func(_ context.Context, auth *coreauth.Auth, _ string) ge.QuotaSnapshot {
				return serviceQuotaObservation(auth)
			}
			s.refreshEnterpriseModels(t.Context())
			d.wg.Wait()
			if m.EnterpriseQuotaBalancingEnabled() {
				t.Fatal("paid quota protection enabled balance-based routing")
			}
			for _, auth := range []*coreauth.Auth{a, b} {
				current, _ := m.GetByID(auth.ID)
				if current == nil || !current.EnterpriseQuotaIdentity.ValidFor(ge.FromMetadata(current.Metadata)) || !current.EnterpriseQuotaIdentity.Paid {
					t.Fatalf("paid subscription was not verified for %s", auth.ID)
				}
			}

			readStarted, releaseRead := make(chan struct{}), make(chan struct{})
			d.fetchQuota = func(ctx context.Context, auth *coreauth.Auth, _ string) ge.QuotaSnapshot {
				if _, ok := ctx.Deadline(); ok {
					t.Error("background quota metadata read acquired an upstream deadline")
				}
				if quotaReads.Add(1) == 1 {
					close(readStarted)
				}
				select {
				case <-releaseRead:
					snapshot := serviceQuotaObservation(auth)
					used, remaining := int64(9500), int64(500)
					snapshot.Pools[0].Used, snapshot.Pools[0].Remaining = &used, &remaining
					return snapshot
				case <-ctx.Done():
					return ge.QuotaSnapshot{}
				}
			}
			executor := &enterpriseQuotaPressureExecutor{body: test.responseBody}
			m.RegisterExecutor(executor)
			m.SetRetryConfig(0, 0, 1)
			execute := func(ctx context.Context, auth *coreauth.Auth, model ge.Model) error {
				opts := coreexec.Options{
					SourceFormat: translator.FormatGemini,
					Metadata: map[string]any{
						coreexec.PinnedAuthMetadataKey: auth.ID,
						coreexec.OperationMetadataKey:  "generate_content",
					},
				}
				_, err := m.Execute(ctx, []string{ge.Provider}, coreexec.Request{Model: model.ID}, opts)
				return err
			}
			reject := func(ctx context.Context, auth *coreauth.Auth, model ge.Model) {
				t.Helper()
				before := executor.calls
				err := execute(ctx, auth, model)
				var upstream *ge.GenerationError
				if !errors.As(err, &upstream) || upstream.UpstreamHTTPStatus != http.StatusTooManyRequests || executor.calls != before+1 {
					t.Fatalf("expected one upstream 429 for %s, got %v after %d calls", model.ID, err, executor.calls-before)
				}
			}
			sharedPool := func(auth *coreauth.Auth) string {
				t.Helper()
				current, ok := m.GetByID(auth.ID)
				if !ok {
					t.Fatalf("missing auth %s", auth.ID)
				}
				for _, view := range coreauth.CooldownSnapshotForAuth(current, time.Now()) {
					if view.Origin == "shared_pool" {
						if view.Phase != "cooling" || view.RelatedAuthCount != 2 {
							t.Fatalf("unexpected shared cooldown %+v", view)
						}
						return view.PoolID
					}
				}
				return ""
			}

			logicalRequest := coreauth.WithEnterpriseQuotaRequest(t.Context())
			reject(logicalRequest, a, models[0])
			select {
			case <-readStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("the first Google 429 did not queue a quota read")
			}
			reject(logicalRequest, b, models[1])
			s.refreshEnterpriseModels(t.Context()) // Scheduled and pressure reads coalesce.
			reject(t.Context(), a, models[2])
			if test.usageBeforeThird {
				close(releaseRead)
				d.wg.Wait()
				if sharedPool(a) != "" || sharedPool(b) != "" {
					t.Fatal("retry of the same logical request was counted twice")
				}
			}
			reject(t.Context(), b, models[3])
			if !test.usageBeforeThird {
				if sharedPool(a) != "" || sharedPool(b) != "" {
					t.Fatal("missing usage observation allowed a premature pause")
				}
				close(releaseRead)
				d.wg.Wait()
			}
			if pool := sharedPool(a); pool == "" || sharedPool(b) != pool || quotaReads.Load() != 1 {
				t.Fatalf("expected a shared pause after one background read and three independent 429s, got pool %q and %d reads", pool, quotaReads.Load())
			}
			if err := execute(t.Context(), a, models[4]); err == nil || executor.calls != 4 {
				t.Fatalf("request reached Google during the shared pause, error = %v, calls = %d", err, executor.calls)
			}
		})
	}
}
