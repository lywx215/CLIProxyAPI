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
	ctx := WithInputLimits(context.Background(), appconfig.GeminiEnterpriseWebInputLimits{MaxContents: 1, MaxSegments: 1})
	const attachment = `,{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}`
	for _, tc := range []struct {
		name                  string
		contents, attachments int
		field                 string
		limit                 contract.InputLimit
	}{
		{name: "past old logical limit", contents: 201, attachments: 12},
		{name: "1000 messages", contents: 1000, attachments: 12},
		{name: "past old segment limit", contents: 5000, attachments: 12},
		{name: "past old configuration maximum", contents: 10001, attachments: 12},
		{
			name: "combined attachment limit", contents: 201, attachments: 13, field: "contents[200].parts[12].inlineData",
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
			contents[len(contents)-2] = `{"role":"model","parts":[{"text":"prior output"}` + attachment + `]}`
			contents[len(contents)-1] = `{"role":"user","parts":[{"text":"read the files"}` +
				strings.Repeat(attachment, tc.attachments-1) + `]}`
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

func TestSerializedWebCountsPartsPerContent(t *testing.T) {
	ctx := WithInputLimits(t.Context(), appconfig.GeminiEnterpriseWebInputLimits{MaxUserParts: 1, MaxModelParts: 1})
	user := `{"role":"user","parts":[{"text":"same role"}]},`
	model := `{"role":"model","parts":[{"text":"same role"}]},`
	body := `{"contents":[` + strings.Repeat(user, 300) + strings.Repeat(model, 4300) + `{"role":"user","parts":[{"text":"last"}]}]}`
	request, err := ParseSerializedBytes(ctx, []byte(body), config.Binding{})
	if err != nil || request.ContextSummary.Contents != 4601 || request.ContextSummary.LogicalContents != 3 {
		t.Fatalf("message boundaries were lost or limited: %+v (%v)", request.ContextSummary, err)
	}
}
