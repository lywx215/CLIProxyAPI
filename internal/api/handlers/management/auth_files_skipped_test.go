package management

import (
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func TestEnterpriseModelProbePreservesSkippedReason(t *testing.T) {
	err := ge.NewSkippedError([]byte(`["CUSTOMER_POLICY_VIOLATION","synthetic-private"]`), false)
	result := enterpriseModelTestError(enterpriseModelTestResult{}, err)
	if result.Status != "failed" || result.HTTPStatus != 400 || result.Code != "upstream_skipped" || !strings.Contains(result.Message, "CUSTOMER_POLICY_VIOLATION") || strings.Contains(result.Message, "synthetic-private") {
		t.Fatalf("model probe lost safe skip reason: %+v", result)
	}
}
