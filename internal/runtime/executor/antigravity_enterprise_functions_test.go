package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const aeNativeMixedFunctionResult = `{"contents":[{"role":"user","parts":[{"text":"Read the test sensor"}]},{"role":"model","parts":[{"functionCall":{"name":"get_test_reading","args":{"station":"bench-47"},"id":"call-a"},"thoughtSignature":"skip_thought_signature_validator"},{"text":""}]},{"role":"user","parts":[{"functionResponse":{"name":"get_test_reading","id":"call-a","response":{"station":"bench-47","reading":6829}}},{"text":"Return the station and reading from the function result"}]}],"tools":[{"functionDeclarations":[{"name":"get_test_reading","parameters":{"type":"object","properties":{"station":{"type":"string"}},"required":["station"]}}]}],"toolConfig":{"functionCallingConfig":{"mode":"NONE"}},"generationConfig":{"maxOutputTokens":128}}`

func TestAntigravityEnterpriseNativeFunctionResponseCompatibilityAndPayloadRules(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, finalRule := range []string{"none", "override", "filter"} {
			t.Run(fmt.Sprintf("stream=%v/final-rule=%s", stream, finalRule), func(t *testing.T) {
				cfg := &config.Config{}
				models := []config.PayloadModelRule{{Name: "*", Protocol: "gemini"}}
				switch finalRule {
				case "override":
					var original map[string]any
					if err := json.Unmarshal([]byte(aeNativeMixedFunctionResult), &original); err != nil {
						t.Fatal(err)
					}
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"contents": original["contents"]}}}
				case "filter":
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"contents.3"}}}
				}
				calls := 0
				ctx := aeMockContext(t, aeQuotaPositive, func(req *http.Request) (*http.Response, error) {
					calls++
					body, errRead := io.ReadAll(req.Body)
					if errRead != nil {
						t.Fatal(errRead)
					}
					if gjson.GetBytes(body, "toolConfig.functionCallingConfig.mode").String() != "NONE" || gjson.GetBytes(body, "tools.#").Int() != 1 {
						t.Fatalf("tool mode or declarations were removed: %s", body)
					}
					for _, path := range []string{"contents.1.parts.0.functionCall.id", "contents.2.parts.0.functionResponse.id"} {
						if gjson.GetBytes(body, path).String() != "call-a" {
							t.Fatalf("function identity changed at %s: %s", path, body)
						}
					}
					if gjson.GetBytes(body, "contents.1.parts.0.thoughtSignature").String() != "skip_thought_signature_validator" || !gjson.GetBytes(body, "contents.1.parts.1.text").Exists() {
						t.Fatalf("model history was changed by function normalization: %s", body)
					}
					switch finalRule {
					case "none":
						if gjson.GetBytes(body, "contents.#").Int() != 4 || gjson.GetBytes(body, "contents.2.parts.#").Int() != 1 || gjson.GetBytes(body, "contents.3.parts.0.text").String() != "Return the station and reading from the function result" {
							t.Fatalf("mixed function result and instruction were not separated: %s", body)
						}
					case "override":
						if gjson.GetBytes(body, "contents.#").Int() != 3 || !gjson.GetBytes(body, "contents.2.parts.1.text").Exists() {
							t.Fatalf("built-in normalization overwrote final user payload rules: %s", body)
						}
					case "filter":
						if gjson.GetBytes(body, "contents.#").Int() != 3 || gjson.GetBytes(body, "contents.2.parts.#").Int() != 1 {
							t.Fatalf("filtered instruction was restored after payload rules: %s", body)
						}
					}
					if stream {
						return aeResponse(200, "text/event-stream", "data: "+aeAnswer+"\n\n"), nil
					}
					return aeResponse(200, "application/json", aeAnswer), nil
				})
				executor := NewAntigravityEnterpriseExecutor(cfg)
				request := coreexec.Request{Model: "gemini-3.8-flash-medium", Payload: []byte(aeNativeMixedFunctionResult)}
				options := coreexec.Options{SourceFormat: translator.FormatGemini}
				if stream {
					result, err := executor.ExecuteStream(ctx, aeExecutorAuth("global"), request, options)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, aeExecutorAuth("global"), request, options); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("generation calls=%d, want 1", calls)
				}
			})
		}
	}
}
