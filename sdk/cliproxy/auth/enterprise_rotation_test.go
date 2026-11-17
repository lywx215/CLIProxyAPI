package auth

import (
	"context"
	"testing"
	"testing/synctest"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestEnterpriseRotationDrainTracksCancelledUpstream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, _ := balanceTestManager(t)
		auth := sharedQuotaAuth(t, m, "rotation-drain", "111", "us", "enterprise_plus", "a")
		m.PublishEnterprisePolicyCatalog(auth, []ge.Model{{ID: ge.ArgonModel}})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := make(chan exec.StreamChunk)
		defer func() {
			if source != nil {
				close(source)
			}
		}()
		e := &enterpriseQuotaExecutor{ProviderExecutor: &sharedQuotaStreamExecutor{chunks: source}}
		stream, err := e.ExecuteStream(ctx, auth, exec.Request{Model: ge.ArgonModel}, exec.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		for range stream.Chunks {
		}
		synctest.Wait()
		if got := m.EnterpriseRotationInFlight(auth.ID); got != 1 {
			t.Fatalf("cancelled producer in flight = %d, want 1", got)
		}
		if view := m.EnterpriseQuotaBalance(auth); view.InFlight != 0 {
			t.Fatalf("the original balancing behavior changed: %+v", view)
		}
		close(source)
		source = nil
		synctest.Wait()
		if got := m.EnterpriseRotationInFlight(auth.ID); got != 0 {
			t.Fatalf("producer finished, still in flight = %d", got)
		}
	})
}
