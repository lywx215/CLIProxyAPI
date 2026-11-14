package auth

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestEnterpriseP0OperationErrorIsChannelNeutralAndDoesNotAffectOtherProviders(t *testing.T) {
	opts := exec.Options{SourceFormat: translator.FormatOpenAI, Metadata: map[string]any{exec.OperationMetadataKey: "responses"}}
	_, err := filterEnterpriseProviders([]string{"gemini-enterprise"}, opts, false, false)
	var detail *Error
	if !errors.As(err, &detail) || detail.Code != "operation_not_supported" || detail.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("operation rejection changed: %v", err)
	}
	for _, forbidden := range []string{"gemini", "enterprise", "discoveryengine", "streamassist"} {
		if strings.Contains(strings.ToLower(err.Error()), forbidden) {
			t.Fatalf("operation error disclosed channel: %v", err)
		}
	}
	for _, candidates := range [][]string{{"gemini", "claude", "codex", "antigravity"}, {"gemini", "gemini-enterprise", "claude", "codex", "antigravity"}} {
		original := append([]string(nil), candidates...)
		actual, err := filterEnterpriseProviders(candidates, opts, false, false)
		if err != nil || !reflect.DeepEqual(actual, []string{"gemini", "claude", "codex", "antigravity"}) || !reflect.DeepEqual(candidates, original) {
			t.Fatalf("unrelated providers or their input order changed: %v, %v", actual, err)
		}
	}
}
