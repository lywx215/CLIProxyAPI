package helps

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
)

func TestEnterpriseFailureRequestLogKeepsOnlySafeDiagnostics(t *testing.T) {
	err := enterprise.UpstreamHTTPError(400, []byte(`{"error":{"status":"INVALID_ARGUMENT","message":"private-prompt private-token","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"query.text","description":"required: private-query"}]}]}}`))
	generation := &ge.GenerationError{HTTPStatus: 400, Diagnostic: err}
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(t.Context(), "gin", ginCtx)
	cfg := &config.Config{}
	cfg.RequestLog = true
	RecordEnterpriseFailure(ctx, cfg, generation)
	raw, _ := ginCtx.Get(apiResponseKey)
	text, _ := raw.([]byte)
	if len(text) == 0 {
		if value, ok := raw.(string); ok {
			text = []byte(value)
		}
	}
	if !strings.Contains(string(text), "INVALID_ARGUMENT") || !strings.Contains(string(text), "query.text") || strings.Contains(string(text), "private-") || strings.Contains(generation.Error(), "private-") {
		t.Fatalf("request log lost diagnostics or leaked source data: %s", text)
	}
}

func TestEnterpriseValidationFields(t *testing.T) {
	_, err := enterprise.Normalize([]byte(`{"contents":[{"parts":[{"functionCall":{"name":"private-function"},"thoughtSignature":"private-signature"}]}]}`), "gemini", "gemini-3.1-pro-preview")
	if err == nil {
		t.Fatal("expected an input validation error")
	}
	fields := EnterpriseValidationFields("input", fmt.Errorf("wrapped private-value: %w", err))
	if len(fields) != 4 || fields["provider"] != "gemini-enterprise" || fields["stage"] != "input" || fields["field"] != "contents[0].parts[0].functionCall" || fields["code"] != "invalid_field" {
		t.Fatalf("incorrect structural diagnostics: %v", fields)
	}
	if fields := EnterpriseValidationFields("private-stage", err); fields["stage"] != "request" {
		t.Fatal("unrecognized stage was exposed")
	}
	if fields := EnterpriseValidationFields("final_payload", enterprise.ValidateFinal([]byte(`{"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-4-argon"},"generationConfig":{"private":"value"}}`))); fields["field"] != "generationConfig" || fields["stage"] != "final_payload" {
		t.Fatalf("missing final payload diagnostic: %v", fields)
	}
	for _, unsafe := range []error{nil, errors.New("private-error"), &enterprise.Error{Status: 400, Kind: "private-kind", Message: "private-message"}} {
		if fields := EnterpriseValidationFields("input", unsafe); fields != nil {
			t.Fatalf("untrusted error was logged: %v", fields)
		}
	}
}
