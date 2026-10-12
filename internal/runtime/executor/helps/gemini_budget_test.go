package helps

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type geminiBudgetTestTransport func(*http.Request) (*http.Response, error)

func (f geminiBudgetTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func geminiBudgetTestSetup(t *testing.T, limit int) (*budget.Engine, *config.Config, *cliproxyauth.Auth) {
	t.Helper()
	ledgerPath := t.TempDir()
	engine, err := budget.Open(context.Background(), budget.Options{Path: ledgerPath, Now: func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }, Models: map[string]budget.ModelPolicy{
		"gemini-2.5-flash": {DailyLimit: limit}, "gemini-3.1-flash-lite-preview": {DailyLimit: 500},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if err = engine.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{GeminiFreeTierPolicy: config.GeminiFreeTierPolicyConfig{
		Enabled: true, StoragePath: ledgerPath, ManagedModels: map[string]int{"gemini-2.5-flash": limit, "gemini-3.1-flash-lite-preview": 500},
	}}
	auth := &cliproxyauth.Auth{Provider: "gemini", Index: "test-key", Attributes: map[string]string{"api_key": "test-key-value"}}
	return engine, cfg, auth
}

func geminiBudgetTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func geminiBudgetTestRequest(ctx context.Context, model string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/"+model+":generateContent", strings.NewReader(`{"contents":[]}`))
	return req
}

func TestGeminiBudgetConcurrentFinalRequestSendsOnce(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 1)
	var sends atomic.Int32
	base := &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return geminiBudgetTestResponse(req, 200, `{}`), nil
	})}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx := budget.WithEngine(context.Background(), engine)
			client := GeminiBudgetHTTPClient(ctx, base, cfg, auth, "public-flash")
			resp, err := client.Do(geminiBudgetTestRequest(ctx, "gemini-2.5-flash"))
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	group.Wait()
	if sends.Load() != 1 {
		t.Fatalf("physical sends = %d, want 1", sends.Load())
	}
}

func TestGeminiBudgetTransmissionFailureNeverCompensates(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithEngine(context.Background(), engine)
	var sends atomic.Int32
	client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: geminiBudgetTestTransport(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return nil, errors.New("connection reset after request headers")
	})}, cfg, auth, "public-flash")
	resp, err := client.Do(geminiBudgetTestRequest(ctx, "gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 503 || sends.Load() != 1 || bytes.Contains(data, []byte("gemini-2.5-flash")) {
		t.Fatalf("unsafe failure: %d %s", resp.StatusCode, data)
	}
	snapshot := engine.Snapshot()
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Used != 1 {
		t.Fatalf("transmitted budget refunded: %+v", snapshot.Entries)
	}
	// Check the physical WAL, not just the resulting counter.
	wal, err := os.ReadFile(filepath.Join(cfg.GeminiFreeTierPolicy.StoragePath, "ledger.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wal, []byte(`"operation":"compensate"`)) {
		t.Fatal("transmission failure appended a compensate event")
	}
}

func TestGeminiBudgetThinkingFormsShareQuotaAndLiteRemainsAvailable(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 2)
	base := &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) { return geminiBudgetTestResponse(req, 200, `{}`), nil })}
	for index, model := range []string{"gemini-2.5-flash:low", "gemini-2.5-flash:thinking", "gemini-2.5-flash", "gemini-3.1-flash-lite-preview"} {
		ctx := budget.WithEngine(context.Background(), engine)
		client := GeminiBudgetHTTPClient(ctx, base, cfg, auth, "public-alias")
		resp, err := client.Do(geminiBudgetTestRequest(ctx, model))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		want := 200
		if index == 2 {
			want = 429
		}
		if resp.StatusCode != want {
			t.Fatalf("model %s status = %d, want %d", model, resp.StatusCode, want)
		}
	}
	for _, entry := range engine.Snapshot().Entries {
		if entry.Model == "gemini-2.5-flash" && entry.Used != 2 {
			t.Fatalf("thinking allocated separate quota: %+v", entry)
		}
	}
}

func TestGeminiBudgetThreeActualSendsAcrossRequests(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithRequest(budget.WithEngine(context.Background(), engine))
	var sends atomic.Int32
	base := &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return geminiBudgetTestResponse(req, 200, `{}`), nil
	})}
	for index := range 10 {
		key := *auth
		key.Attributes = map[string]string{"api_key": fmt.Sprintf("test-key-%d", index)}
		client := GeminiBudgetHTTPClient(ctx, base, cfg, &key, "public-flash")
		resp, err := client.Do(geminiBudgetTestRequest(ctx, "gemini-2.5-flash"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if sends.Load() != 3 {
		t.Fatalf("physical sends = %d, want <= 3", sends.Load())
	}
}

func TestGeminiBudgetGoogle429IsSanitizedAndLocksOnlyActualModel(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithEngine(context.Background(), engine)
	raw := `{"error":{"code":429,"message":"projects/987654 models/gemini-2.5-flash exceeded","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier","quotaValue":"20"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"77s"}]}}`
	client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) { return geminiBudgetTestResponse(req, 429, raw), nil })}, cfg, auth, "public-flash")
	resp, err := client.Do(geminiBudgetTestRequest(ctx, "gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	delay, errDelay := strconv.Atoi(resp.Header.Get("Retry-After"))
	if bytes.Contains(data, []byte("projects/")) || bytes.Contains(data, []byte("gemini-2.5-flash")) || !bytes.Contains(data, []byte("public-flash")) || errDelay != nil || delay < 77 {
		t.Fatalf("error leaked details or missed RetryInfo: %s headers=%v", data, resp.Header)
	}
	if engine.Available(cliproxyauth.GeminiBudgetScope(auth, "gemini-2.5-flash")) || !engine.Available(cliproxyauth.GeminiBudgetScope(auth, "gemini-3.1-flash-lite-preview")) {
		t.Fatal("daily quota lock has incorrect scope")
	}
}

func TestGeminiBudgetStreamingEmbeddedErrorIsSanitized(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithEngine(context.Background(), engine)
	client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) {
		resp := geminiBudgetTestResponse(req, 200, "data: {\"candidates\":[]}\n\ndata: {\"error\":{\"code\":429,\"message\":\"projects/987654 gemini-2.5-flash\"}}\n\n")
		resp.Header.Set("Content-Type", "text/event-stream")
		return resp, nil
	})}, cfg, auth, "public-flash")
	resp, err := client.Do(geminiBudgetTestRequest(ctx, "gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if bytes.Contains(data, []byte("projects/")) || bytes.Contains(data, []byte("gemini-2.5-flash")) || !bytes.Contains(data, []byte("public-flash")) || !bytes.Contains(data, []byte("candidates")) {
		t.Fatalf("stream error leaked or valid chunk lost: %s", data)
	}
}

func TestGeminiBudgetPhysicalDisconnectAfterHeadersNeverCompensates(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	var sends atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Local test server only.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	ctx := budget.WithEngine(context.Background(), engine)
	client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: transport}, cfg, auth, "public-flash")
	resp, err := client.Do(geminiBudgetTestRequest(ctx, "gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if sends.Load() != 1 || engine.Snapshot().Entries[0].Used != 1 {
		t.Fatalf("physical failure lost charged reservation: sends=%d entries=%+v", sends.Load(), engine.Snapshot().Entries)
	}
	wal, err := os.ReadFile(filepath.Join(cfg.GeminiFreeTierPolicy.StoragePath, "ledger.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wal, []byte(`"operation":"compensate"`)) {
		t.Fatal("socket closure after headers appended compensation")
	}
}

func TestGeminiBudgetStructuredQuotaKindsAndErrorInfoMetadata(t *testing.T) {
	for _, tc := range []struct {
		quota string
		kind  budget.SignalKind
	}{
		{"GenerateRequestsPerDayPerProjectPerModel-FreeTier", budget.SignalRPD},
		{"GenerateRequestsPerMinutePerProjectPerModel-FreeTier", budget.SignalRPM},
		{"GenerateContentInputTokensPerModelPerMinute-FreeTier", budget.SignalTPM},
		{"UnknownQuota", budget.SignalUnknown429},
	} {
		body := []byte(fmt.Sprintf(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"quota_limit":%q}}]}}`, tc.quota))
		if signal := geminiBudgetSignal(429, nil, body); signal.Kind != tc.kind {
			t.Errorf("%s kind=%s want=%s", tc.quota, signal.Kind, tc.kind)
		}
	}
}

func TestGeminiBudgetAuthorizationAndModelSignalsRequireStructuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   budget.SignalKind
	}{
		{403, `{"error":{"message":"Permission denied for projects/private"}}`, budget.SignalTemporaryAuth},
		{401, `{"error":{"code":401}}`, budget.SignalTemporaryAuth},
		{403, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID"}]}}`, budget.SignalCredentialInvalid},
		{404, `{"error":{"status":"NOT_FOUND"}}`, budget.SignalModelUnsupported},
		{404, `{"error":{"message":"Unknown route"}}`, ""},
	} {
		if got := geminiBudgetSignal(tc.status, nil, []byte(tc.body)); got.Kind != tc.kind {
			t.Errorf("status=%d body=%s kind=%s want=%s", tc.status, tc.body, got.Kind, tc.kind)
		}
	}
}

func TestGeminiBudgetMultilineSSEErrorHidesWholeFrame(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithEngine(context.Background(), engine)
	raw := "data: {\"error\":{\n" + "data: \"code\":429,\"message\":\"projects/987654 gemini-2.5-flash\"}}\n\n"
	client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) {
		r := geminiBudgetTestResponse(req, 200, raw)
		r.Header.Set("Content-Type", "text/event-stream")
		return r, nil
	})}, cfg, auth, "public-flash")
	response, err := client.Do(geminiBudgetTestRequest(ctx, "gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("projects/")) || bytes.Contains(body, []byte("gemini-2.5-flash")) || !bytes.Contains(body, []byte("public-flash")) {
		t.Fatalf("multiline error leaked: %s", body)
	}
}

func TestGeminiBudgetUnlistedGoogleModelStillSanitizesError(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithRequest(budget.WithEngine(context.Background(), engine))
	client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) {
		return geminiBudgetTestResponse(req, 403, `{"error":{"message":"projects/987654 unknown-private-model"}}`), nil
	})}, cfg, auth, "public-other")
	response, err := client.Do(geminiBudgetTestRequest(ctx, "unknown-private-model"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if bytes.Contains(body, []byte("projects/")) || bytes.Contains(body, []byte("unknown-private-model")) || !bytes.Contains(body, []byte("public-other")) {
		t.Fatalf("unlisted error leaked: %s", body)
	}
	if budget.Attempts(ctx) != 1 || len(engine.Snapshot().Entries) != 0 {
		t.Fatal("unlisted model must consume only the channel send count")
	}
}

func TestGeminiBudgetUnlistedModelSharesThreeSendLimit(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithRequest(budget.WithEngine(t.Context(), engine))
	var sends atomic.Int32
	for i := range 8 {
		a := auth.Clone()
		a.Attributes["api_key"] = fmt.Sprintf("unlisted-synthetic-%d", i)
		client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) {
			sends.Add(1)
			return geminiBudgetTestResponse(req, 503, `{"error":{"message":"projects/private unlisted-model"}}`), nil
		})}, cfg, a, "public-other")
		response, err := client.Do(geminiBudgetTestRequest(ctx, "unlisted-model"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if sends.Load() != 3 || budget.Attempts(ctx) != 3 || len(engine.Snapshot().Entries) != 0 {
		t.Fatalf("unlisted model bypassed limit or spent RPD: sends=%d attempts=%d entries=%d", sends.Load(), budget.Attempts(ctx), len(engine.Snapshot().Entries))
	}
}

func TestGeminiBudgetCountTokensSanitizesWithoutGenerationCharge(t *testing.T) {
	engine, cfg, auth := geminiBudgetTestSetup(t, 20)
	ctx := budget.WithRequest(budget.WithEngine(t.Context(), engine))
	client := GeminiBudgetHTTPClient(ctx, &http.Client{Transport: geminiBudgetTestTransport(func(req *http.Request) (*http.Response, error) {
		return geminiBudgetTestResponse(req, 429, `{"error":{"message":"projects/private gemini-2.5-flash"}}`), nil
	})}, cfg, auth, "public-count")
	req := geminiBudgetTestRequest(ctx, "gemini-2.5-flash")
	req.URL.Path = strings.Replace(req.URL.Path, ":generateContent", ":countTokens", 1)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if bytes.Contains(body, []byte("projects/")) || bytes.Contains(body, []byte("gemini-2.5-flash")) || !bytes.Contains(body, []byte("public-count")) {
		t.Fatalf("auxiliary error leaked: %s", body)
	}
	if budget.Attempts(ctx) != 0 || len(engine.Snapshot().Entries) != 0 {
		t.Fatal("countTokens spent generation quota")
	}
}
