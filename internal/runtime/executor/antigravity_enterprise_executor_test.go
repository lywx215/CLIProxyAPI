package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const aeAnswer = `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"thoughtsTokenCount":2,"totalTokenCount":8},"modelVersion":"served-model"}`
const aeQuotaPositive = `{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":1}]}],"overageInfo":{}}`

type aeTransport func(*http.Request) (*http.Response, error)

func (f aeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func aeResponse(code int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}
func aeExecutorAuth(region string) *coreauth.Auth {
	return &coreauth.Auth{ID: "ae-test-" + region, Provider: ae.Provider, Status: coreauth.StatusActive, Metadata: map[string]any{"email": "test@example.com", "access_token": "access", "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "project_id": "test-project", "location": region, "user_tier": "gcp-ge-plus-tier"}}
}
func aePayload(format translator.Format) []byte {
	switch format {
	case translator.FormatGemini:
		return []byte(`{"contents":[{"role":"user","parts":[{"text":"Reply OK"}]}]}`)
	case translator.FormatClaude:
		return []byte(`{"model":"ignored","max_tokens":64,"messages":[{"role":"user","content":"Reply OK"}]}`)
	case translator.FormatOpenAIResponse:
		return []byte(`{"model":"ignored","input":"Reply OK"}`)
	default:
		return []byte(`{"model":"ignored","messages":[{"role":"user","content":"Reply OK"}]}`)
	}
}
func aeMockContext(t *testing.T, quota string, generate func(*http.Request) (*http.Response, error)) context.Context {
	t.Helper()
	return context.WithValue(context.Background(), "cliproxy.roundtripper", aeTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer access" || req.Header.Get("X-Goog-User-Project") != "test-project" {
			t.Errorf("missing bound authorization headers")
		}
		switch {
		case strings.HasSuffix(req.URL.Path, ":fetchConfig"):
			return aeResponse(200, "application/json", `{"adminControls":{}}`), nil
		case strings.HasSuffix(req.URL.Path, ":fetchQuotaStatus"):
			return aeResponse(200, "application/json", quota), nil
		default:
			return generate(req)
		}
	}))
}

func TestAntigravityEnterpriseProtocolsRegionsAndModels(t *testing.T) {
	for _, region := range []string{"global", "us", "eu"} {
		for _, model := range ae.DefaultModels() {
			for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI, translator.FormatOpenAIResponse, translator.FormatClaude} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/%v", region, model.ID, format, stream), func(t *testing.T) {
						var calls int
						ctx := aeMockContext(t, aeQuotaPositive, func(req *http.Request) (*http.Response, error) {
							calls++
							if _, ok := req.Context().Deadline(); ok {
								t.Fatal("generation has a deadline")
							}
							if req.URL.Scheme+"://"+req.URL.Host != ae.Origin(region) {
								t.Fatal("wrong regional origin")
							}
							body, _ := io.ReadAll(req.Body)
							if gjson.GetBytes(body, "model").String() != model.ID || gjson.GetBytes(body, "parent").String() != "projects/test-project/locations/"+region || gjson.GetBytes(body, "entitlement.userTier").String() != "gcp-ge-plus-tier" || gjson.GetBytes(body, "request").Exists() || gjson.GetBytes(body, "aicode").Exists() {
								t.Fatalf("wrong native body: %s", body)
							}
							if gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel").String() != strings.ToUpper(model.Level) {
								t.Fatalf("wrong thinking level: %s", body)
							}
							if stream {
								return aeResponse(200, "text/event-stream", "data: "+aeAnswer+"\n\n"), nil
							}
							return aeResponse(200, "application/json", aeAnswer), nil
						})
						e := NewAntigravityEnterpriseExecutor(&config.Config{})
						req := coreexec.Request{Model: model.ID, Payload: aePayload(format)}
						opts := coreexec.Options{SourceFormat: format}
						var output []byte
						if stream {
							result, err := e.ExecuteStream(ctx, aeExecutorAuth(region), req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								output = append(output, chunk.Payload...)
							}
						} else {
							result, err := e.Execute(ctx, aeExecutorAuth(region), req, opts)
							if err != nil {
								t.Fatal(err)
							}
							output = result.Payload
						}
						if !strings.Contains(string(output), "OK") || calls != 1 {
							t.Fatalf("invalid visible response or generation count: %d %s", calls, output)
						}
					})
				}
			}
		}
	}
}

func TestAntigravityEnterprisePayloadRulesRemainFinal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini"}}, Params: map[string]any{"generationConfig.thinkingConfig.thinkingLevel": "MEDIUM", "generationConfig.maxOutputTokens": 333}}}
			cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini"}}, Params: []string{"systemInstruction", "generationConfig.thinkingConfig.includeThoughts"}}}
			ctx := aeMockContext(t, aeQuotaPositive, func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				if gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel").String() != "MEDIUM" || gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int() != 333 || gjson.GetBytes(body, "systemInstruction").Exists() || gjson.GetBytes(body, "generationConfig.thinkingConfig.includeThoughts").Exists() {
					t.Fatalf("payload rules were overwritten: %s", body)
				}
				if stream {
					return aeResponse(200, "text/event-stream", "data: "+aeAnswer+"\n\n"), nil
				}
				return aeResponse(200, "application/json", aeAnswer), nil
			})
			e := NewAntigravityEnterpriseExecutor(cfg)
			req := coreexec.Request{Model: "gemini-4-argon-high", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"systemInstruction":{"parts":[{"text":"system"}]},"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`)}
			opts := coreexec.Options{SourceFormat: translator.FormatGemini}
			if stream {
				r, err := e.ExecuteStream(ctx, aeExecutorAuth("global"), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range r.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := e.Execute(ctx, aeExecutorAuth("global"), req, opts); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAntigravityEnterpriseSubscriptionGateAndPayloadConflict(t *testing.T) {
	for _, quota := range []string{
		`{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":0}]}],"overageInfo":{}}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":1}]}],"overageInfo":{"enabled":true}}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly"}]}],"overageInfo":{}}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":1}]}]}`,
	} {
		t.Run(quota, func(t *testing.T) {
			ctx := aeMockContext(t, quota, func(*http.Request) (*http.Response, error) {
				t.Fatal("subscription gate sent generation")
				return nil, nil
			})
			e := NewAntigravityEnterpriseExecutor(&config.Config{})
			if _, err := e.Execute(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-4-argon-low", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini}); err == nil {
				t.Fatal("gate allowed generation")
			}
		})
	}
	for _, path := range []string{"parent", "model", "entitlement.userTier", "aicode.experience"} {
		t.Run(path, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{path: "foreign"}}}
			ctx := aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
				t.Fatal("identity conflict sent generation")
				return nil, nil
			})
			e := NewAntigravityEnterpriseExecutor(cfg)
			if _, err := e.Execute(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-4-argon-low", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini}); err == nil {
				t.Fatal("identity conflict accepted")
			}
		})
	}
}

func TestAntigravityEnterpriseUnlistedCarbonRequiresDiagnostic(t *testing.T) {
	if len(AntigravityEnterpriseModels(ae.Config{})) != 14 {
		t.Fatal("default catalog changed")
	}
	for _, id := range []string{ae.CarbonModel, ae.CarbonModel + "-low", ae.CarbonModel + "-medium", ae.CarbonModel + "-high"} {
		t.Run(id, func(t *testing.T) {
			ctx := aeMockContext(t, aeQuotaPositive, func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				if gjson.GetBytes(body, "model").String() != id || gjson.GetBytes(body, "aicode").Exists() {
					t.Fatal("wrong diagnostic model or native oneof")
				}
				return aeResponse(200, "application/json", aeAnswer), nil
			})
			e := NewAntigravityEnterpriseExecutor(&config.Config{})
			req := coreexec.Request{Model: id, Payload: aePayload(translator.FormatGemini)}
			opts := coreexec.Options{SourceFormat: translator.FormatGemini}
			if _, err := e.Execute(ctx, aeExecutorAuth("global"), req, opts); err == nil {
				t.Fatal("unlisted carbon accepted")
			}
			opts.Metadata = map[string]any{antigravityEnterpriseDiagnosticKey: ae.CarbonModel}
			if _, err := e.Execute(ctx, aeExecutorAuth("global"), req, opts); err != nil {
				t.Fatal(err)
			}
			req.Model = ae.CarbonModel + "-foreign"
			if _, err := e.Execute(ctx, aeExecutorAuth("global"), req, opts); err == nil {
				t.Fatal("diagnostic option admitted an arbitrary model")
			}
		})
	}
}

func TestAntigravityEnterpriseCatalogExperienceDoesNotSetModelOneof(t *testing.T) {
	var generated bool
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", aeTransport(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, ":fetchConfig"):
			return aeResponse(200, "application/json", `{"availableExperiences":[{"name":"configured-experience","model":{"canonicalModelId":"gemini-3.8-flash","supportedThinkingLevels":["LOW"]}}]}`), nil
		case strings.HasSuffix(req.URL.Path, ":fetchQuotaStatus"):
			return aeResponse(200, "application/json", aeQuotaPositive), nil
		default:
			body, _ := io.ReadAll(req.Body)
			if gjson.GetBytes(body, "model").String() != "gemini-3.8-flash-low" || gjson.GetBytes(body, "aicode").Exists() {
				t.Fatalf("catalog experience overwrote native model oneof: %s", body)
			}
			generated = true
			return aeResponse(200, "application/json", aeAnswer), nil
		}
	}))
	e := NewAntigravityEnterpriseExecutor(&config.Config{})
	if _, err := e.Execute(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-3.8-flash-low", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini}); err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Fatal("expected one native model request")
	}
}

func TestAntigravityEnterpriseStreamFailuresAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name, body  string
		wantStarted bool
	}{
		{"rpc_before_candidates", `data: {"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}` + "\n\n", false},
		{"rpc_after_candidates", `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}` + "\n\n" + `data: {"error":{"code":503,"status":"UNAVAILABLE"}}` + "\n\n", true},
		{"truncated", `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}` + "\n\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
				return aeResponse(200, "text/event-stream", test.body), nil
			})
			e := NewAntigravityEnterpriseExecutor(&config.Config{})
			result, err := e.ExecuteStream(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-4-argon-low", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini})
			if err != nil {
				t.Fatal(err)
			}
			var failure error
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					failure = chunk.Err
				}
			}
			if failure == nil {
				t.Fatal("stream failure was hidden")
			}
			started, ok := failure.(interface{ GenerationStarted() bool })
			if !ok || started.GenerationStarted() != test.wantStarted {
				t.Fatalf("wrong replay classification: %v", failure)
			}
		})
	}
	t.Run("cancel_closes_read", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer func() { _ = writer.Close() }()
		var generated atomic.Bool
		ctx, cancel := context.WithCancel(aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
			generated.Store(true)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
		}))
		e := NewAntigravityEnterpriseExecutor(&config.Config{})
		r, err := e.ExecuteStream(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-4-argon-low", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini})
		if err != nil {
			t.Fatal(err)
		}
		if !generated.Load() {
			t.Fatal("generation not started")
		}
		cancel()
		select {
		case _, ok := <-r.Chunks:
			if ok {
				t.Fatal("cancellation emitted a normal response")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled upstream read leaked")
		}
	})
}

func TestAntigravityEnterpriseHTTPToolCannotBypassProtocol(t *testing.T) {
	e := NewAntigravityEnterpriseExecutor(&config.Config{})
	for _, target := range []string{"https://attacker.example/v1beta/projects/test-project/locations/global:generateContent", "https://businessaicode.googleapis.com/v1beta/projects/foreign/locations/global:generateContent", "https://businessaicode.googleapis.com/v1beta/projects/test-project/locations/global:selfAssignLicense"} {
		r, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		if err := e.PrepareRequest(r, aeExecutorAuth("global")); err == nil {
			t.Fatal("untrusted operation accepted")
		}
	}
	r, _ := http.NewRequest(http.MethodPost, "https://businessaicode.googleapis.com/v1beta/projects/test-project/locations/global:generateContent", strings.NewReader(`{}`))
	if _, err := e.HttpRequest(context.Background(), aeExecutorAuth("global"), r); err == nil {
		t.Fatal("management HTTP bypass allowed generation")
	}
}

func TestAntigravityEnterpriseMetadataCacheResetAndDisabledReads(t *testing.T) {
	var configReads, quotaReads, generations int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", aeTransport(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, ":fetchConfig"):
			configReads++
			return aeResponse(200, "application/json", `{"adminControls":{}}`), nil
		case strings.HasSuffix(req.URL.Path, ":fetchQuotaStatus"):
			quotaReads++
			return aeResponse(200, "application/json", aeQuotaPositive), nil
		default:
			generations++
			return aeResponse(200, "application/json", aeAnswer), nil
		}
	}))
	auth := aeExecutorAuth("global")
	e := NewAntigravityEnterpriseExecutor(&config.Config{})
	if _, _, err := e.Metadata(ctx, auth, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Metadata(ctx, auth, false); err != nil {
		t.Fatal(err)
	}
	if configReads != 1 || quotaReads != 1 || generations != 0 {
		t.Fatal("metadata did not cache or generated content")
	}
	key := antigravityEnterpriseMetadataKey(auth)
	e.mu.Lock()
	cached := e.metadata[key]
	past := time.Now().Add(-time.Minute)
	cached.quota.Groups[0].Buckets[0].ResetTime = &past
	e.metadata[key] = cached
	e.mu.Unlock()
	if _, _, err := e.Metadata(ctx, auth, false); err != nil {
		t.Fatal(err)
	}
	if configReads != 1 || quotaReads != 2 {
		t.Fatal("old positive quota crossed a weekly reset")
	}
	auth.Disabled = true
	if _, _, err := e.Metadata(ctx, auth, true); err != nil {
		t.Fatal("disabled binding metadata read failed", err)
	}
	if _, err := e.Execute(ctx, auth, coreexec.Request{Model: "gemini-4-argon-low", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini}); err == nil {
		t.Fatal("disabled binding generated content")
	}
	if generations != 0 {
		t.Fatal("read-only metadata generated content")
	}
}

func TestAntigravityEnterpriseFunctionProtocolRoundTrip(t *testing.T) {
	ctx := aeMockContext(t, aeQuotaPositive, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if gjson.GetBytes(body, "tools.0.functionDeclarations.0.name").String() != "weather" {
			t.Fatalf("client tools were discarded: %s", body)
		}
		if !strings.Contains(string(body), "functionCall") || !strings.Contains(string(body), "functionResponse") {
			t.Fatalf("tool history was discarded: %s", body)
		}
		return aeResponse(200, "application/json", `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"Paris"}},"thoughtSignature":"dGVzdC1zaWduYXR1cmU="}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"totalTokenCount":13}}`), nil
	})
	body := []byte(`{"messages":[{"role":"user","content":"weather"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`)
	e := NewAntigravityEnterpriseExecutor(&config.Config{})
	response, err := e.Execute(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-4-argon-low", Payload: body}, coreexec.Options{SourceFormat: translator.FormatOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.ValidBytes(response.Payload) || gjson.GetBytes(response.Payload, "choices.0.message.tool_calls.0.function.name").String() != "weather" {
		t.Fatalf("tool response was not translated: %s", response.Payload)
	}
}

func TestAntigravityEnterpriseCatalogAndQuotaReadIndependently(t *testing.T) {
	for _, failConfig := range []bool{false, true} {
		t.Run(fmt.Sprint(failConfig), func(t *testing.T) {
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", aeTransport(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, ":fetchConfig") {
					if failConfig {
						return aeResponse(403, "application/json", `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`), nil
					}
					return aeResponse(200, "application/json", `{"adminControls":{}}`), nil
				}
				if !failConfig {
					return aeResponse(403, "application/json", `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`), nil
				}
				return aeResponse(200, "application/json", aeQuotaPositive), nil
			}))
			e := NewAntigravityEnterpriseExecutor(&config.Config{})
			_, configErr := e.Catalog(ctx, aeExecutorAuth("global"), false)
			_, quotaErr := e.Quota(ctx, aeExecutorAuth("global"), false)
			if (configErr != nil) != failConfig || (quotaErr != nil) == failConfig {
				t.Fatalf("independent permission results lost: %v %v", configErr, quotaErr)
			}
			models, err := e.AvailableModels(ctx, aeExecutorAuth("global"), false)
			if failConfig {
				if err == nil {
					t.Fatal("configuration failure converted to default catalog success")
				}
			} else if err != nil || len(models) != 14 {
				t.Fatal("quota denial hid a readable model directory")
			}
		})
	}
}

func TestAntigravityEnterprisePayloadAliasesCannotOverrideBinding(t *testing.T) {
	for _, injection := range []map[string]any{
		{"entitlement.user_tier": "gcp-ge-pay-as-you-go-tier"},
		{"entitlement.license_location": "us"},
		{"aicode.files": []any{}},
	} {
		cfg := &config.Config{}
		cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: injection}}
		ctx := aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
			t.Fatal("conflicting binding alias reached upstream")
			return nil, nil
		})
		if _, err := NewAntigravityEnterpriseExecutor(cfg).Execute(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-4-argon-low", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini}); err == nil {
			t.Fatal("conflicting identity aliases accepted")
		}
	}
	ctx := aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
		t.Fatal("mismatched model test reached generation")
		return nil, nil
	})
	if _, err := NewAntigravityEnterpriseExecutor(&config.Config{}).Execute(ctx, aeExecutorAuth("global"), coreexec.Request{Model: "gemini-4-argon-high", Payload: aePayload(translator.FormatGemini)}, coreexec.Options{SourceFormat: translator.FormatGemini, Metadata: map[string]any{"antigravity_enterprise_expected_model": "gemini-4-argon-low"}}); err == nil {
		t.Fatal("model test silently changed the model")
	}
}

func TestAntigravityEnterpriseNativeSafetyEnums(t *testing.T) {
	ctx := aeMockContext(t, aeQuotaPositive, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		settings := gjson.GetBytes(body, "safetySettings").Array()
		if len(settings) != 4 {
			t.Fatalf("expected four supported native safety settings, got %s", body)
		}
		seen := map[string]bool{}
		for _, setting := range settings {
			category := setting.Get("category").String()
			switch category {
			case "HARM_CATEGORY_HARASSMENT", "HARM_CATEGORY_HATE_SPEECH", "HARM_CATEGORY_SEXUALLY_EXPLICIT", "HARM_CATEGORY_DANGEROUS_CONTENT":
			default:
				t.Fatalf("non-native category reached BAIC: %s", category)
			}
			if seen[category] {
				t.Fatal("native safety category duplicated")
			}
			seen[category] = true
		}
		return aeResponse(200, "application/json", aeAnswer), nil
	})
	req := coreexec.Request{Model: "gemini-4-argon-low", Payload: aePayload(translator.FormatOpenAI)}
	if _, err := NewAntigravityEnterpriseExecutor(&config.Config{}).Execute(ctx, aeExecutorAuth("global"), req, coreexec.Options{SourceFormat: translator.FormatOpenAI}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"safetySettings.0.category": "HARM_CATEGORY_CIVIC_INTEGRITY"}}}
	ctx = aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
		t.Fatal("payload rule reintroduced unsupported native safety enum")
		return nil, nil
	})
	if _, err := NewAntigravityEnterpriseExecutor(cfg).Execute(ctx, aeExecutorAuth("global"), req, coreexec.Options{SourceFormat: translator.FormatOpenAI}); err == nil {
		t.Fatal("post-rule invalid safety category was silently repaired or accepted")
	}
}
