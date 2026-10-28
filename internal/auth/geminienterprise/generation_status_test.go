package geminienterprise

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestEnterpriseGenerationCanonicalErrors(t *testing.T) {
	for _, tt := range []struct {
		rpc, http int
		name      string
	}{
		{1, 499, "CANCELLED"}, {2, 500, "UNKNOWN"}, {3, 400, "INVALID_ARGUMENT"},
		{4, 504, "DEADLINE_EXCEEDED"}, {5, 404, "NOT_FOUND"}, {6, 409, "ALREADY_EXISTS"},
		{7, 403, "PERMISSION_DENIED"}, {8, 429, "RESOURCE_EXHAUSTED"}, {9, 400, "FAILED_PRECONDITION"},
		{10, 409, "ABORTED"}, {11, 400, "OUT_OF_RANGE"}, {12, 501, "UNIMPLEMENTED"},
		{13, 500, "INTERNAL"}, {14, 503, "UNAVAILABLE"}, {15, 500, "DATA_LOSS"}, {16, 401, "UNAUTHENTICATED"},
	} {
		for _, code := range []int{tt.rpc, tt.http, 0} {
			t.Run(fmt.Sprintf("%s/%d", tt.name, code), func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"error":{"code":%d,"status":%q,"message":"private-prompt private-token"}}`, code, tt.name))
				e := ParseGenerationError(raw, 200, nil, config.GeminiEnterpriseOAuth{}, nil, time.Now(), false)
				if e.StatusCode() != tt.http || e.UpstreamHTTPStatus != 200 || e.RPCStatus != tt.name || strings.Contains(e.Error(), "private-") {
					t.Fatalf("lost or unsafe classification: %#v", e)
				}
				if e.Restriction != nil {
					t.Fatal("ordinary RPC error created shared quota state")
				}
			})
		}
	}
}

func TestEnterpriseGenerationUnknownErrorsAreNotReplayed(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"code":3,"status":"UNAVAILABLE"}}`,
		`{"error":{"code":14,"status":"private-status"}}`,
		`{"error":{"message":"UNAVAILABLE private-token"}}`,
		`{"error":{"code":999}}`, `{"error":`,
	} {
		e := ParseGenerationError([]byte(raw), 200, nil, config.GeminiEnterpriseOAuth{}, nil, time.Now(), false)
		if e.HTTPStatus != 502 || !e.IsRequestStop() || e.RPCStatus != "" || e.Restriction != nil {
			t.Fatalf("ambiguous error became retryable or trusted: %#v", e)
		}
	}
}

func TestEnterpriseGenerationPreservesHTTPAndRetryHint(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	raw := []byte(`{"error":{"code":14,"status":"UNAVAILABLE"}}`)
	for _, status := range []int{200, 502, 503} {
		e := ParseGenerationError(raw, status, http.Header{"Retry-After": {"7"}}, config.GeminiEnterpriseOAuth{}, nil, now, false)
		want := status
		if status == 200 {
			want = 503
		}
		if e.HTTPStatus != want || e.UpstreamHTTPStatus != status || e.RPCStatus != "UNAVAILABLE" || e.IsRequestStop() {
			t.Fatalf("HTTP and stream statuses were conflated: %#v", e)
		}
		if e.RetryAfter() == nil || *e.RetryAfter() != 7*time.Second {
			t.Fatal("upstream retry hint was lost")
		}
	}
	e := ParseGenerationError(raw, 200, nil, config.GeminiEnterpriseOAuth{}, nil, now, false)
	e.Model = "auto"
	if !e.IsRequestStop() {
		t.Fatal("unresolved Auto was automatically replayable")
	}
}
