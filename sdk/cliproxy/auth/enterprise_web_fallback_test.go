package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const mediaFallbackTestModel = "gemini-3.8-flash"

type mediaFallbackTestExecutor struct {
	ProviderExecutor
	provider string
	run      func(context.Context, *Auth, exec.Request, exec.Options) error
}

func (e *mediaFallbackTestExecutor) Identifier() string { return e.provider }

func (e *mediaFallbackTestExecutor) Execute(ctx context.Context, a *Auth, req exec.Request, opts exec.Options) (exec.Response, error) {
	if err := e.run(ctx, a, req, opts); err != nil {
		return exec.Response{}, err
	}
	return exec.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP"}]}`)}, nil
}

func (e *mediaFallbackTestExecutor) ExecuteStream(ctx context.Context, a *Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	result, err := e.Execute(ctx, a, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan exec.StreamChunk, 1)
	chunks <- exec.StreamChunk{Payload: result.Payload}
	close(chunks)
	return &exec.StreamResult{Chunks: chunks}, nil
}

func callMediaFallbackTest(t *testing.T, m *Manager, stream bool, ctx context.Context, req exec.Request, opts exec.Options) error {
	t.Helper()
	if !stream {
		_, err := m.Execute(ctx, []string{ge.Provider}, req, opts)
		return err
	}
	opts.Stream = true
	opts.Metadata[exec.OperationMetadataKey] = "stream_generate_content"
	result, err := m.ExecuteStream(ctx, []string{ge.Provider}, req, opts)
	if err != nil {
		return err
	}
	var lastErr error
	for chunk := range result.Chunks {
		lastErr = chunk.Err
	}
	return lastErr
}

func registerMediaFallbackEnterprise(t *testing.T, m *Manager, run func(context.Context, *Auth, exec.Request, exec.Options) error) {
	t.Helper()
	registerEnterpriseModelTestAuth(t, m, "text-fallback-enterprise", "global", "fixture-engine", mediaFallbackTestModel)
	m.RegisterExecutor(&mediaFallbackTestExecutor{provider: ge.Provider, run: run})
}

func assertFallbackDispatch(t *testing.T, ctx context.Context, req exec.Request, opts exec.Options, reason web.FallbackReason) {
	t.Helper()
	info, ok := web.MediaFallbackFrom(ctx)
	if !ok || info.Reason != reason || web.ExistingLease(ctx) != nil || req.Model != mediaFallbackTestModel ||
		!bytes.Equal(req.Payload, opts.OriginalRequest) || web.HasMedia(req.Payload) || !bytes.Contains(req.Payload, []byte(`"read"`)) {
		t.Fatalf("invalid text dispatch: fallback=%+v model=%q payload=%s", info, req.Model, req.Payload)
	}
}

func TestEnterpriseWebMediaFallbackAdmission(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			open   bool
			policy string
			reason web.FallbackReason
		}{
			{"closed", false, web.PolicyExcludeArgon, web.FallbackWebDisabled},
			{"no_credential", true, "", web.FallbackNoCredential},
			{"wrong_policy", true, web.PolicyArgonOnly, web.FallbackNoCredential},
			{"web_available", true, web.PolicyExcludeArgon, ""},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &config.Config{}
				cfg.GeminiEnterpriseWeb.Enabled = tc.open
				m.SetConfig(cfg)
				ordinaryCalls, webCalls := 0, 0
				registerMediaFallbackEnterprise(t, m, func(ctx context.Context, _ *Auth, req exec.Request, opts exec.Options) error {
					ordinaryCalls++
					assertFallbackDispatch(t, ctx, req, opts, tc.reason)
					return nil
				})
				m.RegisterExecutor(&mediaFallbackTestExecutor{provider: web.Provider, run: func(ctx context.Context, _ *Auth, req exec.Request, _ exec.Options) error {
					webCalls++
					if web.ExistingLease(ctx) == nil || !web.HasMedia(req.Payload) {
						t.Fatal("eligible Web credential did not receive admitted media")
					}
					return nil
				}})
				if tc.policy != "" {
					registerWebPolicyAccount(t, m, "fallback-web", tc.policy, 1, "")
				}
				if err := callMediaFallbackTest(t, m, stream, t.Context(), webPolicyRequest(mediaFallbackTestModel), webPolicyOptions()); err != nil {
					t.Fatal(err)
				}
				status := m.WebRuntime().Status()
				counts := status["media_fallback_requests"].(map[string]uint64)
				if tc.reason == "" && (webCalls != 1 || ordinaryCalls != 0) ||
					tc.reason != "" && (webCalls != 0 || ordinaryCalls != 1 || counts[string(tc.reason)] != 1) ||
					status["active_requests"] != 0 || status["inflight_bytes"] != int64(0) {
					t.Fatalf("web=%d ordinary=%d status=%v", webCalls, ordinaryCalls, status)
				}
			})
		}
	}
}

type mediaFallbackLoginTransport int

func (s mediaFallbackLoginTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(s), Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

func TestEnterpriseWebMediaFallbackAfterPreflight(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			secondStatus int
			limit        int
			started      bool
			wantWeb      int
			wantOrdinary int
			wantErr      bool
		}{
			{"both_rejected", 401, 2, false, 2, 1, false},
			{"backup_available", 200, 2, false, 2, 0, false},
			{"backup_rate_limited", 429, 2, false, 2, 0, true},
			{"untried_backup", 200, 1, false, 1, 0, true},
			{"session_started", 401, 1, true, 1, 0, true},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				m := NewManager(nil, &enterpriseModelTestSelector{}, nil)
				cfg := &config.Config{}
				cfg.GeminiEnterpriseWeb.Enabled = true
				m.SetConfig(cfg)
				m.SetRetryConfig(0, 0, tc.limit)
				ordinaryCalls, webCalls := 0, 0
				registerMediaFallbackEnterprise(t, m, func(ctx context.Context, _ *Auth, req exec.Request, opts exec.Options) error {
					ordinaryCalls++
					assertFallbackDispatch(t, ctx, req, opts, web.FallbackNoCredential)
					return nil
				})
				registerWebPolicyAccount(t, m, "fallback-first", web.PolicyExcludeArgon, 2, "")
				registerWebPolicyAccount(t, m, "fallback-second", web.PolicyExcludeArgon, 1, "")
				m.RegisterExecutor(&mediaFallbackTestExecutor{provider: web.Provider, run: func(ctx context.Context, a *Auth, req exec.Request, _ exec.Options) error {
					webCalls++
					status := http.StatusUnauthorized
					if a.ID == "fallback-second" {
						status = tc.secondStatus
					}
					if status == http.StatusOK {
						return nil
					}
					if tc.started {
						web.ExistingLease(ctx).MarkSessionStarted()
					}
					c, err := web.CredentialFrom(a.Metadata)
					if err != nil {
						return err
					}
					client := &http.Client{Transport: mediaFallbackLoginTransport(status)}
					selection := web.CookieSelection{ID: a.ID, RegistrationEpoch: a.RegistrationEpoch, CredentialVersion: a.CredentialVersion}
					_, release, err := m.WebRuntime().PrepareCookie(ctx, &c, client, selection, "test-route", req.Model, false)
					if release != nil {
						release()
					}
					return web.AuthenticationPreflightFailure(err)
				}})
				err := callMediaFallbackTest(t, m, stream, t.Context(), webPolicyRequest(mediaFallbackTestModel), webPolicyOptions())
				status := m.WebRuntime().Status()
				if (err != nil) != tc.wantErr || webCalls != tc.wantWeb || ordinaryCalls != tc.wantOrdinary || status["active_requests"] != 0 || status["inflight_bytes"] != int64(0) {
					t.Fatalf("error=%v web=%d ordinary=%d status=%v", err, webCalls, ordinaryCalls, status)
				}
			})
		}
	}
}

func TestEnterpriseWebMediaFallbackPreservesPinsAndRejectsRestoredMedia(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{})
	m.RegisterExecutor(&webPolicySelectionExecutor{})
	registerMediaFallbackEnterprise(t, m, func(context.Context, *Auth, exec.Request, exec.Options) error {
		t.Fatal("unusable request reached Enterprise")
		return nil
	})
	a := registerWebPolicyAccount(t, m, "fallback-pinned", web.PolicyExcludeArgon, 1, "")
	req := webPolicyRequest(mediaFallbackTestModel)
	pinned := webPolicyOptions()
	pinned.Metadata[exec.PinnedAuthMetadataKey] = a.ID
	if err := callMediaFallbackTest(t, m, false, t.Context(), req, pinned); !web.IsChannelDisabled(err) {
		t.Fatalf("pinned Web request changed route: %v", err)
	}
	opts := webPolicyOptions()
	opts.RequestAfterAuthInterceptor = func(context.Context, exec.RequestAfterAuthInterceptRequest) exec.RequestAfterAuthInterceptResponse {
		return exec.RequestAfterAuthInterceptResponse{Body: req.Payload}
	}
	err := callMediaFallbackTest(t, m, false, t.Context(), req, opts)
	var failure *web.Failure
	if !errors.As(err, &failure) || failure.Reason != "media_fallback_reintroduced_media" {
		t.Fatalf("restored attachment was not rejected: %v", err)
	}
}

func TestEnterpriseWebMediaFallbackMissingOrdinaryCredential(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetConfig(&config.Config{})
			err := callMediaFallbackTest(t, m, stream, t.Context(), webPolicyRequest(mediaFallbackTestModel), webPolicyOptions())
			var failure *web.Failure
			if !errors.As(err, &failure) || failure.Code != 503 || failure.Reason != "media_fallback_enterprise_unavailable" {
				t.Fatalf("no-credential fallback did not return a clear 503: %v", err)
			}
		})
	}
}
