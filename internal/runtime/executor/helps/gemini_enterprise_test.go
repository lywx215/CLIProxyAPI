package helps

import (
	"errors"
	"fmt"
	"testing"

	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
)

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
