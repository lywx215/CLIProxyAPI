package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

func TestSerializedHistoryCompatibleBoundariesPreserveRolesAndPrefixes(t *testing.T) {
	for _, test := range []struct {
		name, prefix, suffix string
		continuation         bool
	}{
		{"leading-model", `{"role":"model","parts":[{"text":"PRIOR-ANSWER"}]},`, "", false},
		{"trailing-model", "", `,{"role":"model","parts":[{"text":"RESPONSE-PREFIX"}]},{"role":"model","parts":[{"text":"MORE-PREFIX"}]}`, true},
		{"empty-model", "", `,{"role":"model","parts":[{"text":""}]}`, false},
		{"thought-only-model", "", `,{"role":"model","parts":[{"text":"PRIVATE-THOUGHT","thought":true},{"thoughtSignature":"PRIVATE-SIGNATURE"}]}`, false},
		{"both-boundaries", `{"role":"model","parts":[{"text":"PRIOR-ANSWER"}]},`, `,{"role":"model","parts":[{"text":"RESPONSE-PREFIX"}]}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"contents":[` + test.prefix + `{"parts":[{"text":"LATEST-USER"},{"inlineData":{"mimeType":"text/plain","data":"RklMRQ=="}}]}` + test.suffix + `]}`)
			original := bytes.Clone(body)
			req, err := ParseSerializedBytes(context.Background(), body, config.Binding{})
			if err != nil {
				t.Fatal(err)
			}
			query := req.QueryParts[0]["text"]
			instruction := contextInstruction
			if test.continuation {
				instruction = contextContinuationInstruction
			}
			if !strings.HasPrefix(query, instruction) {
				t.Fatal("wrong response mode")
			}
			var doc contextDocument
			if err := json.Unmarshal([]byte(strings.TrimPrefix(query, instruction)), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Current.Role != "user" || *doc.Current.Parts[0].Text != "LATEST-USER" || string(req.Attachments[0].Data) != "FILE" {
				t.Fatal("current input or historical attachment changed")
			}
			if test.prefix != "" && (len(doc.History) != 1 || doc.History[0].Role != "model" || *doc.History[0].Parts[0].Text != "PRIOR-ANSWER") {
				t.Fatal("leading model context was lost or relabelled")
			}
			if test.continuation && (len(doc.ModelContinuation) == 0 || doc.ModelContinuation[0].Role != "model" || *doc.ModelContinuation[0].Parts[0].Text != "RESPONSE-PREFIX") {
				t.Fatal("model prefix was lost or relabelled")
			}
			if strings.Contains(test.suffix, "MORE-PREFIX") && *doc.ModelContinuation[1].Parts[0].Text != "MORE-PREFIX" {
				t.Fatal("streamed prefix fragments reordered")
			}
			if !bytes.Equal(body, original) || strings.Contains(query, "PRIVATE-") || strings.Contains(query, "RklMRQ==") {
				t.Fatal("input changed or private replay content retained")
			}
		})
	}
}

func TestSerializedHistoryDefaultsUserRolesAndRejectsUnknownRoles(t *testing.T) {
	body := []byte(`{"contents":[{"parts":[{"text":"FIRST"}]},{"role":"model","parts":[{"text":"ANSWER"}]},{"parts":[{"inlineData":{"mimeType":"text/plain","data":"YQ=="}},{"text":"LAST"}]}]}`)
	req, err := ParseSerializedBytes(context.Background(), body, config.Binding{})
	if err != nil || req.ContextSummary.LogicalContents != 3 || !strings.Contains(req.QueryParts[0]["text"], "FIRST") {
		t.Fatal("optional user role rejected", err)
	}
	for _, contents := range []string{
		`{"role":"model","parts":[{"text":"only-model"}]}`,
		`{"role":"assistant","parts":[{"text":"unknown-role"}]}`,
	} {
		_, err := ParseSerializedBytes(context.Background(), []byte(`{"contents":[`+contents+`]}`), config.Binding{})
		var failure *contract.Failure
		if !errors.As(err, &failure) || failure.Code != 400 || failure.Reason != "invalid_history" {
			t.Fatal("invalid history accepted", err)
		}
	}
}

func TestSerializedHistory413IdentifiesLimitAndMeasurement(t *testing.T) {
	parts := func(part string, count int) string { return strings.TrimSuffix(strings.Repeat(part+",", count), ",") }
	content := func(role, text string) string { return fmt.Sprintf(`{"role":%q,"parts":[{"text":%q}]}`, role, text) }
	turns := make([]string, 35)
	objectFields := make([]string, 257)
	for i := range objectFields {
		objectFields[i] = fmt.Sprintf(`"field%d":0`, i)
	}
	for i := range turns {
		role := "user"
		if i%2 != 0 {
			role = "model"
		}
		turns[i] = content(role, "x")
	}
	for _, test := range []struct {
		name, body, field string
		limit             contract.InputLimitName
		observed, maximum int64
	}{
		{"text", `{"contents":[{"parts":[{"text":"` + strings.Repeat("x", maxTextBytes+1) + `"}]}]}`, "contents[0].parts[0].text", contract.LimitHistoryTextBytes, maxTextBytes + 1, maxTextBytes},
		{"utf8-text", `{"contents":[{"parts":[{"text":"` + strings.Repeat("界", maxTextBytes/3+1) + `"}]}]}`, "contents[0].parts[0].text", contract.LimitHistoryTextBytes, (maxTextBytes/3 + 1) * 3, maxTextBytes},
		{"turns", `{"contents":[` + strings.Join(turns, ",") + `]}`, "contents", contract.LimitHistoryContents, 34, 33},
		{"fragments", `{"contents":[` + parts(content("user", "x"), maxHistorySegments+1) + `]}`, "contents", contract.LimitHistorySegments, maxHistorySegments + 1, maxHistorySegments},
		{"user-parts", `{"contents":[{"parts":[` + parts(`{"text":"x"}`, 257) + `]}]}`, "contents[0].parts", contract.LimitUserParts, 257, 256},
		{"fragmented-user-parts", `{"contents":[{"parts":[` + parts(`{"text":"x"}`, 100) + `]},{"parts":[` + parts(`{"text":"x"}`, 200) + `]}]}`, "contents[1].parts", contract.LimitUserParts, 300, 256},
		{"model-parts", `{"contents":[` + content("user", "x") + `,{"role":"model","parts":[` + parts(`{"text":"x"}`, 4097) + `]}]}`, "contents[1].parts", contract.LimitModelParts, 4097, 4096},
		{"attachments", `{"contents":[{"parts":[` + parts(`{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}`, 33) + `]}]}`, "contents[0].parts[32].inlineData", contract.LimitAttachments, 33, 32},
		{"system-parts", `{"systemInstruction":{"parts":[` + parts(`{"text":"x"}`, 257) + `]},"contents":[` + content("user", "x") + `]}`, "systemInstruction.parts", contract.LimitSystemParts, 257, 256},
		{"tool-array", `{"contents":[` + content("user", "x") + `,{"role":"model","parts":[{"functionCall":{"args":{"x":[` + parts(`0`, 4097) + `]}}}]}]}`, "contents[1].parts[0].functionCall", contract.LimitToolArrayEntries, 4097, 4096},
		{"tool-nodes", `{"contents":[` + content("user", "x") + `,{"role":"model","parts":[{"functionCall":{"args":{"x":[` + parts("["+parts("0", 1000)+"]", 200) + `]}}}]}]}`, "contents[1].parts[0].functionCall", contract.LimitToolNodes, 100001, 100000},
		{"tool-depth", `{"contents":[` + content("user", "x") + `,{"role":"model","parts":[{"functionCall":{"args":{"x":` + strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66) + `}}}]}]}`, "contents[1].parts[0].functionCall", contract.LimitToolDepth, 65, 64},
		{"object-fields", `{"contents":[{"parts":[{"functionResponse":{"response":{` + strings.Join(objectFields, ",") + `}}}]}]}`, "contents[0].parts[0].functionResponse.response", contract.LimitJSONObjectFields, 257, 256},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseSerializedBytes(context.Background(), []byte(test.body), config.Binding{})
			var failure *contract.Failure
			if !errors.As(err, &failure) || failure.Code != 413 || failure.Param != test.field || failure.Limit == nil || !failure.Limit.Valid() {
				t.Fatalf("missing input limit detail: %#v (%v)", failure, err)
			}
			limit := failure.Limit
			if limit.Name != test.limit || limit.Observed != test.observed || limit.Maximum != test.maximum || !limit.AtLeast {
				t.Fatalf("wrong limit measurements: %+v", limit)
			}
		})
	}
}

func TestSerializedGenerationConfigAndAttachmentByteLimits(t *testing.T) {
	generation := `{"ignored":"` + strings.Repeat("x", maxTextBytes) + `"}`
	_, err := ParseSerializedBytes(context.Background(), []byte(`{"generationConfig":`+generation+`,"contents":[{"parts":[{"text":"x"}]}]}`), config.Binding{})
	var failure *contract.Failure
	if !errors.As(err, &failure) || failure.Code != 413 || failure.Param != "generationConfig" || failure.Limit == nil || failure.Limit.Name != contract.LimitGenerationConfigBytes || failure.Limit.Observed != int64(len(generation)) || failure.Limit.AtLeast {
		t.Fatalf("generation size diagnostic lost: %#v (%v)", failure, err)
	}
	// The last attachment must obey the remaining aggregate allowance before
	// allocating decoded data, including valid Base64 near the early size bound.
	_, err = parseInline(context.Background(), json.RawMessage(`{"mimeType":"text/plain","data":"aW5wdXQ="}`), 1, 4)
	if !errors.As(err, &failure) || failure.Code != 413 || failure.Limit == nil || failure.Limit.Name != contract.LimitAttachmentBytes || failure.Limit.Maximum != maxAttachmentBytes || failure.Limit.Observed != maxAttachmentBytes+1 || !failure.Limit.AtLeast {
		t.Fatalf("attachment allowance diagnostic lost: %#v (%v)", failure, err)
	}
}
