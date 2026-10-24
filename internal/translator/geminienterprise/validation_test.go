package geminienterprise

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func assertEnterpriseFieldError(t *testing.T, err error, path string, payload bool) {
	t.Helper()
	var detail *Error
	if !errors.As(err, &detail) || detail.StatusCode() != 400 || !detail.IsRequestScoped() {
		t.Fatalf("expected request-scoped HTTP 400, got %v", err)
	}
	message, code, param := PublicErrorDetail(fmt.Errorf("wrapped: %w", err), 400)
	if code != "invalid_field" || param != path || !strings.Contains(message, path) {
		t.Fatalf("incorrect field diagnostic: message=%q code=%q param=%q", message, code, param)
	}
	if strings.Contains(message, "Check the server request payload configuration.") != payload {
		t.Fatalf("incorrect payload guidance for %q", message)
	}
	if !json.Valid([]byte(err.Error())) || gjson.Get(err.Error(), "error.param").String() != path {
		t.Fatalf("invalid serialized error: %v", err)
	}
}

func TestEnterpriseFinalValidationIsModelIndependentAndReadOnly(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro", "future-enterprise-model"} {
		t.Run(model, func(t *testing.T) {
			for _, enums := range []string{
				`"answerGenerationMode":"NORMAL","assistSkippingMode":"REQUEST_ASSIST"`,
				`"answerGenerationMode":"ANSWER_GENERATION_MODE_UNSPECIFIED","assistSkippingMode":"ASSIST_SKIPPING_MODE_UNSPECIFIED"`,
			} {
				body := []byte(`{"query":{"text":"Keep this exact query 🙂"},"generationSpec":{"modelId":"` + model + `"},"toolsSpec":{"webGroundingSpec":{},"vertexAiSearchSpec":{"disabled":false,"filter":"category:ANY(\"docs\")"}},"userMetadata":{"timeZone":"America/Los_Angeles","preferredLanguageCode":"en"},"actionSpec":{"actionDisabled":true},"isSessionLess":true,"languageCode":"en","googleSearchGroundingEnabled":true,"webGroundingEnabled":false,` + enums + `}`)
				before := bytes.Clone(body)
				if err := ValidateFinal(body); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(body, before) {
					t.Fatal("validation changed the final user payload")
				}
			}
		})
	}
}

func TestEnterpriseFinalValidationRejectsUnsupportedFieldsAndTypes(t *testing.T) {
	const base = `"query":{"text":"hi"},"generationSpec":{"modelId":"gemini-4-argon"}`
	for _, test := range []struct{ name, body, path string }{
		{"native-generation", `{` + base + `,"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`, "generationConfig"},
		{"native-system", `{` + base + `,"systemInstruction":{"parts":[{"text":"private-prompt"}]}}`, "systemInstruction"},
		{"query-nested", `{"query":{"text":"hi","languageCode":"en"},"generationSpec":{"modelId":"model"}}`, "query.languageCode"},
		{"generation-nested", `{"query":{"text":"hi"},"generationSpec":{"modelId":"model","temperature":0.5}}`, "generationSpec.temperature"},
		{"grounding-nested", `{` + base + `,"toolsSpec":{"webGroundingSpec":{"enabled":true}}}`, "toolsSpec.webGroundingSpec.enabled"},
		{"file-tool", `{` + base + `,"toolsSpec":{"fileSearchSpec":{}}}`, "toolsSpec.fileSearchSpec"},
		{"search-filter-type", `{` + base + `,"toolsSpec":{"vertexAiSearchSpec":{"filter":{}}}}`, "toolsSpec.vertexAiSearchSpec.filter"},
		{"search-disable-type", `{` + base + `,"toolsSpec":{"vertexAiSearchSpec":{"disabled":"true"}}}`, "toolsSpec.vertexAiSearchSpec.disabled"},
		{"metadata-nested", `{` + base + `,"userMetadata":{"userId":"private-identity"}}`, "userMetadata.userId"},
		{"metadata-type", `{` + base + `,"userMetadata":{"timeZone":7}}`, "userMetadata.timeZone"},
		{"action-nested", `{` + base + `,"actionSpec":{"agentId":"private-agent"}}`, "actionSpec.agentId"},
		{"action-type", `{` + base + `,"actionSpec":{"actionDisabled":null}}`, "actionSpec.actionDisabled"},
		{"boolean-type", `{` + base + `,"isSessionLess":"true"}`, "isSessionLess"},
		{"language-type", `{` + base + `,"languageCode":[]}`, "languageCode"},
		{"answer-enum", `{` + base + `,"answerGenerationMode":"PRIVATE_INVALID_MODE"}`, "answerGenerationMode"},
		{"assist-enum", `{` + base + `,"assistSkippingMode":"SKIP_ASSIST"}`, "assistSkippingMode"},
		{"enum-number", `{` + base + `,"answerGenerationMode":1}`, "answerGenerationMode"},
		{"missing-query", `{"generationSpec":{"modelId":"model"}}`, "query"},
		{"empty-query", `{"query":{"text":"  "},"generationSpec":{"modelId":"model"}}`, "query.text"},
		{"null-query", `{"query":null,"generationSpec":{"modelId":"model"}}`, "query"},
		{"missing-model", `{"query":{"text":"hi"},"generationSpec":{}}`, "generationSpec.modelId"},
		{"wrong-model-type", `{"query":{"text":"hi"},"generationSpec":{"modelId":7}}`, "generationSpec.modelId"},
		{"tools-null", `{` + base + `,"toolsSpec":null}`, "toolsSpec"},
		{"unsafe-field-name", `{` + base + `,"Bearer private-token":true}`, "body"},
		{"provider-field-name", `{` + base + `,"GeMiNi_EnTeRpRiSe_Config":true}`, "body"},
		{"endpoint-field-name", `{` + base + `,"stream_Assist":true}`, "body"},
		{"service-field-name", `{` + base + `,"discoveryengine.googleapis.com":true}`, "body"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			before := bytes.Clone(body)
			err := ValidateFinal(body)
			assertEnterpriseFieldError(t, err, test.path, true)
			for _, private := range []string{"private-prompt", "private-identity", "private-agent", "private-token", "PRIVATE_INVALID_MODE"} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("request value leaked: %s", private)
				}
			}
			if !bytes.Equal(body, before) {
				t.Fatal("invalid payload was repaired")
			}
		})
	}
	for _, field := range []string{"session", "sessionInfo", "assistToken", "fileIds", "files", "agents", "agentSpec", "imageGenerationSpec", "audioGenerationSpec"} {
		t.Run(field, func(t *testing.T) {
			assertEnterpriseFieldError(t, ValidateFinal([]byte(`{`+base+`,"`+field+`":null}`)), field, true)
		})
	}
}

func TestEnterpriseDuplicateKeysRejectedBeforeNormalizationOrDispatch(t *testing.T) {
	for _, test := range []struct{ protocol, body, path string }{
		{"openai", `{"messages":[{"role":"user","content":"one"}],"messages":[{"role":"user","content":"two"}]}`, "messages"},
		{"openai", `{"messages":[{"role":"user","role":"assistant","content":"hi"}]}`, "messages[0].role"},
		{"openai", `{"messages":[{"role":"user","content":"hi"}],"temperature":0.1,"temperature":0.9}`, "temperature"},
		{"gemini", `{"contents":[{"parts":[{"text":"one","text":"two"}]}]}`, "contents[0].parts[0].text"},
		{"gemini", `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"text/plain","responseMimeType":"application/json"}}`, "generationConfig.responseMimeType"},
	} {
		_, err := Normalize([]byte(test.body), test.protocol, "gemini-4-argon")
		assertEnterpriseFieldError(t, err, test.path, false)
	}
	for _, test := range []struct{ body, path string }{
		{`{"query":{"text":"one","text":"two"},"generationSpec":{"modelId":"model"}}`, "query.text"},
		{`{"query":{"text":"hi"},"generationSpec":{"modelId":"one"},"generationSpec":{"modelId":"two"}}`, "generationSpec"},
	} {
		assertEnterpriseFieldError(t, ValidateFinal([]byte(test.body)), test.path, true)
	}
}

func TestEnterpriseInputDiagnosticsIdentifyExactNestedFields(t *testing.T) {
	const geminiContent = `"contents":[{"parts":[{"text":"hi"}]}]`
	const chatMessages = `"messages":[{"role":"user","content":"hi"}]`
	for _, test := range []struct{ name, protocol, body, path string }{
		{"gemini-second-text-part", "gemini", `{"contents":[{"parts":[{"text":"hi"},{"text":42}]}]}`, "contents[0].parts[1].text"},
		{"chat-second-text-part", "openai", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"text","text":42}]}]}`, "messages[0].content[1].text"},
		{"gemini-second-tool", "gemini", `{` + geminiContent + `,"tools":[{"urlContext":{}},{"googleSearch":true}]}`, "tools[1].googleSearch"},
		{"chat-second-tool", "openai", `{` + chatMessages + `,"tools":[{"urlContext":{}},{"googleSearch":true}]}`, "tools[1].googleSearch"},
		{"nested-tool-aliases", "gemini", `{` + geminiContent + `,"toolConfig":{"functionCallingConfig":{"mode":"AUTO"},"function_calling_config":{"mode":"NONE"}}}`, "toolConfig.functionCallingConfig"},
		{"nested-tool-type", "gemini", `{` + geminiContent + `,"toolConfig":{"functionCallingConfig":true}}`, "toolConfig.functionCallingConfig"},
		{"nested-tool-mode", "gemini", `{` + geminiContent + `,"toolConfig":{"functionCallingConfig":{"mode":"PRIVATE_FORCED_MODE"}}}`, "toolConfig.functionCallingConfig.mode"},
		{"nested-tool-mode-type", "gemini", `{` + geminiContent + `,"toolConfig":{"functionCallingConfig":{"mode":42}}}`, "toolConfig.functionCallingConfig.mode"},
		{"top-generation-aliases", "gemini", `{` + geminiContent + `,"generationConfig":{},"generation_config":{}}`, "generationConfig"},
		{"nested-generation-aliases", "gemini", `{` + geminiContent + `,"generationConfig":{"responseMimeType":"text/plain","response_mime_type":"application/json"}}`, "generationConfig.responseMimeType"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Normalize([]byte(test.body), test.protocol, "gemini-4-argon")
			assertEnterpriseFieldError(t, err, test.path, false)
			if strings.Contains(err.Error(), "PRIVATE_FORCED_MODE") {
				t.Fatal("invalid field value was copied into the diagnostic")
			}
		})
	}
}

func TestEnterpriseMalformedPayloadsHaveSafeDiagnostics(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`[]`), []byte(`null`), []byte(`{"query":`), []byte(`{"secret":"private-prompt"} {}`),
		append([]byte(`{"query":{"text":"`), 0xff),
		[]byte(`{"` + strings.Repeat("field", 60) + `":` + strings.Repeat("[", 66) + `0` + strings.Repeat("]", 66) + `}`),
	} {
		assertEnterpriseFieldError(t, ValidateFinal(body), "body", true)
	}
	body := []byte(`{"query":{"text":"` + strings.Repeat("x", 1024*1024+1) + `"},"generationSpec":{"modelId":"model"}}`)
	original := string(body)
	if err := ValidateFinal(body); err != nil {
		t.Fatalf("valid large final payload rejected: %v", err)
	}
	if string(body) != original {
		t.Fatal("final validation mutated the configured payload")
	}
}

func TestEnterpriseUpstreamStatusAndSafeDiagnostics(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
	}{
		{400, "upstream_invalid_request"}, {422, "upstream_invalid_request"},
		{401, "upstream_authentication_required"}, {403, "permission_denied"},
		{404, "resource_not_found"}, {429, "rate_limit_exceeded"},
		{500, "upstream_error"}, {503, "upstream_error"},
	} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			err := UpstreamHTTPError(test.status, []byte(`{"error":{"message":"Bearer private-token; prompt private-prompt","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"private-reason"}]}}`))
			var detail *Error
			if !errors.As(err, &detail) || detail.StatusCode() != test.status || detail.IsRequestScoped() {
				t.Fatalf("changed HTTP status or started semantics: %v", err)
			}
			message, code, param := PublicErrorDetail(err, test.status)
			if message == "" || code != test.code || param != "" {
				t.Fatalf("incorrect diagnostic: %q %q %q", message, code, param)
			}
			for _, private := range []string{"private-token", "private-prompt", "private-reason", "Bearer"} {
				if strings.Contains(err.Error(), private) || strings.Contains(message, private) {
					t.Fatalf("raw upstream error leaked %q", private)
				}
			}
			if message, code, param := PublicErrorDetail(err, 200); message != "" || code != "" || param != "" {
				t.Fatal("diagnostic used for a different status")
			}
		})
	}
}

func TestEnterpriseUpstreamFieldDiagnosticsUseOnlySafeBadRequestPaths(t *testing.T) {
	for _, test := range []struct{ field, kind, want string }{
		{"generationSpec.modelId", "type.googleapis.com/google.rpc.BadRequest", "generationSpec.modelId"},
		{"query.text", "type.googleapis.com/google.rpc.BadRequest", "query.text"},
		{"generationConfig.responseMimeType", "type.googleapis.com/google.rpc.BadRequest", "generationConfig.responseMimeType"},
		{"gemini_enterprise_config", "type.googleapis.com/google.rpc.BadRequest", ""},
		{"GeMiNiEnTeRpRiSe.config", "type.googleapis.com/google.rpc.BadRequest", ""},
		{"stream_Assist.query", "type.googleapis.com/google.rpc.BadRequest", ""},
		{"DiscoveryEngine.googleapis.com.request", "type.googleapis.com/google.rpc.BadRequest", ""},
		{"Bearer private-token", "type.googleapis.com/google.rpc.BadRequest", ""},
		{"query.text\r\nInjected: private-token", "type.googleapis.com/google.rpc.BadRequest", ""},
		{"generationSpec.modelId", "type.googleapis.com/google.rpc.ErrorInfo", ""},
	} {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "private-prompt", "details": []any{map[string]any{"@type": test.kind, "fieldViolations": []any{map[string]string{"field": test.field, "description": "Bearer private-token"}}}}}})
		err := UpstreamHTTPError(400, body)
		message, code, param := PublicErrorDetail(err, 400)
		if param != test.want || code != "upstream_invalid_request" || strings.Contains(message, "private-") || strings.Contains(err.Error(), "private-") {
			t.Fatalf("unsafe field diagnostic: %v", err)
		}
	}
	for _, raw := range []string{
		`<html>Bearer private-token private-prompt</html>`,
		`{"error":{"message":"Model private-model is not allowed for this account; private-token"}}`,
		`{"error":{"message":"Model private-model permission denied"},"padding":"` + strings.Repeat("x", 64*1024) + `"}`,
	} {
		err := UpstreamHTTPError(400, []byte(raw))
		message, code, param := PublicErrorDetail(err, 400)
		if strings.Contains(message, "private-") || strings.Contains(err.Error(), "private-") {
			t.Fatal("upstream message was copied")
		}
		if len(raw) <= 64*1024 && strings.Contains(raw, "not allowed") {
			if code != "model_access_denied" || param != "model" {
				t.Fatalf("model permission was not classified: %v", err)
			}
		} else if code != "upstream_invalid_request" || param != "" {
			t.Fatalf("untrusted or oversize body affected classification: %v", err)
		}
	}
	for _, err := range []error{errors.New("private-prompt"), Invalid("private-prompt"), Failure("private-code", "private-prompt", false)} {
		message, code, param := PublicErrorDetail(err, 400)
		if message != "" || code != "" || param != "" {
			t.Fatal("untrusted Error text became a public diagnostic")
		}
	}
}

func TestEnterprisePublicDiagnosticsDoNotIdentifyProvider(t *testing.T) {
	type diagnosticTest struct {
		name   string
		err    error
		status int
		code   string
	}
	tests := []diagnosticTest{
		{"input-field", invalidField("contents[1].parts[0].functionCall", "is not supported in text history", false), 400, "invalid_field"},
		{"payload-field", ValidateFinal([]byte(`{"query":{"text":"hi"},"generationSpec":{"modelId":"private-model"},"generationConfig":{}}`)), 400, "invalid_field"},
		{"private-field-name", invalidField("GeMiNi_EnTeRpRiSe_config", "is not supported by the request schema", true), 400, "invalid_field"},
		{"private-endpoint-name", invalidField("stream_Assist.request", "is not supported by the request schema", true), 400, "invalid_field"},
		{"private-service-name", invalidField("DiscoveryEngine.googleapis.com.request", "is not supported by the request schema", true), 400, "invalid_field"},
	}
	for _, input := range []struct{ name, protocol, body string }{
		{"message-field", "openai", `{"messages":[{"role":"assistant","content":"hi","tool_calls":[{}]},{"role":"user","content":"continue"}]}`},
		{"tool-field", "gemini", `{"contents":[{"parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[]}]}`},
		{"history-field", "gemini", `{"contents":[{"parts":[{"functionCall":{"name":"private-function"}}]}]}`},
	} {
		_, err := Normalize([]byte(input.body), input.protocol, "private-model")
		tests = append(tests, diagnosticTest{input.name, err, 400, "invalid_field"})
	}
	const raw = `{"error":{"message":"Google Gemini Enterprise streamAssist requests.payload protocol gemini-enterprise private-token","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"query.text"}]}]}}`
	for _, upstream := range []struct {
		status int
		code   string
	}{
		{400, "upstream_invalid_request"}, {422, "upstream_invalid_request"},
		{401, "upstream_authentication_required"}, {403, "permission_denied"},
		{404, "resource_not_found"}, {429, "rate_limit_exceeded"}, {503, "upstream_error"},
	} {
		tests = append(tests, diagnosticTest{fmt.Sprintf("upstream-%d", upstream.status), UpstreamHTTPError(upstream.status, []byte(raw)), upstream.status, upstream.code})
	}
	tests = append(tests, diagnosticTest{"model-access", UpstreamHTTPError(400, []byte(`{"error":{"message":"Google Enterprise model private-model is not allowed for this account"}}`)), 400, "model_access_denied"})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, code, _ := PublicErrorDetail(test.err, test.status)
			if message == "" || code != test.code {
				t.Fatalf("missing generic public diagnostic: message=%q code=%q error=%v", message, code, test.err)
			}
			for _, forbidden := range []string{"gemini", "enterprise", "google", "streamassist", "requests.payload", "protocol", "private-token", "private-model", "private-function"} {
				if strings.Contains(strings.ToLower(message), forbidden) || strings.Contains(strings.ToLower(code), forbidden) {
					t.Fatalf("public diagnostic identifies the provider or echoes a private value: message=%q code=%q", message, code)
				}
			}
			if actual := gjson.Get(test.err.Error(), "error.message").String(); actual != message {
				t.Fatalf("serialized diagnostic differs: got %q, want %q", actual, message)
			}
			if actual := gjson.Get(test.err.Error(), "error.code").String(); actual != code {
				t.Fatalf("serialized code differs: got %q, want %q", actual, code)
			}
		})
	}
}
