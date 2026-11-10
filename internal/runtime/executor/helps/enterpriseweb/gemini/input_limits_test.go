package gemini

import (
	"context"
	"errors"
	"strings"
	"testing"

	appconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

func TestConfiguredWebInputLimitsIdentifyTheActualThreshold(t *testing.T) {
	const file = `{"inlineData":{"mimeType":"text/plain","data":"YWJj"}}`
	const user = `{"role":"user","parts":[{"text":"x"}]}`
	base := `{"contents":[{"parts":[{"text":"abcde"},` + file + `]}]}`
	for _, tc := range []struct {
		name    string
		limits  appconfig.GeminiEnterpriseWebInputLimits
		body    string
		limit   contract.InputLimitName
		maximum int64
	}{
		{"text", appconfig.GeminiEnterpriseWebInputLimits{MaxTextBytes: 4}, base, contract.LimitHistoryTextBytes, 4},
		{"user-parts", appconfig.GeminiEnterpriseWebInputLimits{MaxUserParts: 1}, base, contract.LimitUserParts, 1},
		{"model-parts", appconfig.GeminiEnterpriseWebInputLimits{MaxModelParts: 1}, `{"contents":[{"role":"model","parts":[{"text":"a"},{"text":"b"}]},` + user + `]}`, contract.LimitModelParts, 1},
		{"system-parts", appconfig.GeminiEnterpriseWebInputLimits{MaxSystemParts: 1}, `{"systemInstruction":{"parts":[{"text":"a"},{"text":"b"}]},"contents":[` + user + `]}`, contract.LimitSystemParts, 1},
		{"attachments", appconfig.GeminiEnterpriseWebInputLimits{MaxAttachments: 1}, `{"contents":[{"parts":[` + file + `,` + file + `]}]}`, contract.LimitAttachments, 1},
		{"attachment-bytes", appconfig.GeminiEnterpriseWebInputLimits{MaxAttachmentBytes: 1}, base, contract.LimitAttachmentBytes, 1},
		{"request-bytes", appconfig.GeminiEnterpriseWebInputLimits{MaxRequestBytes: int64(len(base) - 1)}, base, contract.LimitRequestBytes, int64(len(base) - 1)},
		{"object-fields", appconfig.GeminiEnterpriseWebInputLimits{MaxJSONObjectFields: 2}, `{"a":0,"b":0,"contents":[` + user + `]}`, contract.LimitJSONObjectFields, 2},
		{"tool-depth", appconfig.GeminiEnterpriseWebInputLimits{MaxToolDepth: 1}, `{"contents":[{"parts":[{"functionResponse":{"response":{"a":{"b":1}}}}]}]}`, contract.LimitToolDepth, 1},
		{"tool-nodes", appconfig.GeminiEnterpriseWebInputLimits{MaxToolNodes: 2}, `{"contents":[{"parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`, contract.LimitToolNodes, 2},
		{"tool-array", appconfig.GeminiEnterpriseWebInputLimits{MaxToolArrayEntries: 1}, `{"contents":[{"parts":[{"functionResponse":{"response":{"a":[1,2]}}}]}]}`, contract.LimitToolArrayEntries, 1},
		{"generation", appconfig.GeminiEnterpriseWebInputLimits{MaxGenerationConfigBytes: 2}, `{"generationConfig":{"temperature":1},"contents":[` + user + `]}`, contract.LimitGenerationConfigBytes, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithInputLimits(context.Background(), tc.limits)
			_, err := ParseSerializedBytes(ctx, []byte(tc.body), config.Binding{})
			var failure *contract.Failure
			if !errors.As(err, &failure) || failure.Code != 413 || failure.Limit == nil || failure.Limit.Name != tc.limit || failure.Limit.Maximum != tc.maximum || !failure.Limit.Valid() {
				t.Fatalf("configured limit or measurement was lost: %#v (%v)", failure, err)
			}
		})
	}
}

func TestWebTextConfigurationDoesNotChangeOtherParserCallers(t *testing.T) {
	body := []byte(`{"contents":[{"parts":[{"text":"` + strings.Repeat("x", (1<<20)+1) + `"}]}]}`)
	ctx := WithInputLimits(context.Background(), appconfig.GeminiEnterpriseWebInputLimits{})
	if _, err := ParseSerializedBytes(ctx, body, config.Binding{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSerializedBytes(context.Background(), body, config.Binding{}); err == nil {
		t.Fatal("standalone Business parser limits changed")
	}
}
