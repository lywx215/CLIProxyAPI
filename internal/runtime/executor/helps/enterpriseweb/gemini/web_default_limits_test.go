package gemini

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

func TestSerializedWebDefaultContentsAndAttachments(t *testing.T) {
	ctx := WithInputLimits(context.Background(), appconfig.GeminiEnterpriseWebInputLimits{})
	const attachment = `,{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}`
	for _, tc := range []struct {
		name                  string
		contents, attachments int
		field                 string
		limit                 contract.InputLimit
	}{
		{name: "at both limits", contents: 200, attachments: 12},
		{
			name: "one extra logical content", contents: 201, attachments: 12, field: "contents",
			limit: contract.InputLimit{Name: contract.LimitHistoryContents, Observed: 201, Maximum: 200, AtLeast: true},
		},
		{
			name: "one extra attachment", contents: 200, attachments: 13, field: "contents[199].parts[13].inlineData",
			limit: contract.InputLimit{Name: contract.LimitAttachments, Observed: 13, Maximum: 12, AtLeast: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contents := make([]string, tc.contents)
			for i := range contents {
				role := "model"
				if (len(contents)-i)%2 != 0 {
					role = "user"
				}
				contents[i] = fmt.Sprintf(`{"role":%q,"parts":[{"text":"turn"}]}`, role)
			}
			contents[len(contents)-1] = `{"role":"user","parts":[{"text":"read the files"}` +
				strings.Repeat(attachment, tc.attachments) + `]}`
			body := `{"contents":[` + strings.Join(contents, ",") + `]}`
			request, err := ParseSerializedBytes(ctx, []byte(body), config.Binding{})
			if tc.field == "" {
				if err != nil || request.ContextSummary.LogicalContents != tc.contents || len(request.Attachments) != tc.attachments {
					t.Fatalf("request at default limits did not parse: summary=%+v, err=%v", request.ContextSummary, err)
				}
				return
			}
			var failure *contract.Failure
			if !errors.As(err, &failure) || failure.Code != 413 || failure.Param != tc.field || failure.Limit == nil || *failure.Limit != tc.limit {
				t.Fatalf("unexpected limit rejection: %+v", failure)
			}
		})
	}
}
