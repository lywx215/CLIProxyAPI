package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"

	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestEnterpriseOperationRejectedBeforeCredentialSelection(t *testing.T) {
	m := NewManager(nil, nil, nil)
	opts := exec.Options{SourceFormat: translator.FormatOpenAI, Metadata: map[string]any{exec.OperationMetadataKey: "responses"}}
	for _, method := range []string{"full", "stream", "count"} {
		var err error
		switch method {
		case "full":
			_, err = m.Execute(context.Background(), []string{"gemini-enterprise"}, exec.Request{}, opts)
		case "stream":
			_, err = m.ExecuteStream(context.Background(), []string{"gemini-enterprise"}, exec.Request{}, opts)
		case "count":
			_, err = m.ExecuteCount(context.Background(), []string{"gemini-enterprise"}, exec.Request{}, opts)
		}
		var authErr *Error
		if !errors.As(err, &authErr) || authErr.HTTPStatus != 400 || authErr.Code != "operation_not_supported" {
			t.Fatalf("%s: %v", method, err)
		}
	}
	providers := []string{"codex", "gemini-enterprise", "gemini", "claude"}
	filtered, err := filterEnterpriseProviders(providers, opts, false, false)
	if err != nil || !reflect.DeepEqual(filtered, []string{"codex", "gemini", "claude"}) {
		t.Fatalf("other providers reordered: %v, %v", filtered, err)
	}
	opts.Metadata[exec.OperationMetadataKey] = "chat_completions"
	filtered, err = filterEnterpriseProviders(providers, opts, false, false)
	if err != nil || !reflect.DeepEqual(filtered, providers) {
		t.Fatal("native Chat candidates changed")
	}
	filtered, err = filterEnterpriseProviders(providers, opts, false, true)
	if err != nil || len(filtered) != 3 {
		t.Fatal("Home retained unsupported Enterprise provider")
	}
}
