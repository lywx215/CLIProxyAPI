package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func TestEnterpriseQuotaManualAndBackgroundReadsShareOrSerializeByCredential(t *testing.T) {
	m, _ := quotaPressureTestManager()
	a := quotaPressureAuth(t, m, "read-a", "12345", "us", true)
	b := quotaPressureAuth(t, m, "read-b", "12345", "us", true)
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		first := func(ctx context.Context) ge.QuotaSnapshot {
			calls.Add(1)
			<-release
			return ge.QuotaSnapshot{Status: "ok"}
		}
		results := make(chan string, 3)
		read := func(ctx context.Context, auth *Auth, fetch func(context.Context) ge.QuotaSnapshot) {
			results <- m.ReadEnterpriseQuota(ctx, auth, auth.EnterpriseQuotaIdentity, "", fetch).Status
		}
		go read(t.Context(), a, first)
		synctest.Wait()
		go read(t.Context(), a.Clone(), first)
		go read(t.Context(), b, func(context.Context) ge.QuotaSnapshot { calls.Add(1); return ge.QuotaSnapshot{Status: "no_permission"} })
		cancelCtx, cancel := context.WithCancel(t.Context())
		go read(cancelCtx, a.Clone(), first)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("concurrent Google reads: %d", calls.Load())
		}
		cancel()
		if status := <-results; status != "" {
			t.Fatalf("cancelled reader received %q", status)
		}
		close(release)
		got := map[string]int{}
		for range 3 {
			got[<-results]++
		}
		if calls.Load() != 2 || got["ok"] != 2 || got["no_permission"] != 1 {
			t.Fatalf("calls=%d, responses=%v", calls.Load(), got)
		}
	})
}
