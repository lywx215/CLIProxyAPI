package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	"github.com/tidwall/gjson"
)

func TestEnterpriseP0ArrayDiagnosticsSurviveGenerationAndHandlerWrapping(t *testing.T) {
	for _, test := range []struct {
		name, raw, code, param string
		status                 int
	}{
		{"field", `[{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"private-token","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"generation_spec.temperature","description":"private-prompt"}]}]}}]`, "upstream_invalid_request", "generation_spec.temperature", http.StatusBadRequest},
		{"subscription", `[{"error":{"code":400,"status":"FAILED_PRECONDITION","message":"The model private-model is not available during the free trial. Upgrade to a paid subscription or select a different model."}}]`, "model_subscription_required", "model", http.StatusBadRequest},
		{"service", `[{"error":{"code":403,"status":"PERMISSION_DENIED","message":"private-service","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"SERVICE_DISABLED"}]}}]`, "upstream_service_disabled", "", http.StatusForbidden},
		{"method", `[{"error":{"code":404,"status":"NOT_FOUND","message":"Method not found. private-resource"}}]`, "upstream_method_unavailable", "", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &ge.GenerationError{HTTPStatus: test.status, Diagnostic: enterprise.UpstreamHTTPError(test.status, []byte(test.raw))}
			wrapped := fmt.Errorf("private wrapper: %w", err)
			body := BuildErrorResponseBodyWithError(test.status, wrapped.Error(), wrapped)
			if !gjson.ValidBytes(body) || gjson.GetBytes(body, "error.code").String() != test.code || gjson.GetBytes(body, "error.param").String() != test.param {
				t.Fatalf("typed upstream diagnostic was lost: %s", body)
			}
			if strings.Contains(string(body), "private-") || strings.Contains(string(body), "private wrapper") {
				t.Fatalf("upstream values escaped the trusted diagnostic: %s", body)
			}
			assertEnterprisePublicErrorChannelNeutral(t, body)
		})
	}
}
