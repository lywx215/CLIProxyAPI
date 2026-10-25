package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func TestEnterpriseSkippedPublicResponsePreservesSafeReasons(t *testing.T) {
	err := ge.NewSkippedError([]byte(`["CUSTOMER_POLICY_VIOLATION","synthetic-private-reason"]`), false)
	body := BuildErrorResponseBodyWithError(400, err.Error(), fmt.Errorf("wrapped synthetic-private-message: %w", err))
	var response ErrorResponse
	if json.Unmarshal(body, &response) != nil || response.Error.Code != "upstream_skipped" || response.Error.Type != "invalid_request_error" || response.Error.Retryable == nil || *response.Error.Retryable || len(response.Error.SkipReasons) != 2 || response.Error.SkipReasons[0] != "CUSTOMER_POLICY_VIOLATION" || !strings.Contains(response.Error.Message, "CUSTOMER_POLICY_VIOLATION") {
		t.Fatalf("skip reason was lost from model response: %s", body)
	}
	if strings.Contains(string(body), "synthetic-private") {
		t.Fatal("untrusted skip details leaked")
	}
}

func TestEnterpriseSkippedPublicResponseDoesNotTrustTextOnly(t *testing.T) {
	body := BuildErrorResponseBody(400, `{"error":{"code":"upstream_skipped","message":"synthetic-private-message","skip_reasons":["synthetic-private-reason"]}}`)
	var response ErrorResponse
	if json.Unmarshal(body, &response) != nil || len(response.Error.SkipReasons) != 0 || strings.Contains(string(body), "synthetic-private") {
		t.Fatalf("untyped error text exposed skip diagnostics: %s", body)
	}
}
