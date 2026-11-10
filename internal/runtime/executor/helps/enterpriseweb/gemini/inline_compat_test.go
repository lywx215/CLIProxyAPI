package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	appconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

func TestWebInlineMIMENormalizesWithoutChangingAttachmentBytes(t *testing.T) {
	for _, test := range []struct {
		mime, normalized string
		data             []byte
	}{
		{"image/jpg", "image/jpeg", []byte{0xff, 0xd8, 0xff, 0xd9}},
		{"text/plain; charset=UTF-8", "text/plain", []byte("caf\xc3\xa9\n")},
		{"text/plain; charset=US-ASCII", "text/plain", []byte("example\r\n")},
	} {
		t.Run(test.mime, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"contents":[{"parts":[{"inlineData":{"mimeType":%q,"data":%q}}]}]}`,
				test.mime, base64.StdEncoding.EncodeToString(test.data)))
			ctx := WithInputLimits(t.Context(), appconfig.GeminiEnterpriseWebInputLimits{})
			for _, serialized := range []bool{false, true} {
				request, err := parseBytes(ctx, body, config.Binding{}, nil, serialized)
				if err != nil || len(request.Attachments) != 1 {
					t.Fatalf("serialized=%t: attachment missing: %v", serialized, err)
				}
				attachment := request.Attachments[0]
				if attachment.MIMEType != test.normalized || !bytes.Equal(attachment.Data, test.data) {
					t.Fatalf("serialized=%t: MIME or attachment bytes changed incorrectly: %+v", serialized, attachment)
				}
			}
			if _, err := ParseBytes(t.Context(), body, config.Binding{}, nil); err == nil {
				t.Fatal("standalone Business parser unexpectedly accepted Web-only MIME compatibility")
			}
		})
	}
}

func TestWebInlineFailureIdentifiesFieldAndReason(t *testing.T) {
	const prefix = "contents[0].parts[1].inlineData"
	for _, test := range []struct {
		name, part, suffix string
		reason             contract.AttachmentReason
	}{
		{"part aliases", `{"inlineData":{},"inline_data":{}}`, "", contract.AttachmentFieldConflict},
		{"MIME aliases", `{"inlineData":{"mimeType":"text/plain","mime_type":"text/plain","data":"YQ=="}}`, ".mimeType", contract.AttachmentFieldConflict},
		{"duplicate data key", `{"inlineData":{"mimeType":"text/plain","data":"YQ==","data":"YQ=="}}`, ".data", contract.AttachmentFieldConflict},
		{"null object", `{"inlineData":null}`, "", contract.AttachmentInvalidObject},
		{"invalid MIME", `{"inlineData":{"mimeType":"not a MIME type","data":"YQ=="}}`, ".mimeType", contract.AttachmentInvalidMIME},
		{"unsupported MIME", `{"inlineData":{"mimeType":"image/gif","data":"YQ=="}}`, ".mimeType", contract.AttachmentUnsupportedMIME},
		{"unsupported charset", `{"inlineData":{"mimeType":"text/plain; charset=latin1","data":"YQ=="}}`, ".mimeType", contract.AttachmentUnsupportedMIME},
		{"unexpected parameter", `{"inlineData":{"mimeType":"text/plain; charset=utf-8; boundary=ignored","data":"YQ=="}}`, ".mimeType", contract.AttachmentUnsupportedMIME},
		{"extra RFC 2231 parameter", `{"inlineData":{"mimeType":"text/plain; charset=utf-8; filename*=ignored","data":"YQ=="}}`, ".mimeType", contract.AttachmentUnsupportedMIME},
		{"missing data", `{"inlineData":{"mimeType":"text/plain"}}`, ".data", contract.AttachmentMissingData},
		{"empty data", `{"inlineData":{"mimeType":"text/plain","data":""}}`, ".data", contract.AttachmentMissingData},
		{"null data", `{"inlineData":{"mimeType":"text/plain","data":null}}`, ".data", contract.AttachmentMissingData},
		{"wrong data type", `{"inlineData":{"mimeType":"text/plain","data":123}}`, ".data", contract.AttachmentInvalidData},
		{"Base64 mixed alphabets", `{"inlineData":{"mimeType":"text/plain","data":"-_+/"}}`, ".data", contract.AttachmentInvalidBase64},
		{"Base64 nonzero trailing bits", `{"inlineData":{"mimeType":"text/plain","data":"YR=="}}`, ".data", contract.AttachmentInvalidBase64},
		{"UTF-8 mismatch", `{"inlineData":{"mimeType":"text/plain; charset=UTF-8","data":"/w=="}}`, ".data", contract.AttachmentCharsetMismatch},
		{"US-ASCII mismatch", `{"inlineData":{"mimeType":"text/plain; charset=US-ASCII","data":"w6k="}}`, ".data", contract.AttachmentCharsetMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := WithInputLimits(t.Context(), appconfig.GeminiEnterpriseWebInputLimits{})
			body := []byte(`{"contents":[{"parts":[{"text":"read the attachment"},` + test.part + `]}]}`)
			for _, serialized := range []bool{false, true} {
				_, err := parseBytes(ctx, body, config.Binding{}, nil, serialized)
				var failure *contract.Failure
				if !errors.As(err, &failure) || failure.Code != 400 || failure.Param != prefix+test.suffix || failure.AttachmentReason != test.reason {
					t.Fatalf("serialized=%t: wrong failure: %#v", serialized, failure)
				}
				wantReason := ""
				if serialized {
					wantReason = "invalid_history"
				}
				if failure.Reason != wantReason {
					t.Fatalf("serialized=%t: public error code changed: %q", serialized, failure.Reason)
				}
			}
		})
	}
}

func TestWebModelHistoryAttachmentUsesUserValidationAndPreservesRole(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]},{"role":"model","parts":[{"text":"before"},{"inline_data":{"mime_type":"image/png","data":"Yg=="},"thoughtSignature":"private-signature"},{"text":"after"}]},{"role":"user","parts":[{"text":"last"}]}]}`
	ctx := WithInputLimits(context.Background(), appconfig.GeminiEnterpriseWebInputLimits{})
	request, err := ParseSerializedBytes(ctx, []byte(body), config.Binding{})
	if err != nil || len(request.Attachments) != 2 || string(request.Attachments[1].Data) != "b" {
		t.Fatalf("model history attachment lost: %v", err)
	}
	query := request.QueryParts[0]["text"]
	if !strings.Contains(query, `"role":"model","parts":[{"kind":"text","text":"before"},{"kind":"attachment","attachment":{"id":"c1_p1","filename":"c0001-p0001.png","mime_type":"image/png"}},{"kind":"text","text":"after"}]`) ||
		strings.Contains(query, "private-signature") || strings.Contains(query, "Yg==") {
		t.Fatal("model role, part order, or visibility changed")
	}
	_, err = ParseSerializedBytes(ctx, []byte(strings.Replace(body, "Yg==", "bad base64", 1)), config.Binding{})
	var failure *contract.Failure
	if !errors.As(err, &failure) || failure.Code != 400 || failure.Reason != "invalid_history" ||
		failure.Param != "contents[1].parts[1].inlineData.data" || failure.AttachmentReason != contract.AttachmentInvalidBase64 {
		t.Fatalf("wrong model attachment failure: %#v", failure)
	}
	small := WithInputLimits(t.Context(), appconfig.GeminiEnterpriseWebInputLimits{MaxAttachmentBytes: 1})
	_, err = ParseSerializedBytes(small, []byte(body), config.Binding{})
	if !errors.As(err, &failure) || failure.Code != 413 || failure.Limit == nil ||
		failure.Limit.Name != contract.LimitAttachmentBytes || failure.Limit.Observed != 2 {
		t.Fatalf("model attachment bypassed the combined byte limit: %#v", failure)
	}
}
