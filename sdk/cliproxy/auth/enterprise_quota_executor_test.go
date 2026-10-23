package auth

import (
	"context"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type sharedQuotaStreamExecutor struct {
	ProviderExecutor
	chunks <-chan exec.StreamChunk
}

func (e *sharedQuotaStreamExecutor) ExecuteStream(context.Context, *Auth, exec.Request, exec.Options) (*exec.StreamResult, error) {
	return &exec.StreamResult{Chunks: e.chunks}, nil
}

func TestEnterpriseStreamRejectionPublishedBeforeForwarding(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	m.enterpriseQuota.nowFunc = func() time.Time { return now }
	a := sharedQuotaAuth(t, m, "stream-source", "12345", "us", "enterprise_plus", "alice")
	b := sharedQuotaAuth(t, m, "stream-sibling", "12345", "us", "enterprise_plus", "bob")
	rejection := quotaRejection(now, "edition", "enterprise_plus").(*ge.GenerationError)
	rejection.Started = true
	source := make(chan exec.StreamChunk, 1)
	source <- exec.StreamChunk{Err: rejection}
	close(source)
	e := &enterpriseQuotaExecutor{ProviderExecutor: &sharedQuotaStreamExecutor{chunks: source}}
	stream, err := e.ExecuteStream(context.Background(), a, exec.Request{}, exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	chunk := <-stream.Chunks
	if chunk.Err != rejection || !isRequestScopedError(chunk.Err) {
		t.Fatal("stream rejection lost its started/no-replay classification")
	}
	if blocked, _, _ := isAuthBlockedForModel(b, "new-model", now); !blocked {
		t.Fatal("downstream received the quota error before its shared restriction was published")
	}
	for range stream.Chunks {
	}
	current, _ := m.GetByID(b.ID)
	if current.Failed != 0 || len(current.ModelStates) != 0 {
		t.Fatal("stream rejection manufactured a sibling health failure")
	}
}

func TestEnterpriseStreamProbeRequiresTerminalSuccessAndReleasesOnCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "terminal_success"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			now := time.Now().UTC()
			m.enterpriseQuota.nowFunc = func() time.Time { return now }
			a := sharedQuotaAuth(t, m, "stream-probe", "12345", "us", "enterprise_plus", "alice")
			rejectSharedQuota(t, a, now, "edition")
			now = now.Add(2 * time.Hour)
			source := make(chan exec.StreamChunk, 1)
			source <- exec.StreamChunk{Payload: []byte("synthetic partial response")}
			e := &enterpriseQuotaExecutor{ProviderExecutor: &sharedQuotaStreamExecutor{chunks: source}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := e.ExecuteStream(ctx, a, exec.Request{}, exec.Options{})
			if err != nil {
				t.Fatal(err)
			}
			<-stream.Chunks
			if views := a.enterpriseQuota.views(now); len(views) != 1 || views[0].Phase != "probing" {
				t.Fatal("partial response claimed successful recovery")
			}
			if canceled {
				cancel()
				for range stream.Chunks {
				}
				close(source)
				if views := a.enterpriseQuota.views(now); len(views) != 1 || views[0].Phase != "probe_pending" {
					t.Fatal("cancellation stranded or cleared the recovery lease")
				}
			} else {
				close(source)
				for range stream.Chunks {
				}
				if len(a.enterpriseQuota.views(now)) != 0 {
					t.Fatal("terminal stream success did not verify recovery")
				}
			}
		})
	}
}
